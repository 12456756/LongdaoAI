//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func requireCompleteMediaReceipt(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &receipt), recorder.Body.String())
	for _, field := range []string{"schema_version", "request_id", "task_id", "operation", "submission_status", "status", "is_final", "status_observed_at", "status_source", "task_error", "error", "operation_error", "billing", "submission_retry"} {
		require.Contains(t, receipt, field)
	}
	for _, field := range []string{"status", "actual_cost_usd", "error"} {
		require.Contains(t, receipt["billing"], field)
	}
	require.EqualValues(t, 2, receipt["schema_version"])
	return receipt
}

func TestMediaReceiptEarlyHTTPFailuresAndUnauthenticatedIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct{ method, path, operation string }{
		{"POST", "/v1/images/edits", "create"}, {"POST", "/images/generations/async", "create"},
		{"POST", "/v1/videos", "create"}, {"GET", "/videos/task/content", "download"},
		{"PUT", "/v1/videos/task/cancel", "cancel"}, {"GET", "/v1/images/tasks/task", "query"},
		{"POST", "/v1/sub2api/images/pricing", "pricing"}, {"GET", "/v1/sub2api/videos/usage", "usage"},
	} {
		for _, status := range []int{400, 401, 403, 409, 413, 422, 429, 500, 502, 503, 504} {
			t.Run(test.path+http.StatusText(status), func(t *testing.T) {
				router := gin.New()
				router.Use(middleware2.ClientRequestID(), (*OpenAIGatewayHandler)(nil).MediaReceiptMiddleware())
				router.Handle(test.method, test.path, func(c *gin.Context) {
					middleware2.AbortWithError(c, status, "precise_error", "Reference resolution not supported")
				})
				request := httptest.NewRequest(test.method, test.path, nil)
				request.Header.Set("X-Client-Request-ID", "sep-original-id")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, request)
				require.Equal(t, status, w.Code)
				receipt := requireCompleteMediaReceipt(t, w)
				require.Equal(t, "sep-original-id", receipt["request_id"])
				require.Equal(t, test.operation, receipt["operation"])
				require.Nil(t, receipt["task_id"])
				require.Equal(t, "Reference resolution not supported", receipt["error"].(map[string]any)["message"])
				if test.operation == "create" {
					require.Equal(t, "not_submitted", receipt["submission_status"])
					require.Equal(t, "not_charged", receipt["billing"].(map[string]any)["status"])
				} else if test.operation != "pricing" {
					require.Equal(t, "unknown", receipt["billing"].(map[string]any)["status"])
				}
			})
		}
	}
}

type mediaReceiptUpstream struct {
	service.HTTPUpstream
	calls  int
	body   string
	status int
}

func (u *mediaReceiptUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"mock-provider-id"}}, Body: io.NopCloser(strings.NewReader(u.body))}, nil
}

