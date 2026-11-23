package alt

import (
	"net/http"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The ingress headers of the alt fronts (EXTENDER.md L1).
//
// Alt has no load balancer in front of it, so alt is the ingress: it sets the
// headers nginx sets for a service behind the lb from the connection, before
// any handler reads them (router/ingress.go). Otherwise a client of alt names
// its own address to every per-address limit and address-based decision
// behind the api, the extender activation probe among them.
//
// The api front takes them inside its limits (NewLimitedHandler), so no front
// built on the limits can serve a client's own value; the status listener,
// which has no limits, takes them from NewIngressHandler.

// Wraps a handler so every request reaches it with the ingress headers set
// from its connection rather than from its client.
func NewIngressHandler(handler http.Handler) http.Handler {
	return router.NewIngressHandler(handler)
}

// The handler the api front serves, as NewAlt mounts it: the limits, which
// set the ingress headers first, so nothing behind them ever reads a client's
// own value, then the api router, without its status route and without what
// the lb serves only to its own sources.
func newApiFrontHandler(apiHandler http.Handler, limits *Limits) http.Handler {
	return NewLimitedHandler(newPublicApiHandler(apiHandler), limits)
}

const (
	statusPath = "/status"
	// §21.16's staff route, x-internal
	abuseStaffPath = "/abuse/action"
)

// Whether the path is one the lb refuses to a source outside the ones it
// lists (§19.4's api row): api's internal gate surface, which the lb answers
// 403 outside the site's ranges (QD-055, warp's internal_routes), and the
// abuse staff's route, which it refuses outside the operators' addresses
// (owner decision 532). Alt has no lb in front and no side the site reaches it
// by: the gate hosts call api at https://api.<domain> through the lb, as the
// staff's sidecarctl abuse does, and none of them speaks HTTP/3. So alt
// refuses both to every caller, as the lb refuses an outside one, and a
// bearer that leaked is no use over alt either. The router matches the same
// decoded path, so no other spelling reaches either route.
func lbRestrictedPath(path string) bool {
	return strings.HasPrefix(path, server.InternalGatePathPrefix) || path == abuseStaffPath
}

// The api router as the public front serves it: a request for /status, by
// any method, is answered as the router answers a path it has no route for.
// Behind the lb, /status names the api process that answered, for the deploy
// poll and fleet sampling. On alt the deploy poll reads the status listener,
// and over the public udp sockets the route would name the proxy host, the
// alt service, its block and its version to anyone who asks. A path the lb
// serves only to its own sources (lbRestrictedPath) is answered 403, as the
// lb answers an outside source, by any method and before the router reads the
// request.
func newPublicApiHandler(apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == statusPath:
			http.NotFound(w, r)
		case lbRestrictedPath(r.URL.Path):
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		default:
			apiHandler.ServeHTTP(w, r)
		}
	})
}
