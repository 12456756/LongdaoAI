//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const sepNativeVideoTask = "video_native_billing"

func sepNativeVideoFixture(t *testing.T, origin string) (*OpenAIGatewayService, *SEPService, *APIKey, *Account, *sepMediaOutcomeDB) {
	t.Helper()
	svc, key, account, _ := newVideoContractFixture(t)
	account.Credentials["video_protocol"] = VideoProtocolOpenAI
	svc.cfg.RunMode = "standard"
	s, repo := sepMediaFixture(t, "video", origin, key, account)
	s.billing = &BillingService{}
	svc.accountRepo = videoTestAccountRepo{account: account}
	s.keys = &APIKeyService{apiKeyRepo: &apiKeyRepoStubForGroupUpdate{key: key}}
	s.groups = &mockGroupRepoForGateway{groups: map[int64]*Group{3: key.Group}}
	s.RefreshVideo = svc.RefreshSEPVideo
	_, err := repo.db.Exec(`UPDATE sep_executions SET task_id=$1,client_request_id='original-video',execution_status='running',dispatched_at=NOW() WHERE id='original-execution'`, sepNativeVideoTask)
	require.NoError(t, err)
	now := time.Now().Unix()
	require.NoError(t, svc.cache.(VideoStateStore).VideoBind(t.Context(), &VideoTaskMetadata{SchemaVersion: 1, TaskID: sepNativeVideoTask, RequestID: "original-video", BillingRequestID: "client:original-video", APIKeyID: key.ID, GroupID: 3, AccountID: account.ID, Protocol: VideoProtocolOpenAI, RequestedModel: "fixture", UpstreamModel: "fixture", BoundAt: now, ExpiresAt: now + 604800}))
	return svc, s, key, account, repo
}

func nativeSEPPoll(t *testing.T, svc *OpenAIGatewayService, key *APIKey, account *Account) map[string]any {
	t.Helper()
	c, w := videoTestContext("GET", "/v1/videos/"+sepNativeVideoTask, nil, "")
	require.NoError(t, InitVideoContract(c))
	svc.SetVideoTaskContract(t.Context(), c, key, sepNativeVideoTask)
	_, err := svc.ForwardVideo(t.Context(), c, account, key, nil, sepNativeVideoTask, false, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func TestSEPNativeVideoFailureFinalizesZeroOnce(t *testing.T) {
	for _, origin := range []string{"sep", "compute"} {
		for _, state := range []string{"failed", "cancelled", "canceled"} {
			t.Run(origin+"/"+state, func(t *testing.T) {
				svc, s, key, account, repo := sepNativeVideoFixture(t, origin)
				// No creation usage record is necessary to close a failed job for free.
				body := sepJSON(map[string]any{"id": sepNativeVideoTask, "status": state, "billing": map[string]any{"status": "settled", "actual_cost_usd": "999"}, "error": map[string]any{"code": "ContentModerationFailed", "message": "Reference copyright moderation failed", "request_id": "upstream-review-id"}})
				svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
					require.Equal(t, http.MethodGet, r.Method)
					return asyncResponse(200, string(body)), nil
				}}
				var wg sync.WaitGroup
				failures := make(chan error, 8)
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); failures <- s.ObserveVideo(t.Context(), key, sepNativeVideoTask, body) }()
				}
				wg.Wait()
				close(failures)
				for err := range failures {
					require.NoError(t, err)
				}
				if state == "canceled" {
					state = "cancelled"
				}
				assertSEPMediaAmounts(t, s, repo.db, origin, state)
				out := nativeSEPPoll(t, svc, key, account)
				require.Equal(t, "original-video", out["request_id"])
				require.Equal(t, "not_charged", out["billing"].(map[string]any)["status"])
				require.Equal(t, "0.000000000000", out["billing"].(map[string]any)["actual_cost_usd"])
				if state == "failed" {
					require.Equal(t, "ContentModerationFailed", out["task_error"].(map[string]any)["code"])
					require.Equal(t, "Reference copyright moderation failed", out["task_error"].(map[string]any)["message"])
				}
				bill, err := s.Billing(t.Context(), "sep-test", "original")
				require.NoError(t, err)
				require.Equal(t, "final", bill["fee_status"])
				require.Equal(t, "0.000000", bill["gross_cny"])
				require.NotEmpty(t, bill["bill_id"])
				require.Equal(t, "", bill["billing_error"])
			})
		}
	}
}

