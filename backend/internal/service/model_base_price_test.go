//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type managedPriceRepoStub struct {
	ModelBasePriceRepository
	price *ModelBasePrice
	err   error
	calls int
}

func (r *managedPriceRepoStub) GetBase(_ context.Context, model string) (*ModelBasePrice, error) {
	r.calls++
	if r.price != nil && r.price.Model != model {
		return nil, r.err
	}
	return r.price, r.err
}

func testManagedPrice(t *testing.T, mode BillingMode, items ...ModelPriceItem) (*BillingService, *managedPriceRepoStub, *ModelBasePrice) {
	t.Helper()
	card := AccountModelPrice{Model: "custom-model", Mode: mode, Discount: "0.5", Markup: "2", Items: items}
	candidate, err := card.Candidate()
	require.NoError(t, err)
	price := &ModelBasePrice{Model: card.Model, Version: 1, Price: candidate}
	repo := &managedPriceRepoStub{price: price}
	svc := ProvideBillingService(&config.Config{}, nil, repo)
	return svc, repo, price
}

func TestManagedBasePriceExactDecimalAndValidation(t *testing.T) {
	p := AccountModelPrice{Model: "custom", Mode: BillingModeImage, Discount: "0.1", Markup: "1.2", Items: []ModelPriceItem{{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "0.3"}}}
	candidate, err := p.Candidate()
	require.NoError(t, err)
	require.Equal(t, "0.036", candidate.Items[0].Amount)
	require.Equal(t, "0.3", p.Items[0].Amount)
	p.Items[0].Amount = "0"
	candidate, err = p.Candidate()
	require.NoError(t, err)
	require.Equal(t, "0", candidate.Items[0].Amount)
	for _, value := range []string{"", "-1", "NaN", "1e3", "0.1234567890123"} {
		p.Items[0].Amount = value
		require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{p}))
	}
	p.Items[0].Amount = "1"
	p.Discount = "1.1"
	require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{p}))
	p.Discount = "1"
	p.Items = append(p.Items, p.Items[0])
	require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{p}))
}

func TestManagedBasePriceCurrenciesAndConversionSnapshot(t *testing.T) {
	for _, mode := range []BillingMode{BillingModeToken, BillingModeImage, BillingModeVideo, BillingModePerRequest} {
		t.Run(string(mode), func(t *testing.T) {
			kind, tier, unit := "input", "", "MTok"
			switch mode {
			case BillingModeImage:
				kind, tier, unit = "image", "1K", "image"
			case BillingModeVideo:
				kind, tier, unit = "video", "480p", "second"
			case BillingModePerRequest:
				kind, unit = "request", "request"
			}
			card := AccountModelPrice{Model: "cny-model", Currency: "CNY", Mode: mode, Discount: "0.5", Markup: "2", Items: []ModelPriceItem{{Kind: kind, Tier: tier, Unit: "CNY/" + unit, Amount: "1.43"}}}
			if mode == BillingModeToken {
				card.Items = append(card.Items, ModelPriceItem{Kind: "output", Unit: "CNY/MTok", Amount: "0"}, ModelPriceItem{Kind: "cache_read", Unit: "CNY/MTok", Amount: "0.143"})
			}
			_, err := card.Candidate()
			require.Error(t, err)
			converted, err := card.Candidate("7.15")
			require.NoError(t, err)
			require.Equal(t, "USD", converted.Currency)
			require.Equal(t, "0.2", converted.Items[0].Amount)
			require.Equal(t, "USD/"+unit, converted.Items[0].Unit)
			require.Equal(t, "CNY", converted.Conversion.SourceCurrency)
			require.Equal(t, "7.15", converted.Conversion.USDToCNYRate)
			require.Equal(t, "1.43", converted.Conversion.SourceItems[0].Amount)
			require.Equal(t, "CNY/"+unit, card.Items[0].Unit)
			if mode == BillingModeToken {
				require.Equal(t, "0", converted.Items[1].Amount)
				require.Equal(t, "0.02", converted.Items[2].Amount)
			}
			svc, _, base := testManagedPrice(t, mode, converted.Items...)
			base.Price = converted
			cost, err := svc.CalculateBasePrice(WithBasePriceSnapshot(context.Background(), base), card.Model, tier, UsageTokens{InputTokens: 1000000, OutputTokens: 100, CacheReadTokens: 1000000}, 2, 4, 1.5, "")
			require.NoError(t, err)
			expected := 0.4
			if mode == BillingModeToken {
				expected = 0.22
			} else if mode == BillingModeVideo {
				expected = 1.6
			}
			require.InDelta(t, expected, cost.TotalCost, 1e-12)
			require.InDelta(t, expected*1.5, cost.ActualCost, 1e-12)
			card.Currency = "EUR"
			require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{card}))
			card.Currency = "USD"
			require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{card}))
		})
	}
}

