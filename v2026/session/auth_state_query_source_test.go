// Request text selects only these fixed API operation classes.
package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionStateQuerySourceUsesBoundedRouteClasses(t *testing.T) {
	for _, control := range []struct {
		method, path string
		want         StateQuerySource
	}{
		{"POST", "/connect/control", StateQueryApiControl},
		{"POST", "/network/auth-client", StateQueryApiMint},
		{"POST", "/network/register-client-v1", StateQueryApiMint},
		{"POST", "/network/remove-client", StateQueryApiRetire},
		{"POST", "/network/remove-clients", StateQueryApiRetire},
		{"POST", "/network/find-providers2", StateQueryApiDiscovery},
		{"GET", "/network/provider-locations", StateQueryApiDiscovery},
		{"GET", "/auth/refresh", StateQueryApiRefresh},
		{"POST", "/auth/network-refresh", StateQueryApiRefresh},
		{"GET", "/auth/network-refresh", StateQueryApiOther},
		{"GET", "/connect/control", StateQueryApiOther},
		{"POST", "/connect/control/extra-synthetic-identity", StateQueryApiOther},
		{"GET", "/key/synthetic-identity", StateQueryApiOther},
	} {
		req := httptest.NewRequest(control.method, "https://fixture.example"+control.path, nil)
		req.Header.Set("X-Jwt-Caller", "prober")
		if got := sessionStateQuerySource(req); got != control.want {
			t.Fatalf("method=%s route class got=%d want=%d", control.method, got, control.want)
		}
	}
	for _, req := range []*http.Request{nil, {}} {
		if sessionStateQuerySource(req) != StateQueryApiOther {
			t.Fatal("missing URL escaped the API other bucket")
		}
	}
}
