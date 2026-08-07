package server

import (
	"net/http"
	"testing"
)

func TestSafeHttpHeadersForLogUsesAllowlist(t *testing.T) {
	header := http.Header{
		"Authorization":  []string{"Bearer secret"},
		"Cookie":         []string{"session=secret"},
		"X-Payment":      []string{"signed-payment"},
		"Content-Type":   []string{"application/json"},
		"Content-Length": []string{"123"},
		"User-Agent":     []string{"test-agent"},
	}

	safe := SafeHttpHeadersForLog(header)
	if _, ok := safe["Authorization"]; ok {
		t.Fatal("authorization header was retained")
	}
	if _, ok := safe["Cookie"]; ok {
		t.Fatal("cookie header was retained")
	}
	if _, ok := safe["X-Payment"]; ok {
		t.Fatal("payment header was retained")
	}
	if safe["Content-Type"][0] != "application/json" || safe["User-Agent"][0] != "test-agent" {
		t.Fatalf("safe metadata was removed: %v", safe)
	}

	safe["Content-Type"][0] = "changed"
	if header.Get("Content-Type") != "application/json" {
		t.Fatal("safe header result aliases the request header")
	}
}

func TestSafeLogValueRedactsNestedSecrets(t *testing.T) {
	value := map[string]any{
		"request": map[string]any{
			"password": "secret",
			"metadata": map[string]string{
				"refresh-token": "secret",
				"operation":     "login",
			},
		},
		"headers": map[string][]string{
			"Authorization": []string{"Bearer secret"},
			"Content-Type":  []string{"application/json"},
		},
	}

	safe := SafeLogValue(value).(map[string]any)
	request := safe["request"].(map[string]any)
	if request["password"] != "[REDACTED]" {
		t.Fatal("nested password was not redacted")
	}
	metadata := request["metadata"].(map[string]string)
	if metadata["refresh-token"] != "[REDACTED]" || metadata["operation"] != "login" {
		t.Fatalf("nested metadata redaction is incorrect: %v", metadata)
	}
	headers := safe["headers"].(map[string][]string)
	if headers["Authorization"][0] != "[REDACTED]" || headers["Content-Type"][0] != "application/json" {
		t.Fatalf("header map redaction is incorrect: %v", headers)
	}
}