func TestManagedBasePriceCacheIncludedAndGroupRateAppliedOnce(t *testing.T) {
	svc, _, _ := testManagedPrice(t, BillingModeToken,
		ModelPriceItem{Kind: "input", Unit: "USD/MTok", Amount: "2"}, ModelPriceItem{Kind: "output", Unit: "USD/MTok", Amount: "10"},
		ModelPriceItem{Kind: "cache_read", Unit: "USD/MTok", Amount: "0.2"}, ModelPriceItem{Kind: "cache_write", Unit: "USD/MTok", Amount: "2.5"})
	cost, err := svc.CalculateBasePrice(context.Background(), "custom-model", "", UsageTokens{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 3000, CacheCreationTokens: 400}, 1, 0, 1.5, "")
	require.NoError(t, err)
	require.InDelta(t, 0.0056, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.0084, cost.ActualCost, 1e-12)
	require.InDelta(t, 0.0006, cost.CacheReadCost, 1e-12)
	require.InDelta(t, 0.001, cost.CacheCreationCost, 1e-12)
}

func TestManagedBasePriceSnapshotSurvivesEditsAndAccountMapping(t *testing.T) {
	svc, repo, p := testManagedPrice(t, BillingModeImage, ModelPriceItem{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "0.08"})
	ctx := WithBasePriceSnapshot(context.Background(), p)
	repo.price = &ModelBasePrice{Model: p.Model, Version: 2, Price: AccountModelPrice{Mode: BillingModeImage, Items: []ModelPriceItem{{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "9"}}}}
	cost, err := svc.CalculateBasePrice(ctx, "account-A-upstream-alias", "1K", UsageTokens{}, 2, 0, 1.5, "")
	require.NoError(t, err)
	require.InDelta(t, 0.24, cost.ActualCost, 1e-12)
	require.Zero(t, repo.calls)
	_, err = svc.CalculateBasePrice(context.Background(), "unknown-model", "1K", UsageTokens{}, 1, 0, 1, "")
	require.ErrorIs(t, err, ErrBasePriceMissing)
	repo.err = errors.New("database unavailable")
	_, err = svc.BasePrice(context.Background(), p.Model)
	require.ErrorIs(t, err, ErrBasePriceUnavailable)
}

func TestManagedBasePriceVideoUnitAndNoDefaults(t *testing.T) {
	svc, _, _ := testManagedPrice(t, BillingModeVideo, ModelPriceItem{Kind: "video", Tier: "720p", Unit: "USD/second", Amount: "0.12"})
	cost, err := svc.CalculateBasePrice(context.Background(), "custom-model", "720p", UsageTokens{}, 1, 10, 1.5, "")
	require.NoError(t, err)
	require.InDelta(t, 1.8, cost.ActualCost, 1e-12)
	_, err = svc.CalculateBasePrice(context.Background(), "custom-model", "1080p", UsageTokens{}, 1, 10, 1, "")
	require.Error(t, err)
	svc, _, _ = testManagedPrice(t, BillingModePerRequest, ModelPriceItem{Kind: "request", Tier: "720p", Unit: "USD/request", Amount: "0.12"})
	cost, err = svc.CalculateBasePrice(context.Background(), "custom-model", "720p", UsageTokens{}, 1, 10, 1.5, "")
	require.NoError(t, err)
	require.InDelta(t, 0.18, cost.ActualCost, 1e-12)
}

func TestManagedBasePriceCacheTTLandMissingItem(t *testing.T) {
	svc, _, _ := testManagedPrice(t, BillingModeToken, ModelPriceItem{Kind: "input", Unit: "USD/MTok", Amount: "2"}, ModelPriceItem{Kind: "output", Unit: "USD/MTok", Amount: "10"}, ModelPriceItem{Kind: "cache_write", Tier: "5m", Unit: "USD/MTok", Amount: "3"}, ModelPriceItem{Kind: "cache_write", Tier: "1h", Unit: "USD/MTok", Amount: "5"})
	cost, err := svc.CalculateBasePrice(context.Background(), "custom-model", "", UsageTokens{CacheCreationTokens: 3000, CacheCreation5mTokens: 1000, CacheCreation1hTokens: 2000}, 1, 0, 1, "")
	require.NoError(t, err)
	require.InDelta(t, 0.013, cost.CacheCreationCost, 1e-12)
	_, err = svc.CalculateBasePrice(context.Background(), "custom-model", "", UsageTokens{CacheReadTokens: 10}, 1, 0, 1, "")
	require.Error(t, err)
}

func TestManagedBasePriceBypassesChannelAndLegacyPrices(t *testing.T) {
	svc, _, p := testManagedPrice(t, BillingModeToken, ModelPriceItem{Kind: "input", Unit: "USD/MTok", Amount: "2"}, ModelPriceItem{Kind: "output", Unit: "USD/MTok", Amount: "10"})
	resolver := NewModelPricingResolver(nil, svc)
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: p.Model})
	require.Equal(t, "managed", resolved.Source)
	override := 900.0
	price, err := svc.GetModelPricingWithChannel(p.Model, &ChannelModelPricing{InputPrice: &override})
	require.NoError(t, err)
	require.InDelta(t, 0.000002, price.InputPricePerToken, 1e-15)
	require.Empty(t, svc.fallbackPrices)
}

