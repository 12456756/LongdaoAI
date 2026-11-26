package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestPlatformExchangeRateValidation(t *testing.T) {
	for _, raw := range []string{"", "0", "-1", "NaN", "Inf", "1e2", "7.1234567890123", "1000000", " 7.15 "} {
		t.Run(raw, func(t *testing.T) { require.Error(t, ValidatePlatformExchangeRate(raw)) })
	}
	for _, raw := range []string{"7.15", "7.123456789012", "0.000000000001", "999999.999999999999"} {
		require.NoError(t, ValidatePlatformExchangeRate(raw))
	}
}

func TestPlatformExchangeRateSingleSource(t *testing.T) {
	repo := &paymentConfigSettingRepoStub{values: map[string]string{
		SettingKeyPlatformUSDToCNYRate: "7.123456789012",
		"BALANCE_RECHARGE_MULTIPLIER":  "0.5",
		"SUBSCRIPTION_USD_TO_CNY_RATE": "9",
		"balance_display_cny_rate":     "8",
	}}
	settings := &SettingService{settingRepo: repo}
	rate, err := settings.GetPlatformExchangeRate(context.Background())
	require.NoError(t, err)
	require.Equal(t, "7.123456789012", rate)
	cfgService := &PaymentConfigService{settingRepo: repo}
	cfg, err := cfgService.GetPaymentConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, rate, cfg.PlatformUSDToCNYRate)
	fee := 2.5
	require.NoError(t, cfgService.UpdatePaymentConfig(context.Background(), UpdatePaymentConfigRequest{RechargeFeeRate: &fee}))
	require.NotContains(t, repo.updates, SettingKeyPlatformUSDToCNYRate)
	require.Equal(t, rate, repo.values[SettingKeyPlatformUSDToCNYRate])

	updated := "7.234567890123"
	repo.values[SettingKeyPlatformUSDToCNYRate] = updated
	rate, err = settings.GetPlatformExchangeRate(context.Background())
	require.NoError(t, err)
	require.Equal(t, updated, rate)
	delete(repo.values, SettingKeyPlatformUSDToCNYRate)
	rate, err = settings.GetPlatformExchangeRate(context.Background())
	require.NoError(t, err)
	require.Equal(t, DefaultPlatformUSDToCNYRate, rate)
}

func TestPaymentsRejectUnsupportedCurrencies(t *testing.T) {
	for _, currency := range []string{"HKD", "JPY", "EUR"} {
		_, err := creditedBalanceForCurrency(10, "7.15", currency)
		require.Error(t, err)
		for _, orderType := range []string{payment.OrderTypeBalance, payment.OrderTypeSubscription} {
			_, _, err := calculateCreateOrderPayAmountForOrderType(10, 0, currency, orderType, "7.15")
			require.Error(t, err)
		}
	}
}

func TestRefundKeepsEightDecimalBalanceProportion(t *testing.T) {
	// A partial USD refund must not become a full CNY refund due to a cent tolerance.
	require.Equal(t, 10.19, calculateGatewayRefundAmount(1.3986014, 10.25, 1.39, "CNY"))
	require.Equal(t, 10.25, calculateGatewayRefundAmount(1.3986014, 10.25, 1.3986014, "CNY"))
}
