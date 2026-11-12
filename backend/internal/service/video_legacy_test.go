package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const legacyTestTaskID = "mtask_00000000000000000000000000"

type videoMemoryStore struct {
	videoTestCache
	mu         sync.Mutex
	records    map[string][]byte
	expiries   map[string]time.Time
	references map[string]VideoReference
}

func newVideoMemoryStore() *videoMemoryStore {
	return &videoMemoryStore{records: map[string][]byte{}, expiries: map[string]time.Time{}, references: map[string]VideoReference{}}
}
func (v *videoMemoryStore) VideoLoad(_ context.Context, key string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	b, ok := v.records[key]
	if !ok {
		return nil, redis.Nil
	}
	return append([]byte(nil), b...), nil
}
func (v *videoMemoryStore) VideoSave(_ context.Context, key string, b []byte, expires time.Time, nx bool) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.records[key]; nx && ok {
		return false, nil
	}
	v.records[key] = append([]byte(nil), b...)
	v.expiries[key] = expires
	return true, nil
}
func (v *videoMemoryStore) VideoReplace(_ context.Context, key string, b []byte) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.records[key]; !ok {
		return false, nil
	}
	v.records[key] = append([]byte(nil), b...)
	return true, nil
}
func (v *videoMemoryStore) VideoBind(ctx context.Context, m *VideoTaskMetadata) error {
	b, _ := json.Marshal(m)
	_, err := v.VideoSave(ctx, VideoTaskMetadataKey(m.GroupID, m.APIKeyID, m.TaskID), b, time.Unix(m.ExpiresAt, 0), true)
	if err != nil {
		return err
	}
	return v.SetSessionAccountID(ctx, m.GroupID, videoTaskCacheKey(m.APIKeyID, m.TaskID), m.AccountID, openAIVideoTaskTTL)
}
func (v *videoMemoryStore) VideoStatus(ctx context.Context, key, status string) (string, error) {
	b, e := v.VideoLoad(ctx, key)
	if e != nil {
		return "", e
	}
	var m VideoTaskMetadata
	_ = json.Unmarshal(b, &m)
	if m.LastStatus == "completed" || m.LastStatus == "failed" || m.LastStatus == "cancelled" {
		return m.LastStatus, nil
	}
	m.LastStatus = status
	b, _ = json.Marshal(m)
	_, e = v.VideoReplace(ctx, key, b)
	return status, e
}
func (v *videoMemoryStore) VideoSaveReference(_ context.Context, r *VideoReference) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.references[r.AssetID] = *r
	return nil
}
func (v *videoMemoryStore) VideoLoadReference(_ context.Context, id string) (*VideoReference, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	r, ok := v.references[id]
	if !ok {
		return nil, redis.Nil
	}
	return &r, nil
}
func (v *videoMemoryStore) VideoReferenceTask(_ context.Context, id, task string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	r, ok := v.references[id]
	if ok {
		r.TaskID = task
		v.references[id] = r
	}
	return nil
}

func legacyTestConfig(t *testing.T) *config.Config {
	return &config.Config{VideoLegacy: config.VideoLegacyConfig{ReferenceDir: t.TempDir(), PublicBaseURL: "https://relay.example", SigningKeyID: "v1", SigningKeys: map[string]string{"v1": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}, MaxStorageBytes: 100 << 20, UploadConcurrency: 2, ReferenceDownloadConcurrency: 2, DownloadAllowedHosts: []string{"cdn.example"}, DownloadTimeoutSeconds: 150, DownloadIdleSeconds: 2, Models: map[string]config.VideoLegacyModel{"seedance-2.0": {Resolutions: []string{"720p"}, Ratios: []string{"16:9"}, SupportsAudio: true}}}}
}
func legacyTestAccount() *Account {
	return &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"api_key": "secret-upstream", "base_url": "https://api.example/v1/videos", "video_protocol": VideoProtocolLegacy}}
}
func legacyTestResponse(status int, model, taskStatus, videoURL string) *http.Response {
	final := taskStatus == "succeeded" || taskStatus == "failed" || taskStatus == "canceled"
	data, _ := json.Marshal(map[string]any{"code": 0, "message": "accepted", "data": map[string]any{"id": legacyTestTaskID, "model_id": model, "status": taskStatus, "is_final": final, "created_at": 12345, "video_url": videoURL}})
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data))}
}
func legacyTestPNG(t *testing.T) []byte {
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return b.Bytes()
}

