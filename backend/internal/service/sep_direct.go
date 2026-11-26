package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Insufficient postpaid funds keep the confirmed bill outstanding. The worker retries
// settlement after recharge; it never retries model generation.
func settleSEPDirect(ctx context.Context, tx *sql.Tx, id string, pk int64, available, frozen string, seq int64, gross string) error {
	current, _ := decimal.NewFromString(available)
	amount, _ := decimal.NewFromString(gross)
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_pending_settlements WHERE enterprise_pk=$1)`, pk).Scan(&pending); err != nil {
		return err
	}
	if pending && amount.IsPositive() {
		_, err := tx.ExecContext(ctx, `UPDATE sep_executions SET billing_error=$1 WHERE id=$2`, "企业已有待结算费用，等待原账单处理；不重新生成", id)
		return err
	}
	if current.LessThan(amount) {
		_, err := tx.ExecContext(ctx, `UPDATE sep_executions SET billing_error=$1 WHERE id=$2`, fmt.Sprintf("可用余额 %s 元，已确认费用 %s 元；本次尚未扣款，充值后自动补扣原账单，不重新生成", available, gross), id)
		return err
	}
	var execution, integration, bill, enterprise string
	if err := tx.QueryRowContext(ctx, `SELECT x.execution_id,x.integration_id,x.bill_id,e.enterprise_id FROM sep_executions x JOIN sep_enterprises e ON e.id=x.enterprise_pk WHERE x.id=$1`, id).Scan(&execution, &integration, &bill, &enterprise); err != nil {
		return err
	}
	current = current.Sub(amount)
	seq++
	settlement := "direct:" + id
	lid := sepRandom("ledger_")
	receipt := map[string]any{"contract_version": 1, "enterprise_id": enterprise, "execution_id": execution, "settlement_id": settlement, "bill_id": bill, "status": "committed", "currency": "CNY", "charged_paid_cny": gross, "recorded_free_cny": "0.000000", "ledger_entry_id": lid, "sequence": fmt.Sprint(seq), "origin": "compute", "committed_at": time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `UPDATE sep_enterprises SET available_cny=$1,sequence=$2 WHERE id=$3`, current.StringFixed(6), seq, pk); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sep_ledger(id,enterprise_pk,sequence,type,amount_cny,available_cny,frozen_cny,execution_id,settlement_id,origin,business_ref) VALUES($1,$2,$3,'paid_consumption',$4,$5,$6,$7,$8,'compute',$9)`, lid, pk, seq, gross, current.StringFixed(6), frozen, execution, settlement, execution); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sep_settlements(integration_id,settlement_id,execution_pk,request_hash,gross_cny,free_cny,paid_cny,receipt) VALUES($1,$2,$3,$4,$5,0,$5,$6)`, integration, settlement, id, sepHash([]byte(id+":"+gross)), gross, string(sepJSON(receipt)))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sep_executions SET billing_error='' WHERE id=$1`, id)
	return err
}

func (s *SEPService) retryDirectBills(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT x.id,x.enterprise_pk,x.gross_cny::text FROM sep_executions x JOIN sep_enterprises e ON e.id=x.enterprise_pk LEFT JOIN sep_settlements t ON t.execution_pk=x.id WHERE x.billing_origin='compute' AND x.fee_status='final' AND t.execution_pk IS NULL AND e.available_cny>=x.gross_cny ORDER BY x.created_at,x.id LIMIT 100`)
	if err != nil {
		return err
	}
	type bill struct {
		id    string
		pk    int64
		gross string
	}
	var bills []bill
	for rows.Next() {
		var b bill
		if err = rows.Scan(&b.id, &b.pk, &b.gross); err != nil {
			rows.Close()
			return err
		}
		bills = append(bills, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range bills {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		var available, frozen string
		var seq int64
		err = tx.QueryRowContext(ctx, `SELECT available_cny::text,frozen_cny::text,sequence FROM sep_enterprises WHERE id=$1 FOR UPDATE`, b.pk).Scan(&available, &frozen, &seq)
		var exists bool
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_settlements WHERE execution_pk=$1)`, b.id).Scan(&exists)
		}
		if err == nil && !exists {
			err = settleSEPDirect(ctx, tx, b.id, b.pk, available, frozen, seq, b.gross)
		}
		if err == nil {
			err = tx.Commit()
		}
		tx.Rollback()
		if err != nil {
			return err
		}
	}
	return nil
}
