package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestErrorIntegrityFailoverHTTPAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "sse"}[stream], func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Header("X-Request-ID", "platform-id")
			c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{
				{UpstreamStatusCode: 403, Message: "Provider balance exhausted"},
				{UpstreamStatusCode: 422, Message: "Unsupported size"},
			})
			if stream {
				c.Header("Content-Type", "text/event-stream")
				c.Writer.WriteHeaderNow()
			}
			failure := &service.UpstreamFailoverError{
				StatusCode:      422,
				ResponseHeaders: http.Header{"X-Request-Id": []string{"provider-id"}},
				ResponseBody:    []byte(`{"error":{"message":"Unsupported size; Bearer dummy-secret","type":"invalid_request_error","code":"bad_size","param":"size","details":{"allowed":["720p"]}}}`),
			}
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failure, stream)
			require.Equal(t, "platform-id", w.Header().Get("X-Request-ID"))
			if stream {
				require.Equal(t, 200, w.Code)
				require.Contains(t, w.Body.String(), "event: response.failed")
			} else {
				require.Equal(t, 502, w.Code)
				require.Equal(t, "provider-id", w.Header().Get("X-Upstream-Request-ID"))
			}
			for _, diagnostic := range []string{"Unsupported size", "bad_size", "invalid_request_error", "allowed", "provider-id", "Provider balance exhausted", "attempts"} {
				require.Contains(t, w.Body.String(), diagnostic)
			}
			require.NotContains(t, w.Body.String(), "dummy-secret")
		})
	}
}

func TestErrorIntegrityAsyncImageFailure(t *testing.T) {
	for _, input := range []struct {
		name, body, reason string
		status             int
	}{
		{"business_error", `{"error":{"message":"Provider balance exhausted; Bearer dummy-secret","code":"quota","details":{"remaining":0}},"request_id":"body-id"}`, "Provider balance exhausted", 200},
		{"plain_text", "Invalid image format; Bearer dummy-secret", "Invalid image format", 400},
		{"no_image", `{"data":[]}`, "no valid data[]", 200},
	} {
		t.Run(input.name, func(t *testing.T) {
			store := &asyncImageMemoryStore{tasks: make(map[string]*service.ImageTaskRecord)}
			tasks := service.NewImageTaskServiceWithUploader(store, nil, time.Hour, time.Minute)
			owner := service.ImageTaskOwner{UserID: 7, APIKeyID: 9}
			task, err := tasks.Create(context.Background(), owner)
			require.NoError(t, err)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			h := &AsyncImageHandler{tasks: tasks, execute: func(_ string, c *gin.Context) { c.Data(input.status, "application/json", []byte(input.body)) }}
			h.run(task.ID, service.PlatformOpenAI, c, w, func() {})
			got, err := tasks.Get(context.Background(), owner, task.ID)
			require.NoError(t, err)
			require.Equal(t, service.ImageTaskStatusFailed, got.Status)
			require.Contains(t, string(got.Error), input.reason)
			require.NotContains(t, string(got.Error), "dummy-secret")
			require.Empty(t, got.Result)
			require.True(t, json.Valid(got.Error))
			if input.name == "business_error" {
				require.Contains(t, string(got.Error), "quota")
				require.Contains(t, string(got.Error), "body-id")
			}
		})
	}
}

func TestErrorIntegrityPartialDataIncludesActualReason(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	markGatewayDataError(c, "models", &diagnosticTestError{})
	writeGatewayData(c, gin.H{"data": []any{}})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "true", w.Header().Get("X-Data-Degraded"))
	require.Contains(t, w.Body.String(), "Database read timed out")
	require.NotContains(t, w.Body.String(), "dummy-secret")
	require.True(t, strings.Contains(w.Body.String(), `"partial":true`))
}

type diagnosticTestError struct{}

func (*diagnosticTestError) Error() string { return "Database read timed out; password=dummy-secret" }

func TestErrorIntegritySelectionFailurePreservesCause(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		cause := errors.New("Account repository read timed out; password=dummy-secret")
		if anthropic {
			(&GatewayHandler{}).handleFailoverExhaustedSimple(c, 502, false, cause)
		} else {
			(&OpenAIGatewayHandler{}).handleFailoverExhaustedSimple(c, 502, false, cause)
		}
		require.Equal(t, http.StatusBadGateway, w.Code)
		require.Contains(t, w.Body.String(), "Account repository read timed out")
		require.Contains(t, w.Body.String(), `"source":"platform"`)
		require.NotContains(t, w.Body.String(), "dummy-secret")
	}
}