func TestLegacyVideoCreateReplayAndQuery(t *testing.T) {
	store := newVideoMemoryStore()
	cfg := legacyTestConfig(t)
	calls := 0
	account := legacyTestAccount()
	key := &APIKey{ID: 8}
	svc := &OpenAIGatewayService{cfg: cfg, cache: store, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "Bearer secret-upstream", r.Header.Get("Authorization"))
		require.Equal(t, "/v1/video/tasks", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "seedance-2.0", body["model_id"])
		params := body["params"].(map[string]any)
		require.Equal(t, false, params["generate_audio"])
		require.Equal(t, float64(4), params["duration"])
		require.Len(t, r.Header.Get("Idempotency-Key"), 64)
		require.Nil(t, r.GetBody)
		require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}}
	body := []byte(`{"model":"alias","prompt":"waves","seconds":4,"resolution":"720p","generate_audio":false}`)
	for attempt := 0; attempt < 2; attempt++ {
		req, e := ParseOpenAIVideoRequest(body, "application/json")
		require.NoError(t, e)
		c, w := videoTestContext("POST", "/v1/videos", body, "application/json")
		c.Request.Header.Set("X-Video-Operation", "text-to-video")
		c.Request.Header.Set("Idempotency-Key", "same")
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, fmt.Sprintf("attempt-%d", attempt)))
		result, e := svc.ForwardVideo(c.Request.Context(), c, account, key, req, "", false, "seedance-2.0")
		require.NoError(t, e)
		require.Equal(t, 202, w.Code)
		require.JSONEq(t, `{"object":"video","id":"`+legacyTestTaskID+`","model":"alias","status":"queued"}`, w.Body.String())
		if attempt == 0 {
			require.NotNil(t, result)
			require.Equal(t, 4, result.VideoDurationSeconds)
		} else {
			require.Nil(t, result)
			require.Equal(t, "attempt-0", w.Header().Get("X-Client-Request-ID"))
		}
	}
	require.Equal(t, 1, calls)
	// A later account setting change must not change an existing task's protocol.
	account.Credentials["video_protocol"] = VideoProtocolOpenAI
	svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/v1/video/tasks/"+legacyTestTaskID, r.URL.Path)
		return legacyTestResponse(200, "seedance-2.0", "processing", ""), nil
	}}
	c, w := videoTestContext("GET", "/v1/videos/"+legacyTestTaskID, nil, "")
	result, e := svc.ForwardVideo(context.Background(), c, account, key, nil, legacyTestTaskID, false, "")
	require.NoError(t, e)
	require.Nil(t, result)
	require.Contains(t, w.Body.String(), `"status":"in_progress"`)
	_, e = svc.videoTaskMetadata(context.Background(), &APIKey{ID: 9}, legacyTestTaskID)
	require.ErrorIs(t, e, redis.Nil)
}

func TestLegacyVideoRejectsUnsupportedBeforeDispatch(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: legacyTestConfig(t)}
	for _, field := range []string{`"fps":30`, `"seconds":"4"`, `"generate_audio":"false"`, `"output_format":"webm"`, `"output_format":null`, `"size":"1920x1080"`, `"unexpected":"x"`, `"model":"duplicate"`} {
		body := []byte(`{"model":"seedance-2.0","prompt":"waves","seconds":4,"resolution":"720p",` + field + `}`)
		req, e := ParseOpenAIVideoRequest(body, "application/json")
		require.NoError(t, e)
		_, _, e = svc.legacyParams(req, "seedance-2.0", "text-to-video")
		require.Error(t, e, field)
	}
}

