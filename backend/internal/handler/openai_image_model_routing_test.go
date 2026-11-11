//go:build unit

package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type imageModelRoutingUpstream struct {
	service.HTTPUpstream
	accountIDs []int64
	bodies     [][]byte
}

func (u *imageModelRoutingUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	u.accountIDs = append(u.accountIDs, accountID)
	u.bodies = append(u.bodies, body)
	// Stop at the upstream boundary so these routing tests cannot create usage.
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"error":{"type":"invalid_request_error","message":"routing test upstream response"}}`,
		)),
	}, nil
}

func TestOpenAIImageModelRoutingPreservesRequestedName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []struct {
		path     string
		body     string
		call     func(*OpenAIGatewayHandler, *gin.Context)
		sizePath string
		status   int
	}{
		{
			path:     "/v1/images/generations",
			body:     `{"model":%q,"prompt":"draw a cat","size":%q,"quality":"high","n":1}`,
			call:     (*OpenAIGatewayHandler).Images,
			sizePath: "size",
			status:   http.StatusBadRequest,
		},
		{
			path:     "/v1/responses",
			body:     `{"model":%q,"input":"draw a cat","size":%q,"quality":"high","stream":false}`,
			call:     (*OpenAIGatewayHandler).Responses,
			sizePath: "tools.0.size",
			status:   http.StatusBadGateway,
		},
		{
			path:   "/v1/chat/completions",
			body:   `{"model":%q,"messages":[{"role":"user","content":"draw a cat"}],"size":%q,"quality":"high","stream":false}`,
			call:   (*OpenAIGatewayHandler).ChatCompletions,
			status: http.StatusBadRequest,
		},
	} {
		for _, tc := range []struct {
			name          string
			model         string
			size          string
			upstreamModel string
		}{
			{"1k", "gpt-image-2-1k", "1024x1024", "gpt-image-2-1k"},
			{"2k", "gpt-image-2-2k", "2048x2048", "gpt-image-2-2k"},
			{"4k", "gpt-image-2-4k", "4096x4096", "gpt-image-2-4k"},
			{"explicit size", "gpt-image-2-4k", "1024x1024", "gpt-image-2-4k"},
			{"explicit mapping", "gpt-image-2-1k", "1024x1024", "gpt-image-2"},
		} {
			t.Run(endpoint.path+"/"+tc.name, func(t *testing.T) {
				upstream := &imageModelRoutingUpstream{}
				h := newImageModelRoutingHandler(t, upstream, tc.model, tc.upstreamModel)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, bytes.NewBufferString(fmt.Sprintf(endpoint.body, tc.model, tc.size)))
				c.Request.Header.Set("Content-Type", "application/json")
				groupID := int64(3132)
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
					ID: 99, UserID: 100, GroupID: &groupID,
					Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowImageGeneration: true},
					User:  &service.User{ID: 100},
				})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 100, Concurrency: 0})

				endpoint.call(h, c)

				require.Equal(t, []int64{1}, upstream.accountIDs, "the account only admits the original model name; response: %s", rec.Body.String())
				require.Equal(t, endpoint.status, rec.Code)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, tc.upstreamModel, gjson.GetBytes(upstream.bodies[0], "model").String())
				if endpoint.sizePath != "" {
					require.Equal(t, tc.size, gjson.GetBytes(upstream.bodies[0], endpoint.sizePath).String())
				}
			})
		}
	}
}

func newImageModelRoutingHandler(t *testing.T, upstream service.HTTPUpstream, requestedModel, upstreamModel string) *OpenAIGatewayHandler {
	t.Helper()
	account := service.Account{
		ID: 1, Name: "image-model-routing", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key":       "test-key",
			"base_url":      "https://images.example/v1",
			"model_mapping": map[string]any{requestedModel: upstreamModel},
		},
		Extra: map[string]any{"openai_responses_image_model_passthrough": true},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: []service.Account{account}},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	return NewOpenAIGatewayHandler(
		gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil, nil, nil, nil, cfg,
	)
}
