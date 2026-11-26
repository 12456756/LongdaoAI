//go:build unit

package service

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestManagedAsyncImageUsesConfiguredPrice(t *testing.T) {
	billing, repo, price := testManagedPrice(t, BillingModeImage, ModelPriceItem{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "0.12"})
	svc, key := newImageBillingServiceForTest(nil)
	svc.billingService = billing
	key.User = &User{ID: key.UserID}
	key.SEP = &SEPExecution{ComputeRequestID: "original", Multipliers: SEPMultipliers{Image: 2}}
	snapshot, err := svc.PrepareAsyncImageBilling(t.Context(), key, &OpenAIImagesRequest{Model: price.Model, Size: "1024x1024", N: 1, Endpoint: openAIImagesGenerationsEndpoint}, ChannelMappingResult{}, nil)
	require.NoError(t, err)
	require.Equal(t, "original", snapshot.SEPComputeRequestID)
	require.InDelta(t, 0.24, snapshot.Cost.ActualCost, 1e-12)
	repo.price = nil
	key.Group.RateMultiplier = 99
	cost, err := snapshot.recordCost(&OpenAIRecordUsageInput{APIKey: key, User: key.User, Result: &OpenAIForwardResult{Model: price.Model, ImageCount: 1}})
	require.NoError(t, err)
	require.InDelta(t, 0.24, cost.ActualCost, 1e-12)
}

