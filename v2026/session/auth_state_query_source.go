// Attribute authenticated API state queries without exporting request paths.
package session

import (
	"net/http"
)

// Exact method/path pairs select fixed operation classes. Dynamic routes and
// unrecognized input remain API other; headers never override the caller.
func sessionStateQuerySource(req *http.Request) StateQuerySource {
	if req != nil && req.URL != nil {
		switch req.Method + " " + req.URL.Path {
		case "POST /connect/control":
			return StateQueryApiControl
		case "POST /network/auth-client", "POST /network/register-client-v1":
			return StateQueryApiMint
		case "POST /network/remove-client", "POST /network/remove-clients":
			return StateQueryApiRetire
		case "POST /network/find-providers2", "POST /network/find-provider-locations", "POST /network/find-locations", "GET /network/provider-locations":
			return StateQueryApiDiscovery
		case "GET /auth/refresh", "POST /auth/network-refresh":
			return StateQueryApiRefresh
		}
	}
	return StateQueryApiOther
}
