package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type videoTestCache struct {
	GatewayCache
	bindings map[string]int64
	err      error
}

func (s *videoTestCache) GetSessionAccountID(_ context.Context, groupID int64, key string) (int64, error) {
	return s.bindings[fmt.Sprintf("%d:%s", groupID, key)], s.err
}

func (s *videoTestCache) SetSessionAccountID(_ context.Context, groupID int64, key string, accountID int64, _ time.Duration) error {
	if s.bindings == nil {
		s.bindings = make(map[string]int64)
	}
	s.bindings[fmt.Sprintf("%d:%s", groupID, key)] = accountID
	return s.err
}

type videoTestUpstream struct {
	HTTPUpstream
	do func(*http.Request) (*http.Response, error)
}

func (s videoTestUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return s.do(r)
}

type videoTestAccountRepo struct {
	AccountRepository
	account *Account
}

func (r videoTestAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func videoTestContext(method, path string, body []byte, contentType string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)
	return c, w
}

func TestBuildOpenAIVideoURL(t *testing.T) {
	for _, base := range []string{"https://upstream.example", "https://upstream.example/v1", "https://upstream.example/v1/videos", "https://upstream.example/v1/video/generations"} {
		require.Equal(t, "https://upstream.example/v1/videos", buildOpenAIVideoURL(base, "", false))
		require.Equal(t, "https://upstream.example/v1/videos/video_123", buildOpenAIVideoURL(base, "video_123", false))
		require.Equal(t, "https://upstream.example/v1/videos/video_123/content", buildOpenAIVideoURL(base, "video_123", true))
	}
	require.Equal(t, "https://upstream.example/api/v2/videos/video_123?tenant=a", buildOpenAIVideoURL("https://upstream.example/api/v2/videos?tenant=a", "video_123", false))
}

func TestParseOpenAIVideoRequest(t *testing.T) {
	r, err := ParseOpenAIVideoRequest([]byte(`{"model":"minimax-h3","prompt":"waves","seconds":"20","size":"1280x720","audio":false}`), "application/json")
	require.NoError(t, err)
	require.Equal(t, 20, r.Seconds)
	require.Equal(t, "720p", r.Resolution)
	require.False(t, r.Multipart)
	for _, body := range []string{`[]`, `{`, `{"model":"minimax-h3"}`, `{"model":5,"prompt":"waves"}`, `{"model":"m","prompt":"waves","seconds":"bad"}`, `{"model":"m","prompt":"waves","duration":-2}`} {
		_, err := ParseOpenAIVideoRequest([]byte(body), "application/json")
		require.Error(t, err, body)
	}
	_, err = ParseOpenAIVideoRequest([]byte("bad"), "multipart/form-data; boundary=a")
	require.Error(t, err)
}

