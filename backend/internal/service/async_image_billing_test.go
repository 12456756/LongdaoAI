package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAsyncImageBillingSnapshotFreezesPriceAndPaymentMethod(t *testing.T) {
	svc, key := newImageBillingServiceForTest(nil)
	key.User = &User{ID: key.UserID}
	price := 0.12
	key.Group.ImagePrice1K = &price
	parsed := &OpenAIImagesRequest{Model: "gpt-image-2", N: 1, Size: "1024x1024", Endpoint: openAIImagesGenerationsEndpoint}
	billing, err := svc.PrepareAsyncImageBilling(context.Background(), key, parsed, ChannelMappingResult{}, nil)
	require.NoError(t, err)
	require.Equal(t, 0.12, billing.Cost.ActualCost)
	price = 9.9
	key.Group.RateMultiplier = 99
	cost, err := billing.recordCost(&OpenAIRecordUsageInput{APIKey: key, User: key.User, Result: &OpenAIForwardResult{Model: parsed.Model, ImageCount: 1}})
	require.NoError(t, err)
	require.Equal(t, 0.12, cost.ActualCost)
	_, err = billing.recordCost(&OpenAIRecordUsageInput{APIKey: key, User: key.User, Result: &OpenAIForwardResult{Model: parsed.Model, ImageCount: 2}})
	require.Error(t, err)
	key.Group.ImagePrice1K = nil
	svc, key = newImageBillingServiceForTest(&ChannelModelPricing{BillingMode: BillingModeToken})
	_, err = svc.PrepareAsyncImageBilling(context.Background(), key, parsed, ChannelMappingResult{}, nil)
	require.ErrorContains(t, err, "deterministic")
}

func TestAsyncImageBillingInProgressRegistrationDoesNotConfirmNoCharge(t *testing.T) {
	svc := &OpenAIGatewayService{}
	record := &ImageTaskRecord{Status: "processing", Provider: &AsyncImageMetadata{SubmissionStatus: "submitting", Billing: &AsyncImageBillingSnapshot{}}}
	for _, status := range []*ImageTaskBillingStatus{
		svc.AsyncImageBillingStatus(context.Background(), &APIKey{}, record),
		svc.finishAsyncImageBilling(context.Background(), &APIKey{}, nil, record, nil),
	} {
		require.Equal(t, "pending", status.Status)
		require.Nil(t, status.ActualCostUSD)
	}
	record.Provider.SubmissionStatus = "failed"
	status := svc.finishAsyncImageBilling(context.Background(), &APIKey{}, nil, record, nil)
	require.Equal(t, "not_charged", status.Status)
	require.Equal(t, "0", *status.ActualCostUSD)
}

