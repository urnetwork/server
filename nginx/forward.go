package nginx

// The contract between the front's routing and a service's upstream side
// (owner: s-forward). Routing (s-http) picks the service, the block scope and
// the route's options; the forwarder carries out nginx's request contract
// (§4.2): X-UR-Forwarded-For from the accepted peer, the other ingress
// headers and Early-Data removed, Host normalized with no port, websocket
// upgrades, streamed or buffered bodies (D7, D19, D21), retries and the
// response limits.

import (
	"net/http"
	"net/netip"
	"time"
)

// One request's forwarding options, set by routing.
type forwardOptions struct {
	// the interface the request arrived on
	interfaceName string
	// the accepted peer. X-UR-Forwarded-For is set from it, never from the
	// client's own headers.
	clientAddrPort netip.AddrPort
	// set for a per-block lb route (`/by/b/<service>/<block>/`): that block
	// only, which reports its own result and is never retried elsewhere.
	// Empty means any eligible block of the service.
	blockName string
	// the path and query to send upstream. Empty sends the normalized request
	// URI.
	upstreamPath string
	// the body streams through unbuffered, with X-UR-Request-Buffering: off,
	// and is no longer eligible for a retry once forwarding begins
	streamed bool
	// the service relays websocket upgrades
	websocket bool
	// the longest wait between two reads of the body: a route's
	// client_body_timeout_seconds, else BodySettings.ClientBodyTimeout
	clientBodyTimeout time.Duration
	// the bytes of the body held in memory before the rest spills: a route's
	// client_body_buffer_size, else BodySettings.MemoryPrefixBytes
	clientBodyBufferSize int64
	// the upstream read timeout: ForwardSettings.StreamTimeout on websocket
	// and streamed routes, else ForwardSettings.ProxyReadTimeout
	readTimeout time.Duration
}

// A service's upstream side as routing sees it. Safe for concurrent use.
type serviceForwarder interface {
	// forwards one request and writes the upstream's response, or the front's
	// own error response (413, 502, 503, 504), to w
	serveHttp(w http.ResponseWriter, r *http.Request, options forwardOptions)
	// releases the pools; requests in flight end with their connections
	close()
}
