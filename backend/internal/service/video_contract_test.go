package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type videoOutcomeMemory struct {
	UsageBillingRepository
	mu        sync.Mutex
	rows      map[string]*VideoBillingOutcome
	commands  []*UsageBillingCommand
	failApply bool
	failClose bool
}

func (r *videoOutcomeMemory) RegisterVideoBilling(_ context.Context, b *VideoBillingOutcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows[b.RequestID] != nil {
		return ErrUsageBillingRequestConflict
	}
	clone := *b
	r.rows[b.RequestID] = &clone
	return nil
}
func (r *videoOutcomeMemory) FindVideoBillingOutcome(_ context.Context, key, user int64, id string) (*VideoBillingOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.rows[id]
	if b == nil || b.APIKeyID != key || b.UserID != user {
		return nil, nil
	}
	clone := *b
	return &clone, nil
}
func (r *videoOutcomeMemory) CloseVideoBillingWithoutCharge(_ context.Context, key, user int64, id string) (*VideoBillingOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failClose {
		return nil, errors.New("billing close transaction unavailable")
	}
	b := r.rows[id]
	if b == nil || b.APIKeyID != key || b.UserID != user {
		return nil, errors.New("billing identity not found")
	}
	if b.Status == "pending" {
		zero := "0"
		b.Status = "not_charged"
		b.ActualCostUSD = &zero
	}
	clone := *b
	return &clone, nil
}
func (r *videoOutcomeMemory) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failApply {
		return nil, errors.New("settlement transaction unavailable")
	}
	b := r.rows[cmd.RequestID]
	if b == nil || !cmd.DeferredVideo {
		return nil, errors.New("invalid deferred billing identity")
	}
	if b.Status == "settled" {
		return &UsageBillingApplyResult{}, nil
	}
	if b.Status != "pending" {
		return nil, errors.New("request finalized without charge")
	}
	amount := BillingUSDString(cmd.BalanceCost + cmd.SubscriptionCost)
	b.Status = "settled"
	b.ActualCostUSD = &amount
	clone := *cmd
	r.commands = append(r.commands, &clone)
	return &UsageBillingApplyResult{Applied: true}, nil
}

func newVideoContractFixture(t *testing.T) (*OpenAIGatewayService, *APIKey, *Account, *videoOutcomeMemory) {
	t.Helper()
	cfg := legacyTestConfig(t)
	key := &APIKey{ID: 8, UserID: 11, User: &User{ID: 11}, GroupID: videoBillingPtr(int64(3)), Group: &Group{ID: 3, Platform: PlatformOpenAI, RateMultiplier: 1}}
	ledger := &videoOutcomeMemory{rows: map[string]*VideoBillingOutcome{}}
	svc := &OpenAIGatewayService{cfg: cfg, cache: newVideoMemoryStore(), usageBillingRepo: ledger, usageLogRepo: &openAIRecordUsageLogRepoStub{inserted: true}, billingCacheService: &BillingCacheService{}, deferredService: &DeferredService{}}
	svc.videoSubmissionRetryWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(svc.CloseOpenAIWSPool)
	return svc, key, legacyTestAccount(), ledger
}

func createVideoContract(t *testing.T, s *OpenAIGatewayService, key *APIKey, a *Account, id string) (int, map[string]any, error) {
	t.Helper()
	body := []byte(`{"model":"seedance-2.0","prompt":"waves","seconds":4,"resolution":"720p"}`)
	request, err := ParseOpenAIVideoRequest(body, "application/json")
	require.NoError(t, err)
	c, w := videoTestContext("POST", "/v1/videos", body, "application/json")
	c.Request.Header.Set("X-Client-Request-ID", id)
	c.Request.Header.Set("Idempotency-Key", id)
	c.Request.Header.Set("X-Video-Operation", "text-to-video")
	require.NoError(t, InitVideoContract(c))
	frozen := &FrozenVideoBilling{keyID: key.ID, userID: key.UserID, groupID: *key.GroupID, model: request.Model, multiplier: 1, cost: CostBreakdown{TotalCost: 0.12, ActualCost: 0.12, BillingMode: "video"}, mapping: ChannelMappingResult{MappedModel: request.Model, BillingModelSource: BillingModelSourceRequested}}
	ctx := WithVideoBillingInput(c.Request.Context(), &OpenAIRecordUsageInput{FrozenVideoBilling: frozen, RequestPayloadHash: request.PayloadHash(), InboundEndpoint: "/v1/videos"})
	c.Request = c.Request.WithContext(ctx)
	_, err = s.ForwardVideo(ctx, c, a, key, request, "", false, "")
	var output map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &output))
	return w.Code, output, err
}

