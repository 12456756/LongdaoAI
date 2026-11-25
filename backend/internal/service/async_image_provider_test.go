package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type asyncFixtureStore struct {
	mu     sync.Mutex
	record *ImageTaskRecord
}

func (s *asyncFixtureStore) Save(_ context.Context, r *ImageTaskRecord, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record != nil && (s.record.Status == ImageTaskStatusCompleted || s.record.Status == ImageTaskStatusFailed ||
		(s.record.Provider != nil && s.record.Provider.SubmissionStatus == "accepted" && r.Provider.SubmissionStatus != "accepted")) {
		return nil
	}
	data, _ := json.Marshal(r)
	_ = json.Unmarshal(data, &s.record)
	return nil
}
func (s *asyncFixtureStore) Get(_ context.Context, _ string) (*ImageTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return nil, ErrImageTaskNotFound
	}
	data, _ := json.Marshal(s.record)
	var r ImageTaskRecord
	_ = json.Unmarshal(data, &r)
	return &r, nil
}
func (s *asyncFixtureStore) Claim(ctx context.Context, r *ImageTaskRecord, ttl time.Duration) (*ImageTaskRecord, bool, error) {
	s.mu.Lock()
	existing := s.record != nil
	s.mu.Unlock()
	if existing {
		old, err := s.Get(ctx, r.ID)
		return old, false, err
	}
	if err := s.Save(ctx, r, ttl); err != nil {
		return nil, false, err
	}
	return r, true, nil
}
func (s *asyncFixtureStore) FindClaim(ctx context.Context, _ int64, _, _ string) (*ImageTaskRecord, error) {
	r, err := s.Get(ctx, "")
	if errors.Is(err, ErrImageTaskNotFound) {
		return nil, nil
	}
	return r, err
}

func asyncFixture(t *testing.T, upstream func(*http.Request) (*http.Response, error)) (*OpenAIGatewayService, *ImageTaskService, *APIKey, *Account, *OpenAIImagesRequest, []byte) {
	t.Helper()
	groupID := int64(3)
	key := &APIKey{ID: 7, UserID: 11, User: &User{ID: 11}, GroupID: &groupID, Group: &Group{ID: 3, Platform: PlatformOpenAI}}
	account := &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, GroupIDs: []int64{3}, Credentials: map[string]any{"api_key": "fixture-image-credential", "base_url": "https://upstream.example/v1", "image_protocol": ImageProtocolNaoshengyun}}
	svc := &OpenAIGatewayService{cfg: &config.Config{RunMode: config.RunModeSimple}, httpUpstream: videoTestUpstream{do: upstream}, accountRepo: videoTestAccountRepo{account: account}}
	tasks := NewImageTaskService(&asyncFixtureStore{})
	parsed := &OpenAIImagesRequest{Endpoint: openAIImagesGenerationsEndpoint, Model: "gpt-image-2-1k", ExplicitModel: true, Prompt: "draw ocean", N: 1, Size: "1024x1024"}
	payload, err := BuildAsyncImagePayload(parsed, parsed.Model, 0)
	require.NoError(t, err)
	return svc, tasks, key, account, parsed, payload
}

func asyncResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"provider-fixture"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const asyncQueued = `{"id":"imgjob_fixture","object":"image.generation.job","status":"queued","created":123}`

