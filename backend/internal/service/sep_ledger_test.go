package service

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestSEPLedgerPostgresNumericPaginationAndSnapshot(t *testing.T) {
	dsn := os.Getenv("TEST_SEP_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set TEST_SEP_DATABASE_DSN to an isolated PostgreSQL test database")
	}
	for _, limit := range []int{3, 5} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			db, err := sql.Open("postgres", dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			// Session-local tables shadow real tables without touching their data.
			db.SetMaxOpenConns(1)
			_, err = db.ExecContext(t.Context(), `
CREATE TEMP TABLE sep_enterprises (
    id BIGINT PRIMARY KEY,
    integration_id TEXT NOT NULL,
    enterprise_id TEXT NOT NULL,
    sequence BIGINT NOT NULL,
    available_cny NUMERIC(24,6) NOT NULL,
    frozen_cny NUMERIC(24,6) NOT NULL
);
CREATE TEMP TABLE sep_ledger (
    id TEXT PRIMARY KEY,
    enterprise_pk BIGINT NOT NULL,
    sequence BIGINT NOT NULL,
    type TEXT NOT NULL,
    amount_cny NUMERIC(24,6) NOT NULL,
    free_cny NUMERIC(24,6) NOT NULL,
    available_cny NUMERIC(24,6) NOT NULL,
    frozen_cny NUMERIC(24,6) NOT NULL,
    transfer_id TEXT,
    execution_id TEXT,
    settlement_id TEXT,
    effective_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (enterprise_pk, sequence)
);
INSERT INTO sep_enterprises VALUES (1,'integration-test','enterprise-test',12,8,0);
INSERT INTO sep_ledger (
    id, enterprise_pk, sequence, type, amount_cny, free_cny,
    available_cny, frozen_cny, transfer_id, execution_id, settlement_id
)
SELECT 'ledger-' || n, 1, n,
    CASE WHEN n=11 THEN 'paid_consumption' WHEN n=12 THEN 'free_record' ELSE 'credit_compute' END,
    CASE WHEN n=11 THEN 2 WHEN n=12 THEN 0 ELSE 1 END,
    CASE WHEN n=12 THEN 3 ELSE 0 END,
    CASE WHEN n<=10 THEN n ELSE 8 END, 0,
    CASE WHEN n<=10 THEN 'transfer-' || n END,
    CASE WHEN n>=11 THEN 'execution-mixed' END,
    CASE WHEN n>=11 THEN 'settlement-mixed' END
FROM generate_series(1,12) AS n;
`)
			require.NoError(t, err)
			svc := &SEPService{db: db}
			var after, watermark int64
			var sequences []string
			var items []map[string]any
			for page := 0; ; page++ {
				require.Less(t, page, 12, "pagination must terminate")
				result, err := svc.Ledger(t.Context(), "integration-test", "enterprise-test", after, watermark, limit)
				require.NoError(t, err)
				require.Equal(t, "12", result["snapshot_sequence"])
				require.Equal(t, "8.000000", result["available_paid_cny"])
				require.Equal(t, "0.000000", result["frozen_paid_cny"])
				require.Equal(t, "8.000000", result["total_paid_cny"])
				batch := result["items"].([]map[string]any)
				require.NotEmpty(t, batch)
				require.LessOrEqual(t, len(batch), limit)
				for _, item := range batch {
					sequence := item["sequence"].(string)
					require.Equal(t, strconv.Itoa(len(sequences)+1), sequence)
					sequences = append(sequences, sequence)
					items = append(items, item)
				}
				require.Equal(t, sequences[len(sequences)-1], result["after_sequence"])
				after, err = strconv.ParseInt(result["after_sequence"].(string), 10, 64)
				require.NoError(t, err)
				watermark, err = strconv.ParseInt(result["snapshot_sequence"].(string), 10, 64)
				require.NoError(t, err)
				require.Equal(t, after < watermark, result["has_more"])
				if page == 0 {
					_, err = db.ExecContext(t.Context(), `
INSERT INTO sep_ledger (id,enterprise_pk,sequence,type,amount_cny,free_cny,available_cny,frozen_cny,transfer_id)
VALUES ('ledger-13',1,13,'credit_compute',1,0,9,0,'transfer-13');
UPDATE sep_enterprises SET sequence=13,available_cny=9 WHERE id=1;
`)
					require.NoError(t, err)
				}
				if !result["has_more"].(bool) {
					break
				}
			}
			require.Equal(t, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}, sequences)
			require.Equal(t, "paid_consumption", items[10]["type"])
			require.Equal(t, "2.000000", items[10]["amount_cny"])
			require.Equal(t, "free_record", items[11]["type"])
			require.Equal(t, "0.000000", items[11]["amount_cny"])
			require.Equal(t, "3.000000", items[11]["free_cny"])
			require.Equal(t, items[10]["settlement_id"], items[11]["settlement_id"])
			var paidBalance, freeBalance string
			err = db.QueryRowContext(t.Context(), `SELECT paid.available_cny::text,free.available_cny::text FROM sep_ledger paid JOIN sep_ledger free ON free.enterprise_pk=paid.enterprise_pk WHERE paid.sequence=11 AND free.sequence=12`).Scan(&paidBalance, &freeBalance)
			require.NoError(t, err)
			require.Equal(t, paidBalance, freeBalance, "free records must not reduce the paid balance")
			latest, err := svc.Ledger(t.Context(), "integration-test", "enterprise-test", after, 0, limit)
			require.NoError(t, err)
			require.Equal(t, "13", latest["snapshot_sequence"])
			require.Equal(t, "9.000000", latest["total_paid_cny"])
			latestItems := latest["items"].([]map[string]any)
			require.Len(t, latestItems, 1)
			require.Equal(t, "13", latestItems[0]["sequence"])
			require.Equal(t, "13", latest["after_sequence"])
			require.Equal(t, false, latest["has_more"])
		})
	}
}