func TestAsyncImageDisconnectedCallerPreservesReceiptAndSingleSettlement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	posts := 0
	svc, tasks, key, account, parsed, payload := asyncFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			posts++
			cancel()
			require.NoError(t, r.Context().Err())
			return asyncResponse(202, asyncQueued), nil
		}
		return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"succeeded","created":123,"data":[{"url":"https://images.example/a.png"}]}`), nil
	})
	svc.cfg.RunMode = "standard"
	ledger := &asyncImageLedgerFixture{}
	svc.usageBillingRepo = ledger
	svc.usageLogRepo = &openAIRecordUsageLogRepoStub{inserted: true}
	svc.deferredService = &DeferredService{}
	svc.billingCacheService = &BillingCacheService{}
	billing := &AsyncImageBillingSnapshot{KeyID: key.ID, UserID: key.UserID, GroupID: *key.GroupID, Model: parsed.Model, Size: parsed.Size, Multiplier: 1, Cost: CostBreakdown{BillingMode: "image", TotalCost: 0.12, ActualCost: 0.12}, Endpoint: parsed.Endpoint + "/async"}
	c, _ := videoTestContext("POST", "/v1/images/generations/async", nil, "application/json")
	created, err := svc.SubmitAsyncImage(ctx, c, tasks, key, account, parsed, payload, "disconnected-original", "disconnected-original", billing)
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, "accepted", created.SubmissionStatus)
	require.Equal(t, "pending", created.Billing.Status)
	for i := 0; i < 2; i++ {
		record, err := tasks.FindAsyncClaim(context.Background(), key, "disconnected-original", "")
		require.NoError(t, err)
		require.Equal(t, created.ID, record.ID)
		require.Equal(t, "imgjob_fixture", record.Provider.UpstreamTaskID)
		query, _ := videoTestContext("GET", "/v1/images/tasks/disconnected-original?id_type=request_id", nil, "")
		result, err := svc.PollAsyncImage(context.Background(), query, tasks, key, record, account, nil)
		require.NoError(t, err)
		require.Equal(t, "completed", result.Status)
		require.Equal(t, "settled", result.Billing.Status)
	}
	require.Equal(t, 1, posts)
	require.Len(t, ledger.commands, 1)
	require.Equal(t, "client:disconnected-original", ledger.commands[0].RequestID)
}

type asyncImageLedgerFixture struct {
	UsageBillingRepository
	mu        sync.Mutex
	row       *ImageBillingOutcome
	commands  []*UsageBillingCommand
	failApply bool
}

func (r *asyncImageLedgerFixture) RegisterImageBilling(_ context.Context, row *ImageBillingOutcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.row != nil {
		return ErrUsageBillingRequestConflict
	}
	clone := *row
	clone.Status = "pending"
	r.row = &clone
	return nil
}
func (r *asyncImageLedgerFixture) FindImageBillingOutcome(_ context.Context, key, user int64, id string) (*ImageBillingOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.row == nil || r.row.APIKeyID != key || r.row.UserID != user || r.row.RequestID != id {
		return nil, nil
	}
	clone := *r.row
	return &clone, nil
}
func (r *asyncImageLedgerFixture) CloseImageBillingWithoutCharge(_ context.Context, key, user int64, id string) (*ImageBillingOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.row == nil || r.row.APIKeyID != key || r.row.UserID != user || r.row.RequestID != id {
		return nil, errors.New("Invalid financial identity")
	}
	if r.row.Status == "pending" {
		r.row.Status = "not_charged"
		zero := "0"
		r.row.ActualCostUSD = &zero
	}
	clone := *r.row
	return &clone, nil
}
func (r *asyncImageLedgerFixture) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failApply {
		return nil, errors.New("Image settlement transaction unavailable")
	}
	if !cmd.DeferredImage || r.row == nil || r.row.RequestID != cmd.RequestID {
		return nil, errors.New("Invalid deferred image identity")
	}
	if r.row.Status == "settled" {
		return &UsageBillingApplyResult{}, nil
	}
	if r.row.Status != "pending" {
		return nil, errors.New("Image closed without charge")
	}
	r.row.Status = "settled"
	amount := BillingUSDString(cmd.BalanceCost + cmd.SubscriptionCost)
	r.row.ActualCostUSD = &amount
	clone := *cmd
	r.commands = append(r.commands, &clone)
	return &UsageBillingApplyResult{Applied: true}, nil
}

func TestAsyncImageBillingChargesOriginalRequestAndPreservesGenerationOnBillingFailure(t *testing.T) {
	for _, scenario := range []string{"success", "submission_error", "task_failure", "settlement_error"} {
		t.Run(scenario, func(t *testing.T) {
			svc, tasks, key, account, parsed, payload := asyncFixture(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					if scenario == "submission_error" {
						return asyncResponse(500, `{"message":"Image queue unavailable"}`), nil
					}
					return asyncResponse(202, asyncQueued), nil
				}
				if scenario == "task_failure" {
					return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"failed","created":123,"error":{"message":"Input content rejected","code":"content_rejected"}}`), nil
				}
				return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"succeeded","created":123,"data":[{"url":"https://images.example/a.png"}]}`), nil
			})
			svc.cfg.RunMode = "standard"
			ledger := &asyncImageLedgerFixture{failApply: scenario == "settlement_error"}
			svc.usageBillingRepo = ledger
			svc.usageLogRepo = &openAIRecordUsageLogRepoStub{inserted: true}
			svc.deferredService = &DeferredService{}
			svc.billingCacheService = &BillingCacheService{}
			billing := &AsyncImageBillingSnapshot{KeyID: key.ID, UserID: key.UserID, GroupID: *key.GroupID, Model: parsed.Model, Size: parsed.Size, Multiplier: 1, Cost: CostBreakdown{BillingMode: "image", TotalCost: 0.12, ActualCost: 0.12}, Endpoint: parsed.Endpoint + "/async"}
			c, _ := videoTestContext("POST", "/v1/images/generations/async", nil, "application/json")
			task, err := svc.SubmitAsyncImage(context.Background(), c, tasks, key, account, parsed, payload, "original-image", "original-image", billing)
			if scenario == "submission_error" {
				require.Error(t, err)
				require.Equal(t, "not_charged", task.Billing.Status)
				require.Empty(t, ledger.commands)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "pending", task.Billing.Status)
			require.Empty(t, ledger.commands)
			owner := ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}
			record, _ := tasks.LoadRecord(context.Background(), owner, task.ID)
			key.Group.RateMultiplier = 99
			got, err := svc.PollAsyncImage(context.Background(), c, tasks, key, record, account, nil)
			require.NoError(t, err)
			switch scenario {
			case "task_failure":
				require.Equal(t, "failed", got.Status)
				require.Equal(t, "not_charged", got.Billing.Status)
				require.Empty(t, ledger.commands)
			case "settlement_error":
				require.Equal(t, "completed", got.Status)
				require.Equal(t, "unknown", got.Billing.Status)
				require.Contains(t, got.Billing.Error.(map[string]any)["message"], "transaction unavailable")
				require.Empty(t, ledger.commands)
			default:
				require.Equal(t, "completed", got.Status)
				require.Equal(t, "settled", got.Billing.Status)
				require.Len(t, ledger.commands, 1)
				require.Equal(t, "client:original-image", ledger.commands[0].RequestID)
				require.InDelta(t, 0.12, ledger.commands[0].BalanceCost, 1e-12)
				record, _ = tasks.LoadRecord(context.Background(), owner, task.ID)
				_, err = svc.PollAsyncImage(context.Background(), c, tasks, key, record, account, nil)
				require.NoError(t, err)
				require.Len(t, ledger.commands, 1)
				changed := *parsed
				changed.Prompt = "different"
				changedPayload, err := BuildAsyncImagePayload(&changed, changed.Model, 0)
				require.NoError(t, err)
				conflict, _, err := svc.ReplayAsyncImage(context.Background(), tasks, key, &changed, changedPayload, "original-image", "original-image")
				require.Error(t, err)
				require.Equal(t, "settled", conflict.Billing.Status)
			}
		})
	}
}
