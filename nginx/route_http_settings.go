package nginx

// Client-facing HTTP behavior of the front's handlers (owner: s-http):
// nginx's client timeouts, header limits and keepalive, the drain (D12), and
// gzip of text/html. The defaults are the lb image's nginx values
// (warp/warpctl/config.go:1338-1363) and the nginx defaults its config leaves
// in effect (ngx_http_core_module.c, ngx_http_v2_module.c,
// ngx_http_gzip_filter_module.c at the pinned commit).

import (
	"time"
)

// How the front serves clients. Read-only once the front is built.
//
// The handlers apply the header limits, SendTimeout, the drain, gzip and
// the access log themselves. The rest are the servers', so connect's: an
// http.Server's ReadHeaderTimeout is ClientHeaderTimeout, its IdleTimeout
// KeepaliveTimeout, and its HTTP2.MaxConcurrentStreams
// Http2MaxConcurrentStreams. Its MaxHeaderBytes must be above
// MaxRequestHeadersTotalBytes, twice it say, so the handler sees every head
// nginx would answer with a 414 or 400 instead of Go answering 431; and it
// sets no WriteTimeout, which would cut a stream SendTimeout lets live. Go's
// server has no KeepaliveRequests or KeepaliveTime, nor nginx's lingering
// close, so those need per-connection state connect's server would have to
// provide.
type HttpSettings struct {
	// how long a new connection has for its TLS handshake, request line and
	// headers, and a keepalive connection for a request's headers once they
	// start: nginx's `client_header_timeout 15s`. A stall past it closes the
	// connection without a response, as nginx does. Non-positive means no
	// bound.
	ClientHeaderTimeout time.Duration
	// how long an idle client connection, h1 or h2, waits for its next
	// request: nginx's `keepalive_timeout 1m`. Non-positive closes each
	// connection after its response, as nginx's 0 does.
	KeepaliveTimeout time.Duration
	// the most requests one client connection serves, the last response
	// carrying Connection: close or a GOAWAY: nginx's default
	// `keepalive_requests 1000`. Non-positive means no limit.
	KeepaliveRequests int
	// the longest one client connection serves requests, after which its
	// current request is its last: nginx's default `keepalive_time 1h`.
	// Non-positive means no limit.
	KeepaliveTime time.Duration
	// the longest wait between two writes to a client: nginx's
	// `send_timeout 30s`. A stall past it closes the connection. Non-positive
	// means no bound.
	SendTimeout time.Duration
	// the longest request line in bytes, one of nginx's
	// `large_client_header_buffers 8 8k`. A longer one is a 414. Non-positive
	// means no limit.
	MaxRequestLineBytes int
	// the longest single request header, name and value, in bytes, from the
	// same buffers. A longer one is a 400. Non-positive means no limit.
	MaxRequestHeaderBytes int
	// the request line and all headers together, in bytes: nginx's eight
	// 8 KiB buffers. More is a 400. nginx fills whole buffers, so it can
	// refuse a header set a little under this. Non-positive means no limit.
	MaxRequestHeadersTotalBytes int
	// concurrent streams on one h2 connection: nginx's default
	// `http2_max_concurrent_streams 128`. Each stream counts against the
	// address's concurrency limit (D8). Non-positive leaves Go's default.
	Http2MaxConcurrentStreams int
	// after an error response sent before the body was read, such as a 413,
	// how long the front keeps reading and discarding the body so the client
	// can read the response: nginx's default `lingering_time 30s`.
	// Non-positive closes at once.
	LingeringTime time.Duration
	// the longest wait between two reads while lingering: nginx's default
	// `lingering_timeout 5s`. Non-positive means no bound within
	// LingeringTime.
	LingeringTimeout time.Duration
	// how long a drain lets accepted requests and forwarded websockets run
	// before they are cut: nginx's `worker_shutdown_timeout 60m`, the same as
	// connect's resident drain (D12). Non-positive means no deadline, as an
	// unset worker_shutdown_timeout.
	DrainTimeout time.Duration
	// how long Close waits for the forced close of what outlived the drain,
	// so a stuck connection can't hold the exit (D12's bounded completion
	// path). alt bounds its own close at 30 s. Non-positive returns without
	// waiting.
	ForcedCloseTimeout time.Duration
	// the shortest text/html response gzip compresses, in bytes: nginx's
	// default `gzip_min_length 20`. A response of unknown length is
	// compressed. Non-positive compresses any length.
	GzipMinLength int
	// the gzip compression level: nginx's default `gzip_comp_level 1`.
	// Non-positive disables gzip.
	GzipLevel int
	// one request in this many is written to the access log, at glog's V(1)
	// only (D17): nginx logged every request at the default level, and the
	// lines served only ad hoc counts, which a sample answers. Hosts in
	// no_access_log and successes on no_success_access_log paths are never
	// written. Non-positive writes none.
	AccessLogSampleInterval int
}

// A new instance on every call, with nginx's values.
func DefaultHttpSettings() *HttpSettings {
	return &HttpSettings{
		ClientHeaderTimeout:         15 * time.Second,
		KeepaliveTimeout:            1 * time.Minute,
		KeepaliveRequests:           1000,
		KeepaliveTime:               1 * time.Hour,
		SendTimeout:                 30 * time.Second,
		MaxRequestLineBytes:         8 * 1024,
		MaxRequestHeaderBytes:       8 * 1024,
		MaxRequestHeadersTotalBytes: 8 * 8 * 1024,
		Http2MaxConcurrentStreams:   128,
		LingeringTime:               30 * time.Second,
		LingeringTimeout:            5 * time.Second,
		DrainTimeout:                60 * time.Minute,
		ForcedCloseTimeout:          30 * time.Second,
		GzipMinLength:               20,
		GzipLevel:                   1,
		AccessLogSampleInterval:     100,
	}
}
