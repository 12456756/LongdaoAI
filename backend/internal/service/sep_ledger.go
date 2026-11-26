package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

func sepLock(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
	return err
}
func readSEPReceipt(ctx context.Context, tx *sql.Tx, table, integration, id, hash string) (map[string]any, error) {
	column := "transfer_id"
	if table == "sep_settlements" {
		column = "settlement_id"
	}
	var previous string
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash,receipt FROM `+table+` WHERE integration_id=$1 AND `+column+`=$2`, integration, id).Scan(&previous, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if previous != hash {
		return nil, sepConflict("IDEMPOTENCY_CONFLICT", "同一业务编号的请求内容不一致")
	}
	var receipt map[string]any
	err = json.Unmarshal(data, &receipt)
	return receipt, err
}
func (s *SEPService) Transfer(ctx context.Context, integration string, r SEPTransfer) (map[string]any, error) {
	for _, id := range []string{r.EnterpriseID, r.TransferID, r.FundingRef} {
		if err := sepID(id); err != nil {
			return nil, err
		}
	}
	if r.ContractVersion != 1 || r.Currency != "CNY" || (r.Direction != "credit_compute" && r.Direction != "debit_compute") {
		return nil, SEPFailure(400, "INVALID_TRANSFER", "划转版本、币种或方向无效", "transfer")
	}
	amount, err := ValidateSEPAmount(r.AmountCNY, true)
	if err != nil {
		return nil, err
	}
	hash := sepHash(sepJSON(r))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = sepLock(ctx, tx, "sep-transfer:"+integration+":"+r.TransferID); err != nil {
		return nil, err
	}
	previous, err := readSEPReceipt(ctx, tx, "sep_transfers", integration, r.TransferID, hash)
	if err != nil || previous != nil {
		return previous, err
	}
	var pk, seq int64
	var available, frozen, status string
	err = tx.QueryRowContext(ctx, `SELECT id,available_cny::text,frozen_cny::text,sequence,status FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2 FOR UPDATE`, integration, r.EnterpriseID).Scan(&pk, &available, &frozen, &seq, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	current, _ := decimal.NewFromString(available)
	receipt := map[string]any{"contract_version": 1, "enterprise_id": r.EnterpriseID, "transfer_id": r.TransferID, "direction": r.Direction, "currency": "CNY", "amount_cny": r.AmountCNY, "funding_ref": r.FundingRef, "status": "committed", "committed_at": time.Now().UTC()}
	reason := ""
	if status != "ready" && r.Direction == "credit_compute" {
		reason = "ENTERPRISE_DISABLED"
	}
	if r.Direction == "debit_compute" && current.LessThan(amount) {
		reason = "INSUFFICIENT_PAID_FUNDS"
	}
	if reason != "" {
		receipt["status"] = "rejected"
		receipt["reason"] = reason
		receipt["sequence"] = strconv.FormatInt(seq, 10)
	} else {
		if r.Direction == "credit_compute" {
			current = current.Add(amount)
		} else {
			current = current.Sub(amount)
		}
		seq++
		if _, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET available_cny=$1,sequence=$2 WHERE id=$3`, current.StringFixed(6), seq, pk); err != nil {
			return nil, err
		}
		ledgerID := sepRandom("ledger_")
		_, err = tx.ExecContext(ctx, `INSERT INTO sep_ledger(id,enterprise_pk,sequence,type,amount_cny,available_cny,frozen_cny,transfer_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, ledgerID, pk, seq, r.Direction, r.AmountCNY, current.StringFixed(6), frozen, r.TransferID)
		if err != nil {
			return nil, err
		}
		receipt["ledger_entry_id"] = ledgerID
		receipt["sequence"] = strconv.FormatInt(seq, 10)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_transfers(integration_id,transfer_id,enterprise_pk,request_hash,receipt) VALUES($1,$2,$3,$4,$5)`, integration, r.TransferID, pk, hash, string(sepJSON(receipt)))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}
