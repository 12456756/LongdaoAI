//go:build unit

package service

import (
	"context"
	"fmt"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type sepPaymentAccountingStub struct {
	UserRepository
	changes       []SEPBalanceChange
	refundActions []string
}

func (s *sepPaymentAccountingStub) SEPAccountBalance(context.Context, int64) (*SEPAccountBalance, error) {
	return &SEPAccountBalance{Available: "10.000000", ExchangeRate: "8"}, nil
}
func (s *sepPaymentAccountingStub) AdjustSEPAccountBalance(_ context.Context, c SEPBalanceChange) (*SEPBalanceReceipt, error) {
	s.changes = append(s.changes, c)
	return &SEPBalanceReceipt{Committed: true}, nil
}
func (s *sepPaymentAccountingStub) SEPRefundBalance(_ context.Context, _ int64, action, amount string) (bool, error) {
	s.refundActions = append(s.refundActions, action+":"+amount)
	return true, nil
}
func newSEPTestPaymentOrder(t *testing.T, status string) (*dbent.Client, *dbent.PaymentOrder) {
	t.Helper()
	client := newPaymentConfigServiceTestClient(t)
	user, err := client.User.Create().SetEmail(fmt.Sprintf("sep-payment-%d@example.test", time.Now().UnixNano())).SetPasswordHash("hash").Save(t.Context())
	require.NoError(t, err)
	order, err := client.PaymentOrder.Create().SetUserID(user.ID).SetUserEmail(user.Email).SetUserName("Enterprise").SetAmount(1).SetPayAmount(7.15).SetFeeRate(0).SetRechargeCode(fmt.Sprintf("sep-%d", time.Now().UnixNano())).SetOutTradeNo(fmt.Sprintf("sep-%d", time.Now().UnixNano())).SetPaymentTradeNo("payment-fixture").SetPaymentType(payment.TypeStripe).SetOrderType(payment.OrderTypeBalance).SetStatus(status).SetExpiresAt(time.Now().Add(time.Hour)).SetClientIP("127.0.0.1").SetSrcHost("localhost").SetProviderSnapshot(map[string]any{"platform_usd_to_cny_rate": "7.15", "sep_credit_cny": "7.150000"}).Save(t.Context())
	require.NoError(t, err)
	return client, order
}
func TestSEPPaymentFulfillmentUsesOriginalYuanCredit(t *testing.T) {
	client, order := newSEPTestPaymentOrder(t, OrderStatusRecharging)
	repo := &sepPaymentAccountingStub{}
	svc := &PaymentService{entClient: client, userRepo: repo}
	require.NoError(t, svc.doBalance(t.Context(), order, &paymentFulfillmentLease{version: order.UpdatedAt}))
	require.Len(t, repo.changes, 1)
	change := repo.changes[0]
	require.Equal(t, "7.150000", change.Amount)
	require.Equal(t, "CNY", change.Currency)
	require.Equal(t, "7.15", change.ExchangeRate)
	require.Equal(t, fmt.Sprintf("payment:%d", order.ID), change.BusinessRef)
	stored, err := client.PaymentOrder.Get(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, stored.Status)
}

func TestSEPPaymentCreditCannotBeReportedAsUncommittedWhenOrderStatusFails(t *testing.T) {
	client, order := newSEPTestPaymentOrder(t, OrderStatusRecharging)
	repo := &sepPaymentAccountingStub{}
	svc := &PaymentService{entClient: client, userRepo: repo}
	err := svc.doBalance(t.Context(), order, nil)
	require.ErrorContains(t, err, "PAYMENT_CREDITED_STATUS_PENDING")
	require.ErrorContains(t, err, "已入账")
	require.Equal(t, "committed", infraerrors.FromError(err).Metadata["accounting_status"])
	require.Len(t, repo.changes, 1)
}
func TestSEPRefundPendingKeepsDeductionAndFinalizationDoesNotUseDollarPath(t *testing.T) {
	client, order := newSEPTestPaymentOrder(t, OrderStatusRefunding)
	repo := &sepPaymentAccountingStub{}
	svc := &PaymentService{entClient: client, userRepo: repo}
	plan := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 1, BalanceToDeduct: 1, SEPRefundCNY: "7.150000", DeductionType: payment.DeductionTypeBalance}
	result, err := svc.finishRefund(t.Context(), plan, &payment.RefundResponse{Status: payment.ProviderStatusPending})
	require.NoError(t, err)
	require.Contains(t, result.Warning, "已保留 7.150000 元退款扣款")
	require.Empty(t, repo.refundActions)
	require.NoError(t, svc.applyRefundFinalDeduction(t.Context(), plan))
	require.Equal(t, []string{"deduct:7.150000", "complete:"}, repo.refundActions)
}
func TestSEPRefundUnconfirmedRetainsOriginalOrderForRecovery(t *testing.T) {
	client, order := newSEPTestPaymentOrder(t, OrderStatusRefunding)
	repo := &sepPaymentAccountingStub{}
	svc := &PaymentService{entClient: client, userRepo: repo}
	plan := &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 1, BalanceToDeduct: 1, SEPRefundCNY: "7.150000", DeductionType: payment.DeductionTypeBalance}
	result, err := svc.sepRefundUnconfirmed(t.Context(), plan, fmt.Errorf("payment provider connection timeout"))
	require.NoError(t, err)
	require.Contains(t, result.Warning, "payment provider connection timeout")
	require.Contains(t, result.Warning, "勿重复申请退款")
	require.Empty(t, repo.refundActions)
	stored, err := client.PaymentOrder.Get(t.Context(), order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefundPending, stored.Status)
}
