package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrBasePriceMissing         = infraerrors.New(http.StatusServiceUnavailable, "model_base_price_not_configured", "模型尚未设置基础价格，请联系管理员配置")
	ErrBasePriceUnavailable     = infraerrors.New(http.StatusServiceUnavailable, "model_pricing_service_unavailable", "暂时无法读取模型基础价格，请稍后重试")
	ErrPriceRevisionConflict    = infraerrors.New(http.StatusConflict, "model_price_version_conflict", "价格已被修改，请刷新后重新确认")
	ErrPriceExchangeRateChanged = infraerrors.New(http.StatusConflict, "model_price_exchange_rate_changed", "平台统一汇率已变化，请刷新报价后重新确认基础价")
)

type UpstreamCompany struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Notes string `json:"notes"`
}

type AccountPricingUpdate struct {
	CompanyID       *int64
	Prices          *[]AccountModelPrice
	ExpectedVersion string
}

func AccountPricesVersion(prices []AccountModelPrice) string {
	type revision struct {
		Model    string
		Revision int64
	}
	entries := make([]revision, 0, len(prices))
	for _, p := range prices {
		entries = append(entries, revision{p.Model, p.Revision})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Model < entries[j].Model })
	data, _ := json.Marshal(entries)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

type upstreamCompanyFilterKey struct{}

func WithUpstreamCompanyFilter(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, upstreamCompanyFilterKey{}, id)
}
func UpstreamCompanyFilter(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(upstreamCompanyFilterKey{}).(int64)
	return id, ok
}

// Amounts are decimal strings. Token prices are expressed per million tokens.
type ModelPriceItem struct {
	Kind   string `json:"kind"`
	Tier   string `json:"tier"`
	Unit   string `json:"unit"`
	Amount string `json:"amount"`
}

type ModelPricePolicy struct {
	LongContextThreshold        int     `json:"long_context_threshold,omitempty"`
	LongContextInputMultiplier  float64 `json:"long_context_input_multiplier,omitempty"`
	LongContextOutputMultiplier float64 `json:"long_context_output_multiplier,omitempty"`
}

type ModelPriceConversion struct {
	SourceCurrency string           `json:"source_currency"`
	USDToCNYRate   string           `json:"usd_to_cny_rate"`
	SourceItems    []ModelPriceItem `json:"source_items"`
}

type AccountModelPrice struct {
	SourceAccountName string                `json:"source_account_name,omitempty"`
	SourceCompanyName string                `json:"source_company_name,omitempty"`
	ID                int64                 `json:"id"`
	AccountID         int64                 `json:"account_id"`
	Model             string                `json:"model"`
	UpstreamModel     string                `json:"upstream_model"`
	Mode              BillingMode           `json:"mode"`
	Currency          string                `json:"currency,omitempty"`
	Conversion        *ModelPriceConversion `json:"conversion,omitempty"`
	Discount          string                `json:"discount"`
	Markup            string                `json:"markup"`
	Items             []ModelPriceItem      `json:"items"`
	Policy            ModelPricePolicy      `json:"policy"`
	Revision          int64                 `json:"revision"`
}

type ModelBasePrice struct {
	Model          string            `json:"model"`
	SourcePriceID  int64             `json:"source_price_id"`
	SourceRevision int64             `json:"source_revision"`
	Version        int64             `json:"version"`
	Price          AccountModelPrice `json:"price"`
	UpdatedBy      int64             `json:"updated_by"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type ModelPriceAccount struct {
	ID          int64               `json:"id"`
	Name        string              `json:"name"`
	CompanyID   *int64              `json:"company_id"`
	CompanyName string              `json:"company_name"`
	Platform    string              `json:"platform"`
	Status      string              `json:"status"`
	Models      []string            `json:"models"`
	Prices      []AccountModelPrice `json:"prices"`
}

type ModelPriceCatalog struct {
	USDToCNYRate string              `json:"usd_to_cny_rate"`
	Accounts     []ModelPriceAccount `json:"accounts"`
	Bases        []ModelBasePrice    `json:"bases"`
	Models       []string            `json:"models"`
	Total        int                 `json:"total,omitempty"`
	Page         int                 `json:"page,omitempty"`
	PageSize     int                 `json:"page_size,omitempty"`
}

type ModelBasePriceRepository interface {
	GetBase(context.Context, string) (*ModelBasePrice, error)
	Catalog(context.Context) (*ModelPriceCatalog, error)
	Companies(context.Context) ([]UpstreamCompany, error)
	SaveCompany(context.Context, UpstreamCompany) (*UpstreamCompany, error)
	DeleteCompany(context.Context, int64) error
	SelectBase(context.Context, string, int64, int64, int64, int64, string) (*ModelBasePrice, error)
	ExchangeRate(context.Context) (string, error)
	History(context.Context, string) ([]ModelBasePrice, error)
	AccountPrices(context.Context, int64) ([]AccountModelPrice, error)
	RememberSnapshot(context.Context, int64, int64, string, *ModelBasePrice) (*ModelBasePrice, error)
}

var priceDecimal = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,11})(?:\.[0-9]{1,12})?$`)

