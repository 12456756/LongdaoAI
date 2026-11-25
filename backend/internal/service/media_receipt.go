package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const mediaSnapshotKey = "media_receipt_snapshot"
const mediaDispatchedKey = "media_receipt_dispatched"
const mediaBillableKey = "media_receipt_billable"

// MediaEndpoint deliberately excludes batch jobs and non-media protocols.
func MediaEndpoint(c *gin.Context) (kind, operation string) {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "", ""
	}
	p := strings.TrimPrefix(c.Request.URL.Path, "/v1")
	switch {
	case p == "/sub2api/images/pricing":
		return "image", "pricing"
	case p == "/sub2api/videos/pricing":
		return "video", "pricing"
	case p == "/sub2api/usage":
		return "image", "usage"
	case p == "/sub2api/videos/usage":
		return "video", "usage"
	case p == "/videos/references":
		return "video", "upload"
	case strings.HasPrefix(p, "/images/tasks/"):
		kind = "image"
	case p == "/images/generations" || p == "/images/edits" || p == "/images/generations/async" || p == "/images/edits/async":
		return "image", "create"
	case p == "/videos" || p == "/video/generations" || strings.HasPrefix(p, "/videos/"):
		kind = "video"
	default:
		return "", ""
	}
	if strings.HasSuffix(p, "/content") {
		return kind, "download"
	}
	if strings.HasSuffix(p, "/cancel") {
		return kind, "cancel"
	}
	if c.Request.Method == http.MethodPost {
		return kind, "create"
	}
	return kind, "query"
}

func MarkMediaDispatched(c *gin.Context) { c.Set(mediaDispatchedKey, true) }
func MarkMediaBillable(c *gin.Context)   { c.Set(mediaBillableKey, true) }

func SetSynchronousImageSuccess(c *gin.Context) {
	MarkMediaBillable(c)
	c.Set(mediaSnapshotKey, map[string]any{"submission_status": "accepted", "status": "completed", "is_final": true, "status_source": "upstream", "status_observed_at": time.Now().UnixMilli()})
}

// Snapshots are set only after ownership verification, before slots or polling.
func SetImageReceipt(c *gin.Context, task *ImageTask) {
	if task == nil {
		return
	}
	b, _ := json.Marshal(task)
	var snapshot map[string]any
	_ = json.Unmarshal(b, &snapshot)
	if task.TaskID == "" {
		snapshot["task_id"] = nil
	}
	if task.RequestID == "" {
		delete(snapshot, "request_id")
	}
	if task.Billing == nil {
		snapshot["billing"] = map[string]any{"status": "unknown", "actual_cost_usd": nil, "error": map[string]any{"source": "platform", "code": "image_billing_unobserved", "message": "No billing evidence was attached to this background image task"}}
	}
	snapshot["is_final"] = task.Status == ImageTaskStatusCompleted || task.Status == ImageTaskStatusFailed
	snapshot["status_observed_at"] = nil
	if task.CompletedAt != nil {
		snapshot["status_observed_at"] = *task.CompletedAt * 1000
	}
	snapshot["status_source"] = "cached"
	snapshot["task_error"] = nil
	if len(task.Error) > 0 {
		var e any
		if json.Unmarshal(task.Error, &e) == nil {
			snapshot["task_error"] = e
		}
	}
	c.Set(mediaSnapshotKey, snapshot)
}