func TestVideoContractExplicitRejectionsAreNotUnknown(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 413, 429, 500, 503, 202} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, key, a, ledger := newVideoContractFixture(t)
			calls := 0
			s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{"X-Request-Id": {"provider-original"}, "Retry-After": {"12"}}, Body: io.NopCloser(strings.NewReader(`{"code":1234,"reason":"model_capacity","message":"No capacity for this resolution; Bearer test-secret"}`))}, nil
			}}
			_, out, err := createVideoContract(t, s, key, a, "request-rejected")
			require.Error(t, err)
			state := "rejected"
			if status >= 500 {
				state = "failed"
			}
			require.Equal(t, state, out["submission_status"])
			require.Equal(t, "not_charged", out["billing"].(map[string]any)["status"])
			require.Contains(t, out["error"].(map[string]any)["message"], "No capacity for this resolution")
			require.Equal(t, "model_capacity", out["error"].(map[string]any)["reason"])
			encoded, _ := json.Marshal(out)
			require.NotContains(t, string(encoded), "test-secret")
			c, w := videoTestContext("GET", "/v1/videos/request-rejected?id_type=request_id", nil, "")
			_, done, e := s.ResolveVideoQuery(context.Background(), c, key, "request-rejected")
			require.NoError(t, e)
			require.True(t, done)
			require.Contains(t, w.Body.String(), "model_capacity")
			attempts := 1
			if status >= 500 {
				attempts = videoSubmissionMaxAttempts
			}
			require.Equal(t, attempts, calls)
			require.Empty(t, ledger.commands)
			require.Equal(t, "not_charged", ledger.rows["client:request-rejected"].Status)
		})
	}
}

func TestVideoContractMissingReceiptAndLookupIsolation(t *testing.T) {
	s, key, a, ledger := newVideoContractFixture(t)
	calls := 0
	s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("connection reset before receiving response")
	}}
	_, out, err := createVideoContract(t, s, key, a, "request-unknown")
	require.Error(t, err)
	require.Equal(t, "failed", out["submission_status"])
	require.Nil(t, out["task_id"])
	require.Equal(t, "not_charged", out["billing"].(map[string]any)["status"])
	c, w := videoTestContext("GET", "/v1/videos/request-unknown?id_type=request_id", nil, "")
	_, done, err := s.ResolveVideoQuery(context.Background(), c, key, "request-unknown")
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, w.Body.String(), "connection reset")
	other := *key
	other.ID++
	c, _ = videoTestContext("GET", "/v1/videos/request-unknown?id_type=request_id", nil, "")
	_, _, err = s.ResolveVideoQuery(context.Background(), c, &other, "request-unknown")
	require.Error(t, err)
	body := []byte(`{"model":"seedance-2.0","prompt":"waves","seconds":4,"resolution":"720p"}`)
	request, err := ParseOpenAIVideoRequest(body, "application/json")
	require.NoError(t, err)
	c, _ = videoTestContext("POST", "/v1/videos", body, "application/json")
	c.Request.Header.Set("Idempotency-Key", "request-unknown")
	c.Request.Header.Set("X-Video-Operation", "text-to-video")
	_, err = s.ReplayVideoCreate(context.Background(), c, key, request)
	require.Error(t, err)
	require.Equal(t, videoSubmissionMaxAttempts, calls)
	require.Empty(t, ledger.commands)
}

