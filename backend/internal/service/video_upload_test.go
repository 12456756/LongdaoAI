package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func videoMultipartFixture(t *testing.T, images ...[]byte) ([]byte, string) {
	return videoMultipartFixtureForModel(t, "seedance-2.0", images...)
}

func videoMultipartFixtureForModel(t *testing.T, model string, images ...[]byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range map[string]string{"model": model, "prompt": "waves", "seconds": "4", "resolution": "720p"} {
		require.NoError(t, form.WriteField(key, value))
	}
	for i, image := range images {
		part, err := form.CreateFormFile("input_reference", fmt.Sprintf("image-%d.png", i))
		require.NoError(t, err)
		_, err = part.Write(image)
		require.NoError(t, err)
	}
	require.NoError(t, form.Close())
	return body.Bytes(), form.FormDataContentType()
}

func TestLegacySeedance25MultipleReferences(t *testing.T) {
	for _, count := range []int{1, 2, 9, 10} {
		t.Run(fmt.Sprintf("images-%d", count), func(t *testing.T) {
			images := make([][]byte, count)
			for i := range images {
				images[i] = legacyTestPNG(t)
			}
			body, ct := videoMultipartFixtureForModel(t, "seedance-2.5", images...)
			request, err := ParseOpenAIVideoUpload(bytes.NewReader(body), ct)
			require.NoError(t, err)
			defer request.Close()
			store := newVideoMemoryStore()
			svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: store}
			t.Cleanup(svc.CloseOpenAIWSPool)
			calls := 0
			svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					Model  string `json:"model_id"`
					Params struct {
						Content []struct {
							Role     string
							ImageURL struct{ URL string } `json:"image_url"`
						}
					}
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				require.Equal(t, "seedance-2.5", payload.Model)
				require.Len(t, payload.Params.Content, count+1)
				for _, item := range payload.Params.Content[1:] {
					require.Equal(t, "reference_image", item.Role)
					require.Contains(t, item.ImageURL.URL, "signature=")
				}
				return legacyTestResponse(202, "seedance-2.5", "queued", ""), nil
			}}
			c, w := videoTestContext("POST", "/v1/videos", nil, ct)
			c.Request.Header.Set("X-Video-Operation", "image-to-video")
			key := &APIKey{ID: 8}
			_, err = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), key, request, "", false, "")
			if count == 10 {
				require.ErrorContains(t, err, "count 10; allowed 1..9")
				require.Equal(t, 400, w.Code)
				require.Zero(t, calls)
				require.Empty(t, store.references)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 202, w.Code)
			require.Equal(t, 1, calls)
			meta, err := svc.videoTaskMetadata(context.Background(), key, legacyTestTaskID)
			require.NoError(t, err)
			require.Equal(t, "seedance-2.5", meta.UpstreamModel)
			require.Len(t, meta.ReferenceAssetIDs, count)
			for _, asset := range meta.ReferenceAssetIDs {
				require.Equal(t, legacyTestTaskID, store.references[asset].TaskID)
			}
		})
	}
}

func sizedVideoPNG(png []byte, size int) []byte {
	// Insert a valid private ancillary chunk before IEND.
	const typ = "vpAg"
	padded := make([]byte, size)
	pos := len(png) - 12
	copy(padded, png[:pos])
	end := size - 12
	binary.BigEndian.PutUint32(padded[pos:], uint32(end-pos-12))
	copy(padded[pos+4:], typ)
	binary.BigEndian.PutUint32(padded[end-4:], crc32.ChecksumIEEE(padded[pos+4:end-4]))
	copy(padded[end:], png[pos:])
	return padded
}

type videoZeroReader struct{}

func (videoZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestLegacyVideoMultipartLimits(t *testing.T) {
	valid := legacyTestPNG(t)
	full := sizedVideoPNG(valid, videoPNGMaxBytes+1)
	for _, tc := range []struct {
		name    string
		images  [][]byte
		status  int
		message string
	}{
		{"empty", nil, 400, "count 0; allowed 1..9"},
		{"too many", [][]byte{valid, valid, valid, valid, valid, valid, valid, valid, valid, valid}, 400, "count 10; allowed 1..9"},
		{"bad second image", [][]byte{valid, []byte("not a PNG")}, 400, "input_reference[2] is not a valid PNG"},
		{"too large by a byte", [][]byte{valid, full}, 413, "input_reference[2] size 10485761 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := videoMultipartFixture(t, tc.images...)
			r, err := ParseOpenAIVideoUpload(bytes.NewReader(body), ct)
			require.NoError(t, err)
			t.Cleanup(r.Close)
			err = r.ValidateVideoReferences(VideoProtocolLegacy, "image-to-video")
			require.Error(t, err)
			status, _, message := VideoRequestErrorDetails(err)
			require.Equal(t, tc.status, status)
			require.Contains(t, message, tc.message)
		})
	}
}

