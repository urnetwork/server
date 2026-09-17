// The in-process API and command-line server share this exact production route.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/v2026/router"
)

// Real routing rejects malformed identities before storage, requires client
// authentication for the new POST route, and still refuses unsupported methods.
func TestSnAttemptArtifactRouteUsesProductionAdmission(t *testing.T) {
	t.Parallel()
	routes := Routes()
	count := 0
	for _, route := range routes {
		if route.String() == "GET ^/sn/attempt-artifact$" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("typed artifact route census=%d, want1", count)
	}
	handler := router.NewRouter(t.Context(), routes)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/sn/attempt-artifact?kind=records&hash=invalid", nil)
		handler.ServeHTTP(response, request)
		want := http.StatusMethodNotAllowed
		if method == http.MethodGet {
			want = http.StatusBadRequest
		} else if method == http.MethodPost {
			want = http.StatusUnauthorized
		}
		if response.Code != want {
			t.Fatalf("typed endpoint did not use production admission: method%s status%d", method, response.Code)
		}
	}
}
