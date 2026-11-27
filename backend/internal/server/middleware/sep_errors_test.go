package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestSEPErrorPreservesBusinessReasonAndTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, err := range []error{
		service.SEPFailure(409, "SETTLEMENT_MISMATCH", "金额不一致", "validate"),
		fmt.Errorf("load price: %w", infraerrors.New(409, "price_changed", "价格已变更")),
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sep/transfers", nil)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, "trace-123"))
		c.Request.Header.Set("X-SEP-Enterprise-ID", "enterprise-1")
		WriteSEPError(c, err, "settle")
		require.Equal(t, 409, rec.Code)
		require.Equal(t, "trace-123", rec.Header().Get("X-Request-ID"))
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, "trace-123", body["request_id"])
		require.Equal(t, "enterprise-1", body["enterprise_id"])
		require.NotEqual(t, "SEP_STORAGE_UNAVAILABLE", body["reason"])
	}
}

func TestSEPErrorRedactsDiagnosticsAndDoesNotLoseCause(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		err              error
		code, diagnostic string
	}{
		{errors.New("read dependency: connection refused Bearer private-token password=secret sk-secret123456"), "SEP_INTERNAL_ERROR", "connection refused"},
		{context.DeadlineExceeded, "SEP_OPERATION_TIMEOUT", "context deadline exceeded"},
		{&pq.Error{Code: "42P01", Message: "relation sep_balance_events does not exist", Detail: "password=secret row=private-data"}, "SEP_DATABASE_42P01", "relation sep_balance_events does not exist"},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/sep/executions/e/billing", nil)
		WriteSEPError(c, test.err, "lookup")
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, test.code, body["reason"])
		require.NotEmpty(t, body["request_id"])
		require.Equal(t, body["request_id"], rec.Header().Get("X-Request-ID"))
		require.Contains(t, body["diagnostic"], test.diagnostic)
		for _, private := range []string{"private-token", "password=secret", "sk-secret123456", "private-data", "private table"} {
			require.NotContains(t, rec.Body.String(), private)
		}
	}
}

func TestSEPMediaErrorSeparatesClientIdentityFromPlatformTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/images/edits/async", "/v1/videos", "/v1/images/tasks/original?id_type=request_id"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)
			ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, "platform-trace")
			ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "sep-original")
			c.Request = c.Request.WithContext(ctx)
			WriteSEPError(c, service.SEPFailure(409, "QUOTE_MISMATCH", "Image operation differs from quote", "quote"), "receive")
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, "sep-original", body["request_id"])
			require.Equal(t, "platform-trace", body["platform_request_id"])
			require.Equal(t, "platform-trace", rec.Header().Get("X-Request-ID"))
			require.EqualValues(t, 2, body["schema_version"])
			require.Equal(t, "QUOTE_MISMATCH", body["error"].(map[string]any)["code"])
			require.Contains(t, body, "submission_error")
			_, ok := GetSEPRejectedMediaKey(c)
			require.False(t, ok, "A plain error must not authorize rejection persistence")
		})
	}
}
