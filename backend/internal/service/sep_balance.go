package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

type SEPAccountBalance struct {
	EnterpriseID string `json:"enterprise_id"`
	Available    string `json:"available_paid_cny"`
	Frozen       string `json:"frozen_paid_cny"`
	ExchangeRate string `json:"exchange_rate"`
}
type SEPBalanceChange struct {
	UserID       int64  `json:"user_id"`
	BusinessRef  string `json:"business_ref"`
	Operation    string `json:"operation"`
	Amount       string `json:"amount"`
	Currency     string `json:"currency"`
	ExchangeRate string `json:"exchange_rate"`
	Kind         string `json:"kind"`
	Notes        string `json:"notes"`
	ActorID      int64  `json:"actor_id"`
	RedeemCodeID int64  `json:"redeem_code_id,omitempty"`
}
type SEPBalanceReceipt struct {
	BusinessRef string `json:"business_ref"`
	EventID     string `json:"event_id"`
	Sequence    string `json:"sequence"`
	Delta       string `json:"delta_cny"`
	Available   string `json:"available_paid_cny"`
	Committed   bool   `json:"committed"`
	SyncStatus  string `json:"sync_status"`
}
type SEPAccountAccounting interface {
	SEPAccountBalance(context.Context, int64) (*SEPAccountBalance, error)
	AdjustSEPAccountBalance(context.Context, SEPBalanceChange) (*SEPBalanceReceipt, error)
	SEPRefundBalance(context.Context, int64, string, string) (bool, error)
}
type sepBalanceContextKey struct{}

