// Package errordetail preserves provider diagnostics while removing sensitive fragments.
package errordetail

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var (
	bearer      = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
	secret      = regexp.MustCompile(`(?i)(\b(?:api[_-]?key|x-api-key|x-amz-security-token|x-goog-credential|authorization|access[_-]?token|refresh[_-]?token|client_secret|password|passwd|secret|signature|sig|x-amz-signature|x-goog-signature|credential|token)\b["']?\s*[:=]\s*["']?)[^\s&,;"'<>]+`)
	queryKey    = regexp.MustCompile(`(?i)(\bkey\s*=\s*)[^\s&,;"\x27<>]+`)
	cookie      = regexp.MustCompile(`(?i)\b((?:set-)?cookie\s*:\s*)(?:[^=;,\s]+=(?:"[^"]*"|[^;\s,\r\n]+)(?:;\s*)?)+`)
	connection  = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+:[^\s/@]+@`)
	key         = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{6,}|AIza[A-Za-z0-9_-]{35}|GOCSPX-[A-Za-z0-9_-]{24,})`)
	email       = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	privateHost = regexp.MustCompile(`(?i)\b(?:localhost|127\.\d+\.\d+\.\d+|10\.\d+\.\d+\.\d+|192\.168\.\d+\.\d+|172\.(?:1[6-9]|2\d|3[01])\.\d+\.\d+)(?::\d+)?\b`)
	localPath   = regexp.MustCompile(`(?i)(?:[a-z]:\\[^\s"'<>]+|/(?:home|root|var|tmp|etc|app|srv|data)/[^\s"'<>]+)`)
	resource    = regexp.MustCompile(`(?i)(?:gs://[^\s"'<>]+|\bprojects/[^/\s"'<>]+|\bfiles/[^\s"'<>]+)`)
)

// Text redacts fragments, rather than replacing an entire error containing a URL or secret.
var privateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

func Text(s string) string {
	s = privateKey.ReplaceAllString(s, "[redacted-private-key]")
	s = bearer.ReplaceAllString(s, "$1 [redacted]")
	s = connection.ReplaceAllString(s, "$1[redacted]@")
	s = secret.ReplaceAllString(s, "$1[redacted]")
	s = queryKey.ReplaceAllString(s, "$1[redacted]")
	s = cookie.ReplaceAllString(s, "$1[redacted]")
	s = key.ReplaceAllString(s, "[redacted-key]")
	s = email.ReplaceAllString(s, "[redacted-email]")
	s = privateHost.ReplaceAllString(s, "[internal-host]")
	s = localPath.ReplaceAllString(s, "[internal-path]")
	s = resource.ReplaceAllString(s, "[private-resource]")
	return strings.TrimSpace(s)
}

func sensitive(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "-", "_"), " ", "_"))
	if strings.HasSuffix(k, "_token") || strings.HasSuffix(k, "_secret") || strings.HasSuffix(k, "_password") || strings.HasSuffix(k, "_api_key") {
		return true
	}
	compact := strings.ReplaceAll(k, "_", "")
	switch compact {
	case "accesstoken", "refreshtoken", "idtoken", "clientsecret", "privatekey", "authorizationcode", "codeverifier", "connectionstring", "fullname", "firstname", "lastname", "phonenumber", "ipaddress", "stacktrace":
		return true
	}
	switch k {
	case "debug", "traceback", "redirect", "private_key", "authorization_code", "code_verifier", "authorization", "proxy_authorization", "cookie", "set_cookie", "api_key", "apikey", "x_api_key", "key", "access_token", "refresh_token", "id_token", "client_secret", "password", "passwd", "secret", "signature", "sig", "token", "connection_string", "dsn", "email", "phone", "phone_number", "full_name", "first_name", "last_name", "username", "user_name", "address", "ip", "ip_address", "stack", "stacktrace", "stack_trace", "sql", "query", "prompt", "input":
		return true
	}
	return false
}

func Value(v any) any { return redact(v, 0) }

