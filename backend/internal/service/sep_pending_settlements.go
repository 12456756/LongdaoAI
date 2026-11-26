package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// A pending decision is durable; it never means cash was deducted or a new model
// request should be made. The existing settlement id remains the retry anchor.
func rememberSEPPendingSettlement(ctx context.Context, tx *sql.Tx, integration, executionID, executionPK string, enterprisePK int64, hash string, decision SEPSettlement, available decimal.Decimal) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO sep_pending_settlements(integration_id,settlement_id,enterprise_pk,execution_pk,execution_id,request_hash,request,paid_cny)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, integration, decision.SettlementID, enterprisePK, executionPK, executionID, hash, string(sepJSON(decision)), decision.PaidCNY)
	if err != nil {
		return err
	}
	var originalHash string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM sep_pending_settlements WHERE execution_pk=$1`, executionPK).Scan(&originalHash)
	if errors.Is(err, sql.ErrNoRows) {
		return sepConflict("IDEMPOTENCY_CONFLICT", "结算编号已用于另一执行，请核对原单")
	}
	if err != nil {
		return err
	}
	if originalHash != hash {
		return sepConflict("IDEMPOTENCY_CONFLICT", "该执行已有待结算决定，请重试原结算编号及金额")
	}
	paid, _ := decimal.NewFromString(decision.PaidCNY)
	message := fmt.Sprintf("待结算费用 %s 元，可用余额 %s 元，资金缺口 %s 元；本次未扣款，补足后自动结算原单，不重新生成", decision.PaidCNY, available.StringFixed(6), decimal.Max(decimal.Zero, paid.Sub(available)).StringFixed(6))
	_, err = tx.ExecContext(ctx, `UPDATE sep_executions SET billing_error=$1,updated_at=NOW() WHERE id=$2`, message, executionPK)
	return err
}

func (s *SEPService) retryPendingSettlements(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT p.integration_id,p.execution_id,p.request FROM sep_pending_settlements p JOIN sep_enterprises e ON e.id=p.enterprise_pk
        WHERE e.available_cny>=p.paid_cny AND NOT EXISTS (SELECT 1 FROM sep_pending_settlements older WHERE older.enterprise_pk=p.enterprise_pk
        AND (older.created_at,older.settlement_id)<(p.created_at,p.settlement_id)) ORDER BY p.created_at,p.settlement_id LIMIT 100`)
	if err != nil {
		return err
	}
	type pending struct {
		integration, execution string
		request                SEPSettlement
	}
	var list []pending
	for rows.Next() {
		var item pending
		var raw []byte
		if err = rows.Scan(&item.integration, &item.execution, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &item.request); err != nil {
			rows.Close()
			return err
		}
		list = append(list, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range list {
		if _, err = s.Settle(ctx, item.integration, item.execution, item.request); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
