package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
)

// TestVerifyHandlersFailClosedWhileSubnetDisabled is the regression for main
// serving /verify/keys and /verify/stats without verify.yml.  st.yml explicitly
// disables the unreleased subsystem, so every route must stop before request
// parsing, vault key/settings loading, or database access and return a stable
// availability response instead of panicking into a 500.
func TestVerifyHandlersFailClosedWhileSubnetDisabled(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: false})
	t.Cleanup(func() { controller.SetStConfig(nil) })

	tests := []struct {
		name    string
		method  string
		target  string
		body    string
		handler http.HandlerFunc
	}{
		{name: "verify", method: http.MethodPost, target: "/verify", body: "not-json", handler: Verify},
		{name: "keys", method: http.MethodGet, target: "/verify/keys", handler: GetVerifyKeys},
		{name: "stats", method: http.MethodGet, target: "/verify/stats?from=not-a-time", handler: GetVerifyStats},
		{name: "proofs", method: http.MethodGet, target: "/verify/proofs?limit=not-a-number", handler: GetVerifyProofs},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			w := httptest.NewRecorder()

			test.handler(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d; body=%q", w.Code, http.StatusServiceUnavailable, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != "3600" {
				t.Fatalf("Retry-After = %q, want 3600", got)
			}
			if got := w.Body.String(); got != "Verification subsystem unavailable.\n" {
				t.Fatalf("body = %q", got)
			}
		})
	}
}
