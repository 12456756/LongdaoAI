package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type videoPricingRequest struct {
	Model           string `json:"model"`
	Operation       string `json:"operation"`
	Resolution      string `json:"resolution"`
	DurationSeconds int    `json:"duration_seconds"`
}

type videoPricingResponse struct {
	Diagnostic    string `json:"diagnostic,omitempty"`
	Object        string `json:"object"`
	SchemaVersion int    `json:"schema_version"`
	Currency      string `json:"currency"`
	videoPricingRequest
	BillingMode             service.BillingMode `json:"billing_mode"`
	PricingStatus           string              `json:"pricing_status"`
	PricingSource           string              `json:"pricing_source"`
	PricingAuthority        string              `json:"pricing_authority"`
	AmountUSD               *string             `json:"amount_usd"`
	PricingVersion          string              `json:"pricing_version"`
	EffectiveRateMultiplier float64             `json:"effective_rate_multiplier"`
	ObservedAt              time.Time           `json:"observed_at"`
	ExpiresAt               time.Time           `json:"expires_at"`
}

func (h *GatewayHandler) videoBillingKey(c *gin.Context) *service.APIKey {
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
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Video billing is unavailable for this key")
		return nil
	}
	return key
}

func validVideoPricingRequest(request videoPricingRequest) bool {
	return validVideoBillingModel(request.Model) && service.ValidVideoOperation(request.Operation) &&
		service.ValidVideoBillingResolution(request.Resolution) && service.ValidVideoPricingDuration(request.DurationSeconds) &&
		(request.DurationSeconds != service.VideoAutoDuration || request.Operation != service.VideoOperationText)
}

func validVideoBillingModel(model string) bool {
	return model != "" && len(model) <= 160 && !strings.ContainsFunc(model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}

func (h *GatewayHandler) VideoPricing(c *gin.Context) {
	key := h.videoBillingKey(c)
	if key == nil {
		return
	}
	var request videoPricingRequest
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Decode video pricing request (maximum 4096 bytes): "+errordetail.Text(err.Error()))
		return
	}
	if !validVideoPricingRequest(request) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Expected model (1-160 characters, no whitespace/control characters), operation (text-to-video/image-to-video/video-to-video), resolution (480p/720p/1080p/2k/4k), duration_seconds (1-600, or -1 for a reference request)")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Expected one JSON object")
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
	quote, err := h.openAIGatewayService.QuoteVideoBilling(c.Request.Context(), key, request.Model, request.Resolution, request.DurationSeconds, now)
	if err != nil || quote == nil || math.IsNaN(quote.Multiplier) || math.IsInf(quote.Multiplier, 0) || quote.Multiplier < 0 {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", localDiagnosticMessage("Read or version video quote", err, "Quote is missing or its multiplier is invalid"))
		return
	}
	if request.Operation == service.VideoOperationVideo && !service.ValidSeedanceReferenceDuration(quote.MappedModel(), request.DurationSeconds) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Seedance 2.0 reference-video duration must be -1 or 4 to 15 seconds")
		return
	}
	response := videoPricingResponse{
		Diagnostic: quote.Diagnostic,
		Object:     "sub2api.video_pricing", SchemaVersion: 1, Currency: "USD", videoPricingRequest: request,
		BillingMode: quote.Mode, PricingStatus: "unavailable", PricingSource: quote.Source,
		PricingAuthority: service.PricingAuthoritySub2APICustomerBilling, AmountUSD: quote.Amount,
		EffectiveRateMultiplier: quote.Multiplier, ObservedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if quote.Amount != nil {
		response.PricingStatus = "quoted"
	}
	response.PricingVersion, err = quote.Version(key, request.Model, request.Operation, request.Resolution, request.DurationSeconds)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video pricing is unavailable")
		return
	}
	c.Header("Cache-Control", "no-store")
	service.WriteMediaJSON(c, http.StatusOK, response)
}

type videoRequestUsageResponse struct {
	Object          string  `json:"object"`
	SchemaVersion   int     `json:"schema_version"`
	Currency        string  `json:"currency"`
	RequestID       string  `json:"request_id"`
	Status          string  `json:"status"`
	Model           *string `json:"model"`
	ActualCostUSD   *string `json:"actual_cost_usd"`
	VideoCount      *int    `json:"video_count"`
	DurationSeconds *int    `json:"duration_seconds"`
	Resolution      *string `json:"resolution"`
}

