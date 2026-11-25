package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sampleReferenceVideo() []byte {
	box := func(kind string, payload []byte) []byte {
		data := binary.BigEndian.AppendUint32(nil, uint32(len(payload)+8))
		return append(append(data, kind...), payload...)
	}
	handler := box("hdlr", []byte("\x00\x00\x00\x00\x00\x00\x00\x00vide"))
	video := append(box("ftyp", []byte("isom\x00\x00\x00\x00mp41")), box("moov", box("trak", box("mdia", handler)))...)
	return append(video, box("mdat", []byte{1})...)
}

func TestUploadedVideoReferenceDispatch(t *testing.T) {
	ctx := context.Background()
	account := legacyTestAccount()
	key := &APIKey{ID: 8}
	now := time.Now().UTC()
	var signedURL string
	calls := 0
	svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: newVideoMemoryStore()}
	runtime, err := svc.videoReferences()
	require.NoError(t, err)
	runtime.cancel()
	<-runtime.done
	runtime.now = func() time.Time { return now }
	video := sampleReferenceVideo()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "untrusted-name.bin")
	require.NoError(t, err)
	_, err = part.Write(video)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	ref, err := svc.SaveVideoReferenceUpload(ctx, key, multipart.NewReader(&body, writer.Boundary()))
	require.NoError(t, err)
	require.Equal(t, "video/mp4", ref.MimeType)
	require.EqualValues(t, len(video), ref.ByteLength)
	require.NotEmpty(t, ref.SHA256)
	require.Empty(t, ref.SignedURL)

	svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "/v1/video/tasks", r.URL.Path)
		var payload struct {
			Model  string `json:"model_id"`
			Params struct {
				Content []videoReferenceContent `json:"content"`
			} `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Len(t, payload.Params.Content, 1)
		require.Empty(t, payload.Params.Content[0].VideoURL.ReferenceID)
		signedURL = payload.Params.Content[0].VideoURL.URL
		require.Contains(t, signedURL, "/media/video-inputs/"+ref.AssetID+".mp4?")
		return legacyTestResponse(http.StatusAccepted, payload.Model, "queued", ""), nil
	}}
	submit := func(key *APIKey, idem string) *httptest.ResponseRecorder {
		body := []byte(`{"model":"seedance-2.0","metadata":{"content":[{"type":"video_url","role":"reference_video","video_url":{"reference_id":"` + ref.AssetID + `"}}]}}`)
		request, err := ParseOpenAIVideoRequest(body, "application/json")
		require.NoError(t, err)
		require.True(t, request.UsesUploadedVideoReference())
		require.Equal(t, VideoOperationVideo, request.VideoBillingOperation())
		c, w := videoTestContext(http.MethodPost, "/v1/videos", body, "application/json")
		c.Request.Header.Set("X-Video-Operation", VideoOperationVideo)
		c.Request.Header.Set("Idempotency-Key", idem)
		_, _ = svc.ForwardVideo(ctx, c, account, key, request, "", false, "")
		return w
	}

	w := submit(&APIKey{ID: 9}, "different-key")
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), "video_reference_not_found")
	now = time.Unix(ref.ExpiresAt-10, 0)
	w = submit(key, "original")
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	now = time.Unix(ref.ExpiresAt+1, 0)
	w = submit(key, "new")
	require.Equal(t, http.StatusGone, w.Code)
	require.Contains(t, w.Body.String(), "video_reference_expired")
	w = submit(key, "original")
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, calls)

	runtime.cleanup(ctx)
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, signedURL, nil)
	r.Header.Set("Range", "bytes=0-7")
	svc.ServeVideoReference(w, r, ref.AssetID+".mp4")
	require.Equal(t, http.StatusPartialContent, w.Code)
	require.Equal(t, video[:8], w.Body.Bytes())

	now = time.Unix(ref.ReadUntil+1, 0)
	runtime.cleanup(ctx)
	_, err = os.Stat(filepath.Join(runtime.cfg.ReferenceDir, ref.RelativePath))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestUploadedVideoReferenceRequiresExactlyOneSource(t *testing.T) {
	for _, source := range []string{
		`{}`,
		`{"reference_id":"invalid"}`,
		`{"reference_id":"vref_0123456789abcdef0123456789abcdef","url":"https://media.example/ref.mp4"}`,
	} {
		content := []byte(`[{"type":"video_url","role":"reference_video","video_url":` + source + `}]`)
		_, err := parseVideoReferenceContent(content, "", VideoOperationVideo)
		status, code, _ := VideoRequestErrorDetails(err)
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, "invalid_video_reference", code)
	}
}

func TestReferenceStorageErrorPreservesCauseAndRedactsOnlySensitiveFragments(t *testing.T) {
	cause := &os.PathError{Op: "write", Path: "/var/private/video.mp4", Err: errors.New("no space left on device; token=fixture-secret")}
	err := referenceStorageError("Save reference video file", cause)
	require.ErrorIs(t, err, cause)
	status, code, message := VideoRequestErrorDetails(err)
	require.Equal(t, 503, status)
	require.Equal(t, "video_reference_storage_error", code)
	require.Contains(t, message, "Save reference video file")
	require.Contains(t, message, "no space left on device")
	require.NotContains(t, message, "/var/private")
	require.NotContains(t, message, "fixture-secret")
	capacity := videoError(503, "video_reference_capacity_exceeded", "Reference storage capacity: used=1024 required=512 limit=1024 free=4096 minimum_free_after_save=2048 bytes")
	wrapped := fmt.Errorf("capacity check: %w", capacity)
	require.Same(t, wrapped, referenceStorageError("Check reference video storage capacity", wrapped))
	status, code, message = VideoRequestErrorDetails(wrapped)
	require.Equal(t, 503, status)
	require.Equal(t, "video_reference_capacity_exceeded", code)
	require.Contains(t, message, "used=1024 required=512")
}
