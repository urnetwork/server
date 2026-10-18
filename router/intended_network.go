package router

import (
	"net/http"

	"github.com/urnetwork/server/session"
)

// A request may name, in X-UR-Intended-Network-Id, the network it is meant
// for: the network the client's screen shows as signed in. A route with the
// intended network gate refuses a request whose bearer credential signs in to
// another network, before its handler runs, whatever wrapper the handler uses
// (AUTHZ1.md §10). The answer is 412 with the JSON body
// {"error": ..., "code": "intended_network_mismatch"}, never 401 or 403, which
// the apps take for a revoked sign-in. A request without the header reaches
// the handler unchanged, so a client that does not send it is served exactly
// as before.

// RefuseIntendedNetworkMismatch returns the route with the intended network
// gate in front of its handler (session.CheckIntendedNetwork).
func RefuseIntendedNetworkMismatch(route *Route) *Route {
	refused := *route
	refused.intendedNetworkRefused = true
	refused.handler = intendedNetworkGate(route.handler)
	return &refused
}

// RefusesIntendedNetworkMismatch reports whether the route has the intended
// network gate.
func (self *Route) RefusesIntendedNetworkMismatch() bool {
	return self.intendedNetworkRefused
}

func intendedNetworkGate(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := session.CheckIntendedNetwork(r.Context(), r); err != nil {
			RaiseHttpError(err, w)
			return
		}
		handler(w, r)
	}
}