func TestSEPNativeVideoPreservesConfirmedChargedBill(t *testing.T) {
	svc, s, key, account, repo := sepNativeVideoFixture(t, "compute")
	e := *key.SEP
	e.MediaFinal = true
	e.MediaTerminalStatus = "failed"
	_, err := s.FinalizeUsage(t.Context(), &e, &UsageBillingCommand{RequestID: e.ComputeRequestID, APIKeyID: key.ID, UserID: key.UserID, AccountID: account.ID, BalanceCost: 0.12}, nil)
	require.NoError(t, err)
	before, err := s.Billing(t.Context(), "sep-test", "original")
	require.NoError(t, err)
	svc.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		return asyncResponse(200, `{"id":"video_native_billing","status":"failed","error":{"message":"Late upstream task failure"}}`), nil
	}}
	for i := 0; i < 2; i++ {
		out := nativeSEPPoll(t, svc, key, account)
		require.Equal(t, "failed", out["status"])
		require.Equal(t, "settled", out["billing"].(map[string]any)["status"])
		require.Equal(t, "0.120000000000", out["billing"].(map[string]any)["actual_cost_usd"])
	}
	after, err := s.Billing(t.Context(), "sep-test", "original")
	require.NoError(t, err)
	require.Equal(t, before["bill_id"], after["bill_id"])
	require.Equal(t, "0.858000", after["gross_cny"])
	var count int
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM sep_settlements WHERE execution_pk='original-execution'`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestSEPNativeVideoBillingFailureCanRecheckWithoutResubmission(t *testing.T) {
	svc, s, key, account, repo := sepNativeVideoFixture(t, "compute")
	_, err := repo.db.Exec(`ALTER TABLE sep_executions ADD CONSTRAINT test_video_bill_unavailable CHECK(fee_status<>'final') NOT VALID`)
	require.NoError(t, err)
	calls := 0
	svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		calls++
		return asyncResponse(200, `{"id":"video_native_billing","status":"failed","error":{"code":"ContentModerationFailed","message":"Specific rejected reference"}}`), nil
	}}
	out := nativeSEPPoll(t, svc, key, account)
	require.Equal(t, "failed", out["status"])
	require.Equal(t, "ContentModerationFailed", out["task_error"].(map[string]any)["code"])
	require.Equal(t, "unknown", out["billing"].(map[string]any)["status"])
	var state, fee, problem string
	require.NoError(t, repo.db.QueryRow(`SELECT execution_status,fee_status,billing_error FROM sep_executions WHERE id='original-execution'`).Scan(&state, &fee, &problem))
	require.Equal(t, "failed", state)
	require.Equal(t, "reconciling", fee)
	require.Contains(t, problem, "test_video_bill_unavailable")
	_, err = repo.db.Exec(`ALTER TABLE sep_executions DROP CONSTRAINT test_video_bill_unavailable`)
	require.NoError(t, err)
	bill, err := s.Billing(t.Context(), "sep-test", "original")
	require.NoError(t, err)
	require.Equal(t, "final", bill["fee_status"])
	require.Equal(t, "0.000000", bill["gross_cny"])
	require.Equal(t, "", bill["billing_error"])
	require.Equal(t, 2, calls)
	assertSEPMediaAmounts(t, s, repo.db, "compute", "failed")
}

func TestSEPNativeVideoRefreshFailureRetainsTerminalStatusAndReason(t *testing.T) {
	svc, s, _, _, repo := sepNativeVideoFixture(t, "sep")
	_, err := repo.db.Exec(`UPDATE sep_executions SET execution_status='failed',fee_status='reconciling'`)
	require.NoError(t, err)
	svc.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }}
	bill, err := s.Billing(t.Context(), "sep-test", "original")
	require.NoError(t, err)
	require.Equal(t, "failed", bill["execution_status"])
	require.Equal(t, "reconciling", bill["fee_status"])
	require.Nil(t, bill["gross_cny"])
	require.Contains(t, bill["billing_error"], "unexpected EOF")
	require.Contains(t, bill["billing_error"], sepNativeVideoTask)
}

func TestSEPNativeVideoRequiresOriginalKeyForFinalization(t *testing.T) {
	_, s, key, _, repo := sepNativeVideoFixture(t, "compute")
	other := *key
	other.ID = 99
	require.NoError(t, s.ObserveVideo(t.Context(), &other, sepNativeVideoTask, []byte(`{"status":"failed"}`)))
	require.Equal(t, "unknown", s.videoTaskBilling(t.Context(), &other, sepNativeVideoTask).Status)
	var fee string
	require.NoError(t, repo.db.QueryRow(`SELECT fee_status FROM sep_executions WHERE id='original-execution'`).Scan(&fee))
	require.Equal(t, "pending", fee)
}

func TestSEPNativeVideoMissingSuccessEvidenceStaysReconciling(t *testing.T) {
	_, s, key, _, repo := sepNativeVideoFixture(t, "sep")
	err := s.ObserveVideo(t.Context(), key, sepNativeVideoTask, []byte(`{"status":"completed"}`))
	require.ErrorContains(t, err, "original pending usage record is missing")
	var fee, state, problem string
	require.NoError(t, repo.db.QueryRow(`SELECT fee_status,execution_status,billing_error FROM sep_executions WHERE id='original-execution'`).Scan(&fee, &state, &problem))
	require.Equal(t, "reconciling", fee)
	require.Equal(t, "completed", state)
	require.Contains(t, problem, "original pending usage record is missing")
	require.Nil(t, s.videoTaskBilling(t.Context(), key, sepNativeVideoTask).ActualCostUSD)
}

func TestSEPNativeVideoRefreshReasonRedactsCredentialOnly(t *testing.T) {
	_, s, _, _, repo := sepNativeVideoFixture(t, "sep")
	s.RefreshVideo = func(context.Context, *APIKey, string) error {
		return errors.New("upstream query rejected: Authorization: Bearer sk-test-secret-1234567890; request trace trace-42")
	}
	bill, err := s.Billing(t.Context(), "sep-test", "original")
	require.NoError(t, err)
	require.Contains(t, bill["billing_error"], "trace-42")
	require.NotContains(t, bill["billing_error"], "sk-test-secret-1234567890")
	var count int
	require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM sep_settlements`).Scan(&count))
	require.Zero(t, count)
}

