package server

import (
	"errors"
	"net/http"
	"strings"
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

func TestSafeLogValueRedactsStructsPointersAndTypedSlices(t *testing.T) {
	type nestedMetadata struct {
		AccessToken  string `json:"access_token"`
		Credential   string `json:"credential"`
		RefreshToken string
		RequestBody  string
		Somebody     string
		Operation    string `json:"operation"`
	}
	type requestMetadata struct {
		Password string
		Nested   *nestedMetadata  `json:"nested"`
		Items    []nestedMetadata `json:"items"`
	}

	value := &requestMetadata{
		Password: "password-secret",
		Nested: &nestedMetadata{
			AccessToken:  "access-secret",
			Credential:   "credential-secret",
			RefreshToken: "refresh-secret",
			RequestBody:  "request-body-secret",
			Somebody:     "nested-visible",
			Operation:    "refresh",
		},
		Items: []nestedMetadata{{
			AccessToken: "slice-secret",
			Operation:   "persist",
		}},
	}
	serialized := ErrorJsonWithCustomNoStack(errors.New("handler failed"), map[string]any{
		"request": value,
	})

	for _, secret := range []string{
		"password-secret", "access-secret", "credential-secret", "refresh-secret",
		"request-body-secret", "slice-secret",
	} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("structured error log retained %q: %s", secret, serialized)
		}
	}
	for _, safeValue := range []string{"refresh", "persist", "nested-visible"} {
		if !strings.Contains(serialized, safeValue) {
			t.Fatalf("structured error log removed safe value %q: %s", safeValue, serialized)
		}
	}

	type cyclicMetadata struct {
		Password string
		Next     *cyclicMetadata
	}
	cycle := &cyclicMetadata{Password: "cycle-secret"}
	cycle.Next = cycle
	serialized = ErrorJsonWithCustomNoStack(errors.New("handler failed"), map[string]any{
		"request": cycle,
	})
	if strings.Contains(serialized, "cycle-secret") {
		t.Fatalf("cyclic structured log retained a secret: %s", serialized)
	}
	if !strings.Contains(serialized, "nesting limit") {
		t.Fatalf("cyclic structured log did not stop at the nesting limit: %s", serialized)
	}
}

func TestErrorJsonWithCustomNoStackRedactsCredentials(t *testing.T) {
	serialized := ErrorJsonWithCustomNoStack(errors.New("handler failed"), map[string]any{
		"headers": http.Header{
			"Authorization": []string{"Bearer raw-secret"},
			"Cookie":        []string{"session=cookie-secret"},
			"Content-Type":  []string{"application/json"},
		},
		"metadata": map[string]any{
			"refresh_token": "refresh-secret",
			"by_jwt":        "jwt-secret",
			"request_body":  "body-secret",
			"operation":     "update",
			"somebody":      "visible",
		},
	})

	for _, secret := range []string{"raw-secret", "cookie-secret", "refresh-secret", "jwt-secret", "body-secret"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("structured error log retained %q: %s", secret, serialized)
		}
	}
	for _, safeValue := range []string{"application/json", "update", "visible", "handler failed"} {
		if !strings.Contains(serialized, safeValue) {
			t.Fatalf("structured error log removed safe value %q: %s", safeValue, serialized)
		}
	}
}
