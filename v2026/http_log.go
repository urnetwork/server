package server

import (
	"net/http"
	"reflect"
	"strings"
	"unicode"
)

const safeLogMaxDepth = 32

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
	return safeLogValue(value, 0)
}

// Carries the recursion depth through the common concrete metadata shapes.
func safeLogValue(value any, depth int) any {
	if safeLogMaxDepth <= depth {
		return "[REDACTED: nesting limit]"
	}

	switch value := value.(type) {
	case http.Header:
		return SafeHttpHeadersForLog(value)
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			if sensitiveLogKey(key) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = safeLogValue(item, depth+1)
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
			result[i] = safeLogValue(item, depth+1)
		}
		return result
	default:
		return safeReflectedLogValue(reflect.ValueOf(value), depth)
	}
}

// Walks arbitrary typed metadata without invoking custom serialization.
func safeReflectedLogValue(value reflect.Value, depth int) any {
	if !value.IsValid() {
		return nil
	}
	if safeLogMaxDepth <= depth {
		return "[REDACTED: nesting limit]"
	}

	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		return safeReflectedLogValue(value.Elem(), depth+1)
	case reflect.Struct:
		valueType := value.Type()
		result := map[string]any{}
		for i := range value.NumField() {
			field := valueType.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name := field.Name
			if jsonName := strings.Split(field.Tag.Get("json"), ",")[0]; jsonName == "-" {
				continue
			} else if jsonName != "" {
				name = jsonName
			}
			if sensitiveLogKey(field.Name) || sensitiveLogKey(name) {
				result[name] = "[REDACTED]"
			} else {
				result[name] = safeReflectedLogValue(value.Field(i), depth+1)
			}
		}
		return result
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return "[REDACTED: unsupported map key]"
		}
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			if sensitiveLogKey(key) {
				result[key] = "[REDACTED]"
			} else {
				result[key] = safeReflectedLogValue(iterator.Value(), depth+1)
			}
		}
		return result
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		result := make([]any, value.Len())
		for i := range value.Len() {
			result[i] = safeReflectedLogValue(value.Index(i), depth+1)
		}
		return result
	default:
		if value.CanInterface() {
			return value.Interface()
		}
		return "[REDACTED: inaccessible value]"
	}
}

// Matches credential-bearing words across JSON, kebab-case, and Go field names.
func sensitiveLogKey(key string) bool {
	runes := []rune(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	var normalized strings.Builder
	for i, current := range runes {
		if unicode.IsUpper(current) && 0 < i && runes[i-1] != '_' &&
			(unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) ||
				(i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			normalized.WriteByte('_')
		}
		normalized.WriteRune(unicode.ToLower(current))
	}
	key = normalized.String()
	for _, fragment := range []string{
		"authorization", "cookie", "password", "passwd", "secret", "seed_phrase",
		"seedphrase",
		"reset_code", "verify_code", "verification_code", "api_key", "private_key",
		"privatekey", "payment", "signature", "webhook",
	} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	for _, word := range []string{
		"token", "jwt", "credential", "body", "payload", "session", "mnemonic",
	} {
		if key == word || strings.HasPrefix(key, word+"_") ||
			strings.HasSuffix(key, "_"+word) || strings.Contains(key, "_"+word+"_") {
			return true
		}
	}
	return false
}
