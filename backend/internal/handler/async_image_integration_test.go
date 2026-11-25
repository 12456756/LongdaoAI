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

type asyncImageHTTPAccounts struct {
	openAIImagesFailoverAccountRepo
}

func (r asyncImageHTTPAccounts) ListByGroup(context.Context, int64) ([]service.Account, error) {
	return r.accounts, nil
}

type asyncImageHTTPUpstream struct {
	service.HTTPUpstream
	calls      []string
	failQuery  bool
	failedJob  bool
	failCreate bool
	onCreate   func(*http.Request)
}

func (u *asyncImageHTTPUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodPost {
		if u.onCreate != nil {
			u.onCreate(r)
		}
		if u.failCreate {
			return nil, io.ErrUnexpectedEOF
		}
	}
	body := `{"id":"imgjob_fixture","object":"image.generation.job","status":"queued","created":123}`
	status := 202
	if r.Method == "GET" {
		status = 200
		if u.failQuery {
			status = 503
			body = `{"error":{"message":"image task database temporarily unavailable","code":"query_db_down","param":"task_id"}}`
		} else if u.failedJob {
			body = `{"id":"imgjob_fixture","object":"image.generation.job","status":"failed","created":123,"error":{"message":"reference content rejected","code":"policy_rejected"}}`
		} else {
			body = `{"id":"imgjob_fixture","object":"image.generation.job","status":"succeeded","created":123,"data":[{"url":"https://images.example/result.png?signature=internal"}]}`
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"fixture-provider-id"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func newAsyncImageHTTPFixture(t *testing.T) (*gin.Engine, *service.APIKey, *asyncImageHTTPUpstream, *redis.Client, *miniredis.Miniredis, *AsyncImageHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	groupID := int64(3)
	key := &service.APIKey{ID: 7, UserID: 11, User: &service.User{ID: 11}, GroupID: &groupID, Group: &service.Group{ID: 3, Platform: service.PlatformOpenAI, AllowImageGeneration: true, RateMultiplier: 1}}
	accounts := asyncImageHTTPAccounts{openAIImagesFailoverAccountRepo{accounts: []service.Account{{ID: 5, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{3}, Credentials: map[string]any{"api_key": "fixture-only", "base_url": "https://upstream.example", "image_protocol": service.ImageProtocolNaoshengyun}}}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	upstream := &asyncImageHTTPUpstream{}
	gateway := service.NewOpenAIGatewayService(accounts, &videoHandlerUsageRepo{}, nil, nil, nil, nil, repository.NewGatewayCache(client), cfg, nil, nil, service.NewBillingService(cfg, nil), nil, nil, upstream, service.NewDeferredService(accounts, nil, time.Minute), nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h := NewAsyncImageHandler(service.NewImageTaskService(repository.NewImageTaskStore(client)), openAI)
	router := gin.New()
	router.Use(middleware2.ClientRequestID(), func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 11})
	})
	router.POST("/v1/images/generations/async", h.Submit)
	router.GET("/v1/images/tasks/:task_id", h.Get)
	router.GET("/v1/images/tasks/:task_id/content", h.Content)
	return router, key, upstream, client, server, h
}

func TestAsyncImageHTTPSubmitPollReplayAndOwnershipWithoutObjectStorage(t *testing.T) {
	router, key, upstream, client, _, h := newAsyncImageHTTPFixture(t)
	perform := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Client-Request-ID", "sep-image-request")
		r.Header.Set("Idempotency-Key", "sep-image-request")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	body := `{"model":"gpt-image-2-1k","prompt":"ocean","n":1,"size":"1024x1024","response_format":"url"}`
	created := perform("POST", "/v1/images/generations/async", body)
	require.Equal(t, 202, created.Code, created.Body.String())
	require.Equal(t, "sep-image-request", created.Header().Get("X-Client-Request-ID"))
	var task service.ImageTask
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &task))
	require.NotEmpty(t, task.ID)
	require.Len(t, upstream.calls, 1)
	replay := perform("POST", "/v1/images/generations/async", body)
	require.Equal(t, 202, replay.Code, replay.Body.String())
	require.Len(t, upstream.calls, 1)
	conflict := perform("POST", "/v1/images/generations/async", strings.Replace(body, "ocean", "mountains", 1))
	require.Equal(t, 409, conflict.Code, conflict.Body.String())
	require.Len(t, upstream.calls, 1)
	key.ID = 8
	forbidden := perform("GET", "/v1/images/tasks/"+task.ID, "")
	require.Equal(t, 404, forbidden.Code)
	require.Len(t, upstream.calls, 1)
	key.ID = 7
	upstream.failQuery = true
	failedQuery := perform("GET", "/v1/images/tasks/"+task.ID, "")
	require.Equal(t, 502, failedQuery.Code, failedQuery.Body.String())
	require.Contains(t, failedQuery.Body.String(), "query_db_down")
	require.Contains(t, failedQuery.Body.String(), `"status":"processing"`)
	upstream.failQuery = false
	completed := perform("GET", "/v1/images/tasks/"+task.ID, "")
	require.Equal(t, 200, completed.Code, completed.Body.String())
	require.Contains(t, completed.Body.String(), `"status":"completed"`)
	require.Contains(t, completed.Body.String(), "/content")
	require.NotContains(t, completed.Body.String(), "images.example")
	require.NotContains(t, completed.Body.String(), "internal")
	calls := len(upstream.calls)
	again := perform("GET", "/v1/images/tasks/"+task.ID, "")
	require.Equal(t, 200, again.Code)
	require.Len(t, upstream.calls, calls)
	// A service restart reconstructs state from Redis; no background execution is needed.
	h = NewAsyncImageHandler(service.NewImageTaskService(repository.NewImageTaskStore(client)), h.openAI)
	record, err := service.NewImageTaskService(repository.NewImageTaskStore(client)).Get(context.Background(), service.ImageTaskOwner{UserID: 11, APIKeyID: 7}, task.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", record.Status)
}

func performAsyncImageHTTP(router *gin.Engine, ctx context.Context, method, path, body, requestID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Client-Request-ID", requestID)
	r.Header.Set("Idempotency-Key", requestID)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

const asyncImageHTTPBody = `{"model":"gpt-image-2-1k","prompt":"ocean","n":1,"size":"1024x1024","response_format":"url"}`

func TestAsyncImageHTTPRequestLookupRecoversDisconnectedSubmission(t *testing.T) {
	router, key, upstream, _, server, _ := newAsyncImageHTTPFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var dispatchErr error
	upstream.onCreate = func(r *http.Request) {
		cancel()
		close(entered)
		<-release
		dispatchErr = r.Context().Err()
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- performAsyncImageHTTP(router, ctx, "POST", "/v1/images/generations/async", asyncImageHTTPBody, "sep-lost-response")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("image submission did not reach upstream")
	}
	query := "/v1/images/tasks/sep-lost-response?id_type=request_id"
	processing := performAsyncImageHTTP(router, context.Background(), "GET", query, "", "poll-1")
	require.Equal(t, 200, processing.Code, processing.Body.String())
	require.Equal(t, "3", processing.Header().Get("Retry-After"))
	require.Contains(t, processing.Body.String(), `"submission_status":"submitting"`)
	var pending service.ImageTask
	require.NoError(t, json.Unmarshal(processing.Body.Bytes(), &pending))
	require.NotEmpty(t, pending.ID)
	require.Len(t, upstream.calls, 1, "An in-progress lookup must not create or poll a provider task")
	close(release)
	var created *httptest.ResponseRecorder
	select {
	case created = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("image submission did not finish after client cancellation")
	}
	require.NoError(t, dispatchErr)
	require.Equal(t, 202, created.Code, created.Body.String())
	// Ignore the create response: recover using only SEP's original request ID.
	ttl := server.TTL("image_task:" + pending.ID)
	key.ID++
	foreign := performAsyncImageHTTP(router, context.Background(), "GET", query, "", "poll-2")
	require.Equal(t, 404, foreign.Code)
	require.Len(t, upstream.calls, 1)
	key.ID--
	upstream.failQuery = true
	queryError := performAsyncImageHTTP(router, context.Background(), "GET", query, "", "poll-3")
	require.Equal(t, 502, queryError.Code, queryError.Body.String())
	require.Equal(t, "sep-lost-response", queryError.Header().Get("X-Image-Request-ID"))
	require.Contains(t, queryError.Body.String(), "query_db_down")
	require.Contains(t, queryError.Body.String(), `"submission_status":"accepted"`)
	upstream.failQuery = false
	completed := performAsyncImageHTTP(router, context.Background(), "GET", query, "", "poll-4")
	require.Equal(t, 200, completed.Code, completed.Body.String())
	var recovered service.ImageTask
	require.NoError(t, json.Unmarshal(completed.Body.Bytes(), &recovered))
	require.Equal(t, pending.ID, recovered.ID)
	require.Equal(t, "sep-lost-response", recovered.RequestID)
	require.Equal(t, "completed", recovered.Status)
	require.Equal(t, "/v1/images/tasks/"+pending.ID+"/content", recovered.ContentURL)
	require.NotContains(t, completed.Body.String(), "images.example")
	require.LessOrEqual(t, server.TTL("image_task:"+pending.ID), ttl)
	calls := len(upstream.calls)
	again := performAsyncImageHTTP(router, context.Background(), "GET", "/v1/images/tasks/"+pending.ID+"?id_type=task_id", "", "poll-5")
	require.Equal(t, 200, again.Code)
	require.Len(t, upstream.calls, calls)
	server.FastForward(24 * time.Hour)
	expired := performAsyncImageHTTP(router, context.Background(), "GET", query, "", "poll-6")
	require.Equal(t, 404, expired.Code)
	require.Contains(t, expired.Body.String(), "image_request_not_found")
	require.NotContains(t, expired.Body.String(), "not_charged")
}

func TestAsyncImageHTTPRequestLookupFailureInterruptionAndValidation(t *testing.T) {
	router, key, upstream, client, _, h := newAsyncImageHTTPFixture(t)
	upstream.failCreate = true
	failed := performAsyncImageHTTP(router, context.Background(), "POST", "/v1/images/generations/async", asyncImageHTTPBody, "failed-create")
	require.Equal(t, 502, failed.Code, failed.Body.String())
	found := performAsyncImageHTTP(router, context.Background(), "GET", "/v1/images/tasks/failed-create?id_type=request_id", "", "poll-failure")
	require.Equal(t, 200, found.Code, found.Body.String())
	require.Contains(t, found.Body.String(), `"submission_status":"failed"`)
	require.Contains(t, found.Body.String(), `"status":"not_charged"`)
	require.Contains(t, found.Body.String(), "unexpected EOF")
	require.Len(t, upstream.calls, 1)
	record, _, err := h.tasks.ClaimAsync(context.Background(), key, &service.AsyncImageMetadata{RequestID: "interrupted", IdempotencyKey: "different-idempotency", Protocol: service.ImageProtocolNaoshengyun, GroupID: *key.GroupID, SubmissionStatus: "submitting", DispatchDeadline: time.Now().Add(-time.Minute).Unix()})
	require.NoError(t, err)
	interrupted := performAsyncImageHTTP(router, context.Background(), "GET", "/v1/images/tasks/interrupted?id_type=request_id", "", "poll-interrupted")
	require.Equal(t, 200, interrupted.Code, interrupted.Body.String())
	require.Contains(t, interrupted.Body.String(), "image_submission_interrupted")
	require.Contains(t, interrupted.Body.String(), `"status":"not_charged"`)
	// The lookup accepts request IDs only, never an idempotency alias.
	wrongAlias := performAsyncImageHTTP(router, context.Background(), "GET", "/v1/images/tasks/different-idempotency?id_type=request_id", "", "poll-alias")
	require.Equal(t, 404, wrongAlias.Code)
	for _, path := range []string{
		"/v1/images/tasks/interrupted?id_type=",
		"/v1/images/tasks/interrupted?id_type=invalid",
		"/v1/images/tasks/interrupted?id_type=request_id&id_type=task_id",
		"/v1/images/tasks/invalid%20identifier?id_type=request_id",
		"/v1/images/tasks/" + strings.Repeat("x", 257) + "?id_type=request_id",
		"/v1/images/tasks/" + record.ID + "/content?id_type=request_id",
	} {
		invalid := performAsyncImageHTTP(router, context.Background(), "GET", path, "", "poll-invalid")
		require.Equal(t, 400, invalid.Code, invalid.Body.String())
	}
	require.Len(t, upstream.calls, 1)
	// A Redis outage is a platform error, not a missing record or proof of zero charge.
	require.NoError(t, client.Close())
	unavailable := performAsyncImageHTTP(router, context.Background(), "GET", "/v1/images/tasks/interrupted?id_type=request_id", "", "poll-outage")
	require.Equal(t, 503, unavailable.Code, unavailable.Body.String())
	require.Contains(t, unavailable.Body.String(), "client is closed")
	require.NotContains(t, unavailable.Body.String(), "image_request_not_found")
}
