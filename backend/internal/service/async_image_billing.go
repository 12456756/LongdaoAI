package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
)

type AsyncImageBillingSnapshot struct {
	KeyID          int64                `json:"key_id"`
	UserID         int64                `json:"user_id"`
	GroupID        int64                `json:"group_id"`
	Model          string               `json:"model"`
	Size           string               `json:"size"`
	Multiplier     float64              `json:"multiplier"`
	Cost           CostBreakdown        `json:"cost"`
	Mapping        ChannelMappingResult `json:"mapping"`
	SubscriptionID *int64               `json:"subscription_id,omitempty"`
	Endpoint       string               `json:"endpoint"`
}

type ImageBillingOutcome struct {
	RequestID                        string
	APIKeyID, UserID                 int64
	Fingerprint, Model, Size, Status string
	ActualCostUSD                    *string
}

type ImageBillingOutcomeRepository interface {
	RegisterImageBilling(context.Context, *ImageBillingOutcome) error
	FindImageBillingOutcome(context.Context, int64, int64, string) (*ImageBillingOutcome, error)
	CloseImageBillingWithoutCharge(context.Context, int64, int64, string) (*ImageBillingOutcome, error)
}

func (s *OpenAIGatewayService) PrepareAsyncImageBilling(ctx context.Context, key *APIKey, request *OpenAIImagesRequest, mapping ChannelMappingResult, subscription *UserSubscription) (*AsyncImageBillingSnapshot, error) {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, nil
	}
	quote, err := s.QuoteImageBilling(ctx, key, request.Model, request.Size, 1, time.Now())
	if err != nil {
		return nil, err
	}
	if quote == nil || quote.Amount == nil || quote.cost == nil {
		return nil, errors.New("Asynchronous image request has no deterministic image/per-request price; configure the model channel or group image price")
	}
	b := &AsyncImageBillingSnapshot{KeyID: key.ID, UserID: key.UserID, GroupID: derefGroupID(key.GroupID), Model: request.Model, Size: request.Size, Multiplier: quote.Multiplier, Cost: *quote.cost, Mapping: mapping, Endpoint: request.Endpoint + "/async"}
	if subscription != nil && key.Group != nil && key.Group.IsSubscriptionType() {
		id := subscription.ID
		b.SubscriptionID = &id
	}
	return b, nil
}

func (b *AsyncImageBillingSnapshot) recordCost(input *OpenAIRecordUsageInput) (*CostBreakdown, error) {
	if b == nil || input == nil || input.APIKey == nil || input.User == nil || input.Result == nil || input.APIKey.ID != b.KeyID || input.User.ID != b.UserID || derefGroupID(input.APIKey.GroupID) != b.GroupID || input.Result.Model != b.Model || input.Result.ImageCount != 1 {
		return nil, errors.New("Frozen image billing identity does not match usage")
	}
	cost := b.Cost
	return &cost, nil
}

func imageBillingProblem(err error) *ImageTaskBillingStatus {
	slog.Error("image_async.billing_failed", "reason", errordetail.Text(err.Error()))
	return &ImageTaskBillingStatus{Status: "unknown", Error: map[string]any{"source": "platform", "code": "image_billing_error", "message": errordetail.Text(err.Error())}}
}

func (s *OpenAIGatewayService) AsyncImageBillingStatus(ctx context.Context, key *APIKey, record *ImageTaskRecord) *ImageTaskBillingStatus {
	if !record.Provider.BillingRegistered {
		if record.Provider.SubmissionStatus == "submitting" && record.Provider.Billing != nil {
			return &ImageTaskBillingStatus{Status: "pending"}
		}
		return imageNotCharged()
	}
	repo, ok := s.usageBillingRepo.(ImageBillingOutcomeRepository)
	if !ok {
		return imageBillingProblem(errors.New("Image billing outcome repository is unavailable"))
	}
	row, err := repo.FindImageBillingOutcome(ctx, key.ID, key.UserID, "client:"+record.Provider.RequestID)
	if err != nil {
		return imageBillingProblem(err)
	}
	if row == nil {
		return imageBillingProblem(errors.New("Image billing evidence is missing"))
	}
	return &ImageTaskBillingStatus{Status: row.Status, ActualCostUSD: row.ActualCostUSD}
}

