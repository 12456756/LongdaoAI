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
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type videoGuardUpstream struct {
	service.HTTPUpstream
	calls  int
	status int
	change func()
}

func (u *videoGuardUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	if u.change != nil {
		u.change()
	}
	status, body := u.status, `{"id":"video_123","status":"completed","seconds":"6","size":"1280x720"}`
	if status == 409 {
		body = `{"error":{"code":"video_pricing_changed"}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{
		"Content-Type": {"application/json"}, "X-Video-Dispatch": {"rejected"},
		"X-Client-Request-Id": {"supplier-forged"}, "x-client-request-id": {"supplier-lowercase"},
		"x-video-dispatch": {"rejected"}, "X-Request-Id": {"supplier-request"},
	}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestOpenAIVideosPriceGuardPreDispatchAndResponseIdentity(t *testing.T) {
	for _, scenario := range []string{"accepted frozen", "changed price", "missing operation", "duplicate version", "wrong operation", "oversize version", "supplier forged rejection", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			groupID := int64(3)
			price := 0.1
			key := &service.APIKey{ID: 99, UserID: 1, User: &service.User{ID: 1}, GroupID: &groupID,
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true, RateMultiplier: 1, VideoPrice720P: &price}}
			repo := openAIImagesFailoverAccountRepo{accounts: []service.Account{{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example"}}}}
			upstream, usage := &videoGuardUpstream{status: 200}, &videoHandlerUsageRepo{}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			// Even an operator allowlist cannot expose supplier-owned proof headers.
			cfg.Security.ResponseHeaders = config.ResponseHeaderConfig{Enabled: true, AdditionalAllowed: []string{"x-video-dispatch", "x-client-request-id"}}
			gateway := service.NewOpenAIGatewayService(repo, usage, nil, nil, nil, nil, &videoHandlerCache{bindings: map[string]int64{}}, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, service.NewDeferredService(repo, nil, time.Minute), nil, nil, nil, nil, nil, nil, nil)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			quote, err := gateway.QuoteVideoBilling(context.Background(), key, "minimax-h3", "720p", 6, time.Now())
			require.NoError(t, err)
			version, err := quote.Version(key, "minimax-h3", "text-to-video", "720p", 6)
			require.NoError(t, err)
			router := gin.New()
			router.Use(middleware2.ClientRequestID(), func(c *gin.Context) {
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1})
			})
			router.POST("/v1/videos", h.Videos)
			r := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"minimax-h3","prompt":"waves","seconds":6,"size":"1280x720"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Client-Request-ID", "caller-original-id")
			r.Header.Set("X-Video-Pricing-Version", version)
			r.Header.Set("X-Video-Operation", "text-to-video")
			switch scenario {
			case "accepted frozen":
				upstream.change = func() { price = 99; key.Group.RateMultiplier = 99 }
			case "changed price":
				price = 0.2
			case "missing operation":
				r.Header.Del("X-Video-Operation")
			case "duplicate version":
				r.Header.Add("X-Video-Pricing-Version", version)
			case "wrong operation":
				r.Header.Set("X-Video-Operation", "image-to-video")
			case "oversize version":
				r.Header.Set("X-Video-Pricing-Version", strings.Repeat("x", 129))
			case "supplier forged rejection":
				upstream.status = 409
			case "legacy":
				r.Header.Del("X-Video-Pricing-Version")
				r.Header.Del("X-Video-Operation")
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			responseID := w.Header().Get("X-Client-Request-ID")
			require.NotEmpty(t, responseID)
			require.NotEqual(t, "caller-original-id", responseID)
			require.NotContains(t, w.Header().Values("X-Client-Request-ID"), "supplier-forged")
			require.Len(t, w.Header().Values("X-Client-Request-ID"), 1)
			switch scenario {
			case "accepted frozen", "legacy":
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, 1, upstream.calls)
				require.Empty(t, w.Header().Get("X-Video-Dispatch"))
				require.Len(t, usage.logs, 1)
				require.Equal(t, "client:"+responseID, usage.logs[0].RequestID)
				require.InDelta(t, 0.6, usage.logs[0].ActualCost, 1e-12)
			case "supplier forged rejection":
				require.Equal(t, 409, w.Code)
				require.Equal(t, 1, upstream.calls)
				require.Empty(t, w.Header().Get("X-Video-Dispatch"), "supplier code is not proof of non-dispatch")
				require.Empty(t, usage.logs)
			default:
				require.Equal(t, 409, w.Code, w.Body.String())
				require.Zero(t, upstream.calls, "price rejection must happen before upstream invocation")
				require.Equal(t, "rejected", w.Header().Get("X-Video-Dispatch"))
				var payload struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
				wantCode := "video_pricing_changed"
				if scenario == "missing operation" || scenario == "duplicate version" || scenario == "oversize version" {
					wantCode = "invalid_video_pricing_headers"
				}
				require.Equal(t, wantCode, payload.Error.Code)
				require.Empty(t, usage.logs)
			}
		})
	}
}