func TestForwardVideoCreatesAndBindsTask(t *testing.T) {
	body := []byte(`{"model":"video-alias","prompt":"waves","seconds":"20","size":"1280x720","audio":false,"fps":30,"extra":{"enabled":true}}`)
	request, err := ParseOpenAIVideoRequest(body, "application/json")
	require.NoError(t, err)
	c, w := videoTestContext(http.MethodPost, "/v1/videos", body, request.ContentType)
	cache := &videoTestCache{}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "provider-key", "base_url": "https://upstream.example/v1", "model_mapping": map[string]any{"video-alias": "minimax-h3"}}}
	apiKey := &APIKey{ID: 9}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, cache: cache, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https://upstream.example/v1/videos", r.URL.String())
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer provider-key", r.Header.Get("Authorization"))
		forwarded, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "minimax-h3", gjson.GetBytes(forwarded, "model").String())
		require.Equal(t, "20", gjson.GetBytes(forwarded, "seconds").String())
		require.True(t, gjson.GetBytes(forwarded, "audio").Exists())
		require.Equal(t, int64(30), gjson.GetBytes(forwarded, "fps").Int())
		require.True(t, gjson.GetBytes(forwarded, "extra.enabled").Bool())
		return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"video_123","status":"queued","seconds":"20","size":"1280x720"}`))}, nil
	}}}
	result, err := svc.ForwardVideo(context.Background(), c, account, apiKey, request, "", false, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.Equal(t, "video_123", result.ResponseID)
	require.Equal(t, 1, result.VideoCount)
	require.Equal(t, 0, result.ImageCount)
	require.Equal(t, 20, result.VideoDurationSeconds)
	require.Equal(t, "720p", result.VideoResolution)
	id, err := cache.GetSessionAccountID(context.Background(), 0, videoTaskCacheKey(9, "video_123"))
	require.NoError(t, err)
	require.Equal(t, int64(7), id)
}

func TestForwardVideoMultipartPreservesReferenceAndMapsModel(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	require.NoError(t, w.WriteField("model", "alias"))
	require.NoError(t, w.WriteField("prompt", "animate"))
	require.NoError(t, w.WriteField("seconds", "6"))
	require.NoError(t, w.WriteField("fps", "30"))
	require.NoError(t, w.WriteField("extra", "preserved"))
	part, err := w.CreateFormFile("input_reference", "frame.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("reference-bytes"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	request, err := ParseOpenAIVideoRequest(body.Bytes(), w.FormDataContentType())
	require.NoError(t, err)
	c, _ := videoTestContext(http.MethodPost, "/v1/video/generations", body.Bytes(), request.ContentType)
	svc := &OpenAIGatewayService{cfg: &config.Config{}, cache: &videoTestCache{}, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		require.NoError(t, r.ParseMultipartForm(1<<20))
		require.Equal(t, "minimax-h3", r.FormValue("model"))
		require.Equal(t, "6", r.FormValue("seconds"))
		require.Equal(t, "30", r.FormValue("fps"))
		require.Equal(t, "preserved", r.FormValue("extra"))
		f, header, err := r.FormFile("input_reference")
		require.NoError(t, err)
		defer f.Close()
		require.Equal(t, "frame.png", header.Filename)
		data, err := io.ReadAll(f)
		require.NoError(t, err)
		require.Equal(t, "reference-bytes", string(data))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"video_1"}`))}, nil
	}}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key", "base_url": "https://upstream.example"}}
	_, err = svc.ForwardVideo(context.Background(), c, account, &APIKey{ID: 1}, request, "", false, "minimax-h3")
	require.NoError(t, err)
}

func TestVideoTaskBindingIsScopedAndNeverFallsBack(t *testing.T) {
	cache := &videoTestCache{}
	groupID := int64(3)
	apiKey := &APIKey{ID: 9, GroupID: &groupID}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, GroupIDs: []int64{3}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, cache: cache, accountRepo: videoTestAccountRepo{account: account}}
	require.NoError(t, cache.SetSessionAccountID(context.Background(), 3, videoTaskCacheKey(9, "video_1"), 7, time.Hour))
	got, err := svc.GetVideoTaskAccount(context.Background(), apiKey, "video_1")
	require.NoError(t, err)
	require.Equal(t, int64(7), got.ID)
	account.Credentials = map[string]any{openAIEndpointCapabilitiesCredentialKey: []string{"chat_completions"}}
	_, err = svc.GetVideoTaskAccount(context.Background(), apiKey, "video_1")
	require.ErrorIs(t, err, ErrVideoTaskNotFound)
	account.Credentials = nil
	for _, key := range []*APIKey{{ID: 10, GroupID: &groupID}, {ID: 9}} {
		_, err := svc.GetVideoTaskAccount(context.Background(), key, "video_1")
		require.ErrorIs(t, err, ErrVideoTaskNotFound)
	}
	_, err = svc.GetVideoTaskAccount(context.Background(), apiKey, "unknown")
	require.ErrorIs(t, err, ErrVideoTaskNotFound)
	cache.err = errors.New("redis unavailable")
	_, err = svc.GetVideoTaskAccount(context.Background(), apiKey, "video_1")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrVideoTaskNotFound)
	cache.err = redis.Nil
	_, err = svc.GetVideoTaskAccount(context.Background(), apiKey, "video_1")
	require.ErrorIs(t, err, ErrVideoTaskNotFound)
}

