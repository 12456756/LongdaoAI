package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func videoContractPtr[T any](value T) *T { return &value }

func newVideoPricingHandlerForTest(channel *service.Channel) (*GatewayHandler, *service.APIKey) {
	h, key := newImagePricingHandlerForTest(channel)
	h.gatewayService = newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		*key.GroupID: {{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{
			"model_mapping": map[string]any{
				"minimax-h3":        "minimax-h3",
				"seedance-2.0-fast": "seedance-2.0-fast",
				"seedance-2.5":      "seedance-2.5",
			},
		}}},
	}}).gatewayService
	key.Group.VideoRateIndependent, key.Group.VideoRateMultiplier = true, 0.5
	return h, key
}

func videoBillingHTTP(t *testing.T, h *GatewayHandler, key *service.APIKey, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	if key != nil {
		ctx.Set(string(middleware2.ContextKeyAPIKey), key)
	}
	if method == http.MethodPost {
		h.VideoPricing(ctx)
	} else {
		h.RequestVideoUsage(ctx)
	}
	return recorder
}

const validVideoPricingJSON = `{"model":"minimax-h3","operation":"image-to-video","resolution":"720p","duration_seconds":4}`

func TestVideoPricingHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		channel    *service.Channel
		groupPrice *float64
		mode       service.BillingMode
		source     string
		amount     *string
	}{
		{name: "group per second", groupPrice: videoContractPtr(0.0324), mode: service.BillingModeVideo, source: "group", amount: videoContractPtr("0.0648000000")},
		{name: "explicit free", groupPrice: videoContractPtr(0.0), mode: service.BillingModeVideo, source: "group", amount: videoContractPtr("0.0000000000")},
		{name: "channel per request", channel: &service.Channel{ModelPricing: []service.ChannelModelPricing{{Models: []string{"minimax-h3"}, BillingMode: service.BillingModePerRequest, PerRequestPrice: videoContractPtr(0.4)}}}, mode: service.BillingModePerRequest, source: "channel", amount: videoContractPtr("0.2000000000")},
		{name: "fallback unavailable", mode: service.BillingModeVideo, source: "fallback"},
		{name: "token unavailable", groupPrice: videoContractPtr(0.1), channel: &service.Channel{ModelPricing: []service.ChannelModelPricing{{Models: []string{"minimax-h3"}, BillingMode: service.BillingModeToken}}}, mode: service.BillingModeVideo, source: "channel_token_unquotable"},
		{name: "upstream unresolved", groupPrice: videoContractPtr(0.1), channel: &service.Channel{BillingModelSource: service.BillingModelSourceUpstream}, mode: service.BillingModeVideo, source: "upstream_model_unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, key := newVideoPricingHandlerForTest(tc.channel)
			key.Group.VideoPrice720P = tc.groupPrice
			recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", validVideoPricingJSON)
			require.Equal(t, 200, recorder.Code, recorder.Body.String())
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			var response videoPricingResponse
			var raw map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
			keys := make([]string, 0, len(raw))
			for key := range raw {
				keys = append(keys, key)
			}
			require.ElementsMatch(t, []string{"object", "schema_version", "currency", "model", "operation", "resolution", "duration_seconds", "billing_mode", "pricing_status", "pricing_source", "pricing_authority", "amount_usd", "pricing_version", "effective_rate_multiplier", "observed_at", "expires_at", "request_id", "task_id", "submission_status", "status", "is_final", "status_observed_at", "status_source", "task_error", "operation_error", "error", "billing", "submission_retry", "submission_error", "receipt_operation"}, keys)
			require.Equal(t, "sub2api.video_pricing", response.Object)
			require.Equal(t, 2, response.SchemaVersion)
			require.Equal(t, "USD", response.Currency)
			require.Equal(t, videoPricingRequest{"minimax-h3", "image-to-video", "720p", 4}, response.videoPricingRequest)
			require.Equal(t, tc.mode, response.BillingMode)
			require.Equal(t, tc.source, response.PricingSource)
			require.Equal(t, service.PricingAuthoritySub2APICustomerBilling, response.PricingAuthority)
			require.Equal(t, tc.amount, response.AmountUSD)
			if tc.amount != nil {
				require.Equal(t, "quoted", response.PricingStatus)
			} else {
				require.Equal(t, "unavailable", response.PricingStatus)
				require.Nil(t, raw["amount_usd"])
			}
			require.Equal(t, 0.5, response.EffectiveRateMultiplier)
			require.Equal(t, time.Minute, response.ExpiresAt.Sub(response.ObservedAt))
			require.Regexp(t, `^v1:[a-f0-9]{64}$`, response.PricingVersion)
		})
	}
}

