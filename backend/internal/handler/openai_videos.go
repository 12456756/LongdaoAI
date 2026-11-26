package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		service.WriteVideoError(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if err := service.InitVideoContract(c); err != nil {
		service.WriteVideoFailure(c, err)
		return
	}
	h.gatewayService.BindVideoContract(c, apiKey)
	if apiKey.Group == nil || apiKey.Group.Platform != service.PlatformOpenAI {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		service.WriteVideoError(c, http.StatusNotFound, "not_found_error", "OpenAI Videos API is only available for OpenAI groups")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		service.WriteVideoError(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(c, "handler.openai_gateway.videos", zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", apiKey.ID))
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	ctx := c.Request.Context()
	ctx = service.WithVideoQuotaUpdater(ctx, h.apiKeyService)
	c.Request = c.Request.WithContext(ctx)
	create := c.Request.Method == http.MethodPost
	taskID := c.Param("request_id")
	content := strings.HasSuffix(c.Request.URL.Path, "/content")
	if c.Request.Method == http.MethodPut && c.Request.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(c.Request.Body, 1))
		if readErr != nil || len(body) > 0 {
			service.WriteVideoError(c, 400, "invalid_request_error", "Video cancellation does not accept a request body")
			return
		}
	}
	if !create && !validVideoTaskID(taskID) && c.Query("id_type") != "request_id" {
		service.WriteVideoError(c, http.StatusBadRequest, "invalid_request_error", "Invalid video task ID")
		return
	}
	if !create && apiKey.SEP != nil {
		var err error
		apiKey, _, err = apiKey.SEP.Service.RestoreTaskGroup(ctx, apiKey, taskID)
		if err != nil {
			h.errorResponse(c, 404, "not_found_error", "Video task not found for this enterprise")
			return
		}
	}
	var request *service.OpenAIVideoRequest
	var body []byte
	var err error
	requestModel := ""
	var channelMapping service.ChannelMappingResult
	if create {
		if !service.GroupAllowsImageGeneration(apiKey.Group) {
			service.WriteVideoError(c, http.StatusForbidden, "permission_error", "Media generation is not enabled for this group")
			return
		}
		mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if mediaType == "multipart/form-data" {
			if encoding := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding"))); encoding != "" && encoding != "identity" {
				service.WriteVideoError(c, http.StatusUnsupportedMediaType, "unsupported_encoding", "Compressed video uploads are not supported")
				return
			}
			if size := c.Request.ContentLength; size > service.VideoUploadMaxBytes {
				service.WriteVideoError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("Video multipart size %d bytes exceeds limit %d bytes", size, service.VideoUploadMaxBytes))
				return
			}
			release, slotErr := h.gatewayService.AcquireVideoUpload(ctx)
			if slotErr != nil {
				service.WriteVideoError(c, http.StatusTooManyRequests, "rate_limit_error", slotErr.Error())
				return
			}
			defer release()
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.VideoUploadMaxBytes)
		}
		if mediaType == "multipart/form-data" {
			request, err = service.ParseOpenAIVideoUpload(c.Request.Body, c.GetHeader("Content-Type"))
		} else {
			body, err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		}
		if err != nil {
			if maxErr, ok := extractMaxBytesError(err); ok {
				service.WriteVideoError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			} else {
				status, code, message := service.VideoRequestErrorDetails(err)
				service.WriteVideoError(c, status, code, message)
			}
			return
		}
		if request == nil {
			request, err = service.ParseOpenAIVideoRequest(body, c.GetHeader("Content-Type"))
		}
		if err != nil {
			service.WriteVideoError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		defer request.Close()
		requestModel = request.Model
		if done, replayErr := h.gatewayService.ReplayVideoCreate(ctx, c, apiKey, request); replayErr != nil {
			service.WriteVideoFailure(c, replayErr)
			return
		} else if done {
			return
		}
		channelMapping, _ = h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, requestModel)
	} else {
		resolved, done, resolveErr := h.gatewayService.ResolveVideoQuery(ctx, c, apiKey, taskID)
		if resolveErr != nil {
			service.WriteVideoFailure(c, resolveErr)
			return
		}
		if done {
			return
		}
		taskID = resolved
		h.gatewayService.SetVideoTaskContract(ctx, c, apiKey, taskID)
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
			service.WriteVideoError(c, status, code, message)
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
		var excluded map[int64]struct{}
		if request.UsesUploadedVideoReference() {
			excluded = make(map[int64]struct{})
		}
		for {
			selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapability(ctx, apiKey.GroupID, "", "", requestModel, excluded, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityVideos, false, false, false)
			if err != nil || selection == nil || selection.Account == nil || excluded == nil || selection.Account.VideoProtocol() == service.VideoProtocolLegacy {
				break
			}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if _, seen := excluded[selection.Account.ID]; seen {
				selection = nil
				break
			}
			excluded[selection.Account.ID] = struct{}{}
		}
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
				service.WriteVideoError(c, http.StatusNotFound, "not_found_error", "Video task not found")
			} else {
				service.WriteVideoError(c, http.StatusServiceUnavailable, "api_error", localDiagnosticMessage("Read original video task account", err, ""))
			}
		} else {
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestModel, requestModel, service.PlatformOpenAI)
			service.WriteVideoError(c, cls.Status, cls.ErrType, cls.Message)
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
		legacy := account.VideoProtocol() == service.VideoProtocolLegacy
		var err error
		if legacy {
			err = request.ValidateVideoReferences(service.VideoProtocolLegacy, c.GetHeader("X-Video-Operation"))
		} else {
			err = request.PrepareNativeVideoUpload()
		}
		if err != nil {
			status, code, message := service.VideoRequestErrorDetails(err)
			service.WriteVideoError(c, status, code, message)
			return
		}
		blocked := false
		moderate := func(moderationBody []byte) bool {
			if len(moderationBody) == 0 {
				return true
			}
			decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, requestModel, moderationBody)
			if decision != nil && decision.Blocked {
				blocked = true
				service.WriteVideoError(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
			}
			return !blocked
		}
		if h.contentModerationService != nil {
			if legacy {
				err = request.VisitModerationBodies(moderate)
			} else {
				moderate(request.ModerationBody())
			}
		}
		if err != nil {
			status, code, message := service.VideoRequestErrorDetails(err)
			service.WriteVideoError(c, status, code, message)
			return
		}
		if blocked {
			return
		}
		var verified bool
		frozenVideoBilling, verified = h.prepareVideoBilling(c, apiKey, account, request)
		if !verified {
			return
		}
		if frozenVideoBilling != nil {
			channelMapping = frozenVideoBilling.Mapping()
		} else if err := h.gatewayService.CheckVideoPricing(ctx, apiKey, account, request.Model, channelMapping.MappedModel, request.Resolution); err != nil {
			service.WriteVideoError(c, http.StatusServiceUnavailable, "video_pricing_not_configured", err.Error())
			return
		}
	}
	if create {
		ctx = service.WithVideoBillingInput(ctx, &service.OpenAIRecordUsageInput{APIKey: apiKey, User: apiKey.User, Account: account, Subscription: subscription, FrozenVideoBilling: frozenVideoBilling, RequestPayloadHash: request.PayloadHash(), InboundEndpoint: GetInboundEndpoint(c)})
		c.Request = c.Request.WithContext(ctx)
	}
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}
	result, err := h.gatewayService.ForwardVideo(ctx, c, account, apiKey, request, taskID, content, channelMapping.MappedModel)
	if err != nil {
		reqLog.Warn("openai_videos.forward_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		if !c.Writer.Written() {
			status, code, message := service.VideoRequestErrorDetails(err)
			if code == "invalid_request_error" {
				status, code, message = http.StatusBadGateway, "upstream_error", localDiagnosticMessage("Forward video request", err, "")
			}
			service.WriteVideoError(c, status, code, message)
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
	payloadHash := request.PayloadHash()
	h.submitOpenAIUsageRecordTask(ctx, result, func(taskCtx context.Context) {
		if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
			Result: result, APIKey: apiKey, User: apiKey.User, Account: account,
			Subscription: subscription, APIKeyService: h.apiKeyService,
			InboundEndpoint: inboundEndpoint, UpstreamEndpoint: service.OpenAIVideosEndpoint,
			UserAgent: userAgent, IPAddress: clientIP, QuotaPlatform: quotaPlatform,
			RequestPayloadHash: payloadHash,
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
