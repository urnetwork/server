package nginx

// Upstream timeouts, attempts, pools and response limits for forwarded
// requests (owner: s-forward). The defaults are the lb image's nginx values
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
	// relayed websocket closes after 60 s without traffic either way.
	// Non-positive means no bound.
	StreamTimeout time.Duration
	// upstream attempts per request, the first included, each on a distinct
	// eligible block and only before any response reaches the client: nginx's
	// `proxy_next_upstream_tries 2` (D7), never more than the service has
	// balanced blocks. A block-specific request makes one. A reused
	// connection that fails before any request byte was written is redone on
	// a fresh one without counting, as nginx's tries++ does. Values below 1
	// mean 1.
	MaxUpstreamAttempts int
	// the largest upstream status line and headers, in bytes: nginx's
	// `proxy_buffer_size`, one 4 KiB page. A larger response head is a 502.
	// Non-positive means no limit.
	MaxUpstreamResponseHeaderBytes int
	// the most requests in flight to one block of a service: nginx's
	// `max_conns=32768` on each server of a service's upstream. A block at
	// its cap is skipped, and a request with no eligible block left is a 502.
	// nginx counts per worker, so its effective cap is larger; this one is per
	// process. A block-specific request isn't capped, as nginx's per-block
	// upstream has no max_conns. Non-positive means no cap.
	MaxBlockActiveRequests int
	// how often a service's idle upstream connections are checked against
	// its keepalive timeout and time, and closed past them. nginx closes each
	// one on its own timer, so this is the granularity of that bound.
	// Non-positive disables the sweep, and an idle connection past its bounds
	// is closed only when it is next taken from the pool.
	KeepaliveSweepInterval time.Duration
	// the read buffer and the write buffer of each upstream connection, in
	// bytes. A response head is limited by MaxUpstreamResponseHeaderBytes, not
	// by this. Non-positive means bufio's default size.
	UpstreamBufferBytes int
	// the buffer a response body copy, or one direction of a tunnel, uses,
	// in bytes, pooled per service. Every read is written to the client and
	// flushed at once, as nginx's `proxy_buffering off` does. Values below 1
	// mean 1.
	CopyBufferBytes int
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
		KeepaliveSweepInterval:         1 * time.Second,
		UpstreamBufferBytes:            4 * 1024,
		CopyBufferBytes:                32 * 1024,
	}
}
