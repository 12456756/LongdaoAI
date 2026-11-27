package service

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

func (s *SEPService) rememberVideoBillingProblem(ctx context.Context, id, state string, problem error) error {
	if state == "success" || state == "succeeded" {
		state = "completed"
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(saveCtx, `UPDATE sep_executions SET execution_status=CASE WHEN $2='' THEN execution_status ELSE $2 END,fee_status='reconciling',billing_error=$3,updated_at=NOW(),resource_version=resource_version+1 WHERE id=$1 AND fee_status<>'final'`, id, state, safeVideoError(problem))
	return err
}

// This reads the customer's committed local fee, never a provider's billing
// object. In particular a late task failure must not rewrite an existing bill.
func (s *SEPService) videoTaskBilling(ctx context.Context, key *APIKey, taskID string) VideoBillingStatus {
	var fee, problem string
	var usd sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT fee_status,gross_usd::text,billing_error FROM sep_executions WHERE enterprise_pk=$1 AND api_key_id=$2 AND kind='video' AND task_id=$3`, key.SEP.EnterprisePK, key.ID, taskID).Scan(&fee, &usd, &problem)
	if errors.Is(err, sql.ErrNoRows) {
		return billingProblem(errors.New("Original video execution billing record was not found for this API key"))
	}
	if err != nil {
		return billingProblem(err)
	}
	if fee == "final" {
		amount, err := decimal.NewFromString(usd.String)
		if !usd.Valid || err != nil || amount.IsNegative() {
			return billingProblem(errors.New("Final video bill has an invalid or missing USD amount"))
		}
		status := "settled"
		if amount.IsZero() {
			status = "not_charged"
		}
		billing := VideoBillingStatus{Status: status, ActualCostUSD: &usd.String}
		if problem != "" {
			billing.Error = billingProblem(errors.New(problem)).Error
		}
		return billing
	}
	if problem != "" {
		return billingProblem(errors.New(problem))
	}
	if fee == "reconciling" {
		return billingProblem(errors.New("Original video fee is still reconciling; no final customer bill has been committed"))
	}
	return VideoBillingStatus{Status: "pending"}
}
