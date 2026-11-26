package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	imagecodec "image"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
)

const asyncImageReferenceMaxBytes = 20 << 20
const asyncImageReferenceMaxCount = 4
const asyncImageMultipartMaxBytes = 82 << 20
const asyncImageJSONMaxBytes = 110 << 20
const asyncImageResponseMaxBytes = 256 << 10
const asyncImageDispatchTimeout = 60 * time.Second

var asyncImageIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,256}$`)
var asyncImageJobPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

type AsyncImageError struct {
	Status     int
	Fields     map[string]any
	RetryAfter string
	Cause      error
}

func (e *AsyncImageError) Error() string { return fmt.Sprint(e.Fields["message"]) }
func (e *AsyncImageError) Unwrap() error { return e.Cause }

func imageLocalError(status int, code, message string) *AsyncImageError {
	return &AsyncImageError{Status: status, Fields: map[string]any{"source": "platform", "code": code, "type": "api_error", "message": errordetail.Text(message)}}
}

func imageProviderError(status int, headers http.Header, body []byte, secret string) *AsyncImageError {
	body = redactAsyncImageSecret(body, secret)
	fields := errordetail.Fields(body)
	if message, ok := fields["message"].(string); !ok || strings.TrimSpace(message) == "" {
		fields["message"] = fmt.Sprintf("Image upstream returned HTTP %d without an error message", status)
	}
	fields["source"], fields["upstream_status"] = "upstream", status
	if id := errordetail.RequestID(headers); id != "" {
		fields["upstream_request_id"] = id
	}
	if _, ok := fields["code"]; !ok {
		fields["code"] = "image_upstream_error"
	}
	if _, ok := fields["type"]; !ok {
		fields["type"] = "api_error"
	}
	out := status
	if out < 400 || out == 401 || out == 403 || out == 404 || out >= 500 {
		out = 502
	}
	retryAfter := headers.Get("Retry-After")
	if retryAfter != "" && !diagnosticRetryAfterBounded(retryAfter) {
		retryAfter = ""
	}
	return &AsyncImageError{Status: out, Fields: fields, RetryAfter: retryAfter}
}

func WriteAsyncImageFailure(c *gin.Context, err error, task *ImageTask) {
	var failure *AsyncImageError
	if !errors.As(err, &failure) {
		failure = imageLocalError(503, "image_platform_error", err.Error())
		failure.Cause = err
	}
	slog.Warn("image_async.operation_failed", "path", c.Request.URL.Path, "status", failure.Status, "diagnostic", failure.Fields)
	upstreamStatus, _ := failure.Fields["upstream_status"].(int)
	upstreamID, _ := failure.Fields["upstream_request_id"].(string)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Platform: PlatformOpenAI, Kind: "image_async_error", UpstreamStatusCode: upstreamStatus, UpstreamRequestID: upstreamID, Message: failure.Error(), Diagnostic: failure.Fields})
	output := map[string]any{"schema_version": 2, "error": failure.Fields, "operation_error": failure.Fields}
	if task != nil {
		SetImageReceipt(c, task)
		output = map[string]any{}
		encoded, _ := json.Marshal(task)
		_ = json.Unmarshal(encoded, &output)
		output["operation_error"] = failure.Fields
		if c.Request.Method == http.MethodPost {
			output["error"] = failure.Fields
		}
	} else if c.Request.Method == http.MethodPost {
		output["request_id"] = c.Writer.Header().Get("X-Client-Request-ID")
		output["submission_status"] = "not_submitted"
		if c.GetBool(mediaDispatchedKey) {
			output["submission_status"] = "failed"
		}
		output["billing"] = imageNotCharged()
	}
	if task != nil {
		c.Header("X-Image-Request-ID", task.RequestID)
	}
	if c.Request.Method == http.MethodPost {
		output["operation"] = "create"
	} else if strings.HasSuffix(c.Request.URL.Path, "/content") {
		output["operation"] = "download"
	} else {
		output["operation"] = "query"
	}
	if failure.RetryAfter != "" {
		c.Header("Retry-After", failure.RetryAfter)
	}
	if id, _ := failure.Fields["upstream_request_id"].(string); id != "" {
		c.Header("X-Upstream-Request-ID", id)
	}
	c.Header("Cache-Control", "no-store")
	WriteMediaJSON(c, failure.Status, output)
}

// Validate the actual image bytes before encoding; declared MIME types are not trusted.
func asyncReferenceDataURI(data []byte) (string, error) {
	if len(data) == 0 || len(data) > asyncImageReferenceMaxBytes {
		return "", imageLocalError(413, "image_reference_too_large", fmt.Sprintf("Reference image size must be 1..%d bytes", asyncImageReferenceMaxBytes))
	}
	config, format, err := imagecodec.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", imageLocalError(400, "invalid_reference_image", "Decode reference image: "+err.Error())
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		return "", imageLocalError(422, "unsupported_image_format", "Reference format must be PNG, JPEG or WebP; received "+format)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 100_000_000 {
		return "", imageLocalError(422, "invalid_image_dimensions", "Reference image must contain at most 100000000 pixels")
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func BuildAsyncImagePayload(parsed *OpenAIImagesRequest, model string, maxBytes int64) ([]byte, error) {
	if parsed == nil || !parsed.ExplicitModel || !IsGPTImageGenerationModel(parsed.Model) || !IsGPTImageGenerationModel(model) {
		return nil, imageLocalError(400, "invalid_model", "An explicit GPT image model is required")
	}
	if parsed.N != 1 {
		return nil, imageLocalError(422, "unsupported_parameter", "Asynchronous image generation requires n=1")
	}
	if parsed.Stream || parsed.HasMask {
		return nil, imageLocalError(422, "unsupported_parameter", "Naoshengyun asynchronous images do not support stream or mask")
	}
	if parsed.ResponseFormat != "" && parsed.ResponseFormat != "url" {
		return nil, imageLocalError(422, "unsupported_parameter", "Asynchronous image content is downloaded through Longdao; response_format must be url")
	}
	if strings.TrimSpace(parsed.Prompt) == "" {
		return nil, imageLocalError(400, "invalid_prompt", "prompt must not be empty")
	}
	if parsed.Size == "" {
		parsed.Size = "1024x1024"
		parsed.SizeTier = normalizeOpenAIImageSizeTier(parsed.Size)
	}
	if _, ok := ClassifyImageBillingTier(parsed.Size); !ok {
		return nil, imageLocalError(422, "invalid_size", "Asynchronous image generation requires an explicit billable image size, for example 1024x1024")
	}
	if parsed.Style != "" || parsed.InputFidelity != "" || parsed.PartialImages != nil {
		return nil, imageLocalError(422, "unsupported_parameter", "Naoshengyun asynchronous images do not support style, input_fidelity or partial_images")
	}
	if len(parsed.Uploads)+len(parsed.InputImageURLs) > asyncImageReferenceMaxCount {
		return nil, imageLocalError(422, "too_many_reference_images", "At most 4 reference images are supported")
	}
	if !parsed.IsEdits() && len(parsed.Uploads)+len(parsed.InputImageURLs) > 0 {
		return nil, imageLocalError(422, "unsupported_parameter", "Reference images must be submitted to /images/edits/async")
	}
	if parsed.IsEdits() && len(parsed.Uploads)+len(parsed.InputImageURLs) == 0 {
		return nil, imageLocalError(400, "missing_reference_image", "Image editing requires 1..4 reference images")
	}
	payload := map[string]any{"model": model, "prompt": parsed.Prompt, "n": 1, "response_format": "url"}
	for name, value := range map[string]string{"size": parsed.Size, "quality": parsed.Quality, "background": parsed.Background, "output_format": parsed.OutputFormat, "moderation": parsed.Moderation} {
		if value != "" {
			payload[name] = value
		}
	}
	if parsed.OutputCompression != nil {
		if *parsed.OutputCompression < 0 || *parsed.OutputCompression > 100 {
			return nil, imageLocalError(422, "invalid_compression", "output_compression must be 0..100")
		}
		payload["output_compression"] = *parsed.OutputCompression
	}
	images := make([]string, 0, len(parsed.Uploads)+len(parsed.InputImageURLs))
	for _, upload := range parsed.Uploads {
		uri, err := asyncReferenceDataURI(upload.Data)
		if err != nil {
			return nil, err
		}
		images = append(images, uri)
	}
	for _, raw := range parsed.InputImageURLs {
		if strings.HasPrefix(raw, "data:") {
			parts := strings.SplitN(raw, ",", 2)
			if len(parts) != 2 || !strings.HasSuffix(parts[0], ";base64") || len(parts[1]) > base64.StdEncoding.EncodedLen(asyncImageReferenceMaxBytes) {
				return nil, imageLocalError(422, "invalid_reference_image", "Expected a Base64 data URI of at most 20 MiB decoded")
			}
			data, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				return nil, imageLocalError(422, "invalid_reference_image", "Decode reference Base64: "+err.Error())
			}
			uri, err := asyncReferenceDataURI(data)
			if err != nil {
				return nil, err
			}
			images = append(images, uri)
		} else {
			if _, err := validateAsyncImageDownloadURL(raw); err != nil {
				return nil, imageLocalError(422, "invalid_reference_url", err.Error())
			}
			images = append(images, raw)
		}
	}
	if len(images) > 0 {
		payload["image_urls"] = images
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	limit := int64(asyncImageJSONMaxBytes)
	if maxBytes > 0 && maxBytes < limit {
		limit = maxBytes
	}
	if int64(len(data)) > limit {
		return nil, imageLocalError(413, "image_request_too_large", fmt.Sprintf("Encoded asynchronous image request exceeds %d bytes", limit))
	}
	return data, nil
}

type asyncImageJob struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Status     string `json:"status"`
	Created    int64  `json:"created"`
	FinishedAt *int64 `json:"finished_at"`
	Data       []struct {
		URL string `json:"url"`
	} `json:"data"`
	Error json.RawMessage `json:"error"`
}

func decodeAsyncImageJob(body []byte, expectedID string) (*asyncImageJob, error) {
	var job asyncImageJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, imageLocalError(502, "image_protocol_error", "Decode asynchronous image job: "+err.Error())
	}
	if !asyncImageJobPattern.MatchString(job.ID) || (expectedID != "" && job.ID != expectedID) || job.Object != "image.generation.job" || job.Created <= 0 {
		return nil, imageLocalError(502, "image_protocol_error", "Upstream image response has an invalid job ID, object or creation time")
	}
	switch job.Status {
	case "queued", "running", "succeeded", "failed":
	default:
		return nil, imageLocalError(502, "image_protocol_error", "Unsupported upstream image job status: "+job.Status)
	}
	if job.Status == "succeeded" {
		if len(job.Data) != 1 {
			return nil, imageLocalError(502, "image_protocol_error", "Upstream n=1 task must return exactly one image URL")
		}
		if _, err := validateAsyncImageDownloadURL(job.Data[0].URL); err != nil {
			return nil, err
		}
	}
	return &job, nil
}

func (s *OpenAIGatewayService) asyncImageRequest(ctx context.Context, c *gin.Context, account *Account, method, path string, body []byte) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, asyncImageDispatchTimeout)
	defer cancel()
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, nil, fmt.Errorf("Read image upstream credential: %w", err)
	}
	req, err := s.buildOpenAIImagesRequest(ctx, c, account, body, "application/json", token, openAIImagesGenerationsEndpoint)
	if err != nil {
		return nil, nil, err
	}
	// The standard URL builder normalizes account base URLs before appending /v1.
	req.URL.Path = strings.TrimSuffix(req.URL.Path, openAIImagesGenerationsEndpoint) + path
	req.URL.RawPath = ""
	req.Method = method
	if method == http.MethodGet {
		req.Body = nil
		req.GetBody = nil
		req.ContentLength = 0
		req.Header.Del("Content-Type")
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	MarkMediaDispatched(c)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		status := 502
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			status = 504
		}
		message := errordetail.Text(strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
		return nil, nil, &AsyncImageError{Status: status, Fields: map[string]any{"source": "transport", "code": "image_upstream_transport_error", "message": "Image upstream request failed: " + message}, Cause: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, asyncImageResponseMaxBytes+1))
	if err != nil {
		failure := imageProviderError(resp.StatusCode, resp.Header, data, token)
		failure.Cause = err
		failure.Fields["body_read_error"] = errordetail.Text(strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
		return nil, resp.Header, failure
	}
	if len(data) > asyncImageResponseMaxBytes {
		failure := imageProviderError(resp.StatusCode, resp.Header, data[:asyncImageResponseMaxBytes], token)
		failure.Fields["body_truncated"] = true
		return nil, resp.Header, failure
	}
	var envelope struct {
		Object string `json:"object"`
		ID     string `json:"id"`
	}
	_ = json.Unmarshal(data, &envelope)
	jobEnvelope := envelope.Object == "image.generation.job" && asyncImageJobPattern.MatchString(envelope.ID)
	if (method == http.MethodPost && resp.StatusCode != 202) || (method == http.MethodGet && resp.StatusCode != 200) || (UpstreamBodyError(data) && !jobEnvelope) {
		return nil, resp.Header, imageProviderError(resp.StatusCode, resp.Header, data, token)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, resp.Header, imageProviderError(resp.StatusCode, resp.Header, data, token)
	}
	return data, resp.Header, nil
}

func (s *OpenAIGatewayService) SubmitAsyncImage(ctx context.Context, c *gin.Context, tasks *ImageTaskService, key *APIKey, account *Account, parsed *OpenAIImagesRequest, payload []byte, requestID, idem string, billing *AsyncImageBillingSnapshot, updaters ...APIKeyQuotaUpdater) (*ImageTask, error) {
	var updater APIKeyQuotaUpdater
	if len(updaters) > 0 {
		updater = updaters[0]
	}
	fingerprint := asyncImageFingerprint(parsed, payload)
	model := struct {
		Model string `json:"model"`
	}{}
	_ = json.Unmarshal(payload, &model)
	meta := &AsyncImageMetadata{RequestID: requestID, IdempotencyKey: idem, Fingerprint: fingerprint, SubmissionStatus: "submitting", AccountID: account.ID, GroupID: derefGroupID(key.GroupID), UpstreamModel: model.Model, Protocol: ImageProtocolNaoshengyun, DispatchDeadline: time.Now().Add(asyncImageDispatchTimeout + 10*time.Second).Unix(), Billing: billing}
	record, created, err := tasks.ClaimAsync(ctx, key, meta)
	if err != nil {
		return nil, imageLocalError(503, "image_task_storage_error", "Claim image submission: "+err.Error())
	}
	output := imageTaskToPublic(record)
	output.Billing = s.AsyncImageBillingStatus(ctx, key, record)
	if !created {
		if record.Provider.Fingerprint != fingerprint {
			return output, imageLocalError(409, "image_request_conflict", "Request ID or Idempotency-Key was already used with different image parameters")
		}
		if record.Provider.SubmissionStatus == "submitting" {
			return output, imageLocalError(409, "image_submission_in_progress", "The original image submission is in progress; it has not been sent again")
		}
		if record.Provider.SubmissionStatus == "failed" {
			return output, asyncImageRecordFailure(record)
		}
		if record.Provider.AccountID != account.ID {
			original, loadErr := s.AsyncImageTaskAccount(ctx, key, record)
			if loadErr != nil {
				return output, loadErr
			}
			account = original
		}
		output.Billing = s.finishAsyncImageBilling(ctx, key, account, record, updater)
		return output, nil
	}
	if key.SEP != nil {
		if err = key.SEP.Service.RecordExecutionProvider(ctx, key, "", record.ID); err != nil {
			return output, imageLocalError(503, "sep_image_association_error", "Save SEP image execution association: "+err.Error())
		}
	}
	if err = s.registerAsyncImageBilling(ctx, record); err == nil {
		record.Provider.BillingRegistered = billing != nil
		record.Provider.DispatchDeadline = time.Now().Add(asyncImageDispatchTimeout + 10*time.Second).Unix()
		err = tasks.SaveAsync(ctx, record)
	}
	if err == nil {
		var body []byte
		var headers http.Header
		// Preserve the upstream receipt if SEP disconnects after dispatch. The
		// original submission deadline still bounds this single paid request.
		dispatchCtx, dispatchCancel := context.WithDeadline(context.WithoutCancel(ctx), time.Unix(record.Provider.DispatchDeadline-10, 0))
		body, headers, err = s.asyncImageRequest(dispatchCtx, c, account, http.MethodPost, "/v1/images/generations/async", payload)
		dispatchCancel()
		record.Provider.UpstreamRequestID = errordetail.RequestID(headers)
		if err == nil {
			var job *asyncImageJob
			job, err = decodeAsyncImageJob(body, "")
			if err == nil {
				record.Provider.SubmissionStatus = "accepted"
				record.Provider.UpstreamTaskID = job.ID
				applyAsyncImageJob(record, job, account.GetOpenAIApiKey())
			} else {
				var failure *AsyncImageError
				if errors.As(err, &failure) {
					failure.Fields["upstream_request_id"] = errordetail.RequestID(headers)
					failure.Fields["upstream_status"] = 202
					failure.Fields["upstream_response"] = errordetail.Fields(redactAsyncImageSecret(body, account.GetOpenAIApiKey()))
				}
			}
		}
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err != nil {
		var failure *AsyncImageError
		if !errors.As(err, &failure) {
			failure = imageLocalError(503, "image_submission_error", err.Error())
		}
		failAsyncImage(record, failure)
		slog.Warn("image_async.submission_failed", "task_id", record.ID, "request_id", requestID, "diagnostic", failure.Fields)
		output = imageTaskToPublic(record)
		output.Billing = s.finishAsyncImageBilling(saveCtx, key, account, record, nil)
		if saveErr := tasks.SaveAsync(saveCtx, record); saveErr != nil {
			failure.Fields["task_store_error"] = errordetail.Text(saveErr.Error())
		}
		return output, failure
	}
	if err = tasks.SaveAsync(saveCtx, record); err != nil {
		failure := imageLocalError(503, "image_task_storage_error", "Upstream accepted image task but saving task association failed; the request is closed without charging: "+err.Error())
		record.Provider.SubmissionStatus = "failed"
		failAsyncImage(record, failure)
		output = imageTaskToPublic(record)
		output.Billing = s.finishAsyncImageBilling(saveCtx, key, account, record, nil)
		return output, failure
	}
	output = imageTaskToPublic(record)
	output.Billing = s.finishAsyncImageBilling(saveCtx, key, account, record, updater)
	return output, nil
}

func asyncImageFingerprint(parsed *OpenAIImagesRequest, payload []byte) string {
	var semantic map[string]any
	_ = json.Unmarshal(payload, &semantic)
	semantic["model"] = parsed.Model
	canonical, _ := json.Marshal(semantic)
	return HashUsageRequestPayload(append([]byte(parsed.Endpoint+"\n"), canonical...))
}

func (s *OpenAIGatewayService) ReplayAsyncImage(ctx context.Context, tasks *ImageTaskService, key *APIKey, parsed *OpenAIImagesRequest, payload []byte, requestID, idem string, updaters ...APIKeyQuotaUpdater) (*ImageTask, bool, error) {
	var updater APIKeyQuotaUpdater
	if len(updaters) > 0 {
		updater = updaters[0]
	}
	record, err := tasks.FindAsyncClaim(ctx, key, requestID, idem)
	if err != nil {
		return nil, true, err
	}
	if record == nil {
		prior, err := s.ImageRequestOutcome(ctx, key, requestID)
		if err != nil {
			return nil, true, err
		}
		if prior != nil {
			return &ImageTask{SchemaVersion: 2, RequestID: requestID, Billing: &ImageTaskBillingStatus{Status: prior.Status, ActualCostUSD: prior.ActualCostUSD}}, true, imageLocalError(410, "image_task_association_expired", "Original image task association is unavailable; durable billing evidence is preserved and this request will not be submitted again")
		}
		return nil, false, nil
	}
	output := imageTaskToPublic(record)
	output.Billing = s.AsyncImageBillingStatus(ctx, key, record)
	if record.Provider.Fingerprint != asyncImageFingerprint(parsed, payload) {
		return output, true, imageLocalError(409, "image_request_conflict", "Request ID or Idempotency-Key was already used with different image parameters")
	}
	if record.Provider.SubmissionStatus == "submitting" {
		if time.Now().Unix() <= record.Provider.DispatchDeadline {
			return output, true, imageLocalError(409, "image_submission_in_progress", "Original image submission is still in progress; it has not been submitted again")
		}
		failAsyncImage(record, imageLocalError(504, "image_submission_interrupted", "Original image submission did not finish before its deadline; this request is closed without charging or resubmitting"))
		if err = tasks.SaveAsync(ctx, record); err != nil {
			return output, true, err
		}
		record, err = tasks.LoadRecord(ctx, ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}, record.ID)
		if err != nil {
			return output, true, err
		}
	}
	if record.Provider.SubmissionStatus == "failed" {
		output = imageTaskToPublic(record)
		output.Billing = s.finishAsyncImageBilling(ctx, key, nil, record, nil)
		return output, true, asyncImageRecordFailure(record)
	}
	account, err := s.AsyncImageTaskAccount(ctx, key, record)
	if err != nil {
		return output, true, err
	}
	output.Billing = s.finishAsyncImageBilling(ctx, key, account, record, updater)
	return output, true, nil
}

func applyAsyncImageJob(record *ImageTaskRecord, job *asyncImageJob, secret string) {
	p := record.Provider
	p.UpstreamStatus = job.Status
	if job.Status == "succeeded" {
		record.Status = ImageTaskStatusCompleted
		p.ResultURL = job.Data[0].URL
		record.HTTPStatus = 200
		record.CompletedAt = job.FinishedAt
	} else if job.Status == "failed" {
		failure := imageProviderError(200, nil, job.Error, secret)
		if len(job.Error) == 0 || string(job.Error) == "null" {
			failure.Fields["message"] = "Upstream image task reports failed but supplies no error details"
			failure.Fields["code"] = "image_task_error_details_missing"
		}
		if p.UpstreamRequestID != "" {
			failure.Fields["upstream_request_id"] = p.UpstreamRequestID
		}
		failAsyncImage(record, failure)
	}
	if (record.Status == ImageTaskStatusCompleted || record.Status == ImageTaskStatusFailed) && record.CompletedAt == nil {
		now := time.Now().Unix()
		record.CompletedAt = &now
	}
}

func (s *OpenAIGatewayService) PollAsyncImage(ctx context.Context, c *gin.Context, tasks *ImageTaskService, key *APIKey, record *ImageTaskRecord, account *Account, updater APIKeyQuotaUpdater) (*ImageTask, error) {
	output := imageTaskToPublic(record)
	if record.Provider.SubmissionStatus == "submitting" && time.Now().Unix() > record.Provider.DispatchDeadline {
		failAsyncImage(record, imageLocalError(504, "image_submission_interrupted", "Original image submission did not finish before its deadline; this request is closed without charging or resubmitting"))
		if err := tasks.SaveAsync(ctx, record); err != nil {
			return output, err
		}
		var err error
		record, err = tasks.LoadRecord(ctx, ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}, record.ID)
		if err != nil {
			return output, err
		}
		// A concurrent receipt can win the stale timeout update. Return that
		// receipt; the next query acquires its original account before polling.
		if record.Provider.SubmissionStatus == "accepted" && account == nil {
			output = imageTaskToPublic(record)
			output.Billing = s.AsyncImageBillingStatus(ctx, key, record)
			return output, nil
		}
	}
	if record.Provider.SubmissionStatus == "accepted" && record.Status == ImageTaskStatusProcessing {
		body, headers, err := s.asyncImageRequest(ctx, c, account, http.MethodGet, "/v1/images/jobs/"+record.Provider.UpstreamTaskID, nil)
		if err != nil {
			output.Billing = s.finishAsyncImageBilling(ctx, key, account, record, updater)
			return output, err
		}
		job, err := decodeAsyncImageJob(body, record.Provider.UpstreamTaskID)
		if err != nil {
			var failure *AsyncImageError
			if errors.As(err, &failure) {
				failure.Fields["upstream_status"] = 200
				failure.Fields["upstream_request_id"] = errordetail.RequestID(headers)
				failure.Fields["upstream_response"] = errordetail.Fields(redactAsyncImageSecret(body, account.GetOpenAIApiKey()))
			}
			output.Billing = s.finishAsyncImageBilling(ctx, key, account, record, updater)
			return output, err
		}
		record.Provider.UpstreamRequestID = errordetail.RequestID(headers)
		applyAsyncImageJob(record, job, account.GetOpenAIApiKey())
		if err = tasks.SaveAsync(ctx, record); err != nil {
			return output, err
		}
		// Reload after CAS save so stale polls cannot regress a terminal task.
		record, err = tasks.LoadRecord(ctx, ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}, record.ID)
		if err != nil {
			return output, err
		}
	}
	output = imageTaskToPublic(record)
	output.Billing = s.finishAsyncImageBilling(ctx, key, account, record, updater)
	return output, nil
}
