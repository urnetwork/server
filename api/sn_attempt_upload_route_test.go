// The actual API route catalog must expose both immutable reads and bounded
// authenticated staging, including in sim-testnet's in-process server.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/router"
)

// A real dispatch must reach authentication, not a missing-route response.
func TestSnAttemptUploadProductionRouteRequiresClientAuthentication(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	routes := Routes()
	counts := map[string]int{}
	for _, route := range routes {
		counts[route.String()]++
	}
	for _, route := range []string{"GET ^/sn/attempt-artifact$", "POST ^/sn/attempt-artifact$"} {
		if counts[route] != 1 {
			t.Fatalf("production route census for %s is %d, want1", route, counts[route])
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/sn/attempt-artifact", nil)
	response := httptest.NewRecorder()
	router.NewRouter(ctx, routes).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("real upload dispatch status%d, want401", response.Code)
	}
}
