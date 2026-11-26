package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type modelBasePriceRepository struct {
	db       *sql.DB
	settings service.SettingRepository
}

func NewModelBasePriceRepository(db *sql.DB, settings service.SettingRepository) service.ModelBasePriceRepository {
	return &modelBasePriceRepository{db: db, settings: settings}
}

func (r *modelBasePriceRepository) ExchangeRate(ctx context.Context) (string, error) {
	return service.ReadPlatformExchangeRate(ctx, r.settings)
}

type baseScanner interface{ Scan(...any) error }

func scanBase(row baseScanner) (*service.ModelBasePrice, error) {
	var p service.ModelBasePrice
	var data []byte
	err := row.Scan(&p.Model, &p.SourcePriceID, &p.SourceRevision, &p.Version, &data, &p.UpdatedBy, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &p.Price); err != nil {
		return nil, err
	}
	return &p, nil
}

const baseColumns = "model,source_price_id,source_revision,version,price,updated_by,updated_at"

func (r *modelBasePriceRepository) GetBase(ctx context.Context, model string) (*service.ModelBasePrice, error) {
	return scanBase(r.db.QueryRowContext(ctx, "SELECT "+baseColumns+" FROM model_base_prices WHERE model=$1", model))
}

func (r *modelBasePriceRepository) Companies(ctx context.Context) ([]service.UpstreamCompany, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT id,name,notes FROM upstream_companies ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []service.UpstreamCompany{}
	for rows.Next() {
		var c service.UpstreamCompany
		if err = rows.Scan(&c.ID, &c.Name, &c.Notes); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (r *modelBasePriceRepository) SaveCompany(ctx context.Context, c service.UpstreamCompany) (*service.UpstreamCompany, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len([]rune(c.Name)) > 100 || len(c.Notes) > 4000 {
		return nil, infraerrors.New(http.StatusBadRequest, "invalid_upstream_company", "请填写有效的上游公司名称（最多 100 字）和备注")
	}
	var err error
	if c.ID == 0 {
		err = r.db.QueryRowContext(ctx, "INSERT INTO upstream_companies(name,notes) VALUES($1,$2) RETURNING id", c.Name, c.Notes).Scan(&c.ID)
	} else {
		err = r.db.QueryRowContext(ctx, "UPDATE upstream_companies SET name=$2,notes=$3,updated_at=now() WHERE id=$1 RETURNING id", c.ID, c.Name, c.Notes).Scan(&c.ID)
	}
	var pe *pq.Error
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, infraerrors.New(http.StatusConflict, "upstream_company_exists", "该上游公司名称已存在")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, infraerrors.New(http.StatusNotFound, "upstream_company_not_found", "上游公司不存在，请刷新后重试")
	}
	return &c, err
}

func (r *modelBasePriceRepository) DeleteCompany(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM upstream_companies WHERE id=$1", id)
	var pe *pq.Error
	if errors.As(err, &pe) && pe.Code == "23503" {
		return infraerrors.New(http.StatusConflict, "upstream_company_in_use", "该公司仍有关联账号，请先调整账号归属")
	}
	return err
}

func (r *modelBasePriceRepository) AccountPrices(ctx context.Context, id int64) ([]service.AccountModelPrice, error) {
	return readAccountPrices(ctx, r.db, id)
}

func (r *modelBasePriceRepository) RememberSnapshot(ctx context.Context, keyID, userID int64, requestID string, p *service.ModelBasePrice) (*service.ModelBasePrice, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO request_model_price_snapshots(api_key_id,user_id,request_id,model,price) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, keyID, userID, requestID, p.Model, string(data))
	if err != nil {
		return nil, err
	}
	var stored []byte
	if err = r.db.QueryRowContext(ctx, "SELECT price FROM request_model_price_snapshots WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3", keyID, userID, requestID).Scan(&stored); err != nil {
		return nil, err
	}
	var result service.ModelBasePrice
	err = json.Unmarshal(stored, &result)
	return &result, err
}

