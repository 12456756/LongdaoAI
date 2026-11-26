//go:build unit

package repository

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestMergedPricingAndMediaMigrations(t *testing.T) {
	dsn := os.Getenv("TEST_SEP_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set TEST_SEP_DATABASE_DSN to an isolated database")
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	for _, initial := range []string{"fresh", "main", "managed_pricing"} {
		t.Run(initial, func(t *testing.T) {
			admin, err := sql.Open("postgres", dsn)
			require.NoError(t, err)
			defer admin.Close()
			name := fmt.Sprintf("merged_pricing_media_%d", time.Now().UnixNano())
			_, err = admin.Exec("CREATE DATABASE " + name)
			require.NoError(t, err)
			defer func() { _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); require.NoError(t, err) }()
			parsed, err := url.Parse(dsn)
			require.NoError(t, err)
			parsed.Path = "/" + name
			db, err := sql.Open("postgres", parsed.String())
			require.NoError(t, err)
			defer db.Close()
			if initial != "fresh" {
				subset := fstest.MapFS{}
				for _, filename := range names {
					media := filename == "182_video_request_billing.sql" || filename == "183_image_request_billing.sql"
					if initial == "main" && filename >= "182_" && !media {
						continue
					}
					if initial == "managed_pricing" && media {
						continue
					}
					data, err := fs.ReadFile(migrations.FS, filename)
					require.NoError(t, err)
					subset[filename] = &fstest.MapFile{Data: data}
				}
				require.NoError(t, applyMigrationsFS(t.Context(), db, subset))
			}
			require.NoError(t, ApplyMigrations(t.Context(), db))
			require.NoError(t, ApplyMigrations(t.Context(), db))
			for _, table := range []string{"model_base_prices", "account_model_prices", "sep_enterprises", "sep_pending_settlements", "sep_credential_rotations", "video_request_billing", "image_request_billing"} {
				var exists bool
				require.NoError(t, db.QueryRow("SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists))
				require.True(t, exists, table)
			}
			var count int
			require.NoError(t, db.QueryRow("SELECT count(*) FROM schema_migrations WHERE filename LIKE '182_%' OR filename LIKE '183_%'").Scan(&count))
			require.Equal(t, 4, count)
		})
	}
}
