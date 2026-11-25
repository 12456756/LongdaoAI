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

func TestDeferredImageBillingCaptureAndEvidenceAtomic(t *testing.T) {
	for _, scenario := range []string{"capture", "settled", "not_charged", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			state := scenario
			if state == "capture" || state == "rollback" {
				state = "pending"
			}
			mock.ExpectQuery(`SELECT status FROM image_request_billing .* FOR UPDATE`).WithArgs(int64(7), int64(11), "client:image-r1").WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(state))
			if state == "pending" {
				mock.ExpectQuery(`INSERT INTO usage_billing_dedup`).WithArgs("client:image-r1", int64(7), "fingerprint").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
				mock.ExpectQuery(`SELECT request_fingerprint\s+FROM usage_billing_dedup_archive`).WithArgs("client:image-r1", int64(7)).WillReturnError(sql.ErrNoRows)
				mock.ExpectQuery(`UPDATE users\s+SET balance = balance - \$1`).WithArgs(0.12, int64(11)).WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(9.88))
				update := mock.ExpectExec(`UPDATE image_request_billing SET status='settled'`).WithArgs(int64(7), int64(11), "client:image-r1", 0.12)
				if scenario == "rollback" {
					update.WillReturnError(errors.New("financial evidence unavailable"))
					mock.ExpectRollback()
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectCommit()
				}
			} else {
				mock.ExpectRollback()
			}
			result, err := (&usageBillingRepository{db: db}).Apply(context.Background(), &service.UsageBillingCommand{DeferredImage: true, RequestID: "client:image-r1", APIKeyID: 7, UserID: 11, AccountID: 5, ImageCount: 1, BalanceCost: 0.12, RequestFingerprint: "fingerprint"})
			if scenario == "capture" {
				require.NoError(t, err)
				require.True(t, result.Applied)
			} else if scenario == "settled" {
				require.NoError(t, err)
				require.False(t, result.Applied)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
