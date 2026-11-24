package handler

import (
	"mime"
	"net/http"
	"strings"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) VideoReference(c *gin.Context) {
	h.gatewayService.ServeVideoReference(c.Writer, c.Request, c.Param("filename"))
}

func (h *OpenAIGatewayHandler) UploadVideoReference(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if key.Group == nil || key.Group.Platform != service.PlatformOpenAI || !service.GroupAllowsImageGeneration(key.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Reference video upload is not enabled for this group")
		return
	}
	if encoding := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding"))); encoding != "" && encoding != "identity" {
		writeVideoReferenceError(c, http.StatusBadRequest, "invalid_video_reference_upload", "Send an uncompressed multipart request.")
		return
	}
	if c.Request.ContentLength > service.VideoUploadMaxBytes {
		writeVideoReferenceError(c, http.StatusRequestEntityTooLarge, "video_reference_too_large", "Multipart request exceeds its size limit.")
		return
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeVideoReferenceError(c, http.StatusBadRequest, "invalid_video_reference_upload", "Expected multipart/form-data with one file field.")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.VideoUploadMaxBytes)
	reader, err := c.Request.MultipartReader()
	if err != nil {
		writeVideoReferenceError(c, http.StatusBadRequest, "invalid_video_reference_upload", "Expected multipart/form-data with one file field.")
		return
	}
	release, err := h.gatewayService.AcquireVideoUpload(c.Request.Context())
	if err != nil {
		c.Header("Retry-After", "1")
		writeVideoReferenceError(c, http.StatusTooManyRequests, "rate_limit_error", "Video uploads are busy. Try again shortly.")
		return
	}
	defer release()
	ref, err := h.gatewayService.SaveVideoReferenceUpload(c.Request.Context(), key, reader)
	if err != nil {
		status, code, message := service.VideoRequestErrorDetails(err)
		writeVideoReferenceError(c, status, code, message)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"reference_id": ref.AssetID,
		"content_type": ref.MimeType,
		"size_bytes":   ref.ByteLength,
		"expires_at":   time.Unix(ref.ExpiresAt, 0).UTC().Format(time.RFC3339),
	})
}

func writeVideoReferenceError(c *gin.Context, status int, code, message string) {
	errorType := "invalid_request_error"
	if status == http.StatusServiceUnavailable {
		errorType = "api_error"
	} else if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(status, gin.H{"error": gin.H{"type": errorType, "code": code, "message": message, "source": "platform"}})
}