func TestVideoPricingHTTPStrictInput(t *testing.T) {
	h, key := newVideoPricingHandlerForTest(nil)
	for _, body := range []string{
		`null`, `{}`, `[]`, validVideoPricingJSON + `{}`, strings.TrimSuffix(validVideoPricingJSON, "}") + `,"url":"http://internal"}`,
		strings.Replace(validVideoPricingJSON, `"duration_seconds":4`, `"duration_seconds":4.5`, 1),
		strings.Replace(validVideoPricingJSON, `"duration_seconds":4`, `"duration_seconds":"4"`, 1),
		strings.Replace(validVideoPricingJSON, `"duration_seconds":4`, `"duration_seconds":0`, 1),
		strings.Replace(validVideoPricingJSON, `"duration_seconds":4`, `"duration_seconds":-2`, 1),
		strings.Replace(validVideoPricingJSON, `"duration_seconds":4`, `"duration_seconds":2147483648`, 1),
		strings.Replace(validVideoPricingJSON, `"duration_seconds":4`, `"duration_seconds":601`, 1),
		strings.Replace(validVideoPricingJSON, "720p", "8k", 1), strings.Replace(validVideoPricingJSON, "720p", "768p", 1),
		strings.Replace(validVideoPricingJSON, "720p", "hd", 1), strings.Replace(validVideoPricingJSON, "image-to-video", "edit", 1),
		strings.Replace(validVideoPricingJSON, "minimax-h3", "minimax h3", 1), strings.Replace(validVideoPricingJSON, "minimax-h3", `minimax\u0000h3`, 1),
		strings.Replace(validVideoPricingJSON, "minimax-h3", strings.Repeat("x", 4096), 1),
		strings.Replace(validVideoPricingJSON, "minimax-h3", strings.Repeat("x", 161), 1),
	} {
		t.Run(body[:min(len(body), 100)], func(t *testing.T) {
			recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", body)
			require.Equal(t, 400, recorder.Code, recorder.Body.String())
		})
	}
	for _, resolution := range []string{"480p", "720p", "1080p"} {
		body := strings.Replace(validVideoPricingJSON, "720p", resolution, 1)
		recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", body)
		require.Equal(t, 200, recorder.Code, recorder.Body.String())
	}
	recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", strings.Replace(validVideoPricingJSON, "image-to-video", "text-to-video", 1))
	require.Equal(t, 200, recorder.Code)
}

func TestVideoToVideoPricingHTTP(t *testing.T) {
	h, key := newVideoPricingHandlerForTest(nil)
	key.Group.VideoPrice720P = videoContractPtr(0.1)
	request := `{"model":"seedance-2.0-fast","operation":"video-to-video","resolution":"720p","duration_seconds":5}`
	recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"operation":"video-to-video"`)
	require.Contains(t, recorder.Body.String(), `"pricing_status":"quoted"`)

	for _, seconds := range []string{"3", "16"} {
		invalid := strings.Replace(request, `"duration_seconds":5`, `"duration_seconds":`+seconds, 1)
		recorder = videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", invalid)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	}

	auto := strings.Replace(request, `"duration_seconds":5`, `"duration_seconds":-1`, 1)
	recorder = videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", auto)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"pricing_status":"unavailable"`)

	channel := &service.Channel{ModelPricing: []service.ChannelModelPricing{{
		Models: []string{"minimax-h3"}, BillingMode: service.BillingModePerRequest, PerRequestPrice: videoContractPtr(0.4),
	}}}
	h, key = newVideoPricingHandlerForTest(channel)
	key.Group.VideoPrice720P = videoContractPtr(0.1)
	auto = strings.Replace(auto, "seedance-2.0-fast", "minimax-h3", 1)
	for _, resolution := range []string{"720p", "2k", "4k"} {
		recorder = videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", strings.Replace(auto, "720p", resolution, 1))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Contains(t, recorder.Body.String(), `"billing_mode":"per_request"`)
		require.Contains(t, recorder.Body.String(), `"amount_usd":"0.2000000000"`)
	}
}

