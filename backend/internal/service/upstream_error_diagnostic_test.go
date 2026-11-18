package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func diagnosticTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, w
}

func TestErrorIntegrityCoreHTTPPreservesDiagnostic(t *testing.T) {
	const body = `{"error":{"message":"Unsupported size Bearer dummy-secret","code":"bad_size","type":"invalid_request_error","param":"size","details":{"allowed":["720p"]}},"request_id":"body-id"}`
	for _, mode := range []string{"anthropic", "openai", "chat", "messages", "images", "passthrough"} {
		t.Run(mode, func(t *testing.T) {
			c, w := diagnosticTestContext()
			resp := &http.Response{StatusCode: 422, Header: http.Header{"X-Request-Id": []string{"provider-id"}, "Retry-After": []string{"12"}}, Body: io.NopCloser(strings.NewReader(body))}
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			svc := &OpenAIGatewayService{}
			wantStatus := 422
			switch mode {
			case "anthropic":
				wantStatus = 502
				_, _ = (&GatewayService{}).handleErrorResponse(context.Background(), resp, c, account)
			case "openai":
				wantStatus = 502
				_, _ = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
			case "chat":
				_, _ = svc.handleCompatErrorResponse(resp, c, account, writeChatCompletionsError)
			case "messages":
				_, _ = svc.handleCompatErrorResponse(resp, c, account, writeAnthropicError)
			case "images":
				_, _ = svc.handleOpenAIImagesErrorResponse(context.Background(), resp, c, account)
			case "passthrough":
				_ = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, []byte(body))
			}
			require.Equal(t, wantStatus, w.Code)
			require.Equal(t, "provider-id", w.Header().Get("X-Upstream-Request-ID"))
			var envelope struct {
				Error map[string]any `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
			require.Equal(t, "bad_size", envelope.Error["code"])
			require.Equal(t, "invalid_request_error", envelope.Error["type"])
			require.Equal(t, "size", envelope.Error["param"])
			require.Contains(t, envelope.Error["message"], "Unsupported size")
			require.Contains(t, w.Body.String(), "allowed")
			require.NotContains(t, w.Body.String(), "dummy-secret")
		})
	}
}

func TestErrorIntegrityAttemptDoesNotPolluteLaterResponse(t *testing.T) {
	c, w := diagnosticTestContext()
	CaptureUpstreamError(c, 429, http.Header{"X-Request-Id": []string{"failed-attempt"}, "Retry-After": []string{"12"}}, []byte(`{"error":"Quota exhausted"}`))
	require.Empty(t, w.Header().Get("X-Upstream-Request-ID"))
	require.Empty(t, w.Header().Get("Retry-After"))
	fields := GatewayErrorFields(c, "storage_error", "Redis write failed")
	require.Equal(t, "Redis write failed", fields["message"])
	require.Equal(t, "platform", fields["source"])
}

func TestErrorIntegrityLegacyHTTPAndTransport(t *testing.T) {
	for _, response := range []struct {
		status int
		body   string
	}{{400, `{"error":{"code":"bad_size","message":"Unsupported size","details":{"allowed":["720p"]}}}`}, {429, `{"message":"Quota exhausted","code":1007}`}, {202, `{"code":1007,"message":"Quota exhausted"}`}} {
		svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), httpUpstream: videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: response.status, Header: http.Header{"X-Request-Id": []string{"provider-video-id"}}, Body: io.NopCloser(strings.NewReader(response.body))}, nil
		}}}
		_, err := svc.legacyRequest(context.Background(), legacyTestAccount(), "", []byte(`{}`), "")
		var failure *videoProtocolError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, "provider-video-id", failure.diagnostic["upstream_request_id"])
		require.NotContains(t, failure.message, "busy")
		require.Equal(t, extractUpstreamErrorMessage([]byte(response.body)), failure.message)
	}
	root := errors.New("TLS certificate expired at upstream")
	svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), httpUpstream: videoTestUpstream{do: func(*http.Request) (*http.Response, error) { return nil, root }}}
	_, err := svc.legacyRequest(context.Background(), legacyTestAccount(), "", []byte(`{}`), "")
	require.ErrorIs(t, err, root)
	require.Contains(t, err.Error(), "TLS certificate expired")
}

func TestErrorIntegrityHTTP200BusinessFailures(t *testing.T) {
	body := `{"error":{"code":"quota","message":"Provider balance exhausted"}}`
	c, w := diagnosticTestContext()
	svc := &OpenAIGatewayService{httpUpstream: &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}}, cfg: &config.Config{}}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "dummy-test", "base_url": "https://example.invalid"}}
	result, err := svc.ForwardEmbeddings(context.Background(), c, account, []byte(`{"model":"test-model","input":"test"}`), "")
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 502, w.Code)
	require.Contains(t, w.Body.String(), "Provider balance exhausted")
	c, w = diagnosticTestContext()
	_, _, _, err = svc.handleOpenAIImagesNonStreamingResponse(&http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, c)
	require.Error(t, err)
	require.Equal(t, 502, w.Code)
	require.Contains(t, w.Body.String(), "Provider balance exhausted")
	_, err = openAIAlphaSearchResponseFromResponsesSSE([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"quota\",\"message\":\"Provider balance exhausted\"}}}\n\n"))
	require.ErrorContains(t, err, "Provider balance exhausted")
}

func TestErrorIntegrityBatchCauseAndMetadata(t *testing.T) {
	raw := &GeminiAPIError{StatusCode: 400, Code: "INVALID_ARGUMENT", Message: "Unsupported image in projects/private/locations/global Bearer dummy-secret"}
	err := mapGeminiClientError(raw)
	require.ErrorIs(t, err, raw)
	require.Contains(t, err.Error(), "Unsupported image")
	require.NotContains(t, err.Error(), "dummy-secret")
	require.NotContains(t, err.Error(), "projects/private")
	code, message := "PROVIDER_FAILED", "Provider quota exhausted"
	out := BatchImageJobToPublic(&BatchImageJob{LastErrorCode: &code, LastErrorMessage: &message, CreatedAt: time.Now()})
	require.Equal(t, message, out.Error.Message)
}

func TestErrorIntegrityVideoDownloadSourceError(t *testing.T) {
	store := newVideoMemoryStore()
	account, key := legacyTestAccount(), &APIKey{ID: 8}
	now := time.Now().Unix()
	require.NoError(t, store.VideoBind(context.Background(), &VideoTaskMetadata{SchemaVersion: 1, TaskID: legacyTestTaskID, APIKeyID: key.ID, AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: "alias", UpstreamModel: "seedance-2.0", ExpiresAt: now + 604800, BoundAt: now}))
	svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: store, httpUpstream: videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		return legacyTestResponse(200, "seedance-2.0", "succeeded", "https://cdn.example/video.mp4?signature=dummy-signature"), nil
	}}}
	downloads := 0
	svc.videoDownloadTransport = videoRoundTripFunc(func(*http.Request) (*http.Response, error) {
		downloads++
		return &http.Response{StatusCode: 403, Header: http.Header{"X-Request-Id": []string{"storage-id"}}, Body: io.NopCloser(strings.NewReader(`<Error><Code>AccessDenied</Code><Message>Signature expired Bearer dummy-secret</Message><RequestId>xml-id</RequestId></Error>`))}, nil
	})
	c, w := videoTestContext("GET", "/v1/videos/"+legacyTestTaskID+"/content", nil, "")
	_, err := svc.ForwardVideo(context.Background(), c, account, key, nil, legacyTestTaskID, true, "")
	require.Error(t, err)
	require.Equal(t, 2, downloads)
	require.Equal(t, 502, w.Code)
	require.Equal(t, "storage-id", w.Header().Get("X-Upstream-Request-ID"))
	for _, value := range []string{"Signature expired", "AccessDenied", "storage-id", "xml-id", "video_download_error"} {
		require.Contains(t, w.Body.String(), value)
	}
	require.NotContains(t, w.Body.String(), "dummy-secret")
}

type diagnosticBrokenReader struct{ read bool }

func (r *diagnosticBrokenReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("CDN connection reset; Bearer dummy-secret")
	}
	r.read = true
	return copy(p, append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 512)...)), nil
}
func (*diagnosticBrokenReader) Close() error { return nil }

func TestErrorIntegrityVideoDownloadInterruptedLogsCause(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store := newVideoMemoryStore()
	account, key := legacyTestAccount(), &APIKey{ID: 8}
	now := time.Now().Unix()
	require.NoError(t, store.VideoBind(context.Background(), &VideoTaskMetadata{SchemaVersion: 1, TaskID: legacyTestTaskID, APIKeyID: key.ID, AccountID: account.ID, Protocol: VideoProtocolLegacy, UpstreamModel: "seedance-2.0", ExpiresAt: now + 604800, BoundAt: now}))
	svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: store, httpUpstream: videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		return legacyTestResponse(200, "seedance-2.0", "succeeded", "https://cdn.example/video.mp4"), nil
	}}}
	svc.videoDownloadTransport = videoRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/mp4"}}, Body: &diagnosticBrokenReader{}}, nil
	})
	c, w := videoTestContext("GET", "/v1/videos/"+legacyTestTaskID+"/content", nil, "")
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		_, _ = svc.ForwardVideo(context.Background(), c, account, key, nil, legacyTestTaskID, true, "")
	})
	require.Contains(t, logs.String(), "CDN connection reset")
	require.NotContains(t, logs.String(), "dummy-secret")
	require.NotContains(t, w.Body.String(), "CDN connection reset")
}

func TestErrorIntegrityLegacyFailedTaskRetainsNumericCodeAndFields(t *testing.T) {
	var task legacyVideoTask
	require.NoError(t, json.Unmarshal([]byte(`{"id":"task","error":{"code":1203,"message":"Reference image rejected","type":"validation_error","param":"images","details":{"limit":9},"request_id":"task-error-id"}}`), &task))
	fields := legacyTaskFailure(&task, "")
	require.Equal(t, json.Number("1203"), fields["code"])
	require.Equal(t, "Reference image rejected", fields["message"])
	require.Equal(t, "validation_error", fields["type"])
	require.Equal(t, "task-error-id", fields["request_id"])
	require.NotNil(t, fields["details"])
}

func TestErrorIntegrityWSHTTPBridgeFailure(t *testing.T) {
	c, _ := diagnosticTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: 422, Header: http.Header{"X-Request-Id": []string{"ws-provider-id"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Unsupported image; Bearer dummy-secret","type":"invalid_request_error","code":"bad_image","param":"input","details":{"limit":9}}}`)),
	}}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	var frames [][]byte
	_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "dummy-token", []byte(`{"type":"response.create","model":"test","input":"hello"}`), 0, "test", "", "", "", "", 1, func(frame []byte) error { frames = append(frames, frame); return nil })
	require.Error(t, err)
	require.Len(t, frames, 1)
	for _, value := range []string{"Unsupported image", "bad_image", "invalid_request_error", "details", "ws-provider-id"} {
		require.Contains(t, string(frames[0]), value)
	}
	require.NotContains(t, string(frames[0]), "dummy-secret")
}

