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

func TestAsyncImageBillingPostgresConcurrencyAndNoCharge(t *testing.T) {
	dsn := os.Getenv("LONGDAO_IMAGE_TEST_DSN")
	if dsn == "" {
		t.Skip("LONGDAO_IMAGE_TEST_DSN must point to a disposable local database")
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
	schema := fmt.Sprintf("image_contract_test_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	defer func() { _, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()
	query := endpoint.Query()
	query.Set("search_path", schema)
	endpoint.RawQuery = query.Encode()
	db, err := sql.Open("postgres", endpoint.String())
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, `CREATE TABLE users(id BIGINT PRIMARY KEY,balance NUMERIC NOT NULL,deleted_at TIMESTAMPTZ,updated_at TIMESTAMPTZ);
CREATE TABLE usage_billing_dedup(id BIGSERIAL PRIMARY KEY,request_id TEXT,api_key_id BIGINT,request_fingerprint TEXT,UNIQUE(request_id,api_key_id));
CREATE TABLE usage_billing_dedup_archive(request_id TEXT,api_key_id BIGINT,request_fingerprint TEXT,UNIQUE(request_id,api_key_id));
INSERT INTO users(id,balance) VALUES(11,100);`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("183_image_request_billing.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	repo := &usageBillingRepository{db: db}
	register := func(id string) {
		require.NoError(t, repo.RegisterImageBilling(ctx, &service.ImageBillingOutcome{APIKeyID: 7, UserID: 11, RequestID: id, Fingerprint: "fixture", Model: "gpt-image-2", Size: "1024x1024"}))
	}
	register("client:paid")
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.Apply(ctx, &service.UsageBillingCommand{DeferredImage: true, APIKeyID: 7, UserID: 11, RequestID: "client:paid", RequestFingerprint: "fixture", BalanceCost: 1})
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var balance float64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=11`).Scan(&balance))
	require.Equal(t, 99.0, balance)
	row, err := repo.FindImageBillingOutcome(ctx, 7, 11, "client:paid")
	require.NoError(t, err)
	require.Equal(t, "settled", row.Status)
	register("client:failed")
	row, err = repo.CloseImageBillingWithoutCharge(ctx, 7, 11, "client:failed")
	require.NoError(t, err)
	require.Equal(t, "not_charged", row.Status)
	_, err = repo.Apply(ctx, &service.UsageBillingCommand{DeferredImage: true, APIKeyID: 7, UserID: 11, RequestID: "client:failed", RequestFingerprint: "fixture", BalanceCost: 1})
	require.Error(t, err)
	register("client:race")
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				_, _ = repo.Apply(ctx, &service.UsageBillingCommand{DeferredImage: true, APIKeyID: 7, UserID: 11, RequestID: "client:race", RequestFingerprint: "fixture", BalanceCost: 1})
			} else {
				_, _ = repo.CloseImageBillingWithoutCharge(ctx, 7, 11, "client:race")
			}
		}(i)
	}
	wg.Wait()
	row, err = repo.FindImageBillingOutcome(ctx, 7, 11, "client:race")
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=11`).Scan(&balance))
	if row.Status == "settled" {
		require.Equal(t, 98.0, balance)
	} else {
		require.Equal(t, "not_charged", row.Status)
		require.Equal(t, 99.0, balance)
	}
	other, err := repo.FindImageBillingOutcome(ctx, 8, 11, "client:paid")
	require.NoError(t, err)
	require.Nil(t, other)
}
