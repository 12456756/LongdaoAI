//go:build unit

package service

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Opt-in offline migration export. Inputs contain model/rate metadata only;
// supplier credentials and customer records are neither needed nor exported.
func TestDeploymentExportLegacyPrices(t *testing.T) {
	dir := os.Getenv("LONGDAO_DEPLOYMENT_PRICE_DIR")
	if dir == "" {
		t.Skip("offline deployment export is not requested")
	}
	var inventory struct {
		Accounts []struct {
			ID      int64             `json:"id"`
			Mapping map[string]string `json:"model_mapping"`
		} `json:"accounts"`
		Recent []struct {
			Model string `json:"model"`
		} `json:"recent_models"`
	}
	raw, err := os.ReadFile(dir + "/pricing-inventory.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &inventory))
	pricing := NewPricingService(&config.Config{}, nil)
	require.NoError(t, pricing.loadPricingData(dir+"/model_pricing.json"))
	billing := NewBillingService(&config.Config{}, pricing)
	models := map[string]bool{}
	for _, account := range inventory.Accounts {
		for model := range account.Mapping {
			if !strings.ContainsAny(model, "*?") {
				models[model] = true
			}
		}
	}
	for _, recent := range inventory.Recent {
		models[recent.Model] = true
	}
	type entry struct {
		Model  string            `json:"model"`
		Token  *ModelPricing     `json:"token,omitempty"`
		Images map[string]string `json:"images,omitempty"`
		Error  string            `json:"error,omitempty"`
	}
	var result []entry
	for model := range models {
		e := entry{Model: model}
		lower := strings.ToLower(model)
		if strings.Contains(lower, "image") || strings.Contains(lower, "imagen") || lower == "grok-imagine" || strings.Contains(lower, "imagine-edit") {
			e.Images = map[string]string{}
			for _, tier := range []string{"1K", "2K", "4K"} {
				e.Images[tier] = decimal.NewFromFloat(billing.CalculateImageCost(model, tier, 1, nil, 1).TotalCost).String()
			}
		} else if strings.Contains(lower, "seedance") || strings.Contains(lower, "imagine-video") || lower == "minimax-h3" {
			// Per-group video prices are copied from the inventory by the importer.
		} else {
			e.Token, err = billing.GetModelPricing(model)
			if err != nil {
				e.Error = err.Error()
			}
		}
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	out, err := json.MarshalIndent(result, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dir+"/legacy-effective-prices.json", out, 0600))
	t.Logf("Exported %d models without upstream requests", len(result))
}

func TestDeploymentValidateManagedPrices(t *testing.T) {
	dir := os.Getenv("LONGDAO_DEPLOYMENT_PRICE_DIR")
	if dir == "" {
		t.Skip("offline deployment comparison is not requested")
	}
	pricing := NewPricingService(&config.Config{}, nil)
	require.NoError(t, pricing.loadPricingData(dir+"/model_pricing.json"))
	billing := NewBillingService(&config.Config{}, pricing)
	raw, err := os.ReadFile(dir + "/managed-base-prices.json")
	require.NoError(t, err)
	var bases []ModelBasePrice
	require.NoError(t, json.Unmarshal(raw, &bases))
	for _, base := range bases {
		t.Run(base.Model, func(t *testing.T) {
			if base.Price.Mode == BillingModeToken {
				old, err := billing.GetModelPricing(base.Model)
				require.NoError(t, err)
				for _, tier := range []string{"", "priority", "flex"} {
					for _, tokens := range []UsageTokens{{InputTokens: 10000, OutputTokens: 500, CacheReadTokens: 3000, CacheCreationTokens: 1000}, {InputTokens: 300000, OutputTokens: 5000, CacheReadTokens: 100000, CacheCreationTokens: 20000}, {InputTokens: 10000, OutputTokens: 1000, CacheCreationTokens: 1500, CacheCreation5mTokens: 1000, CacheCreation1hTokens: 500}} {
						want := billing.computeTokenBreakdown(old, tokens, 0.5, tier, true)
						got, err := billing.CalculateBasePrice(WithBasePriceSnapshot(context.Background(), &base), base.Model, "", tokens, 1, 0, 0.5, tier)
						require.NoError(t, err)
						require.InDelta(t, want.ActualCost, got.ActualCost, 1e-9, "service tier %s", tier)
					}
				}
			} else if base.Price.Mode == BillingModeImage {
				for _, tier := range []string{"1K", "2K", "4K"} {
					want := billing.CalculateImageCost(base.Model, tier, 2, nil, 1)
					got, err := billing.CalculateBasePrice(WithBasePriceSnapshot(context.Background(), &base), base.Model, tier, UsageTokens{}, 2, 0, 1, "")
					require.NoError(t, err)
					require.InDelta(t, want.ActualCost, got.ActualCost, 1e-10)
				}
			}
		})
	}
}