func mediaDefaults(c *gin.Context) map[string]any {
	kind, operation := MediaEndpoint(c)
	id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	v := map[string]any{
		"schema_version": 2, "request_id": id, "task_id": nil, "operation": operation,
		"submission_status": "unobserved", "status": nil, "is_final": nil,
		"status_observed_at": nil, "status_source": nil, "task_error": nil,
		"operation_error": nil, "error": nil, "submission_retry": nil,
		"submission_error": nil,
		"billing":          map[string]any{"status": "unknown", "actual_cost_usd": nil, "error": nil},
	}
	if operation == "create" {
		v["submission_status"] = "not_submitted"
		v["billing"] = map[string]any{"status": "not_charged", "actual_cost_usd": "0", "error": nil}
		if c.GetBool(mediaDispatchedKey) {
			v["submission_status"] = "failed"
		}
		if c.GetBool(mediaBillableKey) {
			v["submission_status"] = "accepted"
			v["billing"] = map[string]any{"status": "pending", "actual_cost_usd": nil, "error": nil}
		}
	} else if operation == "pricing" {
		v["submission_status"] = "not_applicable"
		v["billing"] = map[string]any{"status": "not_applicable", "actual_cost_usd": nil, "error": nil}
	} else if operation == "upload" {
		v["submission_status"] = "not_applicable"
		v["billing"] = map[string]any{"status": "not_charged", "actual_cost_usd": "0", "error": nil}
	}
	if kind == "video" && HasVideoContract(c) {
		data, _ := json.Marshal(contractFor(c))
		_ = json.Unmarshal(data, &v)
	}
	if snapshot, ok := c.Get(mediaSnapshotKey); ok {
		for name, value := range snapshot.(map[string]any) {
			v[name] = value
		}
	}
	v["operation"] = operation
	return v
}

// CompleteMediaReceipt adds nullable fields without rewriting provider diagnostics.
// The operation error is separate from an already observed task failure.
func CompleteMediaReceipt(c *gin.Context, status int, payload map[string]any) map[string]any {
	v := mediaDefaults(c)
	for name, value := range payload {
		v[name] = value
	}
	v["schema_version"] = 2
	_, operation := MediaEndpoint(c)
	v["operation"] = operation
	v["receipt_operation"] = operation
	if operation == "pricing" {
		if requestedOperation, exists := payload["operation"]; exists {
			v["operation"] = requestedOperation
		}
	}
	if status < 400 && operation == "create" && v["task_id"] != nil {
		v["submission_status"] = "accepted"
	}
	if status >= 400 {
		fields, ok := v["operation_error"].(map[string]any)
		if !ok || fields == nil {
			fields, _ = payload["error"].(map[string]any)
		}
		if fields == nil {
			fields = map[string]any{}
			if e, ok := payload["error"].(string); ok {
				fields["message"] = errordetail.Text(e)
			}
			for _, name := range []string{"code", "message", "type", "reason", "details", "param"} {
				if value, exists := payload[name]; exists {
					fields[name] = value
				}
			}
		}
		if _, ok := fields["source"]; !ok {
			fields["source"] = "platform"
		}
		if message, ok := fields["message"].(string); !ok || strings.TrimSpace(message) == "" {
			fields["message"] = fmt.Sprintf("Media operation returned HTTP %d without an error message", status)
		}
		if _, ok := fields["code"]; !ok {
			fields["code"] = "media_operation_error"
		}
		if _, ok := fields["type"]; !ok {
			fields["type"] = "api_error"
		}
		fields = errordetail.Value(fields).(map[string]any)
		fields["operation"] = operation
		v["error"], v["operation_error"] = fields, fields
		if operation == "create" {
			v["submission_error"] = fields
		}
		if operation == "create" && c.GetBool(mediaDispatchedKey) && fields["source"] == "upstream" {
			if current, _ := v["submission_status"].(string); current == "failed" || current == "not_submitted" {
				v["submission_status"] = "rejected"
			}
		}
	}
	// A usage response's top-level status is the billing status, not task status.
	if operation == "usage" && status < 400 {
		v["billing"] = map[string]any{"status": payload["status"], "actual_cost_usd": payload["actual_cost_usd"], "error": nil}
	}
	if billing, ok := v["billing"].(map[string]any); ok {
		if _, exists := billing["error"]; !exists {
			billing["error"] = nil
		}
		if billing["status"] == "unknown" && billing["error"] == nil {
			billing["error"] = map[string]any{"source": "platform", "code": "media_billing_unobserved", "message": "No confirmed billing evidence was loaded; this operation result does not establish whether the original task was charged"}
		}
	}
	if operation != "usage" {
		if taskStatus, ok := v["status"].(string); ok && taskStatus != "" {
			v["is_final"] = taskStatus == "completed" || taskStatus == "failed" || taskStatus == "cancelled"
		}
	}
	if id, ok := v["task_id"].(string); ok && id == "" {
		v["task_id"] = nil
	}
	if status >= 400 {
		if value, ok := c.Get("media_receipt_persist"); ok {
			value.(func(map[string]any, int))(v, status)
		}
	}
	return v
}