func TestSEPAsyncImageDispatchIsClaimedOnce(t *testing.T) {
	for _, endpoint := range []string{"/v1/images/generations/async", "/v1/images/edits/async"} {
		t.Run(endpoint, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			e := &SEPExecution{ComputeRequestID: "compute-async", Service: &SEPService{db: db}}
			r := httptest.NewRequest(http.MethodPost, "https://upstream.test"+endpoint, nil)
			r = r.WithContext(WithSEPExecution(r.Context(), e))
			r.Header.Set("X-SEP-Execution-ID", "private-id")
			mock.ExpectQuery("UPDATE sep_executions SET dispatched_at").WithArgs(int64(42), e.ComputeRequestID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(e.ComputeRequestID))
			require.NoError(t, ClaimSEPUpstreamDispatch(r, 42))
			require.Empty(t, r.Header.Get("X-SEP-Execution-ID"))
			mock.ExpectQuery("UPDATE sep_executions SET dispatched_at").WithArgs(int64(42), e.ComputeRequestID).WillReturnError(sql.ErrNoRows)
			require.ErrorContains(t, ClaimSEPUpstreamDispatch(r, 42), "EXECUTION_DISPATCH_ALREADY_CLAIMED")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestManagedVideoReferenceAutomaticDurationKeepsPerRequestPricing(t *testing.T) {
	svc, key := newImageBillingServiceForTest(nil)
	billing, _, price := testManagedPrice(t, BillingModePerRequest, ModelPriceItem{Kind: "request", Unit: "USD/request", Amount: "0.12"})
	svc.billingService = billing
	quote, err := svc.QuoteVideoBilling(t.Context(), key, price.Model, "480p", VideoAutoDuration, time.Now())
	require.NoError(t, err)
	require.NotNil(t, quote.Amount)
	require.InDelta(t, 0.12, quote.cost.ActualCost, 1e-12)
	billing, _, price = testManagedPrice(t, BillingModeVideo, ModelPriceItem{Kind: "video", Tier: "480p", Unit: "USD/second", Amount: "0.12"})
	svc.billingService = billing
	quote, err = svc.QuoteVideoBilling(t.Context(), key, price.Model, "480p", VideoAutoDuration, time.Now())
	require.NoError(t, err)
	require.Nil(t, quote.Amount)
	require.Contains(t, quote.Diagnostic, "explicit duration")
}

func TestSEPVideoAuxiliaryEndpoints(t *testing.T) {
	for _, test := range []struct {
		method, path string
		allowed      bool
	}{
		{"POST", "/v1/videos/references", true},
		{"PUT", "/v1/videos/task_1/cancel", true},
		{"POST", "/v1/videos/task_1/cancel", false},
		{"PUT", "/v1/videos/a/b/cancel", false},
		{"PUT", "/v1/videos//cancel", false},
		{"POST", "/v1/videos", false},
	} {
		require.Equal(t, test.allowed, sepGatewayMediaOperation(test.method, test.path), test.path)
	}
}

// The fixture uses the real SEP transaction and media outcome tables. Ordinary
// balance billing is forbidden so accidental dollar deductions fail the test.
type sepMediaOutcomeDB struct {
	UsageBillingRepository
	db *sql.DB
}

func (r *sepMediaOutcomeDB) Apply(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	return nil, errors.New("SEP media must not use ordinary balance billing")
}
func (r *sepMediaOutcomeDB) RegisterVideoBilling(ctx context.Context, b *VideoBillingOutcome) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO video_request_billing(api_key_id,user_id,request_id,fingerprint,model,resolution,duration_seconds) VALUES($1,$2,$3,$4,$5,$6,$7)`, b.APIKeyID, b.UserID, b.RequestID, b.Fingerprint, b.Model, b.Resolution, b.Seconds)
	return err
}
func (r *sepMediaOutcomeDB) FindVideoBillingOutcome(ctx context.Context, key, user int64, id string) (*VideoBillingOutcome, error) {
	b := &VideoBillingOutcome{APIKeyID: key, UserID: user, RequestID: id}
	err := r.db.QueryRowContext(ctx, `SELECT status,actual_cost::text,fingerprint FROM video_request_billing WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3`, key, user, id).Scan(&b.Status, &b.ActualCostUSD, &b.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}
func (*sepMediaOutcomeDB) CloseVideoBillingWithoutCharge(context.Context, int64, int64, string) (*VideoBillingOutcome, error) {
	return nil, errors.New("SEP must finalize both records atomically")
}
func (r *sepMediaOutcomeDB) RegisterImageBilling(ctx context.Context, b *ImageBillingOutcome) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO image_request_billing(api_key_id,user_id,request_id,fingerprint,model,image_size) VALUES($1,$2,$3,$4,$5,$6)`, b.APIKeyID, b.UserID, b.RequestID, b.Fingerprint, b.Model, b.Size)
	return err
}
func (r *sepMediaOutcomeDB) FindImageBillingOutcome(ctx context.Context, key, user int64, id string) (*ImageBillingOutcome, error) {
	b := &ImageBillingOutcome{APIKeyID: key, UserID: user, RequestID: id}
	err := r.db.QueryRowContext(ctx, `SELECT status,actual_cost::text,fingerprint FROM image_request_billing WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3`, key, user, id).Scan(&b.Status, &b.ActualCostUSD, &b.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}
func (*sepMediaOutcomeDB) CloseImageBillingWithoutCharge(context.Context, int64, int64, string) (*ImageBillingOutcome, error) {
	return nil, errors.New("SEP must finalize both records atomically")
}

func sepMediaFixture(t *testing.T, kind, origin string, key *APIKey, account *Account) (*SEPService, *sepMediaOutcomeDB) {
	t.Helper()
	s, db, _ := balanceTestService(t)
	_, err := db.Exec(`CREATE TABLE groups(id BIGINT PRIMARY KEY); INSERT INTO groups VALUES(3);
CREATE TABLE usage_logs(request_id TEXT,api_key_id BIGINT,actual_cost NUMERIC,total_cost NUMERIC,video_duration_seconds INT,video_resolution TEXT);
UPDATE api_keys SET user_id=1 WHERE id=1; INSERT INTO accounts(id) VALUES(5) ON CONFLICT DO NOTHING;`)
	require.NoError(t, err)
	for _, name := range []string{"186_sep_quotes_and_pending_media.sql", "187_sep_task_group_snapshot.sql", "182_video_request_billing.sql", "183_image_request_billing.sql"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(raw))
		require.NoError(t, err)
	}
	key.ID, key.UserID, key.User = 1, 1, &User{ID: 1}
	mode := BillingModeImage
	if kind == "video" {
		mode = BillingModeVideo
	}
	price := &ModelBasePrice{Model: "fixture", Version: 1, Price: AccountModelPrice{Mode: mode}}
	e := &SEPExecution{ComputeRequestID: "original-execution", ExecutionID: "original", EnterprisePK: 1, EnterpriseID: "ent-test", IntegrationID: "sep-test", Kind: kind, Rate: "7.15", Price: price, Multipliers: SEPMultipliers{Token: 1, Image: 1, Video: 1}, Service: s, DirectBilling: origin == "compute"}
	key.SEP = e
	_, err = db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,group_id,billing_origin,account_id) VALUES($1,'sep-test',1,'original',1,$2,'fixture','hash',7.15,$3,$4,3,$5,$6)`, e.ComputeRequestID, kind, string(sepJSON(price)), string(sepJSON(e.Multipliers)), origin, account.ID)
	require.NoError(t, err)
	_, err = s.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "initial-funds", Operation: "add", Amount: "10", Currency: "CNY"})
	require.NoError(t, err)
	return s, &sepMediaOutcomeDB{db: db}
}

func TestSEPDeferredVideoUsesOriginalExecutionAndFinalizesOnce(t *testing.T) {
	for _, origin := range []string{"sep", "compute"} {
		for _, state := range []string{"completed", "failed", "cancelled"} {
			t.Run(origin+"/"+state, func(t *testing.T) {
				svc, key, account, _ := newVideoContractFixture(t)
				svc.cfg.RunMode = "standard"
				svc.billingService = &BillingService{}
				sep, ledger := sepMediaFixture(t, "video", origin, key, account)
				svc.usageBillingRepo = ledger
				require.NoError(t, ledger.RegisterVideoBilling(t.Context(), &VideoBillingOutcome{APIKeyID: 1, UserID: 1, RequestID: "client:original-video", Fingerprint: "fixture", Model: "fixture", Resolution: "480p", Seconds: 4}))
				meta := &VideoTaskMetadata{TaskID: "task_original", RequestID: "original-video", BillingRequestID: "client:original-video", LastStatus: state, Billing: &VideoBillingSnapshot{SEPComputeRequestID: key.SEP.ComputeRequestID, KeyID: 1, UserID: 1, GroupID: 3, Model: "fixture", Multiplier: 1, Cost: CostBreakdown{TotalCost: 0.12, ActualCost: 0.12, BillingMode: "video"}, Resolution: "480p", Seconds: 4}}
				key.SEP = &SEPExecution{EnterprisePK: 1, EnterpriseID: "ent-test", IntegrationID: "sep-test", Kind: "video", Service: sep}
				key.Group.RateMultiplier = 99
				var wg sync.WaitGroup
				statuses := make(chan VideoBillingStatus, 8)
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); statuses <- svc.finishVideoBilling(t.Context(), key, account, meta, nil) }()
				}
				wg.Wait()
				close(statuses)
				want := "settled"
				if state != "completed" {
					want = "not_charged"
				}
				for result := range statuses {
					require.Nil(t, result.Error)
					require.Equal(t, want, result.Status)
				}
				assertSEPMediaAmounts(t, sep, ledger.db, origin, state)
			})
		}
	}
}

func assertSEPMediaAmounts(t *testing.T, sep *SEPService, db *sql.DB, origin, state string) {
	t.Helper()
	var fee, status, gross, task string
	require.NoError(t, db.QueryRow(`SELECT fee_status,execution_status,gross_cny::text,COALESCE(task_id,'') FROM sep_executions WHERE id='original-execution'`).Scan(&fee, &status, &gross, &task))
	require.Equal(t, "final", fee)
	require.Equal(t, state, status)
	wantGross := "0.000000"
	if state == "completed" {
		wantGross = "0.858000"
	}
	require.Equal(t, wantGross, gross)
	balance, err := sep.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	wantBalance := "10.000000"
	if origin == "compute" && state == "completed" {
		wantBalance = "9.142000"
	}
	require.Equal(t, wantBalance, balance.Available)
	var dollars string
	require.NoError(t, db.QueryRow(`SELECT balance::text FROM users WHERE id=1`).Scan(&dollars))
	require.Equal(t, "0", dollars)
	if origin == "compute" {
		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_settlements WHERE execution_pk='original-execution'`).Scan(&count))
		require.Equal(t, 1, count)
	}
}

