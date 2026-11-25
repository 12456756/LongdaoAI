package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const videoContractKey = "video_response_contract"

type videoContractRuntime struct {
	service *OpenAIGatewayService
	key     *APIKey
}

func (s *OpenAIGatewayService) BindVideoContract(c *gin.Context, key *APIKey) {
	if s != nil {
		c.Set("video_contract_runtime", videoContractRuntime{s, key})
	}
}
func HasVideoContract(c *gin.Context) bool { _, ok := c.Get(videoContractKey); return ok }

var videoRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,256}$`)
var videoUSDPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,12})?$`)

type videoContract struct {
	SchemaVersion    int                   `json:"schema_version"`
	RequestID        string                `json:"request_id"`
	TaskID           *string               `json:"task_id"`
	Operation        string                `json:"operation"`
	SubmissionStatus string                `json:"submission_status"`
	Status           *string               `json:"status"`
	IsFinal          *bool                 `json:"is_final"`
	StatusObservedAt *int64                `json:"status_observed_at"`
	StatusSource     *string               `json:"status_source"`
	TaskError        map[string]any        `json:"task_error"`
	Billing          VideoBillingStatus    `json:"billing"`
	SubmissionRetry  *VideoSubmissionRetry `json:"submission_retry,omitempty"`
}

func videoOperation(c *gin.Context) string {
	if c.Request.Method == http.MethodPost {
		return "create"
	}
	if c.Request.Method == http.MethodPut {
		return "cancel"
	}
	if strings.HasSuffix(c.Request.URL.Path, "/content") {
		return "download"
	}
	return "query"
}

func contractFor(c *gin.Context) *videoContract {
	if v, ok := c.Get(videoContractKey); ok {
		return v.(*videoContract)
	}
	id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	state := "unknown"
	billing := VideoBillingStatus{Status: "unknown"}
	if videoOperation(c) == "create" {
		state = "not_submitted"
		zero := "0"
		billing = VideoBillingStatus{Status: "not_charged", ActualCostUSD: &zero}
	}
	v := &videoContract{SchemaVersion: 2, RequestID: id, Operation: videoOperation(c), SubmissionStatus: state, Billing: billing}
	c.Set(videoContractKey, v)
	return v
}

// InitVideoContract keeps the original creation identity distinct from polling IDs.
func InitVideoContract(c *gin.Context) error {
	v := contractFor(c)
	c.Header("Cache-Control", "no-store")
	if videoOperation(c) == "create" {
		values := c.Request.Header.Values("X-Client-Request-ID")
		if len(values) > 1 || (len(values) == 1 && !videoRequestIDPattern.MatchString(values[0])) {
			return videoError(400, "invalid_request_error", "Expected one valid X-Client-Request-ID, at most 256 characters")
		}
		if len(values) == 1 {
			v.RequestID = values[0]
		}
		if v.RequestID == "" {
			v.RequestID = strings.TrimPrefix(resolveUsageBillingRequestID(c.Request.Context(), ""), "generated:")
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, v.RequestID))
		c.Header("X-Client-Request-ID", v.RequestID)
		idem := c.Request.Header.Values("Idempotency-Key")
		if len(idem) > 1 || (len(idem) == 1 && (strings.TrimSpace(idem[0]) == "" || len(strings.TrimSpace(idem[0])) > 255)) {
			return videoError(400, "invalid_request_error", "Expected one nonempty Idempotency-Key, at most 255 characters")
		}
	}
	return nil
}

func writeVideoJSON(c *gin.Context, status int, output normalizedVideoTask) {
	output.videoContract = *contractFor(c)
	if output.RequestID != "" {
		c.Header("X-Video-Request-ID", output.RequestID)
	}
	c.Header("Cache-Control", "no-store")
	WriteMediaJSON(c, status, output)
}

func WriteVideoError(c *gin.Context, status int, code, message string) {
	writeVideoError(c, &videoProtocolError{status: status, code: code, message: errordetail.Text(message)})
}

func WriteVideoFailure(c *gin.Context, err error) { writeVideoError(c, err) }

type videoQuotaUpdaterKey struct{}

func WithVideoQuotaUpdater(ctx context.Context, updater APIKeyQuotaUpdater) context.Context {
	return context.WithValue(ctx, videoQuotaUpdaterKey{}, updater)
}
func videoQuotaUpdater(ctx context.Context) APIKeyQuotaUpdater {
	updater, _ := ctx.Value(videoQuotaUpdaterKey{}).(APIKeyQuotaUpdater)
	return updater
}

