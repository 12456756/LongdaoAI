package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// VideoBillingQuote is a customer charge for one video, not a provider rate card.
// Mode video means per-second pricing; per_request does not multiply duration.
type VideoBillingQuote struct {
	managedBase bool
	Diagnostic  string
	Mode        BillingMode
	Source      string
	Amount      *string
	Multiplier  float64
	cost        *CostBreakdown
	mapping     ChannelMappingResult
}

var ErrVideoPricingChanged = errors.New("video pricing changed; obtain a new quote")

func (q *VideoBillingQuote) MappedModel() string { return q.mapping.MappedModel }

// FrozenVideoBilling is created only by server-side pre-dispatch verification.
// Its private value copy survives administrator changes during upstream I/O.
type FrozenVideoBilling struct {
	managedBase            bool
	keyID, userID, groupID int64
	model                  string
	multiplier             float64
	cost                   CostBreakdown
	mapping                ChannelMappingResult
}

func (f *FrozenVideoBilling) Mapping() ChannelMappingResult { return f.mapping }

func (f *FrozenVideoBilling) MatchesAccount(account *Account) bool {
	if f != nil && f.managedBase {
		return account != nil
	}
	return f != nil && account != nil && resolveOpenAIForwardModel(account, f.model, f.mapping.MappedModel) == f.mapping.MappedModel
}

func (q *VideoBillingQuote) Version(key *APIKey, model, operation, resolution string, seconds int) (string, error) {
	if q == nil || key == nil || key.GroupID == nil {
		return "", ErrVideoPricingChanged
	}
	fingerprint, err := json.Marshal(struct {
		KeyID, UserID, GroupID       int64
		Model, Operation, Resolution string
		Seconds                      int
		Mode                         BillingMode
		Source, Authority            string
		Amount                       *string
		Multiplier                   float64
		Cost                         *CostBreakdown
		Mapping                      ChannelMappingResult
	}{key.ID, key.UserID, *key.GroupID, model, operation, resolution, seconds,
		q.Mode, q.Source, PricingAuthoritySub2APICustomerBilling, q.Amount, q.Multiplier, q.cost, q.mapping})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(fingerprint)
	return "v1:" + hex.EncodeToString(sum[:]), nil
}

func (r *OpenAIVideoRequest) VideoBillingOperation() string {
	if r.referenceOperation != "" {
		return r.referenceOperation
	}
	if len(r.moderation.Uploads) > 0 || len(r.moderation.InputImageURLs) > 0 {
		return VideoOperationImage
	}
	return VideoOperationText
}

func (s *OpenAIGatewayService) PrepareVideoBilling(ctx context.Context, key *APIKey, request *OpenAIVideoRequest, operation, version string) (*FrozenVideoBilling, error) {
	if request == nil {
		return nil, fmt.Errorf("Video request is missing: %w", ErrVideoPricingChanged)
	}
	if operation != request.VideoBillingOperation() {
		return nil, fmt.Errorf("X-Video-Operation %q does not match request operation %q: %w", operation, request.VideoBillingOperation(), ErrVideoPricingChanged)
	}
	quote, err := s.QuoteVideoBilling(ctx, key, request.Model, request.Resolution, request.Seconds, time.Now())
	if err != nil {
		return nil, fmt.Errorf("Verify video quote: %w", err)
	}
	if quote == nil {
		return nil, fmt.Errorf("Video quote was not returned: %w", ErrVideoPricingChanged)
	}
	if quote.Amount == nil || quote.cost == nil {
		return nil, fmt.Errorf("Video model %q at %s has no valid price (source=%s, diagnostic=%s): %w", request.Model, request.Resolution, quote.Source, quote.Diagnostic, ErrVideoPricingChanged)
	}
	current, err := quote.Version(key, request.Model, operation, request.Resolution, request.Seconds)
	if err != nil {
		return nil, fmt.Errorf("Compute video pricing version: %w", err)
	}
	if version != "" && current != version {
		return nil, fmt.Errorf("Video pricing version no longer matches the current quote: %w", ErrVideoPricingChanged)
	}
	return &FrozenVideoBilling{keyID: key.ID, userID: key.UserID, groupID: *key.GroupID,
		model: request.Model, multiplier: quote.Multiplier, cost: *quote.cost, mapping: quote.mapping, managedBase: quote.managedBase}, nil
}

