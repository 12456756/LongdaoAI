package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
)

func diagnosticRetryAfterBounded(value string) bool {
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return seconds <= 7*24*3600
	}
	at, err := http.ParseTime(value)
	return err == nil && !at.After(time.Now().Add(7*24*time.Hour))
}

type diagnosticCause struct{ cause error }

func (e diagnosticCause) Error() string { return errordetail.Text(e.cause.Error()) }
func (e diagnosticCause) Unwrap() error { return e.cause }
func safeDiagnosticCause(err error) error {
	if err == nil {
		return nil
	}
	return diagnosticCause{err}
}

const upstreamDiagnosticKey = "gateway_upstream_error_diagnostic"

func failoverErrorDiagnostic(c *gin.Context, failure *UpstreamFailoverError) map[string]any {
	if !failure.IsCredentialFailure() {
		if failure.Diagnostic != nil {
			return errordetail.Value(failure.Diagnostic).(map[string]any)
		}
		d := errordetail.Diagnostic(failure.StatusCode, failure.ResponseHeaders, failure.ResponseBody)
		if failure.DiagnosticSource == "platform" {
			d["source"], d["upstream_status"] = "platform", 0
		}
		if failure.Cause != nil && len(failure.ResponseBody) == 0 {
			d["message"] = errordetail.Text(failure.Cause.Error())
		}
		return d
	}
	message := string(failure.Reason)
	if strings.TrimSpace(failure.ClientMessage) != "" && failure.ClientMessage != GrokCredentialUnavailableClientMessage {
		message = failure.ClientMessage
	}
	if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := value.([]*OpsUpstreamErrorEvent); ok {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i] != nil && events[i].Stage == string(failure.Stage) && events[i].Message != "" {
					message = events[i].Message
					break
				}
			}
		}
	}
	return map[string]any{"type": "credential_error", "code": string(failure.Reason), "message": errordetail.Text(message), "source": "platform", "stage": string(failure.Stage), "scope": string(failure.Scope)}
}

// RecordFailoverDiagnostic records each failed attempt without selecting it for
// the response or changing any retry/scheduling decision.
func RecordFailoverDiagnostic(c *gin.Context, failure *UpstreamFailoverError) {
	if c == nil || failure == nil {
		return
	}
	d := failoverErrorDiagnostic(c, failure)
	const attemptsKey = upstreamDiagnosticKey + "_attempts"
	var attempts []map[string]any
	if value, ok := c.Get(attemptsKey); ok {
		attempts, _ = value.([]map[string]any)
	}
	attempts = append(attempts, d)
	if len(attempts) > 32 {
		attempts = attempts[len(attempts)-32:]
		c.Set(attemptsKey+"_omitted", c.GetInt(attemptsKey+"_omitted")+1)
	}
	c.Set(attemptsKey, attempts)
	c.Set(attemptsKey+"_last", failure)
	if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := value.([]*OpsUpstreamErrorEvent); ok && len(events) > 0 {
			event := events[len(events)-1]
			if event != nil && (event.UpstreamStatusCode == failure.StatusCode && event.Message == fmt.Sprint(d["message"]) || failure.IsCredentialFailure() && event.Stage == string(failure.Stage)) {
				event.Diagnostic = d
			}
		}
	}
}

func CaptureFailoverError(c *gin.Context, failure *UpstreamFailoverError) {
	if c == nil || failure == nil {
		return
	}
	if last, _ := c.Get(upstreamDiagnosticKey + "_attempts_last"); last != failure {
		RecordFailoverDiagnostic(c, failure)
	}
	c.Set(upstreamDiagnosticKey, failoverErrorDiagnostic(c, failure))
	headers := failure.ResponseHeaders
	if failure.IsCredentialFailure() {
		headers = nil
	}
	c.Set(upstreamDiagnosticKey+"_headers", headers.Clone())
	UseUpstreamError(c)
}

// CaptureUpstreamError keeps structured diagnostics independently of optional body logging.
func CaptureUpstreamError(c *gin.Context, status int, headers http.Header, body []byte) {
	if c == nil {
		return
	}
	d := errordetail.Diagnostic(status, headers, body)
	c.Set(upstreamDiagnosticKey, d)
	c.Set(upstreamDiagnosticKey+"_selected", false)
	c.Set(upstreamDiagnosticKey+"_headers", headers.Clone())
}

// UseUpstreamError selects the captured diagnostic only when emitting an upstream
// failure. A failed attempt must not replace a later platform error or decorate success.
func UseUpstreamError(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(upstreamDiagnosticKey+"_selected", true)
	if c.Writer.Written() {
		return
	}
	if value, ok := c.Get(upstreamDiagnosticKey + "_headers"); ok {
		if headers, ok := value.(http.Header); ok {
			if retry := strings.TrimSpace(headers.Get("Retry-After")); len(retry) <= 128 && validOpenAIPassthroughRetryAfter(retry, time.Now()) && diagnosticRetryAfterBounded(retry) {
				c.Header("Retry-After", retry)
			}
		}
	}
	if value, ok := c.Get(upstreamDiagnosticKey); ok {
		if d, ok := value.(map[string]any); ok {
			if id, ok := d["upstream_request_id"].(string); ok && len(id) <= 256 && !strings.ContainsAny(id, "\r\n") {
				c.Header("X-Upstream-Request-ID", id)
			}
		}
	}
}

