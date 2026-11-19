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
// the access log themselves. Front.ConfigureServer sets the rest on each
// public server (front_server.go): its ReadHeaderTimeout is
// ClientHeaderTimeout, its IdleTimeout KeepaliveTimeout, its
// HTTP2.MaxConcurrentStreams Http2MaxConcurrentStreams, its
// HTTP2.WriteByteTimeout SendTimeout, and its MaxHeaderBytes
// ServerHeaderBytesMultiple times MaxRequestHeadersTotalBytes, so the
// handler sees every head nginx would answer with a 414 or 400 instead of Go
// answering 431. It sets no WriteTimeout, which would cut a stream
// SendTimeout lets live. KeepaliveRequests and KeepaliveTime, which Go's
// server lacks, are applied per connection, from state ConfigureServer
// attaches.
//
// Of nginx's lingering close only LingeringTimeout applies: Go's h1 server
// has no lingering_time. nginx answers a request whose body is unread at
// once, then reads and discards the body for up to lingering_time, 30 s,
// each read waiting at most lingering_timeout. Go's reads up to 256 KiB of
// it, which LingeringTimeout bounds: before an answer that keeps the
// connection, after one that closes it. When more is left, or that read is
// cut, or the client asked to close, the connection closes with the body
// unread: Go half-closes it and closes it 500 ms later, reading nothing
// more, which the front has it do in every such case (responseState.finish),
// so a client still sending reads the answer first. One still sending after
// those 500 ms gets a reset (LBALT-IMPL.md §4).
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
	// the longest a write to a client may stay blocked: nginx's
	// `send_timeout 30s`, which runs only while a write waits on the client,
	// so a response may go quiet between two writes for as long as its
	// upstream does. A write blocked past it closes the connection on h1. On
	// h2 it resets the stream, and a frame write blocked on the socket past
	// it, which no reset can get past, closes the connection and so every
	// stream on it, as nginx does (Front.ConfigureServer). Non-positive means
	// no bound.
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
	// what a public server reads of a request line and headers before its
	// handler sees them, its MaxHeaderBytes, as a multiple of
	// MaxRequestHeadersTotalBytes: 2 (LBALT-IMPL.md §7). Within it the
	// handlers answer nginx's own 400 and 414; only a head beyond it gets
	// Go's 431, the band LBALT-IMPL.md §4 accepts. A multiple keeps the
	// server's bound tied to nginx's buffers. Non-positive leaves Go's
	// default, 1 MiB.
	ServerHeaderBytesMultiple int
	// concurrent streams on one h2 connection: nginx's default
	// `http2_max_concurrent_streams 128`. Each stream counts against the
	// address's concurrency limit (D8). Non-positive leaves Go's default.
	Http2MaxConcurrentStreams int
	// on h1, the longest Go's server may spend reading a body the request
	// left unread for an early answer, the front's own or the forwarder's
	// refusal, before the answer or, when it closes the connection, after
	// it: nginx's default `lingering_timeout 5s`, its bound on each
	// lingering read. A read it cuts has the connection closed after the
	// answer. Non-positive cuts that read at once.
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
		ServerHeaderBytesMultiple:   2,
		Http2MaxConcurrentStreams:   128,
		LingeringTimeout:            5 * time.Second,
		DrainTimeout:                60 * time.Minute,
		ForcedCloseTimeout:          30 * time.Second,
		GzipMinLength:               20,
		GzipLevel:                   1,
		AccessLogSampleInterval:     100,
	}
}
