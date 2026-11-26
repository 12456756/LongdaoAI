package service

import (
	"context"
	"regexp"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

var exchangeRatePattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,5})(?:\.[0-9]{1,12})?$`)

func ValidatePlatformExchangeRate(raw string) error {
	rate, err := decimal.NewFromString(raw)
	if !exchangeRatePattern.MatchString(raw) || err != nil || !rate.IsPositive() {
		return infraerrors.BadRequest("INVALID_PLATFORM_EXCHANGE_RATE", "平台统一汇率必须为正数，最多 6 位整数和 12 位小数")
	}
	return nil
}

func platformExchangeRateValue(values map[string]string) string {
	rate := strings.TrimSpace(values[SettingKeyPlatformUSDToCNYRate])
	if rate == "" {
		return DefaultPlatformUSDToCNYRate
	}
	return rate
}

// GetPlatformExchangeRate is also the shared entry point for future CNY settlement.
func (s *SettingService) GetPlatformExchangeRate(ctx context.Context) (string, error) {
	return ReadPlatformExchangeRate(ctx, s.settingRepo)
}

func ReadPlatformExchangeRate(ctx context.Context, repo SettingRepository) (string, error) {
	values, err := repo.GetMultiple(ctx, []string{SettingKeyPlatformUSDToCNYRate})
	if err != nil {
		return "", err
	}
	rate := platformExchangeRateValue(values)
	return rate, ValidatePlatformExchangeRate(rate)
}

func convertUSDToPaymentCurrency(amount float64, rate, currency string) (float64, error) {
	if currency == "USD" {
		return amount, nil
	}
	if currency != "CNY" {
		return 0, infraerrors.BadRequest("UNSUPPORTED_PAYMENT_CURRENCY", "充值和订阅目前仅支持 CNY 或 USD 支付")
	}
	if err := ValidatePlatformExchangeRate(rate); err != nil {
		return 0, err
	}
	d, _ := decimal.NewFromString(rate)
	return decimal.NewFromFloat(amount).Mul(d).Round(2).InexactFloat64(), nil
}

func creditedBalanceForCurrency(amount float64, rate, currency string) (float64, error) {
	if currency == "USD" {
		return decimal.NewFromFloat(amount).Round(8).InexactFloat64(), nil
	}
	if currency != "CNY" {
		return 0, infraerrors.BadRequest("UNSUPPORTED_PAYMENT_CURRENCY", "充值和订阅目前仅支持 CNY 或 USD 支付")
	}
	if err := ValidatePlatformExchangeRate(rate); err != nil {
		return 0, err
	}
	d, _ := decimal.NewFromString(rate)
	return decimal.NewFromFloat(amount).DivRound(d, 8).InexactFloat64(), nil
}