func TestVideoContractSuccessBillsOriginalRequestOnce(t *testing.T) {
	s, key, a, ledger := newVideoContractFixture(t)
	creates, queries := 0, 0
	s.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			creates++
			return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
		}
		queries++
		return legacyTestResponse(200, "seedance-2.0", "succeeded", "https://cdn.example/video.mp4"), nil
	}}
	_, out, err := createVideoContract(t, s, key, a, "original-request")
	require.NoError(t, err)
	require.Equal(t, "pending", out["billing"].(map[string]any)["status"])
	require.Empty(t, ledger.commands)
	key.Group.RateMultiplier = 99
	for i := 0; i < 2; i++ {
		c, w := videoTestContext("GET", "/v1/videos/original-request?id_type=request_id", nil, "")
		id, done, err := s.ResolveVideoQuery(context.Background(), c, key, "original-request")
		require.NoError(t, err)
		require.False(t, done)
		_, err = s.ForwardVideo(context.Background(), c, a, key, nil, id, false, "")
		require.NoError(t, err)
		require.Contains(t, w.Body.String(), `"status":"settled"`)
		require.Contains(t, w.Body.String(), `"request_id":"original-request"`)
	}
	require.Equal(t, 1, creates)
	require.Equal(t, 2, queries)
	require.Len(t, ledger.commands, 1)
	require.Equal(t, "client:original-request", ledger.commands[0].RequestID)
	require.InDelta(t, 0.12, ledger.commands[0].BalanceCost, 1e-12)
	replayBody := []byte(`{"model":"seedance-2.0","prompt":"waves","seconds":4,"resolution":"720p"}`)
	replay, err := ParseOpenAIVideoRequest(replayBody, "application/json")
	require.NoError(t, err)
	c, w := videoTestContext("POST", "/v1/videos", replayBody, "application/json")
	c.Request.Header.Set("Idempotency-Key", "original-request")
	c.Request.Header.Set("X-Video-Operation", "text-to-video")
	done, err := s.ReplayVideoCreate(context.Background(), c, key, replay)
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, w.Body.String(), `"status":"completed"`)
	require.Equal(t, 1, creates)
	conflicting, err := ParseOpenAIVideoRequest([]byte(`{"model":"seedance-2.0","prompt":"different","seconds":4,"resolution":"720p"}`), "application/json")
	require.NoError(t, err)
	c, _ = videoTestContext("POST", "/v1/videos", nil, "")
	c.Request.Header.Set("Idempotency-Key", "original-request")
	c.Request.Header.Set("X-Video-Operation", "text-to-video")
	_, err = s.ReplayVideoCreate(context.Background(), c, key, conflicting)
	require.Error(t, err)
	require.Equal(t, "settled", contractFor(c).Billing.Status, "a parameter conflict must not claim the original request was not charged")
	store := s.cache.(*videoMemoryStore)
	store.mu.Lock()
	delete(store.records, videoRequestIndexKey(key, "original-request"))
	store.mu.Unlock()
	c, _ = videoTestContext("GET", "/v1/videos/original-request?id_type=request_id", nil, "")
	_, _, err = s.ResolveVideoQuery(context.Background(), c, key, "original-request")
	require.Error(t, err)
	require.Equal(t, "settled", contractFor(c).Billing.Status, "losing Redis association must not erase durable billing evidence")
}

