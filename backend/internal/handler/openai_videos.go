package handler

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Videos serves OpenAI-compatible video creation, status and content requests.
func (h *OpenAIGatewayHandler) Videos(c *gin.Context) {
	started := time.Now()
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if apiKey.Group == nil || apiKey.Group.Platform != service.PlatformOpenAI {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "OpenAI Videos API is only available for OpenAI groups")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(c, "handler.openai_gateway.videos", zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", apiKey.ID))
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	ctx := c.Request.Context()
	create := c.Request.Method == http.MethodPost
	taskID := c.Param("request_id")
	content := strings.HasSuffix(c.Request.URL.Path, "/content")
	if !create && !validVideoTaskID(taskID) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid video task ID")
		return
	}
	var request *service.OpenAIVideoRequest
	var body []byte
	var err error
	requestModel := ""
	var channelMapping service.ChannelMappingResult
	if create {
		if !service.GroupAllowsImageGeneration(apiKey.Group) {
			h.errorResponse(c, http.StatusForbidden, "permission_error", "Media generation is not enabled for this group")
			return
		}
		mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if mediaType == "multipart/form-data" {
			if encoding := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding"))); encoding != "" && encoding != "identity" {
				h.errorResponse(c, http.StatusUnsupportedMediaType, "unsupported_encoding", "Compressed video uploads are not supported")
				return
			}
			release, slotErr := h.gatewayService.AcquireVideoUpload(ctx)
			if slotErr != nil {
				h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Video upload concurrency limit reached")
				return
			}
			defer release()
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.VideoUploadMaxBytes)
		}
		body, err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil {
			if maxErr, ok := extractMaxBytesError(err); ok {
				h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			} else {
				h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
			}
			return
		}
		request, err = service.ParseOpenAIVideoRequest(body, c.GetHeader("Content-Type"))
		if err != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		requestModel = request.Model
		// Reuse the media moderation policy for prompts and reference images.
		if moderationBody := request.ModerationBody(); len(moderationBody) > 0 {
			decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, requestModel, moderationBody)
			if decision != nil && decision.Blocked {
				h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
				return
			}
		}
		channelMapping, _ = h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, requestModel)
	}
	setOpsRequestContext(c, requestModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))
	streamStarted := false
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if create {
		if err := h.billingCacheService.CheckBillingEligibility(ctx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(ctx, apiKey)); err != nil {
			status, code, message, retryAfter := billingErrorDetails(err)
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			h.errorResponse(c, status, code, message)
			return
		}
		mediaRelease, acquired := h.acquireImageGenerationSlot(c, false)
		if !acquired {
			return
		}
		if mediaRelease != nil {
			defer mediaRelease()
		}
	}
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(started).Milliseconds())
	var selection *service.AccountSelectionResult
	if create {
		selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapability(ctx, apiKey.GroupID, "", "", requestModel, nil, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityVideos, false, false, false)
	} else {
		var account *service.Account
		account, err = h.gatewayService.GetVideoTaskAccount(ctx, apiKey, taskID)
		if err == nil {
			selection = &service.AccountSelectionResult{Account: account, WaitPlan: &service.AccountWaitPlan{AccountID: account.ID, MaxConcurrency: account.Concurrency, MaxWaiting: 1, Timeout: 10 * time.Second}}
		}
	}
	if err != nil || selection == nil || selection.Account == nil {
		if !create {
			if errors.Is(err, service.ErrVideoTaskNotFound) {
				h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video task not found")
			} else {
				h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video task account is unavailable")
			}
		} else {
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestModel, requestModel, service.PlatformOpenAI)
			h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
		}
		return
	}
	account := selection.Account
	setOpsSelectedAccount(c, account.ID, account.Platform)
	accountRelease, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if accountRelease != nil {
		defer accountRelease()
	}
	var frozenVideoBilling *service.FrozenVideoBilling
	if create {
		var verified bool
		frozenVideoBilling, verified = h.prepareVideoBilling(c, apiKey, account, request)
		if !verified {
			return
		}
		if frozenVideoBilling != nil {
			channelMapping = frozenVideoBilling.Mapping()
		} else if err := h.gatewayService.CheckVideoPricing(ctx, apiKey, account, request.Model, channelMapping.MappedModel, request.Resolution); err != nil {
			h.errorResponse(c, http.StatusServiceUnavailable, "video_pricing_not_configured", err.Error())
			return
		}
	}
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}
	result, err := h.gatewayService.ForwardVideo(ctx, c, account, apiKey, request, taskID, content, channelMapping.MappedModel)
	if err != nil {
		reqLog.Warn("openai_videos.forward_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		if !c.Writer.Written() {
			h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Video upstream request failed")
		}
		return
	}
	if !create || result == nil {
		return
	}
	h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, result.UpstreamModel, true, nil)
	userAgent, clientIP := c.GetHeader("User-Agent"), ip.GetClientIP(c)
	inboundEndpoint := GetInboundEndpoint(c)
	quotaPlatform := service.QuotaPlatform(ctx, apiKey)
	h.submitOpenAIUsageRecordTask(ctx, result, func(taskCtx context.Context) {
		if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
			Result: result, APIKey: apiKey, User: apiKey.User, Account: account,
			Subscription: subscription, APIKeyService: h.apiKeyService,
			InboundEndpoint: inboundEndpoint, UpstreamEndpoint: service.OpenAIVideosEndpoint,
			UserAgent: userAgent, IPAddress: clientIP, QuotaPlatform: quotaPlatform,
			RequestPayloadHash: service.HashUsageRequestPayload(body),
			FrozenVideoBilling: frozenVideoBilling,
			ChannelUsageFields: channelMapping.ToUsageFields(requestModel, result.UpstreamModel),
		}); err != nil {
			reqLog.Error("openai_videos.record_usage_failed", zap.Error(err))
		}
	})
}

func validVideoTaskID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 256 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}
