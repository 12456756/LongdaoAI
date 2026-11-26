package admin

import (
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *ChannelHandler) pricingRepo(c *gin.Context) service.ModelBasePriceRepository {
	if h.billingService == nil || h.billingService.BasePriceRepository() == nil {
		response.ErrorFrom(c, service.ErrBasePriceUnavailable)
		return nil
	}
	return h.billingService.BasePriceRepository()
}

func respondPricingError(c *gin.Context, err error) {
	if infraerrors.Reason(err) == "" {
		err = service.ErrBasePriceUnavailable.WithCause(err)
	}
	response.ErrorFrom(c, err)
}

func (h *ChannelHandler) ModelPriceCatalog(c *gin.Context) {
	repo := h.pricingRepo(c)
	if repo == nil {
		return
	}
	result, err := repo.Catalog(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, service.ErrBasePriceUnavailable.WithCause(err))
		return
	}
	if c.Query("page") != "" {
		page, size := response.ParsePagination(c)
		result = filterPriceCatalog(result, c.Query("search"), c.Query("company_id"), c.Query("platform"), c.Query("mode"), c.Query("status"), page, size)
	}
	response.Success(c, result)
}

func (h *ChannelHandler) ModelPriceExchangeRate(c *gin.Context) {
	repo := h.pricingRepo(c)
	if repo == nil {
		return
	}
	rate, err := repo.ExchangeRate(c.Request.Context())
	if err != nil {
		respondPricingError(c, err)
		return
	}
	response.Success(c, gin.H{"usd_to_cny_rate": rate})
}

func filterPriceCatalog(catalog *service.ModelPriceCatalog, search, company, platform, mode, status string, page, size int) *service.ModelPriceCatalog {
	models := map[string]bool{}
	matches := map[string]bool{}
	modes := map[string]map[string]bool{}
	prices := map[int64]service.AccountModelPrice{}
	bases := map[string]service.ModelBasePrice{}
	for _, base := range catalog.Bases {
		models[base.Model] = true
		bases[base.Model] = base
	}
	for _, account := range catalog.Accounts {
		companyID := int64(0)
		if account.CompanyID != nil {
			companyID = *account.CompanyID
		}
		visible := (company == "" || company == strconv.FormatInt(companyID, 10)) && (platform == "" || platform == account.Platform)
		for _, model := range account.Models {
			models[model] = true
			if visible {
				matches[model] = true
			}
		}
		for _, price := range account.Prices {
			models[price.Model] = true
			prices[price.ID] = price
			if visible {
				matches[price.Model] = true
			}
			if modes[price.Model] == nil {
				modes[price.Model] = map[string]bool{}
			}
			modes[price.Model][string(price.Mode)] = true
		}
	}
	filtered := []string{}
	for model := range models {
		if !strings.Contains(strings.ToLower(model), strings.ToLower(strings.TrimSpace(search))) {
			continue
		}
		if (company != "" || platform != "") && !matches[model] {
			continue
		}
		if mode != "" && !modes[model][mode] {
			continue
		}
		base, priced := bases[model]
		source, exists := prices[base.SourcePriceID]
		changed := priced && (!exists || source.Revision != base.SourceRevision)
		if priced && exists && source.QuoteCurrency() == "CNY" {
			changed = changed || base.Price.Conversion == nil || base.Price.Conversion.USDToCNYRate != catalog.USDToCNYRate
		}
		if (status == "missing" && priced) || (status == "priced" && !priced) || (status == "changed" && !changed) {
			continue
		}
		filtered = append(filtered, model)
	}
	sort.Strings(filtered)
	pages := (len(filtered) + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * size
	end := start + size
	if end > len(filtered) {
		end = len(filtered)
	}
	pageModels := filtered[start:end]
	allowed := map[string]bool{}
	for _, model := range pageModels {
		allowed[model] = true
	}
	result := &service.ModelPriceCatalog{USDToCNYRate: catalog.USDToCNYRate, Accounts: make([]service.ModelPriceAccount, 0, len(catalog.Accounts)), Bases: []service.ModelBasePrice{}, Models: pageModels, Total: len(filtered), Page: page, PageSize: size}
	for _, account := range catalog.Accounts {
		account.Prices = append([]service.AccountModelPrice{}, account.Prices...)
		kept := account.Prices[:0]
		for _, price := range account.Prices {
			if allowed[price.Model] {
				kept = append(kept, price)
			}
		}
		account.Prices = kept
		result.Accounts = append(result.Accounts, account)
	}
	for _, base := range catalog.Bases {
		if allowed[base.Model] {
			result.Bases = append(result.Bases, base)
		}
	}
	return result
}

func (h *ChannelHandler) SelectModelBasePrice(c *gin.Context) {
	repo := h.pricingRepo(c)
	if repo == nil {
		return
	}
	var request struct {
		Model                string `json:"model" binding:"required"`
		SourcePriceID        int64  `json:"source_price_id" binding:"required,gt=0"`
		SourceRevision       int64  `json:"source_revision" binding:"required,gt=0"`
		ExpectedVersion      int64  `json:"expected_version" binding:"gte=0"`
		ExpectedExchangeRate string `json:"expected_exchange_rate"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "请选择有效的模型、来源账号价格及版本")
		return
	}
	actor, _ := middleware.GetAuthSubjectFromContext(c)
	result, err := repo.SelectBase(c.Request.Context(), request.Model, request.SourcePriceID, request.SourceRevision, request.ExpectedVersion, actor.UserID, request.ExpectedExchangeRate)
	if err != nil {
		respondPricingError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ChannelHandler) ModelBasePriceHistory(c *gin.Context) {
	repo := h.pricingRepo(c)
	if repo == nil {
		return
	}
	result, err := repo.History(c.Request.Context(), c.Query("model"))
	if err != nil {
		response.ErrorFrom(c, service.ErrBasePriceUnavailable.WithCause(err))
		return
	}
	response.Success(c, result)
}

func (h *ChannelHandler) UpstreamCompanies(c *gin.Context) {
	repo := h.pricingRepo(c)
	if repo == nil {
		return
	}
	if c.Request.Method == http.MethodGet {
		result, err := repo.Companies(c.Request.Context())
		if err != nil {
			response.ErrorFrom(c, service.ErrBasePriceUnavailable.WithCause(err))
			return
		}
		response.Success(c, result)
		return
	}
	if c.Request.Method == http.MethodDelete {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "公司编号无效")
			return
		}
		if err = repo.DeleteCompany(c.Request.Context(), id); err != nil {
			respondPricingError(c, err)
			return
		}
		response.Success(c, gin.H{"deleted": true})
		return
	}
	var company service.UpstreamCompany
	if err := c.ShouldBindJSON(&company); err != nil {
		response.BadRequest(c, "上游公司信息格式无效")
		return
	}
	result, err := repo.SaveCompany(c.Request.Context(), company)
	if err != nil {
		respondPricingError(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ChannelHandler) AccountModelPrices(c *gin.Context) {
	repo := h.pricingRepo(c)
	if repo == nil {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "账号编号无效")
		return
	}
	result, err := repo.AccountPrices(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, service.ErrBasePriceUnavailable.WithCause(err))
		return
	}
	rate, err := repo.ExchangeRate(c.Request.Context())
	if err != nil {
		respondPricingError(c, err)
		return
	}
	response.Success(c, gin.H{"prices": result, "version": service.AccountPricesVersion(result), "usd_to_cny_rate": rate})
}
