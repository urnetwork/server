// Request text selects only these fixed API operation classes.
package session

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/v2026/jwt"
)

func TestSessionStateQuerySourceUsesBoundedRouteClasses(t *testing.T) {
	for _, control := range []struct {
		method, path string
		want         jwt.StateQuerySource
	}{
		{"POST", "/connect/control", jwt.StateQueryApiControl},
		{"POST", "/network/auth-client", jwt.StateQueryApiMint},
		{"POST", "/network/register-client-v1", jwt.StateQueryApiMint},
		{"POST", "/network/remove-client", jwt.StateQueryApiRetire},
		{"POST", "/network/remove-clients", jwt.StateQueryApiRetire},
		{"POST", "/network/find-providers2", jwt.StateQueryApiDiscovery},
		{"GET", "/network/provider-locations", jwt.StateQueryApiDiscovery},
		{"GET", "/auth/refresh", jwt.StateQueryApiRefresh},
		{"GET", "/connect/control", jwt.StateQueryApiOther},
		{"POST", "/connect/control/extra-synthetic-identity", jwt.StateQueryApiOther},
		{"GET", "/key/synthetic-identity", jwt.StateQueryApiOther},
	} {
		req := httptest.NewRequest(control.method, "https://fixture.example"+control.path, nil)
		req.Header.Set("X-Jwt-Caller", "prober")
		if got := sessionStateQuerySource(req); got != control.want {
			t.Fatalf("method=%s route class got=%d want=%d", control.method, got, control.want)
		}
	}
	for _, req := range []*http.Request{nil, {}} {
		if sessionStateQuerySource(req) != jwt.StateQueryApiOther {
			t.Fatal("missing URL escaped the API other bucket")
		}
	}
}
