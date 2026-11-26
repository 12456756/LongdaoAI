//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type modelPriceSettingsStub struct {
	service.SettingRepository
	rate string
}

func (s *modelPriceSettingsStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyPlatformUSDToCNYRate: s.rate}, nil
}

func TestManagedBasePricePostgresSelectionAndHistory(t *testing.T) {
	dsn := os.Getenv("TEST_PRICING_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set TEST_PRICING_DATABASE_DSN to an isolated test database")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	// A private schema keeps this test isolated from any other database contents.
	db.SetMaxOpenConns(1)
	schema := fmt.Sprintf("pricing_test_%d", time.Now().UnixNano())
	_, err = db.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	defer func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") }()
	_, err = db.Exec("SET search_path TO " + schema)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE accounts(id BIGSERIAL PRIMARY KEY,name text NOT NULL,platform text NOT NULL DEFAULT 'openai',status text NOT NULL DEFAULT 'active',credentials jsonb NOT NULL DEFAULT '{}',deleted_at timestamptz)`)
	require.NoError(t, err)
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "182_model_base_pricing.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	migration, err = os.ReadFile(filepath.Join("..", "..", "migrations", "183_model_base_price_decimal.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	settings := &modelPriceSettingsStub{rate: "7.15"}
	repo := NewModelBasePriceRepository(db, settings)
	ctx := context.Background()
	company, err := repo.SaveCompany(ctx, service.UpstreamCompany{Name: "上游公司 A"})
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO accounts(name,upstream_company_id,credentials) VALUES('账号 A',$1,'{\"model_mapping\":{\"custom-model\":\"alias\"}}')", company.ID)
	require.NoError(t, err)
	require.Error(t, repo.DeleteCompany(ctx, company.ID))
	card := service.AccountModelPrice{Model: "custom-model", Mode: service.BillingModeImage, Discount: "0.5", Markup: "1.6", Items: []service.ModelPriceItem{{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "0.1"}}}
	data, err := json.Marshal(card)
	require.NoError(t, err)
	var priceID int64
	err = db.QueryRow("INSERT INTO account_model_prices(account_id,model,price) VALUES(1,$1,$2) RETURNING id", card.Model, string(data)).Scan(&priceID)
	require.NoError(t, err)
	base, err := repo.SelectBase(ctx, card.Model, priceID, 1, 0, 7, "7.15")
	require.NoError(t, err)
	require.EqualValues(t, 1, base.Version)
	require.Equal(t, "0.08", base.Price.Items[0].Amount)
	_, err = repo.SelectBase(ctx, card.Model, priceID, 1, 0, 7, "7.15")
	require.ErrorIs(t, err, service.ErrPriceRevisionConflict)
	_, err = db.Exec("UPDATE account_model_prices SET revision=2 WHERE id=$1", priceID)
	require.NoError(t, err)
	_, err = repo.SelectBase(ctx, card.Model, priceID, 1, 1, 7, "7.15")
	require.ErrorIs(t, err, service.ErrPriceRevisionConflict)
	stored, err := repo.GetBase(ctx, card.Model)
	require.NoError(t, err)
	require.Equal(t, "0.08", stored.Price.Items[0].Amount)
	base, err = repo.SelectBase(ctx, card.Model, priceID, 2, 1, 7, "7.15")
	require.NoError(t, err)
	require.EqualValues(t, 2, base.Version)
	history, err := repo.History(ctx, card.Model)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.EqualValues(t, 1, history[1].Version)
	frozen, err := repo.RememberSnapshot(ctx, 1, 2, "client:test", &history[1])
	require.NoError(t, err)
	same, err := repo.RememberSnapshot(ctx, 1, 2, "client:test", base)
	require.NoError(t, err)
	require.Equal(t, frozen.Version, same.Version)
	catalog, err := repo.Catalog(ctx)
	require.NoError(t, err)
	require.Len(t, catalog.Accounts, 1)
	require.Equal(t, company.Name, catalog.Accounts[0].CompanyName)
	require.Contains(t, catalog.Accounts[0].Models, "custom-model")
	require.Equal(t, "7.15", catalog.USDToCNYRate)

	card.Currency = "CNY"
	card.Items[0].Unit = "CNY/image"
	card.Items[0].Amount = "1.43"
	card.Discount, card.Markup = "0.5", "2"
	data, err = json.Marshal(card)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE account_model_prices SET price=$1,revision=3 WHERE id=$2", string(data), priceID)
	require.NoError(t, err)
	_, err = repo.SelectBase(ctx, card.Model, priceID, 3, 2, 7, "7")
	require.ErrorIs(t, err, service.ErrPriceExchangeRateChanged)
	_, err = repo.SelectBase(ctx, card.Model, priceID, 3, 2, 7, "")
	require.ErrorIs(t, err, service.ErrPriceExchangeRateChanged)
	base, err = repo.SelectBase(ctx, card.Model, priceID, 3, 2, 7, "7.15")
	require.NoError(t, err)
	require.Equal(t, "0.2", base.Price.Items[0].Amount)
	require.Equal(t, "USD/image", base.Price.Items[0].Unit)
	require.Equal(t, "CNY", base.Price.Conversion.SourceCurrency)
	require.Equal(t, "1.43", base.Price.Conversion.SourceItems[0].Amount)
	settings.rate = "8"
	stored, err = repo.GetBase(ctx, card.Model)
	require.NoError(t, err)
	require.Equal(t, "0.2", stored.Price.Items[0].Amount)
	require.Equal(t, "7.15", stored.Price.Conversion.USDToCNYRate)
	_, err = repo.SelectBase(ctx, card.Model, priceID, 3, 3, 7, "7.15")
	require.ErrorIs(t, err, service.ErrPriceExchangeRateChanged)
	base, err = repo.SelectBase(ctx, card.Model, priceID, 3, 3, 7, "8")
	require.NoError(t, err)
	require.Equal(t, "0.17875", base.Price.Items[0].Amount)
}