func priceRat(value, field string) (*big.Rat, error) {
	if !priceDecimal.MatchString(value) {
		return nil, infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "%s 必须是非负十进制数，最多 12 位小数", field)
	}
	n, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest, "invalid_model_price", "价格格式无效")
	}
	return n, nil
}

func validPricingModel(model string) bool {
	return model != "" && len(model) <= 200 && !strings.ContainsAny(model, " \t\r\n*?")
}

// ValidateAccountModelPrices also rejects ambiguous units and duplicate tiers.
func ValidateAccountModelPrices(prices []AccountModelPrice) error {
	if len(prices) > 500 {
		return infraerrors.New(http.StatusBadRequest, "invalid_model_price", "单个账号最多配置 500 个模型价格")
	}
	seen := map[string]bool{}
	for _, p := range prices {
		currency := p.QuoteCurrency()
		if currency != "USD" && currency != "CNY" {
			return infraerrors.New(http.StatusBadRequest, "invalid_model_price_currency", "模型报价币种仅支持 USD 或 CNY")
		}
		if !validPricingModel(p.Model) || seen[p.Model] {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型名称无效或重复：%s", p.Model)
		}
		seen[p.Model] = true
		if p.UpstreamModel != "" && !validPricingModel(p.UpstreamModel) {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的上游模型名称无效", p.Model)
		}
		d, err := priceRat(p.Discount, p.Model+" 的采购折扣")
		if err != nil {
			return err
		}
		if d.Cmp(big.NewRat(1, 1)) > 0 {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的采购折扣必须在 0 至 1 之间", p.Model)
		}
		if _, err := priceRat(p.Markup, p.Model+" 的公司加价倍数"); err != nil {
			return err
		}
		if p.Mode != BillingModeToken && p.Mode != BillingModeImage && p.Mode != BillingModeVideo && p.Mode != BillingModePerRequest {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的计费方式无效", p.Model)
		}
		if len(p.Items) == 0 || len(p.Items) > 100 {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 必须配置价格明细", p.Model)
		}
		keys := map[string]bool{}
		for _, item := range p.Items {
			key := item.Kind + ":" + item.Tier
			if keys[key] {
				return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的价格项目重复：%s", p.Model, key)
			}
			keys[key] = true
			valid := false
			if p.Mode == BillingModeToken && item.Unit == currency+"/MTok" {
				switch item.Kind {
				case "input", "output", "cache_read", "cache_write", "image_input", "image_output":
					valid = item.Tier == "" || (item.Tier == "priority" && item.Kind != "image_input" && item.Kind != "image_output") || (item.Kind == "cache_write" && (item.Tier == "5m" || item.Tier == "1h"))
				}
			} else if p.Mode == BillingModeImage {
				valid = item.Kind == "image" && item.Unit == currency+"/image" && (item.Tier == "1K" || item.Tier == "2K" || item.Tier == "4K")
			} else if p.Mode == BillingModeVideo {
				valid = item.Kind == "video" && item.Unit == currency+"/second" && ValidVideoBillingResolution(item.Tier)
			} else if p.Mode == BillingModePerRequest {
				valid = item.Kind == "request" && item.Unit == currency+"/request" && len(item.Tier) <= 32
			}
			if !valid {
				return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的价格项目或单位无效：%s (%s)", p.Model, key, item.Unit)
			}
			if _, err := priceRat(item.Amount, p.Model+" / "+key); err != nil {
				return err
			}
		}
		if p.Mode == BillingModeToken && (!keys["input:"] || !keys["output:"]) {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的输入、输出原价必须填写", p.Model)
		}
		if keys["cache_write:5m"] != keys["cache_write:1h"] {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的 5 分钟和 1 小时缓存写入价格必须同时填写", p.Model)
		}
		policy := p.Policy
		if policy.LongContextThreshold < 0 || math.IsNaN(policy.LongContextInputMultiplier) || math.IsInf(policy.LongContextInputMultiplier, 0) || math.IsNaN(policy.LongContextOutputMultiplier) || math.IsInf(policy.LongContextOutputMultiplier, 0) || policy.LongContextInputMultiplier < 0 || policy.LongContextOutputMultiplier < 0 {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 的长上下文配置无效", p.Model)
		}
		if policy.LongContextThreshold > 0 && (policy.LongContextInputMultiplier < 1 || policy.LongContextOutputMultiplier < 1) {
			return infraerrors.Newf(http.StatusBadRequest, "invalid_model_price", "模型 %s 启用长上下文策略后，输入和输出倍率必须至少为 1", p.Model)
		}
	}
	return nil
}

