//go:build unit

package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSEPLegacyVideoRetryKeepsUpstreamIdempotencyAndSingleClaim(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "recovered"}[recover], func(t *testing.T) {
			s, _, account, _ := newVideoContractFixture(t)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			e := &SEPExecution{ComputeRequestID: "compute-video", Service: &SEPService{db: db}}
			mock.ExpectQuery("UPDATE sep_executions SET dispatched_at").WithArgs(account.ID, e.ComputeRequestID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(e.ComputeRequestID))
			calls := 0
			s.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
				require.NoError(t, ClaimSEPUpstreamDispatch(r, account.ID))
				require.Equal(t, "same-video-key", r.Header.Get("Idempotency-Key"))
				body, readErr := io.ReadAll(r.Body)
				require.NoError(t, readErr)
				require.JSONEq(t, `{"model_id":"seedance-2.0"}`, string(body))
				calls++
				if recover && calls == 3 {
					return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
				}
				return nil, io.ErrUnexpectedEOF
			}}
			s.videoSubmissionRetryWait = func(context.Context, time.Duration) error { return nil }
			sub := &videoSubmission{RequestID: "video-request", Retry: &VideoSubmissionRetry{MaxAttempts: 4}}
			task, err := s.createLegacyVideoWithRetry(WithSEPExecution(t.Context(), e), account, []byte(`{"model_id":"seedance-2.0"}`), "same-video-key", sub, func() error { return nil })
			if recover {
				require.NoError(t, err)
				require.Equal(t, legacyTestTaskID, task.ID)
				require.Equal(t, 3, calls)
			} else {
				require.ErrorContains(t, err, "unexpected EOF")
				require.Equal(t, 4, calls)
				require.True(t, sub.Retry.Exhausted)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSEPReplayCannotDispatchEvenWhenReceiptExpired(t *testing.T) {
	e := &SEPExecution{ComputeRequestID: "original", ReplayOnly: true, Service: &SEPService{}}
	r := httptest.NewRequest("POST", "https://upstream.test/v1/video/tasks", nil)
	r = r.WithContext(withSEPVideoRetry(WithSEPExecution(r.Context(), e)))
	r.Header.Set("Idempotency-Key", "same-key")
	require.ErrorContains(t, ClaimSEPUpstreamDispatch(r, 5), "EXECUTION_ALREADY_RECEIVED")
}

func TestSEPMediaReplayUsesOriginalSnapshotBeforeNewRequestGuards(t *testing.T) {
	_, key, account, _ := newVideoContractFixture(t)
	s, db := sepMediaFixture(t, "video", "compute", key, account)
	s.settings.settingRepo.(*sepBalanceSettings).cfg.DirectBillingEnabled = true
	_, err := db.db.Exec(`INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,encryption_key_id,masked_key,encrypted_key) VALUES('video-credential',1,'video',1,'fixture','fixture','fixture');
UPDATE sep_executions SET client_request_id='client-original',idempotency_key='original',request_path='/v1/videos';
UPDATE sep_enterprises SET status='disabled',available_cny=0;`)
	require.NoError(t, err)
	r := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(`{"prompt":"same","model":"fixture"}`))
	r.Header.Set("Authorization", "Bearer "+key.Key)
	r.Header.Set("X-Compute-Billing-Mode", "direct")
	r.Header.Set("Idempotency-Key", "original")
	r.Header.Set("X-Client-Request-ID", "client-original")
	r.Header.Set("X-SEP-Quote-ID", "expired-quote")
	// Neither current pricing nor current funds are required to recover a receipt.
	s.billing = nil
	restored, err := s.AuthorizeGateway(t.Context(), r, key)
	require.NoError(t, err)
	require.True(t, restored.SEP.ReplayOnly)
	require.Equal(t, "original-execution", restored.SEP.ComputeRequestID)
	require.Equal(t, "fixture", restored.SEP.Price.Model)
	r.Header.Set("X-Client-Request-ID", "other-request")
	restored, err = s.AuthorizeGateway(t.Context(), r, key)
	require.NoError(t, err)
	require.True(t, restored.SEP.ReplayOnly)
	require.Equal(t, "client-original", r.Header.Get("X-Client-Request-ID"))
	r.Header.Set("Idempotency-Key", "different-idempotency")
	_, err = s.replayMediaExecution(t.Context(), key, r, "original", "different-body-hash")
	require.ErrorContains(t, err, "IDEMPOTENCY_CONFLICT")
}

