package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
)

var legacyTaskPattern = regexp.MustCompile(`^mtask_[0-9A-HJKMNP-TV-Z]{26}$`)

func safeVideoError(err error) string {
	if err == nil {
		return ""
	}
	return errordetail.Text(err.Error())
}

func mustVideoDiagnosticJSON(value any) []byte { body, _ := json.Marshal(value); return body }

type videoProtocolError struct {
	status        int
	code, message string
	cause         error
	diagnostic    map[string]any
}

func (e *videoProtocolError) Error() string { return e.message }
func (e *videoProtocolError) Unwrap() error { return e.cause }

func videoCause(status int, code, message string, cause error) error {
	return &videoProtocolError{status: status, code: code, message: errordetail.Text(message + ": " + cause.Error()), cause: cause}
}

func videoUpstreamError(status int, code string, upstreamStatus int, headers http.Header, body []byte) error {
	d := errordetail.Diagnostic(upstreamStatus, headers, body)
	return &videoProtocolError{status: status, code: code, message: fmt.Sprint(d["message"]), diagnostic: d}
}

func videoError(status int, code, message string) error {
	return &videoProtocolError{status: status, code: code, message: errordetail.Text(message)}
}

type legacyVideoTask struct {
	ID                string          `json:"id"`
	Model             string          `json:"model_id"`
	Status            string          `json:"status"`
	Final             *bool           `json:"is_final"`
	CreatedAt         int64           `json:"created_at"`
	VideoURL          string          `json:"video_url"`
	RawError          json.RawMessage `json:"-"`
	UpstreamRequestID string          `json:"-"`
	Error             *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (t *legacyVideoTask) UnmarshalJSON(body []byte) error {
	type taskAlias legacyVideoTask
	var decoded struct {
		*taskAlias
		Error json.RawMessage `json:"error"`
	}
	decoded.taskAlias = (*taskAlias)(t)
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	t.RawError = decoded.Error
	if len(decoded.Error) > 0 && string(decoded.Error) != "null" {
		_ = json.Unmarshal(decoded.Error, &t.Error)
	}
	return nil
}

type normalizedVideoTask struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Error  any    `json:"error,omitempty"`
}

func legacyStatus(t *legacyVideoTask) (string, error) {
	statuses := map[string]string{"queued": "queued", "processing": "in_progress", "succeeded": "completed", "failed": "failed", "canceled": "cancelled"}
	status, ok := statuses[t.Status]
	final := status == "completed" || status == "failed" || status == "cancelled"
	if !ok || t.Final == nil || *t.Final != final {
		return "", videoError(502, "video_protocol_error", "Invalid upstream video status")
	}
	return status, nil
}