func TestLegacyVideoReferenceIsReadableBeforeAcceptanceAndReused(t *testing.T) {
	cfg := legacyTestConfig(t)
	store := newVideoMemoryStore()
	key := &APIKey{ID: 8}
	account := legacyTestAccount()
	svc := &OpenAIGatewayService{cfg: cfg, cache: store}
	t.Cleanup(svc.CloseOpenAIWSPool)
	calls := 0
	svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			Model  string `json:"model_id"`
			Params struct {
				Content []struct {
					Type     string `json:"type"`
					Role     string `json:"role"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
				Audio bool `json:"generate_audio"`
			} `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body.Params.Content, 2)
		require.Equal(t, "reference_image", body.Params.Content[1].Role)
		require.False(t, body.Params.Audio)
		raw := body.Params.Content[1].ImageURL.URL
		ref := strings.TrimSuffix(raw[strings.LastIndex(raw, "/")+1:strings.Index(raw, "?")], ".png")
		require.Empty(t, store.references[ref].TaskID)
		w := httptest.NewRecorder()
		svc.ServeVideoReference(w, httptest.NewRequest("GET", raw, nil), ref+".png")
		require.Equal(t, 200, w.Code)
		require.Equal(t, legacyTestPNG(t), w.Body.Bytes())
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}
	for i := 0; i < 2; i++ {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for k, v := range map[string]string{"model": "seedance-2.0", "prompt": "waves", "seconds": "4", "resolution": "720p", "generate_audio": "false"} {
			require.NoError(t, form.WriteField(k, v))
		}
		part, e := form.CreateFormFile("input_reference", "reference.png")
		require.NoError(t, e)
		_, e = part.Write(legacyTestPNG(t))
		require.NoError(t, e)
		require.NoError(t, form.Close())
		req, e := ParseOpenAIVideoRequest(body.Bytes(), form.FormDataContentType())
		require.NoError(t, e)
		c, w := videoTestContext("POST", "/v1/videos", body.Bytes(), form.FormDataContentType())
		c.Request.Header.Set("Idempotency-Key", "same-image")
		c.Request.Header.Set("X-Video-Operation", "image-to-video")
		_, e = svc.ForwardVideo(context.Background(), c, account, key, req, "", false, "")
		require.NoError(t, e)
		require.Equal(t, 202, w.Code)
	}
	require.Equal(t, 1, calls)
	require.Len(t, store.references, 1)
	for _, r := range store.references {
		require.Equal(t, legacyTestTaskID, r.TaskID)
	}
}

func TestVideoReferenceSigningExpiryCleanupAndIsolation(t *testing.T) {
	cfg := legacyTestConfig(t)
	store := newVideoMemoryStore()
	runtime, e := newVideoReferenceRuntime(cfg.VideoLegacy, store)
	require.NoError(t, e)
	runtime.cancel()
	<-runtime.done
	fixed := time.Now().UTC()
	runtime.now = func() time.Time { return fixed }
	original := legacyTestPNG(t)
	ref, e := runtime.save(context.Background(), &APIKey{ID: 4}, original)
	require.NoError(t, e)
	require.Equal(t, int64(604800), ref.ExpiresAt-ref.CreatedAt)
	svc := &OpenAIGatewayService{cfg: cfg, cache: store, videoReferenceRuntime: runtime}
	svc.videoReferenceOnce.Do(func() {})
	fetch := func(raw, method string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		svc.ServeVideoReference(w, httptest.NewRequest(method, raw, nil), ref.AssetID+".png")
		return w
	}
	w := fetch(ref.SignedURL, "GET")
	require.Equal(t, 200, w.Code)
	require.Equal(t, original, w.Body.Bytes())
	require.Equal(t, 200, fetch(ref.SignedURL, "HEAD").Code)
	require.Equal(t, 403, fetch(ref.SignedURL+"&expires=123", "GET").Code)
	require.Equal(t, 403, fetch(strings.Replace(ref.SignedURL, "kid=v1", "kid=bad", 1), "GET").Code)
	runtime.now = func() time.Time { return time.Unix(ref.ExpiresAt, 0) }
	require.Equal(t, 410, fetch(ref.SignedURL, "GET").Code)
	runtime.cleanup(context.Background())
	_, e = os.Stat(filepath.Join(cfg.VideoLegacy.ReferenceDir, filepath.FromSlash(ref.RelativePath)))
	require.True(t, os.IsNotExist(e))
}

type videoRoundTripFunc func(*http.Request) (*http.Response, error)

