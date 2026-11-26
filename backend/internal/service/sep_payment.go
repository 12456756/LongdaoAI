package service

import (
	"context"
	"fmt"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/shopspring/decimal"
)

func (s *PaymentService) sepRefundUnconfirmed(ctx context.Context, p *RefundPlan, cause error) (*RefundResult, error) {
	result, err := s.markRefundPending(ctx, p, &payment.RefundResponse{Status: payment.ProviderStatusPending})
	if err != nil {
		return nil, err
	}
	result.Warning = fmt.Sprintf("退款结果尚未确认：%s；已保留 %s 元退款扣款，请查询原订单 %d，勿重复申请退款", safeSEPDiagnostic(cause), p.SEPRefundCNY, p.OrderID)
	s.writeAuditLog(ctx, p.OrderID, "REFUND_RESULT_UNCONFIRMED", "system", map[string]any{"reason": result.Warning})
	return result, nil
}

func sepOrderCredit(o *dbent.PaymentOrder) (string, string, error) {
	rate, _ := o.ProviderSnapshot["platform_usd_to_cny_rate"].(string)
	if err := ValidatePlatformExchangeRate(rate); err != nil {
		return "", "", SEPFailure(409, "PAYMENT_RATE_SNAPSHOT_MISSING", fmt.Sprintf("支付订单 %d 缺少有效的汇率快照，未入账；请核对原订单", o.ID), "payment")
	}
	amount, _ := o.ProviderSnapshot["sep_credit_cny"].(string)
	if amount == "" {
		r, _ := decimal.NewFromString(rate)
		amount = decimal.NewFromFloat(o.Amount).Mul(r).Round(6).StringFixed(6)
	}
	if _, err := ValidateSEPAmount(amount, true); err != nil {
		return "", "", err
	}
	return amount, rate, nil
}