func (s *OpenAIGatewayService) ImageRequestOutcome(ctx context.Context, key *APIKey, id string) (*ImageBillingOutcome, error) {
	repo, ok := s.usageBillingRepo.(ImageBillingOutcomeRepository)
	if !ok {
		return nil, nil
	}
	var found *ImageBillingOutcome
	for _, alias := range RequestBillingIDs(id) {
		row, err := repo.FindImageBillingOutcome(ctx, key.ID, key.UserID, alias)
		if err != nil {
			return nil, err
		}
		if row != nil {
			if found != nil {
				return nil, errors.New("Ambiguous image billing request identity")
			}
			found = row
		}
	}
	return found, nil
}

func (s *OpenAIGatewayService) registerAsyncImageBilling(ctx context.Context, record *ImageTaskRecord) error {
	if record.Provider.Billing == nil {
		return nil
	}
	repo, ok := s.usageBillingRepo.(ImageBillingOutcomeRepository)
	if !ok {
		return errors.New("Image billing outcome repository is unavailable")
	}
	p := record.Provider
	return repo.RegisterImageBilling(ctx, &ImageBillingOutcome{APIKeyID: record.APIKeyID, UserID: record.UserID, RequestID: "client:" + p.RequestID, Fingerprint: p.Fingerprint, Model: p.Billing.Model, Size: p.Billing.Size})
}

func (s *OpenAIGatewayService) finishAsyncImageBilling(ctx context.Context, key *APIKey, account *Account, record *ImageTaskRecord, updater APIKeyQuotaUpdater) *ImageTaskBillingStatus {
	p := record.Provider
	if !p.BillingRegistered {
		return s.AsyncImageBillingStatus(ctx, key, record)
	}
	if p.Billing == nil {
		if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
			return imageNotCharged()
		}
		return imageBillingProblem(errors.New("Image task has no billing snapshot"))
	}
	repo, ok := s.usageBillingRepo.(ImageBillingOutcomeRepository)
	if !ok {
		return imageBillingProblem(errors.New("Image billing outcome repository is unavailable"))
	}
	row, err := repo.FindImageBillingOutcome(ctx, key.ID, key.UserID, "client:"+p.RequestID)
	if err != nil {
		return imageBillingProblem(err)
	}
	if row == nil {
		return imageBillingProblem(errors.New("Image billing record is missing"))
	}
	if row.Status == "settled" || row.Status == "not_charged" {
		return &ImageTaskBillingStatus{Status: row.Status, ActualCostUSD: row.ActualCostUSD}
	}
	if record.Status == ImageTaskStatusFailed {
		row, err = repo.CloseImageBillingWithoutCharge(ctx, key.ID, key.UserID, "client:"+p.RequestID)
	} else if record.Status == ImageTaskStatusCompleted {
		b := p.Billing
		if key.User == nil || key.Group == nil || key.ID != b.KeyID || key.UserID != b.UserID || derefGroupID(key.GroupID) != b.GroupID || account == nil {
			return imageBillingProblem(errors.New("Image settlement identity or original account is unavailable"))
		}
		billingKey := *key
		group := *key.Group
		billingKey.Group = &group
		group.SubscriptionType = SubscriptionTypeStandard
		var subscription *UserSubscription
		if b.SubscriptionID != nil {
			group.SubscriptionType = SubscriptionTypeSubscription
			subscription = &UserSubscription{ID: *b.SubscriptionID}
		}
		billingCtx := context.WithValue(ctx, ctxkey.ClientRequestID, p.RequestID)
		err = s.RecordUsage(billingCtx, &OpenAIRecordUsageInput{DeferredImage: true, FrozenImageBilling: b, APIKey: &billingKey, User: key.User, Account: account, Subscription: subscription, APIKeyService: updater, RequestPayloadHash: p.Fingerprint, InboundEndpoint: b.Endpoint, UpstreamEndpoint: "/v1/images/generations/async", QuotaPlatform: PlatformOpenAI, ChannelUsageFields: b.Mapping.ToUsageFields(b.Model, p.UpstreamModel), Result: &OpenAIForwardResult{ResponseID: p.UpstreamTaskID, Model: b.Model, UpstreamModel: p.UpstreamModel, ImageCount: 1, ImageSize: b.Size, ImageInputSize: b.Size}})
		if err == nil {
			row, err = repo.FindImageBillingOutcome(ctx, key.ID, key.UserID, "client:"+p.RequestID)
		}
	}
	if err != nil {
		return imageBillingProblem(err)
	}
	if row == nil {
		return imageBillingProblem(errors.New("Image settlement evidence is missing"))
	}
	return &ImageTaskBillingStatus{Status: row.Status, ActualCostUSD: row.ActualCostUSD}
}
