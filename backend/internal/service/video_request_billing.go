package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
)

// The snapshot contains prices and billing identity, never provider credentials.
type VideoBillingSnapshot struct {
	SEPComputeRequestID string               `json:"sep_compute_request_id,omitempty"`
	KeyID               int64                `json:"key_id"`
	UserID              int64                `json:"user_id"`
	GroupID             int64                `json:"group_id"`
	Model               string               `json:"model"`
	Multiplier          float64              `json:"multiplier"`
	Cost                CostBreakdown        `json:"cost"`
	Mapping             ChannelMappingResult `json:"mapping"`
	SubscriptionID      *int64               `json:"subscription_id,omitempty"`
	PayloadHash         string               `json:"payload_hash"`
	Resolution          string               `json:"resolution"`
	Seconds             int                  `json:"seconds"`
	Endpoint            string               `json:"endpoint"`
}

type VideoBillingOutcome struct {
	RequestID                      string
	APIKeyID, UserID               int64
	Fingerprint, Model, Resolution string
	Seconds                        int
	Status                         string
	ActualCostUSD                  *string
}

type VideoBillingOutcomeRepository interface {
	RegisterVideoBilling(context.Context, *VideoBillingOutcome) error
	FindVideoBillingOutcome(context.Context, int64, int64, string) (*VideoBillingOutcome, error)
	CloseVideoBillingWithoutCharge(context.Context, int64, int64, string) (*VideoBillingOutcome, error)
}

type VideoBillingStatus struct {
	Status        string  `json:"status"`
	ActualCostUSD *string `json:"actual_cost_usd"`
	Error         any     `json:"error,omitempty"`
}

type videoBillingInputKey struct{}

func WithVideoBillingInput(ctx context.Context, input *OpenAIRecordUsageInput) context.Context {
	return context.WithValue(ctx, videoBillingInputKey{}, input)
}

func (s *OpenAIGatewayService) videoBillingSnapshot(ctx context.Context, key *APIKey, request *OpenAIVideoRequest) (*VideoBillingSnapshot, error) {
	input, _ := ctx.Value(videoBillingInputKey{}).(*OpenAIRecordUsageInput)
	if input == nil {
		return nil, nil
	} // Existing internal callers and historical tasks.
	frozen := input.FrozenVideoBilling
	if frozen == nil {
		quote, err := s.QuoteVideoBilling(ctx, key, request.Model, request.Resolution, request.Seconds, time.Now())
		if err != nil {
			return nil, err
		}
		if quote == nil || quote.cost == nil || quote.Amount == nil {
			return nil, errors.New("Video request has no valid billing quote")
		}
		frozen = &FrozenVideoBilling{keyID: key.ID, userID: key.UserID, groupID: derefGroupID(key.GroupID), model: request.Model, multiplier: quote.Multiplier, cost: *quote.cost, mapping: quote.mapping}
	}
	snap := &VideoBillingSnapshot{KeyID: key.ID, UserID: key.UserID, GroupID: derefGroupID(key.GroupID), Model: request.Model, Multiplier: frozen.multiplier, Cost: frozen.cost, Mapping: frozen.mapping, PayloadHash: input.RequestPayloadHash, Resolution: request.Resolution, Seconds: request.Seconds, Endpoint: input.InboundEndpoint}
	if key.SEP != nil {
		snap.SEPComputeRequestID = key.SEP.ComputeRequestID
	}
	if input.Subscription != nil && key.Group != nil && key.Group.IsSubscriptionType() {
		id := input.Subscription.ID
		snap.SubscriptionID = &id
	}
	return snap, nil
}

