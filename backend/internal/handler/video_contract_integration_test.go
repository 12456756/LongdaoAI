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
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type videoContractHTTPUpstream struct {
	service.HTTPUpstream
	methods          []string
	queryFailure     bool
	createDisconnect bool
}

func (u *videoContractHTTPUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.methods = append(u.methods, r.Method+" "+r.URL.Path)
	status, message, state, final := 200, "success", "processing", false
	if r.Method == http.MethodPost {
		if u.createDisconnect {
			return nil, io.ErrUnexpectedEOF
		}
		status = 202
		message = "accepted"
		state = "queued"
	}
	if r.Method == http.MethodPut {
		state = "canceled"
		final = true
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			panic("cancel request must have no body")
		}
	}
	body, _ := json.Marshal(map[string]any{"code": 0, "message": message, "data": map[string]any{"id": "mtask_00000000000000000000000000", "model_id": "seedance-2.0", "status": state, "is_final": final, "created_at": 12345}})
	if u.queryFailure && r.Method == http.MethodGet {
		status = 503
		body = []byte(`{"code":503,"message":"Video status backend temporarily unavailable","reason":"status_backend_unavailable"}`)
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func TestVideoContractHTTPCreateLookupCancelAndOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	groupID := int64(3)
	price := 0.03
	key := &service.APIKey{ID: 99, UserID: 1, User: &service.User{ID: 1}, GroupID: &groupID, Group: &service.Group{ID: 3, Platform: service.PlatformOpenAI, AllowImageGeneration: true, RateMultiplier: 1, VideoPrice720P: &price}}
	accounts := openAIImagesFailoverAccountRepo{accounts: []service.Account{{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example", "video_protocol": service.VideoProtocolLegacy}}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	upstream := &videoContractHTTPUpstream{}
	gateway := service.NewOpenAIGatewayService(accounts, &videoHandlerUsageRepo{}, nil, nil, nil, nil, repository.NewGatewayCache(client), cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, service.NewDeferredService(accounts, nil, time.Minute), nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	quote, err := gateway.QuoteVideoBilling(context.Background(), key, "seedance-2.0", "720p", 4, time.Now())
	require.NoError(t, err)
	version, err := quote.Version(key, "seedance-2.0", "text-to-video", "720p", 4)
	require.NoError(t, err)
	router := gin.New()
	router.Use(middleware2.ClientRequestID(), func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1})
	})
	router.POST("/v1/videos", h.Videos)
	router.GET("/v1/videos/:request_id", h.Videos)
	router.PUT("/v1/videos/:request_id/cancel", h.Videos)
	perform := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if method == "POST" {
			request.Header.Set("X-Client-Request-ID", "sep-original")
			request.Header.Set("Idempotency-Key", "sep-original")
			request.Header.Set("X-Video-Operation", "text-to-video")
			request.Header.Set("X-Video-Pricing-Version", version)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, request)
		return w
	}
	body := `{"model":"seedance-2.0","prompt":"waves","seconds":4,"resolution":"720p"}`
	created := perform("POST", "/v1/videos", body)
	require.Equal(t, 202, created.Code, created.Body.String())
	require.Contains(t, created.Body.String(), `"request_id":"sep-original"`)
	replay := perform("POST", "/v1/videos", body)
	require.Equal(t, 202, replay.Code, replay.Body.String())
	require.Len(t, upstream.methods, 1)
	lookup := perform("GET", "/v1/videos/sep-original?id_type=request_id", "")
	require.Equal(t, 200, lookup.Code, lookup.Body.String())
	require.Contains(t, lookup.Body.String(), `"status":"in_progress"`)
	upstream.queryFailure = true
	failedQuery := perform("GET", "/v1/videos/sep-original?id_type=request_id", "")
	require.Equal(t, 502, failedQuery.Code, failedQuery.Body.String())
	require.Contains(t, failedQuery.Body.String(), "status_backend_unavailable")
	require.Contains(t, failedQuery.Body.String(), `"status":"in_progress"`)
	cancel := perform("PUT", "/v1/videos/mtask_00000000000000000000000000/cancel", "")
	require.Equal(t, 200, cancel.Code, cancel.Body.String())
	require.Contains(t, cancel.Body.String(), `"status":"cancelled"`)
	count := len(upstream.methods)
	canceledAgain := perform("PUT", "/v1/videos/mtask_00000000000000000000000000/cancel", "")
	require.Equal(t, 200, canceledAgain.Code, canceledAgain.Body.String())
	require.Len(t, upstream.methods, count)
	key.ID++
	foreign := perform("GET", "/v1/videos/sep-original?id_type=request_id", "")
	require.Equal(t, 404, foreign.Code)
	require.Len(t, upstream.methods, count)
	invalid := perform("GET", "/v1/videos/anything?id_type=invalid", "")
	require.Equal(t, 400, invalid.Code)
	upstream.createDisconnect = true
	quote, err = gateway.QuoteVideoBilling(context.Background(), key, "seedance-2.0", "720p", 4, time.Now())
	require.NoError(t, err)
	version, err = quote.Version(key, "seedance-2.0", "text-to-video", "720p", 4)
	require.NoError(t, err)
	count = len(upstream.methods)
	failedCreate := perform("POST", "/v1/videos", body)
	require.Equal(t, 502, failedCreate.Code, failedCreate.Body.String())
	require.Len(t, upstream.methods, count+4)
	require.Contains(t, failedCreate.Body.String(), `"submission_status":"failed"`)
	require.Contains(t, failedCreate.Body.String(), `"status":"not_charged"`)
	require.Contains(t, failedCreate.Body.String(), `"actual_cost_usd":"0"`)
	require.Contains(t, failedCreate.Body.String(), `"exhausted":true`)
	require.Contains(t, failedCreate.Body.String(), "unexpected EOF")
	lookup = perform("GET", "/v1/videos/sep-original?id_type=request_id", "")
	require.Equal(t, 200, lookup.Code, lookup.Body.String())
	require.Contains(t, lookup.Body.String(), `"submission_status":"failed"`)
	require.Contains(t, lookup.Body.String(), `"status":"not_charged"`)
	replay = perform("POST", "/v1/videos", body)
	require.Equal(t, 502, replay.Code, replay.Body.String())
	require.Len(t, upstream.methods, count+4)
}
