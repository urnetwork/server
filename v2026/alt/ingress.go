package alt

import (
	"net/http"

	"github.com/urnetwork/server/v2026/router"
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
// own value, then the api router, without its status route.
func newApiFrontHandler(apiHandler http.Handler, limits *Limits) http.Handler {
	return NewLimitedHandler(newPublicApiHandler(apiHandler), limits)
}

const statusPath = "/status"

// The api router as the public front serves it: a request for /status, by
// any method, is answered as the router answers a path it has no route for.
// Behind the lb, /status names the api process that answered, for the deploy
// poll and fleet sampling. On alt the deploy poll reads the status listener,
// and over the public udp sockets the route would name the proxy host, the
// alt service, its block and its version to anyone who asks.
func newPublicApiHandler(apiHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == statusPath {
			http.NotFound(w, r)
			return
		}
		apiHandler.ServeHTTP(w, r)
	})
}
