package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.VideoBillingOutcomeRepository = (*usageBillingRepository)(nil)

func (r *usageBillingRepository) RegisterVideoBilling(ctx context.Context, b *service.VideoBillingOutcome) error {
	if b == nil || b.APIKeyID <= 0 || b.UserID <= 0 || b.RequestID == "" {
		return errors.New("Invalid video billing identity")
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO video_request_billing (api_key_id,request_id,user_id,fingerprint,model,resolution,duration_seconds)
SELECT $1::bigint,$2::varchar(320),$3::bigint,$4::varchar(64),$5::text,$6::varchar(10),$7::integer WHERE NOT EXISTS (SELECT 1 FROM usage_billing_dedup WHERE api_key_id=$1 AND request_id=$2)
AND NOT EXISTS (SELECT 1 FROM usage_billing_dedup_archive WHERE api_key_id=$1 AND request_id=$2)
ON CONFLICT (api_key_id,request_id) DO NOTHING`, b.APIKeyID, b.RequestID, b.UserID, b.Fingerprint, b.Model, b.Resolution, b.Seconds)
	if err != nil {
		return err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if created != 1 {
		return service.ErrUsageBillingRequestConflict
	}
	row, err := r.FindVideoBillingOutcome(ctx, b.APIKeyID, b.UserID, b.RequestID)
	if err != nil {
		return err
	}
	if row == nil || row.Fingerprint != b.Fingerprint {
		return service.ErrUsageBillingRequestConflict
	}
	if row.Status != "pending" {
		return errors.New("Video request already has a final billing outcome")
	}
	return nil
}

func (r *usageBillingRepository) FindVideoBillingOutcome(ctx context.Context, keyID, userID int64, id string) (*service.VideoBillingOutcome, error) {
	b := &service.VideoBillingOutcome{APIKeyID: keyID, UserID: userID, RequestID: id}
	var amount sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT fingerprint,model,resolution,duration_seconds,status,actual_cost::text FROM video_request_billing WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3`, keyID, userID, id).Scan(&b.Fingerprint, &b.Model, &b.Resolution, &b.Seconds, &b.Status, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if amount.Valid {
		b.ActualCostUSD = &amount.String
	}
	return b, nil
}

func (r *usageBillingRepository) CloseVideoBillingWithoutCharge(ctx context.Context, keyID, userID int64, id string) (*service.VideoBillingOutcome, error) {
	// This update and successful capture serialize on the same financial row.
	_, err := r.db.ExecContext(ctx, `UPDATE video_request_billing SET status='not_charged',actual_cost=0,updated_at=NOW() WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3 AND status='pending'`, keyID, userID, id)
	if err != nil {
		return nil, err
	}
	row, err := r.FindVideoBillingOutcome(ctx, keyID, userID, id)
	if err == nil && row == nil {
		err = errors.New("Video billing record is missing")
	}
	return row, err
}