func TestVideoPricingVersionStableAndBindsInputAndPrice(t *testing.T) {
	h, key := newVideoPricingHandlerForTest(nil)
	key.Group.VideoPrice720P = videoContractPtr(0.03)
	version := func(body string) string {
		recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", body)
		require.Equal(t, 200, recorder.Code)
		var response videoPricingResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		return response.PricingVersion
	}
	first := version(validVideoPricingJSON)
	require.Equal(t, first, version(validVideoPricingJSON))
	require.NotEqual(t, first, version(strings.Replace(validVideoPricingJSON, "image-to-video", "text-to-video", 1)))
	require.NotEqual(t, first, version(strings.Replace(validVideoPricingJSON, ":4", ":5", 1)))
	key.Group.VideoPrice720P = videoContractPtr(0.04)
	require.NotEqual(t, first, version(validVideoPricingJSON))
}

func TestVideoBillingHTTPAuthenticationAndAvailability(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, tc := range []struct {
			name   string
			change func(*GatewayHandler, *service.APIKey) *service.APIKey
			status int
		}{
			{name: "missing key", change: func(_ *GatewayHandler, _ *service.APIKey) *service.APIKey { return nil }, status: 401},
			{name: "missing user", change: func(_ *GatewayHandler, key *service.APIKey) *service.APIKey { key.UserID = 0; return key }, status: 401},
			{name: "wrong group identity", change: func(_ *GatewayHandler, key *service.APIKey) *service.APIKey { key.Group.ID++; return key }, status: 403},
			{name: "wrong platform", change: func(_ *GatewayHandler, key *service.APIKey) *service.APIKey {
				key.Group.Platform = service.PlatformAnthropic
				return key
			}, status: 403},
			{name: "simple mode", change: func(h *GatewayHandler, key *service.APIKey) *service.APIKey {
				h.cfg = &config.Config{RunMode: config.RunModeSimple}
				return key
			}, status: 404},
		} {
			t.Run(method+tc.name, func(t *testing.T) {
				h, key := newVideoPricingHandlerForTest(nil)
				key = tc.change(h, key)
				recorder := videoBillingHTTP(t, h, key, method, "/v1/sub2api/videos/usage?request_id=req-1", validVideoPricingJSON)
				require.Equal(t, tc.status, recorder.Code)
			})
		}
	}
	h, key := newVideoPricingHandlerForTest(nil)
	recorder := videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", strings.Replace(validVideoPricingJSON, "minimax-h3", "unauthorized-model", 1))
	require.Equal(t, 403, recorder.Code)
	key.Group.AllowImageGeneration = false
	recorder = videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", validVideoPricingJSON)
	require.Equal(t, 403, recorder.Code)
}

type videoUsageBillingRepo struct {
	service.UsageBillingRepository
	evidence      *service.VideoRequestBillingEvidence
	err           error
	keyID, userID int64
	ids           []string
}

func (r *videoUsageBillingRepo) FindVideoRequestBillingEvidence(_ context.Context, keyID, userID int64, ids []string) (*service.VideoRequestBillingEvidence, error) {
	r.keyID, r.userID, r.ids = keyID, userID, ids
	return r.evidence, r.err
}

func validVideoEvidence() *service.VideoRequestBillingEvidence {
	return &service.VideoRequestBillingEvidence{Model: videoContractPtr("minimax-h3"), BillingMode: "video", VideoCount: videoContractPtr(1),
		DurationSeconds: videoContractPtr(4), Resolution: videoContractPtr("720p"), ActualCostUSD: videoContractPtr("0.1296000000"), TransactionConfirmed: true}
}

