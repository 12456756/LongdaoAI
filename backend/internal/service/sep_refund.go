package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/shopspring/decimal"
)

func (s *SEPService) RefundBalance(ctx context.Context, orderID int64, action, rawAmount string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	var pk, seq int64
	var available, frozen string
	err = tx.QueryRowContext(ctx, `SELECT e.id,e.sequence,e.available_cny::text,e.frozen_cny::text FROM sep_enterprises e JOIN payment_orders o ON o.user_id=e.user_id WHERE o.id=$1 FOR UPDATE OF e`, orderID).Scan(&pk, &seq, &available, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var attempt int
	var stored, state string
	err = tx.QueryRowContext(ctx, `SELECT attempt,amount_cny::text,status FROM sep_payment_refunds WHERE order_id=$1 FOR UPDATE`, orderID).Scan(&attempt, &stored, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	current, _ := decimal.NewFromString(available)
	var delta decimal.Decimal
	switch action {
	case "deduct":
		if state == "deducted" || state == "completed" {
			if stored != rawAmount {
				return true, sepConflict("REFUND_AMOUNT_CONFLICT", "该订单已有不同金额的退款扣款记录，未重复扣款")
			}
			return true, nil
		}
		amount, err := ValidateSEPAmount(rawAmount, true)
		if err != nil {
			return true, err
		}
		if current.LessThan(amount) {
			return true, SEPFailure(409, "INSUFFICIENT_REFUND_BALANCE", fmt.Sprintf("退款扣款失败：企业可用余额 %s 元，退款需扣 %s 元；未向支付渠道发起退款", available, rawAmount), "refund")
		}
		attempt++
		delta = amount.Neg()
		state = "deducted"
		stored = rawAmount
	case "restore":
		if state == "" || state == "restored" || state == "completed" {
			return true, nil
		}
		delta, _ = decimal.NewFromString(stored)
		state = "restored"
	case "complete":
		if state == "deducted" {
			_, err = tx.ExecContext(ctx, `UPDATE sep_payment_refunds SET status='completed' WHERE order_id=$1`, orderID)
			if err != nil {
				return true, err
			}
			return true, tx.Commit()
		}
		return true, nil
	default:
		return true, SEPFailure(400, "INVALID_REFUND_ACTION", "退款账务操作无效", "refund")
	}
	next := current.Add(delta)
	seq++
	kind := "compute_refund"
	if delta.IsPositive() {
		kind = "compute_credit"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET available_cny=$1,sequence=$2 WHERE id=$3`, next.StringFixed(6), seq, pk); err != nil {
		return true, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sep_ledger(id,enterprise_pk,sequence,type,amount_cny,available_cny,frozen_cny,origin,business_ref,notes) VALUES($1,$2,$3,$4,$5,$6,$7,'compute',$8,$9)`, sepRandom("ledger_"), pk, seq, kind, delta.Abs().StringFixed(6), next.StringFixed(6), frozen, fmt.Sprintf("payment-refund:%d:%d:%s", orderID, attempt, action), "支付订单退款："+action); err != nil {
		return true, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_payment_refunds(order_id,attempt,amount_cny,status) VALUES($1,$2,$3,$4) ON CONFLICT(order_id) DO UPDATE SET attempt=$2,amount_cny=$3,status=$4`, orderID, attempt, stored, state)
	if err != nil {
		return true, err
	}
	return true, tx.Commit()
}
