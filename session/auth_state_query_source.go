// Attribute authenticated API state queries without exporting request paths.
package session

import (
	"net/http"

	"github.com/urnetwork/server/jwt"
)

// Exact method/path pairs select fixed operation classes. Dynamic routes and
// unrecognized input remain API other; headers never override the caller.
func sessionStateQuerySource(req *http.Request) jwt.StateQuerySource {
	if req != nil && req.URL != nil {
		switch req.Method + " " + req.URL.Path {
		case "POST /connect/control":
			return jwt.StateQueryApiControl
		case "POST /network/auth-client", "POST /network/register-client-v1":
			return jwt.StateQueryApiMint
		case "POST /network/remove-client", "POST /network/remove-clients":
			return jwt.StateQueryApiRetire
		case "POST /network/find-providers2", "POST /network/find-provider-locations", "POST /network/find-locations", "GET /network/provider-locations":
			return jwt.StateQueryApiDiscovery
		case "GET /auth/refresh":
			return jwt.StateQueryApiRefresh
		}
	}
	return jwt.StateQueryApiOther
}
