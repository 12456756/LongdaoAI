package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func videoTestImage(t *testing.T, format string) []byte {
	t.Helper()
	var data bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var err error
	switch format {
	case "png":
		return legacyTestPNG(t)
	case "jpeg":
		err = jpeg.Encode(&data, img, nil)
	case "gif":
		err = gif.Encode(&data, img, nil)
	case "bmp":
		err = bmp.Encode(&data, img)
	case "tiff":
		err = tiff.Encode(&data, img, nil)
	case "webp":
		decoded, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
		require.NoError(t, err)
		return decoded
	default:
		t.Fatalf("unknown test image format %q", format)
	}
	require.NoError(t, err)
	return data.Bytes()
}

func TestLegacyVideoReferenceImageFormats(t *testing.T) {
	for _, tc := range []struct {
		format, mimeType, extension string
	}{
		{"png", "image/png", ".png"},
		{"jpeg", "image/jpeg", ".jpg"},
		{"webp", "image/webp", ".webp"},
		{"gif", "image/gif", ".gif"},
		{"bmp", "image/bmp", ".bmp"},
		{"tiff", "image/tiff", ".tiff"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			original := videoTestImage(t, tc.format)
			// The multipart fixture deliberately labels every file as .png/octet-stream.
			body, ct := videoMultipartFixture(t, original)
			request, err := ParseOpenAIVideoUpload(bytes.NewReader(body), ct)
			require.NoError(t, err)
			defer request.Close()
			require.NoError(t, request.ValidateVideoReferences(VideoProtocolLegacy, "image-to-video"))
			require.Len(t, request.legacyImages, 1)
			require.Equal(t, tc.mimeType, request.legacyImages[0].mimeType)
			visited := 0
			require.NoError(t, request.VisitModerationBodies(func(body []byte) bool {
				visited++
				require.Contains(t, string(body), "data:"+tc.mimeType+";base64,"+base64.StdEncoding.EncodeToString(original))
				return true
			}))
			require.Equal(t, 1, visited)
			store := newVideoMemoryStore()
			svc := &OpenAIGatewayService{cfg: legacyTestConfig(t), cache: store}
			t.Cleanup(svc.CloseOpenAIWSPool)
			calls := 0
			svc.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					Params struct {
						Content []struct {
							ImageURL struct{ URL string } `json:"image_url"`
						}
					}
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				require.Len(t, payload.Params.Content, 2)
				u, err := url.Parse(payload.Params.Content[1].ImageURL.URL)
				require.NoError(t, err)
				require.Equal(t, tc.extension, filepath.Ext(u.Path))
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					w := httptest.NewRecorder()
					svc.ServeVideoReference(w, httptest.NewRequest(method, u.String(), nil), filepath.Base(u.Path))
					require.Equal(t, 200, w.Code)
					require.Equal(t, tc.mimeType, w.Header().Get("Content-Type"))
					if method == http.MethodGet {
						require.Equal(t, original, w.Body.Bytes())
					} else {
						require.Empty(t, w.Body.Bytes())
					}
				}
				forged := *u
				forged.Path = strings.TrimSuffix(u.Path, tc.extension) + ".invalid"
				w := httptest.NewRecorder()
				svc.ServeVideoReference(w, httptest.NewRequest(http.MethodGet, forged.String(), nil), filepath.Base(forged.Path))
				require.Equal(t, 403, w.Code)
				forged.Path = strings.TrimSuffix(u.Path, tc.extension) + ".jpg"
				if tc.extension == ".jpg" {
					forged.Path = strings.TrimSuffix(u.Path, tc.extension) + ".png"
				}
				w = httptest.NewRecorder()
				svc.ServeVideoReference(w, httptest.NewRequest(http.MethodGet, forged.String(), nil), filepath.Base(forged.Path))
				require.Equal(t, 403, w.Code)
				return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
			}}
			c, w := videoTestContext("POST", "/v1/videos", nil, ct)
			c.Request.Header.Set("X-Video-Operation", "image-to-video")
			_, err = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), &APIKey{ID: 8}, request, "", false, "")
			require.NoError(t, err)
			require.Equal(t, 202, w.Code)
			require.Equal(t, 1, calls)
			require.Len(t, store.references, 1)
			for _, ref := range store.references {
				require.Equal(t, tc.mimeType, ref.MimeType)
				require.Equal(t, "input"+tc.extension, filepath.Base(ref.RelativePath))
				stored, err := os.ReadFile(filepath.Join(svc.cfg.VideoLegacy.ReferenceDir, filepath.FromSlash(ref.RelativePath)))
				require.NoError(t, err)
				require.Equal(t, original, stored)
				runtime := svc.videoReferenceRuntime
				runtime.cancel()
				<-runtime.done
				runtime.now = func() time.Time { return time.Unix(ref.ExpiresAt, 0) }
				runtime.cleanup(context.Background())
				_, err = os.Stat(filepath.Join(svc.cfg.VideoLegacy.ReferenceDir, filepath.FromSlash(ref.RelativePath)))
				require.True(t, os.IsNotExist(err))
			}
		})
	}
}

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
	full := sizedVideoPNG(valid, videoReferenceImageMaxBytes+1)
	var wideJPEG bytes.Buffer
	require.NoError(t, jpeg.Encode(&wideJPEG, image.NewRGBA(image.Rect(0, 0, 4097, 1)), nil))
	for _, tc := range []struct {
		name    string
		images  [][]byte
		status  int
		message string
	}{
		{"empty", nil, 400, "count 0; allowed 1..9"},
		{"empty image", [][]byte{{}}, 400, "size 0 bytes"},
		{"too many", [][]byte{valid, valid, valid, valid, valid, valid, valid, valid, valid, valid}, 400, "count 10; allowed 1..9"},
		{"bad second image", [][]byte{valid, []byte("not an image")}, 400, "input_reference[2] is not a valid image"},
		{"truncated image", [][]byte{valid[:len(valid)-12]}, 400, "image decoding failed"},
		{"oversized dimensions", [][]byte{wideJPEG.Bytes()}, 400, "dimensions 4097x1"},
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
	full := sizedVideoPNG(legacyTestPNG(t), videoReferenceImageMaxBytes)
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
	second := videoTestImage(t, "jpeg")
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
	idempotencyKey := "multiple-images"
	submit := func(images ...[]byte) (int, string, error) {
		body, ct := videoMultipartFixture(t, images...)
		r, err := ParseOpenAIVideoUpload(bytes.NewReader(body), ct)
		require.NoError(t, err)
		defer r.Close()
		c, w := videoTestContext("POST", "/v1/videos", nil, ct)
		c.Request.Header.Set("X-Video-Operation", "image-to-video")
		c.Request.Header.Set("Idempotency-Key", idempotencyKey)
		_, err = svc.ForwardVideo(context.Background(), c, legacyTestAccount(), key, r, "", false, "")
		return w.Code, w.Body.String(), err
	}
	status, _, err := submit(first, second)
	require.Equal(t, 503, status)
	require.ErrorContains(t, err, "Save input_reference[2]: register video reference in Redis: test Redis write failed")
	require.Empty(t, store.references)
	require.NotEmpty(t, store.records, "Keep the submission failure so the client can query its cause")
	require.Zero(t, calls)
	require.NoError(t, filepath.WalkDir(cfg.VideoLegacy.ReferenceDir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		require.False(t, strings.HasSuffix(entry.Name(), ".png"))
		return nil
	}))
	idempotencyKey = "multiple-images-new-attempt"
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
