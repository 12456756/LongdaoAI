package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
)

const OpenAIVideosEndpoint = "/v1/videos"

const openAIVideoTaskTTL = 7 * 24 * time.Hour

var ErrVideoTaskNotFound = errors.New("video task not found")

// OpenAIVideoRequest retains provider-specific fields and uploaded reference images.
type OpenAIVideoRequest struct {
	Body                  []byte
	ContentType           string
	Multipart             bool
	Model                 string
	Prompt                string
	Seconds               int
	Resolution            string
	tempDir               string
	bodyPath              string
	bodyHash              string
	uploadPaths           []string
	spooledReferenceCount int
	wireBytes             int64
	legacyImages          []videoReferenceInput
	referenceContent      []byte
	legacyReferences      []videoReferenceContent
	referenceOperation    string
	uploadedReference     bool
	legacyValidated       bool
	fields                map[string]string
	otherFiles            int
	moderation            GrokMediaRequestInfo
}

func ParseOpenAIVideoRequest(body []byte, contentType string) (*OpenAIVideoRequest, error) {
	r := &OpenAIVideoRequest{Body: body, ContentType: contentType}
	fields := make(map[string]string)
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil && contentType != "" {
		return nil, fmt.Errorf("invalid Content-Type")
	}
	switch mediaType {
	case "", "application/json":
		r.moderation = ParseGrokMediaRequest(contentType, body)
		if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
			return nil, fmt.Errorf("request body must be a JSON object")
		}
		if content := gjson.GetBytes(body, "metadata.content"); content.Exists() {
			r.referenceContent = []byte(content.Raw)
			for _, item := range content.Array() {
				switch item.Get("type").String() {
				case "video_url":
					r.referenceOperation = VideoOperationVideo
					r.uploadedReference = r.uploadedReference || item.Get("video_url.reference_id").Exists()
				case "image_url":
					if r.referenceOperation == "" {
						r.referenceOperation = VideoOperationImage
					}
				}
			}
		}
		for _, name := range []string{"model", "prompt", "seconds", "duration", "resolution", "size"} {
			value := gjson.GetBytes(body, name)
			if (name == "model" || name == "prompt") && value.Exists() && value.Type != gjson.String {
				return nil, fmt.Errorf("%s must be a string", name)
			}
			fields[name] = value.String()
		}
		for _, name := range []string{"input_reference", "image_url"} {
			if value := gjson.GetBytes(body, name); value.Type == gjson.String && value.String() != "" {
				r.moderation.InputImageURLs = append(r.moderation.InputImageURLs, value.String())
			}
		}
		r.ContentType = "application/json"
	case "multipart/form-data":
		if params["boundary"] == "" {
			return nil, fmt.Errorf("multipart boundary is required")
		}
		r.Multipart = true
		r.moderation.N = 1
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("invalid multipart body")
			}
			if part.FormName() == "input_reference" && part.FileName() != "" {
				data, err := io.ReadAll(part)
				if err != nil {
					return nil, fmt.Errorf("invalid input_reference upload")
				}
				r.moderation.Uploads = append(r.moderation.Uploads, OpenAIImagesUpload{FieldName: part.FormName(), FileName: part.FileName(), ContentType: part.Header.Get("Content-Type"), Data: data})
			} else if part.FileName() == "" {
				value, err := io.ReadAll(io.LimitReader(part, 1<<20))
				if err != nil {
					return nil, fmt.Errorf("invalid multipart field")
				}
				name := part.FormName()
				if _, exists := fields[name]; exists {
					return nil, fmt.Errorf("duplicate multipart field: %s", name)
				}
				fields[name] = string(value)
			} else {
				r.otherFiles++
				if name := part.FormName(); name == "mask" || name == "image" || strings.HasPrefix(name, "image[") {
					data, err := io.ReadAll(part)
					if err != nil {
						return nil, fmt.Errorf("invalid image upload")
					}
					upload := OpenAIImagesUpload{FieldName: name, FileName: part.FileName(), ContentType: part.Header.Get("Content-Type"), Data: data}
					if name == "mask" {
						r.moderation.MaskUpload = &upload
					} else {
						r.moderation.Uploads = append(r.moderation.Uploads, upload)
					}
				}
			}
			_ = part.Close()
		}
		for _, name := range []string{"image", "image_url"} {
			if value := strings.TrimSpace(fields[name]); value != "" {
				r.moderation.InputImageURLs = append(r.moderation.InputImageURLs, value)
			}
		}
		r.moderation.MaskImageURL = firstNonEmptyString(fields["mask"], fields["mask_image_url"])
	default:
		return nil, fmt.Errorf("Content-Type must be application/json or multipart/form-data")
	}
	return finishOpenAIVideoRequest(r, fields)
}