func decodeLegacyTask(data []byte, expectedID, model string) (*legacyVideoTask, error) {
	var envelope struct {
		Code    *int             `json:"code"`
		Message string           `json:"message"`
		Data    *legacyVideoTask `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, videoCause(502, "video_protocol_error", "Decode upstream video response JSON", err)
	}
	if envelope.Code == nil || *envelope.Code != 0 || envelope.Data == nil {
		return nil, videoUpstreamError(502, "video_protocol_error", 200, nil, data)
	}
	task := envelope.Data
	if !legacyTaskPattern.MatchString(task.ID) || (expectedID != "" && task.ID != expectedID) || task.Model != model || task.CreatedAt <= 0 {
		return nil, videoError(502, "video_protocol_error", "Upstream video identity mismatch")
	}
	if expectedID == "" && envelope.Message != "accepted" {
		return nil, videoError(502, "video_protocol_error", "Invalid upstream acceptance response")
	}
	if _, err := legacyStatus(task); err != nil {
		return nil, err
	}
	return task, nil
}

func legacyVideoURL(base, task string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	for _, suffix := range []string{"/v1/video/tasks", "/v1/video/generations", "/v1/videos", "/v1"} {
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), suffix) {
			u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), suffix)
			break
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/video/tasks"
	u.RawPath = ""
	if task != "" {
		if !legacyTaskPattern.MatchString(task) {
			return "", videoError(400, "invalid_request_error", "Invalid legacy video task ID")
		}
		u.Path += "/" + task
	}
	return u.String(), nil
}

func legacyFields(request *OpenAIVideoRequest) (map[string]string, error) {
	allowed := map[string]bool{"model": true, "prompt": true, "seconds": true, "resolution": true, "aspect_ratio": true, "generate_audio": true, "size": true, "output_format": true}
	fields := make(map[string]string)
	var ignored []string
	defer func() {
		if len(ignored) > 0 {
			sort.Strings(ignored)
			slog.Info("video_legacy.parameters_ignored", "fields", ignored)
		}
	}()
	if request.Multipart {
		for name, value := range request.fields {
			if allowed[name] {
				fields[name] = value
			} else {
				ignored = append(ignored, name)
			}
		}
	} else {
		seen := make(map[string]bool)
		decoder := json.NewDecoder(bytes.NewReader(request.Body))
		if _, err := decoder.Token(); err != nil {
			return nil, videoError(400, "invalid_request_error", "Invalid JSON")
		}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := token.(string)
			if !ok {
				return nil, fmt.Errorf("invalid JSON key")
			}
			if seen[name] {
				return nil, videoError(400, "invalid_request_error", "Duplicate field: "+name)
			}
			seen[name] = true
			var raw json.RawMessage
			if err = decoder.Decode(&raw); err != nil {
				return nil, err
			}
			if !allowed[name] {
				ignored = append(ignored, name)
				continue
			}
			switch name {
			case "seconds":
				var v json.Number
				if json.Unmarshal(raw, &v) != nil || len(raw) == 0 || raw[0] == '"' {
					return nil, videoError(400, "invalid_request_error", "seconds must be a JSON integer")
				}
				fields[name] = v.String()
			case "generate_audio":
				var v bool
				if string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
					return nil, videoError(400, "invalid_request_error", "generate_audio must be a JSON boolean")
				}
				fields[name] = strconv.FormatBool(v)
			default:
				var v string
				if string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
					return nil, videoError(422, "unsupported_parameter", "Unsupported parameter type: "+name)
				}
				fields[name] = v
			}
		}
	}
	return fields, nil
}

func (s *OpenAIGatewayService) legacyParams(request *OpenAIVideoRequest, model, operation string) (map[string]any, []videoReferenceInput, error) {
	fields, err := legacyFields(request)
	if err != nil {
		return nil, nil, err
	}
	if request.otherFiles != 0 || len(request.moderation.InputImageURLs) != 0 {
		return nil, nil, videoError(422, "unsupported_parameter", "Only input_reference image uploads are supported")
	}
	switch model {
	case "seedance-2.0", "seedance-2.0-fast", "seedance-2.0-mini", "seedance-2.5":
	default:
		return nil, nil, videoError(422, "unsupported_spec", "Unsupported legacy video model")
	}
	duration, err := strconv.Atoi(fields["seconds"])
	maxDuration := 15
	if model == "seedance-2.5" {
		maxDuration = 30
	}
	if err != nil || duration < 4 || duration > maxDuration {
		return nil, nil, videoError(422, "unsupported_duration", "Unsupported legacy video duration")
	}
	if strings.TrimSpace(fields["resolution"]) == "" {
		return nil, nil, videoError(400, "invalid_request_error", "resolution is required")
	}
	if fields["output_format"] != "" && fields["output_format"] != "video/mp4" {
		return nil, nil, videoError(422, "unsupported_parameter", "Only video/mp4 output is supported")
	}
	if fields["size"] != "" {
		parts := strings.Split(fields["size"], "x")
		if len(parts) != 2 {
			return nil, nil, videoError(422, "unsupported_spec", "Invalid size")
		}
		w, e1 := strconv.Atoi(parts[0])
		h, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || w <= 0 || h <= 0 || w > 16384 || h > 16384 || strconv.Itoa(min(w, h))+"p" != fields["resolution"] {
			return nil, nil, videoError(422, "unsupported_spec", "size conflicts with resolution")
		}
		if ratio := fields["aspect_ratio"]; ratio != "" {
			parts = strings.Split(ratio, ":")
			if len(parts) != 2 {
				return nil, nil, videoError(422, "unsupported_spec", "Invalid ratio")
			}
			a, e1 := strconv.Atoi(parts[0])
			b, e2 := strconv.Atoi(parts[1])
			if e1 != nil || e2 != nil || a <= 0 || b <= 0 || int64(w)*int64(b) != int64(h)*int64(a) {
				return nil, nil, videoError(422, "unsupported_spec", "size conflicts with aspect_ratio")
			}
		}
	}
	params := map[string]any{"duration": duration, "resolution": fields["resolution"], "content": []map[string]any{{"type": "text", "text": request.Prompt}}}
	if ratio := fields["aspect_ratio"]; ratio != "" {
		params["ratio"] = ratio
	}
	if audio, present := fields["generate_audio"]; present {
		if audio != "true" && audio != "false" {
			return nil, nil, videoError(400, "invalid_request_error", "generate_audio must be true or false")
		}
		params["generate_audio"] = audio == "true"
	}
	if err := request.ValidateVideoReferences(VideoProtocolLegacy, operation); err != nil {
		return nil, nil, err
	}
	return params, request.legacyImages, nil
}

type videoSubmission struct {
	SchemaVersion     int                `json:"schema_version"`
	SemanticSHA256    string             `json:"semantic_sha256"`
	AccountID         int64              `json:"account_id"`
	Protocol          string             `json:"protocol"`
	State             string             `json:"state"`
	ReferenceAssetID  string             `json:"reference_asset_id,omitempty"`
	ReferenceAssetIDs []string           `json:"reference_asset_ids,omitempty"`
	BillingRequestID  string             `json:"billing_request_id"`
	Task              *VideoTaskMetadata `json:"task,omitempty"`
	ExpiresAt         int64              `json:"expires_at"`
}

func legacyVideoSemanticHash(model, upstreamModel string, params map[string]any, images []videoReferenceInput) string {
	semantic := map[string]any{"model": model, "params": params}
	if len(images) <= 1 {
		if len(images) == 1 {
			semantic["image_sha256"] = images[0].sha256
		} else {
			digest := sha256.Sum256(nil)
			semantic["image_sha256"] = hex.EncodeToString(digest[:])
		}
	} else {
		digests := make([]string, 0, len(images))
		for _, image := range images {
			digests = append(digests, image.sha256)
		}
		semantic["image_sha256s"] = digests
		semantic["upstream_model"] = upstreamModel
	}
	encoded, _ := json.Marshal(semantic)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (s *OpenAIGatewayService) legacyRequest(ctx context.Context, account *Account, task string, body []byte, idem string) (*legacyVideoTask, error) {
	base, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		return nil, videoError(503, "video_configuration_error", "Invalid legacy upstream base URL")
	}
	target, err := legacyVideoURL(base, task)
	if err != nil {
		return nil, err
	}
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	if account.GetOpenAIApiKey() == "" {
		return nil, videoError(503, "video_configuration_error", "Video account is missing api_key")
	}
	requestContext := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI))
	req, err := http.NewRequestWithContext(requestContext, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// A paid POST must not be replayed by the transport after an ambiguous failure.
	if body != nil {
		req.GetBody = nil
	}
	account.ApplyHeaderOverrides(req.Header)
	req.Header.Set("Authorization", "Bearer "+account.GetOpenAIApiKey())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
	}
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, videoCause(504, "video_upstream_timeout", "Video upstream request timed out", err)
		}
		return nil, videoCause(502, "video_upstream_error", "Send video upstream request", err)
	}
	defer resp.Body.Close()
	expected := http.StatusOK
	if body != nil {
		expected = http.StatusAccepted
	}
	if resp.StatusCode != expected {
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 65536))
		errorBody = []byte(sanitizeLegacyErrorMessage(string(errorBody), body, account.GetOpenAIApiKey()))
		detail := legacyErrorMessage(errorBody, nil, "")
		slog.Warn("video_legacy.upstream_rejected", "account_id", account.ID, "task_id", task, "operation", method, "status", resp.StatusCode, "upstream_request_id", errordetail.RequestID(resp.Header), "reason", detail, "read_error", safeVideoError(readErr))
		status, code := 502, "video_upstream_error"
		if resp.StatusCode == 429 {
			status, code = 429, "rate_limit_error"
		}
		if resp.StatusCode == 404 && body == nil {
			status, code = 404, "not_found_error"
		}
		failure := videoUpstreamError(status, code, resp.StatusCode, resp.Header, errorBody)
		if readErr != nil {
			failure.(*videoProtocolError).cause = readErr
			failure.(*videoProtocolError).diagnostic["body_read_error"] = errordetail.Text(readErr.Error())
		}
		return nil, failure
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return nil, videoCause(502, "video_protocol_error", "Read upstream video response body", err)
	}
	if len(data) > 65536 {
		return nil, videoError(502, "video_protocol_error", "Upstream video response body exceeds 65536 bytes")
	}
	// The expected model is checked by the caller against the frozen task identity.
	var wrapper struct {
		Data struct {
			Model string `json:"model_id"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &wrapper) != nil {
		return nil, videoError(502, "video_protocol_error", "Invalid upstream video JSON")
	}
	result, decodeErr := decodeLegacyTask(data, task, wrapper.Data.Model)
	if failure, ok := decodeErr.(*videoProtocolError); ok && failure.diagnostic != nil {
		failure.diagnostic["upstream_status"] = resp.StatusCode
		if id := errordetail.RequestID(resp.Header); id != "" {
			failure.diagnostic["upstream_request_id"] = id
		}
		failure.message = sanitizeLegacyErrorMessage(failure.message, body, account.GetOpenAIApiKey())
		failure.diagnostic = errordetail.Fields([]byte(sanitizeLegacyErrorMessage(string(mustVideoDiagnosticJSON(failure.diagnostic)), body, account.GetOpenAIApiKey())))
	}
	if result != nil {
		result.UpstreamRequestID = errordetail.RequestID(resp.Header)
	}
	return result, decodeErr
}