// GatewayErrorFields preserves the provider type/code and supplements, rather than
// overwrites, them with the platform's existing status and protocol envelope.
func GatewayErrorFields(c *gin.Context, errorType, message string) gin.H {
	r := gin.H{"type": errorType, "message": errordetail.Text(message), "source": "platform"}
	if c != nil {
		if value, exists := c.Get(upstreamDiagnosticKey); exists && c.GetBool(upstreamDiagnosticKey+"_selected") {
			if d, ok := value.(map[string]any); ok {
				for k, v := range d {
					r[k] = v
				}
			}
		}
	}
	if c != nil && c.GetBool(upstreamDiagnosticKey+"_selected") {
		if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
			if events, ok := value.([]*OpsUpstreamErrorEvent); ok && len(events) > 1 {
				attempts := make([]any, 0, len(events))
				for _, event := range events {
					if event != nil {
						attempts = append(attempts, map[string]any{"upstream_status": event.UpstreamStatusCode, "upstream_request_id": event.UpstreamRequestID, "message": event.Message, "diagnostic": event.Diagnostic, "stage": event.Stage, "reason": event.Reason})
					}
				}
				r["attempts"] = attempts
			}
		}
	}
	if c != nil && c.GetBool(upstreamDiagnosticKey+"_selected") {
		if value, ok := c.Get(upstreamDiagnosticKey + "_attempts"); ok {
			if attempts, ok := value.([]map[string]any); ok && len(attempts) > 1 && r["attempts"] == nil {
				r["attempts"] = attempts
			}
		}
		if omitted := c.GetInt(upstreamDiagnosticKey + "_attempts_omitted"); omitted > 0 {
			r["earlier_attempts_omitted"] = omitted
		}
	}
	return gin.H(errordetail.Value(map[string]any(r)).(map[string]any))
}

func writeUpstreamError(c *gin.Context, status int, errorType, message string, anthropic bool) {
	UseUpstreamError(c)
	b := gin.H{"error": GatewayErrorFields(c, errorType, message)}
	if anthropic {
		b["type"] = "error"
	}
	WriteMediaJSON(c, status, b)
}

func upstreamDiagnosticJSON(c *gin.Context, errorType, message string) []byte {
	UseUpstreamError(c)
	b, _ := json.Marshal(gin.H{"error": GatewayErrorFields(c, errorType, message)})
	return b
}

// UpstreamBodyError detects explicit business failure envelopes inside HTTP 2xx.
func UpstreamBodyError(body []byte) bool {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return false
	}
	if e, ok := root["error"].(map[string]any); ok && len(e) > 0 {
		return true
	}
	if e, ok := root["error"].(string); ok && strings.TrimSpace(e) != "" {
		return true
	}
	if success, ok := root["success"].(bool); ok && !success {
		return true
	}
	return root["type"] == "error"
}

func CaptureProviderError(c *gin.Context, err error) {
	if c == nil {
		return
	}
	c.Set(upstreamDiagnosticKey+"_selected", false)
	d := ProviderErrorDiagnostic(err)
	c.Set(upstreamDiagnosticKey, d)
	c.Set(upstreamDiagnosticKey+"_headers", http.Header{})
	if d != nil {
		status, _ := d["upstream_status"].(int)
		requestID, _ := d["upstream_request_id"].(string)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Kind: "http_error", UpstreamStatusCode: status, UpstreamRequestID: requestID, Message: fmt.Sprint(d["message"]), Diagnostic: d})
	}
}

func antigravitySwitchDiagnostic(c *gin.Context, cause error) map[string]any {
	if c != nil {
		if value, ok := c.Get(upstreamDiagnosticKey); ok {
			if fields, ok := value.(map[string]any); ok {
				return fields
			}
		}
	}
	return map[string]any{"source": "platform", "type": "account_scheduling_error", "message": errordetail.Text(cause.Error()), "stage": "account_selection"}
}

func platformFailoverError(status int, errorType, operation string, cause error) *UpstreamFailoverError {
	message := operation
	if cause != nil {
		message += ": " + errordetail.Text(cause.Error())
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"type": errorType, "message": message}})
	return &UpstreamFailoverError{StatusCode: status, ResponseBody: body, Cause: cause, DiagnosticSource: "platform"}
}

func ProviderErrorDiagnostic(err error) map[string]any {
	var codex *codexModelsManifestUpstreamError
	if errors.As(err, &codex) {
		return codex.diagnostic
	}
	var gemini *GeminiAPIError
	if errors.As(err, &gemini) {
		if gemini.Diagnostic != nil {
			return gemini.Diagnostic
		}
		return map[string]any{"message": errordetail.Text(gemini.Message), "code": gemini.Code, "source": "upstream", "upstream_status": gemini.StatusCode}
	}
	var vertex *VertexAPIError
	if errors.As(err, &vertex) {
		if vertex.Diagnostic != nil {
			return vertex.Diagnostic
		}
		return map[string]any{"message": errordetail.Text(vertex.Message), "code": vertex.Code, "source": "upstream", "upstream_status": vertex.StatusCode}
	}
	return nil
}