func finishOpenAIVideoRequest(r *OpenAIVideoRequest, fields map[string]string) (*OpenAIVideoRequest, error) {
	r.Model = strings.TrimSpace(fields["model"])
	r.fields = fields
	r.Prompt = strings.TrimSpace(fields["prompt"])
	r.moderation.Model = r.Model
	r.moderation.Prompt = r.Prompt
	if r.Model == "" || (r.Prompt == "" && len(r.referenceContent) == 0) {
		return nil, fmt.Errorf("model and prompt are required")
	}
	seconds := firstNonEmptyString(fields["seconds"], fields["duration"])
	if seconds == "" && r.referenceOperation != "" {
		r.Seconds = 5
	} else if seconds != "" {
		var err error
		r.Seconds, err = strconv.Atoi(seconds)
		if err != nil || (r.Seconds <= 0 && (r.referenceOperation == "" || r.Seconds != VideoAutoDuration)) {
			return nil, fmt.Errorf("seconds or duration must be a positive integer")
		}
	}
	r.Resolution = videoRequestResolution(firstNonEmptyString(fields["resolution"], fields["size"]))
	return r, nil
}

func (r *OpenAIVideoRequest) ModerationBody() []byte {
	return r.moderation.ModerationBody()
}

func (r *OpenAIVideoRequest) UsesUploadedVideoReference() bool {
	return r.uploadedReference
}

// Unknown providers have no reliable default rate card; require administrator pricing.
func (s *OpenAIGatewayService) CheckVideoPricing(ctx context.Context, apiKey *APIKey, account *Account, model, mappedModel, resolution string) error {
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil
	}
	if s.billingService.UsesManagedBasePrices() {
		p, err := s.billingService.BasePrice(ctx, model)
		if err != nil {
			return err
		}
		if p.Price.Mode == BillingModeToken {
			return nil
		}
		kind := "video"
		tier := resolution
		if p.Price.Mode == BillingModePerRequest {
			kind = "request"
		}
		if _, ok := p.Rate(kind, tier); ok {
			return nil
		}
		if kind == "request" {
			if _, ok := p.Rate(kind, ""); ok {
				return nil
			}
		}
		return missingPriceItem(model, resolution)
	}
	model = resolveOpenAIForwardModel(account, model, mappedModel)
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil
	}
	if apiKeyHasConfiguredVideoPrice(apiKey, resolution) || isGrokVideoBillingModel(model) || s.resolveOpenAIChannelPricing(ctx, model, apiKey) != nil {
		return nil
	}
	if fresh := s.apiKeyWithFreshGroupMediaPricing(ctx, apiKey); apiKeyHasConfiguredVideoPrice(fresh, resolution) {
		return nil
	}
	return errors.New("Video pricing is not configured; configure the group's per-second video price or channel model pricing")
}

func videoRequestResolution(value string) string {
	if dimensions := strings.Split(strings.ToLower(value), "x"); len(dimensions) == 2 {
		w, _ := strconv.Atoi(dimensions[0])
		h, _ := strconv.Atoi(dimensions[1])
		if w > 0 && h > 0 {
			value = strconv.Itoa(min(w, h)) + "p"
		}
	}
	if strings.TrimSpace(value) == "" {
		return VideoBillingResolution720P
	}
	return NormalizeVideoBillingResolutionOrDefault(value)
}

