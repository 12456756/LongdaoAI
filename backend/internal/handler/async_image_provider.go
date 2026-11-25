package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func validAsyncImageIdentity(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_.:-", r)) {
			return false
		}
	}
	return true
}

func asyncImageIdentity(c *gin.Context) (string, string, error) {
	id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	if values := c.Request.Header.Values("X-Client-Request-ID"); len(values) > 1 {
		return "", "", errors.New("Only one X-Client-Request-ID is allowed")
	} else if len(values) == 1 {
		id = values[0]
	}
	if !validAsyncImageIdentity(id) {
		return "", "", errors.New("X-Client-Request-ID must contain 1..256 letters, digits or _.:-")
	}
	idem := id
	if values := c.Request.Header.Values("Idempotency-Key"); len(values) > 1 {
		return "", "", errors.New("Only one Idempotency-Key is allowed")
	} else if len(values) == 1 {
		idem = values[0]
	}
	if !validAsyncImageIdentity(idem) {
		return "", "", errors.New("Idempotency-Key must contain 1..256 letters, digits or _.:-")
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, id))
	c.Header("X-Client-Request-ID", id)
	return id, idem, nil
}

func (h *AsyncImageHandler) tryProviderSubmit(c *gin.Context, key *service.APIKey, platform string, body []byte) bool {
	if platform != service.PlatformOpenAI || h.openAI == nil || h.openAI.gatewayService == nil {
		return false
	}
	gateway := h.openAI.gatewayService
	configured, err := gateway.AsyncImageProtocolConfigured(c.Request.Context(), key.GroupID, service.AsyncImageModelHint(c, body))
	if err != nil {
		service.WriteAsyncImageFailure(c, err, nil)
		return true
	}
	if !configured {
		return false
	}
	parsed, err := gateway.ParseAsyncImageRequest(c, body)
	if err != nil {
		service.WriteAsyncImageFailure(c, err, nil)
		return true
	}
	if h.tasks == nil {
		service.WriteAsyncImageFailure(c, errors.New("Image task storage is unavailable"), nil)
		return true
	}
	requestID, idem, err := asyncImageIdentity(c)
	if err != nil {
		imageTaskJSONError(c, 400, "invalid_request_error", err.Error())
		return true
	}
	maxBytes := int64(0)
	if h.openAI.cfg != nil {
		maxBytes = h.openAI.cfg.Gateway.MaxBodySize
	}
	semanticPayload, err := service.BuildAsyncImagePayload(parsed, parsed.Model, maxBytes)
	if err != nil {
		service.WriteAsyncImageFailure(c, err, nil)
		return true
	}
	if task, found, replayErr := gateway.ReplayAsyncImage(c.Request.Context(), h.tasks, key, parsed, semanticPayload, requestID, idem, h.openAI.apiKeyService); found {
		if replayErr != nil {
			service.WriteAsyncImageFailure(c, replayErr, task)
		} else {
			writeAsyncImageAccepted(c, task)
		}
		return true
	}
	selection, selectErr := gateway.SelectAccountForAsyncImages(c.Request.Context(), key.GroupID, parsed.Model)
	if selectErr != nil || selection == nil || selection.Account == nil {
		service.WriteAsyncImageFailure(c, fmt.Errorf("Select configured Naoshengyun asynchronous image account for model %q: %v", parsed.Model, selectErr), nil)
		return true
	}
	// Selection may already own a slot. Release it on every pre-dispatch exit.
	release := selection.ReleaseFunc
	defer func() {
		if release != nil {
			release()
		}
	}()
	account := selection.Account
	mapping, _ := gateway.ResolveChannelMappingAndRestrict(c.Request.Context(), key.GroupID, parsed.Model)
	model := parsed.Model
	if mapping.MappedModel != "" {
		model = mapping.MappedModel
	}
	model = account.GetMappedModel(model)
	payload, err := service.BuildAsyncImagePayload(parsed, model, maxBytes)
	if err != nil {
		service.WriteAsyncImageFailure(c, err, nil)
		return true
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		service.WriteAsyncImageFailure(c, errors.New("Image request user context is missing"), nil)
		return true
	}
	log := requestLogger(c, "handler.image_async")
	if decision := h.openAI.checkContentModeration(c, log, key, subject, service.ContentModerationProtocolOpenAIImages, parsed.Model, parsed.ModerationBody()); decision != nil && decision.Blocked {
		imageTaskJSONError(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return true
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if h.openAI.billingCacheService == nil {
		service.WriteAsyncImageFailure(c, errors.New("Image billing eligibility service is unavailable"), nil)
		return true
	}
	if err = h.openAI.billingCacheService.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, subscription, service.QuotaPlatform(c.Request.Context(), key)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", fmt.Sprint(retryAfter))
		}
		imageTaskJSONError(c, status, code, message)
		return true
	}
	billing, err := gateway.PrepareAsyncImageBilling(c.Request.Context(), key, parsed, mapping, subscription)
	if err != nil {
		service.WriteAsyncImageFailure(c, err, nil)
		return true
	}
	streamStarted := false
	userRelease, ok := h.openAI.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, log)
	if !ok {
		return true
	}
	if userRelease != nil {
		defer userRelease()
	}
	mediaRelease, ok := h.openAI.acquireImageGenerationSlot(c, false)
	if !ok {
		return true
	}
	if mediaRelease != nil {
		defer mediaRelease()
	}
	// acquireResponsesAccountSlot consumes the scheduler's already-acquired slot.
	release = nil
	accountRelease, ok := h.openAI.acquireResponsesAccountSlot(c, key.GroupID, "", selection, false, &streamStarted, log)
	if !ok {
		return true
	}
	if accountRelease != nil {
		defer accountRelease()
	}
	setOpsRequestContext(c, parsed.Model, false)
	setOpsSelectedAccount(c, account.ID, account.Platform)
	task, err := gateway.SubmitAsyncImage(c.Request.Context(), c, h.tasks, key, account, parsed, payload, requestID, idem, billing, h.openAI.apiKeyService)
	if err != nil {
		service.WriteAsyncImageFailure(c, err, task)
		return true
	}
	writeAsyncImageAccepted(c, task)
	return true
}