func TestManagedBasePriceExplicitZeroAndPolicies(t *testing.T) {
	svc, _, p := testManagedPrice(t, BillingModeToken,
		ModelPriceItem{Kind: "input", Unit: "USD/MTok", Amount: "2"}, ModelPriceItem{Kind: "output", Unit: "USD/MTok", Amount: "10"},
		ModelPriceItem{Kind: "image_input", Unit: "USD/MTok", Amount: "0"}, ModelPriceItem{Kind: "input", Tier: "priority", Unit: "USD/MTok", Amount: "0"},
		ModelPriceItem{Kind: "cache_write", Unit: "USD/MTok", Amount: "5"}, ModelPriceItem{Kind: "cache_write", Tier: "5m", Unit: "USD/MTok", Amount: "0"}, ModelPriceItem{Kind: "cache_write", Tier: "1h", Unit: "USD/MTok", Amount: "0"})
	cost, err := svc.CalculateBasePrice(context.Background(), p.Model, "", UsageTokens{InputTokens: 100, ImageInputTokens: 50, CacheCreationTokens: 100, CacheCreation1hTokens: 100}, 1, 0, 1, "priority")
	require.NoError(t, err)
	require.Zero(t, cost.ActualCost)
	p.Price.Policy = ModelPricePolicy{LongContextThreshold: 100, LongContextInputMultiplier: 2}
	require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{p.Price}))
	p.Price.Policy.LongContextOutputMultiplier = 1.5
	require.NoError(t, ValidateAccountModelPrices([]AccountModelPrice{p.Price}))
	p.Price.Items = p.Price.Items[:len(p.Price.Items)-1]
	require.Error(t, ValidateAccountModelPrices([]AccountModelPrice{p.Price}))
}

func TestManagedBasePriceGatewayMultipliersAndAliases(t *testing.T) {
	svc, repo, price := testManagedPrice(t, BillingModeToken,
		ModelPriceItem{Kind: "input", Unit: "USD/MTok", Amount: "2"}, ModelPriceItem{Kind: "output", Unit: "USD/MTok", Amount: "10"}, ModelPriceItem{Kind: "cache_read", Unit: "USD/MTok", Amount: "0.2"})
	ctx := WithBasePriceSnapshot(context.Background(), price)
	openai := &OpenAIGatewayService{billingService: svc}
	for _, alias := range []string{"account-a-model", "account-b-model"} {
		cost, err := openai.calculateOpenAIRecordUsageCost(ctx, &OpenAIForwardResult{ImageCount: 1}, &APIKey{}, []string{alias}, 1.5, 9, 10, 1, UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 500}, "", true)
		require.NoError(t, err)
		require.InDelta(t, 0.00465, cost.ActualCost, 1e-12)
	}
	require.Zero(t, repo.calls)
	gateway := &GatewayService{billingService: svc}
	result := &ForwardResult{Usage: ClaudeUsage{InputTokens: 220000, OutputTokens: 100}}
	cost := gateway.calculateRecordUsageCost(ctx, result, &APIKey{}, "account-model", 1.5, 9, &recordUsageOpts{LongContextThreshold: 200000, LongContextMultiplier: 2})
	require.NoError(t, cost.Error)
	require.InDelta(t, 0.7215, cost.ActualCost, 1e-12)
}

func TestManagedBasePriceVideoQuoteFreeze(t *testing.T) {
	svc, repo, price := testManagedPrice(t, BillingModeVideo, ModelPriceItem{Kind: "video", Tier: "720p", Unit: "USD/second", Amount: "0.12"})
	gateway := &OpenAIGatewayService{billingService: svc}
	groupID := int64(7)
	key := &APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: &Group{ID: groupID, RateMultiplier: 1.5, VideoRateIndependent: true, VideoRateMultiplier: 0.7}}
	quote, err := gateway.QuoteVideoBilling(context.Background(), key, price.Model, "720p", 10, time.Now())
	require.NoError(t, err)
	require.InDelta(t, 0.84, quote.cost.ActualCost, 1e-12)
	version, err := quote.Version(key, price.Model, "text-to-video", "720p", 10)
	require.NoError(t, err)
	frozen, err := gateway.PrepareVideoBilling(context.Background(), key, &OpenAIVideoRequest{Model: price.Model, Resolution: "720p", Seconds: 10}, "text-to-video", version)
	require.NoError(t, err)
	require.True(t, frozen.MatchesAccount(&Account{Credentials: map[string]any{"model_mapping": map[string]any{price.Model: "different-upstream-alias"}}}))
	repo.price = nil
	cost, err := frozen.recordCost(&OpenAIRecordUsageInput{APIKey: key, User: &User{ID: 2}, Result: &OpenAIForwardResult{Model: price.Model, VideoCount: 1}})
	require.NoError(t, err)
	require.InDelta(t, 0.84, cost.ActualCost, 1e-12)
}