// Task bindings are scoped to the API key and group, and are never rebalanced.
func videoTaskCacheKey(apiKeyID int64, taskID string) string {
	return fmt.Sprintf("openai-video:%d:%s", apiKeyID, DeriveSessionHashFromSeed(taskID))
}

func (s *OpenAIGatewayService) GetVideoTaskAccount(ctx context.Context, apiKey *APIKey, taskID string) (*Account, error) {
	if apiKey != nil && apiKey.SEP != nil {
		_, id, err := apiKey.SEP.Service.RestoreTaskGroup(ctx, apiKey, taskID)
		if err != nil && !errors.Is(err, ErrVideoTaskNotFound) {
			return nil, err
		}
		if id > 0 {
			return s.accountRepo.GetByID(ctx, id)
		}
	}
	if s.cache == nil || apiKey == nil {
		return nil, errors.New("video task storage is unavailable")
	}
	accountID, err := s.cache.GetSessionAccountID(ctx, derefGroupID(apiKey.GroupID), videoTaskCacheKey(apiKey.ID, taskID))
	if errors.Is(err, redis.Nil) {
		return nil, ErrVideoTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read video task binding: %w", err)
	}
	if accountID <= 0 {
		return nil, ErrVideoTaskNotFound
	}
	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("read video task account: %w", err)
	}
	if account == nil || !account.IsOpenAI() || account.Type != AccountTypeAPIKey || account.Status != StatusActive ||
		!account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityVideos) || !s.openAIAccountMatchesSchedulingGroup(account, apiKey.GroupID) {
		return nil, ErrVideoTaskNotFound
	}
	return account, nil
}

func buildOpenAIVideoURL(base, taskID string, content bool) string {
	// An explicitly configured creation endpoint is also a valid base for polling.
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err == nil {
		for _, suffix := range []string{"/v1/video/generations", "/video/generations", "/videos"} {
			if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), suffix) {
				parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), suffix)
				parsed.RawPath = ""
				base = parsed.String()
				break
			}
		}
	}
	target := buildOpenAIEndpointURL(base, OpenAIVideosEndpoint)
	if taskID != "" {
		parsed, _ := url.Parse(target)
		parsed.Path += "/" + taskID
		if content {
			parsed.Path += "/content"
		}
		target = parsed.String()
	}
	return target
}