func writeAsyncImageAccepted(c *gin.Context, task *service.ImageTask) {
	service.SetImageReceipt(c, task)
	c.Header("X-Client-Request-ID", task.RequestID)
	pollURL := imageTaskPollURL(c.Request.URL.Path, task.ID)
	c.Header("Location", pollURL)
	c.Header("Retry-After", "3")
	c.Header("Cache-Control", "no-store")
	service.WriteMediaJSON(c, 202, gin.H{"schema_version": 2, "id": task.ID, "task_id": task.TaskID, "object": task.Object, "request_id": task.RequestID, "submission_status": task.SubmissionStatus, "status": task.Status, "upstream_status": task.UpstreamStatus, "created_at": task.CreatedAt, "expires_at": task.ExpiresAt, "poll_url": pollURL, "billing": task.Billing})
}

func (h *AsyncImageHandler) tryProviderRead(c *gin.Context, key *service.APIKey, content bool) bool {
	kind := "task_id"
	if values, exists := c.Request.URL.Query()["id_type"]; exists {
		if len(values) != 1 || (values[0] != "task_id" && values[0] != "request_id") {
			imageTaskJSONError(c, 400, "invalid_request_error", "Expected exactly one id_type: task_id or request_id")
			return true
		}
		kind = values[0]
	}
	id := c.Param("task_id")
	if kind == "request_id" && (content || !validAsyncImageIdentity(id)) {
		imageTaskJSONError(c, 400, "invalid_request_error", "request_id lookup is only supported for status queries with 1..256 letters, digits or _.:-")
		return true
	}
	if h.openAI == nil || h.openAI.gatewayService == nil || h.tasks == nil {
		if kind == "request_id" {
			service.WriteAsyncImageFailure(c, errors.New("Image submission lookup service is unavailable"), nil)
			return true
		}
		return false
	}
	if kind == "task_id" && (len(id) > 200 || !strings.HasPrefix(id, "imgtask_")) {
		imageTaskError(c, service.ErrImageTaskNotFound)
		return true
	}
	var record *service.ImageTaskRecord
	var err error
	if kind == "request_id" {
		record, err = h.tasks.FindAsyncClaim(c.Request.Context(), key, id, "")
		if err == nil && record == nil {
			err = service.ErrImageTaskNotFound
		}
	} else {
		record, err = h.tasks.LoadRecord(c.Request.Context(), service.ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}, id)
	}
	if err != nil {
		if errors.Is(err, service.ErrImageTaskNotFound) {
			if kind == "request_id" {
				receipt, lookupErr := h.openAI.gatewayService.MediaFailure(c.Request.Context(), key, "image", id)
				if lookupErr != nil {
					service.WriteAsyncImageFailure(c, lookupErr, nil)
					return true
				}
				if receipt != nil {
					receipt["operation_error"], receipt["error"] = nil, nil
					service.WriteMediaJSON(c, 200, receipt)
					return true
				}
				imageTaskJSONError(c, 404, "image_request_not_found", "Image submission record does not exist or has expired; this does not establish whether an upstream task or charge exists")
			} else {
				imageTaskError(c, err)
			}
		} else {
			service.WriteAsyncImageFailure(c, err, nil)
		}
		return true
	}
	if record.Provider == nil {
		if kind == "request_id" {
			service.WriteAsyncImageFailure(c, errors.New("Image submission index references a task without provider metadata"), nil)
			return true
		}
		return false
	}
	id = record.ID
	c.Header("X-Image-Request-ID", record.Provider.RequestID)
	task := service.PublicAsyncImage(record)
	gateway := h.openAI.gatewayService
	task.Billing = gateway.AsyncImageBillingStatus(c.Request.Context(), key, record)
	service.SetImageReceipt(c, task)
	var account *service.Account
	if record.Provider.SubmissionStatus == "accepted" {
		account, err = gateway.AsyncImageTaskAccount(c.Request.Context(), key, record)
		if err != nil {
			service.WriteAsyncImageFailure(c, err, task)
			return true
		}
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		service.WriteAsyncImageFailure(c, errors.New("Image query user context is missing"), task)
		return true
	}
	log := requestLogger(c, "handler.image_async.query")
	streamStarted := false
	userRelease, ok := h.openAI.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, log)
	if !ok {
		return true
	}
	if userRelease != nil {
		defer userRelease()
	}
	if account != nil {
		selection := &service.AccountSelectionResult{Account: account, WaitPlan: &service.AccountWaitPlan{AccountID: account.ID, MaxConcurrency: account.Concurrency, MaxWaiting: 1, Timeout: 10 * time.Second}}
		accountRelease, ok := h.openAI.acquireResponsesAccountSlot(c, key.GroupID, "", selection, false, &streamStarted, log)
		if !ok {
			return true
		}
		if accountRelease != nil {
			defer accountRelease()
		}
	}
	task, err = gateway.PollAsyncImage(c.Request.Context(), c, h.tasks, key, record, account, h.openAI.apiKeyService)
	if err != nil {
		service.WriteAsyncImageFailure(c, err, task)
		return true
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Image-Request-ID", task.RequestID)
	service.SetImageReceipt(c, task)
	if !content {
		if task.Status == service.ImageTaskStatusProcessing {
			c.Header("Retry-After", "3")
		}
		service.WriteMediaJSON(c, http.StatusOK, task)
		return true
	}
	record, err = h.tasks.LoadRecord(c.Request.Context(), service.ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}, id)
	if err == nil {
		c.Header("X-Image-Billing-Status", task.Billing.Status)
		err = gateway.DownloadAsyncImage(c.Request.Context(), c, record)
	}
	if err != nil {
		service.WriteAsyncImageFailure(c, err, task)
	}
	return true
}

func (h *AsyncImageHandler) Content(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key == nil {
		imageTaskError(c, service.ErrImageTaskForbidden)
		return
	}
	if !h.tryProviderRead(c, key, true) {
		imageTaskJSONError(c, 404, "image_content_not_found", "Task does not use the asynchronous image download protocol")
	}
}
