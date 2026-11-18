//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIImagesFailoverAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r openAIImagesFailoverAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

// SetTempUnschedulable 必须实现：持久性传输错误（connection refused 等）会调用它给
// 账号打临时不可调度。嵌入的 nil 接口会 panic，panic 被 handler recover 后表现为
// 普通 502，会掩盖"到底有没有走 failover"。
func (r openAIImagesFailoverAccountRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	return nil
}

func (r openAIImagesFailoverAccountRepo) accountsForPlatform(platform string) []service.Account {
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out
}

type openAIImagesFailoverHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
}

func (u *openAIImagesFailoverHTTPUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req_img_failover"},
		},
		Body: io.NopCloser(bytes.NewBufferString(
			"data: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"server_error\",\"message\":\"image backend unavailable\"}}\n\n",
		)),
	}, nil
}

func (u *openAIImagesFailoverHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func TestOpenAIGatewayHandlerImages_ServerErrorFailsOverAndReturnsClearErrorWhenExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3130)
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "image-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1"},
		},
		{
			ID:          2,
			Name:        "image-account-2",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2"},
		},
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	upstream := &openAIImagesFailoverHTTPUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10

	body := []byte(`{"model":"gpt-image-2","prompt":"draw a cat"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: true,
		},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})

	handler.Images(c)

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "server_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "image backend unavailable", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())

	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, "failover", events[1].Kind)
}

// openAIImagesTransportErrorUpstream 模拟"第一个账号传输层挂掉、第二个正常"。
// 这是线上真实形态：同分组两个上游都支持 Images API，但其中一家会出现
// dial tcp / http2 connection lost / 连接被掐这类没有 HTTP 响应的失败。
type openAIImagesTransportErrorUpstream struct {
	service.HTTPUpstream
	mu          sync.Mutex
	accountIDs  []int64
	failAccount int64
	failErr     error
}

func (u *openAIImagesTransportErrorUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	if accountID == u.failAccount {
		return nil, u.failErr
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"created":1,"data":[{"b64_json":"` + tinyPNGBase64ForImagesTest + `"}]}`,
		)),
	}, nil
}

func (u *openAIImagesTransportErrorUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

const tinyPNGBase64ForImagesTest = "iVBORw0KGgoAAAANSUhEUgAAAAcAAAADCAIAAADQoYKSAAAAEklEQVR4nGP4z8CAibAI4RQFAMeWFOx1QjWwAAAAAElFTkSuQmCC"

func newOpenAIImagesTransportTestHandler(t *testing.T, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	t.Helper()
	accounts := []service.Account{
		{
			ID: 1, Name: "image-apikey-1", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive,
			Schedulable: true, Priority: 0,
			Credentials: map[string]any{"api_key": "sk-1", "base_url": "https://up1.example/v1"},
		},
		{
			ID: 2, Name: "image-apikey-2", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive,
			Schedulable: true, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-2", "base_url": "https://up2.example/v1"},
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		service.NewConcurrencyService(nil),
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

func runOpenAIImagesTransportRequest(t *testing.T, handler *OpenAIGatewayHandler) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	groupID := int64(3131)
	body := []byte(`{"model":"gpt-image-2","prompt":"draw a cat"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID: 99, GroupID: &groupID,
		Group: &service.Group{ID: groupID, AllowImageGeneration: true},
		User:  &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	handler.Images(c)
	return rec, c
}

// 传输层失败必须切到分组里的另一个账号，而不是直接把 502 抛给客户端。
// 线上现象：aipro 连接抽风时 7 次失败全部返回 502，从未尝试同分组同样支持
// Images API 的第二个账号。
func TestOpenAIGatewayHandlerImages_TransportErrorFailsOverToHealthyAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesTransportErrorUpstream{
		failAccount: 1,
		failErr:     errors.New(`Post "https://up1.example/v1/images/generations": dial tcp 104.233.199.1:443: connect: connection refused`),
	}
	handler := newOpenAIImagesTransportTestHandler(t, upstream)

	rec, _ := runOpenAIImagesTransportRequest(t, handler)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "第一个账号传输失败后必须重试第二个")
	require.Equal(t, http.StatusOK, rec.Code, "第二个账号成功时客户端应拿到 200")
	require.NotEmpty(t, gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String())
}

// 客户端主动断开不是上游故障：不能因此切换账号（会重复出图重复计费），
// 也不能给账号记降级。
func TestOpenAIGatewayHandlerImages_ClientCancelDoesNotFailOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesTransportErrorUpstream{
		failAccount: 1,
		failErr:     fmt.Errorf("Post \"https://up1.example/v1/images/generations\": %w", context.Canceled),
	}
	handler := newOpenAIImagesTransportTestHandler(t, upstream)

	_, _ = runOpenAIImagesTransportRequest(t, handler)

	require.Equal(t, []int64{1}, upstream.calls(), "客户端断开不应触发账号切换")
}

// 瞬时传输错误（线上出现过 "http2: client connection lost"）同样要切换账号，
// 且不应把账号打成临时不可调度。
func TestOpenAIGatewayHandlerImages_TransientTransportErrorFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &openAIImagesTransportErrorUpstream{
		failAccount: 1,
		failErr:     errors.New(`Post "https://up1.example/v1/images/generations": http2: client connection lost`),
	}
	handler := newOpenAIImagesTransportTestHandler(t, upstream)

	rec, _ := runOpenAIImagesTransportRequest(t, handler)

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusOK, rec.Code)
}