func (p AccountModelPrice) QuoteCurrency() string {
	if p.Currency == "" {
		return "USD"
	}
	return p.Currency
}

func (p AccountModelPrice) Candidate(exchangeRates ...string) (AccountModelPrice, error) {
	if err := ValidateAccountModelPrices([]AccountModelPrice{p}); err != nil {
		return p, err
	}
	d, _ := priceRat(p.Discount, "discount")
	m, _ := priceRat(p.Markup, "markup")
	currency := p.QuoteCurrency()
	var rate *big.Rat
	if len(exchangeRates) > 0 {
		if err := ValidatePlatformExchangeRate(exchangeRates[0]); err != nil {
			return p, err
		}
		rate, _ = new(big.Rat).SetString(exchangeRates[0])
		p.Conversion = &ModelPriceConversion{SourceCurrency: currency, USDToCNYRate: exchangeRates[0], SourceItems: append([]ModelPriceItem(nil), p.Items...)}
	} else if currency == "CNY" {
		return p, infraerrors.New(http.StatusServiceUnavailable, "model_price_exchange_rate_unavailable", "无法换算人民币报价，请读取平台统一汇率后重试")
	}
	p.Currency = "USD"
	p.Items = append([]ModelPriceItem(nil), p.Items...)
	for i := range p.Items {
		a, _ := priceRat(p.Items[i].Amount, "amount")
		a.Mul(a, d).Mul(a, m)
		if currency == "CNY" {
			a.Quo(a, rate)
		}
		p.Items[i].Unit = "USD/" + strings.SplitN(p.Items[i].Unit, "/", 2)[1]
		// Keep the exact product of three 12-decimal inputs; round FX division to 36 decimals.
		p.Items[i].Amount = strings.TrimRight(strings.TrimRight(a.FloatString(36), "0"), ".")
	}
	return p, nil
}

func (p *ModelBasePrice) Rate(kind, tier string) (float64, bool) {
	if p == nil {
		return 0, false
	}
	for _, item := range p.Price.Items {
		if item.Kind == kind && item.Tier == tier {
			n, ok := new(big.Rat).SetString(item.Amount)
			if !ok {
				return 0, false
			}
			if item.Unit == "USD/MTok" {
				n.Quo(n, big.NewRat(1000000, 1))
			}
			v, _ := n.Float64()
			return v, !math.IsInf(v, 0) && !math.IsNaN(v) && v >= 0
		}
	}
	return 0, false
}

func (p *ModelBasePrice) TokenPricing() *ModelPricing {
	r := &ModelPricing{CacheCreationPriceExplicit: true, ImageOutputPriceExplicit: true, ImageInputPriceExplicit: true}
	r.InputPricePerToken, _ = p.Rate("input", "")
	r.OutputPricePerToken, _ = p.Rate("output", "")
	r.CacheReadPricePerToken, _ = p.Rate("cache_read", "")
	r.CacheCreationPricePerToken, _ = p.Rate("cache_write", "")
	r.ImageInputPricePerToken, _ = p.Rate("image_input", "")
	r.ImageOutputPricePerToken, _ = p.Rate("image_output", "")
	r.InputPricePerTokenPriority, r.InputPriorityPriceExplicit = p.Rate("input", "priority")
	r.OutputPricePerTokenPriority, r.OutputPriorityPriceExplicit = p.Rate("output", "priority")
	r.CacheReadPricePerTokenPriority, r.CacheReadPriorityPriceExplicit = p.Rate("cache_read", "priority")
	r.CacheCreationPricePerTokenPriority, r.CacheCreationPriorityPriceExplicit = p.Rate("cache_write", "priority")
	var has5m, has1h bool
	r.CacheCreation5mPrice, has5m = p.Rate("cache_write", "5m")
	r.CacheCreation1hPrice, has1h = p.Rate("cache_write", "1h")
	r.SupportsCacheBreakdown = has5m && has1h
	r.CacheBreakdownPriceExplicit = r.SupportsCacheBreakdown
	r.LongContextInputThreshold = p.Price.Policy.LongContextThreshold
	r.LongContextInputMultiplier = p.Price.Policy.LongContextInputMultiplier
	r.LongContextOutputMultiplier = p.Price.Policy.LongContextOutputMultiplier
	return r
}

type basePriceSnapshotKey struct{}

func WithBasePriceSnapshot(ctx context.Context, price *ModelBasePrice) context.Context {
	return context.WithValue(ctx, basePriceSnapshotKey{}, price)
}