func (s *SEPService) TransferResult(ctx context.Context, integration, id string) (map[string]any, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT receipt FROM sep_transfers WHERE integration_id=$1 AND transfer_id=$2`, integration, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	var v map[string]any
	err = json.Unmarshal(data, &v)
	return v, err
}
func (s *SEPService) Settle(ctx context.Context, integration, executionID string, r SEPSettlement) (map[string]any, error) {
	for _, id := range []string{r.EnterpriseID, r.SettlementID, r.BillID, executionID} {
		if err := sepID(id); err != nil {
			return nil, err
		}
	}
	if r.ContractVersion != 1 || r.Currency != "CNY" || r.BillVersion != 1 {
		return nil, SEPFailure(400, "INVALID_SETTLEMENT", "结算协议、币种或费用版本无效", "settle")
	}
	gross, err := ValidateSEPAmount(r.GrossCNY, false)
	if err != nil {
		return nil, err
	}
	free, err := ValidateSEPAmount(r.FreeCNY, false)
	if err != nil {
		return nil, err
	}
	paid, err := ValidateSEPAmount(r.PaidCNY, false)
	if err != nil {
		return nil, err
	}
	if !gross.Equal(free.Add(paid)) {
		return nil, sepConflict("SETTLEMENT_MISMATCH", "总费用必须等于免费金额与付费金额之和")
	}
	hash := sepHash(sepJSON(struct {
		ExecutionID string
		Decision    SEPSettlement
	}{executionID, r}))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = sepLock(ctx, tx, "sep-settlement:"+integration+":"+r.SettlementID); err != nil {
		return nil, err
	}
	previous, err := readSEPReceipt(ctx, tx, "sep_settlements", integration, r.SettlementID, hash)
	if err != nil || previous != nil {
		return previous, err
	}
	var reservedHash string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM sep_pending_settlements WHERE integration_id=$1 AND settlement_id=$2`, integration, r.SettlementID).Scan(&reservedHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && reservedHash != hash {
		return nil, sepConflict("IDEMPOTENCY_CONFLICT", "结算编号已有待结算原单，不能更改执行或金额")
	}
	var pk, seq int64
	var available, frozen string
	err = tx.QueryRowContext(ctx, `SELECT id,available_cny::text,frozen_cny::text,sequence FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2 FOR UPDATE`, integration, r.EnterpriseID).Scan(&pk, &available, &frozen, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	var eid, feeStatus, storedBill, storedGross, origin string
	var version int
	err = tx.QueryRowContext(ctx, `SELECT id,fee_status,COALESCE(bill_id,''),COALESCE(gross_cny::text,''),COALESCE(bill_version,0),billing_origin FROM sep_executions WHERE integration_id=$1 AND enterprise_pk=$2 AND execution_id=$3 FOR UPDATE`, integration, pk, executionID).Scan(&eid, &feeStatus, &storedBill, &storedGross, &version, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	if feeStatus != "final" {
		return nil, sepConflict("BILL_NOT_FINAL", "实际费用尚未确认，请查询原执行记录")
	}
	if origin == "compute" {
		return nil, sepConflict("DIRECT_EXECUTION_ALREADY_BILLED", "该执行由算力平台直接计费，SEP 只能导入同步事件，不能再次结算扣款")
	}
	if storedBill != r.BillID || version != r.BillVersion || storedGross != r.GrossCNY {
		return nil, sepConflict("SETTLEMENT_MISMATCH", "结算决定与平台最终费用单不一致")
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_settlements WHERE execution_pk=$1)`, eid).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, sepConflict("EXECUTION_ALREADY_SETTLED", "该执行已完成结算，不能再次扣款")
	}
	var pendingHash string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM sep_pending_settlements WHERE execution_pk=$1`, eid).Scan(&pendingHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && pendingHash != hash {
		return nil, sepConflict("IDEMPOTENCY_CONFLICT", "该执行已有待结算决定，请重试原单")
	}
	current, _ := decimal.NewFromString(available)
	var older bool
	if paid.IsPositive() {
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_pending_settlements p WHERE p.enterprise_pk=$1 AND p.execution_pk<>$2
		AND NOT EXISTS(SELECT 1 FROM sep_pending_settlements own WHERE own.execution_pk=$2 AND (own.created_at,own.settlement_id)<(p.created_at,p.settlement_id)))`, pk, eid).Scan(&older)
		if err != nil {
			return nil, err
		}
	}
	if current.LessThan(paid) || older {
		if err = rememberSEPPendingSettlement(ctx, tx, integration, executionID, eid, pk, hash, r, current); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		failure := SEPFailure(409, "INSUFFICIENT_PAID_FUNDS", fmt.Sprintf("原费用单已保存待结算：费用 %s 元，当前余额 %s 元；余额不足或需优先结清更早账单，补足后重试原单", r.PaidCNY, available), "settle").(*SEPError)
		failure.AccountingStatus = "not_committed"
		failure.BusinessRef = r.SettlementID
		return nil, failure
	}
	current = current.Sub(paid)
	seq++
	ledgerID := sepRandom("ledger_")
	receipt := map[string]any{"contract_version": 1, "enterprise_id": r.EnterpriseID, "execution_id": executionID, "settlement_id": r.SettlementID, "bill_id": r.BillID, "status": "committed", "currency": "CNY", "charged_paid_cny": r.PaidCNY, "recorded_free_cny": r.FreeCNY, "ledger_entry_id": ledgerID, "sequence": strconv.FormatInt(seq, 10), "committed_at": time.Now().UTC()}
	if _, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET available_cny=$1,sequence=$2 WHERE id=$3`, current.StringFixed(6), seq, pk); err != nil {
		return nil, err
	}
	typeName := "paid_consumption"
	freeAmount := "0.000000"
	if paid.IsZero() {
		typeName = "free_record"
		freeAmount = r.FreeCNY
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_ledger(id,enterprise_pk,sequence,type,amount_cny,free_cny,available_cny,frozen_cny,execution_id,settlement_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, ledgerID, pk, seq, typeName, r.PaidCNY, freeAmount, current.StringFixed(6), frozen, executionID, r.SettlementID)
	if err != nil {
		return nil, err
	}
	if paid.IsPositive() && free.IsPositive() {
		seq++
		freeID := sepRandom("ledger_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO sep_ledger(id,enterprise_pk,sequence,type,amount_cny,free_cny,available_cny,frozen_cny,execution_id,settlement_id) VALUES($1,$2,$3,'free_record',0,$4,$5,$6,$7,$8)`, freeID, pk, seq, r.FreeCNY, current.StringFixed(6), frozen, executionID, r.SettlementID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET sequence=$1 WHERE id=$2`, seq, pk); err != nil {
			return nil, err
		}
		receipt["sequence"] = strconv.FormatInt(seq, 10)
		receipt["free_ledger_entry_id"] = freeID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_settlements(integration_id,settlement_id,execution_pk,request_hash,gross_cny,free_cny,paid_cny,receipt) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, integration, r.SettlementID, eid, hash, r.GrossCNY, r.FreeCNY, r.PaidCNY, string(sepJSON(receipt)))
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sep_executions SET resource_version=resource_version+1,updated_at=NOW() WHERE id=$1`, eid); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sep_pending_settlements WHERE execution_pk=$1`, eid); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sep_executions SET billing_error='' WHERE id=$1`, eid); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}
func (s *SEPService) Ledger(ctx context.Context, integration, id string, after, watermark int64, limit int) (map[string]any, error) {
	if after < 0 || watermark < 0 || limit < 1 || limit > 200 {
		return nil, SEPFailure(400, "INVALID_CURSOR", "流水游标无效，单页最多 200 条", "ledger")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pk, current int64
	var available, frozen string
	err = tx.QueryRowContext(ctx, `SELECT id,sequence,available_cny::text,frozen_cny::text FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2`, integration, id).Scan(&pk, &current, &available, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	if watermark == 0 {
		watermark = current
	}
	if watermark > current || after > watermark {
		return nil, SEPFailure(400, "INVALID_CURSOR", "流水水位超出账户范围", "ledger")
	}
	if watermark == 0 {
		available = "0.000000"
		frozen = "0.000000"
	} else if watermark != current {
		if err = tx.QueryRowContext(ctx, `SELECT available_cny::text,frozen_cny::text FROM sep_ledger WHERE enterprise_pk=$1 AND sequence=$2`, pk, watermark).Scan(&available, &frozen); err != nil {
			return nil, err
		}
	}
	// Qualify the integer column: the unqualified output name is sequence::text.
	rows, err := tx.QueryContext(ctx, `SELECT id,sequence::text,type,amount_cny::text,free_cny::text,transfer_id,execution_id,settlement_id,effective_at FROM sep_ledger WHERE enterprise_pk=$1 AND sequence>$2 AND sequence<=$3 ORDER BY sep_ledger.sequence ASC LIMIT $4`, pk, after, watermark, limit)
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	next := after
	for rows.Next() {
		var lid, seq, kind, amount, free string
		var transfer, execution, settlement sql.NullString
		var at time.Time
		if err = rows.Scan(&lid, &seq, &kind, &amount, &free, &transfer, &execution, &settlement, &at); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, map[string]any{"enterprise_id": id, "ledger_entry_id": lid, "sequence": seq, "type": kind, "currency": "CNY", "amount_cny": amount, "free_cny": free, "transfer_id": transfer.String, "execution_id": execution.String, "settlement_id": settlement.String, "effective_at": at})
		next, _ = strconv.ParseInt(seq, 10, 64)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	a, _ := decimal.NewFromString(available)
	f, _ := decimal.NewFromString(frozen)
	return map[string]any{"enterprise_id": id, "currency": "CNY", "items": items, "after_sequence": strconv.FormatInt(next, 10), "snapshot_sequence": strconv.FormatInt(watermark, 10), "has_more": next < watermark, "available_paid_cny": available, "frozen_paid_cny": frozen, "total_paid_cny": a.Add(f).StringFixed(6)}, nil
}
