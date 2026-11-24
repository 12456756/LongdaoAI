package handler

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUploadVideoReferenceErrorContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	key := &service.APIKey{ID: 1, Group: &service.Group{Platform: service.PlatformOpenAI, AllowImageGeneration: true}}
	for _, tc := range []struct {
		name   string
		files  int
		tooBig bool
		status int
		code   string
	}{
		{"missing file", 0, false, http.StatusBadRequest, "invalid_video_reference_upload"},
		{"two files", 2, false, http.StatusBadRequest, "invalid_video_reference_upload"},
		{"oversize request", 0, true, http.StatusRequestEntityTooLarge, "video_reference_too_large"},
		{"invalid container", 1, false, http.StatusUnsupportedMediaType, "unsupported_video_reference_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for i := 0; i < tc.files; i++ {
				part, err := writer.CreateFormFile("file", "sample.mp4")
				require.NoError(t, err)
				_, err = part.Write([]byte("not a video"))
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set(string(middleware2.ContextKeyAPIKey), key)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/references", &body)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			if tc.tooBig {
				c.Request.ContentLength = service.VideoUploadMaxBytes + 1
			}
			h.UploadVideoReference(c)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
			require.Contains(t, w.Body.String(), `"source":"platform"`)
		})
	}
}
