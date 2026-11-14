// Shared checks that an answer carries only its declared status and message.
package handlers

import (
	"net/http"
	"strings"
	"testing"
)

// Requires exactly the declared answer, and that no listed cause text, such
// as a socket, a path, a store endpoint or a Go name, reached the body.
func requireDeclaredAnswer(t testing.TB, status int, body string, wantStatus int, wantMessage string, causes ...string) {
	t.Helper()
	for _, cause := range causes {
		if strings.Contains(body, cause) {
			t.Fatalf("raw error text reached the caller: status=%d %q in %q", status, cause, body)
		}
	}
	if status != wantStatus || body != wantMessage+"\n" {
		t.Fatalf("answer differs: status=%d body=%q, want %d %q", status, body, wantStatus, wantMessage)
	}
}

// An unexpected failure is answered as the provider work routes answer theirs:
// 503 and a fixed message, so the caller retries. No cause text reaches it.
func requirePrivateUnavailable(t testing.TB, status int, body string, causes ...string) {
	t.Helper()
	requireDeclaredAnswer(t, status, body, http.StatusServiceUnavailable, "Service temporarily unavailable.", causes...)
}