func (f *FrozenVideoBilling) recordCost(input *OpenAIRecordUsageInput) (*CostBreakdown, error) {
	if f == nil || input == nil || input.APIKey == nil || input.User == nil || input.Result == nil ||
		input.APIKey.ID != f.keyID || input.APIKey.UserID != f.userID || input.User.ID != f.userID ||
		input.APIKey.GroupID == nil || *input.APIKey.GroupID != f.groupID || input.Result.Model != f.model || input.Result.VideoCount != 1 {
		return nil, errors.New("frozen video billing does not match usage")
	}
	cost := f.cost
	return &cost, nil
}

func ValidVideoBillingResolution(resolution string) bool {
	switch resolution {
	case VideoBillingResolution480P, VideoBillingResolution720P, VideoBillingResolution1080P, VideoBillingResolution2K, VideoBillingResolution4K:
		return true
	}
	return false
}

func ValidVideoPricingDuration(seconds int) bool {
	return seconds == VideoAutoDuration || (seconds > 0 && seconds <= 600)
}

func (s *OpenAIGatewayService) QuoteVideoBilling(ctx context.Context, key *APIKey, model, resolution string, durationSeconds int, _ time.Time) (*VideoBillingQuote, error) {
	if s == nil || s.billingService == nil || key == nil || key.GroupID == nil || key.Group == nil ||
		key.ID <= 0 || key.UserID <= 0 || *key.GroupID != key.Group.ID || !ValidVideoBillingResolution(resolution) || !ValidVideoPricingDuration(durationSeconds) {
		return nil, errors.New("video billing is unavailable")
	}
	// Quotes and dispatch checks read current configuration, not auth/rate caches.
	if s.billingService.UsesManagedBasePrices() {
		base := s.ResolveUserGroupRateMultiplier(ctx, key.UserID, *key.GroupID, key.Group.RateMultiplier)
		p, err := s.billingService.BasePrice(ctx, model)
		if err != nil {
			return nil, err
		}
		quote := &VideoBillingQuote{managedBase: true, Mode: p.Price.Mode, Source: p.String(), Multiplier: resolveVideoRateMultiplier(key, base), mapping: ChannelMappingResult{MappedModel: model, BillingModelSource: BillingModelSourceRequested}}
		_, _, quote.Multiplier = SEPRequestMultipliers(key, 1, 1, quote.Multiplier)
		if s.channelService != nil {
			quote.mapping = s.channelService.ResolveChannelMapping(ctx, *key.GroupID, model)
		}
		if p.Price.Mode == BillingModeToken {
			return quote, nil
		}
		if durationSeconds == VideoAutoDuration && p.Price.Mode != BillingModePerRequest {
			quote.Diagnostic = "Automatic duration requires a configured per-request model base price; per-second billing requires an explicit duration"
			return quote, nil
		}
		cost, err := s.billingService.CalculateBasePrice(WithBasePriceSnapshot(ctx, p), model, resolution, UsageTokens{}, 1, durationSeconds, quote.Multiplier, "")
		if err != nil {
			return nil, err
		}
		cost.BillingMode = string(BillingModeVideo)
		quote.cost = cost
		amount := BillingUSDString(cost.ActualCost)
		quote.Amount = &amount
		return quote, nil
	}
	// Use a private channel snapshot so mapping and cost resolve against one view.
	view := &OpenAIGatewayService{billingService: s.billingService, channelService: s.channelService, resolver: s.resolver}
	if s.channelService != nil && s.channelService.groupRepo != nil {
		group, err := s.channelService.groupRepo.GetByIDLite(ctx, *key.GroupID)
		if err != nil {
			return nil, fmt.Errorf("Read video billing group: %w", err)
		}
		if group == nil || group.ID != *key.GroupID {
			return nil, errors.New("Video billing group was not found or does not match the API key group")
		}
		clone := *key
		clone.Group = group
		key = &clone
	}
	if s.channelService != nil && s.channelService.repo != nil {
		channels := NewChannelService(s.channelService.repo, nil, nil, nil)
		if _, err := channels.buildCache(ctx); err != nil {
			return &VideoBillingQuote{Mode: BillingModeVideo, Source: "model_mapping_unavailable", Multiplier: 1, Diagnostic: sanitizeUpstreamErrorMessage("Read video channel configuration: " + err.Error())}, nil
		}
		view.channelService = channels
		view.resolver = NewModelPricingResolver(channels, s.billingService)
	}
	base := key.Group.RateMultiplier
	if s.userGroupRateResolver != nil && s.userGroupRateResolver.repo != nil {
		rate, err := s.userGroupRateResolver.repo.GetByUserAndGroup(ctx, key.UserID, *key.GroupID)
		if err != nil {
			return nil, fmt.Errorf("Read user video billing rate: %w", err)
		}
		base = key.Group.RateMultiplier
		if rate != nil {
			base = *rate
		}
	}
	s = view
	// Like actual video billing, neither token peak pricing nor image multipliers apply.
	quote := &VideoBillingQuote{Mode: BillingModeVideo, Source: PricingSourceFallback, Multiplier: resolveVideoRateMultiplier(key, base), mapping: ChannelMappingResult{MappedModel: model, BillingModelSource: BillingModelSourceRequested}}
	billingModel := model
	if s.channelService != nil {
		lookup, err := s.channelService.lookupGroupChannel(ctx, *key.GroupID)
		if err != nil {
			quote.Source = "model_mapping_unavailable"
			quote.Diagnostic = sanitizeUpstreamErrorMessage("Read video group channel mapping: " + err.Error())
			return quote, nil
		}
		mapping := ChannelMappingResult{MappedModel: model}
		if lookup != nil {
			mapping = resolveMapping(lookup, *key.GroupID, model)
			quote.mapping = mapping
		}
		if mapping.BillingModelSource == BillingModelSourceUpstream {
			quote.Source = "upstream_model_unresolved"
			return quote, nil
		}
		if mapping.BillingModelSource == BillingModelSourceChannelMapped && mapping.MappedModel != "" {
			billingModel = mapping.MappedModel
		}
	}
	if durationSeconds != VideoAutoDuration && normalizeVideoUsageDurationSeconds(billingModel, durationSeconds) != durationSeconds {
		quote.Source = "duration_unresolved"
		return quote, nil
	}
	// RecordUsage selects token pricing before its group/video cost branch.
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, key); resolved != nil && resolved.Mode == BillingModeToken {
		quote.Source = "channel_token_unquotable"
		return quote, nil
	}
	cost, source, configured := s.videoCostDecision(ctx, billingModel, key, &OpenAIForwardResult{
		VideoCount: 1, VideoResolution: resolution, VideoDurationSeconds: durationSeconds,
	}, quote.Multiplier)
	quote.Source = source
	if durationSeconds == VideoAutoDuration && source != PricingSourceChannel {
		quote.Diagnostic = "Automatic duration requires a configured per-request channel price"
		return quote, nil
	}
	if source == PricingSourceChannel {
		quote.Mode = BillingModePerRequest
	}
	if configured && cost != nil && cost.BillingMode == string(BillingModeVideo) &&
		validImageBillingAmount(cost.TotalCost) && validImageBillingAmount(cost.ActualCost) && validImageBillingAmount(quote.Multiplier) {
		amount := BillingUSDString(cost.ActualCost)
		quote.Amount = &amount
		copy := *cost
		quote.cost = &copy
	}
	return quote, nil
}

