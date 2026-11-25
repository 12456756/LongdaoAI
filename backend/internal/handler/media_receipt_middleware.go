package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
)

// MediaReceiptMiddleware runs before authentication. Only error bodies are
// buffered; successful binary downloads and SSE keep their original writer.
func (h *OpenAIGatewayHandler) MediaReceiptMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		kind, operation := service.MediaEndpoint(c)
		if kind == "" {
			c.Next()
			return
		}
		original := c.Writer
		writer := &mediaErrorWriter{ResponseWriter: original, c: c}
		persist := func(receipt map[string]any, status int) {
			if c.GetBool("media_receipt_persisted") {
				return
			}
			c.Set("media_receipt_persisted", true)
			receipt["http_status"] = status
			key, authenticated := middleware2.GetAPIKeyFromContext(c)
			if authenticated && key != nil && h != nil && h.gatewayService != nil && !c.GetBool("media_receipt_billable") {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
				err := h.gatewayService.SaveMediaFailure(ctx, key, kind, receipt)
				cancel()
				if err != nil {
					receipt["submission_record_error"] = map[string]any{"source": "platform", "code": "media_receipt_store_error", "message": errordetail.Text(err.Error())}
				}
			}
		}
		c.Set("media_receipt_persist", persist)
		writer.complete = func(payload map[string]any) map[string]any {
			receipt := service.CompleteMediaReceipt(c, writer.Status(), payload)
			persist(receipt, writer.Status())
			return receipt
		}
		c.Writer = writer
		defer func() {
			if failure := recover(); failure != nil {
				if original.Written() || failure == http.ErrAbortHandler {
					c.Writer = original
					panic(failure)
				}
				slog.Error("media.operation_panic", "path", c.Request.URL.Path, "reason", errordetail.Text(fmt.Sprint(failure)), "stack", errordetail.Text(string(debug.Stack())))
				service.WriteMediaJSON(c, 500, gin.H{"error": gin.H{"source": "platform", "code": "media_platform_panic", "message": "Media operation panicked: " + errordetail.Text(fmt.Sprint(failure))}})
				c.Abort()
			}
			writer.finish()
			c.Writer = original
		}()
		values := c.Request.Header.Values("X-Client-Request-ID")
		if len(values) > 1 || (len(values) == 1 && !videoBillingRequestID.MatchString(values[0])) {
			c.AbortWithStatusJSON(400, gin.H{"error": gin.H{"code": "invalid_client_request_id", "message": "Expected one X-Client-Request-ID with 1..256 letters, digits or _.:-"}})
			return
		}
		if len(values) == 1 {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, values[0]))
			c.Header("X-Client-Request-ID", values[0])
		}
		c.Header("Cache-Control", "no-store")
		if operation == "create" && kind == "video" {
			if err := service.InitVideoContract(c); err != nil {
				service.WriteVideoFailure(c, err)
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

// Reusing a completed failed request ID returns its original receipt. It must
// not start a new paid dispatch that contradicts the saved no-charge decision.
func (h *OpenAIGatewayHandler) MediaRequestReplayMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		kind, operation := service.MediaEndpoint(c)
		if operation != "create" || h == nil || h.gatewayService == nil {
			c.Next()
			return
		}
		key, ok := middleware2.GetAPIKeyFromContext(c)
		if !ok || key == nil {
			c.Next()
			return
		}
		id, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		receipt, err := h.gatewayService.MediaFailure(c.Request.Context(), key, kind, id)
		if err != nil {
			service.WriteMediaJSON(c, 503, gin.H{"error": gin.H{"source": "platform", "code": "media_receipt_unavailable", "message": "Read original media receipt: " + errordetail.Text(err.Error())}})
			c.Abort()
			return
		}
		if receipt != nil {
			status := 502
			if value, ok := receipt["http_status"].(float64); ok && value >= 400 && value <= 599 {
				status = int(value)
			}
			service.WriteMediaJSON(c, status, receipt)
			c.Abort()
			return
		}
		c.Next()
	}
}

const mediaErrorMaxBytes = 256 << 10

type mediaErrorWriter struct {
	gin.ResponseWriter
	c             *gin.Context
	body          bytes.Buffer
	complete      func(map[string]any) map[string]any
	headerWritten bool
	finished      bool
	truncated     bool
}

func (w *mediaErrorWriter) WriteHeader(code int) {
	if w.headerWritten || w.ResponseWriter.Written() {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *mediaErrorWriter) WriteHeaderNow() {
	if w.Status() < 400 {
		w.ResponseWriter.WriteHeaderNow()
		return
	}
	w.headerWritten = true
}
func (w *mediaErrorWriter) Written() bool { return w.headerWritten || w.ResponseWriter.Written() }
func (w *mediaErrorWriter) Size() int {
	if w.Status() >= 400 && !w.finished {
		if !w.headerWritten {
			return -1
		}
		return w.body.Len()
	}
	return w.ResponseWriter.Size()
}
func (w *mediaErrorWriter) Write(data []byte) (int, error) {
	if w.Status() < 400 || w.ResponseWriter.Written() || w.c.GetBool("media_receipt_encoded") {
		return w.ResponseWriter.Write(data)
	}
	w.headerWritten = true
	remaining := mediaErrorMaxBytes - w.body.Len()
	if len(data) > remaining {
		w.truncated = true
		_, _ = w.body.Write(data[:remaining])
	} else {
		_, _ = w.body.Write(data)
	}
	return len(data), nil
}
func (w *mediaErrorWriter) WriteString(data string) (int, error) { return w.Write([]byte(data)) }
func (w *mediaErrorWriter) Flush()                               { w.finish(); w.ResponseWriter.Flush() }

func (w *mediaErrorWriter) finish() {
	if w.finished || w.Status() < 400 || w.ResponseWriter.Written() {
		return
	}
	w.finished = true
	var payload map[string]any
	if json.Unmarshal(w.body.Bytes(), &payload) != nil || payload == nil {
		fields := errordetail.Fields(w.body.Bytes())
		if len(w.body.Bytes()) == 0 {
			fields["message"] = fmt.Sprintf("Media operation returned HTTP %d without a response body", w.Status())
		}
		payload = map[string]any{"error": fields}
	}
	if w.truncated {
		payload["diagnostic_truncated"] = true
	}
	payload = w.complete(payload)
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.Header().Del("Content-Disposition")
	w.Header().Del("Content-Range")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.ResponseWriter.Write(data)
}

// Assert the embedded writer still supplies Hijacker, CloseNotifier and Pusher.
var _ http.Flusher = (*mediaErrorWriter)(nil)