func TestSEPNativeVideoSuccessUsesFrozenPriceAndClearsReconciliation(t *testing.T) {
	svc, s, key, account, repo := sepNativeVideoFixture(t, "compute")
	price := &ModelBasePrice{Model: "fixture", Version: 1, Price: AccountModelPrice{Mode: BillingModeVideo, Items: []ModelPriceItem{{Kind: "video", Tier: "720p", Unit: "USD/second", Amount: "0.03"}}}}
	_, err := repo.db.Exec(`UPDATE sep_executions SET price_snapshot=$1`, string(sepJSON(price)))
	require.NoError(t, err)
	seconds, resolution := 4, "720p"
	_, err = s.SavePendingMedia(t.Context(), key.SEP, &UsageBillingCommand{RequestID: "original-execution", APIKeyID: key.ID, UserID: key.UserID, AccountID: account.ID, BalanceCost: 123}, &UsageLog{ActualCost: 123, VideoCount: 1, VideoDurationSeconds: &seconds, VideoResolution: &resolution})
	require.NoError(t, err)
	require.NoError(t, s.rememberVideoBillingProblem(t.Context(), "original-execution", "", errors.New("earlier fee verification failure")))
	svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, r.Method)
		return asyncResponse(200, `{"id":"video_native_billing","status":"completed","seconds":4,"resolution":"720p","billing":{"status":"settled","actual_cost_usd":"999"}}`), nil
	}}
	key.Group.RateMultiplier = 99
	bill, err := s.Billing(t.Context(), "sep-test", "original")
	require.NoError(t, err)
	require.Equal(t, "final", bill["fee_status"])
	require.Equal(t, "0.858000", bill["gross_cny"])
	require.Equal(t, "", bill["billing_error"])
	out := nativeSEPPoll(t, svc, key, account)
	require.Equal(t, "settled", out["billing"].(map[string]any)["status"])
	require.Equal(t, "0.120000000000", out["billing"].(map[string]any)["actual_cost_usd"])
	assertSEPMediaAmounts(t, s, repo.db, "compute", "completed")
}

func TestSEPNativeVideoMismatchedTaskCannotFinalizeFee(t *testing.T) {
	svc, s, key, account, repo := sepNativeVideoFixture(t, "compute")
	svc.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		return asyncResponse(200, `{"id":"different-task","status":"failed"}`), nil
	}}
	c, _ := videoTestContext("GET", "/v1/videos/"+sepNativeVideoTask, nil, "")
	_, err := svc.ForwardVideo(t.Context(), c, account, key, nil, sepNativeVideoTask, false, "")
	require.ErrorContains(t, err, "task ID does not match")
	var fee string
	require.NoError(t, repo.db.QueryRow(`SELECT fee_status FROM sep_executions WHERE id='original-execution'`).Scan(&fee))
	require.Equal(t, "pending", fee)
	require.Nil(t, s.videoTaskBilling(t.Context(), key, sepNativeVideoTask).ActualCostUSD)
}
