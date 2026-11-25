//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Opt in only with a dedicated disposable PostgreSQL instance, never application configuration.
func TestVideoBillingPostgresCaptureCancelRace(t *testing.T) {
	dsn := os.Getenv("LONGDAO_VIDEO_TEST_DSN")
	if dsn == "" {
		t.Skip("LONGDAO_VIDEO_TEST_DSN is not set to a disposable test database")
	}
	endpoint, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"localhost", "127.0.0.1"}, endpoint.Hostname())
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx))
	schema := fmt.Sprintf("video_contract_test_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	defer func() { _, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()
	query := endpoint.Query()
	query.Set("search_path", schema)
	endpoint.RawQuery = query.Encode()
	db, err := sql.Open("postgres", endpoint.String())
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, `CREATE TABLE users (id BIGINT PRIMARY KEY,balance NUMERIC NOT NULL,deleted_at TIMESTAMPTZ,updated_at TIMESTAMPTZ);
CREATE TABLE usage_billing_dedup (id BIGSERIAL PRIMARY KEY,request_id TEXT,api_key_id BIGINT,request_fingerprint TEXT,UNIQUE(request_id,api_key_id));
CREATE TABLE usage_billing_dedup_archive (request_id TEXT,api_key_id BIGINT,request_fingerprint TEXT,UNIQUE(request_id,api_key_id));
INSERT INTO users(id,balance) VALUES (11,100);`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("182_video_request_billing.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	repo := &usageBillingRepository{db: db}
	// First prove a normal paid capture only deducts once under concurrency.
	require.NoError(t, repo.RegisterVideoBilling(ctx, &service.VideoBillingOutcome{APIKeyID: 7, UserID: 11, RequestID: "client:paid", Fingerprint: "paid", Model: "seedance-2.0", Resolution: "720p", Seconds: 4}))
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := repo.Apply(ctx, &service.UsageBillingCommand{DeferredVideo: true, RequestID: "client:paid", APIKeyID: 7, UserID: 11, BalanceCost: 1, RequestFingerprint: "paid"})
			errorsSeen <- e
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for e := range errorsSeen {
		require.NoError(t, e)
	}
	var balance float64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=11`).Scan(&balance))
	require.Equal(t, 99.0, balance)
	// Then race successful capture against non-charge finalization on the same row.
	require.NoError(t, repo.RegisterVideoBilling(ctx, &service.VideoBillingOutcome{APIKeyID: 7, UserID: 11, RequestID: "client:race", Fingerprint: "race", Model: "seedance-2.0", Resolution: "720p", Seconds: 4}))
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				_, _ = repo.Apply(ctx, &service.UsageBillingCommand{DeferredVideo: true, RequestID: "client:race", APIKeyID: 7, UserID: 11, BalanceCost: 1, RequestFingerprint: "race"})
			} else {
				_, _ = repo.CloseVideoBillingWithoutCharge(ctx, 7, 11, "client:race")
			}
		}(i)
	}
	wg.Wait()
	outcome, err := repo.FindVideoBillingOutcome(ctx, 7, 11, "client:race")
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=11`).Scan(&balance))
	var captures int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id='client:race'`).Scan(&captures))
	if outcome.Status == "settled" {
		require.Equal(t, 98.0, balance)
		require.Equal(t, 1, captures)
	} else {
		require.Equal(t, "not_charged", outcome.Status)
		require.Equal(t, 99.0, balance)
		require.Equal(t, 0, captures)
	}
	foreign, err := repo.FindVideoBillingOutcome(ctx, 8, 11, "client:paid")
	require.NoError(t, err)
	require.Nil(t, foreign)
}
