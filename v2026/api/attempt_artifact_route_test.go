// The in-process API and command-line server share this exact production route.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/v2026/router"
)

// Real routing rejects malformed identities before storage, requires client
// authentication for the new POST route, answers the public head, and still
// refuses unsupported methods.
func TestSnAttemptArtifactRouteUsesProductionAdmission(t *testing.T) {
	t.Parallel()
	routes := Routes()
	for _, id := range []string{"GET ^/sn/attempt-artifact$", "HEAD ^/sn/attempt-artifact$"} {
		count := 0
		for _, route := range routes {
			if route.String() == id {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("typed artifact route %s census=%d, want1", id, count)
		}
	}
	handler := router.NewRouter(t.Context(), routes)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/sn/attempt-artifact?kind=records&hash=invalid", nil)
		handler.ServeHTTP(response, request)
		want := http.StatusMethodNotAllowed
		if method == http.MethodGet || method == http.MethodHead {
			want = http.StatusBadRequest
		} else if method == http.MethodPost {
			want = http.StatusUnauthorized
		}
		if response.Code != want {
			t.Fatalf("typed endpoint did not use production admission: method%s status%d", method, response.Code)
		}
	}
}
