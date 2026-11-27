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
	rejected      bool
	notDispatched bool
}

func (e *videoProtocolError) Error() string { return e.message }
func (e *videoProtocolError) Unwrap() error { return e.cause }

func (e *videoProtocolError) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"status": e.status, "code": e.code, "message": e.message, "diagnostic": e.diagnostic, "rejected": e.rejected})
}
func (e *videoProtocolError) UnmarshalJSON(data []byte) error {
	var value struct {
		Status     int            `json:"status"`
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Diagnostic map[string]any `json:"diagnostic"`
		Rejected   bool           `json:"rejected"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	e.status, e.code, e.message, e.diagnostic, e.rejected = value.Status, value.Code, value.Message, value.Diagnostic, value.Rejected
	return nil
}

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
	videoContract
	Object string         `json:"object"`
	ID     any            `json:"id"`
	Model  string         `json:"model,omitempty"`
	Status any            `json:"status"`
	Error  map[string]any `json:"error,omitempty"`
}

func legacyStatus(t *legacyVideoTask) (string, error) {
	statuses := map[string]string{"queued": "queued", "processing": "in_progress", "succeeded": "completed", "failed": "failed", "canceled": "cancelled"}
	status, ok := statuses[t.Status]
	final := status == "completed" || status == "failed" || status == "cancelled"
	if !ok || t.Final == nil || *t.Final != final {
		return "", videoError(502, "video_protocol_error", fmt.Sprintf("Invalid upstream video status=%q, is_final=%s", t.Status, mustVideoDiagnosticJSON(t.Final)))
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
	if expectedID != "" && envelope.Message != "success" {
		return task, videoError(502, "video_protocol_error", "Invalid upstream task response: expected message=success")
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
			if name == "metadata" && len(request.referenceContent) > 0 {
				continue
			}
			if !allowed[name] && !(name == "duration" && len(request.referenceContent) > 0) {
				ignored = append(ignored, name)
				continue
			}
			switch name {
			case "seconds", "duration":
				var v json.Number
				if json.Unmarshal(raw, &v) != nil || len(raw) == 0 || raw[0] == '"' {
					return nil, videoError(400, "invalid_request_error", name+" must be a JSON integer")
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
	if err := request.ValidateVideoReferences(VideoProtocolLegacy, operation); err != nil {
		return nil, nil, err
	}
	references := request.legacyReferences
	rawDuration := fields["seconds"]
	if len(references) > 0 {
		rawDuration = firstNonEmptyString(rawDuration, fields["duration"], "5")
	}
	duration, err := strconv.Atoi(rawDuration)
	if err != nil || !ValidSeedanceReferenceDuration(model, duration) || (len(references) == 0 && duration == VideoAutoDuration) {
		return nil, nil, videoError(422, "unsupported_duration", "Unsupported legacy video duration")
	}
	if len(references) > 0 {
		if err := validateVideoReferenceCounts(references, model); err != nil {
			return nil, nil, err
		}
		if fields["resolution"] == "" && fields["size"] == "" {
			fields["resolution"] = VideoBillingResolution720P
		}
		switch fields["resolution"] {
		case "480p", "720p", "1080p", "2k", "4k":
		default:
			return nil, nil, videoError(422, "unsupported_spec", "Unsupported Seedance resolution")
		}
		switch fields["aspect_ratio"] {
		case "", "adaptive", "1:1", "4:3", "3:4", "16:9", "9:16", "21:9":
		default:
			return nil, nil, videoError(422, "unsupported_spec", "Unsupported Seedance aspect ratio")
		}
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
	if len(references) > 0 {
		params["content"] = references
	}
	if ratio := fields["aspect_ratio"]; ratio != "" {
		params["ratio"] = ratio
	}
	if audio, present := fields["generate_audio"]; present {
		if audio != "true" && audio != "false" {
			return nil, nil, videoError(400, "invalid_request_error", "generate_audio must be true or false")
		}
		params["generate_audio"] = audio == "true"
	}
	return params, request.legacyImages, nil
}

type videoSubmission struct {
	SchemaVersion     int                   `json:"schema_version"`
	SemanticSHA256    string                `json:"semantic_sha256"`
	AccountID         int64                 `json:"account_id"`
	Protocol          string                `json:"protocol"`
	State             string                `json:"state"`
	ReferenceAssetID  string                `json:"reference_asset_id,omitempty"`
	ReferenceAssetIDs []string              `json:"reference_asset_ids,omitempty"`
	BillingRequestID  string                `json:"billing_request_id"`
	Task              *VideoTaskMetadata    `json:"task,omitempty"`
	ExpiresAt         int64                 `json:"expires_at"`
	RequestID         string                `json:"request_id,omitempty"`
	APIKeyID          int64                 `json:"api_key_id,omitempty"`
	GroupID           int64                 `json:"group_id,omitempty"`
	UpstreamModel     string                `json:"upstream_model,omitempty"`
	Billing           *VideoBillingSnapshot `json:"billing,omitempty"`
	BillingRegistered bool                  `json:"billing_registered,omitempty"`
	Failure           *videoProtocolError   `json:"failure,omitempty"`
	Retry             *VideoSubmissionRetry `json:"submission_retry,omitempty"`
	DispatchDeadline  int64                 `json:"dispatch_deadline,omitempty"`
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
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	return s.legacyRequestMethod(ctx, account, task, body, idem, method)
}

func (s *OpenAIGatewayService) legacyRequestMethod(ctx context.Context, account *Account, task string, body []byte, idem, method string) (*legacyVideoTask, error) {
	base, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		failure := videoCause(503, "video_configuration_error", "Validate legacy upstream base URL", err).(*videoProtocolError)
		failure.notDispatched = true
		return nil, failure
	}
	target, err := legacyVideoURL(base, task)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPut {
		target += "/cancel"
	}
	if account.GetOpenAIApiKey() == "" {
		failure := videoError(503, "video_configuration_error", "Video account is missing api_key").(*videoProtocolError)
		failure.notDispatched = true
		return nil, failure
	}
	requestContext := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI))
	req, err := http.NewRequestWithContext(requestContext, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// A paid POST must not be replayed by the transport after an ambiguous failure.
	if method != http.MethodGet {
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
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		truncated := len(errorBody) > 65536
		if truncated {
			errorBody = errorBody[:65536]
		}
		errorBody = []byte(sanitizeLegacyErrorMessage(string(errorBody), body, account.GetOpenAIApiKey()))
		detail := legacyErrorMessage(legacyErrorFields(errorBody), nil, "")
		slog.Warn("video_legacy.upstream_rejected", "account_id", account.ID, "task_id", task, "operation", method, "status", resp.StatusCode, "upstream_request_id", errordetail.RequestID(resp.Header), "reason", detail, "read_error", safeVideoError(readErr))
		status, code := 502, "video_upstream_error"
		if resp.StatusCode == 429 {
			status, code = 429, "rate_limit_error"
		}
		if resp.StatusCode == 404 && body == nil {
			status, code = 404, "not_found_error"
		}
		failure := videoUpstreamError(status, code, resp.StatusCode, resp.Header, legacyErrorFields(errorBody))
		var recovered struct {
			Code *int             `json:"code"`
			Data *legacyVideoTask `json:"data"`
		}
		_ = json.Unmarshal(errorBody, &recovered)
		failure.(*videoProtocolError).rejected = resp.StatusCode >= 400 || (recovered.Code != nil && *recovered.Code != 0) || UpstreamBodyError(errorBody)
		if truncated {
			failure.(*videoProtocolError).diagnostic["body_truncated"] = true
			failure.(*videoProtocolError).diagnostic["body_limit_bytes"] = 65536
		}
		if resp.StatusCode == 409 && method == http.MethodPut {
			failure.(*videoProtocolError).status = 409
			failure.(*videoProtocolError).code = "video_cancel_conflict"
		}
		if retry := resp.Header.Get("Retry-After"); retry != "" && len(retry) <= 256 && !strings.ContainsAny(retry, "\r\n") {
			failure.(*videoProtocolError).diagnostic["retry_after"] = errordetail.Text(retry)
		}
		if readErr != nil {
			failure.(*videoProtocolError).cause = readErr
			failure.(*videoProtocolError).diagnostic["body_read_error"] = errordetail.Text(readErr.Error())
		}
		if recovered.Data != nil && legacyTaskPattern.MatchString(recovered.Data.ID) {
			recovered.Data.UpstreamRequestID = errordetail.RequestID(resp.Header)
			return recovered.Data, failure
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
	if decodeErr := json.Unmarshal(data, &wrapper); decodeErr != nil {
		failure := videoCause(502, "video_protocol_error", "Decode upstream video JSON", decodeErr).(*videoProtocolError)
		failure.diagnostic = errordetail.Diagnostic(resp.StatusCode, resp.Header, []byte(sanitizeLegacyErrorMessage(string(data), body, account.GetOpenAIApiKey())))
		failure.diagnostic["body_text"] = sanitizeLegacyErrorMessage(string(data), body, account.GetOpenAIApiKey())
		failure.diagnostic["message"] = failure.message
		var identity struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &identity) == nil && legacyTaskPattern.MatchString(identity.Data.ID) {
			return &legacyVideoTask{ID: identity.Data.ID}, failure
		}
		return nil, failure
	}
	result, decodeErr := decodeLegacyTask(data, task, wrapper.Data.Model)
	if decodeErr != nil && result == nil {
		var recovered struct {
			Code *int             `json:"code"`
			Data *legacyVideoTask `json:"data"`
		}
		_ = json.Unmarshal(data, &recovered)
		if recovered.Data != nil && legacyTaskPattern.MatchString(recovered.Data.ID) {
			result = recovered.Data
		}
		if failure, ok := decodeErr.(*videoProtocolError); ok {
			failure.diagnostic = errordetail.Diagnostic(resp.StatusCode, resp.Header, legacyErrorFields(data))
			failure.diagnostic["message"] = failure.message
			failure.rejected = result == nil && ((recovered.Code != nil && *recovered.Code != 0) || UpstreamBodyError(data))
		}
	}
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

func legacyErrorFields(body []byte) []byte {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	if metadata, ok := root["ResponseMetadata"].(map[string]any); ok {
		if failure, ok := metadata["Error"].(map[string]any); ok {
			if root["message"] == nil {
				root["message"] = failure["Message"]
			}
			if root["code"] == nil {
				root["code"] = failure["Code"]
			}
		}
		if root["request_id"] == nil {
			root["request_id"] = metadata["RequestId"]
		}
	}
	data, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return data
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
			Content []videoReferenceContent `json:"content"`
		} `json:"params"`
	}
	if json.Unmarshal(request, &input) == nil {
		for _, content := range input.Params.Content {
			if content.Text != "" {
				message = strings.ReplaceAll(message, content.Text, "[redacted-prompt]")
			}
			if media := content.mediaURL(); media != nil && media.URL != "" {
				message = strings.ReplaceAll(message, media.URL, "[redacted-reference]")
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
	fields["operation"] = "generation"
	if task.UpstreamRequestID != "" {
		fields["upstream_request_id"] = task.UpstreamRequestID
	}
	return fields
}

func (s *OpenAIGatewayService) queryLegacyVideo(ctx context.Context, account *Account, key *APIKey, id string) (*legacyVideoTask, *VideoTaskMetadata, error) {
	return s.observeLegacyVideo(ctx, account, key, id, false)
}

func (s *OpenAIGatewayService) observeLegacyVideo(ctx context.Context, account *Account, key *APIKey, id string, cancel bool) (*legacyVideoTask, *VideoTaskMetadata, error) {
	meta, err := s.videoTaskMetadata(ctx, key, id)
	if err != nil {
		return nil, nil, videoCause(503, "video_task_metadata_unavailable", "Read video task association from Redis", err)
	}
	if meta.AccountID != account.ID || meta.Protocol != VideoProtocolLegacy {
		return nil, nil, videoError(502, "video_protocol_error", "Video task association mismatch")
	}
	if cancel && meta.LastStatus == "cancelled" {
		return nil, meta, nil
	}
	method := http.MethodGet
	if cancel {
		method = http.MethodPut
	}
	task, err := s.legacyRequestMethod(ctx, account, id, nil, "", method)
	if err != nil {
		return nil, nil, err
	}
	if task.Model != meta.UpstreamModel {
		return nil, nil, videoError(502, "video_protocol_error", fmt.Sprintf("Upstream video model mismatch: expected %q, received %q", meta.UpstreamModel, task.Model))
	}
	if cancel && task.Status != "canceled" {
		return nil, nil, videoError(502, "video_protocol_error", "Upstream cancellation response did not confirm status=canceled")
	}
	status, _ := legacyStatus(task)
	store, _ := s.videoStore()
	var failure map[string]any
	if task.Status == "failed" {
		failure = legacyTaskFailure(task, account.GetOpenAIApiKey())
	}
	meta, err = store.VideoObserve(ctx, VideoTaskMetadataKey(meta.GroupID, meta.APIKeyID, id), status, time.Now().UnixMilli(), failure)
	if err != nil {
		return nil, nil, videoCause(503, "video_task_metadata_unavailable", "Write video task status to Redis", err)
	}
	if key.SEP != nil && meta.Billing == nil {
		if err = key.SEP.Service.ObserveVideo(ctx, key, id, sepJSON(map[string]any{"status": meta.LastStatus})); err != nil {
			slog.Warn("video_legacy.sep_observation_failed", "task_id", id, "reason", safeVideoError(err))
		}
	}
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
			writeVideoError(c, err)
		}
	}()
	SetActualOpenAIUpstreamEndpoint(c, "/v1/video/tasks")
	if request == nil {
		if content {
			return nil, s.downloadLegacyVideo(ctx, c, account, key, taskID)
		}
		task, meta, e := s.observeLegacyVideo(ctx, account, key, taskID, c.Request.Method == http.MethodPut)
		if e != nil {
			return nil, e
		}
		source := "upstream"
		if task == nil {
			source = "cached"
		} else if observed, _ := legacyStatus(task); observed != meta.LastStatus {
			source = "cached"
			slog.Warn("video_legacy.status_conflict", "task_id", taskID, "recorded_status", meta.LastStatus, "upstream_status", observed)
		}
		s.contractFromTask(ctx, c, key, meta, source)
		contractFor(c).Billing = s.finishVideoBilling(ctx, key, account, meta, videoQuotaUpdater(ctx))
		writeVideoJSON(c, 200, normalizedVideoTask{Object: "video", ID: taskID, Model: meta.RequestedModel, Status: meta.LastStatus, Error: meta.TaskError})
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
	billing, e := s.videoBillingSnapshot(ctx, key, request)
	if e != nil {
		return nil, videoCause(503, "video_pricing_unavailable", "Freeze video billing quote", e)
	}
	requestID := contractFor(c).RequestID
	if requestID == "" {
		requestID = strings.TrimPrefix(resolveUsageBillingRequestID(ctx, ""), "client:")
		contractFor(c).RequestID = requestID
	}
	submission := &videoSubmission{SchemaVersion: 1, SemanticSHA256: legacyVideoSemanticHash(request.Model, upstreamModel, params, images), AccountID: account.ID, Protocol: VideoProtocolLegacy, State: "preparing", BillingRequestID: "client:" + requestID, ExpiresAt: time.Now().Unix() + 604800, RequestID: requestID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), UpstreamModel: upstreamModel, Billing: billing}
	idem := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idem == "" {
		idem = requestID
	}
	submissionKey := ""
	upstreamIdem := ""
	dispatched := false
	if idem != "" {
		if len(idem) > 255 && c.GetHeader("Idempotency-Key") != "" {
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
			_, e := s.ReplayVideoCreate(ctx, c, key, request)
			return nil, e
		}
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
	defer func() {
		if err == nil {
			return
		}
		if !dispatched {
			submission.State = "not_submitted"
		}
		var failure *videoProtocolError
		if errors.As(err, &failure) {
			submission.Failure = failure
		} else {
			submission.Failure = &videoProtocolError{status: 503, code: "video_service_error", message: safeVideoError(err)}
		}
		if saveErr := saveSubmission(); saveErr != nil {
			slog.Warn("video_legacy.submission_result_store_failed", "request_id", requestID, "reason", safeVideoError(saveErr))
		}
		v := contractFor(c)
		v.RequestID = requestID
		v.SubmissionStatus = submission.State
		v.SubmissionRetry = submission.Retry
		if videoSubmissionClosed(submission) {
			v.Billing = s.closedVideoSubmissionBilling(ctx, key, submission)
		} else {
			v.Billing = s.VideoRequestBillingStatus(ctx, key, requestID)
		}
	}()
	index := videoRequestIndexKey(key, requestID)
	if ok, indexErr := store.VideoSave(ctx, index, []byte(submissionKey), time.Unix(submission.ExpiresAt, 0), true); indexErr != nil {
		return nil, videoCause(503, "video_submission_unavailable", "Save video request index", indexErr)
	} else if !ok {
		previous, indexErr := store.VideoLoad(ctx, index)
		if indexErr != nil {
			return nil, indexErr
		}
		if string(previous) != submissionKey {
			return nil, videoError(409, "idempotency_conflict", "Request ID is already associated with another video submission")
		}
	}
	if e = s.registerVideoBilling(ctx, submission); e != nil {
		return nil, videoCause(503, "video_billing_unavailable", "Register video billing outcome", e)
	}
	submission.BillingRegistered = billing != nil && (s.cfg == nil || s.cfg.RunMode != "simple")
	if request.UsesUploadedVideoReference() {
		items, assets, err := s.resolveVideoReferenceFiles(ctx, key, request.legacyReferences)
		if err != nil {
			return nil, err
		}
		params["content"] = items
		submission.ReferenceAssetIDs = assets
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
	submission.Retry = &VideoSubmissionRetry{MaxAttempts: videoSubmissionMaxAttempts}
	submission.DispatchDeadline = time.Now().Add(videoSubmissionTimeout + 10*time.Second).Unix()
	if e = saveSubmission(); e != nil {
		submission.State = "preparing"
		return nil, videoError(503, "video_submission_unavailable", "Mark video submission as dispatching in Redis: "+e.Error())
	}
	dispatched = true
	task, e := s.createLegacyVideoWithRetry(ctx, account, body, upstreamIdem, submission, saveSubmission)
	contractFor(c).SubmissionRetry = submission.Retry
	if e != nil {
		submission.State = "failed"
		var failure *videoProtocolError
		if errors.As(e, &failure) && failure.rejected && !submission.Retry.Exhausted {
			submission.State = "rejected"
		}
		if failure != nil && failure.notDispatched {
			submission.State = "not_submitted"
		}
		if task != nil && legacyTaskPattern.MatchString(task.ID) {
			if failure == nil || (!failure.rejected && !failure.notDispatched) {
				submission.State = "protocol_error"
			}
			now := time.Now().Unix()
			submission.Task = &VideoTaskMetadata{SchemaVersion: 1, TaskID: task.ID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: request.Model, UpstreamModel: upstreamModel, BillingRequestID: submission.BillingRequestID, RequestID: requestID, SubmissionKey: submissionKey, BoundAt: now, ExpiresAt: submission.ExpiresAt, Billing: billing}
			submission.Task.SubmissionRetry = submission.Retry
			submission.Task.SubmissionStatus = submission.State
			_ = store.VideoBind(context.WithoutCancel(ctx), submission.Task)
			id := task.ID
			contractFor(c).TaskID = &id
		}
		_ = saveSubmission()
		return nil, e
	}
	if task.Model != upstreamModel {
		submission.State = "protocol_error"
		now := time.Now().Unix()
		submission.Task = &VideoTaskMetadata{SchemaVersion: 1, TaskID: task.ID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: request.Model, UpstreamModel: upstreamModel, BillingRequestID: submission.BillingRequestID, RequestID: requestID, SubmissionKey: submissionKey, BoundAt: now, ExpiresAt: submission.ExpiresAt, Billing: billing}
		submission.Task.SubmissionRetry = submission.Retry
		submission.Task.SubmissionStatus = submission.State
		_ = store.VideoBind(context.WithoutCancel(ctx), submission.Task)
		id := task.ID
		contractFor(c).TaskID = &id
		_ = saveSubmission()
		return nil, videoError(502, "video_protocol_error", fmt.Sprintf("Upstream video model mismatch: expected %q, received %q", upstreamModel, task.Model))
	}
	status, _ := legacyStatus(task)
	now := time.Now().Unix()
	meta := &VideoTaskMetadata{SchemaVersion: 1, TaskID: task.ID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: request.Model, UpstreamModel: upstreamModel, ReferenceAssetID: submission.ReferenceAssetID, ReferenceAssetIDs: submission.ReferenceAssetIDs, BillingRequestID: submission.BillingRequestID, LastStatus: status, BoundAt: now, ExpiresAt: submission.ExpiresAt, RequestID: requestID, SubmissionKey: submissionKey, ObservedAt: time.Now().UnixMilli(), Billing: billing}
	meta.SubmissionRetry = submission.Retry
	if status == "failed" {
		meta.TaskError = legacyTaskFailure(task, account.GetOpenAIApiKey())
	}
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
	if key.SEP != nil {
		if err = key.SEP.Service.RecordExecutionProvider(bindCtx, key, "", task.ID); err != nil {
			slog.Warn("video_legacy.sep_association_failed", "task_id", task.ID, "reason", safeVideoError(err))
		}
	}
	s.contractFromTask(bindCtx, c, key, meta, "upstream")
	contractFor(c).Billing = s.finishVideoBilling(bindCtx, key, account, meta, videoQuotaUpdater(ctx))
	writeVideoJSON(c, 202, normalizedVideoTask{Object: "video", ID: task.ID, Model: request.Model, Status: status, Error: meta.TaskError})
	return nil, nil
}