func readAccountPrices(ctx context.Context, q sqlExecutor, id int64) ([]service.AccountModelPrice, error) {
	query := "SELECT id,account_id,model,revision,price FROM account_model_prices WHERE active"
	args := []any{}
	if id > 0 {
		query += " AND account_id=$1"
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY model", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.AccountModelPrice{}
	for rows.Next() {
		var p service.AccountModelPrice
		var data []byte
		var pid, aid, rev int64
		var model string
		if err = rows.Scan(&pid, &aid, &model, &rev, &data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		p.ID = pid
		p.AccountID = aid
		p.Model = model
		p.Revision = rev
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *modelBasePriceRepository) Catalog(ctx context.Context) (*service.ModelPriceCatalog, error) {
	out := &service.ModelPriceCatalog{Accounts: []service.ModelPriceAccount{}, Bases: []service.ModelBasePrice{}}
	rate, err := r.ExchangeRate(ctx)
	if err != nil {
		return nil, err
	}
	out.USDToCNYRate = rate
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,a.name,a.upstream_company_id,COALESCE(c.name,'未归属'),a.platform,a.status,a.credentials->'model_mapping'
        FROM accounts a LEFT JOIN upstream_companies c ON c.id=a.upstream_company_id WHERE a.deleted_at IS NULL ORDER BY COALESCE(c.name,'未归属'),a.id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a service.ModelPriceAccount
		var mapping []byte
		if err = rows.Scan(&a.ID, &a.Name, &a.CompanyID, &a.CompanyName, &a.Platform, &a.Status, &mapping); err != nil {
			rows.Close()
			return nil, err
		}
		a.Models = []string{}
		var m map[string]string
		if len(mapping) > 0 {
			if err = json.Unmarshal(mapping, &m); err != nil {
				rows.Close()
				return nil, err
			}
		}
		a.Models = service.PricingCatalogModels(a.Platform, m)
		out.Accounts = append(out.Accounts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	prices, err := readAccountPrices(ctx, r.db, 0)
	if err != nil {
		return nil, err
	}
	byAccount := make(map[int64][]service.AccountModelPrice)
	for _, price := range prices {
		byAccount[price.AccountID] = append(byAccount[price.AccountID], price)
	}
	for i := range out.Accounts {
		out.Accounts[i].Prices = byAccount[out.Accounts[i].ID]
		if out.Accounts[i].Prices == nil {
			out.Accounts[i].Prices = []service.AccountModelPrice{}
		}
	}
	bases, err := r.db.QueryContext(ctx, "SELECT "+baseColumns+" FROM model_base_prices ORDER BY model")
	if err != nil {
		return nil, err
	}
	defer bases.Close()
	for bases.Next() {
		p, e := scanBase(bases)
		if e != nil {
			return nil, e
		}
		out.Bases = append(out.Bases, *p)
	}
	return out, bases.Err()
}

func (r *modelBasePriceRepository) SelectBase(ctx context.Context, model string, id, revision, expectedVersion, actor int64, expectedRate string) (*service.ModelBasePrice, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Serialize first selection as well as updates of an existing model.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", model); err != nil {
		return nil, err
	}
	var raw []byte
	var rev int64
	var accountName, companyName string
	err = tx.QueryRowContext(ctx, `SELECT p.price,p.revision,a.name,COALESCE(c.name,'未归属')
        FROM account_model_prices p JOIN accounts a ON a.id=p.account_id
        LEFT JOIN upstream_companies c ON c.id=a.upstream_company_id
        WHERE p.id=$1 AND p.model=$2 AND p.active AND a.deleted_at IS NULL FOR UPDATE OF p`, id, model).Scan(&raw, &rev, &accountName, &companyName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, infraerrors.New(http.StatusConflict, "model_price_source_unavailable", "所选账号模型价格已失效，请刷新后重新选择")
	}
	if err != nil {
		return nil, err
	}
	if rev != revision {
		return nil, service.ErrPriceRevisionConflict
	}
	var currentVersion int64
	err = tx.QueryRowContext(ctx, "SELECT version FROM model_base_prices WHERE model=$1 FOR UPDATE", model).Scan(&currentVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if currentVersion != expectedVersion {
		return nil, service.ErrPriceRevisionConflict
	}
	var source service.AccountModelPrice
	if err = json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	if err := service.ValidateAccountModelPrices([]service.AccountModelPrice{source}); err != nil {
		return nil, err
	}
	rate, err := r.ExchangeRate(ctx)
	if err != nil {
		return nil, err
	}
	if (expectedRate != "" && expectedRate != rate) || (source.QuoteCurrency() == "CNY" && expectedRate == "") {
		return nil, service.ErrPriceExchangeRateChanged
	}
	price, err := source.Candidate(rate)
	if err != nil {
		return nil, err
	}
	price.ID = id
	price.Revision = rev
	price.SourceAccountName = accountName
	price.SourceCompanyName = companyName
	data, err := json.Marshal(price)
	if err != nil {
		return nil, err
	}
	p, err := scanBase(tx.QueryRowContext(ctx, `INSERT INTO model_base_prices(model,source_price_id,source_revision,version,price,updated_by)
        VALUES($1,$2,$3,1,$4,$5) ON CONFLICT(model) DO UPDATE SET source_price_id=$2,source_revision=$3,
        version=model_base_prices.version+1,price=$4,updated_by=$5,updated_at=now() RETURNING `+baseColumns, model, id, rev, string(data), actor))
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO model_base_price_history(model,source_price_id,source_revision,version,price,updated_by,updated_at)
        SELECT `+baseColumns+` FROM model_base_prices WHERE model=$1`, model); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *modelBasePriceRepository) History(ctx context.Context, model string) ([]service.ModelBasePrice, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+baseColumns+" FROM model_base_price_history WHERE model=$1 ORDER BY version DESC LIMIT 100", model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.ModelBasePrice{}
	for rows.Next() {
		p, e := scanBase(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Account prices share the account mutation transaction. Never overwrite a revision silently.
func saveAccountModelPrices(ctx context.Context, client *dbent.Client, a *service.Account) error {
	if a.PricingUpdate == nil {
		return nil
	}
	update := a.PricingUpdate
	if update.CompanyID != nil {
		var company any
		if *update.CompanyID > 0 {
			company = *update.CompanyID
		}
		if _, err := client.ExecContext(ctx, "UPDATE accounts SET upstream_company_id=$2 WHERE id=$1", a.ID, company); err != nil {
			var pe *pq.Error
			if errors.As(err, &pe) && pe.Code == "23503" {
				return infraerrors.New(http.StatusBadRequest, "upstream_company_not_found", "所选上游公司不存在，请刷新公司列表")
			}
			return err
		}
		a.UpstreamCompanyID = update.CompanyID
	}
	if update.Prices == nil {
		return nil
	}
	// The account row is already locked by the caller. Check all revisions before writing.
	previous, err := readAccountPrices(ctx, client, a.ID)
	if err != nil {
		return err
	}
	if update.ExpectedVersion != "" {
		if update.ExpectedVersion != service.AccountPricesVersion(previous) {
			return service.ErrPriceRevisionConflict
		}
	} else if len(previous) > 0 {
		return service.ErrPriceRevisionConflict
	}
	versions := map[string]int64{}
	oldPrices := map[string]service.AccountModelPrice{}
	for _, p := range previous {
		versions[p.Model] = p.Revision
		oldPrices[p.Model] = p
	}
	for _, p := range *update.Prices {
		if versions[p.Model] != p.Revision {
			return service.ErrPriceRevisionConflict
		}
	}
	if _, err = client.ExecContext(ctx, "UPDATE account_model_prices SET active=false WHERE account_id=$1", a.ID); err != nil {
		return err
	}
	for _, p := range *update.Prices {
		p.Conversion = nil
		if p.QuoteCurrency() == "USD" {
			p.Currency = ""
		}
		p.SourceAccountName = ""
		p.SourceCompanyName = ""
		p.ID = 0
		p.AccountID = a.ID
		p.Revision = 0
		b, e := json.Marshal(p)
		if e != nil {
			return e
		}
		if old, ok := oldPrices[p.Model]; ok {
			old.Conversion = nil
			if old.QuoteCurrency() == "USD" {
				old.Currency = ""
			}
			old.ID = 0
			old.AccountID = a.ID
			old.Revision = 0
			previousJSON, e := json.Marshal(old)
			if e != nil {
				return e
			}
			if bytes.Equal(previousJSON, b) {
				if _, err = client.ExecContext(ctx, "UPDATE account_model_prices SET active=true WHERE account_id=$1 AND model=$2", a.ID, p.Model); err != nil {
					return err
				}
				continue
			}
		}
		_, err = client.ExecContext(ctx, `INSERT INTO account_model_prices(account_id,model,price) VALUES($1,$2,$3)
            ON CONFLICT(account_id,model) DO UPDATE SET revision=account_model_prices.revision+1,price=$3,active=true,updated_at=now()`, a.ID, p.Model, string(b))
		if err != nil {
			return err
		}
	}
	return nil
}