func writeVideoError(c *gin.Context, err error) {
	var e *videoProtocolError
	if !errors.As(err, &e) {
		e = &videoProtocolError{status: 503, code: "video_service_error", message: errordetail.Text(err.Error())}
	}
	fields := videoFailureFields(e)
	fields["operation"] = videoOperation(c)
	if recordingErr := persistVideoLocalRejection(c, e); recordingErr != nil {
		fields["submission_record_error"] = safeVideoError(recordingErr)
	}
	setOpsUpstreamError(c, e.status, e.message, string(mustVideoDiagnosticJSON(fields)))
	payload := map[string]any{}
	data, _ := json.Marshal(contractFor(c))
	_ = json.Unmarshal(data, &payload)
	payload["error"] = fields
	if id := contractFor(c).RequestID; id != "" {
		c.Header("X-Video-Request-ID", id)
	}
	if id, ok := fields["upstream_request_id"].(string); ok && !strings.ContainsAny(id, "\r\n") {
		c.Header("X-Upstream-Request-ID", id)
	}
	if retry, ok := fields["retry_after"].(string); ok && !strings.ContainsAny(retry, "\r\n") {
		c.Header("Retry-After", retry)
	}
	c.Header("Cache-Control", "no-store")
	payload["operation_error"] = fields
	WriteMediaJSON(c, e.status, payload)
}

func persistVideoLocalRejection(c *gin.Context, failure *videoProtocolError) error {
	v := contractFor(c)
	if v.Operation != "create" || v.SubmissionStatus != "not_submitted" || !videoRequestIDPattern.MatchString(v.RequestID) {
		return nil
	}
	value, ok := c.Get("video_contract_runtime")
	if !ok {
		return nil
	}
	runtime := value.(videoContractRuntime)
	store, err := runtime.service.videoStore()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
	defer cancel()
	index := videoRequestIndexKey(runtime.key, v.RequestID)
	if _, err = store.VideoLoad(ctx, index); err == nil {
		previous, loadErr := runtime.service.loadVideoSubmission(ctx, runtime.key, v.RequestID)
		if loadErr != nil {
			return loadErr
		}
		runtime.service.contractFromSubmission(ctx, c, runtime.key, previous)
		if previous.Task != nil {
			runtime.service.SetVideoTaskContract(ctx, c, runtime.key, previous.Task.TaskID)
		}
		return nil
	} else if !errors.Is(err, redis.Nil) {
		return err
	}
	expires := time.Now().Add(7 * 24 * time.Hour)
	sub := &videoSubmission{SchemaVersion: 1, State: "not_submitted", RequestID: v.RequestID, APIKeyID: runtime.key.ID, GroupID: derefGroupID(runtime.key.GroupID), BillingRequestID: "client:" + v.RequestID, ExpiresAt: expires.Unix(), Failure: failure}
	data, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	localKey := "video:submission:local:" + DeriveSessionHashFromSeed(index)
	if _, err = store.VideoSave(ctx, localKey, data, expires, true); err != nil {
		return err
	}
	_, err = store.VideoSave(ctx, index, []byte(localKey), expires, true)
	return err
}

func videoFailureFields(e *videoProtocolError) map[string]any {
	if e == nil {
		return nil
	}
	fields := map[string]any{"type": e.code, "code": e.code, "platform_code": e.code, "message": e.message, "source": "platform"}
	switch e.code {
	case "invalid_video_reference", "video_reference_not_found", "video_reference_expired":
		fields["type"] = "invalid_request_error"
	}
	if e.diagnostic != nil {
		for k, v := range e.diagnostic {
			fields[k] = v
		}
		fields["upstream"] = e.diagnostic
		if !e.rejected && e.code == "video_protocol_error" {
			fields["source"] = "platform"
		}
	}
	fields["platform_code"] = e.code
	return fields
}

func videoSubmissionKey(key *APIKey, idem string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", derefGroupID(key.GroupID), key.ID, idem)))
	return "video:submission:" + hex.EncodeToString(digest[:])
}
func videoRequestIndexKey(key *APIKey, id string) string {
	return fmt.Sprintf("video:request:%d:%d:%s", derefGroupID(key.GroupID), key.ID, DeriveSessionHashFromSeed(id))
}