func WriteMediaJSON(c *gin.Context, status int, value any) {
	if kind, _ := MediaEndpoint(c); kind == "" {
		c.JSON(status, value)
		return
	}
	data, err := json.Marshal(value)
	var payload map[string]any
	if err != nil || json.Unmarshal(data, &payload) != nil || payload == nil {
		c.JSON(status, value)
		return
	}
	c.Header("Cache-Control", "no-store")
	receipt := CompleteMediaReceipt(c, status, payload)
	c.Writer.Header().Del("Content-Length")
	c.Set("media_receipt_encoded", true)
	defer c.Set("media_receipt_encoded", false)
	c.JSON(status, receipt)
}

// Large synchronous image results are already bounded by the upstream reader.
// Retain their raw values instead of decoding Base64 images into interfaces.
func WriteMediaRawJSON(c *gin.Context, status int, body []byte) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		WriteMediaJSON(c, 502, map[string]any{"error": map[string]any{"source": "upstream", "code": "media_protocol_error", "message": "Upstream media response is not a JSON object"}})
		return
	}
	metadata := CompleteMediaReceipt(c, status, map[string]any{})
	for name, value := range metadata {
		encoded, _ := json.Marshal(value)
		payload[name] = encoded
	}
	c.Writer.Header().Del("Content-Length")
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(status)
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, name)
	}
	sort.Strings(names)
	_, _ = c.Writer.WriteString("{")
	for i, name := range names {
		if i > 0 {
			_, _ = c.Writer.WriteString(",")
		}
		encoded, _ := json.Marshal(name)
		_, _ = c.Writer.Write(encoded)
		_, _ = c.Writer.WriteString(":")
		if _, err := c.Writer.Write(payload[name]); err != nil {
			return
		}
	}
	_, _ = c.Writer.WriteString("}")
}

// Only local failed creates without billable output are eligible for this cache.
// Durable billing evidence always takes priority in the usage endpoint.
func (s *OpenAIGatewayService) SaveMediaFailure(ctx context.Context, key *APIKey, kind string, receipt map[string]any) error {
	if key == nil || receipt["operation"] != "create" || receipt["task_id"] != nil {
		return nil
	}
	b, _ := json.Marshal(receipt["billing"])
	var billing ImageTaskBillingStatus
	if json.Unmarshal(b, &billing) != nil || billing.Status != "not_charged" {
		return nil
	}
	id, _ := receipt["request_id"].(string)
	if !videoRequestIDPattern.MatchString(id) {
		return nil
	}
	store, err := s.videoStore()
	if err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = store.VideoSave(ctx, mediaFailureKey(key, kind, id), data, time.Now().Add(7*24*time.Hour), true)
	return err
}

func mediaFailureKey(key *APIKey, kind, id string) string {
	return fmt.Sprintf("media:failed:%s:%d:%d:%d:%s", kind, key.UserID, derefGroupID(key.GroupID), key.ID, DeriveSessionHashFromSeed(id))
}

func (s *OpenAIGatewayService) MediaFailure(ctx context.Context, key *APIKey, kind, id string) (map[string]any, error) {
	if s == nil || key == nil {
		return nil, nil
	}
	if _, ok := s.cache.(VideoStateStore); !ok {
		return nil, nil
	}
	store, err := s.videoStore()
	if err != nil {
		return nil, err
	}
	data, err := store.VideoLoad(ctx, mediaFailureKey(key, kind, id))
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, fmt.Errorf("Decode saved media failure: %w", err)
	}
	return receipt, nil
}