func TestSEPRejectedReceiptClosesZeroFeeWithoutOverwritingAcceptedTasks(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "local-rejection", true: "already-dispatched"}[dispatched], func(t *testing.T) {
			_, key, account, _ := newVideoContractFixture(t)
			s, repo := sepMediaFixture(t, "video", "compute", key, account)
			if dispatched {
				_, err := repo.db.Exec(`UPDATE sep_executions SET dispatched_at=NOW(),task_id='accepted-task',execution_status='running'`)
				require.NoError(t, err)
			}
			receipt := map[string]any{"operation": "create", "submission_status": "not_submitted", "task_id": nil, "billing": map[string]any{"status": "not_charged"}}
			for i := 0; i < 2; i++ {
				require.NoError(t, s.FinalizeRejectedMediaReceipt(t.Context(), key, receipt))
				s.FinishGateway(t.Context(), key, 503)
			}
			var state, fee string
			require.NoError(t, repo.db.QueryRow(`SELECT execution_status,fee_status FROM sep_executions`).Scan(&state, &fee))
			if dispatched {
				require.Equal(t, "running", state)
				require.Equal(t, "pending", fee)
			} else {
				require.Equal(t, "failed", state)
				require.Equal(t, "final", fee)
				var gross, balance string
				require.NoError(t, repo.db.QueryRow(`SELECT gross_cny::text FROM sep_executions`).Scan(&gross))
				require.Equal(t, "0.000000", gross)
				require.NoError(t, repo.db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
				require.Equal(t, "10.000000", balance)
			}
		})
	}
}

func TestSEPVideoRequestRecoveryRestoresGroupAndKeepsKeyOwnership(t *testing.T) {
	svc, key, account, _ := newVideoContractFixture(t)
	sep, repo := sepMediaFixture(t, "video", "sep", key, account)
	svc.usageBillingRepo = repo
	originalGroup := key.Group
	sep.groups = &groupRepoStubForAdmin{getByID: originalGroup}
	_, err := repo.db.Exec(`UPDATE sep_executions SET client_request_id='original-request'`)
	require.NoError(t, err)
	// The provider returned an explicit failure, so there is no provider task ID.
	svc.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		return asyncResponse(403, `{"message":"reference permission denied","code":"ReferenceNotAllowed"}`), nil
	}}
	_, out, err := createVideoContract(t, svc, key, account, "original-request")
	require.Error(t, err)
	require.Equal(t, "rejected", out["submission_status"])
	key.GroupID = videoBillingPtr(int64(99))
	key.Group = &Group{ID: 99, Platform: PlatformOpenAI}
	restored, err := sep.RestoreVideoRequestGroup(t.Context(), key, "original-request")
	require.NoError(t, err)
	require.EqualValues(t, 3, *restored.GroupID)
	c, w := videoTestContext("GET", "/v1/videos/original-request?id_type=request_id", nil, "")
	_, done, err := svc.ResolveVideoQuery(t.Context(), c, restored, "original-request")
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, w.Body.String(), "ReferenceNotAllowed")
	require.Contains(t, w.Body.String(), `"submission_status":"rejected"`)
	require.Contains(t, w.Body.String(), `"status":"not_charged"`)
	wrongKey := *restored
	wrongKey.ID++
	c, _ = videoTestContext("GET", "/v1/videos/original-request?id_type=request_id", nil, "")
	_, _, err = svc.ResolveVideoQuery(t.Context(), c, &wrongKey, "original-request")
	require.ErrorContains(t, err, "does not exist")
}