func (s *OpenAIGatewayService) loadVideoSubmission(ctx context.Context, key *APIKey, id string) (*videoSubmission, error) {
	store, err := s.videoStore()
	if err != nil {
		return nil, err
	}
	index, err := store.VideoLoad(ctx, videoRequestIndexKey(key, id))
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(index), "video:submission:") {
		return nil, errors.New("Invalid video request index")
	}
	data, err := store.VideoLoad(ctx, string(index))
	if err != nil {
		return nil, err
	}
	var sub videoSubmission
	if err = json.Unmarshal(data, &sub); err != nil {
		return nil, err
	}
	if sub.SchemaVersion != 1 || sub.RequestID != id || sub.APIKeyID != key.ID || sub.GroupID != derefGroupID(key.GroupID) || sub.ExpiresAt <= time.Now().Unix() {
		return nil, errors.New("Video submission identity mismatch or expired")
	}
	if err := s.closeInterruptedVideoSubmission(ctx, &sub, string(index), data); err != nil {
		return nil, err
	}
	return &sub, nil
}

func (s *OpenAIGatewayService) contractFromSubmission(ctx context.Context, c *gin.Context, key *APIKey, sub *videoSubmission) {
	v := contractFor(c)
	v.RequestID = sub.RequestID
	v.SubmissionRetry = sub.Retry
	v.SubmissionStatus = sub.State
	if v.SubmissionStatus == "preparing" {
		v.SubmissionStatus = "not_submitted"
	}
	if v.SubmissionStatus == "dispatching" {
		v.SubmissionStatus = "submitting"
	}
	if sub.Task != nil {
		s.contractFromTask(ctx, c, key, sub.Task, "cached")
		if sub.Task.LastStatus == "" && sub.State != "accepted" {
			v.SubmissionStatus = sub.State
			v.IsFinal = nil
			v.StatusSource = nil
		}
	} else {
		v.Billing = s.VideoRequestBillingStatus(ctx, key, sub.RequestID)
	}
	if videoSubmissionClosed(sub) {
		v.Billing = s.closedVideoSubmissionBilling(ctx, key, sub)
	}
}

func (s *OpenAIGatewayService) contractFromTask(ctx context.Context, c *gin.Context, key *APIKey, meta *VideoTaskMetadata, source string) {
	v := contractFor(c)
	if meta.SubmissionRetry != nil {
		v.SubmissionRetry = meta.SubmissionRetry
	}
	v.RequestID = meta.RequestID
	if v.RequestID == "" {
		v.RequestID = strings.TrimPrefix(meta.BillingRequestID, "client:")
	}
	if v.RequestID != "" {
		c.Header("X-Video-Request-ID", v.RequestID)
	}
	id := meta.TaskID
	v.TaskID = &id
	v.SubmissionStatus = "accepted"
	final := meta.LastStatus == "completed" || meta.LastStatus == "failed" || meta.LastStatus == "cancelled"
	v.IsFinal = &final
	v.StatusSource = &source
	v.TaskError = meta.TaskError
	if meta.LastStatus != "" {
		status := meta.LastStatus
		v.Status = &status
	}
	if meta.ObservedAt > 0 {
		observed := meta.ObservedAt
		v.StatusObservedAt = &observed
	}
	if meta.LastStatus == "" {
		if meta.SubmissionStatus != "" {
			v.SubmissionStatus = meta.SubmissionStatus
		}
		v.IsFinal = nil
		v.StatusSource = nil
	}
	v.Billing = s.VideoRequestBillingStatus(ctx, key, v.RequestID)
}

// ResolveVideoQuery returns done=true when local evidence is all that is available.
func (s *OpenAIGatewayService) ResolveVideoQuery(ctx context.Context, c *gin.Context, key *APIKey, id string) (taskID string, done bool, err error) {
	types, exists := c.Request.URL.Query()["id_type"]
	kind := "task_id"
	if exists {
		if len(types) != 1 {
			return "", false, videoError(400, "invalid_request_error", "Expected exactly one id_type")
		}
		kind = types[0]
	}
	if kind != "task_id" && kind != "request_id" {
		return "", false, videoError(400, "invalid_request_error", "id_type must be task_id or request_id")
	}
	if kind == "task_id" {
		return id, false, nil
	}
	if videoOperation(c) != "query" || !videoRequestIDPattern.MatchString(id) {
		return "", false, videoError(400, "invalid_request_error", "request_id lookup is only supported for video status queries with a valid request ID")
	}
	contractFor(c).RequestID = id
	sub, err := s.loadVideoSubmission(ctx, key, id)
	if errors.Is(err, redis.Nil) {
		contractFor(c).Billing = s.VideoRequestBillingStatus(ctx, key, id)
		receipt, lookupErr := s.MediaFailure(ctx, key, "video", id)
		if lookupErr != nil {
			return "", false, videoCause(503, "video_submission_unavailable", "Read saved video failure", lookupErr)
		}
		if receipt != nil {
			receipt["error"], receipt["operation_error"] = nil, nil
			WriteMediaJSON(c, 200, receipt)
			return "", true, nil
		}
		return "", false, videoError(404, "video_request_not_found", "Video submission record does not exist or has expired; this does not establish whether an upstream task or charge exists")
	}
	if err != nil {
		return "", false, videoCause(503, "video_submission_unavailable", "Read video submission", err)
	}
	s.contractFromSubmission(ctx, c, key, sub)
	if sub.Task == nil {
		failure := videoFailureFields(sub.Failure)
		if failure != nil {
			failure["operation"] = "create"
		}
		out := normalizedVideoTask{Object: "video", Error: failure}
		writeVideoJSON(c, 200, out)
		return "", true, nil
	}
	store, err := s.videoStore()
	if err != nil {
		return "", false, err
	}
	if err = store.VideoBind(ctx, sub.Task); err != nil {
		return "", false, videoCause(503, "video_task_metadata_unavailable", "Restore video task association", err)
	}
	return sub.Task.TaskID, false, nil
}