func redact(v any, depth int) any {
	if depth > 32 {
		return "[depth limit]"
	}
	switch x := v.(type) {
	case map[string]any:
		r := make(map[string]any, len(x))
		for k, v := range x {
			if sensitive(k) {
				r[k] = "[redacted]"
			} else {
				r[k] = redact(v, depth+1)
			}
		}
		return r
	case []any:
		r := make([]any, len(x))
		for i, v := range x {
			r[i] = redact(v, depth+1)
		}
		return r
	case []map[string]any:
		r := make([]any, len(x))
		for i, v := range x {
			r[i] = redact(v, depth+1)
		}
		return r
	case map[string]string:
		r := make(map[string]any, len(x))
		for k, v := range x {
			r[k] = v
		}
		return redact(r, depth+1)
	case string:
		return Text(x)
	default:
		return v
	}
}

func Body(raw []byte) []byte {
	var v any
	if json.Unmarshal(raw, &v) == nil {
		b, _ := json.Marshal(Value(v))
		return b
	}
	return []byte(Text(string(raw)))
}

// Fields accepts nested, top-level, string and plain-text error formats. Business
// codes remain diagnostic data; they must not be treated as authorization codes.
func Fields(body []byte) map[string]any {
	var v any
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	if d.Decode(&v) != nil {
		var storage struct {
			XMLName   xml.Name `xml:"Error"`
			Code      string   `xml:"Code"`
			Message   string   `xml:"Message"`
			RequestID string   `xml:"RequestId"`
			Details   string   `xml:"Details"`
		}
		if xml.Unmarshal(body, &storage) == nil && storage.Message != "" {
			return Value(map[string]any{"code": storage.Code, "message": storage.Message, "request_id": storage.RequestID, "details": storage.Details}).(map[string]any)
		}
		return map[string]any{"message": Text(string(body))}
	}
	r := make(map[string]any)
	if root, ok := v.(map[string]any); ok {
		for k, v := range root {
			if k != "error" && k != "type" {
				r[k] = v
			}
		}
		if e, ok := root["error"].(map[string]any); ok {
			for k, v := range e {
				r[k] = v
			}
		} else if e, ok := root["error"].(string); ok {
			r["message"] = e
		}
		if r["message"] == nil {
			if m, ok := root["detail"]; ok {
				r["message"] = m
			}
		}
		if _, ok := r["type"]; !ok {
			if t, ok := root["type"].(string); ok && t != "error" {
				r["type"] = t
			}
		}
	} else if s, ok := v.(string); ok {
		r["message"] = s
	}
	if m, ok := r["message"].(string); ok && strings.HasPrefix(strings.TrimSpace(m), "{") {
		var inner map[string]any
		if json.Unmarshal([]byte(m), &inner) == nil {
			for k, v := range Fields([]byte(m)) {
				r[k] = v
			}
		}
	}
	// Success payloads and echoed request bodies do not belong in error metadata.
	for _, k := range []string{"data", "output", "choices", "content", "model", "usage"} {
		delete(r, k)
	}
	return Value(r).(map[string]any)
}

func Message(body []byte) string {
	v := Fields(body)["message"]
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func headerValue(h http.Header, key string) string {
	if value := h.Get(key); value != "" {
		return value
	}
	for k, values := range h {
		if strings.EqualFold(k, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func RequestID(h http.Header) string {
	for _, k := range []string{"x-request-id", "request-id", "x-amzn-requestid", "x-amz-request-id", "x-goog-request-id", "xai-request-id"} {
		if id := strings.TrimSpace(headerValue(h, k)); id != "" && len(id) <= 256 && !strings.ContainsAny(id, "\r\n") {
			return Text(id)
		}
	}
	return ""
}

func Diagnostic(status int, h http.Header, body []byte) map[string]any {
	r := Fields(body)
	r["source"] = "upstream"
	if status > 0 {
		r["upstream_status"] = status
	}
	if id := RequestID(h); id != "" {
		r["upstream_request_id"] = id
	} else {
		for _, k := range []string{"request_id", "requestId", "requestID"} {
			if id, ok := r[k]; ok {
				r["upstream_request_id"] = id
				break
			}
		}
	}
	if m := Message(body); m != "" {
		r["message"] = m
	} else {
		if status > 0 {
			r["message"] = fmt.Sprintf("Upstream HTTP %d response did not include an error message", status)
		} else {
			r["message"] = "Upstream error event did not include an error message"
		}
	}
	return r
}