func (f videoRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLegacyVideoDownloadRefreshNoSecretsAndExactBytes(t *testing.T) {
	cfg := legacyTestConfig(t)
	store := newVideoMemoryStore()
	account := legacyTestAccount()
	key := &APIKey{ID: 8}
	now := time.Now().Unix()
	require.NoError(t, store.VideoBind(context.Background(), &VideoTaskMetadata{SchemaVersion: 1, TaskID: legacyTestTaskID, APIKeyID: key.ID, AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: "alias", UpstreamModel: "seedance-2.0", ExpiresAt: now + 604800, BoundAt: now}))
	queries, downloads := 0, 0
	file := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 512)...)
	svc := &OpenAIGatewayService{cfg: cfg, cache: store, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		queries++
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "Bearer secret-upstream", r.Header.Get("Authorization"))
		return legacyTestResponse(200, "seedance-2.0", "succeeded", fmt.Sprintf("https://cdn.example/video.mp4?signature=a%%2Bb&attempt=%d", queries)), nil
	}}}
	svc.videoDownloadTransport = videoRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		downloads++
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Empty(t, r.Header.Get("Range"))
		require.Contains(t, r.URL.RawQuery, "signature=a%2Bb")
		if downloads == 1 {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("expired")), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(file)), Header: http.Header{"Content-Type": {"video/mp4"}}}, nil
	})
	c, w := videoTestContext("GET", "/v1/videos/"+legacyTestTaskID+"/content?client=secret", nil, "")
	c.Request.Header.Set("Range", "bytes=0-9")
	result, e := svc.ForwardVideo(context.Background(), c, account, key, nil, legacyTestTaskID, true, "")
	require.NoError(t, e)
	require.Nil(t, result)
	require.Equal(t, file, w.Body.Bytes())
	require.Equal(t, 2, queries)
	require.Equal(t, 2, downloads)
	require.Empty(t, w.Header().Get("Location"))
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestVideoDownloadAddressProtection(t *testing.T) {
	for _, raw := range []string{"http://cdn.example/a", "https://evil.example/a", "https://user:pass@cdn.example/a", "https://cdn.example:8443/a", "https://127.0.0.1/a", "file:///tmp/a"} {
		_, e := validateVideoDownloadURL(raw, []string{"cdn.example", "127.0.0.1"})
		require.Error(t, e, raw)
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "::ffff:127.0.0.1", "fd00::1", "fe80::1", "64:ff9b::a00:1"} {
		require.False(t, videoPublicIP(netip.MustParseAddr(ip)), ip)
	}
	require.True(t, videoPublicIP(netip.MustParseAddr("8.8.8.8")))
}

func TestLegacyVideoContentValidationAndAbort(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, response := range []*http.Response{
		{Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("<html>error</html>"))},
		{Header: http.Header{"Content-Type": {"video/mp4"}, "Content-Length": {"268435457"}}, Body: io.NopCloser(strings.NewReader("too big"))},
		{Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader("invalid media bytes"))},
	} {
		c, w := videoTestContext("GET", "/", nil, "")
		require.Error(t, svc.streamLegacyVideo(c, response, time.Second))
		require.False(t, c.Writer.Written())
		require.Empty(t, w.Body.Bytes())
	}
}

func TestLegacyTaskStatesAndIdentity(t *testing.T) {
	for upstream, wanted := range map[string]string{"queued": "queued", "processing": "in_progress", "succeeded": "completed", "failed": "failed", "canceled": "cancelled"} {
		resp := legacyTestResponse(200, "seedance-2.0", upstream, "")
		data, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		task, e := decodeLegacyTask(data, legacyTestTaskID, "seedance-2.0")
		require.NoError(t, e)
		status, e := legacyStatus(task)
		require.NoError(t, e)
		require.Equal(t, wanted, status)
		_, e = decodeLegacyTask(data, legacyTestTaskID, "different-model")
		require.Error(t, e)
	}
	for _, raw := range []string{
		`{"code":1,"data":{}}`,
		`{"code":0,"data":{"id":"` + legacyTestTaskID + `","model_id":"seedance-2.0","created_at":1,"status":"mystery","is_final":false}}`,
		`{"code":0,"data":{"id":"` + legacyTestTaskID + `","model_id":"seedance-2.0","created_at":1,"status":"succeeded","is_final":false}}`,
	} {
		_, e := decodeLegacyTask([]byte(raw), legacyTestTaskID, "seedance-2.0")
		require.Error(t, e)
	}
}