func (s *OpenAIGatewayService) ReplayVideoCreate(ctx context.Context, c *gin.Context, key *APIKey, request *OpenAIVideoRequest) (bool, error) {
	idem := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idem == "" {
		idem = contractFor(c).RequestID
	}
	store, err := s.videoStore()
	if err != nil {
		return false, nil
	}
	data, err := store.VideoLoad(ctx, videoSubmissionKey(key, idem))
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, videoCause(503, "video_submission_unavailable", "Read original video submission", err)
	}
	var sub videoSubmission
	if err = json.Unmarshal(data, &sub); err != nil {
		return false, videoCause(503, "video_submission_unavailable", "Decode original video submission", err)
	}
	if sub.SchemaVersion != 1 || sub.ExpiresAt <= time.Now().Unix() {
		return false, videoError(503, "video_submission_unavailable", "Original video submission record is invalid or expired")
	}
	if err = s.closeInterruptedVideoSubmission(ctx, &sub, videoSubmissionKey(key, idem), data); err != nil {
		return false, err
	}
	s.contractFromSubmission(ctx, c, key, &sub)
	if sub.Task != nil {
		s.SetVideoTaskContract(ctx, c, key, sub.Task.TaskID)
	}
	upstreamModel := sub.UpstreamModel
	if upstreamModel == "" && sub.Task != nil {
		upstreamModel = sub.Task.UpstreamModel
	}
	if err = request.ValidateVideoReferences(VideoProtocolLegacy, c.GetHeader("X-Video-Operation")); err != nil {
		return false, err
	}
	params, images, err := s.legacyParams(request, upstreamModel, c.GetHeader("X-Video-Operation"))
	if err != nil {
		return false, err
	}
	if sub.SemanticSHA256 != legacyVideoSemanticHash(request.Model, upstreamModel, params, images) {
		return false, videoError(409, "idempotency_conflict", "Idempotency-Key was used with different parameters")
	}
	if sub.Task != nil {
		if err = store.VideoBind(ctx, sub.Task); err != nil {
			return false, err
		}
		meta, e := s.videoTaskMetadata(ctx, key, sub.Task.TaskID)
		if e != nil {
			return false, e
		}
		if meta.LastStatus == "" {
			if sub.Failure != nil {
				return false, sub.Failure
			}
			return false, videoError(409, "video_protocol_error", "Original video task receipt is invalid; its task ID was retained for diagnosis and no new task will be submitted")
		}
		s.contractFromTask(ctx, c, key, meta, "cached")
		if sub.RequestID != "" {
			c.Header("X-Client-Request-ID", sub.RequestID)
		}
		writeVideoJSON(c, 202, normalizedVideoTask{Object: "video", ID: meta.TaskID, Model: meta.RequestedModel, Status: meta.LastStatus, Error: meta.TaskError})
		return true, nil
	}
	if sub.Failure != nil {
		return false, sub.Failure
	}
	if sub.State == "preparing" || sub.State == "dispatching" {
		return false, videoError(409, "video_submission_in_progress", "Original video submission is still in progress; query its original request ID")
	}
	return false, videoError(409, "submission_unknown", "Original video submission is pending confirmation; automatic replay is disabled because the provider's idempotency retention is not established")
}

func (s *OpenAIGatewayService) SetVideoTaskContract(ctx context.Context, c *gin.Context, key *APIKey, id string) {
	if meta, err := s.videoTaskMetadata(ctx, key, id); err == nil {
		s.contractFromTask(ctx, c, key, meta, "cached")
	}
}
