package nginx

// Upstream timeouts, attempts and response limits for forwarded requests
// (owner: s-forward). The defaults are the lb image's nginx values
// (warp/warpctl/config.go:1338-1363, 1838, 2526-2556) and D7's retry rule.

import (
	"time"
)

// How the front talks to upstream blocks. Read-only once the front is built.
type ForwardSettings struct {
	// the longest a dial to an upstream block may take: nginx's
	// `proxy_connect_timeout 30s`. A dial past it is a timeout, which D7
	// retries on another block. Non-positive means no bound beyond the
	// request's own context.
	ProxyConnectTimeout time.Duration
	// on buffered routes, the longest wait between two reads from the
	// upstream, the response head included: nginx's `proxy_read_timeout 30s`.
	// A wait past it before the response head is a timeout, which D7 retries,
	// and a 504 when no attempt is left. Non-positive means no bound.
	ProxyReadTimeout time.Duration
	// on buffered routes, the longest wait between two writes to the
	// upstream: nginx's `proxy_send_timeout 30s`. Non-positive means no bound.
	ProxySendTimeout time.Duration
	// on websocket and streamed routes, both the read and the send timeout:
	// nginx sets `proxy_read_timeout 60s; proxy_send_timeout 60s` there, so a
	// relayed websocket closes after 60 s without traffic. Non-positive means
	// no bound.
	StreamTimeout time.Duration
	// upstream attempts per request, the first included, each on a distinct
	// eligible block and only before any response reaches the client: nginx's
	// `proxy_next_upstream_tries 2` (D7). A block-specific request makes one.
	// Values below 1 mean 1.
	MaxUpstreamAttempts int
	// the largest upstream status line and headers, in bytes: nginx's
	// `proxy_buffer_size`, one 4 KiB page. A larger response head is a 502.
	// Non-positive means no limit.
	MaxUpstreamResponseHeaderBytes int
	// the most requests in flight to one block of a service: nginx's
	// `max_conns=32768` on each server of a service's upstream. A block at
	// its cap is skipped, and a request with no eligible block left is a 502.
	// nginx counts per worker, so its effective cap is larger; this one is per
	// process. Non-positive means no cap.
	MaxBlockActiveRequests int
}

// A new instance on every call, with nginx's values.
func DefaultForwardSettings() *ForwardSettings {
	return &ForwardSettings{
		ProxyConnectTimeout:            30 * time.Second,
		ProxyReadTimeout:               30 * time.Second,
		ProxySendTimeout:               30 * time.Second,
		StreamTimeout:                  60 * time.Second,
		MaxUpstreamAttempts:            2,
		MaxUpstreamResponseHeaderBytes: 4 * 1024,
		MaxBlockActiveRequests:         32768,
	}
}
