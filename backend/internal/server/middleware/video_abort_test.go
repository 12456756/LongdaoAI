package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestVideoAbortPropagatesAsTruncatedHTTPStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Recovery())
	router.GET("/content", func(c *gin.Context) {
		c.Header("Content-Type", "video/mp4")
		_, _ = c.Writer.Write([]byte("partial-video"))
		c.Writer.Flush()
		panic(http.ErrAbortHandler)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/content")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.Error(t, err)
	require.Equal(t, "partial-video", string(body))
}