func TestAsyncImageLifecycleNoBackgroundPollingAndOriginalAccount(t *testing.T) {
	posts, queries := 0, 0
	failQuery := false
	complete := false
	svc, tasks, key, account, parsed, payload := asyncFixture(t, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer fixture-image-credential", r.Header.Get("Authorization"))
		if r.Method == "POST" {
			posts++
			require.Equal(t, "/v1/images/generations/async", r.URL.Path)
			data, _ := io.ReadAll(r.Body)
			require.Contains(t, string(data), `"model":"gpt-image-2-1k"`)
			return asyncResponse(202, asyncQueued), nil
		}
		queries++
		require.Equal(t, "/v1/images/jobs/imgjob_fixture", r.URL.Path)
		if failQuery {
			return asyncResponse(503, `{"error":{"message":"status database overloaded","code":"status_busy","details":{"retryable":true}}}`), nil
		}
		if complete {
			return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"succeeded","created":123,"data":[{"url":"https://images.example/image.png?signature=private"}]}`), nil
		}
		return asyncResponse(200, asyncQueued), nil
	})
	c, _ := videoTestContext("POST", "/v1/images/generations/async", nil, "application/json")
	task, err := svc.SubmitAsyncImage(context.Background(), c, tasks, key, account, parsed, payload, "request-one", "request-one", nil)
	require.NoError(t, err)
	require.Equal(t, "accepted", task.SubmissionStatus)
	require.Equal(t, 1, posts)
	require.Zero(t, queries)
	replay, found, err := svc.ReplayAsyncImage(context.Background(), tasks, key, parsed, payload, "request-one", "request-one")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, task.ID, replay.ID)
	require.Equal(t, 1, posts)
	_, err = tasks.LoadRecord(context.Background(), ImageTaskOwner{UserID: 11, APIKeyID: 8}, task.ID)
	require.ErrorIs(t, err, ErrImageTaskNotFound)
	owner := ImageTaskOwner{UserID: 11, APIKeyID: 7}
	record, err := tasks.LoadRecord(context.Background(), owner, task.ID)
	require.NoError(t, err)
	failQuery = true
	got, err := svc.PollAsyncImage(context.Background(), c, tasks, key, record, account, nil)
	require.ErrorContains(t, err, "status database overloaded")
	require.Equal(t, "processing", got.Status)
	record, err = tasks.LoadRecord(context.Background(), owner, task.ID)
	require.NoError(t, err)
	require.Equal(t, "processing", record.Status)
	failQuery = false
	complete = true
	got, err = svc.PollAsyncImage(context.Background(), c, tasks, key, record, account, nil)
	require.NoError(t, err)
	require.Equal(t, "completed", got.Status)
	require.Equal(t, "/v1/images/tasks/"+task.ID+"/content", got.ContentURL)
	public, _ := json.Marshal(got)
	require.NotContains(t, string(public), "images.example")
	require.NotContains(t, string(public), "private")
	record, _ = tasks.LoadRecord(context.Background(), owner, task.ID)
	_, err = svc.PollAsyncImage(context.Background(), c, tasks, key, record, account, nil)
	require.NoError(t, err)
	require.Equal(t, 2, queries)
	other := *key
	other.GroupID = imageBillingInt64Ptr(9)
	_, err = svc.AsyncImageTaskAccount(context.Background(), &other, record)
	require.ErrorIs(t, err, ErrImageTaskNotFound)
}

func imageBillingInt64Ptr(v int64) *int64 { return &v }

func TestAsyncImageConcurrentReceiptWinsInterruptedLookupWithoutPollingUnacquiredAccount(t *testing.T) {
	svc, tasks, key, _, _, _ := asyncFixture(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("Stale submission lookup must not poll without acquiring the original account")
		return nil, errors.New("unexpected poll")
	})
	record, _, err := tasks.ClaimAsync(context.Background(), key, &AsyncImageMetadata{RequestID: "receipt-race", IdempotencyKey: "receipt-race", GroupID: *key.GroupID, AccountID: 5, SubmissionStatus: "submitting", DispatchDeadline: time.Now().Add(-time.Minute).Unix()})
	require.NoError(t, err)
	stale, err := tasks.LoadRecord(context.Background(), ImageTaskOwner{UserID: key.UserID, APIKeyID: key.ID}, record.ID)
	require.NoError(t, err)
	record.Provider.SubmissionStatus = "accepted"
	record.Provider.UpstreamTaskID = "imgjob_fixture"
	record.Provider.UpstreamStatus = "queued"
	require.NoError(t, tasks.SaveAsync(context.Background(), record))
	c, _ := videoTestContext("GET", "/v1/images/tasks/receipt-race?id_type=request_id", nil, "")
	got, err := svc.PollAsyncImage(context.Background(), c, tasks, key, stale, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "accepted", got.SubmissionStatus)
	require.Equal(t, "processing", got.Status)
	require.Equal(t, record.ID, got.ID)
}

func TestAsyncImageCreationFailuresCloseWithoutCharge(t *testing.T) {
	for _, scenario := range []string{"detailed", "transport", "timeout", "invalid_receipt", "wrong_http", "business_error"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			svc, tasks, key, account, parsed, payload := asyncFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				switch scenario {
				case "detailed":
					return asyncResponse(422, `{"error":{"message":"size is unsupported fixture-image-credential","code":"bad_size","type":"invalid_request_error","param":"size","details":{"allowed":["1024x1024"]}}}`), nil
				case "transport":
					return nil, errors.New("TLS connection reset by peer")
				case "timeout":
					return nil, context.DeadlineExceeded
				case "invalid_receipt":
					return asyncResponse(202, `{"status":"queued"}`), nil
				case "wrong_http":
					return asyncResponse(200, asyncQueued), nil
				default:
					return asyncResponse(202, `{"error":"generation is disabled"}`), nil
				}
			})
			c, _ := videoTestContext("POST", "/v1/images/generations/async", nil, "application/json")
			task, err := svc.SubmitAsyncImage(context.Background(), c, tasks, key, account, parsed, payload, "r1", "r1", nil)
			require.Error(t, err)
			require.Equal(t, "failed", task.Status)
			require.Equal(t, "failed", task.SubmissionStatus)
			require.Equal(t, "not_charged", task.Billing.Status)
			if scenario == "detailed" {
				require.Contains(t, string(task.Error), "bad_size")
				require.Contains(t, string(task.Error), "allowed")
				require.NotContains(t, string(task.Error), "fixture-image-credential")
			}
			_, _, err = svc.ReplayAsyncImage(context.Background(), tasks, key, parsed, payload, "r1", "r1")
			require.Error(t, err)
			require.Equal(t, 1, calls)
		})
	}
}

func TestAsyncImageFailedJobIsTaskFailureNotQueryFailure(t *testing.T) {
	svc, tasks, key, account, parsed, payload := asyncFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			return asyncResponse(202, asyncQueued), nil
		}
		return asyncResponse(200, `{"id":"imgjob_fixture","object":"image.generation.job","status":"failed","created":123,"error":{"message":"reference image violates upstream policy","code":"policy_rejected","details":{"rule":"image-policy"}}}`), nil
	})
	c, _ := videoTestContext("POST", "/v1/images/generations/async", nil, "application/json")
	task, err := svc.SubmitAsyncImage(context.Background(), c, tasks, key, account, parsed, payload, "r1", "r1", nil)
	require.NoError(t, err)
	record, _ := tasks.LoadRecord(context.Background(), ImageTaskOwner{UserID: 11, APIKeyID: 7}, task.ID)
	got, err := svc.PollAsyncImage(context.Background(), c, tasks, key, record, account, nil)
	require.NoError(t, err)
	require.Equal(t, "failed", got.Status)
	require.Equal(t, "accepted", got.SubmissionStatus)
	require.Contains(t, string(got.Error), "policy_rejected")
	require.Equal(t, "not_charged", got.Billing.Status)
}

func TestAsyncImageReferencesAndStrictParsing(t *testing.T) {
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	var multipartBytes bytes.Buffer
	writer := multipart.NewWriter(&multipartBytes)
	for i := 0; i < 4; i++ {
		part, err := writer.CreateFormFile("image[]", "ref.png")
		require.NoError(t, err)
		_, err = part.Write(pngBytes.Bytes())
		require.NoError(t, err)
	}
	for k, v := range map[string]string{"model": "gpt-image-2-1k", "prompt": "ocean", "n": "1", "response_format": "url"} {
		require.NoError(t, writer.WriteField(k, v))
	}
	require.NoError(t, writer.Close())
	svc := &OpenAIGatewayService{}
	c, _ := videoTestContext("POST", "/v1/images/edits/async", multipartBytes.Bytes(), writer.FormDataContentType())
	parsed, err := svc.ParseAsyncImageRequest(c, multipartBytes.Bytes())
	require.NoError(t, err)
	payload, err := BuildAsyncImagePayload(parsed, parsed.Model, 0)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	require.Len(t, fields["image_urls"], 4)
	parsed.Uploads = append(parsed.Uploads, parsed.Uploads[0])
	_, err = BuildAsyncImagePayload(parsed, parsed.Model, 0)
	require.ErrorContains(t, err, "4")
	for _, body := range []string{`{"model":"gpt-image-2","prompt":"x","n":1.2}`, `{"model":"gpt-image-2","prompt":"x","mask":"abc"}`, `{"model":"gpt-image-2","prompt":"x","model":"gpt-image-1"}`, `{"model":"gpt-image-2","prompt":"x","unknown":true}`, `{"model":"gpt-image-2","prompt":"x","image_urls":[],"image_url":"https://public.example/a.png"}`} {
		c, _ := videoTestContext("POST", "/v1/images/edits/async", []byte(body), "application/json")
		_, err = svc.ParseAsyncImageRequest(c, []byte(body))
		require.Error(t, err, body)
	}
}

type asyncDownloadTransport func(*http.Request) (*http.Response, error)

func (f asyncDownloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAsyncImageDownloadStreamsWithoutUpstreamCredentials(t *testing.T) {
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	called := 0
	svc := &OpenAIGatewayService{asyncImageDownloadTransport: asyncDownloadTransport(func(r *http.Request) (*http.Response, error) {
		called++
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, ContentLength: int64(data.Len()), Body: io.NopCloser(bytes.NewReader(data.Bytes()))}, nil
	})}
	c, w := videoTestContext("GET", "/v1/images/tasks/imgtask_x/content", nil, "")
	c.Request.Header.Set("Authorization", "Bearer caller-secret")
	record := &ImageTaskRecord{ID: "imgtask_x", Status: ImageTaskStatusCompleted, Provider: &AsyncImageMetadata{ResultURL: "https://images.example/a.png?signature=secret"}}
	require.NoError(t, svc.DownloadAsyncImage(context.Background(), c, record))
	require.Equal(t, data.Bytes(), w.Body.Bytes())
	require.Equal(t, "image/png", w.Header().Get("Content-Type"))
	require.Equal(t, 1, called)
	record.Provider.ResultURL = "https://127.0.0.1/a.png"
	require.Error(t, svc.DownloadAsyncImage(context.Background(), c, record))
	require.Equal(t, 1, called)
	svc.asyncImageDownloadOnce.Do(func() { svc.asyncImageDownloads = make(chan struct{}, 4) })
	for i := 0; i < 4; i++ {
		svc.asyncImageDownloads <- struct{}{}
	}
	record.Provider.ResultURL = "https://images.example/a.png"
	err := svc.DownloadAsyncImage(context.Background(), c, record)
	require.ErrorContains(t, err, "concurrency")
	require.Equal(t, 1, called)
}

func TestAsyncImageDownloadInterruptedAbortsInsteadOfAppendingJSON(t *testing.T) {
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	svc := &OpenAIGatewayService{asyncImageDownloadTransport: asyncDownloadTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, ContentLength: int64(data.Len() + 10), Body: io.NopCloser(bytes.NewReader(data.Bytes()))}, nil
	})}
	c, _ := videoTestContext("GET", "/v1/images/tasks/imgtask_x/content", nil, "")
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		_ = svc.DownloadAsyncImage(context.Background(), c, &ImageTaskRecord{ID: "imgtask_x", Status: ImageTaskStatusCompleted, Provider: &AsyncImageMetadata{ResultURL: "https://images.example/a.png"}})
	})
}

func TestAsyncImageQueryErrorResponseKeepsTaskAndOperationSeparate(t *testing.T) {
	c, w := videoTestContext("GET", "/v1/images/tasks/imgtask_x", nil, "")
	WriteAsyncImageFailure(c, imageProviderError(503, http.Header{"X-Request-Id": {"rid"}}, []byte(`{"message":"queue storage unavailable","code":"queue_busy"}`), ""), &ImageTask{TaskID: "imgtask_x", Status: "processing", SubmissionStatus: "accepted", Billing: &ImageTaskBillingStatus{Status: "pending"}})
	require.Equal(t, 502, w.Code)
	require.Contains(t, w.Body.String(), "queue storage unavailable")
	require.Contains(t, w.Body.String(), `"status":"processing"`)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &receipt))
	require.Nil(t, receipt["task_error"])
	require.Equal(t, receipt["error"], receipt["operation_error"])
	require.Equal(t, "pending", receipt["billing"].(map[string]any)["status"])
}

func TestAsyncImageProtocolConfigurationDoesNotAffectOtherAccounts(t *testing.T) {
	account := &Account{Type: AccountTypeAPIKey, Platform: PlatformOpenAI}
	require.False(t, account.SupportsOpenAIImageCapability(OpenAIImagesCapabilityAsync))
	account.Credentials = map[string]any{"image_protocol": ImageProtocolNaoshengyun}
	require.True(t, account.SupportsOpenAIImageCapability(OpenAIImagesCapabilityAsync))
	require.NoError(t, ValidateImageProtocolCredentials(account.Credentials))
	require.Error(t, ValidateImageProtocolCredentials(map[string]any{"image_protocol": "invalid"}))
}

func TestAsyncImageDefaultSizeAndMalformedReferenceItems(t *testing.T) {
	parsed := &OpenAIImagesRequest{Endpoint: openAIImagesGenerationsEndpoint, ExplicitModel: true, Model: "gpt-image-2", Prompt: "ocean", N: 1}
	payload, err := BuildAsyncImagePayload(parsed, parsed.Model, 0)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"size":"1024x1024"`)
	parsed.Size = "auto"
	_, err = BuildAsyncImagePayload(parsed, parsed.Model, 0)
	require.ErrorContains(t, err, "billable")
	for _, reference := range []string{`[{"image_url":"https://images.example/ref.png"},{"other":"x"}]`, `["https://images.example/ref.png"]`, `null`} {
		body := []byte(`{"model":"gpt-image-2","prompt":"x","images":` + reference + `}`)
		c, _ := videoTestContext("POST", "/v1/images/edits/async", body, "application/json")
		_, err := (&OpenAIGatewayService{}).ParseAsyncImageRequest(c, body)
		require.Error(t, err)
	}
}