var videoBillingDecimal = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,12})?$`)
var videoBillingRequestID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,256}$`)
var videoPricingVersion = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// Legacy callers remain compatible. SEP opts into price protection with both
// headers; only this local, pre-dispatch path may emit the rejection marker.
func (h *OpenAIGatewayHandler) prepareVideoBilling(c *gin.Context, key *service.APIKey, account *service.Account, request *service.OpenAIVideoRequest) (*service.FrozenVideoBilling, bool) {
	message, code := "Expected exactly one valid X-Video-Pricing-Version and X-Video-Operation (text-to-video, image-to-video or video-to-video) header", "invalid_video_pricing_headers"
	versions, operations := c.Request.Header.Values("X-Video-Pricing-Version"), c.Request.Header.Values("X-Video-Operation")
	if len(versions) == 0 && len(operations) == 0 {
		if request.Seconds == service.VideoAutoDuration {
			frozen, err := h.gatewayService.PrepareVideoBilling(c.Request.Context(), key, request, request.VideoBillingOperation(), "")
			if err != nil || !frozen.MatchesAccount(account) {
				h.errorResponse(c, http.StatusServiceUnavailable, "video_pricing_not_configured", localDiagnosticMessage("Verify automatic-duration video price", err, "Selected account does not match the configured pricing"))
				return nil, false
			}
			return frozen, true
		}
		return nil, true
	}
	if len(versions) == 1 && len(operations) == 1 && videoPricingVersion.MatchString(versions[0]) &&
		service.ValidVideoOperation(operations[0]) {
		frozen, err := h.gatewayService.PrepareVideoBilling(c.Request.Context(), key, request, operations[0], versions[0])
		if err == nil && frozen.MatchesAccount(account) {
			return frozen, true
		}
		if err != nil {
			message = errordetail.Text(err.Error())
			code = "video_pricing_verification_failed"
			if errors.Is(err, service.ErrVideoPricingChanged) {
				code = "video_pricing_changed"
			}
		} else {
			message, code = "Selected upstream account model mapping does not match the quoted video model", "video_pricing_account_mismatch"
		}
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Video-Dispatch", "rejected")
	service.WriteVideoError(c, http.StatusConflict, code, message)
	return nil, false
}

func (h *GatewayHandler) RequestVideoUsage(c *gin.Context) {
	key := h.videoBillingKey(c)
	if key == nil {
		return
	}
	ids := c.QueryArray("request_id")
	if len(ids) != 1 || !videoBillingRequestID.MatchString(ids[0]) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "A valid request_id is required")
		return
	}
	id := ids[0]
	outcome, outcomeErr := h.openAIGatewayService.VideoRequestOutcome(c.Request.Context(), key, id)
	if outcomeErr != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "video_billing_unavailable", localDiagnosticMessage("Read video billing outcome", outcomeErr, ""))
		return
	}
	if outcome != nil {
		var count *int
		if outcome.Status == "settled" {
			value := 1
			count = &value
		} else if outcome.Status == "not_charged" {
			value := 0
			count = &value
		}
		response := videoRequestUsageResponse{Object: "sub2api.video_request_usage", SchemaVersion: 2, Currency: "USD", RequestID: id, Status: outcome.Status, ActualCostUSD: outcome.ActualCostUSD, VideoCount: count}
		if validVideoBillingModel(outcome.Model) {
			response.Model = &outcome.Model
		}
		if outcome.Seconds > 0 && outcome.Seconds <= 600 {
			response.DurationSeconds = &outcome.Seconds
		}
		if service.ValidVideoBillingResolution(outcome.Resolution) {
			response.Resolution = &outcome.Resolution
		}
		c.Header("Cache-Control", "no-store")
		service.WriteMediaJSON(c, 200, response)
		return
	}
	evidence, err := h.openAIGatewayService.RequestVideoBilling(c.Request.Context(), key, id)
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", localDiagnosticMessage("Read video request billing", err, ""))
		return
	}
	response := videoRequestUsageResponse{Object: "sub2api.video_request_usage", SchemaVersion: 2, Currency: "USD", RequestID: id, Status: "unknown"}
	if evidence != nil {
		// Preserve the strict nullable SEP wire shape even for incomplete or
		// malformed legacy metadata; invalid evidence never confirms a charge.
		if evidence.Model != nil && validVideoBillingModel(*evidence.Model) {
			response.Model = evidence.Model
		}
		if evidence.VideoCount != nil && *evidence.VideoCount >= 0 {
			response.VideoCount = evidence.VideoCount
		}
		if evidence.DurationSeconds != nil && *evidence.DurationSeconds > 0 && *evidence.DurationSeconds <= 600 {
			response.DurationSeconds = evidence.DurationSeconds
		}
		if evidence.Resolution != nil && service.ValidVideoBillingResolution(*evidence.Resolution) {
			response.Resolution = evidence.Resolution
		}
		if reliableVideoBillingEvidence(evidence) {
			response.Status, response.ActualCostUSD = "settled", evidence.ActualCostUSD
		}
	}
	if evidence == nil {
		receipt, err := h.openAIGatewayService.MediaFailure(c.Request.Context(), key, "video", id)
		if err != nil {
			h.errorResponse(c, 503, "video_receipt_unavailable", localDiagnosticMessage("Read saved video failure", err, ""))
			return
		}
		if receipt != nil {
			receipt["operation"] = "usage"
			receipt["object"], receipt["currency"] = "sub2api.video_request_usage", "USD"
			receipt["status"], receipt["actual_cost_usd"] = "not_charged", "0"
			receipt["error"], receipt["operation_error"] = nil, nil
			service.WriteMediaJSON(c, 200, receipt)
			return
		}
	}
	c.Header("Cache-Control", "no-store")
	service.WriteMediaJSON(c, http.StatusOK, response)
}

func reliableVideoBillingEvidence(evidence *service.VideoRequestBillingEvidence) bool {
	if evidence == nil || !evidence.TransactionConfirmed || evidence.BillingMode != string(service.BillingModeVideo) ||
		evidence.Model == nil || !validVideoBillingModel(*evidence.Model) || evidence.ActualCostUSD == nil ||
		evidence.VideoCount == nil || *evidence.VideoCount <= 0 || evidence.DurationSeconds == nil ||
		!service.ValidVideoPricingDuration(*evidence.DurationSeconds) ||
		evidence.Resolution == nil || !service.ValidVideoBillingResolution(*evidence.Resolution) {
		return false
	}
	if !videoBillingDecimal.MatchString(*evidence.ActualCostUSD) {
		return false
	}
	amount, err := strconv.ParseFloat(*evidence.ActualCostUSD, 64)
	return err == nil && !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount >= 0
}
