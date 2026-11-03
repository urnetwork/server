package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/router"
)

// The claim relay's routes answer for the caller's own network, so they need a
// session, and a client token of a network that hands its tokens to a third party
// is refused as for the rest of the provider's payout.
func TestSnClaimRelayRoutesNeedASession(t *testing.T) {
	t.Parallel()
	routes := Routes()
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/sn/claim-relay"},
		{http.MethodPost, "/sn/claim-relay/claim-now"},
	} {
		count := 0
		for _, candidate := range routes {
			if candidate.String() == route.method+" ^"+route.path+"$" {
				count++
			}
		}
		if count != 1 || routeAccessByRoute[route.method+" "+route.path] != routeAccessOwnClientPayout {
			t.Fatalf("route %s %s census=%d access=%d", route.method, route.path, count, routeAccessByRoute[route.method+" "+route.path])
		}
		response := httptest.NewRecorder()
		handler := router.NewRouter(t.Context(), routes)
		handler.ServeHTTP(response, httptest.NewRequest(route.method, route.path, strings.NewReader(`{"coldkey_ss58":"x"}`)))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a session: status %d", route.method, route.path, response.Code)
		}
	}
}