func TestSEPAsyncImageUsesManagedPriceAndOriginalExecution(t *testing.T) {
	for _, origin := range []string{"sep", "compute"} {
		for _, failure := range []bool{false, true} {
			name := origin + "/success"
			if failure {
				name = origin + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				svc, tasks, key, account, parsed, payload := asyncFixture(t, func(r *http.Request) (*http.Response, error) {
					if r.Method == "POST" {
						return asyncResponse(202, asyncQueued), nil
					}
					if failure {
						return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"failed","created":123,"error":{"message":"Reference rejected"}}`), nil
					}
					return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"succeeded","created":123,"data":[{"url":"https://images.example/a.png"}]}`), nil
				})
				svc.cfg.RunMode = "standard"
				sep, ledger := sepMediaFixture(t, "image", origin, key, account)
				svc.usageBillingRepo = ledger
				svc.billingService = &BillingService{}
				svc.usageLogRepo = &openAIRecordUsageLogRepoStub{inserted: true}
				svc.deferredService = &DeferredService{}
				svc.billingCacheService = &BillingCacheService{}
				billing := &AsyncImageBillingSnapshot{SEPComputeRequestID: key.SEP.ComputeRequestID, KeyID: 1, UserID: 1, GroupID: 3, Model: parsed.Model, Size: parsed.Size, Multiplier: 1, Cost: CostBreakdown{TotalCost: 0.12, ActualCost: 0.12, BillingMode: "image"}, Endpoint: parsed.Endpoint + "/async"}
				c, _ := videoTestContext("POST", "/v1/images/generations/async", nil, "application/json")
				task, err := svc.SubmitAsyncImage(t.Context(), c, tasks, key, account, parsed, payload, "original-image", "original-image", billing)
				require.NoError(t, err)
				require.Equal(t, "pending", task.Billing.Status)
				key.SEP = &SEPExecution{EnterprisePK: 1, EnterpriseID: "ent-test", IntegrationID: "sep-test", Kind: "image", Service: sep}
				key.Group.RateMultiplier = 99
				state, want := "completed", "settled"
				if failure {
					state, want = "failed", "not_charged"
				}
				for i := 0; i < 2; i++ {
					record, err := tasks.LoadRecord(t.Context(), ImageTaskOwner{APIKeyID: 1, UserID: 1}, task.ID)
					require.NoError(t, err)
					result, err := svc.PollAsyncImage(t.Context(), c, tasks, key, record, account, nil)
					require.NoError(t, err)
					require.Nil(t, result.Billing.Error)
					require.Equal(t, want, result.Billing.Status)
				}
				assertSEPMediaAmounts(t, sep, ledger.db, origin, state)
				var associated string
				require.NoError(t, ledger.db.QueryRow(`SELECT task_id FROM sep_executions WHERE id='original-execution'`).Scan(&associated))
				require.Equal(t, task.ID, associated)
			})
		}
	}
}