// Shared with actual video billing. The final flag describes explicit, valid
// configuration, not whether the legacy fallback happens to produce a number.
func (s *OpenAIGatewayService) videoCostDecision(ctx context.Context, billingModel string, apiKey *APIKey, result *OpenAIForwardResult, multiplier float64) (*CostBreakdown, string, bool) {
	videoCount := result.VideoCount
	if videoCount <= 0 {
		videoCount = 1
	}
	resolution := NormalizeVideoBillingResolutionOrDefault(result.VideoResolution)
	durationSeconds := normalizeVideoUsageDurationSeconds(billingModel, result.VideoDurationSeconds)
	groupConfig := videoPriceConfigFromAPIKey(apiKey)
	if result.VideoDurationSeconds != VideoAutoDuration {
		if !apiKeyHasConfiguredVideoPrice(apiKey, resolution) {
			apiKey = s.apiKeyWithFreshGroupMediaPricing(ctx, apiKey)
			groupConfig = videoPriceConfigFromAPIKey(apiKey)
		}
		if apiKeyHasConfiguredVideoPrice(apiKey, resolution) {
			return s.billingService.CalculateVideoCost(billingModel, resolution, videoCount, durationSeconds, groupConfig, multiplier), "group", validImageBillingAmount(*apiKey.Group.GetVideoPrice(resolution))
		}
	}
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage) {
		gid := apiKey.Group.ID
		cost, err := s.billingService.CalculateCostUnified(CostInput{
			Ctx: ctx, Model: billingModel, GroupID: &gid, RequestCount: videoCount,
			SizeTier: resolution, RateMultiplier: multiplier, Resolver: s.resolver, Resolved: resolved,
		})
		if err == nil {
			cost.BillingMode = string(BillingModeVideo)
			return cost, PricingSourceChannel, videoChannelPriceConfigured(resolved, resolution)
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate video channel cost failed: %v", err)
	}
	return s.billingService.CalculateVideoCost(billingModel, resolution, videoCount, durationSeconds, groupConfig, multiplier), PricingSourceFallback, false
}

