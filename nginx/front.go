package nginx

// The front's API (LBALT-IMPL.md §2.7). One Front per connect process serves
// every interface. This file is the skeleton the s-http unit implements;
// until then NewFront refuses, so nothing can run half-built.

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/netip"
)

// How the front treats a served name. Names match by nginx's server_name
// precedence: an exact name first, then the longest leading wildcard.
type NameClass int

const (
	// not a name the route table serves
	NameClassUnknown NameClass = iota
	// an lb name: lb status and the hidden per-block routes. The first lb
	// name is the TCP default server.
	NameClassLb
	// the connect service: served in-process on every protocol, never
	// forwarded
	NameClassConnect
	// the api service: forwarded, and admitted on h3/whodis
	NameClassApi
	// every other fronted service
	NameClassForwarded
	// a redirect alias: a 301 to its target
	NameClassRedirect
)

// A short lowercase name, bounded for logs and metric labels. A value outside
// the constants is "unknown".
func (self NameClass) String() string {
	switch self {
	case NameClassLb:
		return "lb"
	case NameClassConnect:
		return "connect"
	case NameClassApi:
		return "api"
	case NameClassForwarded:
		return "forwarded"
	case NameClassRedirect:
		return "redirect"
	default:
		return "unknown"
	}
}

// The lb_alt front of one connect process. It owns the name matcher, the two
// long-lived TLS configs, the upstream pools, the body store and the limit
// state, which is keyed by (interface name, normalized client IP), so one
// source on two interfaces never shares a quota. Its methods are safe for
// concurrent use, and so are the handlers they return.
type Front struct {
}

// Builds the front from a loaded route table and its certificates; revision
// is the route file's, from LoadRouteTable. connectHandler serves connect
// authorities in-process (the websocket and the H1+ upgrade): the front
// applies the ingress rewrite and the D8 limits around it and never forwards
// a connect name. version is connect's WARP_VERSION, which the lb status
// route reports. The front's own goroutines end with ctx; Close releases what
// it holds.
func NewFront(ctx context.Context, routeTable *RouteTable, revision string, certificateStore *CertificateStore, connectHandler http.Handler, version string, settings *FrontSettings) (*Front, error) {
	return nil, errors.New("lbalt: front not implemented")
}

// The class of a name, normalized first as nginx's $host is.
// NameClassUnknown when the table doesn't serve it. Connect dispatches QUIC
// by it: connect names to its own handler, api names to H3ApiHandler, and
// any other name is closed with 0x1000.
func (self *Front) ClassifyName(name string) NameClass {
	panic("lbalt: not implemented")
}

// The base TLS config for every TCP 443 socket: one long-lived config, so
// session tickets resume across the block's sockets (D6c).
// GetConfigForClient picks the certificate by SNI, the default server's for
// a missing or unknown one, and ALPN by class: http/1.1 only for connect
// names, h2 and http/1.1 for the rest. Serve it through tls.NewListener and
// http.Server.Serve; ServeTLS would clone it.
func (self *Front) TlsConfig() *tls.Config {
	panic("lbalt: not implemented")
}

// The base TLS config for every QUIC socket: one long-lived config, for
// connect and api names, with alt's ALPN rule (the client's offer intersected
// with [h3], so raw-QUIC connect without ALPN still passes). The public QUIC
// config that uses it sets Allow0RTT false.
func (self *Front) QuicTlsConfig() *tls.Config {
	panic("lbalt: not implemented")
}

// The TCP 443 handler for one interface. It routes each request by its
// authority (Host on h1, each stream's :authority on h2), not by SNI, with
// nginx's server_name semantics, and carries everything per request: the
// ingress rewrite, the D8 limits, body handling, forwarding with retries,
// redirects, the /status deny and the added response headers. It expects
// r.RemoteAddr to be the accepted peer and r.TLS to be set.
func (self *Front) HttpsHandler(interfaceName string) http.Handler {
	panic("lbalt: not implemented")
}

// The TCP 80 handler for one interface: on lb names the lb status routes,
// which answer 200 with the status json and never redirect (Route53's health
// check depends on it), and otherwise a 301 to https for every served name
// and a deny for any other. It expects r.RemoteAddr to be the accepted peer.
func (self *Front) HttpHandler(interfaceName string) http.Handler {
	panic("lbalt: not implemented")
}

// The h3 handler for one interface's api connections. It serves only api
// authorities, so a connection whose SNI is an api name can't carry a request
// for another service (D3), and forwards them as HttpsHandler does.
func (self *Front) H3ApiHandler(interfaceName string) http.Handler {
	panic("lbalt: not implemented")
}

// Admits one QUIC connection from addr on the interface, under alt's
// per-address connection cap (D8): the interface's rate_limit.net_connections,
// counted per (interface name, normalized client IP) apart from HTTP
// concurrency, with exclude_subnets exempt and zero meaning no cap. ok is false
// when the address is at its cap, and connect then closes the connection with
// application error 0x1001. A true result must be paired with exactly one call
// of release, when the connection ends.
func (self *Front) AcquireQuicConnection(interfaceName string, addr netip.Addr) (release func(), ok bool) {
	panic("lbalt: not implemented")
}

// Starts the drain (D12): the front stops admitting new HTTP work, idle
// keepalive connections close, and responses carry Connection: close.
// Accepted requests and forwarded websockets run on until they end or the
// drain timeout passes. Connect calls it right after SetDrainingIfReady,
// while its transports can still send.
func (self *Front) BeginDrain() {
	panic("lbalt: not implemented")
}

// Whether BeginDrain has run.
func (self *Front) Draining() bool {
	panic("lbalt: not implemented")
}

// The revision of the route file the front was built from, the lowercase hex
// sha256 of its bytes, reported in X-UR-Front-Route-Revision.
func (self *Front) Revision() string {
	panic("lbalt: not implemented")
}

// Releases the upstream pools and spill files, cutting whatever outlived the
// drain, and waits for that at most HttpSettings.ForcedCloseTimeout.
func (self *Front) Close() error {
	panic("lbalt: not implemented")
}