func TestSEPClosedVideoSubmissionFinalizesWithoutCharge(t *testing.T) {
	svc, key, account, _ := newVideoContractFixture(t)
	sep, ledger := sepMediaFixture(t, "video", "compute", key, account)
	svc.usageBillingRepo = ledger
	require.NoError(t, ledger.RegisterVideoBilling(t.Context(), &VideoBillingOutcome{APIKeyID: 1, UserID: 1, RequestID: "client:closed", Fingerprint: "fixture", Model: "fixture", Resolution: "480p", Seconds: 4}))
	sub := &videoSubmission{RequestID: "closed", BillingRequestID: "client:closed", SemanticSHA256: "fixture", BillingRegistered: true, Billing: &VideoBillingSnapshot{SEPComputeRequestID: key.SEP.ComputeRequestID}}
	for i := 0; i < 2; i++ {
		status := svc.closedVideoSubmissionBilling(t.Context(), key, sub)
		require.Nil(t, status.Error)
		require.Equal(t, "not_charged", status.Status)
	}
	assertSEPMediaAmounts(t, sep, ledger.db, "compute", "failed")
}

func TestSEPMediaRestorationRejectsOtherKeysAndEnterprises(t *testing.T) {
	_, key, account, _ := newVideoContractFixture(t)
	_, _ = sepMediaFixture(t, "video", "sep", key, account)
	original := key.SEP.ComputeRequestID
	wrongKey := *key
	wrongKey.ID = 2
	_, err := restoreSEPMediaBillingKey(t.Context(), &wrongKey, original, "client:video")
	require.ErrorContains(t, err, "SEP_RESOURCE_NOT_FOUND")
	wrongEnterprise := *key
	e := *key.SEP
	e.EnterprisePK = 2
	wrongEnterprise.SEP = &e
	_, err = restoreSEPMediaBillingKey(t.Context(), &wrongEnterprise, original, "client:video")
	require.ErrorContains(t, err, "SEP_RESOURCE_NOT_FOUND")
}

func TestSEPVideoAuxiliaryAuthorizationKeepsPurposeAndRotationRules(t *testing.T) {
	s, db, settings := balanceTestService(t)
	settings.cfg.DirectBillingEnabled = true
	_, err := db.Exec(`UPDATE api_keys SET user_id=1 WHERE id=1;
INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,encryption_key_id,masked_key,encrypted_key) VALUES('video-credential',1,'video',1,'fixture','fixture','fixture')`)
	require.NoError(t, err)
	key := &APIKey{ID: 1, UserID: 1, Key: "sk-fixture"}
	request := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+key.Key)
		r.Header.Set("X-Compute-Billing-Mode", "direct")
		return r
	}
	for _, method := range []string{"upload", "cancel"} {
		r := request("POST", "/v1/videos/references")
		if method == "cancel" {
			r = request("PUT", "/v1/videos/task_1/cancel")
		}
		_, err = s.AuthorizeGateway(t.Context(), r, key)
		require.NoError(t, err)
	}
	_, err = db.Exec(`UPDATE sep_credentials SET kind='image'`)
	require.NoError(t, err)
	_, err = s.AuthorizeGateway(t.Context(), request("POST", "/v1/videos/references"), key)
	require.ErrorContains(t, err, "KEY_PURPOSE_MISMATCH")
	_, err = db.Exec(`UPDATE sep_credentials SET kind='video',generation_state='read_only',is_current=FALSE`)
	require.NoError(t, err)
	_, err = s.AuthorizeGateway(t.Context(), request("PUT", "/v1/videos/task_1/cancel"), key)
	require.ErrorContains(t, err, "CREDENTIAL_RETIRED")
}
