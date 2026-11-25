package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDeferredVideoBillingCaptureIsAtomic(t *testing.T) {
	for _, scenario := range []string{"capture", "settled", "not_charged", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			cmd := &service.UsageBillingCommand{DeferredVideo: true, RequestID: "client:original", APIKeyID: 7, UserID: 11, AccountID: 5, Model: "seedance-2.0", BalanceCost: 0.12, RequestFingerprint: "fingerprint"}
			mock.ExpectBegin()
			state := scenario
			if state == "capture" || state == "rollback" {
				state = "pending"
			}
			mock.ExpectQuery(`SELECT status FROM video_request_billing .* FOR UPDATE`).WithArgs(int64(7), int64(11), "client:original").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(state))
			if state == "pending" {
				mock.ExpectQuery(`INSERT INTO usage_billing_dedup`).WithArgs("client:original", int64(7), "fingerprint").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
				mock.ExpectQuery(`SELECT request_fingerprint\s+FROM usage_billing_dedup_archive`).WithArgs("client:original", int64(7)).WillReturnError(sql.ErrNoRows)
				mock.ExpectQuery(`UPDATE users\s+SET balance = balance - \$1`).WithArgs(0.12, int64(11)).WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(9.88))
				update := mock.ExpectExec(`UPDATE video_request_billing SET status='settled'`).WithArgs(int64(7), int64(11), "client:original", 0.12)
				if scenario == "rollback" {
					update.WillReturnError(errors.New("financial state update failed"))
					mock.ExpectRollback()
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectCommit()
				}
			} else {
				mock.ExpectRollback()
			}
			result, err := (&usageBillingRepository{db: db}).Apply(context.Background(), cmd)
			switch scenario {
			case "capture":
				require.NoError(t, err)
				require.True(t, result.Applied)
			case "settled":
				require.NoError(t, err)
				require.False(t, result.Applied)
			default:
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestVideoBillingCloseKeepsSettledEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec(`UPDATE video_request_billing .* AND status='pending'`).WithArgs(int64(7), int64(11), "client:original").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT fingerprint,model,resolution,duration_seconds,status,actual_cost::text FROM video_request_billing WHERE api_key_id=\$1 AND user_id=\$2 AND request_id=\$3`).WithArgs(int64(7), int64(11), "client:original").WillReturnRows(sqlmock.NewRows([]string{"fingerprint", "model", "resolution", "duration_seconds", "status", "actual_cost"}).AddRow("fingerprint", "seedance-2.0", "720p", 4, "settled", "0.12"))
	row, err := (&usageBillingRepository{db: db}).CloseVideoBillingWithoutCharge(context.Background(), 7, 11, "client:original")
	require.NoError(t, err)
	require.Equal(t, "settled", row.Status)
	require.Equal(t, "0.12", *row.ActualCostUSD)
	require.NoError(t, mock.ExpectationsWereMet())
}
