package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type imagePricingRequest struct {
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Size      string `json:"size"`
	Quality   string `json:"quality"`
	N         int    `json:"n"`
}

type imagePricingResponse struct {
	Object        string `json:"object"`
	SchemaVersion int    `json:"schema_version"`
	Currency      string `json:"currency"`
	imagePricingRequest
	BillingMode             service.BillingMode `json:"billing_mode"`
	PricingStatus           string              `json:"pricing_status"`
	PricingSource           string              `json:"pricing_source"`
	PricingAuthority        string              `json:"pricing_authority,omitempty"`
	AmountUSD               *string             `json:"amount_usd"`
	PricingVersion          string              `json:"pricing_version"`
	ObservedAt              time.Time           `json:"observed_at"`
	ExpiresAt               time.Time           `json:"expires_at"`
	EffectiveRateMultiplier float64             `json:"effective_rate_multiplier"`
}

func (h *GatewayHandler) imageBillingKey(c *gin.Context) *service.APIKey {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return nil
	}
	if h.cfg != nil && h.cfg.RunMode == config.RunModeSimple {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Billing information is unavailable")
		return nil
	}
	if key.GroupID == nil || key.Group == nil || *key.GroupID != key.Group.ID || key.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Image billing is unavailable for this key")
		return nil
	}
	return key
}

func (h *GatewayHandler) ImagePricing(c *gin.Context) {
	key := h.imageBillingKey(c)
	if key == nil {
		return
	}
	var request imagePricingRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Decode image pricing request (maximum 4096 bytes): "+errordetail.Text(err.Error()))
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Expected one JSON object")
		return
	}
	if request.Quality == "" {
		request.Quality = "medium"
	}
	_, validSize := service.ClassifyImageBillingTier(request.Size)
	if request.Model == "" || len(request.Model) > 200 || strings.TrimSpace(request.Model) != request.Model || !validSize || len(request.Size) > 32 || request.N < 1 || request.N > 4 ||
		(request.Operation != "generate" && request.Operation != "edit") ||
		(request.Quality != "low" && request.Quality != "medium" && request.Quality != "high") {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid model, operation, size, quality or n")
		return
	}
	if !service.GroupAllowsImageGeneration(key.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Image/video generation is disabled for this API key group")
		return
	}
	if h.gatewayService == nil {
		h.errorResponse(c, http.StatusForbidden, "api_error", "Platform model validation service is not initialized")
		return
	}
	if !h.gatewayService.ImageBillingModelAllowed(c.Request.Context(), key, request.Model) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", fmt.Sprintf("Model %q is not available for this API key group", request.Model))
		return
	}
	now := time.Now().UTC()
	quote, err := h.openAIGatewayService.QuoteImageBilling(c.Request.Context(), key, request.Model, request.Size, request.N, now)
	if err != nil || quote == nil || math.IsNaN(quote.Multiplier) || math.IsInf(quote.Multiplier, 0) {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", localDiagnosticMessage("Read image quote", err, "Quote is missing or its multiplier is not finite"))
		return
	}
	response := imagePricingResponse{Object: "sub2api.image_pricing", SchemaVersion: 1, Currency: "USD", imagePricingRequest: request,
		BillingMode: quote.Mode, PricingStatus: "unavailable", PricingSource: quote.Source, PricingAuthority: quote.PricingAuthority, AmountUSD: quote.Amount,
		ObservedAt: now, ExpiresAt: now.Add(time.Minute), EffectiveRateMultiplier: quote.Multiplier}
	if quote.Amount != nil {
		response.PricingStatus = "quoted"
	}
	// Observed/expiry times are excluded, making identical effective quotes stable.
	fingerprint, _ := json.Marshal(struct {
		Request    imagePricingRequest
		Mode       service.BillingMode
		Source     string
		Authority  string
		Amount     *string
		Multiplier float64
	}{request, quote.Mode, quote.Source, quote.PricingAuthority, quote.Amount, quote.Multiplier})
	sum := sha256.Sum256(fingerprint)
	response.PricingVersion = "v1:" + hex.EncodeToString(sum[:])
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, response)
}

type requestUsageResponse struct {
	Object        string  `json:"object"`
	SchemaVersion int     `json:"schema_version"`
	Currency      string  `json:"currency"`
	RequestID     string  `json:"request_id"`
	Status        string  `json:"status"`
	Model         string  `json:"model,omitempty"`
	ActualCostUSD *string `json:"actual_cost_usd"`
	ImageCount    *int    `json:"image_count,omitempty"`
	PendingReason string  `json:"pending_reason"`
}

func (h *GatewayHandler) RequestImageUsage(c *gin.Context) {
	key := h.imageBillingKey(c)
	if key == nil {
		return
	}
	id := strings.TrimSpace(c.Query("request_id"))
	if id == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "A valid request_id is required")
		return
	}
	evidence, err := h.openAIGatewayService.RequestImageBilling(c.Request.Context(), key, id)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", localDiagnosticMessage("Read image request billing", err, ""))
		return
	}
	response := requestUsageResponse{Object: "sub2api.request_usage", SchemaVersion: 1, Currency: "USD", RequestID: id,
		Status: "pending", PendingReason: "not_recorded_or_ambiguous"}
	if evidence != nil {
		response.Model, response.ImageCount = evidence.Model, &evidence.ImageCount
		response.PendingReason = "usage_recorded_billing_unconfirmed"
		if evidence.TransactionConfirmed {
			response.PendingReason = "transaction_confirmed_image_amount_unverified"
			if reliableImageBillingEvidence(evidence) {
				response.Status = "settled"
				response.ActualCostUSD = evidence.ActualCostUSD
				response.PendingReason = ""
			}
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, response)
}

func reliableImageBillingEvidence(evidence *service.RequestBillingEvidence) bool {
	if evidence == nil || !evidence.TransactionConfirmed || strings.TrimSpace(evidence.Model) == "" || evidence.ImageCount <= 0 || evidence.ActualCostUSD == nil {
		return false
	}
	if evidence.BillingMode != string(service.BillingModeImage) && evidence.BillingMode != string(service.BillingModePerRequest) {
		return false
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(*evidence.ActualCostUSD), 64)
	return err == nil && !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount >= 0
}
