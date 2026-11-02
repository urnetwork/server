package alt

import (
	"net/http"
	"net/netip"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The ingress headers of the alt fronts (EXTENDER.md L1).
//
// Behind the load balancer, nginx sets the lb-to-service contract headers on
// every proxied request over any value the client sent (warpctl
// addProxyRequestHeaders): X-UR-Forwarded-For is the address and port it
// accepted the client's connection from, X-Forwarded-For and
// X-Forwarded-Source-Port are cleared, and X-UR-Request-Buffering marks only a
// body the lb streams through. The session takes the client's address from
// X-UR-Forwarded-For whenever it holds one ip:port value
// (session.ResolveClientAddress), on the contract that the ingress overwrote
// it. Alt has no load balancer in front of it, so alt is the ingress: it sets
// the same headers from the connection before any handler reads them.
// Otherwise a client of alt names its own address to every per-address limit
// and address-based decision behind the api, the extender activation probe
// among them.

const (
	urForwardedForHeader      = "X-UR-Forwarded-For"
	forwardedForHeader        = "X-Forwarded-For"
	forwardedSourcePortHeader = "X-Forwarded-Source-Port"
)

// Wraps a handler so every request reaches it with the ingress headers set
// from its connection rather than from its client.
func NewIngressHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setIngressHeaders(r)
		handler.ServeHTTP(w, r)
	})
}

// Sets X-UR-Forwarded-For to the peer the request arrived from, in the form
// nginx renders it (an ipv6 address bracketed, an ipv4-mapped one as ipv4),
// and clears the headers nginx clears. The streaming mark is the lb's: alt
// reads every body from the client itself, so on alt a route's own body
// policy is what applies (router.StreamingBody), never one a client claims.
//
// A peer alt cannot read leaves no X-UR-Forwarded-For at all, so the session
// falls back to that same peer and refuses it, rather than acting for the
// address the client named.
func setIngressHeaders(r *http.Request) {
	r.Header.Del(forwardedForHeader)
	r.Header.Del(forwardedSourcePortHeader)
	r.Header.Del(router.RequestBufferingHeader)
	addrPort, err := server.ParseClientAddress(r.RemoteAddr)
	if err != nil {
		r.Header.Del(urForwardedForHeader)
		return
	}
	r.Header.Set(
		urForwardedForHeader,
		netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port()).String(),
	)
}

// The handler the api front serves, as NewAlt mounts it: the ingress headers
// first, so nothing behind them ever reads a client's own value, then the
// limits, then the api router.
func newApiFrontHandler(apiHandler http.Handler, limits *Limits) http.Handler {
	return NewIngressHandler(NewLimitedHandler(apiHandler, limits))
}
