package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

func (r *userRepository) SetSEPAccountingService(s *service.SEPService) { r.sep = s }
func (r *userRepository) SEPAccountBalance(ctx context.Context, id int64) (*service.SEPAccountBalance, error) {
	if r.sep == nil {
		return nil, nil
	}
	return r.sep.AccountBalance(ctx, id)
}
func (r *userRepository) AdjustSEPAccountBalance(ctx context.Context, c service.SEPBalanceChange) (*service.SEPBalanceReceipt, error) {
	if r.sep == nil {
		return nil, nil
	}
	return r.sep.AdjustAccountBalance(ctx, c)
}
func (r *userRepository) SEPRefundBalance(ctx context.Context, id int64, action, amount string) (bool, error) {
	if r.sep == nil {
		return false, nil
	}
	return r.sep.RefundBalance(ctx, id, action, amount)
}
func (r *userRepository) enrichSEPUser(ctx context.Context, u *service.User) error {
	b, err := r.SEPAccountBalance(ctx, u.ID)
	if err != nil {
		return err
	}
	u.SEPBalance = b
	applySEPUserBalance(u, b)
	return nil
}
func applySEPUserBalance(u *service.User, b *service.SEPAccountBalance) {
	u.SEPBalance = b
	if b != nil {
		rate, _ := decimal.NewFromString(b.ExchangeRate)
		available, _ := decimal.NewFromString(b.Available)
		frozen, _ := decimal.NewFromString(b.Frozen)
		u.Balance = available.Div(rate).InexactFloat64()
		u.FrozenBalance = frozen.Div(rate).InexactFloat64()
	}
}
func (r *userRepository) enrichSEPUsers(ctx context.Context, users []service.User) error {
	if r.sep == nil {
		return nil
	}
	ids := make([]int64, len(users))
	for i := range users {
		ids[i] = users[i].ID
	}
	balances, err := r.sep.AccountBalances(ctx, ids)
	if err != nil {
		return err
	}
	for i := range users {
		applySEPUserBalance(&users[i], balances[users[i].ID])
	}
	return nil
}