func (s *OpenAIGatewayService) registerVideoBilling(ctx context.Context, sub *videoSubmission) error {
	if sub.Billing == nil || (s.cfg != nil && s.cfg.RunMode == config.RunModeSimple) {
		return nil
	}
	repo, ok := s.usageBillingRepo.(VideoBillingOutcomeRepository)
	if !ok {
		return errors.New("Video billing outcome repository is unavailable")
	}
	b := sub.Billing
	return repo.RegisterVideoBilling(ctx, &VideoBillingOutcome{RequestID: sub.BillingRequestID, APIKeyID: b.KeyID, UserID: b.UserID, Fingerprint: sub.SemanticSHA256, Model: b.Model, Resolution: b.Resolution, Seconds: b.Seconds, Status: "pending"})
}

func billingProblem(err error) VideoBillingStatus {
	return VideoBillingStatus{Status: "unknown", Error: map[string]any{"source": "platform", "code": "video_billing_unavailable", "message": errordetail.Text(err.Error())}}
}

func (s *OpenAIGatewayService) VideoRequestBillingStatus(ctx context.Context, key *APIKey, requestID string) VideoBillingStatus {
	// New rows use one canonical identity, not the current polling request ID.
	row, err := s.VideoRequestOutcome(ctx, key, requestID)
	if err != nil {
		return billingProblem(err)
	}
	if row != nil {
		return VideoBillingStatus{Status: row.Status, ActualCostUSD: row.ActualCostUSD}
	}
	evidence, err := s.RequestVideoBilling(ctx, key, requestID)
	if err != nil {
		return billingProblem(err)
	}
	if evidence != nil && evidence.TransactionConfirmed && evidence.BillingMode == string(BillingModeVideo) && evidence.Model != nil && *evidence.Model != "" && evidence.VideoCount != nil && *evidence.VideoCount > 0 && evidence.DurationSeconds != nil && ValidVideoPricingDuration(*evidence.DurationSeconds) && evidence.Resolution != nil && ValidVideoBillingResolution(*evidence.Resolution) && evidence.ActualCostUSD != nil && videoUSDPattern.MatchString(*evidence.ActualCostUSD) {
		if _, parseErr := strconv.ParseFloat(*evidence.ActualCostUSD, 64); parseErr == nil {
			return VideoBillingStatus{Status: "settled", ActualCostUSD: evidence.ActualCostUSD}
		}
	}
	return VideoBillingStatus{Status: "unknown"}
}

func (s *OpenAIGatewayService) VideoRequestOutcome(ctx context.Context, key *APIKey, id string) (*VideoBillingOutcome, error) {
	repo, ok := s.usageBillingRepo.(VideoBillingOutcomeRepository)
	if ok {
		row, err := findVideoOutcome(ctx, repo, key, id)
		return row, err
	}
	return nil, nil
}

func findVideoOutcome(ctx context.Context, repo VideoBillingOutcomeRepository, key *APIKey, id string) (*VideoBillingOutcome, error) {
	row, err := repo.FindVideoBillingOutcome(ctx, key.ID, key.UserID, "client:"+id)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(id, "client:") {
		alias, aliasErr := repo.FindVideoBillingOutcome(ctx, key.ID, key.UserID, id)
		if aliasErr != nil {
			return nil, aliasErr
		}
		if row != nil && alias != nil {
			return nil, errors.New("Ambiguous video billing request identity")
		}
		if row == nil {
			row = alias
		}
	}
	return row, nil
}

