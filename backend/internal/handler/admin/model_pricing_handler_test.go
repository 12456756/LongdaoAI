package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestModelPriceCatalogPagination(t *testing.T) {
	companyID := int64(1)
	catalog := &service.ModelPriceCatalog{
		Accounts: []service.ModelPriceAccount{{ID: 1, CompanyID: &companyID, Platform: "openai", Models: []string{"a", "b", "c"}, Prices: []service.AccountModelPrice{{ID: 1, Model: "a", Revision: 2, Mode: service.BillingModeToken}}}},
		Bases:    []service.ModelBasePrice{{Model: "a", SourcePriceID: 1, SourceRevision: 1}},
	}
	result := filterPriceCatalog(catalog, "", "1", "openai", "", "missing", 1, 1)
	require.Equal(t, []string{"b"}, result.Models)
	require.Equal(t, 2, result.Total)
	require.Empty(t, result.Accounts[0].Prices)
	result = filterPriceCatalog(catalog, "", "", "", "", "changed", 1, 20)
	require.Equal(t, []string{"a"}, result.Models)
	require.Len(t, result.Accounts[0].Prices, 1)
	result = filterPriceCatalog(catalog, "does-not-exist", "", "", "", "", 1, 20)
	require.Empty(t, result.Models)
	require.Zero(t, result.Total)
	require.Len(t, catalog.Accounts[0].Prices, 1)
}

func TestModelPriceCatalogExchangeRateChange(t *testing.T) {
	catalog := &service.ModelPriceCatalog{
		USDToCNYRate: "8",
		Accounts: []service.ModelPriceAccount{{Prices: []service.AccountModelPrice{
			{ID: 1, Model: "cny", Revision: 1, Currency: "CNY"},
			{ID: 2, Model: "usd", Revision: 1},
		}}},
		Bases: []service.ModelBasePrice{
			{Model: "cny", SourcePriceID: 1, SourceRevision: 1, Price: service.AccountModelPrice{Conversion: &service.ModelPriceConversion{USDToCNYRate: "7.15"}}},
			{Model: "usd", SourcePriceID: 2, SourceRevision: 1},
		},
	}
	result := filterPriceCatalog(catalog, "", "", "", "", "changed", 1, 20)
	require.Equal(t, []string{"cny"}, result.Models)
	require.Equal(t, "8", result.USDToCNYRate)
	catalog.Bases[0].Price.Conversion.USDToCNYRate = "8"
	require.Empty(t, filterPriceCatalog(catalog, "", "", "", "", "changed", 1, 20).Models)
}
