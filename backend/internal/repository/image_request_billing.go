package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *usageBillingRepository) RegisterImageBilling(ctx context.Context, b *service.ImageBillingOutcome) error {
	if b == nil || b.APIKeyID <= 0 || b.UserID <= 0 || b.RequestID == "" {
		return errors.New("Invalid image billing identity")
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO image_request_billing(api_key_id,request_id,user_id,fingerprint,model,image_size)
SELECT $1::bigint,$2::varchar(320),$3::bigint,$4::varchar(64),$5::text,$6::varchar(32)
WHERE NOT EXISTS (SELECT 1 FROM usage_billing_dedup WHERE api_key_id=$1 AND request_id=$2)
AND NOT EXISTS (SELECT 1 FROM usage_billing_dedup_archive WHERE api_key_id=$1 AND request_id=$2)
ON CONFLICT (api_key_id,request_id) DO NOTHING`, b.APIKeyID, b.RequestID, b.UserID, b.Fingerprint, b.Model, b.Size)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrUsageBillingRequestConflict
	}
	return nil
}

func (r *usageBillingRepository) FindImageBillingOutcome(ctx context.Context, keyID, userID int64, id string) (*service.ImageBillingOutcome, error) {
	b := &service.ImageBillingOutcome{APIKeyID: keyID, UserID: userID, RequestID: id}
	var amount sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT fingerprint,model,image_size,status,actual_cost::text FROM image_request_billing WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3`, keyID, userID, id).Scan(&b.Fingerprint, &b.Model, &b.Size, &b.Status, &amount)
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

func (r *usageBillingRepository) CloseImageBillingWithoutCharge(ctx context.Context, keyID, userID int64, id string) (*service.ImageBillingOutcome, error) {
	_, err := r.db.ExecContext(ctx, `UPDATE image_request_billing SET status='not_charged',actual_cost=0,updated_at=NOW() WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3 AND status='pending'`, keyID, userID, id)
	if err != nil {
		return nil, err
	}
	row, err := r.FindImageBillingOutcome(ctx, keyID, userID, id)
	if err == nil && row == nil {
		return nil, errors.New("Image billing record is missing")
	}
	return row, err
}
