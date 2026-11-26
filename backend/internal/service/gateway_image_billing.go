package service

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// ImageBillingQuote describes sub2api's customer charge, not its supplier cost.
type ImageBillingQuote struct {
	cost             *CostBreakdown
	Mode             BillingMode
	Source           string
	PricingAuthority string
	Amount           *string
	Multiplier       float64
}

const PricingAuthoritySub2APICustomerBilling = "sub2api_customer_billing"

func BillingUSDString(amount float64) string {
	return strconv.FormatFloat(amount, 'f', 10, 64)
}

func validImageBillingAmount(amount float64) bool {
	return !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount >= 0
}

// ImageBillingModelAllowed uses the same group-scoped discovery as /v1/models.
func (s *GatewayService) ImageBillingModelAllowed(ctx context.Context, key *APIKey, model string) bool {
	if s == nil || key == nil || key.Group == nil || key.GroupID == nil || *key.GroupID != key.Group.ID {
		return false
	}
	if s.channelService != nil && s.channelService.IsModelRestricted(ctx, *key.GroupID, model) {
		return false
	}
	return customModelsListAllowsModel(s.GetEffectiveModels(ctx, key.Group), model)
}

func (s *OpenAIGatewayService) QuoteImageBilling(ctx context.Context, key *APIKey, model, size string, count int, now time.Time) (*ImageBillingQuote, error) {
	if s == nil || s.billingService == nil || key == nil || key.Group == nil || key.GroupID == nil {
		return nil, errors.New("image billing is unavailable")
	}
	base := s.ResolveUserGroupRateMultiplier(ctx, key.UserID, *key.GroupID, key.Group.RateMultiplier)
	tokenMultiplier, imageMultiplier := computePeakAwareMultipliers(key, base, now)
	tokenMultiplier, imageMultiplier, _ = SEPRequestMultipliers(key, tokenMultiplier, imageMultiplier, 1)
	if s.billingService.UsesManagedBasePrices() {
		p, err := s.billingService.BasePrice(ctx, model)
		if err != nil {
			return nil, err
		}
		quote := &ImageBillingQuote{Mode: p.Price.Mode, Source: p.String(), Multiplier: imageMultiplier, PricingAuthority: PricingAuthoritySub2APICustomerBilling}
		if p.Price.Mode == BillingModeToken {
			quote.Multiplier = tokenMultiplier
			return quote, nil
		}
		cost, err := s.billingService.CalculateBasePrice(WithBasePriceSnapshot(ctx, p), model, NormalizeImageBillingTierOrDefault(size), UsageTokens{}, count, 0, imageMultiplier, "")
		if err != nil {
			return nil, err
		}
		amount := BillingUSDString(cost.ActualCost)
		quote.Amount = &amount
		quote.cost = cost
		return quote, nil
	}
	quote := &ImageBillingQuote{Mode: BillingModeImage, Source: PricingSourceFallback, Multiplier: imageMultiplier}
	billingModel := model
	if s.channelService != nil {
		mapping := s.channelService.ResolveChannelMapping(ctx, *key.GroupID, model)
		if mapping.BillingModelSource == BillingModelSourceUpstream {
			quote.Source = "upstream_model_unresolved"
			return quote, nil
		}
		if mapping.BillingModelSource == BillingModelSourceChannelMapped && mapping.MappedModel != "" {
			billingModel = mapping.MappedModel
		}
	}
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, key); resolved != nil && resolved.Mode == BillingModeToken {
		quote.Mode, quote.Source, quote.Multiplier = BillingModeToken, PricingSourceChannel, tokenMultiplier
		return quote, nil
	}
	cost, source, deterministic := s.imageCostDecision(ctx, billingModel, key, &OpenAIForwardResult{ImageSize: size, ImageCount: count}, imageMultiplier, true)
	quote.Source = source
	if cost != nil {
		quote.Mode = BillingMode(cost.BillingMode)
		if deterministic && (quote.Mode == BillingModeImage || quote.Mode == BillingModePerRequest) &&
			validImageBillingAmount(cost.TotalCost) && validImageBillingAmount(cost.ActualCost) && validImageBillingAmount(imageMultiplier) {
			amount := BillingUSDString(cost.ActualCost)
			quote.Amount = &amount
			copy := *cost
			quote.cost = &copy
			quote.PricingAuthority = PricingAuthoritySub2APICustomerBilling
		}
	}
	return quote, nil
}

// Shared by actual image billing and read-only quoting, including existing defaults.
// The boolean result indicates a deterministic image/per-request calculation.
func (s *OpenAIGatewayService) imageCostDecision(ctx context.Context, model string, key *APIKey, result *OpenAIForwardResult, multiplier float64, quoteOnly bool) (*CostBreakdown, string, bool) {
	tier := NormalizeImageBillingTierOrDefault(result.ImageSize)
	if !apiKeyHasConfiguredImagePrice(key, tier) {
		key = s.apiKeyWithFreshGroupMediaPricing(ctx, key)
	}
	config := imagePriceConfigFromAPIKey(key)
	if apiKeyHasConfiguredImagePrice(key, tier) {
		price := key.Group.GetImagePrice(tier)
		if quoteOnly && !validImageBillingAmount(*price) {
			return nil, "group", false
		}
		return s.billingService.CalculateImageCost(model, tier, result.ImageCount, config, multiplier), "group", true
	}
	if resolved := s.resolveOpenAIChannelPricing(ctx, model, key); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage) {
		gid := key.Group.ID
		// Actual image billing also omits Tokens: missing/zero size tiers use the
		// resolver's zero-context/default price, not future upstream token usage.
		cost, err := s.billingService.CalculateCostUnified(CostInput{Ctx: ctx, Model: model, GroupID: &gid,
			RequestCount: result.ImageCount, SizeTier: tier, RateMultiplier: multiplier, Resolver: s.resolver, Resolved: resolved})
		if err == nil {
			return cost, PricingSourceChannel, true
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate image channel cost failed: %v", err)
	}
	return s.billingService.CalculateImageCost(model, tier, result.ImageCount, config, multiplier), PricingSourceFallback, true
}

// RequestBillingEvidence combines the usage log amount with the billing
// transaction marker. The amount is the customer-facing charge in USD.
type RequestBillingEvidence struct {
	Model                string
	BillingMode          string
	ImageCount           int
	ActualCostUSD        *string
	TransactionConfirmed bool
}

type RequestBillingEvidenceReader interface {
	FindRequestBillingEvidence(context.Context, int64, int64, []string) (*RequestBillingEvidence, error)
}

// RequestBillingIDs accepts a stored ID, a response X-Client-Request-ID (client:),
// or a response X-Request-ID used by legacy local: billing. It never equates two
// unrelated headers. Multiple matches are ambiguous and must remain pending.
func RequestBillingIDs(id string) []string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "client:") || strings.HasPrefix(id, "local:") || strings.HasPrefix(id, "generated:") {
		return []string{id}
	}
	return []string{id, "client:" + id, "local:" + id}
}

func (s *OpenAIGatewayService) RequestImageBilling(ctx context.Context, key *APIKey, id string) (*RequestBillingEvidence, error) {
	if s == nil || key == nil || key.ID <= 0 || key.UserID <= 0 {
		return nil, errors.New("request billing is unavailable")
	}
	reader, ok := s.usageBillingRepo.(RequestBillingEvidenceReader)
	if !ok {
		return nil, errors.New("request billing is unavailable")
	}
	return reader.FindRequestBillingEvidence(ctx, key.ID, key.UserID, RequestBillingIDs(id))
}
