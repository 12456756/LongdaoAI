package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayRoutesVideoBillingReadOnlyContracts(t *testing.T) {
	router := newGatewayRoutesTestRouter()
	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	require.True(t, registered["POST /v1/sub2api/videos/pricing"])
	require.True(t, registered["GET /v1/sub2api/videos/usage"])
	require.True(t, registered["GET /v1/videos/:request_id"])
	require.True(t, registered["PUT /v1/videos/:request_id/cancel"])
	require.True(t, registered["PUT /videos/:request_id/cancel"])
	// Existing image contracts keep their paths and methods.
	require.True(t, registered["POST /v1/sub2api/images/pricing"])
	require.True(t, registered["GET /v1/sub2api/usage"])
	for _, path := range []string{"/v1/sub2api/videos/pricing", "/v1/sub2api/videos/usage?request_id=req-1"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "pricing") {
			method = http.MethodPost
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		// The router helper supplies a synthetic key without a user/ID. Both new
		// handlers reject it before pricing, database or upstream work.
		require.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
}
