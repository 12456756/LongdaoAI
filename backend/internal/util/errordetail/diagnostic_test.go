package errordetail

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDiagnosticFormats(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"Unsupported size","code":"bad_size"}}`, `{"message":"Unsupported size","code":"bad_size"}`, `{"error":"Unsupported size"}`, `Unsupported size`, `"Unsupported size"`, `<Error><Code>bad_size</Code><Message>Unsupported size</Message><RequestId>xml-id</RequestId></Error>`} {
		t.Run(body, func(t *testing.T) {
			if got := Message([]byte(body)); got != "Unsupported size" {
				t.Fatalf("message=%q", got)
			}
		})
	}
	d := Diagnostic(422, http.Header{"X-Request-Id": []string{"provider-id"}}, []byte(`{"error":{"type":"invalid_request_error","code":"bad_size","message":"Unsupported size","param":"size","details":{"allowed":["720p"],"password":"dummy-pass"}},"request_id":"body-id"}`))
	if d["code"] != "bad_size" || d["upstream_status"] != 422 || d["upstream_request_id"] != "provider-id" || d["request_id"] != "body-id" {
		t.Fatalf("diagnostic=%v", d)
	}
	details := d["details"].(map[string]any)
	if details["password"] != "[redacted]" || details["allowed"].([]any)[0] != "720p" {
		t.Fatalf("details=%v", details)
	}
}

func TestRedactionPreservesReason(t *testing.T) {
	input := "Unsupported resolution; Bearer dummy-bearer; https://files.example/v1/video?signature=dummy-sig&api_key=dummy-key; postgres://alice:dummy-pass@db.example/data; user=dummy@example.org; path C:\\data\\private.txt; projects/private-project/locations/global; -----BEGIN PRIVATE KEY-----dummy-pem-----END PRIVATE KEY-----"
	out := Text(input)
	for _, secret := range []string{"dummy-bearer", "dummy-sig", "dummy-key", "dummy-pass", "dummy@example.org", "private.txt", "private-project", "dummy-pem"} {
		if strings.Contains(out, secret) {
			t.Fatalf("leaked %q: %s", secret, out)
		}
	}
	for _, reason := range []string{"Unsupported resolution", "https://files.example/v1/video", "/locations/global"} {
		if !strings.Contains(out, reason) {
			t.Fatalf("lost %q: %s", reason, out)
		}
	}
	jsonBody := Body([]byte(`{"error":{"code":"bad_size","message":"Bearer dummy-bearer; wrong size","details":{"authorization":"dummy","nested":[{"message":"Cookie: sid=dummy-cookie"}]}}}`))
	if !json.Valid(jsonBody) || strings.Contains(string(jsonBody), "dummy") || !strings.Contains(string(jsonBody), "bad_size") || !strings.Contains(string(jsonBody), "wrong size") {
		t.Fatalf("body=%s", jsonBody)
	}
}

func TestRedactionIsIdempotentAndPreservesStorageCause(t *testing.T) {
	for _, input := range []string{
		"Validate API key in platform storage: get api key: db down",
		"Invalid image; Cookie: sid=dummy-cookie; mode=dummy-mode; unsupported resolution",
		"Access denied https://api.example?key=dummy-key&size=720p",
	} {
		out := Text(input)
		if Text(out) != out {
			t.Fatalf("redaction changed on second pass: %q -> %q", out, Text(out))
		}
		if strings.Contains(out, "dummy-") {
			t.Fatalf("leaked secret: %q", out)
		}
	}
	if !strings.Contains(Text("get api key: db down"), "db down") {
		t.Fatal("lost database failure cause")
	}
	if !strings.Contains(Text("Cookie: sid=dummy-cookie; unsupported resolution"), "unsupported resolution") {
		t.Fatal("lost reason after cookie")
	}
}

func TestRedactionCamelCaseSensitiveFields(t *testing.T) {
	input := []byte(`{"error":{"code":"invalid_token","message":"Token refresh denied","details":{"accessToken":"dummy-access","refreshToken":"dummy-refresh","privateKey":"dummy-private","connectionString":"dummy-dsn","fullName":"dummy-person","phoneNumber":"dummy-phone"}}}`)
	out := string(Body(input))
	if strings.Contains(out, "dummy-") || !strings.Contains(out, "Token refresh denied") || !strings.Contains(out, "invalid_token") {
		t.Fatalf("invalid sanitized error: %s", out)
	}
}
