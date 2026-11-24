package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyReferenceVideoDispatchAndIdempotency(t *testing.T) {
	const content = `[
		{"type":"video_url","video_url":{"url":"https://media.example/ref.mp4?signature=sample"},"role":"reference_video"},
		{"type":"image_url","image_url":{"url":"asset://tva_image"},"role":"reference_image"},
		{"type":"audio_url","audio_url":{"url":"https://media.example/ref.mp3"},"role":"reference_audio"}
	]`
	body := []byte(`{"model":"seedance-2.5","metadata":{"content":` + content + `}}`)
	calls := 0
	svc := &OpenAIGatewayService{
		cfg:   legacyTestConfig(t),
		cache: newVideoMemoryStore(),
		httpUpstream: videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, "/v1/video/tasks", r.URL.Path)
			var payload struct {
				Model  string `json:"model_id"`
				Params struct {
					Content    json.RawMessage `json:"content"`
					Duration   int             `json:"duration"`
					Resolution string          `json:"resolution"`
				} `json:"params"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			require.Equal(t, "seedance-2.5", payload.Model)
			require.JSONEq(t, content, string(payload.Params.Content))
			require.Equal(t, 5, payload.Params.Duration)
			require.Equal(t, "720p", payload.Params.Resolution)
			return legacyTestResponse(http.StatusAccepted, payload.Model, "queued", ""), nil
		}},
	}
	submit := func(body []byte) (int, error) {
		request, err := ParseOpenAIVideoRequest(body, "application/json")
		require.NoError(t, err)
		require.Equal(t, VideoOperationVideo, request.VideoBillingOperation())
		c, w := videoTestContext(http.MethodPost, "/v1/videos", body, "application/json")
		c.Request.Header.Set("Idempotency-Key", "reference-video")
		c.Request.Header.Set("X-Video-Operation", VideoOperationVideo)
		_, err = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), &APIKey{ID: 8}, request, "", false, "")
		return w.Code, err
	}
	status, err := submit(body)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, status)
	status, err = submit(body)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, status)
	status, err = submit([]byte(strings.Replace(string(body), "ref.mp4", "other.mp4", 1)))
	require.Error(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, 1, calls)
}

func TestLegacyReferenceVideoLimits(t *testing.T) {
	media := func(kind string, count int) string {
		item := fmt.Sprintf(`{"type":%q,%q:{"url":"asset://tva_asset"},"role":%q}`, kind+"_url", kind+"_url", "reference_"+kind)
		return strings.Repeat(item+",", count)
	}
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct {
		model                  string
		images, videos, audios int
		seconds                int
		wantError              string
	}{
		{"seedance-2.0", 9, 3, 3, 15, ""},
		{"seedance-2.0-fast", 9, 4, 3, 15, "at most 3 video_url"},
		{"seedance-2.0-mini", 10, 3, 3, 15, "at most 9 image_url"},
		{"seedance-2.0", 9, 3, 4, 15, "at most 3 audio_url"},
		{"seedance-2.0", 9, 3, 3, 16, "Unsupported legacy video duration"},
		{"seedance-2.5", 30, 10, 10, 30, ""},
		{"seedance-2.5", 30, 11, 10, 30, "at most 10 video_url"},
		{"seedance-2.5", 31, 10, 10, 30, "at most 30 image_url"},
		{"seedance-2.5", 30, 10, 11, 30, "at most 10 audio_url"},
		{"seedance-2.5", 0, 1, 0, 31, "Unsupported legacy video duration"},
		{"seedance-2.5", 0, 1, 0, -1, ""},
		{"seedance-2.5", 0, 0, 1, 5, "requires at least one reference image or video"},
	} {
		content := strings.TrimSuffix(media("image", tc.images)+media("video", tc.videos)+media("audio", tc.audios), ",")
		body := fmt.Sprintf(`{"model":%q,"seconds":%d,"resolution":"4k","aspect_ratio":"adaptive","metadata":{"content":[%s]}}`, tc.model, tc.seconds, content)
		req, err := ParseOpenAIVideoRequest([]byte(body), "application/json")
		require.NoError(t, err)
		params, _, err := svc.legacyParams(req, tc.model, VideoOperationVideo)
		if tc.wantError != "" {
			require.ErrorContains(t, err, tc.wantError, body)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.seconds, params["duration"])
			require.Equal(t, "4k", params["resolution"])
		}
	}
}

func TestReferenceVideoRejectsInvalidContent(t *testing.T) {
	const video = `{"type":"video_url","video_url":{"url":"https://media.example:8443/ref.mp4"},"role":"reference_video"}`
	for _, item := range []string{
		strings.Replace(video, "reference_video", "first_frame", 1),
		strings.Replace(video, "media.example:8443", "media.example:65536", 1),
		strings.Replace(video, "media.example:8443", "user@media.example", 1),
		strings.Replace(video, "ref.mp4", "ref.mp4#fragment", 1),
		strings.Replace(video, "media.example:8443", "127.0.0.1", 1),
		video + `,{"type":"text","text":"second prompt"}`,
	} {
		_, err := parseVideoReferenceContent([]byte("["+item+"]"), "first prompt", VideoOperationVideo)
		require.Error(t, err, item)
	}
	_, err := parseVideoReferenceContent([]byte("["+video+"]"), "", VideoOperationImage)
	require.ErrorContains(t, err, "does not match")
}