func TestLegacyAmbiguousSubmissionIsNotResubmitted(t *testing.T) {
	cfg := legacyTestConfig(t)
	store := newVideoMemoryStore()
	calls := 0
	svc := &OpenAIGatewayService{cfg: cfg, cache: store, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) { calls++; return nil, context.DeadlineExceeded }}}
	body := []byte(`{"model":"seedance-2.0","prompt":"waves","seconds":4,"resolution":"720p"}`)
	for attempt := 0; attempt < 2; attempt++ {
		request, e := ParseOpenAIVideoRequest(body, "application/json")
		require.NoError(t, e)
		c, _ := videoTestContext("POST", "/v1/videos", body, "application/json")
		c.Request.Header.Set("Idempotency-Key", "ambiguous")
		c.Request.Header.Set("X-Video-Operation", "text-to-video")
		_, e = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), &APIKey{ID: 8}, request, "", false, "")
		require.Error(t, e)
	}
	require.Equal(t, 1, calls)
}

func TestLegacyConcurrentAndConflictingSubmissionsDoNotDispatch(t *testing.T) {
	store := newVideoMemoryStore()
	entered, release := make(chan struct{}), make(chan struct{})
	svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: store, httpUpstream: videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}}
	invoke := func(prompt string) error {
		body, _ := json.Marshal(map[string]any{"model": "seedance-2.0", "prompt": prompt, "seconds": 4, "resolution": "720p"})
		request, e := ParseOpenAIVideoRequest(body, "application/json")
		if e != nil {
			return e
		}
		c, _ := videoTestContext("POST", "/v1/videos", body, "application/json")
		c.Request.Header.Set("Idempotency-Key", "concurrent")
		c.Request.Header.Set("X-Video-Operation", "text-to-video")
		_, e = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), &APIKey{ID: 8}, request, "", false, "")
		return e
	}
	finished := make(chan error, 1)
	go func() { finished <- invoke("waves") }()
	<-entered
	err := invoke("waves")
	require.EqualError(t, err, "Original video submission is pending confirmation")
	err = invoke("different prompt")
	require.EqualError(t, err, "Idempotency-Key was used with different parameters")
	close(release)
	require.NoError(t, <-finished)
}

type failingVideoReferenceStore struct{ *videoMemoryStore }

func (f failingVideoReferenceStore) VideoSaveReference(context.Context, *VideoReference) error {
	return fmt.Errorf("test Redis unavailable")
}

func TestVideoReferenceRegistrationFailureRemovesPublishedFile(t *testing.T) {
	cfg := legacyTestConfig(t)
	runtime, err := newVideoReferenceRuntime(cfg.VideoLegacy, failingVideoReferenceStore{newVideoMemoryStore()})
	require.NoError(t, err)
	runtime.cancel()
	<-runtime.done
	_, err = runtime.save(context.Background(), &APIKey{ID: 8}, legacyTestPNG(t))
	require.EqualError(t, err, "register video reference")
	require.Equal(t, int64(1), runtime.uploadFailures.Load())
	err = filepath.WalkDir(cfg.VideoLegacy.ReferenceDir, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		require.NotEqual(t, "input.png", d.Name())
		require.NotEqual(t, "manifest.json", d.Name())
		require.False(t, strings.HasPrefix(d.Name(), ".upload_"))
		return nil
	})
	require.NoError(t, err)
}