func newVideoUsageHandlerForTest(repo *videoUsageBillingRepo) (*GatewayHandler, *service.APIKey) {
	h, key := newVideoPricingHandlerForTest(nil)
	h.openAIGatewayService = service.NewOpenAIGatewayService(nil, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	return h, key
}

func TestVideoUsageHTTPSettlesOnlyReliableVideoEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence
		want   string
	}{
		{name: "confirmed", want: "settled"},
		{name: "confirmed zero", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = videoContractPtr("0")
			return e
		}, want: "settled"},
		{name: "absent or ambiguous", change: func(_ *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence { return nil }, want: "unknown"},
		{name: "dedup missing", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.TransactionConfirmed = false
			return e
		}, want: "unknown"},
		{name: "image", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.BillingMode = "image"
			return e
		}, want: "unknown"},
		{name: "per request is not video evidence", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.BillingMode = "per_request"
			return e
		}, want: "unknown"},
		{name: "token", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.BillingMode = "token"
			return e
		}, want: "unknown"},
		{name: "missing model", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.Model = nil
			return e
		}, want: "unknown"},
		{name: "missing amount", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = nil
			return e
		}, want: "unknown"},
		{name: "negative amount", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = videoContractPtr("-0.1")
			return e
		}, want: "unknown"},
		{name: "negative underflow must not become zero", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = videoContractPtr("-1e-9999")
			return e
		}, want: "unknown"},
		{name: "non decimal amount", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = videoContractPtr("not-money")
			return e
		}, want: "unknown"},
		{name: "nan", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = videoContractPtr("NaN")
			return e
		}, want: "unknown"},
		{name: "infinity", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.ActualCostUSD = videoContractPtr("1e9999")
			return e
		}, want: "unknown"},
		{name: "no videos", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.VideoCount = videoContractPtr(0)
			return e
		}, want: "unknown"},
		{name: "missing count", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.VideoCount = nil
			return e
		}, want: "unknown"},
		{name: "missing duration", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.DurationSeconds = nil
			return e
		}, want: "unknown"},
		{name: "zero duration", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.DurationSeconds = videoContractPtr(0)
			return e
		}, want: "unknown"},
		{name: "confirmed automatic duration", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.DurationSeconds = videoContractPtr(service.VideoAutoDuration)
			e.Resolution = videoContractPtr("2k")
			return e
		}, want: "settled"},
		{name: "missing resolution", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.Resolution = nil
			return e
		}, want: "unknown"},
		{name: "unsupported resolution", change: func(e *service.VideoRequestBillingEvidence) *service.VideoRequestBillingEvidence {
			e.Resolution = videoContractPtr("8k")
			return e
		}, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := validVideoEvidence()
			if tc.change != nil {
				evidence = tc.change(evidence)
			}
			repo := &videoUsageBillingRepo{evidence: evidence}
			h, key := newVideoUsageHandlerForTest(repo)
			recorder := videoBillingHTTP(t, h, key, http.MethodGet, "/v1/sub2api/videos/usage?request_id=client:req-1", "")
			require.Equal(t, 200, recorder.Code, recorder.Body.String())
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			var response videoRequestUsageResponse
			var raw map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
			keys := make([]string, 0, len(raw))
			for key := range raw {
				keys = append(keys, key)
			}
			require.ElementsMatch(t, []string{"object", "schema_version", "currency", "request_id", "status", "model", "actual_cost_usd", "video_count", "duration_seconds", "resolution", "operation", "task_id", "submission_status", "is_final", "status_observed_at", "status_source", "task_error", "operation_error", "error", "billing", "submission_retry", "submission_error", "receipt_operation"}, keys)
			require.Equal(t, "sub2api.video_request_usage", response.Object)
			require.Equal(t, 2, response.SchemaVersion)
			require.Equal(t, "USD", response.Currency)
			require.Equal(t, "client:req-1", response.RequestID)
			require.Equal(t, tc.want, response.Status)
			require.Equal(t, key.ID, repo.keyID)
			require.Equal(t, key.UserID, repo.userID)
			require.Equal(t, []string{"client:req-1"}, repo.ids)
			if tc.want == "settled" {
				require.Equal(t, evidence.ActualCostUSD, response.ActualCostUSD)
				require.Equal(t, evidence.Model, response.Model)
				require.Equal(t, evidence.VideoCount, response.VideoCount)
			} else {
				require.Nil(t, raw["actual_cost_usd"])
			}
			if evidence == nil {
				for _, field := range []string{"model", "video_count", "duration_seconds", "resolution"} {
					require.Contains(t, raw, field)
					require.Nil(t, raw[field])
				}
			}
		})
	}
}