func TestForwardVideoContentStreamsRangeAndDoesNotBill(t *testing.T) {
	c, w := videoTestContext(http.MethodGet, "/v1/videos/video_123/content?variant=thumbnail", nil, "")
	c.Request.Header.Set("Range", "bytes=0-3")
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/v1/videos/video_123/content", r.URL.Path)
		require.Equal(t, "thumbnail", r.URL.Query().Get("variant"))
		require.Equal(t, "bytes=0-3", r.Header.Get("Range"))
		return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": {"video/mp4"}, "Content-Length": {"4"}, "Content-Range": {"bytes 0-3/100"}}, Body: io.NopCloser(strings.NewReader("mp4!"))}, nil
	}}}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key", "base_url": "https://upstream.example/v1/videos"}}
	result, err := svc.ForwardVideo(context.Background(), c, account, &APIKey{}, nil, "video_123", true, "")
	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, "video/mp4", w.Header().Get("Content-Type"))
	require.Equal(t, "bytes 0-3/100", w.Header().Get("Content-Range"))
	require.Equal(t, "mp4!", w.Body.String())
}

func TestForwardVideoUpstreamErrorIsPreservedWithoutRetry(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			request, err := ParseOpenAIVideoRequest([]byte(`{"model":"minimax-h3","prompt":"waves","seconds":"6"}`), "application/json")
			require.NoError(t, err)
			c, w := videoTestContext(http.MethodPost, "/v1/videos", request.Body, request.ContentType)
			calls := 0
			svc := &OpenAIGatewayService{cfg: &config.Config{}, cache: &videoTestCache{}, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"provider_error"}}`))}, nil
			}}}
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "key", "base_url": "https://upstream.example"}}
			_, err = svc.ForwardVideo(context.Background(), c, account, &APIKey{}, request, "", false, "")
			require.Error(t, err)
			require.Equal(t, status, w.Code)
			require.Contains(t, w.Body.String(), "provider_error")
			require.Equal(t, 1, calls)
		})
	}
}

func TestOpenAIVideoBillingPreservesModelAndLongDuration(t *testing.T) {
	groupID := int64(3)
	price := 0.1
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "video-billing", Model: "minimax-h3", BillingModel: "minimax-h3", VideoCount: 1, VideoResolution: "720p", VideoDurationSeconds: 20},
		APIKey: &APIKey{ID: 9, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI, RateMultiplier: 1, VideoPrice720P: &price}},
		User:   &User{ID: 1}, Account: &Account{ID: 7, Platform: PlatformOpenAI},
	})
	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.lastLog.VideoCount)
	require.Equal(t, 20, *usageRepo.lastLog.VideoDurationSeconds)
	require.Equal(t, string(BillingModeVideo), *usageRepo.lastLog.BillingMode)
	require.InDelta(t, 2.0, usageRepo.lastLog.TotalCost, 1e-9)
}

func TestOpenAIVideosRequireConfiguredPricing(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	apiKey := &APIKey{Group: &Group{}}
	require.Error(t, svc.CheckVideoPricing(context.Background(), apiKey, nil, "minimax-h3", "", "720p"))
	price := 0.1
	apiKey.Group.VideoPrice720P = &price
	require.NoError(t, svc.CheckVideoPricing(context.Background(), apiKey, nil, "minimax-h3", "", "720p"))
	require.Error(t, svc.CheckVideoPricing(context.Background(), apiKey, nil, "minimax-h3", "", "1080p"))
	price = 0
	require.NoError(t, svc.CheckVideoPricing(context.Background(), apiKey, nil, "minimax-h3", "", "720p"), "an explicit zero price is allowed")
	apiKey.Group.VideoPrice720P = nil
	require.NoError(t, svc.CheckVideoPricing(context.Background(), apiKey, nil, "grok-imagine-video", "", "720p"))
}