func WithSEPBalanceOperation(ctx context.Context, ref string, actor int64) context.Context {
	return context.WithValue(ctx, sepBalanceContextKey{}, struct {
		Ref   string
		Actor int64
	}{ref, actor})
}
func (s *SEPService) AccountBalance(ctx context.Context, userID int64) (*SEPAccountBalance, error) {
	balances, err := s.AccountBalances(ctx, []int64{userID})
	return balances[userID], err
}
func (s *SEPService) AccountBalances(ctx context.Context, ids []int64) (map[int64]*SEPAccountBalance, error) {
	result := map[int64]*SEPAccountBalance{}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,enterprise_id,available_cny::text,frozen_cny::text FROM sep_enterprises WHERE user_id=ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var b SEPAccountBalance
		if err = rows.Scan(&id, &b.EnterpriseID, &b.Available, &b.Frozen); err != nil {
			rows.Close()
			return nil, err
		}
		result[id] = &b
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		rate, err := s.settings.GetPlatformExchangeRate(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range result {
			b.ExchangeRate = rate
		}
	}
	return result, nil
}
func (s *SEPService) AdjustAccountBalance(ctx context.Context, r SEPBalanceChange) (receipt *SEPBalanceReceipt, resultErr error) {
	defer func() {
		if resultErr != nil {
			se, detail := NormalizeSEPError(resultErr, "balance")
			if detail != "" && !strings.Contains(se.Message, detail) {
				se.Message += "：" + detail
			}
			se.BusinessRef = r.BusinessRef
			se.AccountingStatus = "not_committed"
			if se.Code == "BALANCE_COMMIT_UNCONFIRMED" {
				se.AccountingStatus = "unknown"
			}
			resultErr = se
		}
	}()
	if v, ok := ctx.Value(sepBalanceContextKey{}).(struct {
		Ref   string
		Actor int64
	}); ok {
		if r.BusinessRef == "" {
			r.BusinessRef = v.Ref
		}
		r.ActorID = v.Actor
	}
	if r.BusinessRef == "" {
		return nil, SEPFailure(400, "BALANCE_OPERATION_ID_REQUIRED", "余额操作必须提供唯一业务编号；请使用原编号重试", "balance")
	}
	if len(r.BusinessRef) > 200 {
		return nil, SEPFailure(400, "INVALID_BUSINESS_ID", "余额业务编号最多 200 个字符", "balance")
	}
	amount, err := decimal.NewFromString(r.Amount)
	if len(r.Amount) > 40 || err != nil || amount.IsNegative() || amount.Exponent() < -12 {
		return nil, SEPFailure(400, "INVALID_BALANCE_AMOUNT", "余额金额必须是非负十进制数", "balance")
	}
	if r.ExchangeRate == "" {
		r.ExchangeRate, err = s.settings.GetPlatformExchangeRate(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err = ValidatePlatformExchangeRate(r.ExchangeRate); err != nil {
		return nil, err
	}
	if r.Currency == "USD" {
		rate, _ := decimal.NewFromString(r.ExchangeRate)
		amount = amount.Mul(rate)
	} else if r.Currency != "CNY" {
		return nil, SEPFailure(400, "INVALID_BALANCE_CURRENCY", "SEP 企业余额操作仅支持 CNY 或 USD", "balance")
	}
	amount = amount.Round(6)
	if _, err = ValidateSEPAmount(amount.StringFixed(6), false); err != nil {
		return nil, err
	}
	if len(r.Notes) > 2000 {
		return nil, SEPFailure(400, "INVALID_BALANCE_NOTES", "余额操作备注最多 2000 字节", "balance")
	}
	// Exchange rate is execution metadata; a retry after a rate change returns the original receipt.
	fingerprint := r
	fingerprint.ExchangeRate = ""
	hash := sepHash(sepJSON(fingerprint))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pk, seq int64
	var available, frozen, status string
	err = tx.QueryRowContext(ctx, `SELECT id,sequence,available_cny::text,frozen_cny::text,status FROM sep_enterprises WHERE user_id=$1 FOR UPDATE`, r.UserID).Scan(&pk, &seq, &available, &frozen, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var previousHash string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT request_hash,receipt FROM sep_balance_operations WHERE user_id=$1 AND business_ref=$2`, r.UserID, r.BusinessRef).Scan(&previousHash, &raw)
	if err == nil {
		if previousHash != hash {
			return nil, sepConflict("BALANCE_OPERATION_CONFLICT", fmt.Sprintf("余额业务编号 %s 已使用，本次金额、方向或备注与原记录不一致；未执行新操作", r.BusinessRef))
		}
		var receipt SEPBalanceReceipt
		err = json.Unmarshal(raw, &receipt)
		return &receipt, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	current, _ := decimal.NewFromString(available)
	next := current
	switch r.Operation {
	case "add":
		next = current.Add(amount)
	case "subtract":
		next = current.Sub(amount)
	case "set":
		next = amount
	default:
		return nil, SEPFailure(400, "INVALID_BALANCE_OPERATION", "余额操作必须为 add、subtract 或 set", "balance")
	}
	if next.IsNegative() {
		return nil, SEPFailure(409, "INSUFFICIENT_PAID_FUNDS", fmt.Sprintf("扣减失败：企业可用余额 %s 元，本次需扣 %s 元；未扣款", available, amount.StringFixed(6)), "balance")
	}
	if status != "ready" && next.GreaterThan(current) && r.Kind != "payment" {
		return nil, SEPFailure(409, "ENTERPRISE_DISABLED", "企业已停用，无法增加余额；未入账", "balance")
	}
	delta := next.Sub(current)
	receipt = &SEPBalanceReceipt{BusinessRef: r.BusinessRef, Sequence: fmt.Sprint(seq), Delta: delta.StringFixed(6), Available: next.StringFixed(6), Committed: true, SyncStatus: "not_required"}
	if r.RedeemCodeID > 0 {
		result, err := tx.ExecContext(ctx, `UPDATE redeem_codes SET status='used',used_by=$1,used_at=NOW() WHERE id=$2 AND status='unused' AND type='balance' AND (expires_at IS NULL OR expires_at>NOW())`, r.UserID, r.RedeemCodeID)
		if err != nil {
			return nil, err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return nil, ErrRedeemCodeUsed
		}
	}
	if !delta.IsZero() {
		seq++
		receipt.EventID = sepRandom("ledger_")
		receipt.Sequence = fmt.Sprint(seq)
		receipt.SyncStatus = "pending"
		kind := "compute_credit"
		if delta.IsNegative() {
			kind = "compute_debit"
			if r.Kind == "refund" {
				kind = "compute_refund"
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET available_cny=$1,sequence=$2 WHERE id=$3`, next.StringFixed(6), seq, pk)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sep_ledger(id,enterprise_pk,sequence,type,amount_cny,available_cny,frozen_cny,origin,business_ref,notes,actor_id,exchange_rate) VALUES($1,$2,$3,$4,$5,$6,$7,'compute',$8,$9,$10,$11)`, receipt.EventID, pk, seq, kind, delta.Abs().StringFixed(6), next.StringFixed(6), frozen, r.BusinessRef, r.Notes, r.ActorID, r.ExchangeRate)
		if err != nil {
			return nil, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_balance_operations(user_id,business_ref,request_hash,receipt) VALUES($1,$2,$3,$4)`, r.UserID, r.BusinessRef, hash, string(sepJSON(receipt)))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, SEPFailure(503, "BALANCE_COMMIT_UNCONFIRMED", fmt.Sprintf("余额提交结果未确认：%s；请查询原业务编号 %s，勿换编号重试", safeSEPDiagnostic(err), r.BusinessRef), "commit")
	}
	if s.keys != nil {
		s.keys.InvalidateAuthCacheByUserID(ctx, r.UserID)
	}
	return receipt, nil
}
func safeSEPDiagnostic(err error) string {
	_, detail := NormalizeSEPError(err, "balance")
	return strings.TrimSpace(detail)
}
