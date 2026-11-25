package routes

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayRegisteredMediaRoutesIncludeReceiptsBeforeAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := &config.Config{}
	RegisterGatewayRoutes(r, &handler.Handlers{Gateway: &handler.GatewayHandler{}, OpenAIGateway: &handler.OpenAIGatewayHandler{}, AsyncImage: handler.NewAsyncImageHandler(nil, nil)}, middleware.NewAPIKeyAuthMiddleware(nil, nil, cfg), nil, nil, nil, nil, cfg)
	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/images/generations"}, {"POST", "/images/edits"},
		{"POST", "/v1/images/generations/async"}, {"POST", "/images/edits/async"},
		{"GET", "/v1/images/tasks/imgtask_x"}, {"GET", "/images/tasks/imgtask_x/content"},
		{"POST", "/v1/videos"}, {"POST", "/videos"}, {"POST", "/video/generations"},
		{"POST", "/v1/videos/references"},
		{"GET", "/v1/videos/task"}, {"GET", "/videos/task/content"}, {"PUT", "/v1/videos/task/cancel"},
		{"POST", "/v1/sub2api/images/pricing"}, {"POST", "/v1/sub2api/videos/pricing"},
		{"GET", "/v1/sub2api/usage?request_id=old"}, {"GET", "/v1/sub2api/videos/usage?request_id=old"},
	} {
		t.Run(route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set("X-Client-Request-ID", "sep-auth-request")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, 401, w.Code, w.Body.String())
			var out map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
			for _, field := range []string{"request_id", "task_id", "operation", "submission_status", "status", "is_final", "status_observed_at", "status_source", "task_error", "billing", "error", "operation_error", "submission_retry"} {
				require.Contains(t, out, field)
			}
			require.Equal(t, "sep-auth-request", out["request_id"])
			require.Nil(t, out["task_id"])
			require.Equal(t, "platform", out["error"].(map[string]any)["source"])
			if route.path == "/v1/videos/references" {
				require.Equal(t, "upload", out["operation"])
				require.Equal(t, "not_applicable", out["submission_status"])
				require.Equal(t, "not_charged", out["billing"].(map[string]any)["status"])
			}
		})
	}
}
