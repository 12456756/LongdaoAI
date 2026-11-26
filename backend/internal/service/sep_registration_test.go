package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestSEPRegistrationPostgresLoginIdentity(t *testing.T) {
	dsn := os.Getenv("TEST_SEP_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set TEST_SEP_DATABASE_DSN to an isolated PostgreSQL test database")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(t.Context(), `
CREATE TEMP TABLE users (
    id BIGSERIAL PRIMARY KEY,email TEXT UNIQUE,password_hash TEXT,role TEXT,
    balance NUMERIC,frozen_balance NUMERIC,concurrency INT,status TEXT,username TEXT
);
CREATE TEMP TABLE groups (id BIGINT PRIMARY KEY,status TEXT,deleted_at TIMESTAMPTZ,subscription_type TEXT);
INSERT INTO groups VALUES (1,'active',NULL,'standard'),(2,'active',NULL,'standard'),(3,'active',NULL,'standard');
CREATE TEMP TABLE sep_enterprises (
    id BIGSERIAL PRIMARY KEY,integration_id TEXT,enterprise_id TEXT,registration_ref TEXT,
    registration_hash TEXT,user_id BIGINT UNIQUE,company_name TEXT,admin_email TEXT,status TEXT DEFAULT 'ready',
    UNIQUE(integration_id,enterprise_id),UNIQUE(integration_id,registration_ref)
);
CREATE TEMP TABLE api_keys (id BIGSERIAL PRIMARY KEY,user_id BIGINT,key TEXT,name TEXT,group_id BIGINT,status TEXT);
CREATE TEMP TABLE user_allowed_groups (user_id BIGINT,group_id BIGINT,UNIQUE(user_id,group_id));
CREATE TEMP TABLE sep_credentials (
    id TEXT PRIMARY KEY,enterprise_pk BIGINT,kind TEXT,api_key_id BIGINT UNIQUE,version INT DEFAULT 1,
    encryption_key_id TEXT,masked_key TEXT,encrypted_key TEXT,is_current BOOLEAN DEFAULT TRUE,UNIQUE(enterprise_pk,kind)
);
`)
	require.NoError(t, err)
	private, err := rsa.GenerateKey(rand.Reader, 3072)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	require.NoError(t, err)
	cfg := &SEPConfig{IntegrationID: "sep-registration-test", EncryptionKeyID: "enc-test",
		EncryptionPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		ChatGroupID:         1, ImageGroupID: 2, VideoGroupID: 3}
	hash, err := bcrypt.GenerateFromPassword([]byte("RegistrationFixture2026!"), bcrypt.MinCost)
	require.NoError(t, err)
	request := SEPRegistration{EnterpriseID: "enterprise-test", RegistrationRef: "register-test",
		CompanyName: "Fixture company", AdminEmail: "registration@example.test", EncryptionKeyID: cfg.EncryptionKeyID,
		PasswordHash: string(hash)}
	svc := &SEPService{db: db}
	created, err := svc.Register(t.Context(), cfg, request)
	require.NoError(t, err)
	require.Len(t, created.Credentials, 3)
	var user User
	err = db.QueryRowContext(t.Context(), `SELECT id,email,password_hash,role FROM users WHERE id=$1`, created.ComputeAccountID).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role)
	require.NoError(t, err)
	require.Equal(t, request.AdminEmail, user.Email)
	require.Equal(t, request.PasswordHash, user.PasswordHash)
	require.Equal(t, RoleUser, user.Role)
	require.True(t, user.CheckPassword("RegistrationFixture2026!"))
	require.False(t, user.CheckPassword("wrong-password"))
	replay, err := svc.Register(t.Context(), cfg, request)
	require.NoError(t, err)
	require.Equal(t, created, replay)
	conflict := request
	conflict.EnterpriseID, conflict.RegistrationRef = "other-enterprise", "other-registration"
	_, err = svc.Register(t.Context(), cfg, conflict)
	var failure *SEPError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "EMAIL_ALREADY_EXISTS", failure.Code)
	var users, keys int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM users`).Scan(&users))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM api_keys`).Scan(&keys))
	require.Equal(t, 1, users)
	require.Equal(t, 3, keys)
	request.RegistrationRef, request.EnterpriseID, request.AdminEmail = "missing-hash", "missing-hash", "missing@example.test"
	request.PasswordHash = ""
	_, err = svc.Register(t.Context(), cfg, request)
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "INVALID_PASSWORD_HASH", failure.Code)
}