func legacyErrorMessage(data, request []byte, apiKey string) string {
	return sanitizeLegacyErrorMessage(extractUpstreamErrorMessage(data), request, apiKey)
}

func sanitizeLegacyErrorMessage(message string, request []byte, apiKey string) string {
	if apiKey != "" {
		message = strings.ReplaceAll(message, apiKey, "[redacted-key]")
	}
	var input struct {
		Params struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"params"`
	}
	if json.Unmarshal(request, &input) == nil {
		for _, content := range input.Params.Content {
			if content.Text != "" {
				message = strings.ReplaceAll(message, content.Text, "[redacted-prompt]")
			}
		}
	}
	return string(errordetail.Body([]byte(message)))
}

func legacyTaskFailure(task *legacyVideoTask, apiKey string) map[string]any {
	fields := errordetail.Fields([]byte(sanitizeLegacyErrorMessage(string(task.RawError), nil, apiKey)))
	if task.Error != nil {
		if fields["code"] == nil || fmt.Sprint(fields["code"]) == "" {
			fields["code"] = sanitizeLegacyErrorMessage(task.Error.Code, nil, apiKey)
		}
		if fields["message"] == nil || fmt.Sprint(fields["message"]) == "" {
			fields["message"] = sanitizeLegacyErrorMessage(task.Error.Message, nil, apiKey)
		}
	}
	if fields["code"] == nil || fmt.Sprint(fields["code"]) == "" {
		fields["code"] = "video_generation_failed"
	}
	if fields["message"] == nil || fmt.Sprint(fields["message"]) == "" {
		fields["message"] = fmt.Sprintf("Upstream reported status=failed without error.message (task_id=%s)", task.ID)
	}
	fields["source"] = "upstream"
	if task.UpstreamRequestID != "" {
		fields["upstream_request_id"] = task.UpstreamRequestID
	}
	return fields
}

func (s *OpenAIGatewayService) queryLegacyVideo(ctx context.Context, account *Account, key *APIKey, id string) (*legacyVideoTask, *VideoTaskMetadata, error) {
	meta, err := s.videoTaskMetadata(ctx, key, id)
	if err != nil {
		return nil, nil, videoCause(503, "video_task_metadata_unavailable", "Read video task association from Redis", err)
	}
	if meta.AccountID != account.ID || meta.Protocol != VideoProtocolLegacy {
		return nil, nil, videoError(502, "video_protocol_error", "Video task association mismatch")
	}
	task, err := s.legacyRequest(ctx, account, id, nil, "")
	if err != nil {
		return nil, nil, err
	}
	if task.Model != meta.UpstreamModel {
		return nil, nil, videoError(502, "video_protocol_error", "Upstream video model mismatch")
	}
	status, _ := legacyStatus(task)
	store, _ := s.videoStore()
	status, err = store.VideoStatus(ctx, VideoTaskMetadataKey(meta.GroupID, meta.APIKeyID, id), status)
	if err != nil {
		return nil, nil, videoCause(503, "video_task_metadata_unavailable", "Write video task status to Redis", err)
	}
	meta.LastStatus = status
	if task.Status == "failed" {
		failure := legacyTaskFailure(task, account.GetOpenAIApiKey())
		slog.Warn("video_legacy.task_failed", "task_id", id, "account_id", account.ID,
			"api_key_id", key.ID, "model", meta.UpstreamModel, "billing_request_id", meta.BillingRequestID,
			"upstream_code", failure["code"], "reason", failure["message"])
	}
	return task, meta, nil
}

func (s *OpenAIGatewayService) forwardLegacyVideo(ctx context.Context, c *gin.Context, account *Account, key *APIKey, request *OpenAIVideoRequest, taskID string, content bool, mappedModel string) (result *OpenAIForwardResult, err error) {
	defer func() {
		if err != nil && !c.Writer.Written() {
			var e *videoProtocolError
			if errors.As(err, &e) {
				fields := gin.H{"type": e.code, "code": e.code, "message": e.message, "source": "platform"}
				if e.diagnostic != nil {
					for k, v := range e.diagnostic {
						fields[k] = v
					}
					fields["platform_code"] = e.code
				}
				if id, ok := fields["upstream_request_id"].(string); ok && !strings.ContainsAny(id, "\r\n") {
					c.Header("X-Upstream-Request-ID", id)
				}
				setOpsUpstreamError(c, e.status, e.message, string(mustVideoDiagnosticJSON(fields)))
				c.JSON(e.status, gin.H{"error": fields})
			} else {
				setOpsUpstreamError(c, 0, errordetail.Text(err.Error()), "")
				c.JSON(503, gin.H{"error": gin.H{"type": "video_service_error", "source": "platform", "message": errordetail.Text(err.Error())}})
			}
		}
	}()
	SetActualOpenAIUpstreamEndpoint(c, "/v1/video/tasks")
	if request == nil {
		if content {
			return nil, s.downloadLegacyVideo(ctx, c, account, key, taskID)
		}
		task, meta, e := s.queryLegacyVideo(ctx, account, key, taskID)
		if e != nil {
			return nil, e
		}
		output := normalizedVideoTask{Object: "video", ID: taskID, Model: meta.RequestedModel, Status: meta.LastStatus}
		if meta.LastStatus == "failed" {
			output.Error = legacyTaskFailure(task, account.GetOpenAIApiKey())
		}
		c.JSON(200, output)
		return nil, nil
	}
	store, e := s.videoStore()
	if e != nil {
		return nil, e
	}
	upstreamModel := resolveOpenAIForwardModel(account, request.Model, mappedModel)
	params, images, e := s.legacyParams(request, upstreamModel, c.GetHeader("X-Video-Operation"))
	if e != nil {
		return nil, e
	}
	submission := &videoSubmission{SchemaVersion: 1, SemanticSHA256: legacyVideoSemanticHash(request.Model, upstreamModel, params, images), AccountID: account.ID, Protocol: VideoProtocolLegacy, State: "preparing", BillingRequestID: resolveUsageBillingRequestID(ctx, ""), ExpiresAt: time.Now().Unix() + 604800}
	idem := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	submissionKey := ""
	upstreamIdem := ""
	var reservation []byte
	dispatched := false
	if idem != "" {
		if len(idem) > 255 {
			return nil, videoError(400, "invalid_request_error", "Idempotency-Key is too long")
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", derefGroupID(key.GroupID), key.ID, idem)))
		upstreamIdem = hex.EncodeToString(digest[:])
		submissionKey = "video:submission:" + upstreamIdem
		encoded, _ := json.Marshal(submission)
		created, e := store.VideoSave(ctx, submissionKey, encoded, time.Unix(submission.ExpiresAt, 0), true)
		if e != nil {
			return nil, videoError(503, "video_submission_unavailable", "Create video submission in Redis: "+e.Error())
		}
		if !created {
			encoded, e = store.VideoLoad(ctx, submissionKey)
			if e != nil {
				return nil, videoError(503, "video_submission_unavailable", "Read video submission from Redis: "+e.Error())
			}
			var previous videoSubmission
			if json.Unmarshal(encoded, &previous) != nil || previous.SchemaVersion != 1 {
				return nil, videoError(503, "video_submission_unavailable", "Submission record is invalid")
			}
			if previous.SemanticSHA256 != submission.SemanticSHA256 {
				return nil, videoError(409, "idempotency_conflict", "Idempotency-Key was used with different parameters")
			}
			if previous.Task == nil || previous.State != "accepted" {
				return nil, videoError(409, "submission_unknown", "Original video submission is pending confirmation")
			}
			if e = store.VideoBind(ctx, previous.Task); e != nil {
				return nil, videoError(503, "video_submission_unavailable", "Restore video task binding in Redis: "+e.Error())
			}
			if strings.HasPrefix(previous.BillingRequestID, "client:") {
				c.Header("X-Client-Request-ID", strings.TrimPrefix(previous.BillingRequestID, "client:"))
			}
			c.JSON(202, normalizedVideoTask{Object: "video", ID: previous.Task.TaskID, Model: previous.Task.RequestedModel, Status: previous.Task.LastStatus})
			return nil, nil
		}
		reservation = encoded
		defer func() {
			if !dispatched {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cancel()
				if e := store.VideoDeleteIfEqual(cleanupCtx, submissionKey, reservation); e != nil {
					slog.Warn("video_legacy.reservation_cleanup_failed", "billing_request_id", submission.BillingRequestID, "reason", e.Error())
				}
			}
		}()
	}
	saveSubmission := func() error {
		if submissionKey == "" {
			return nil
		}
		data, _ := json.Marshal(submission)
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		ok, e := store.VideoReplace(saveCtx, submissionKey, data)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("video submission record expired")
		}
		return nil
	}
	if len(images) != 0 {
		runtime, e := s.videoReferences()
		if e != nil {
			return nil, videoError(503, "video_reference_storage_error", "Initialize video reference storage: "+e.Error())
		}
		references, e := runtime.saveBatch(ctx, key, images)
		if e != nil {
			return nil, e
		}
		defer func() {
			if submission.State == "preparing" {
				runtime.removeReferences(references)
			}
		}()
		items := params["content"].([]map[string]any)
		for _, ref := range references {
			submission.ReferenceAssetIDs = append(submission.ReferenceAssetIDs, ref.AssetID)
			items = append(items, map[string]any{"type": "image_url", "image_url": map[string]string{"url": ref.SignedURL}, "role": "reference_image"})
		}
		if len(references) == 1 {
			submission.ReferenceAssetID = references[0].AssetID
		}
		params["content"] = items
	}
	body, e := json.Marshal(map[string]any{"model_id": upstreamModel, "params": params})
	if e != nil {
		return nil, e
	}
	if len(body) > 65536 {
		return nil, videoError(413, "invalid_request_error", fmt.Sprintf("Legacy video JSON size %d bytes exceeds limit 65536 bytes", len(body)))
	}
	submission.State = "dispatching"
	if e = saveSubmission(); e != nil {
		submission.State = "preparing"
		return nil, videoError(503, "video_submission_unavailable", "Mark video submission as dispatching in Redis: "+e.Error())
	}
	started := time.Now()
	dispatched = true
	task, e := s.legacyRequest(ctx, account, "", body, upstreamIdem)
	if e != nil {
		submission.State = "unknown"
		_ = saveSubmission()
		return nil, e
	}
	if task.Model != upstreamModel {
		submission.State = "unknown"
		_ = saveSubmission()
		return nil, videoError(502, "video_protocol_error", "Upstream video model mismatch")
	}
	status, _ := legacyStatus(task)
	now := time.Now().Unix()
	meta := &VideoTaskMetadata{SchemaVersion: 1, TaskID: task.ID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: request.Model, UpstreamModel: upstreamModel, ReferenceAssetID: submission.ReferenceAssetID, ReferenceAssetIDs: submission.ReferenceAssetIDs, BillingRequestID: submission.BillingRequestID, LastStatus: status, BoundAt: now, ExpiresAt: now + 604800}
	submission.Task = meta
	submission.State = "accepted"
	bindCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Keep the accepted ID even when association storage is temporarily degraded.
	if e = saveSubmission(); e != nil {
		slog.Warn("video_legacy.accepted_submission_store_failed", "task_id", task.ID, "account_id", account.ID)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if e = store.VideoBind(bindCtx, meta); e == nil {
			break
		}
	}
	if e != nil {
		slog.Warn("video_legacy.accepted_binding_failed", "task_id", task.ID, "account_id", account.ID)
	}
	for _, asset := range meta.referenceAssets() {
		if e = store.VideoReferenceTask(bindCtx, asset, task.ID); e != nil {
			slog.Warn("video_legacy.reference_task_update_failed", "task_id", task.ID, "asset_id", asset, "reason", e.Error())
		}
	}
	c.JSON(202, normalizedVideoTask{Object: "video", ID: task.ID, Model: request.Model, Status: status})
	return &OpenAIForwardResult{ResponseID: task.ID, Model: request.Model, BillingModel: upstreamModel, UpstreamModel: upstreamModel, VideoCount: 1, VideoResolution: request.Resolution, VideoDurationSeconds: request.Seconds, Duration: time.Since(started)}, nil
}
