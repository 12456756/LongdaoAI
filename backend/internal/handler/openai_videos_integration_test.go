//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type videoHandlerCache struct {
	service.GatewayCache
	bindings map[string]int64
}

func (s *videoHandlerCache) GetSessionAccountID(_ context.Context, group int64, key string) (int64, error) {
	return s.bindings[fmt.Sprintf("%d:%s", group, key)], nil
}

func (s *videoHandlerCache) SetSessionAccountID(_ context.Context, group int64, key string, id int64, _ time.Duration) error {
	s.bindings[fmt.Sprintf("%d:%s", group, key)] = id
	return nil
}

type videoHandlerUsageRepo struct {
	service.UsageLogRepository
	logs []*service.UsageLog
}

func (r *videoHandlerUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.logs = append(r.logs, log)
	return true, nil
}

type videoHandlerUpstream struct {
	service.HTTPUpstream
	accounts []int64
	paths    []string
}

func (s *videoHandlerUpstream) Do(r *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	s.accounts = append(s.accounts, accountID)
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	body := `{"id":"video_123","status":"completed","seconds":"6","size":"1280x720"}`
	contentType := "application/json"
	if strings.HasSuffix(r.URL.Path, "/content") {
		body, contentType = "binary-video", "video/mp4"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestOpenAIVideosHandlerCreatePollDownloadAndOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3)
	accounts := []service.Account{
		{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "provider-key", "base_url": "https://upstream.example"}},
		{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "other-provider", "base_url": "https://other.example"}},
	}
	repo := openAIImagesFailoverAccountRepo{accounts: accounts}
	upstream := &videoHandlerUpstream{}
	usageRepo := &videoHandlerUsageRepo{}
	cache := &videoHandlerCache{bindings: make(map[string]int64)}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrency := service.NewConcurrencyService(nil)
	deferred := service.NewDeferredService(repo, nil, time.Minute)
	gateway := service.NewOpenAIGatewayService(repo, usageRepo, nil, nil, nil, nil, cache, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, deferred, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	price := 0.1
	keyID := int64(99)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: keyID, GroupID: &groupID, User: &service.User{ID: 1}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true, RateMultiplier: 1, VideoPrice720P: &price}})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1})
	})
	router.POST("/v1/videos", h.Videos)
	router.POST("/v1/video/generations", h.Videos)
	router.GET("/v1/videos/:request_id", h.Videos)
	router.GET("/v1/videos/:request_id/content", h.Videos)
	perform := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		return w
	}
	tooLarge := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(strings.Repeat("x", int(service.VideoUploadMaxBytes)+1)))
	tooLarge.Header.Set("Content-Type", "Multipart/Form-Data; boundary=testing")
	rejected := httptest.NewRecorder()
	router.ServeHTTP(rejected, tooLarge)
	require.Equal(t, http.StatusRequestEntityTooLarge, rejected.Code)
	compressed := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader("compressed"))
	compressed.Header.Set("Content-Type", "multipart/form-data; boundary=testing")
	compressed.Header.Set("Content-Encoding", "gzip")
	rejected = httptest.NewRecorder()
	router.ServeHTTP(rejected, compressed)
	require.Equal(t, http.StatusUnsupportedMediaType, rejected.Code)
	require.Empty(t, upstream.paths)
	created := perform(http.MethodPost, "/v1/video/generations", `{"model":"minimax-h3","prompt":"waves","seconds":"6","size":"1280x720"}`)
	require.Equal(t, 200, created.Code, created.Body.String())
	require.Len(t, usageRepo.logs, 1)
	require.Equal(t, 1, usageRepo.logs[0].VideoCount)
	require.InDelta(t, 0.6, usageRepo.logs[0].TotalCost, 1e-9)
	require.Equal(t, 200, perform(http.MethodGet, "/v1/videos/video_123", "").Code)
	download := perform(http.MethodGet, "/v1/videos/video_123/content", "")
	require.Equal(t, 200, download.Code)
	require.Equal(t, "video/mp4", download.Header().Get("Content-Type"))
	require.Equal(t, "binary-video", download.Body.String())
	require.Equal(t, []string{"POST /v1/videos", "GET /v1/videos/video_123", "GET /v1/videos/video_123/content"}, upstream.paths)
	require.Len(t, upstream.accounts, 3)
	require.Equal(t, upstream.accounts[0], upstream.accounts[1])
	require.Equal(t, upstream.accounts[0], upstream.accounts[2])
	require.Len(t, usageRepo.logs, 1, "polling and downloads must not charge again")
	keyID = 100
	require.Equal(t, 404, perform(http.MethodGet, "/v1/videos/video_123", "").Code)
	require.Len(t, upstream.accounts, 3, "another API key cannot reach the task")
	keyID = 99
	require.Equal(t, 404, perform(http.MethodGet, "/v1/videos/unknown", "").Code)
	require.Len(t, upstream.accounts, 3, "unknown tasks cannot be load balanced")
}