// ForwardVideo forwards the native video protocol without chat/responses conversion.
// A nil request retrieves an existing task or streams its content.
func (s *OpenAIGatewayService) ForwardVideo(ctx context.Context, c *gin.Context, account *Account, apiKey *APIKey, request *OpenAIVideoRequest, taskID string, content bool, mappedModel string) (*OpenAIForwardResult, error) {
	start := time.Now()
	if account == nil || !account.IsOpenAI() || account.Type != AccountTypeAPIKey {
		return nil, errors.New("video requests require an OpenAI API key account")
	}
	if request != nil && (s.cache == nil || apiKey == nil) {
		return nil, errors.New("video task storage is unavailable")
	}
	protocol := account.VideoProtocol()
	if request == nil {
		meta, metaErr := s.videoTaskMetadata(ctx, apiKey, taskID)
		if metaErr == nil {
			if meta.AccountID != account.ID {
				return nil, errors.New("video task account mismatch")
			}
			protocol = meta.Protocol
		} else if !errors.Is(metaErr, redis.Nil) {
			if _, available := s.cache.(VideoStateStore); available {
				return nil, metaErr
			}
		} else if protocol == VideoProtocolLegacy || strings.HasPrefix(taskID, "mtask_") {
			return nil, errors.New("video task metadata is missing")
		}
	}
	if protocol == VideoProtocolLegacy {
		return s.forwardLegacyVideo(ctx, c, account, apiKey, request, taskID, content, mappedModel)
	}
	if protocol != VideoProtocolOpenAI {
		return nil, errors.New("unsupported video protocol")
	}
	if c.Request.Method == http.MethodPut {
		return nil, videoError(422, "unsupported_video_operation", "Video cancellation is only implemented for the naoshengyun_legacy protocol")
	}
	if request != nil && request.UsesUploadedVideoReference() {
		return nil, videoError(400, "invalid_video_reference", "Uploaded reference videos require a Seedance account using the Naoshengyun legacy protocol.")
	}
	base, err := s.validateUpstreamBaseURL(account.GetOpenAIBaseURL())
	if err != nil {
		return nil, fmt.Errorf("invalid base_url: %w", err)
	}
	target := buildOpenAIVideoURL(base, taskID, content)
	method, originalModel, billingModel := http.MethodGet, "", ""
	var body []byte
	contentType := ""
	if request != nil {
		method, originalModel = http.MethodPost, request.Model
		billingModel = resolveOpenAIForwardModel(account, originalModel, mappedModel)
		body, contentType = request.Body, request.ContentType
		if billingModel != originalModel {
			if request.Multipart {
				body, contentType, err = rewriteOpenAIImagesMultipartModel(body, contentType, billingModel)
				if err != nil {
					return nil, err
				}
			} else {
				body = ReplaceModelInBody(body, billingModel)
			}
		}
	}
	upstreamReq, err := http.NewRequestWithContext(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI), method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Preserve download variants and range requests, but never forward client auth.
	query := upstreamReq.URL.Query()
	for key, values := range c.Request.URL.Query() {
		if key == "id_type" {
			continue
		}
		query[key] = values
	}
	upstreamReq.URL.RawQuery = query.Encode()
	if account.GetOpenAIApiKey() == "" {
		return nil, errors.New("video account is missing api_key")
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+account.GetOpenAIApiKey())
	upstreamReq.Header.Set("Accept", "application/json")
	if request != nil {
		upstreamReq.Header.Set("Content-Type", contentType)
		if value := c.GetHeader("Idempotency-Key"); value != "" {
			upstreamReq.Header.Set("Idempotency-Key", value)
		}
	}
	if content {
		upstreamReq.Header.Set("Accept", "*/*")
		for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
			if value := c.GetHeader(name); value != "" {
				upstreamReq.Header.Set(name, value)
			}
		}
	}
	if ua := account.GetOpenAIUserAgent(); ua != "" {
		upstreamReq.Header.Set("User-Agent", ua)
	}
	account.ApplyHeaderOverrides(upstreamReq.Header)
	SetActualOpenAIUpstreamEndpoint(c, OpenAIVideosEndpoint)
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	if request != nil {
		contractFor(c).SubmissionStatus = "failed"
		zero := "0"
		contractFor(c).Billing = VideoBillingStatus{Status: "not_charged", ActualCostUSD: &zero}
	}
	MarkMediaDispatched(c)
	resp, err := s.httpUpstream.Do(upstreamReq, proxy, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(start).Milliseconds())
	if err != nil {
		// Do not retry an ambiguous POST: the provider may already have accepted it.
		return nil, fmt.Errorf("video upstream request failed: %s", sanitizeUpstreamErrorMessage(err.Error()))
	}
	defer resp.Body.Close()
	// These are relay-owned billing/correlation signals, even when operators
	// allow additional upstream headers. Never let a provider forge rejection.
	for name := range resp.Header {
		if strings.EqualFold(name, "X-Video-Dispatch") || strings.EqualFold(name, "X-Client-Request-ID") || strings.EqualFold(name, "X-Video-Request-ID") || strings.EqualFold(name, "X-Video-Billing-Status") {
			delete(resp.Header, name)
		}
	}
	if resp.StatusCode >= 400 {
		responseBody := s.readUpstreamErrorBody(resp)
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody, billingModel)
		CaptureUpstreamError(c, resp.StatusCode, resp.Header, responseBody)
		setOpsUpstreamError(c, resp.StatusCode, extractUpstreamErrorMessage(responseBody), string(responseBody))
		UseUpstreamError(c)
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		if request != nil {
			contractFor(c).SubmissionStatus = "rejected"
		}
		WriteVideoFailure(c, &videoProtocolError{status: resp.StatusCode, code: "video_upstream_error", message: errordetail.Message(responseBody), diagnostic: map[string]any(GatewayErrorFields(c, "video_upstream_error", errordetail.Message(responseBody)))})
		return nil, fmt.Errorf("video upstream returned status %d", resp.StatusCode)
	}
	if content {
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Cache-Control"} {
			if value := resp.Header.Get(name); value != "" {
				c.Header(name, value)
			}
		}
		c.Status(resp.StatusCode)
		c.Writer.WriteHeaderNow()
		_, err = io.Copy(c.Writer, resp.Body)
		return nil, err
	}
	responseBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if request == nil {
		if responseID := firstNonEmptyString(gjson.GetBytes(responseBody, "id").String(), gjson.GetBytes(responseBody, "data.id").String()); responseID != "" && responseID != taskID {
			return nil, videoError(502, "video_protocol_error", "Video query response task ID does not match the requested task; its status cannot be used for billing")
		}
	}
	if request == nil && apiKey != nil && apiKey.SEP != nil {
		if err = apiKey.SEP.Service.ObserveVideo(ctx, apiKey, taskID, responseBody); err != nil {
			contractFor(c).Billing = billingProblem(err)
		} else {
			contractFor(c).Billing = apiKey.SEP.Service.videoTaskBilling(ctx, apiKey, taskID)
		}
	}
	result := &OpenAIForwardResult{RequestID: firstNonEmptyString(resp.Header.Get("x-request-id"), resp.Header.Get("x-oneapi-request-id")), Model: originalModel, BillingModel: billingModel, UpstreamModel: billingModel, Duration: time.Since(start)}
	if request != nil {
		result.ResponseID = firstNonEmptyString(gjson.GetBytes(responseBody, "id").String(), gjson.GetBytes(responseBody, "request_id").String(), gjson.GetBytes(responseBody, "data.id").String())
		if result.ResponseID == "" {
			if UpstreamBodyError(responseBody) {
				CaptureUpstreamError(c, resp.StatusCode, resp.Header, responseBody)
				setOpsUpstreamError(c, resp.StatusCode, extractUpstreamErrorMessage(responseBody), string(responseBody))
				writeUpstreamError(c, 502, "video_upstream_error", extractUpstreamErrorMessage(responseBody), false)
				return nil, errors.New(extractUpstreamErrorMessage(responseBody))
			}
			return nil, errors.New("Video create response does not contain id, request_id, or data.id")
		}
		bindCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := s.cache.SetSessionAccountID(bindCtx, derefGroupID(apiKey.GroupID), videoTaskCacheKey(apiKey.ID, result.ResponseID), account.ID, openAIVideoTaskTTL)
		cancel()
		if err != nil {
			// Preserve the accepted task ID even if Redis is temporarily unavailable.
			logger.LegacyPrintf("service.openai_gateway", "bind video task account failed: account_id=%d task_id=%s err=%v", account.ID, result.ResponseID, err)
		}
		result.VideoCount = 1
		v := contractFor(c)
		v.SubmissionStatus, v.TaskID = "accepted", &result.ResponseID
		v.Billing = VideoBillingStatus{Status: "pending"}
		MarkMediaBillable(c)
		observedStatus := gjson.GetBytes(responseBody, "status").String()
		if observedStatus != "" {
			observed := time.Now().UnixMilli()
			source := "upstream"
			final := observedStatus == "completed" || observedStatus == "failed" || observedStatus == "cancelled"
			v.Status, v.IsFinal, v.StatusObservedAt, v.StatusSource = &observedStatus, &final, &observed, &source
		}
		if apiKey.SEP != nil {
			if err = apiKey.SEP.Service.RecordExecutionProvider(ctx, apiKey, result.RequestID, result.ResponseID); err != nil {
				logger.LegacyPrintf("service.openai_gateway", "save SEP video association failed: task_id=%s err=%v", result.ResponseID, safeVideoError(err))
			}
		}
		if store, storeErr := s.videoStore(); storeErr == nil {
			now := time.Now().Unix()
			meta := &VideoTaskMetadata{SchemaVersion: 1, TaskID: result.ResponseID, RequestID: v.RequestID, APIKeyID: apiKey.ID, GroupID: derefGroupID(apiKey.GroupID), AccountID: account.ID, Protocol: VideoProtocolOpenAI, RequestedModel: originalModel, UpstreamModel: billingModel, BillingRequestID: resolveUsageBillingRequestID(ctx, result.RequestID), BoundAt: now, ExpiresAt: now + 604800}
			metadataCtx, metadataCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := store.VideoBind(metadataCtx, meta); err != nil {
				logger.LegacyPrintf("service.openai_gateway", "bind native video metadata failed: account_id=%d task_id=%s", account.ID, result.ResponseID)
			}
			metadataCancel()
		}
		result.VideoResolution = request.Resolution
		result.VideoDurationSeconds = request.Seconds
		if seconds := firstPositiveGJSONInt(gjson.GetBytes(responseBody, "seconds"), gjson.GetBytes(responseBody, "duration")); seconds > 0 {
			result.VideoDurationSeconds = seconds
		}
		if size := firstNonEmptyString(gjson.GetBytes(responseBody, "resolution").String(), gjson.GetBytes(responseBody, "size").String()); size != "" {
			result.VideoResolution = videoRequestResolution(size)
		}
		result.Usage = extractOpenAIEmbeddingsUsage(responseBody)
	}
	var payload map[string]any
	if json.Unmarshal(responseBody, &payload) != nil || payload == nil {
		return nil, errors.New("Video upstream returned a response that is not a JSON object")
	}
	state := strings.ToLower(strings.TrimSpace(gjson.GetBytes(responseBody, "status").String()))
	if state == "" {
		state = strings.ToLower(strings.TrimSpace(gjson.GetBytes(responseBody, "data.status").String()))
	}
	switch state {
	case "canceled":
		state = "cancelled"
	case "succeeded", "success":
		state = "completed"
	}
	if state != "" {
		payload["status"] = state
	}
	if request == nil && UpstreamBodyError(responseBody) && state != "failed" && state != "cancelled" {
		CaptureUpstreamError(c, resp.StatusCode, resp.Header, responseBody)
		UseUpstreamError(c)
		WriteVideoFailure(c, &videoProtocolError{status: 502, code: "video_upstream_error", message: errordetail.Message(responseBody), diagnostic: map[string]any(GatewayErrorFields(c, "video_upstream_error", errordetail.Message(responseBody)))})
		return nil, errors.New(errordetail.Message(responseBody))
	}
	if value, ok := payload["status"].(string); ok && value != "" {
		v := contractFor(c)
		observed, source := time.Now().UnixMilli(), "upstream"
		final := value == "completed" || value == "failed" || value == "cancelled"
		v.Status, v.IsFinal, v.StatusObservedAt, v.StatusSource = &value, &final, &observed, &source
		if value == "failed" {
			payload["error"] = errordetail.Value(payload["error"])
			v.TaskError, _ = payload["error"].(map[string]any)
		}
	}
	// Provider payloads cannot override relay-owned task identity or billing.
	encodedContract, _ := json.Marshal(contractFor(c))
	_ = json.Unmarshal(encodedContract, &payload)
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	WriteMediaJSON(c, resp.StatusCode, payload)
	return result, nil
}

func (s *OpenAIGatewayService) RefreshSEPVideo(ctx context.Context, key *APIKey, taskID string) error {
	var err error
	key, _, err = key.SEP.Service.RestoreTaskGroup(ctx, key, taskID)
	if err != nil {
		return err
	}
	account, err := s.GetVideoTaskAccount(ctx, key, taskID)
	if err != nil {
		return err
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request, _ = http.NewRequestWithContext(ctx, http.MethodGet, "/v1/videos/"+taskID, nil)
	_, err = s.ForwardVideo(ctx, c, account, key, nil, taskID, false, "")
	return err
}
