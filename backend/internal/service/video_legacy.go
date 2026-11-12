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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var legacyTaskPattern = regexp.MustCompile(`^mtask_[0-9A-HJKMNP-TV-Z]{26}$`)
var legacyErrorURLPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

type videoProtocolError struct {
	status        int
	code, message string
}

func (e *videoProtocolError) Error() string { return e.message }
func videoError(status int, code, message string) error {
	return &videoProtocolError{status, code, message}
}

type legacyVideoTask struct {
	ID        string `json:"id"`
	Model     string `json:"model_id"`
	Status    string `json:"status"`
	Final     *bool  `json:"is_final"`
	CreatedAt int64  `json:"created_at"`
	VideoURL  string `json:"video_url"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
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
	if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil || *envelope.Code != 0 || envelope.Data == nil {
		return nil, videoError(502, "video_protocol_error", "Invalid upstream video response")
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
	fields := request.fields
	if !request.Multipart {
		fields = make(map[string]string)
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
			if _, exists := fields[name]; exists {
				return nil, videoError(400, "invalid_request_error", "Duplicate field: "+name)
			}
			var raw json.RawMessage
			if err = decoder.Decode(&raw); err != nil {
				return nil, err
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
	allowed := map[string]bool{"model": true, "prompt": true, "seconds": true, "resolution": true, "aspect_ratio": true, "generate_audio": true, "size": true, "output_format": true}
	for name := range fields {
		if !allowed[name] {
			return nil, videoError(422, "unsupported_parameter", "Unsupported legacy video parameter: "+name)
		}
	}
	return fields, nil
}

func (s *OpenAIGatewayService) legacyParams(request *OpenAIVideoRequest, model, operation string) (map[string]any, []byte, error) {
	fields, err := legacyFields(request)
	if err != nil {
		return nil, nil, err
	}
	if request.otherFiles != 0 || len(request.moderation.InputImageURLs) != 0 {
		return nil, nil, videoError(422, "unsupported_parameter", "Only input_reference PNG uploads are supported")
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
	var image []byte
	switch operation {
	case "text-to-video":
		if len(request.moderation.Uploads) != 0 {
			return nil, nil, videoError(400, "invalid_request_error", "Text-to-video cannot include an image")
		}
	case "image-to-video":
		if len(request.moderation.Uploads) != 1 {
			return nil, nil, videoError(400, "invalid_request_error", "Exactly one input_reference PNG is required")
		}
		image = request.moderation.Uploads[0].Data
	default:
		return nil, nil, videoError(400, "invalid_request_error", "X-Video-Operation must be text-to-video or image-to-video")
	}
	return params, image, nil
}

type videoSubmission struct {
	SchemaVersion    int                `json:"schema_version"`
	SemanticSHA256   string             `json:"semantic_sha256"`
	AccountID        int64              `json:"account_id"`
	Protocol         string             `json:"protocol"`
	State            string             `json:"state"`
	ReferenceAssetID string             `json:"reference_asset_id,omitempty"`
	BillingRequestID string             `json:"billing_request_id"`
	Task             *VideoTaskMetadata `json:"task,omitempty"`
	ExpiresAt        int64              `json:"expires_at"`
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
			return nil, videoError(504, "video_upstream_timeout", "Video upstream timed out")
		}
		return nil, videoError(502, "video_upstream_error", "Video upstream request failed")
	}
	defer resp.Body.Close()
	expected := http.StatusOK
	if body != nil {
		expected = http.StatusAccepted
	}
	if resp.StatusCode != expected {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		detail := legacyErrorMessage(errorBody, body, account.GetOpenAIApiKey())
		slog.Warn("video_legacy.upstream_rejected", "account_id", account.ID, "task_id", task,
			"operation", method, "status", resp.StatusCode, "reason", detail)
		if resp.StatusCode == 429 {
			return nil, videoError(429, "rate_limit_error", "Video upstream is busy")
		}
		if resp.StatusCode == 404 && body == nil {
			return nil, videoError(404, "not_found_error", "Upstream video task unavailable")
		}
		message := fmt.Sprintf("Video upstream returned HTTP %d", resp.StatusCode)
		if detail != "" {
			message += ": " + detail
		}
		return nil, videoError(502, "video_upstream_error", message)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, videoError(502, "video_protocol_error", "Invalid upstream video response body")
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
	return decodeLegacyTask(data, task, wrapper.Data.Model)
}

func legacyErrorMessage(data, request []byte, apiKey string) string {
	var envelope struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return ""
	}
	return sanitizeLegacyErrorMessage(envelope.Message, request, apiKey)
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
	message = legacyErrorURLPattern.ReplaceAllString(message, "[redacted-url]")
	return truncateString(strings.Join(strings.Fields(message), " "), 512)
}

func legacyTaskFailure(task *legacyVideoTask, apiKey string) map[string]string {
	code, message := "", ""
	if task.Error != nil {
		code = sanitizeLegacyErrorMessage(task.Error.Code, nil, apiKey)
		message = sanitizeLegacyErrorMessage(task.Error.Message, nil, apiKey)
	}
	if code == "" {
		code = "video_generation_failed"
	}
	if message == "" {
		message = fmt.Sprintf("Upstream reported status=failed without error.message (task_id=%s)", task.ID)
	}
	return map[string]string{"code": code, "message": message}
}

func (s *OpenAIGatewayService) queryLegacyVideo(ctx context.Context, account *Account, key *APIKey, id string) (*legacyVideoTask, *VideoTaskMetadata, error) {
	meta, err := s.videoTaskMetadata(ctx, key, id)
	if err != nil {
		return nil, nil, videoError(503, "video_task_metadata_unavailable", "Video task association unavailable")
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
		return nil, nil, videoError(503, "video_task_metadata_unavailable", "Video task status storage unavailable")
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
				c.JSON(e.status, gin.H{"error": gin.H{"type": e.code, "code": e.code, "message": e.message}})
			} else {
				c.JSON(503, gin.H{"error": gin.H{"type": "video_service_unavailable", "message": "Video service unavailable"}})
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
	params, image, e := s.legacyParams(request, upstreamModel, c.GetHeader("X-Video-Operation"))
	if e != nil {
		return nil, e
	}
	digestImage := sha256.Sum256(image)
	semantic, _ := json.Marshal(map[string]any{"model": request.Model, "params": params, "image_sha256": hex.EncodeToString(digestImage[:])})
	digest := sha256.Sum256(semantic)
	submission := &videoSubmission{SchemaVersion: 1, SemanticSHA256: hex.EncodeToString(digest[:]), AccountID: account.ID, Protocol: VideoProtocolLegacy, State: "preparing", BillingRequestID: resolveUsageBillingRequestID(ctx, ""), ExpiresAt: time.Now().Unix() + 604800}
	idem := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	submissionKey := ""
	upstreamIdem := ""
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
			return nil, e
		}
		if !created {
			encoded, e = store.VideoLoad(ctx, submissionKey)
			if e != nil {
				return nil, e
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
				return nil, e
			}
			if strings.HasPrefix(previous.BillingRequestID, "client:") {
				c.Header("X-Client-Request-ID", strings.TrimPrefix(previous.BillingRequestID, "client:"))
			}
			c.JSON(202, normalizedVideoTask{Object: "video", ID: previous.Task.TaskID, Model: previous.Task.RequestedModel, Status: previous.Task.LastStatus})
			return nil, nil
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
	if image != nil {
		runtime, e := s.videoReferences()
		if e != nil {
			return nil, e
		}
		reference, e := runtime.save(ctx, key, image)
		if e != nil {
			return nil, e
		}
		submission.ReferenceAssetID = reference.AssetID
		params["content"] = append(params["content"].([]map[string]any), map[string]any{"type": "image_url", "image_url": map[string]string{"url": reference.SignedURL}, "role": "reference_image"})
	}
	body, e := json.Marshal(map[string]any{"model_id": upstreamModel, "params": params})
	if e != nil {
		return nil, e
	}
	if len(body) > 65536 {
		return nil, videoError(413, "invalid_request_error", "Legacy video request exceeds 64 KiB")
	}
	submission.State = "dispatching"
	if e = saveSubmission(); e != nil {
		return nil, e
	}
	started := time.Now()
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
	meta := &VideoTaskMetadata{SchemaVersion: 1, TaskID: task.ID, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: request.Model, UpstreamModel: upstreamModel, ReferenceAssetID: submission.ReferenceAssetID, BillingRequestID: submission.BillingRequestID, LastStatus: status, BoundAt: now, ExpiresAt: now + 604800}
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
	if meta.ReferenceAssetID != "" {
		if e = store.VideoReferenceTask(bindCtx, meta.ReferenceAssetID, task.ID); e != nil {
			slog.Warn("video_legacy.reference_task_update_failed", "task_id", task.ID)
		}
	}
	c.JSON(202, normalizedVideoTask{Object: "video", ID: task.ID, Model: request.Model, Status: status})
	return &OpenAIForwardResult{ResponseID: task.ID, Model: request.Model, BillingModel: upstreamModel, UpstreamModel: upstreamModel, VideoCount: 1, VideoResolution: request.Resolution, VideoDurationSeconds: request.Seconds, Duration: time.Since(started)}, nil
}