// A bare per_request mode with no applicable price is not an explicit free
// quote. Calculation remains entirely in CalculateCostUnified above.
func videoChannelPriceConfigured(resolved *ResolvedPricing, resolution string) bool {
	if resolved == nil || resolved.channelPricing == nil {
		return false
	}
	configured := resolved.channelPricing.PerRequestPrice != nil
	if configured && !validImageBillingAmount(*resolved.channelPricing.PerRequestPrice) {
		return false
	}
	for _, tier := range resolved.RequestTiers {
		if tier.PerRequestPrice != nil {
			if !validImageBillingAmount(*tier.PerRequestPrice) {
				return false
			}
			if tier.TierLabel == resolution {
				configured = true
			}
		}
	}
	if tier := FindMatchingInterval(resolved.RequestTiers, 0); tier != nil && tier.PerRequestPrice != nil {
		configured = true
	}
	return configured
}

// Video evidence is deliberately separate from the existing image contract.
type VideoRequestBillingEvidence struct {
	Model                *string
	BillingMode          string
	VideoCount           *int
	DurationSeconds      *int
	Resolution           *string
	ActualCostUSD        *string
	TransactionConfirmed bool
}

type VideoRequestBillingEvidenceReader interface {
	FindVideoRequestBillingEvidence(context.Context, int64, int64, []string) (*VideoRequestBillingEvidence, error)
}

func (s *OpenAIGatewayService) RequestVideoBilling(ctx context.Context, key *APIKey, id string) (*VideoRequestBillingEvidence, error) {
	if s == nil || key == nil || key.ID <= 0 || key.UserID <= 0 || strings.TrimSpace(id) == "" {
		return nil, errors.New("video request billing is unavailable")
	}
	reader, ok := s.usageBillingRepo.(VideoRequestBillingEvidenceReader)
	if !ok {
		return nil, errors.New("video request billing is unavailable")
	}
	return reader.FindVideoRequestBillingEvidence(ctx, key.ID, key.UserID, RequestBillingIDs(id))
}