func TestSEPAsyncImageEditQuoteMatchesOriginalOperation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	s := &SEPService{db: db}
	key := &APIKey{ID: 1, SEP: &SEPExecution{EnterprisePK: 1, IntegrationID: "test", Kind: "image", Multipliers: SEPMultipliers{Image: 1}}}
	r := httptest.NewRequest("POST", "/v1/images/edits/async", nil)
	r.Header.Set("X-SEP-Quote-ID", "quote-original")
	body := []byte(`{"model":"gpt-image-2","size":"1024x1024","quality":"high","n":1}`)
	price := &ModelBasePrice{Model: "gpt-image-2", Price: AccountModelPrice{Mode: BillingModeImage}}
	mock.ExpectQuery("SELECT kind,exchange_rate").WithArgs("quote-original", "test", int64(1), int64(1)).WillReturnRows(sqlmock.NewRows([]string{"kind", "rate", "native", "spec", "price", "multiplier"}).AddRow("image", "7.15", "native-version", []byte(`{"model":"gpt-image-2","operation":"edit","size":"1024x1024","quality":"high","n":1}`), sepJSON(price), 1))
	require.NoError(t, s.VerifyMediaQuote(t.Context(), r, body, key))
	require.Equal(t, "gpt-image-2", key.SEP.Price.Model)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSEPImageProviderRejectionPreservesZeroFee(t *testing.T) {
	_, key, account, _ := newVideoContractFixture(t)
	s, repo := sepMediaFixture(t, "image", "compute", key, account)
	_, err := repo.db.Exec(`UPDATE sep_executions SET dispatched_at=NOW(),execution_status='running'`)
	require.NoError(t, err)
	receipt := map[string]any{"operation": "create", "submission_status": "failed", "task_id": nil, "billing": map[string]any{"status": "not_charged"}}
	for i := 0; i < 2; i++ {
		require.NoError(t, s.FinalizeRejectedMediaReceipt(t.Context(), key, receipt))
	}
	var state, fee, gross string
	require.NoError(t, repo.db.QueryRow(`SELECT execution_status,fee_status,gross_cny::text FROM sep_executions`).Scan(&state, &fee, &gross))
	require.Equal(t, "failed", state)
	require.Equal(t, "final", fee)
	require.Equal(t, "0.000000", gross)
	key.SEP.ReplayOnly = true
	s.FinishGateway(t.Context(), key, 502)
	require.NoError(t, s.FinalizeRejectedMediaReceipt(t.Context(), key, receipt))
}

