package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSEPAmountsUseSixDecimalStrings(t *testing.T) {
	for _, raw := range []string{"1", "1.0", "-1.000000", "1e6", "NaN", "01.000000", "1.0000001", " 1.000000", "1000000000000000000.000000"} {
		_, err := ValidateSEPAmount(raw, false)
		require.Error(t, err, raw)
	}
	for _, raw := range []string{"0.000000", "1.000001", "999999999999999999.999999"} {
		_, err := ValidateSEPAmount(raw, false)
		require.NoError(t, err, raw)
	}
	_, err := ValidateSEPAmount("0.000000", true)
	require.Error(t, err)
}
func TestSEPServiceSignatureBindsRoutingAndBillingFields(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	r := httptest.NewRequest("POST", "/v1/chat/completions?mode=test", nil)
	for k, v := range map[string]string{"X-SEP-Key-ID": "sign-v1", "X-SEP-Timestamp": "1795150000", "X-SEP-Nonce": "nonce-1", "Authorization": "Bearer sk-test", "X-SEP-Enterprise-ID": "enterprise-1", "X-SEP-Execution-ID": "execution-1", "Idempotency-Key": "settlement-1", "Content-Type": "application/json", "X-SEP-Quote-ID": "quote-1", "X-Video-Operation": "text-to-video"} {
		r.Header.Set(k, v)
	}
	body := []byte(`{"model":"test"}`)
	signature := ed25519.Sign(private, []byte(SEPCanonicalRequest(r, body)))
	require.True(t, ed25519.Verify(pub, []byte(SEPCanonicalRequest(r, body)), signature))
	for _, header := range []string{"Authorization", "X-SEP-Enterprise-ID", "X-SEP-Execution-ID", "Idempotency-Key", "Content-Type", "X-SEP-Quote-ID", "X-Video-Operation", "X-SEP-Nonce", "X-SEP-Timestamp"} {
		copy := r.Clone(r.Context())
		copy.Header.Set(header, "tampered")
		require.False(t, ed25519.Verify(pub, []byte(SEPCanonicalRequest(copy, body)), signature), header)
	}
	require.False(t, ed25519.Verify(pub, []byte(SEPCanonicalRequest(r, []byte(`{"model":"other"}`))), signature))
	copy := r.Clone(r.Context())
	copy.URL.RawQuery = "mode=other"
	require.False(t, ed25519.Verify(pub, []byte(SEPCanonicalRequest(copy, body)), signature))
}
func TestSEPIdentifiersCannotInjectSignatureLines(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 129), "enterprise\nother", "../other", "ent/other"} {
		require.Error(t, sepID(id))
	}
	require.NoError(t, sepID("enterprise-1.test:abc"))
}
func TestSEPRequestContextKeepsFrozenPricesAndMultipliers(t *testing.T) {
	price := &ModelBasePrice{Model: "test", Version: 5}
	e := &SEPExecution{ComputeRequestID: "compute_1", Price: price, Multipliers: SEPMultipliers{Token: 1.5, Image: 2, Video: 3}}
	key := &APIKey{SEP: e}
	ctx := SEPUsageContext(t.Context(), key)
	require.Same(t, price, BasePriceSnapshot(ctx))
	require.Same(t, e, SEPExecutionFromContext(ctx))
	require.Equal(t, "compute_1", SEPRequestID(key, "old"))
	token, image, video := SEPRequestMultipliers(key, 9, 9, 9)
	require.Equal(t, 1.5, token)
	require.Equal(t, 2.0, image)
	require.Equal(t, 3.0, video)
}

func TestSEPTokenFeeRequiresUsageEvidence(t *testing.T) {
	e := &SEPExecution{Price: &ModelBasePrice{Price: AccountModelPrice{Mode: BillingModeToken}}}
	require.False(t, sepHasFeeEvidence(e, &UsageBillingCommand{}, nil))
	require.False(t, sepHasFeeEvidence(e, &UsageBillingCommand{BalanceCost: 1}, nil))
	require.True(t, sepHasFeeEvidence(e, &UsageBillingCommand{CacheReadTokens: 10}, nil))
	require.True(t, sepHasFeeEvidence(e, &UsageBillingCommand{}, &UsageLog{ImageOutputTokens: 10}))
	require.True(t, sepHasFeeEvidence(e, &UsageBillingCommand{}, &UsageLog{CacheCreation1hTokens: 10}))
	e.Price.Price.Mode = BillingModePerRequest
	require.True(t, sepHasFeeEvidence(e, &UsageBillingCommand{}, nil))
}
func TestSEPDispatchClaimsOnceAndStripsPrivateHeaders(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	e := &SEPExecution{ComputeRequestID: "compute_test", Service: &SEPService{db: db}}
	req := httptest.NewRequest(http.MethodPost, "https://upstream.test/v1/chat/completions", strings.NewReader(`{}`))
	req = req.WithContext(WithSEPExecution(req.Context(), e))
	req.Header.Set("X-SEP-Execution-ID", "private-execution")
	req.Header.Set("Idempotency-Key", "private-idempotency")
	req.Header.Set("X-Idempotency-Key", "private-idempotency")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	query := regexp.QuoteMeta(`UPDATE sep_executions SET dispatched_at=NOW(),execution_status='running',account_id=$1,resource_version=resource_version+1,updated_at=NOW() WHERE id=$2 AND dispatched_at IS NULL RETURNING id`)
	mock.ExpectQuery(query).WithArgs(int64(42), "compute_test").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("compute_test"))
	require.NoError(t, ClaimSEPUpstreamDispatch(req, 42))
	require.Empty(t, req.Header.Get("X-SEP-Execution-ID"))
	require.Empty(t, req.Header.Get("Idempotency-Key"))
	require.Empty(t, req.Header.Get("X-Idempotency-Key"))
	require.Nil(t, req.GetBody)
	mock.ExpectQuery(query).WithArgs(int64(43), "compute_test").WillReturnError(sql.ErrNoRows)
	err = ClaimSEPUpstreamDispatch(req, 43)
	var failure *SEPError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "EXECUTION_DISPATCH_ALREADY_CLAIMED", failure.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSEPDispatchDoesNotBlockTaskReadsOrOrdinaryRequests(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://upstream.test/v1/chat/completions", nil)
	require.NoError(t, ClaimSEPUpstreamDispatch(req, 42))
	e := &SEPExecution{ComputeRequestID: "compute_test", Service: &SEPService{}}
	req = httptest.NewRequest(http.MethodGet, "https://upstream.test/v1/videos/task/content", nil)
	req = req.WithContext(WithSEPExecution(req.Context(), e))
	require.NoError(t, ClaimSEPUpstreamDispatch(req, 42))
}
