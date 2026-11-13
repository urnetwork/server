// Provider refusals are private operational evidence, never an honesty verdict.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The ordinary error string is safe for operational logs. Detailed fields and
// the redacted body are explicitly private, not a public submission failure.
type winnerReviewProviderRefusal struct {
	StatusCode          int    `json:"http_status"`
	Code                string `json:"provider_code"`
	Type                string `json:"provider_type"`
	RequestId           string `json:"provider_request_id"`
	CapturedBodySha256  string `json:"captured_body_sha256"`
	CapturedBodyBytes   int    `json:"captured_body_bytes"`
	BodyComplete        bool   `json:"body_complete"`
	PrivateRedactedBody []byte `json:"-"`
}

// Neither provider text nor credentials are interpolated into normal logs.
func (self *winnerReviewProviderRefusal) Error() string {
	return fmt.Sprintf("review provider refused request (HTTP %d)", self.StatusCode)
}

// Captures at most the owner's bound, distinguishes complete from truncated
// bytes, and redacts even JSON-escaped echoes of the dedicated credential.
func readWinnerModelRefusal(response *http.Response, key string, settings winnerModelSettings) *winnerReviewProviderRefusal {
	result := &winnerReviewProviderRefusal{StatusCode: response.StatusCode}
	if settings.MaximumRefusalBytes <= 0 {
		return result
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, settings.MaximumRefusalBytes+1))
	defer clear(content)
	result.BodyComplete = err == nil && int64(len(content)) <= settings.MaximumRefusalBytes
	if int64(len(content)) > settings.MaximumRefusalBytes {
		content = content[:settings.MaximumRefusalBytes]
	}
	digest := sha256.Sum256(content)
	result.CapturedBodySha256 = hex.EncodeToString(digest[:])
	result.CapturedBodyBytes = len(content)
	requestId := response.Header.Get("x-request-id")
	if int64(len(requestId)) <= settings.MaximumRefusalBytes {
		result.RequestId = redactWinnerProviderString(requestId, key)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if !result.BodyComplete || decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		// Malformed/truncated bytes retain only a bounded digest, never potentially
		// encoded credential material masquerading as a private diagnostic body.
		return result
	}
	var redact func(any) any
	redact = func(value any) any {
		switch value := value.(type) {
		case string:
			return redactWinnerProviderString(value, key)
		case []any:
			for i := range value {
				value[i] = redact(value[i])
			}
			return value
		case map[string]any:
			redacted := map[string]any{}
			for name, child := range value {
				redacted[redactWinnerProviderString(name, key)] = redact(child)
			}
			return redacted
		default:
			return value
		}
	}
	value = redact(value)
	if object, ok := value.(map[string]any); ok {
		if details, ok := object["error"].(map[string]any); ok {
			result.Code, _ = details["code"].(string)
			result.Type, _ = details["type"].(string)
		}
	}
	result.PrivateRedactedBody, _ = json.Marshal(value)
	return result
}

// Redaction is applied after JSON decoding as well as to response headers.
func redactWinnerProviderString(value string, key string) string {
	if key == "" {
		return value
	}
	return strings.ReplaceAll(value, key, "[redacted]")
}
