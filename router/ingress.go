package router

// The ingress headers of a front with no load balancer in front of it.
//
// Behind the load balancer, nginx sets the lb-to-service contract headers on
// every proxied request over any value the client sent (warpctl
// addProxyRequestHeaders): X-UR-Forwarded-For is the address and port it
// accepted the client's connection from, X-Forwarded-For and
// X-Forwarded-Source-Port are cleared, and X-UR-Request-Buffering marks only a
// body the lb streams through. The session takes the client's address from
// X-UR-Forwarded-For whenever it holds one ip:port value
// (session.ResolveClientAddress), on the contract that the ingress overwrote
// it. A front that clients reach directly is its own ingress, since nothing
// stands in front of it to do this: alt's api front and status listener
// (alt/ingress.go) and the proxy's api and status routers, which warp exposes
// by DNAT (proxy/proxy_api.go). It sets the same headers from the connection
// before any handler reads them. Otherwise a client of that front names its
// own address to every per-address limit and address-based decision behind
// it.
//
// Legitimate clients never send these headers, so each one a front overwrites
// or removes is counted (urnetwork_ingress_overwritten_headers_total): a
// sustained rate is a client trying to name an address of its own.

import (
	"net/http"
	"net/netip"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

const (
	urForwardedForHeader      = "X-UR-Forwarded-For"
	forwardedForHeader        = "X-Forwarded-For"
	forwardedSourcePortHeader = "X-Forwarded-Source-Port"
)

// The headers an ingress owns, with the counter's label for each: a closed
// set, decided by the code, never by request content.
var ingressHeaders = []struct {
	header string
	label  string
}{
	{header: urForwardedForHeader, label: "x_ur_forwarded_for"},
	{header: forwardedForHeader, label: "x_forwarded_for"},
	{header: forwardedSourcePortHeader, label: "x_forwarded_source_port"},
	{header: RequestBufferingHeader, label: "x_ur_request_buffering"},
}

var ingressOverwrittenHeadersCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "ingress",
		Name:      "overwritten_headers_total",
		Help:      "Requests to a front with no load balancer in front of it that arrived with a forwarding header the front owns, which it overwrote or removed, by header",
	},
	[]string{"header"},
)

func init() {
	// materialize the whole partition, so a front that never sees one of the
	// headers reads 0 rather than absent
	for _, ingressHeader := range ingressHeaders {
		ingressOverwrittenHeadersCounter.WithLabelValues(ingressHeader.label)
	}
	prometheus.MustRegister(ingressOverwrittenHeadersCounter)
}

// Wraps a handler so every request reaches it with the ingress headers set
// from its connection rather than from its client.
func NewIngressHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetIngressHeaders(r)
		handler.ServeHTTP(w, r)
	})
}

// Sets X-UR-Forwarded-For to the peer the request arrived from, in the form
// nginx renders it (an ipv6 address bracketed, an ipv4-mapped one as ipv4),
// and clears the headers nginx clears. The streaming mark is the lb's: a front
// with no lb reads every body from the client itself, so there a route's own
// body policy is what applies (StreamingBody), never one a client claims.
// Each of these headers the client sent is counted once per request.
//
// A peer the front cannot read leaves no X-UR-Forwarded-For at all, so the
// session falls back to that same peer and refuses it, rather than acting for
// the address the client named.
//
// Call it once per request: a second call would count the header the first
// one set.
func SetIngressHeaders(r *http.Request) {
	for _, ingressHeader := range ingressHeaders {
		if 0 < len(r.Header.Values(ingressHeader.header)) {
			ingressOverwrittenHeadersCounter.WithLabelValues(ingressHeader.label).Inc()
		}
	}
	r.Header.Del(forwardedForHeader)
	r.Header.Del(forwardedSourcePortHeader)
	r.Header.Del(RequestBufferingHeader)
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