func TestMediaReceiptSynchronousProviderFailureUsageRecoveryAndKeyIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	groupID := int64(3)
	key := &service.APIKey{ID: 7, UserID: 11, User: &service.User{ID: 11}, GroupID: &groupID, Group: &service.Group{ID: 3, Platform: service.PlatformOpenAI, AllowImageGeneration: true}}
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{{ID: 5, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example"}}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	upstream := &mediaReceiptUpstream{status: 503, body: `{"error":{"message":"No available accounts for image generation","type":"api_error","code":"no_accounts","param":"model","details":{"pool":"empty","api_key":"sk-fixture-secret"}}}`}
	usage := &imageUsageBillingRepo{}
	gateway := service.NewOpenAIGatewayService(accounts, nil, usage, nil, nil, nil, repository.NewGatewayCache(client), cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, service.NewDeferredService(accounts, nil, time.Minute), nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	reader := &GatewayHandler{openAIGatewayService: gateway}
	router := gin.New()
	router.Use(middleware2.ClientRequestID(), h.MediaReceiptMiddleware(), func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 11})
		c.Next()
	}, h.MediaRequestReplayMiddleware())
	router.POST("/v1/images/generations", h.Images)
	router.GET("/v1/sub2api/usage", reader.RequestImageUsage)
	recovery := NewAsyncImageHandler(service.NewImageTaskService(repository.NewImageTaskStore(client)), h)
	router.GET("/v1/images/tasks/:task_id", recovery.Get)
	perform := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{"model":"gpt-image-2-1k","prompt":"sea","size":"1024x1024","n":1}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Client-Request-ID", "sep-create-id")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	failed := perform("POST", "/v1/images/generations")
	require.Equal(t, 502, failed.Code, failed.Body.String())
	receipt := requireCompleteMediaReceipt(t, failed)
	require.Equal(t, "rejected", receipt["submission_status"])
	require.Equal(t, "not_charged", receipt["billing"].(map[string]any)["status"])
	require.Contains(t, failed.Body.String(), "No available accounts for image generation")
	require.Contains(t, failed.Body.String(), "mock-provider-id")
	require.NotContains(t, failed.Body.String(), "sk-fixture-secret")
	require.Equal(t, 1, upstream.calls)
	query := perform("GET", "/v1/sub2api/usage?request_id=sep-create-id")
	require.Equal(t, 200, query.Code, query.Body.String())
	require.Equal(t, "not_charged", requireCompleteMediaReceipt(t, query)["status"])
	recovered := perform("GET", "/v1/images/tasks/sep-create-id?id_type=request_id")
	require.Equal(t, 200, recovered.Code, recovered.Body.String())
	recoveredReceipt := requireCompleteMediaReceipt(t, recovered)
	require.Equal(t, "rejected", recoveredReceipt["submission_status"])
	require.Nil(t, recoveredReceipt["operation_error"])
	require.Contains(t, recovered.Body.String(), "No available accounts for image generation")
	replay := perform("POST", "/v1/images/generations")
	require.Equal(t, 502, replay.Code)
	require.Equal(t, 1, upstream.calls)
	// Existing debit evidence wins over a failure cached under a reused ID.
	amount := "0.5"
	usage.evidence = &service.RequestBillingEvidence{Model: "gpt-image-2-1k", BillingMode: "image", ImageCount: 1, ActualCostUSD: &amount, TransactionConfirmed: true}
	require.Equal(t, "settled", requireCompleteMediaReceipt(t, perform("GET", "/v1/sub2api/usage?request_id=sep-create-id"))["status"])
	usage.evidence = nil
	key.ID = 8
	require.Equal(t, "pending", requireCompleteMediaReceipt(t, perform("GET", "/v1/sub2api/usage?request_id=sep-create-id"))["status"])
	_, err := gateway.MediaFailure(context.Background(), key, "image", "missing")
	require.NoError(t, err)
	key.ID = 7
	server.FastForward(7*24*time.Hour + time.Second)
	require.Equal(t, "pending", requireCompleteMediaReceipt(t, perform("GET", "/v1/sub2api/usage?request_id=sep-create-id"))["status"])
}

