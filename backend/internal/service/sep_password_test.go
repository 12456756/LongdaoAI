package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func passwordRequest(t *testing.T, version int64) SEPPasswordUpdate {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	return SEPPasswordUpdate{ContractVersion: 1, EnterpriseID: "ent-test", ComputeAccountID: 1, OperationID: "pw-test", PasswordVersion: version, PasswordHash: string(hash)}
}

func TestSEPPasswordValidation(t *testing.T) {
	svc := &SEPService{}
	r := passwordRequest(t, 1)
	r.PasswordHash = "plaintext"
	_, err := svc.UpdatePassword(t.Context(), "integration", "ent-test", r)
	require.ErrorContains(t, err, "INVALID_PASSWORD_HASH")
	r = passwordRequest(t, 0)
	_, err = svc.UpdatePassword(t.Context(), "integration", "ent-test", r)
	require.ErrorContains(t, err, "INVALID_PASSWORD_UPDATE")
}

func TestSEPPasswordAccountIsolation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	r := passwordRequest(t, 1)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT user_id,password_sync_version").WithArgs("integration", "ent-test").WillReturnRows(sqlmock.NewRows([]string{"user", "version", "ref", "hash"}).AddRow(2, 0, "", ""))
	mock.ExpectRollback()
	_, err = (&SEPService{db: db}).UpdatePassword(t.Context(), "integration", "ent-test", r)
	require.ErrorContains(t, err, "COMPUTE_ACCOUNT_MISMATCH")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSEPPasswordReplayAndOrdering(t *testing.T) {
	for _, test := range []struct {
		name              string
		version           int64
		ref, hash, status string
		failure           bool
	}{
		{"replay", 2, "pw-test", "same", "applied", false},
		{"stale", 3, "newer", "different", "superseded", false},
		{"conflict", 2, "pw-test", "different", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			r := passwordRequest(t, 2)
			hash := test.hash
			if hash == "same" {
				hash = sepHash(sepJSON(r))
			}
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT user_id,password_sync_version").WillReturnRows(sqlmock.NewRows([]string{"user", "version", "ref", "hash"}).AddRow(1, test.version, test.ref, hash))
			mock.ExpectRollback()
			v, err := (&SEPService{db: db}).UpdatePassword(t.Context(), "integration", "ent-test", r)
			if test.failure {
				require.ErrorContains(t, err, "IDEMPOTENCY_CONFLICT")
			} else {
				require.NoError(t, err)
				require.Equal(t, test.status, v.Status)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSEPPasswordAtomicUpdate(t *testing.T) {
	for _, fail := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		r := passwordRequest(t, 1)
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT user_id,password_sync_version").WithArgs("integration", "ent-test").WillReturnRows(sqlmock.NewRows([]string{"user", "version", "ref", "hash"}).AddRow(1, 0, "", ""))
		mock.ExpectExec("UPDATE users SET password_hash").WithArgs(r.PasswordHash, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
		update := mock.ExpectExec("UPDATE sep_enterprises SET password_sync_version").WithArgs(int64(1), r.OperationID, sepHash(sepJSON(r)), "integration", "ent-test")
		if fail {
			update.WillReturnError(errors.New("storage failure"))
			mock.ExpectRollback()
		} else {
			update.WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
		}
		v, err := (&SEPService{db: db}).UpdatePassword(t.Context(), "integration", "ent-test", r)
		if fail {
			require.ErrorContains(t, err, "storage failure")
		} else {
			require.NoError(t, err)
			require.Equal(t, int64(1), v.AppliedVersion)
			require.Equal(t, "applied", v.Status)
		}
		require.NoError(t, mock.ExpectationsWereMet())
		mock.ExpectClose()
		require.NoError(t, db.Close())
	}
}

func TestSEPPasswordInvalidatesLoginTokenVersion(t *testing.T) {
	first := &User{Email: "owner@example.test", PasswordHash: passwordRequest(t, 1).PasswordHash}
	second := &User{Email: first.Email, PasswordHash: passwordRequest(t, 2).PasswordHash}
	require.NotEqual(t, resolvedTokenVersion(first), resolvedTokenVersion(second))
}

func TestSEPPasswordConcurrentPersistence(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	_, err := db.Exec(`ALTER TABLE users ADD COLUMN status TEXT DEFAULT 'active', ADD COLUMN updated_at TIMESTAMPTZ DEFAULT NOW()`)
	require.NoError(t, err)
	for _, name := range []string{"191_sep_login_accounts.sql", "194_sep_password_sync.sql"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(raw))
		require.NoError(t, err)
	}
	r := passwordRequest(t, 1)
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.UpdatePassword(t.Context(), "sep-test", "ent-test", r)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var stored string
	require.NoError(t, db.QueryRow(`SELECT password_hash FROM users WHERE id=1`).Scan(&stored))
	require.Equal(t, r.PasswordHash, stored)
	newer := passwordRequest(t, 2)
	newer.OperationID = "pw-newer"
	_, err = svc.UpdatePassword(t.Context(), "sep-test", "ent-test", newer)
	require.NoError(t, err)
	stale, err := svc.UpdatePassword(t.Context(), "sep-test", "ent-test", r)
	require.NoError(t, err)
	require.Equal(t, "superseded", stale.Status)
	require.NoError(t, db.QueryRow(`SELECT password_hash FROM users WHERE id=1`).Scan(&stored))
	require.Equal(t, newer.PasswordHash, stored)
	encoded, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "password_hash")
}