func BasePriceSnapshot(ctx context.Context) *ModelBasePrice {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(basePriceSnapshotKey{}).(*ModelBasePrice)
	return p
}

func (s *BillingService) BasePrice(ctx context.Context, model string) (*ModelBasePrice, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p := BasePriceSnapshot(ctx); p != nil {
		return p, nil
	}
	if s.basePrices == nil {
		return nil, ErrBasePriceUnavailable
	}
	p, err := s.basePrices.GetBase(ctx, model)
	if err != nil {
		return nil, ErrBasePriceUnavailable.WithCause(err)
	}
	if p == nil {
		return nil, ErrBasePriceMissing.WithMetadata(map[string]string{"model": model})
	}
	return p, nil
}

func (s *BillingService) BasePriceRepository() ModelBasePriceRepository { return s.basePrices }

func (s *BillingService) SetBasePriceRepository(repo ModelBasePriceRepository) { s.basePrices = repo }

func (s *BillingService) UsesManagedBasePrices() bool { return s != nil && s.basePrices != nil }

func (s *GatewayService) BillingService() *BillingService       { return s.billingService }
func (s *OpenAIGatewayService) BillingService() *BillingService { return s.billingService }

func VideoRequestBillingResolution(value string) string { return videoRequestResolution(value) }

func (s *BillingService) RememberBasePrice(ctx context.Context, keyID, userID int64, p *ModelBasePrice) (*ModelBasePrice, error) {
	requestID := resolveUsageBillingRequestID(ctx, "")
	snapshot, err := s.basePrices.RememberSnapshot(ctx, keyID, userID, requestID, p)
	if err != nil {
		return nil, ErrBasePriceUnavailable.WithCause(err)
	}
	if snapshot.Model != p.Model {
		return nil, ErrPriceRevisionConflict
	}
	return snapshot, nil
}

func (s *BillingService) CalculateBasePrice(ctx context.Context, model, tier string, tokens UsageTokens, count, seconds int, multiplier float64, serviceTier string, longContextEnabled ...bool) (*CostBreakdown, error) {
	p, err := s.BasePrice(ctx, model)
	if err != nil {
		return nil, err
	}
	if p.Price.Mode == BillingModeToken {
		if detailed := tokens.CacheCreation5mTokens + tokens.CacheCreation1hTokens; detailed > tokens.CacheCreationTokens {
			tokens.CacheCreationTokens = detailed
		}
		for _, check := range []struct {
			kind  string
			count int
		}{{"cache_read", tokens.CacheReadTokens}, {"cache_write", tokens.CacheCreationTokens}, {"image_input", tokens.ImageInputTokens}, {"image_output", tokens.ImageOutputTokens}} {
			if check.count > 0 {
				_, configured := p.Rate(check.kind, "")
				if normalizeBillingServiceTier(serviceTier) == "priority" {
					if _, priorityConfigured := p.Rate(check.kind, "priority"); priorityConfigured {
						configured = true
					}
				}
				if !configured && !(check.kind == "cache_write" && p.TokenPricing().SupportsCacheBreakdown) {
					return nil, missingPriceItem(model, check.kind)
				}
			}
		}
		applyLongContext := true
		if len(longContextEnabled) > 0 {
			applyLongContext = longContextEnabled[0]
		}
		cost := s.computeTokenBreakdown(p.TokenPricing(), tokens, multiplier, serviceTier, applyLongContext)
		cost.BillingMode = string(BillingModeToken)
		return cost, nil
	}
	kind := "request"
	if p.Price.Mode == BillingModeImage {
		kind = "image"
	}
	if p.Price.Mode == BillingModeVideo {
		kind = "video"
	}
	unit, ok := p.Rate(kind, tier)
	if !ok && p.Price.Mode == BillingModePerRequest {
		unit, ok = p.Rate(kind, "")
	}
	if !ok {
		return nil, missingPriceItem(model, tier)
	}
	if count <= 0 {
		count = 1
	}
	if multiplier < 0 {
		multiplier = 0
	}
	total := unit * float64(count)
	if p.Price.Mode == BillingModeVideo {
		if seconds <= 0 {
			return nil, missingPriceItem(model, "duration_seconds")
		}
		total *= float64(seconds)
	}
	mode := p.Price.Mode
	return &CostBreakdown{TotalCost: total, ActualCost: total * multiplier, BillingMode: string(mode)}, nil
}

func missingPriceItem(model, item string) error {
	return infraerrors.Newf(http.StatusServiceUnavailable, "model_price_item_not_configured", "模型 %s 未配置价格项目 %s，请检查模型基础价格", model, item)
}

func MarshalBasePrice(p *ModelBasePrice) json.RawMessage {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return b
}

func (p *ModelBasePrice) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s:v%d", p.Model, p.Version)
}