func TestErrorIntegrityFailoverAttemptLimitAndPlatformSource(t *testing.T) {
	c, _ := diagnosticTestContext()
	for i := 0; i < 35; i++ {
		failure := platformFailoverError(502, "transport_error", "Send request", errors.New("TLS certificate expired Bearer dummy-secret"))
		RecordFailoverDiagnostic(c, failure)
		CaptureFailoverError(c, failure)
	}
	fields := GatewayErrorFields(c, "upstream_error", "fallback")
	require.Equal(t, "platform", fields["source"])
	require.Equal(t, 0, fields["upstream_status"])
	require.Equal(t, 3, fields["earlier_attempts_omitted"])
	require.Len(t, fields["attempts"], 32)
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "dummy-secret")
	require.Contains(t, string(encoded), "TLS certificate expired")
	CaptureUpstreamError(c, 422, nil, []byte(`{"message":"Invalid size","source":"platform"}`))
	UseUpstreamError(c)
	require.Equal(t, "upstream", GatewayErrorFields(c, "api_error", "")["source"])
}

func TestErrorIntegrityGeminiDownloadValidationDoesNotPanic(t *testing.T) {
	require.ErrorContains(t, validateGeminiDownloadHost("http://files.example/image.png", "https://api.example"), "must use https")
	require.ErrorContains(t, validateGeminiDownloadHost("https://untrusted.example/image.png", "https://api.example"), "host is not allowed")
}

func TestErrorIntegrityGeminiNonStreamingLocalCause(t *testing.T) {
	for _, mode := range []string{"messages_read", "messages_parse", "chat_parse"} {
		t.Run(mode, func(t *testing.T) {
			c, w := diagnosticTestContext()
			response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{]"))}
			svc := &GeminiMessagesCompatService{cfg: &config.Config{}}
			var err error
			reason := "invalid character"
			switch mode {
			case "messages_read":
				response.Body = &diagnosticBrokenReader{read: true}
				reason = "CDN connection reset"
				_, err = svc.handleNonStreamingResponse(c, response, "gemini")
			case "messages_parse":
				_, err = svc.handleNonStreamingResponse(c, response, "gemini")
			case "chat_parse":
				_, err = svc.handleChatCompletionsNonStreamingResponseFromGemini(c, response, "gemini", false)
			}
			require.Error(t, err)
			require.Equal(t, 502, w.Code)
			require.Contains(t, w.Body.String(), reason)
			require.NotContains(t, w.Body.String(), "dummy-secret")
		})
	}
}