func TestLegacyVideoTwoConcurrentFullUploads(t *testing.T) {
	full := sizedVideoPNG(legacyTestPNG(t), videoPNGMaxBytes)
	digest := sha256.Sum256(full)
	var prefix bytes.Buffer
	form := multipart.NewWriter(&prefix)
	for key, value := range map[string]string{"model": "seedance-2.0", "prompt": "waves", "seconds": "4", "resolution": "720p"} {
		require.NoError(t, form.WriteField(key, value))
	}
	var pieces [][]byte
	for i := 0; i < 9; i++ {
		_, err := form.CreateFormFile("input_reference", "image.png")
		require.NoError(t, err)
		pieces = append(pieces, bytes.Clone(prefix.Bytes()), full)
		prefix.Reset()
	}
	require.NoError(t, form.Close())
	pieces = append(pieces, bytes.Clone(prefix.Bytes()))
	var size int64
	for _, piece := range pieces {
		size += int64(len(piece))
	}
	makeBody := func(extra int64) io.Reader {
		readers := make([]io.Reader, 0, len(pieces)+1)
		for _, piece := range pieces {
			readers = append(readers, bytes.NewReader(piece))
		}
		readers = append(readers, io.LimitReader(videoZeroReader{}, VideoUploadMaxBytes-size+extra))
		return io.MultiReader(readers...)
	}
	t.Run("concurrent", func(t *testing.T) {
		for _, name := range []string{"first", "second"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r, err := ParseOpenAIVideoUpload(makeBody(0), form.FormDataContentType())
				require.NoError(t, err)
				defer r.Close()
				require.NoError(t, r.ValidateVideoReferences(VideoProtocolLegacy, "image-to-video"))
				require.Len(t, r.legacyImages, 9)
				require.Equal(t, VideoUploadMaxBytes, r.wireBytes)
				for _, image := range r.legacyImages {
					require.Equal(t, hex.EncodeToString(digest[:]), image.sha256)
				}
				visited := 0
				require.NoError(t, r.VisitModerationBodies(func(body []byte) bool {
					visited++
					input := ExtractContentModerationInput(ContentModerationProtocolOpenAIImages, body)
					require.Len(t, input.Images, 1)
					require.Equal(t, "waves", input.Text)
					return true
				}))
				require.Equal(t, 9, visited)
				require.ErrorContains(t, r.PrepareNativeVideoUpload(), "limit 12582912 bytes")
			})
		}
	})
	_, err := ParseOpenAIVideoUpload(makeBody(1), form.FormDataContentType())
	status, _, message := VideoRequestErrorDetails(err)
	require.Equal(t, 413, status)
	require.Contains(t, message, "limit 96468992 bytes")
}

type batchVideoReferenceStore struct {
	*videoMemoryStore
	failAt, saves int
}

func (s *batchVideoReferenceStore) VideoSaveReference(ctx context.Context, ref *VideoReference) error {
	s.saves++
	if s.saves == s.failAt {
		return errors.New("test Redis write failed")
	}
	return s.videoMemoryStore.VideoSaveReference(ctx, ref)
}

func TestLegacyVideoMultiReferenceOrderReplayAndRollback(t *testing.T) {
	first := legacyTestPNG(t)
	second := sizedVideoPNG(first, len(first)+20)
	store := &batchVideoReferenceStore{videoMemoryStore: newVideoMemoryStore(), failAt: 2}
	cfg := legacyTestConfig(t)
	svc := &OpenAIGatewayService{cfg: cfg, cache: store}
	t.Cleanup(svc.CloseOpenAIWSPool)
	calls := 0
	key := &APIKey{ID: 8}
	svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			Params struct {
				Content []struct {
					Role     string
					ImageURL struct{ URL string } `json:"image_url"`
				}
			}
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body.Params.Content, 3)
		for i, original := range [][]byte{first, second} {
			item := body.Params.Content[i+1]
			require.Equal(t, "reference_image", item.Role)
			u, err := url.Parse(item.ImageURL.URL)
			require.NoError(t, err)
			w := httptest.NewRecorder()
			svc.ServeVideoReference(w, httptest.NewRequest("GET", u.String(), nil), filepath.Base(u.Path))
			require.Equal(t, 200, w.Code)
			require.Equal(t, original, w.Body.Bytes())
		}
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}
	submit := func(images ...[]byte) (int, string, error) {
		body, ct := videoMultipartFixture(t, images...)
		r, err := ParseOpenAIVideoUpload(bytes.NewReader(body), ct)
		require.NoError(t, err)
		defer r.Close()
		c, w := videoTestContext("POST", "/v1/videos", nil, ct)
		c.Request.Header.Set("X-Video-Operation", "image-to-video")
		c.Request.Header.Set("Idempotency-Key", "multiple-images")
		_, err = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), key, r, "", false, "")
		return w.Code, w.Body.String(), err
	}
	status, _, err := submit(first, second)
	require.Equal(t, 503, status)
	require.ErrorContains(t, err, "Save input_reference[2]: register video reference in Redis: test Redis write failed")
	require.Empty(t, store.references)
	require.Empty(t, store.records)
	require.Zero(t, calls)
	require.NoError(t, filepath.WalkDir(cfg.VideoLegacy.ReferenceDir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		require.False(t, strings.HasSuffix(entry.Name(), ".png"))
		return nil
	}))
	status, _, err = submit(first, second)
	require.NoError(t, err)
	require.Equal(t, 202, status)
	meta, err := svc.videoTaskMetadata(context.Background(), key, legacyTestTaskID)
	require.NoError(t, err)
	require.Len(t, meta.ReferenceAssetIDs, 2)
	for _, asset := range meta.ReferenceAssetIDs {
		require.Equal(t, legacyTestTaskID, store.references[asset].TaskID)
	}
	require.Equal(t, store.references[meta.ReferenceAssetIDs[0]].ExpiresAt, store.references[meta.ReferenceAssetIDs[1]].ExpiresAt)
	status, _, err = submit(first, second)
	require.NoError(t, err)
	require.Equal(t, 202, status)
	status, _, err = submit(second, first)
	require.Error(t, err)
	require.Equal(t, 409, status)
	require.Equal(t, 1, calls)
}