func TestVideoUsageHTTPRejectsInvalidRequestIDAndPreservesRepositoryError(t *testing.T) {
	repo := &videoUsageBillingRepo{err: errors.New("private database diagnostic")}
	h, key := newVideoUsageHandlerForTest(repo)
	for _, query := range []string{"", "request_id=", "request_id=a&request_id=b", "request_id=%0Areq-1", "request_id=with%20space", "request_id=%20req-1", "request_id=invalid%2Fpath", "request_id=" + url.QueryEscape(strings.Repeat("a", 257))} {
		recorder := videoBillingHTTP(t, h, key, http.MethodGet, "/v1/sub2api/videos/usage?"+query, "")
		require.Equal(t, 400, recorder.Code)
	}
	recorder := videoBillingHTTP(t, h, key, http.MethodGet, "/v1/sub2api/videos/usage?request_id=req-1", "")
	require.Equal(t, 503, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Read video request billing: private database diagnostic")
}

func TestVideoUsageHTTPStrictSEPProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		field  string
		change func(*service.VideoRequestBillingEvidence)
	}{
		{name: "oversized model", field: "model", change: func(e *service.VideoRequestBillingEvidence) { e.Model = videoContractPtr(strings.Repeat("x", 161)) }},
		{name: "control in model", field: "model", change: func(e *service.VideoRequestBillingEvidence) { e.Model = videoContractPtr("video\nmodel") }},
		{name: "negative count", field: "video_count", change: func(e *service.VideoRequestBillingEvidence) { e.VideoCount = videoContractPtr(-1) }},
		{name: "nonpositive duration", field: "duration_seconds", change: func(e *service.VideoRequestBillingEvidence) { e.DurationSeconds = videoContractPtr(0) }},
		{name: "oversized duration", field: "duration_seconds", change: func(e *service.VideoRequestBillingEvidence) { e.DurationSeconds = videoContractPtr(601) }},
		{name: "unknown resolution", field: "resolution", change: func(e *service.VideoRequestBillingEvidence) { e.Resolution = videoContractPtr("not-a-resolution") }},
		{name: "excess decimal precision", field: "actual_cost_usd", change: func(e *service.VideoRequestBillingEvidence) { e.ActualCostUSD = videoContractPtr("0.1234567890123") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := validVideoEvidence()
			tc.change(evidence)
			h, key := newVideoUsageHandlerForTest(&videoUsageBillingRepo{evidence: evidence})
			recorder := videoBillingHTTP(t, h, key, http.MethodGet, "/v1/sub2api/videos/usage?request_id=client:original", "")
			require.Equal(t, http.StatusOK, recorder.Code)
			var raw map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
			require.Equal(t, "unknown", raw["status"])
			require.Contains(t, raw, tc.field)
			require.Nil(t, raw[tc.field])
			require.Nil(t, raw["actual_cost_usd"])
		})
	}
}

// Verbose test output can be piped into SEP's actual adapter without a server,
// credentials, fixture files, or upstream traffic.
func TestVideoBillingSEPWireFixtures(t *testing.T) {
	emit := func(kind string, accepted bool, recorder *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		wire, err := json.Marshal(struct {
			Kind     string          `json:"kind"`
			Accepted bool            `json:"accepted"`
			Value    json.RawMessage `json:"value"`
		}{kind, accepted, recorder.Body.Bytes()})
		require.NoError(t, err)
		t.Logf("SEP_VIDEO_WIRE %s", wire)
	}
	for _, tc := range []struct {
		channel *service.Channel
		price   *float64
		quoted  bool
	}{
		{price: videoContractPtr(0.0324), quoted: true},
		{price: videoContractPtr(0.0), quoted: true},
		{channel: &service.Channel{ModelPricing: []service.ChannelModelPricing{{Models: []string{"minimax-h3"}, BillingMode: service.BillingModePerRequest, PerRequestPrice: videoContractPtr(0.4)}}}, quoted: true},
		{},
	} {
		h, key := newVideoPricingHandlerForTest(tc.channel)
		key.Group.VideoPrice720P = tc.price
		body := strings.Replace(validVideoPricingJSON, "image-to-video", "text-to-video", 1)
		emit("price", tc.quoted, videoBillingHTTP(t, h, key, http.MethodPost, "/v1/sub2api/videos/pricing", body))
	}
	for _, evidence := range []*service.VideoRequestBillingEvidence{validVideoEvidence(), nil, {
		Model: videoContractPtr("invalid\nmodel"), VideoCount: videoContractPtr(-1), DurationSeconds: videoContractPtr(601), Resolution: videoContractPtr("unrecognized"),
	}} {
		h, key := newVideoUsageHandlerForTest(&videoUsageBillingRepo{evidence: evidence})
		emit("usage", true, videoBillingHTTP(t, h, key, http.MethodGet, "/v1/sub2api/videos/usage?request_id=client:original", ""))
	}
}