func TestVideoContractQueryAndCancelErrorsDoNotChangeTask(t *testing.T) {
	for _, scenario := range []string{"query unavailable", "cancel conflict", "cancel success", "settlement failure", "task failed"} {
		t.Run(scenario, func(t *testing.T) {
			s, key, a, ledger := newVideoContractFixture(t)
			calls := 0
			s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
				return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
			}}
			_, _, err := createVideoContract(t, s, key, a, "task-operation")
			require.NoError(t, err)
			s.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				switch scenario {
				case "query unavailable", "cancel conflict":
					status := 503
					if scenario == "cancel conflict" {
						status = 409
						require.Equal(t, "PUT", r.Method)
						require.True(t, strings.HasSuffix(r.URL.Path, "/cancel"))
						content, _ := io.ReadAll(r.Body)
						require.Empty(t, content)
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":"Task cannot be canceled in its current state","reason":"not_cancelable"}`))}, nil
				case "cancel success":
					return legacyTestResponse(200, "seedance-2.0", "canceled", ""), nil
				case "task failed":
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0,"message":"success","data":{"id":"` + legacyTestTaskID + `","model_id":"seedance-2.0","status":"failed","is_final":true,"created_at":12345,"error":{"code":1203,"message":"Reference image rejected","details":{"limit":9}}}}`))}, nil
				default:
					return legacyTestResponse(200, "seedance-2.0", "succeeded", "https://cdn.example/output.mp4"), nil
				}
			}}
			method, path := "GET", "/v1/videos/"+legacyTestTaskID
			if strings.HasPrefix(scenario, "cancel") {
				method = "PUT"
				path += "/cancel"
			}
			ledger.failApply = scenario == "settlement failure"
			c, w := videoTestContext(method, path, nil, "")
			s.SetVideoTaskContract(context.Background(), c, key, legacyTestTaskID)
			_, err = s.ForwardVideo(context.Background(), c, a, key, nil, legacyTestTaskID, false, "")
			require.Equal(t, 1, calls)
			switch scenario {
			case "query unavailable", "cancel conflict":
				require.Error(t, err)
				require.Equal(t, "pending", ledger.rows["client:task-operation"].Status)
				meta, e := s.videoTaskMetadata(context.Background(), key, legacyTestTaskID)
				require.NoError(t, e)
				require.Equal(t, "queued", meta.LastStatus)
			case "cancel success", "task failed":
				require.NoError(t, err)
				require.Equal(t, "not_charged", ledger.rows["client:task-operation"].Status)
				if scenario == "task failed" {
					require.Contains(t, w.Body.String(), "Reference image rejected")
					require.Contains(t, w.Body.String(), `"task_error"`)
				}
			case "settlement failure":
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), `"status":"completed"`)
				require.Contains(t, w.Body.String(), "settlement transaction unavailable")
				require.Equal(t, "pending", ledger.rows["client:task-operation"].Status)
			}
			require.Empty(t, ledger.commands)
		})
	}
}

func TestVideoContractConcurrentDuplicateCannotDispatchTwice(t *testing.T) {
	s, key, a, _ := newVideoContractFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}
	done := make(chan struct{})
	go func() { defer close(done); _, _, _ = createVideoContract(t, s, key, a, "concurrent-request") }()
	<-entered
	code, out, err := createVideoContract(t, s, key, a, "concurrent-request")
	require.Error(t, err)
	require.Equal(t, 409, code)
	require.Equal(t, "submitting", out["submission_status"])
	require.Equal(t, "video_submission_in_progress", out["error"].(map[string]any)["code"])
	close(release)
	<-done
}

func TestVideoContractPreservesProviderMetadataAndMalformedReceipt(t *testing.T) {
	for _, test := range []struct {
		name, body, state, message string
		status                     int
	}{
		{"metadata", `{"ResponseMetadata":{"RequestId":"metadata-request","Error":{"Code":"InternalError","Message":"Database temporarily unavailable; Bearer fixture-secret"}}}`, "failed", "Database temporarily unavailable", 500},
		{"plain text", "Video worker exhausted; Bearer fixture-secret", "failed", "Video worker exhausted", 503},
		{"malformed accepted", "not-json; Bearer fixture-secret", "failed", "Decode upstream video JSON", 202},
		{"known ID invalid status", `{"code":0,"message":"accepted","data":{"id":"` + legacyTestTaskID + `","model_id":"seedance-2.0","status":"mystery","is_final":false,"created_at":12345}}`, "protocol_error", "Invalid upstream video status", 202},
		{"known ID invalid model type", `{"code":0,"message":"accepted","data":{"id":"` + legacyTestTaskID + `","model_id":123,"status":"queued","is_final":false,"created_at":12345}}`, "protocol_error", "Decode upstream video JSON", 202},
		{"business HTTP200", `{"code":1234,"message":"Model unavailable"}`, "rejected", "Model unavailable", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, key, a, _ := newVideoContractFixture(t)
			calls := 0
			s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: test.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			}}
			_, out, err := createVideoContract(t, s, key, a, "diagnostic-request")
			require.Error(t, err)
			require.Equal(t, test.state, out["submission_status"])
			require.Contains(t, out["error"].(map[string]any)["message"], test.message)
			encoded, _ := json.Marshal(out)
			require.NotContains(t, string(encoded), "fixture-secret")
			if test.name == "metadata" {
				require.Equal(t, "metadata-request", out["error"].(map[string]any)["upstream_request_id"])
			}
			if strings.HasPrefix(test.name, "known ID") {
				require.Equal(t, "not_charged", out["billing"].(map[string]any)["status"])
				require.Equal(t, legacyTestTaskID, out["task_id"])
				require.Equal(t, 1, calls, "A received task ID must stop creation retries even when other receipt fields are invalid")
			}
		})
	}
}

func TestVideoContractLocalRejectionIsQueryableAndNotCharged(t *testing.T) {
	s, key, _, ledger := newVideoContractFixture(t)
	c, w := videoTestContext("POST", "/v1/videos", nil, "")
	c.Request.Header.Set("X-Client-Request-ID", "local-rejection")
	require.NoError(t, InitVideoContract(c))
	s.BindVideoContract(c, key)
	WriteVideoError(c, 400, "invalid_request_error", "model and prompt are required")
	require.Equal(t, 400, w.Code)
	outcome, err := s.VideoRequestOutcome(context.Background(), key, "local-rejection")
	require.NoError(t, err)
	require.Nil(t, outcome, "No financial row was created for a pre-dispatch parameter rejection")
	c, w = videoTestContext("GET", "/v1/videos/local-rejection?id_type=request_id", nil, "")
	_, done, err := s.ResolveVideoQuery(context.Background(), c, key, "local-rejection")
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, w.Body.String(), "model and prompt are required")
	require.Contains(t, w.Body.String(), `"submission_status":"not_submitted"`)
	require.Contains(t, w.Body.String(), `"status":"not_charged"`)
	require.Empty(t, ledger.commands)
}