func TestSEPQuoteRejectionHasRecoverableFinalZeroFeeExecution(t *testing.T) {
	for _, origin := range []string{"sep", "compute"} {
		for _, failure := range []string{"operation", "size", "expired", "valid"} {
			t.Run(origin+"/"+failure, func(t *testing.T) {
				_, key, account, _ := newVideoContractFixture(t)
				s, repo := sepMediaFixture(t, "image", origin, key, account)
				settings := s.settings.settingRepo.(*sepBalanceSettings)
				settings.cfg.DirectBillingEnabled = true
				s.keys = &APIKeyService{}
				billing, _, price := testManagedPrice(t, BillingModeImage, ModelPriceItem{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "0.12"})
				s.billing = billing
				_, err := repo.db.Exec(`INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,encryption_key_id,masked_key,encrypted_key) VALUES('image-credential',1,'image',1,'fixture','fixture','fixture')`)
				require.NoError(t, err)
				spec := map[string]any{"model": price.Model, "operation": "edit", "size": "1024x1024", "quality": "high", "n": 1}
				if failure == "operation" {
					spec["operation"] = "generate"
				}
				if failure == "size" {
					spec["size"] = "2048x2048"
				}
				expires := time.Now().Add(time.Minute)
				if failure == "expired" {
					expires = time.Now().Add(-time.Minute)
				}
				_, err = repo.db.Exec(`INSERT INTO sep_quotes(id,integration_id,enterprise_pk,api_key_id,kind,specification,native_version,exchange_rate,amount_cny,price_snapshot,expires_at,quoted_multiplier) VALUES('quote-test','sep-test',1,1,'image',$1,'native',7.15,1,$2,$3,1)`, string(sepJSON(spec)), string(sepJSON(price)), expires)
				require.NoError(t, err)
				pub, private, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				der, err := x509.MarshalPKIXPublicKey(pub)
				require.NoError(t, err)
				settings.cfg.SigningKeyID = "test-signing"
				settings.cfg.SigningPublicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
				body := sepJSON(map[string]any{"model": price.Model, "prompt": "test", "size": "1024x1024", "quality": "high", "n": 1})
				request := func() *http.Request {
					r := httptest.NewRequest("POST", "/v1/images/edits/async", strings.NewReader(string(body)))
					r.Header.Set("Authorization", "Bearer "+key.Key)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("X-Client-Request-ID", "sep-original")
					r.Header.Set("Idempotency-Key", "quote-rejected")
					r.Header.Set("X-SEP-Quote-ID", "quote-test")
					if origin == "compute" {
						r.Header.Set("X-Compute-Billing-Mode", "direct")
					} else {
						r.Header.Set("X-SEP-Version", "1")
						r.Header.Set("X-SEP-Key-ID", "test-signing")
						r.Header.Set("X-SEP-Timestamp", fmt.Sprint(time.Now().Unix()))
						r.Header.Set("X-SEP-Nonce", sepRandom("nonce_"))
						r.Header.Set("X-SEP-Enterprise-ID", "ent-test")
						r.Header.Set("X-SEP-Execution-ID", "quote-rejected")
						r.Header.Set("X-SEP-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(SEPCanonicalRequest(r, body)))))
					}
					return r
				}
				received, err := s.AuthorizeGateway(t.Context(), request(), key)
				if failure == "valid" {
					require.NoError(t, err)
					require.False(t, received.SEP.ReplayOnly)
					bill, billErr := s.Billing(t.Context(), "sep-test", "quote-rejected")
					require.NoError(t, billErr)
					require.Equal(t, "received", bill["execution_status"])
					require.Equal(t, "pending", bill["fee_status"])
					return
				}
				var rejected *SEPRejectedMediaError
				require.ErrorAs(t, err, &rejected)
				require.Nil(t, rejected.BillingError)
				require.NotEmpty(t, rejected.Key.SEP.ComputeRequestID)
				code := "QUOTE_MISMATCH"
				if failure == "expired" {
					code = "QUOTE_EXPIRED_OR_MISSING"
				}
				require.ErrorContains(t, err, code)
				for i := 0; i < 2; i++ {
					bill, billErr := s.Billing(t.Context(), "sep-test", "quote-rejected")
					require.NoError(t, billErr)
					require.Equal(t, "failed", bill["execution_status"])
					require.Equal(t, "final", bill["fee_status"])
					require.Equal(t, "0.000000", bill["gross_cny"])
					require.Equal(t, "", bill["task_id"])
				}
				replay, replayErr := s.AuthorizeGateway(t.Context(), request(), key)
				require.NoError(t, replayErr)
				require.True(t, replay.SEP.ReplayOnly)
				require.Equal(t, rejected.Key.SEP.ComputeRequestID, replay.SEP.ComputeRequestID)
				var count int
				require.NoError(t, repo.db.QueryRow(`SELECT count(*) FROM sep_executions WHERE execution_id='quote-rejected'`).Scan(&count))
				require.Equal(t, 1, count)
				var balance string
				require.NoError(t, repo.db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
				require.Equal(t, "10.000000", balance)
				wrong := *key
				wrong.ID++
				_, err = restoreSEPMediaBillingKey(t.Context(), &wrong, rejected.Key.SEP.ComputeRequestID, "sep-original")
				require.ErrorContains(t, err, "SEP_RESOURCE_NOT_FOUND")
				_, err = s.Billing(t.Context(), "other-integration", "quote-rejected")
				require.ErrorContains(t, err, "SEP_RESOURCE_NOT_FOUND")
			})
		}
	}
}