func (s *OpenAIGatewayService) finishVideoBilling(ctx context.Context, key *APIKey, account *Account, meta *VideoTaskMetadata, updater APIKeyQuotaUpdater) VideoBillingStatus {
	if meta.Billing == nil {
		return s.VideoRequestBillingStatus(ctx, key, strings.TrimPrefix(meta.BillingRequestID, "client:"))
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		zero := "0"
		return VideoBillingStatus{Status: "not_charged", ActualCostUSD: &zero}
	}
	if meta.Billing.SEPComputeRequestID != "" {
		var err error
		key, err = restoreSEPMediaBillingKey(ctx, key, meta.Billing.SEPComputeRequestID, meta.BillingRequestID)
		if err != nil {
			return billingProblem(err)
		}
	}
	repo, ok := s.usageBillingRepo.(VideoBillingOutcomeRepository)
	if !ok {
		return billingProblem(errors.New("Video billing outcome repository is unavailable"))
	}
	prior, priorErr := repo.FindVideoBillingOutcome(ctx, key.ID, key.UserID, meta.BillingRequestID)
	if priorErr != nil {
		return billingProblem(priorErr)
	}
	if prior == nil {
		return billingProblem(errors.New("Video billing record is missing"))
	}
	if prior.Status == "settled" || prior.Status == "not_charged" {
		return VideoBillingStatus{Status: prior.Status, ActualCostUSD: prior.ActualCostUSD}
	}
	if meta.LastStatus == "failed" || meta.LastStatus == "cancelled" {
		if key.SEP != nil {
			if err := key.SEP.Service.FinalizeUnchargedMedia(ctx, key, meta.LastStatus, true, prior.Fingerprint); err != nil {
				return billingProblem(err)
			}
			return s.VideoRequestBillingStatus(ctx, key, meta.RequestID)
		}
		row, err := repo.CloseVideoBillingWithoutCharge(ctx, key.ID, key.UserID, meta.BillingRequestID)
		if err != nil {
			return billingProblem(err)
		}
		return VideoBillingStatus{Status: row.Status, ActualCostUSD: row.ActualCostUSD}
	}
	if meta.LastStatus != "completed" {
		return s.VideoRequestBillingStatus(ctx, key, meta.RequestID)
	}
	b := meta.Billing
	if b.KeyID != key.ID || b.UserID != key.UserID || b.GroupID != derefGroupID(key.GroupID) || key.User == nil {
		return billingProblem(errors.New("Video billing snapshot identity mismatch"))
	}
	// Restore the payment method chosen at creation; group edits must not change it.
	billingKey := *key
	if key.Group == nil {
		return billingProblem(errors.New("Video billing group is unavailable"))
	}
	group := *key.Group
	billingKey.Group = &group
	var subscription *UserSubscription
	group.SubscriptionType = SubscriptionTypeStandard
	if b.SubscriptionID != nil {
		group.SubscriptionType = SubscriptionTypeSubscription
		subscription = &UserSubscription{ID: *b.SubscriptionID}
	}
	frozen := &FrozenVideoBilling{keyID: b.KeyID, userID: b.UserID, groupID: b.GroupID, model: b.Model, multiplier: b.Multiplier, cost: b.Cost, mapping: b.Mapping}
	billingCtx := SEPUsageContext(context.WithValue(ctx, ctxkey.ClientRequestID, strings.TrimPrefix(meta.BillingRequestID, "client:")), &billingKey)
	err := s.RecordUsage(billingCtx, &OpenAIRecordUsageInput{DeferredVideo: true, APIKey: &billingKey, User: key.User, Account: account, Subscription: subscription, APIKeyService: updater, FrozenVideoBilling: frozen, RequestPayloadHash: b.PayloadHash, InboundEndpoint: b.Endpoint, UpstreamEndpoint: "/v1/video/tasks", QuotaPlatform: PlatformOpenAI, ChannelUsageFields: b.Mapping.ToUsageFields(b.Model, meta.UpstreamModel), Result: &OpenAIForwardResult{ResponseID: meta.TaskID, Model: b.Model, BillingModel: meta.UpstreamModel, UpstreamModel: meta.UpstreamModel, VideoCount: 1, VideoResolution: b.Resolution, VideoDurationSeconds: b.Seconds}})
	if err != nil {
		status := s.VideoRequestBillingStatus(ctx, key, meta.RequestID)
		status.Error = map[string]any{"source": "platform", "code": "video_settlement_failed", "message": errordetail.Text(fmt.Sprint(err))}
		return status
	}
	return s.VideoRequestBillingStatus(ctx, key, meta.RequestID)
}