func TestVideoReferenceRestartAndSigningKeyRotation(t *testing.T) {
	cfg := legacyTestConfig(t)
	store := newVideoMemoryStore()
	first, err := newVideoReferenceRuntime(cfg.VideoLegacy, store)
	require.NoError(t, err)
	first.cancel()
	<-first.done
	oldRef, err := first.save(context.Background(), &APIKey{ID: 8}, legacyTestPNG(t))
	require.NoError(t, err)
	cfg.VideoLegacy.SigningKeys["v2"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	cfg.VideoLegacy.SigningKeyID = "v2"
	second, err := newVideoReferenceRuntime(cfg.VideoLegacy, store)
	require.NoError(t, err)
	second.cancel()
	<-second.done
	newRef, err := second.save(context.Background(), &APIKey{ID: 8}, legacyTestPNG(t))
	require.NoError(t, err)
	require.Equal(t, "v2", newRef.SigningKeyID)
	svc := &OpenAIGatewayService{cfg: cfg, cache: store, videoReferenceRuntime: second}
	svc.videoReferenceOnce.Do(func() {})
	for _, reference := range []*VideoReference{oldRef, newRef} {
		w := httptest.NewRecorder()
		svc.ServeVideoReference(w, httptest.NewRequest("GET", reference.SignedURL, nil), reference.AssetID+".png")
		require.Equal(t, 200, w.Code)
		require.Equal(t, legacyTestPNG(t), w.Body.Bytes())
	}
}

func TestLegacyVideoErrorDetailsDoNotExposeSecrets(t *testing.T) {
	request := []byte(`{"params":{"content":[{"type":"text","text":"private prompt"}]}}`)
	response := []byte(`{"message":"Invalid reference https://relay.example/a?signature=secret using upstream-secret and private prompt"}`)
	message := legacyErrorMessage(response, request, "upstream-secret")
	require.Contains(t, message, "Invalid reference")
	for _, secret := range []string{"https://", "signature=", "upstream-secret", "private prompt"} {
		require.NotContains(t, message, secret)
	}
}

func TestLegacyVideoFailureDetailsReachQueryDownloadAndLogs(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	missingReason := "Upstream reported status=failed without error.message (task_id=" + legacyTestTaskID + ")"
	for _, test := range []struct {
		name, code, message string
		upstreamError       any
	}{
		{"provider_reason", "InvalidResolution", "Resolution 720p is not supported for this model", map[string]string{"code": "InvalidResolution", "message": "Resolution 720p is not supported for this model"}},
		{"redacted_reason", "ImageFetchFailed", "Could not fetch [redacted-url] using [redacted-key]", map[string]string{"code": "ImageFetchFailed", "message": "Could not fetch https://relay.example/input.png?signature=private using secret-upstream"}},
		{"redacted_code", "[redacted-key]", "Image fetch rejected", map[string]string{"code": "secret-upstream", "message": "Image fetch rejected"}},
		{"missing_code", "video_generation_failed", "Provider quota exhausted", map[string]string{"message": "Provider quota exhausted"}},
		{"missing_message", "InternalError", missingReason, map[string]string{"code": "InternalError", "message": " \n "}},
		{"missing_error", "video_generation_failed", missingReason, nil},
	} {
		for _, download := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/download=%t", test.name, download), func(t *testing.T) {
				logs.Reset()
				store := newVideoMemoryStore()
				account, key := legacyTestAccount(), &APIKey{ID: 8}
				now := time.Now().Unix()
				require.NoError(t, store.VideoBind(context.Background(), &VideoTaskMetadata{SchemaVersion: 1, TaskID: legacyTestTaskID, APIKeyID: key.ID, AccountID: account.ID, Protocol: VideoProtocolLegacy, RequestedModel: "alias", UpstreamModel: "seedance-2.0", BillingRequestID: "billing-test", BoundAt: now, ExpiresAt: now + 604800}))
				body, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"id": legacyTestTaskID, "model_id": "seedance-2.0", "status": "failed", "is_final": true, "created_at": 12345, "error": test.upstreamError}})
				require.NoError(t, err)
				svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: store, httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
					require.Equal(t, http.MethodGet, r.Method)
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
				}}}
				svc.videoDownloadTransport = videoRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("A failed task must not trigger a file download")
					return nil, nil
				})
				c, w := videoTestContext("GET", "/v1/videos/"+legacyTestTaskID, nil, "")
				_, err = svc.ForwardVideo(context.Background(), c, account, key, nil, legacyTestTaskID, download, "")
				var output struct {
					Status string            `json:"status"`
					Error  map[string]string `json:"error"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &output))
				if download {
					require.Error(t, err)
					require.Equal(t, http.StatusConflict, w.Code)
					require.Equal(t, "video_not_downloadable", output.Error["code"])
					require.Contains(t, output.Error["message"], "["+test.code+"]: "+test.message)
				} else {
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, w.Code)
					require.Equal(t, "failed", output.Status)
					require.Equal(t, map[string]string{"code": test.code, "message": test.message}, output.Error)
				}
				var logEntry map[string]any
				require.NoError(t, json.Unmarshal(logs.Bytes(), &logEntry))
				require.Equal(t, "video_legacy.task_failed", logEntry["msg"])
				require.Equal(t, legacyTestTaskID, logEntry["task_id"])
				require.Equal(t, float64(account.ID), logEntry["account_id"])
				require.Equal(t, "billing-test", logEntry["billing_request_id"])
				require.Equal(t, test.code, logEntry["upstream_code"])
				require.Equal(t, test.message, logEntry["reason"])
				for _, secret := range []string{"secret-upstream", "https://relay.example", "signature=private"} {
					require.NotContains(t, w.Body.String(), secret)
					require.NotContains(t, logs.String(), secret)
				}
			})
		}
	}
}