func TestMediaReceiptQueryAndDownloadFailuresPreserveAcceptedState(t *testing.T) {
	for _, path := range []string{"/v1/images/tasks/imgtask_known", "/v1/images/tasks/imgtask_known/content"} {
		r := gin.New()
		r.Use(middleware2.ClientRequestID(), (*OpenAIGatewayHandler)(nil).MediaReceiptMiddleware())
		r.GET(path, func(c *gin.Context) {
			service.SetImageReceipt(c, &service.ImageTask{TaskID: "imgtask_known", RequestID: "create-original", SubmissionStatus: "accepted", Status: "completed", Billing: &service.ImageTaskBillingStatus{Status: "settled", ActualCostUSD: videoContractPtr("0.1")}})
			c.JSON(503, gin.H{"error": gin.H{"message": "Download storage unavailable", "source": "upstream", "code": "storage_busy"}})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		out := requireCompleteMediaReceipt(t, w)
		require.Equal(t, "completed", out["status"])
		require.Equal(t, "settled", out["billing"].(map[string]any)["status"])
		require.Equal(t, "create-original", out["request_id"])
	}
}

func TestMediaReceiptBinaryAndSSEAreNotBufferedOrCorrupted(t *testing.T) {
	for _, mime := range []string{"video/mp4", "text/event-stream"} {
		t.Run(mime, func(t *testing.T) {
			r := gin.New()
			r.Use(middleware2.ClientRequestID(), (*OpenAIGatewayHandler)(nil).MediaReceiptMiddleware())
			r.GET("/v1/videos/task/content", func(c *gin.Context) {
				c.Header("Content-Type", mime)
				_, _ = c.Writer.Write([]byte("binary-or-event"))
				c.Writer.Flush()
				_, _ = c.Writer.Write([]byte("-tail"))
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/videos/task/content", nil))
			require.Equal(t, "binary-or-event-tail", w.Body.String())
			require.Equal(t, mime, w.Header().Get("Content-Type"))
		})
	}
}

func TestMediaReceiptSynchronousSuccessPreservesImageAndReportsPendingSettlement(t *testing.T) {
	r := gin.New()
	r.Use(middleware2.ClientRequestID(), (*OpenAIGatewayHandler)(nil).MediaReceiptMiddleware())
	r.POST("/v1/images/generations", func(c *gin.Context) {
		service.SetSynchronousImageSuccess(c)
		service.WriteMediaRawJSON(c, 200, []byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U=","revised_prompt":"sea"}]}`))
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/images/generations", nil))
	out := requireCompleteMediaReceipt(t, w)
	require.Equal(t, "completed", out["status"])
	require.Equal(t, "pending", out["billing"].(map[string]any)["status"])
	require.Equal(t, "aW1hZ2U=", out["data"].([]any)[0].(map[string]any)["b64_json"])
}

func TestMediaReceiptReferenceUploadDoesNotShareCreateFailureCache(t *testing.T) {
	_, key, _, _, _, async := newAsyncImageHTTPFixture(t)
	h := async.openAI
	router := gin.New()
	router.Use(middleware2.ClientRequestID(), h.MediaReceiptMiddleware(), func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Next()
	}, h.MediaRequestReplayMiddleware())
	router.POST("/v1/videos/references", h.UploadVideoReference)
	creates := 0
	router.POST("/v1/videos", func(c *gin.Context) {
		creates++
		service.WriteVideoError(c, 422, "unsupported_duration", "Requested duration is not supported")
	})
	perform := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Client-Request-ID", "shared-upload-create-id")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	upload := perform("/v1/videos/references")
	require.Equal(t, 400, upload.Code)
	receipt := requireCompleteMediaReceipt(t, upload)
	require.Equal(t, "upload", receipt["operation"])
	require.Equal(t, "not_applicable", receipt["submission_status"])
	require.Nil(t, receipt["submission_error"])
	saved, err := h.gatewayService.MediaFailure(context.Background(), key, "video", "shared-upload-create-id")
	require.NoError(t, err)
	require.Nil(t, saved, "Failed uploads must not persist a video create rejection")
	created := perform("/v1/videos")
	require.Equal(t, 422, created.Code, created.Body.String())
	require.Equal(t, 1, creates, "Creation must reach the handler despite the prior upload failure")
	// A saved create rejection must also not replace a later upload response.
	upload = perform("/v1/videos/references")
	require.Equal(t, 400, upload.Code)
	require.Contains(t, upload.Body.String(), "invalid_video_reference_upload")
	require.Equal(t, "upload", requireCompleteMediaReceipt(t, upload)["operation"])
	created = perform("/v1/videos")
	require.Equal(t, 422, created.Code)
	require.Equal(t, 1, creates, "Existing create replay behavior must remain intact")
}

func TestMediaReceiptErrorAfterJSONKeepaliveAndSSEHeaders(t *testing.T) {
	for _, sse := range []bool{false, true} {
		r := gin.New()
		h := &OpenAIGatewayHandler{}
		r.Use(middleware2.ClientRequestID(), h.MediaReceiptMiddleware())
		r.POST("/v1/images/generations", func(c *gin.Context) {
			if sse {
				c.Header("Content-Type", "text/event-stream")
			} else {
				c.Header("Content-Type", "application/json")
			}
			_, _ = c.Writer.WriteString(" \n")
			c.Writer.Flush()
			if sse {
				h.handleStreamingAwareError(c, 502, "image_upstream_error", "GPU worker disconnected", true)
			} else {
				service.WriteMediaJSON(c, 502, gin.H{"error": gin.H{"source": "upstream", "code": "worker_disconnected", "message": "GPU worker disconnected"}})
			}
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/images/generations", nil))
		require.Equal(t, 200, w.Code)
		if sse {
			require.Contains(t, w.Body.String(), "event: error")
			data := strings.TrimSpace(strings.SplitN(w.Body.String(), "data: ", 2)[1])
			var out map[string]any
			require.NoError(t, json.Unmarshal([]byte(data), &out))
			require.Contains(t, out, "billing")
			require.Contains(t, out, "operation_error")
		} else {
			requireCompleteMediaReceipt(t, w)
		}
		require.Contains(t, w.Body.String(), "GPU worker disconnected")
	}
}

func TestSEPQuoteRejectionRecoveryRetainsOriginalIdentityReasonAndZeroFee(t *testing.T) {
	_, key, upstream, _, _, h := newAsyncImageHTTPFixture(t)
	router := gin.New()
	router.Use(middleware2.ClientRequestID(), h.openAI.MediaReceiptMiddleware())
	rejections := 0
	router.POST("/v1/images/edits/async", func(c *gin.Context) {
		rejections++
		middleware2.SetOpsFallbackAPIKey(c, key)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "platform-trace"))
		// The service supplies this rejection-only identity after enterprise
		// authentication. It does not grant the handler permission to generate.
		rejectedKey := *key
		rejectedKey.SEP = &service.SEPExecution{Service: service.NewSEPService(nil, nil, nil, &service.APIKeyService{}, nil, nil)}
		failure := &service.SEPRejectedMediaError{Key: &rejectedKey, Cause: service.SEPFailure(409, "QUOTE_MISMATCH", "Image operation differs from quote", "quote")}
		middleware2.WriteSEPError(c, failure, "receive")
		_, authenticated := middleware2.GetAPIKeyFromContext(c)
		require.False(t, authenticated)
	})
	router.GET("/v1/images/tasks/:task_id", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID})
		h.Get(c)
	})
	perform := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Client-Request-ID", "sep-original")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	failed := perform("POST", "/v1/images/edits/async")
	require.Equal(t, 409, failed.Code, failed.Body.String())
	receipt := requireCompleteMediaReceipt(t, failed)
	require.Equal(t, "sep-original", receipt["request_id"])
	require.Equal(t, "platform-trace", failed.Header().Get("X-Request-ID"))
	require.Equal(t, "not_submitted", receipt["submission_status"])
	require.Equal(t, "not_charged", receipt["billing"].(map[string]any)["status"])
	for i := 0; i < 2; i++ {
		query := perform("GET", "/v1/images/tasks/sep-original?id_type=request_id")
		require.Equal(t, 200, query.Code, query.Body.String())
		recovered := requireCompleteMediaReceipt(t, query)
		require.Equal(t, "sep-original", recovered["request_id"])
		require.Equal(t, "not_submitted", recovered["submission_status"])
		require.Equal(t, "not_charged", recovered["billing"].(map[string]any)["status"])
		require.Equal(t, "0", recovered["billing"].(map[string]any)["actual_cost_usd"])
		require.Equal(t, "QUOTE_MISMATCH", recovered["submission_error"].(map[string]any)["code"])
		require.Nil(t, recovered["operation_error"])
		require.EqualValues(t, 409, recovered["http_status"])
	}
	key.ID++
	otherKey := perform("GET", "/v1/images/tasks/sep-original?id_type=request_id")
	require.Equal(t, 404, otherKey.Code)
	require.NotContains(t, otherKey.Body.String(), "QUOTE_MISMATCH")
	key.ID--
	require.Empty(t, upstream.calls)
	require.Equal(t, 1, rejections)
}

func TestSEPMediaAuthFailureDoesNotPersistFromLoggingFallbackKey(t *testing.T) {
	_, key, upstream, _, _, h := newAsyncImageHTTPFixture(t)
	router := gin.New()
	router.Use(middleware2.ClientRequestID(), h.openAI.MediaReceiptMiddleware())
	router.POST("/v1/images/edits/async", func(c *gin.Context) {
		middleware2.SetOpsFallbackAPIKey(c, key)
		middleware2.WriteSEPError(c, service.SEPFailure(401, "INVALID_SERVICE_SIGNATURE", "Service signature is invalid", "authenticate"), "receive")
	})
	r := httptest.NewRequest("POST", "/v1/images/edits/async", nil)
	r.Header.Set("X-Client-Request-ID", "unverified-original")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	require.Equal(t, 401, w.Code)
	require.Equal(t, "unverified-original", requireCompleteMediaReceipt(t, w)["request_id"])
	saved, err := h.openAI.gatewayService.MediaFailure(t.Context(), key, "image", "unverified-original")
	require.NoError(t, err)
	require.Nil(t, saved)
	require.Empty(t, upstream.calls)
}
