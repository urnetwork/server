package server

import (
	"net/http"
	"strings"
)

// SafeHttpHeadersForLog returns only low-sensitivity request metadata. It is
// intentionally an allowlist: newly introduced credential headers cannot
// silently become loggable.
func SafeHttpHeadersForLog(header http.Header) map[string][]string {
	safe := map[string][]string{}
	for _, name := range []string{
		"Content-Length",
		"Content-Type",
		"User-Agent",
		"X-Ur-Appversion",
		"X-Ur-Transportversion",
	} {
		if values := header.Values(name); len(values) != 0 {
			safe[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	return safe
}

// SafeLogValue recursively removes values whose keys commonly carry secrets.
// It is defense in depth for structured error metadata; callers should still
// prefer logging only explicit, non-sensitive fields.
func SafeLogValue(value any) any {
	switch value := value.(type) {
	case http.Header:
		return SafeHttpHeadersForLog(value)
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			if sensitiveLogKey(key) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = SafeLogValue(item)
			}
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(value))
		for key, item := range value {
			if sensitiveLogKey(key) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = item
			}
		}
		return result
	case map[string][]string:
		result := make(map[string][]string, len(value))
		for key, items := range value {
			if sensitiveLogKey(key) {
				result[key] = []string{"[REDACTED]"}
			} else {
				result[key] = append([]string(nil), items...)
			}
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = SafeLogValue(item)
		}
		return result
	default:
		return value
	}
}

func sensitiveLogKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	for _, fragment := range []string{
		"authorization", "cookie", "password", "passwd", "secret", "seedphrase",
		"reset_code", "verify_code", "verification_code", "api_key", "access_token",
		"refresh_token", "payment", "signature", "signed_payload", "webhook",
	} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}
