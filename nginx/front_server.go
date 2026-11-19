package nginx

// The front's public TCP servers (LBALT-IMPL.md §2.7, §7). Connect builds
// each http.Server with its Handler, ErrorLog and BaseContext, and the front
// sets the rest from HttpSettings through ConfigureServer, so connect's
// servers and the parity harness's apply nginx's client limits the same way.
//
// Go's server has neither of nginx's keepalive_requests and keepalive_time,
// so ConfigureServer gives every connection a frontConnection through
// ConnContext: when the server accepted it and how many requests it has
// carried. The front's handlers count each request as it arrives, and the one
// nginx would make the connection's last closes it the way the drain closes
// connections: its response head carries Connection: close, which Go's h1
// server honors by closing the connection after the response, and its h2
// server by sending a GOAWAY and closing once the open streams end.

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// The context key of a connection's frontConnection.
type frontConnectionContextKey struct{}

// One client connection of a public server, as ConfigureServer's ConnContext
// attaches it to the connection's context, which every request on it, each
// h2 stream included, inherits. Safe for concurrent use: an h2 connection's
// streams count themselves concurrently.
type frontConnection struct {
	// when the server accepted the connection, by the front's clock: nginx's
	// c->start_time
	acceptTime time.Time
	// the requests the connection has carried, the current one included:
	// nginx's c->requests
	requestCount atomic.Int64
}

// Sets every field of a public TCP server that derives from HttpSettings, on
// a server whose Handler is one of this front's (LBALT-IMPL.md §7):
//   - ReadHeaderTimeout is ClientHeaderTimeout, which bounds the TLS handshake
//     and h1 request head. NewTlsListener applies it to each h2 header block;
//   - IdleTimeout is KeepaliveTimeout, and a non-positive one turns
//     keepalives off, as nginx's keepalive_timeout 0 does;
//   - MaxHeaderBytes is ServerHeaderBytesMultiple times
//     MaxRequestHeadersTotalBytes, twice by default, so the handlers answer
//     nginx's 400 and 414 for every head within that, and Go's 431 answers
//     only beyond it;
//   - MaxHeaderValueCount, which bounds h1's header lines and h2's header
//     fields, is ServerHeaderLinesMultiple times MaxRequestHeaderLines,
//     twice nginx's max_headers by default, so the handlers answer nginx's
//     400 past its 1000 lines, where Go's default 500 would answer 431 to
//     heads nginx serves;
//   - the h2 stream limit is Http2MaxConcurrentStreams;
//   - the h2 write byte timeout is SendTimeout, so an h2 connection whose
//     frame write stays blocked on the socket that long is closed, which
//     ends every stream on it, as nginx closes an h2 connection whose output
//     stays blocked past send_timeout (ngx_http_v2_write_handler). The
//     handlers' own deadline can't: on h2 it resets only the stream, and the
//     reset waits behind the blocked write;
//   - DisableGeneralOptionsHandler, so `OPTIONS *` reaches the handler;
//   - ConnContext gives each connection the state for KeepaliveRequests and
//     KeepaliveTime, after any ConnContext the server already has.
//
// A non-positive setting leaves Go's default. Nothing bounds a whole request
// or response here, so ReadTimeout and WriteTimeout stay as the caller left
// them, unset by connect: nginx has no such bound, and the handlers bound
// each read and write themselves. TLSConfig is left alone too: connect
// serves TLS through NewTlsListener with the front's one long-lived config,
// which net/http would otherwise write to.
//
// When a write times out over TLS, the server closes the connection, and
// crypto/tls first tries close_notify under a 5 s deadline of its own; the
// handler's write returns only after that close. Connect's public sockets
// fail that write at once (LBALT-IMPL.md §7); on a plain socket the close,
// and with it the handler, waits out those 5 s.
func (self *Front) ConfigureServer(httpServer *http.Server) {
	httpSettings := self.settings.Http
	if 0 < httpSettings.ClientHeaderTimeout {
		httpServer.ReadHeaderTimeout = httpSettings.ClientHeaderTimeout
	}
	if 0 < httpSettings.KeepaliveTimeout {
		httpServer.IdleTimeout = httpSettings.KeepaliveTimeout
	} else {
		httpServer.SetKeepAlivesEnabled(false)
	}
	if 0 < httpSettings.MaxRequestHeadersTotalBytes && 0 < httpSettings.ServerHeaderBytesMultiple {
		httpServer.MaxHeaderBytes = httpSettings.ServerHeaderBytesMultiple * httpSettings.MaxRequestHeadersTotalBytes
	}
	if 0 < httpSettings.MaxRequestHeaderLines && 0 < httpSettings.ServerHeaderLinesMultiple {
		httpServer.MaxHeaderValueCount = httpSettings.ServerHeaderLinesMultiple * httpSettings.MaxRequestHeaderLines
	}
	if 0 < httpSettings.Http2MaxConcurrentStreams || 0 < httpSettings.SendTimeout {
		// a copy, so a config the caller shares is never written
		http2Config := http.HTTP2Config{}
		if httpServer.HTTP2 != nil {
			http2Config = *httpServer.HTTP2
		}
		if 0 < httpSettings.Http2MaxConcurrentStreams {
			http2Config.MaxConcurrentStreams = httpSettings.Http2MaxConcurrentStreams
		}
		if 0 < httpSettings.SendTimeout {
			http2Config.WriteByteTimeout = httpSettings.SendTimeout
		}
		httpServer.HTTP2 = &http2Config
	}
	httpServer.DisableGeneralOptionsHandler = true
	connContext := httpServer.ConnContext
	httpServer.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		if connContext != nil {
			ctx = connContext(ctx, conn)
		}
		return context.WithValue(ctx, frontConnectionContextKey{}, &frontConnection{
			acceptTime: self.now(),
		})
	}
}

// Counts a request on its connection and reports whether it is the
// connection's last, as nginx decides at each h1 request
// (ngx_http_update_location_config) and each new h2 stream
// (ngx_http_v2_state_headers): the request is the KeepaliveRequests-th or
// later, or more than KeepaliveTime has passed since the connection was
// accepted. nginx's third reason, keepalive_timeout 0, is the server's:
// ConfigureServer turns keepalives off then, which Go's h1 server applies to
// each response and its h2 server once the connection is idle.
//
// Only requests that reach the front's handlers count. On h1 the ones Go's
// server answers itself, such as its 431 and 400, close the connection
// anyway. On h2 they don't: a stream Go's server answers itself, 431 to a
// header list over its limit or 400 to a connection-specific header, or
// resets for a malformed head, is not counted and leaves the connection
// open, where nginx counts every stream (LBALT-IMPL.md §4).
//
// False for a request whose connection ConfigureServer didn't set up, such
// as an h3 one.
func (self *Front) lastRequestOnConnection(r *http.Request) bool {
	connection, ok := r.Context().Value(frontConnectionContextKey{}).(*frontConnection)
	if !ok {
		return false
	}
	httpSettings := self.settings.Http
	requestCount := connection.requestCount.Add(1)
	if 0 < httpSettings.KeepaliveRequests && int64(httpSettings.KeepaliveRequests) <= requestCount {
		return true
	}
	return 0 < httpSettings.KeepaliveTime && httpSettings.KeepaliveTime < self.now().Sub(connection.acceptTime)
}
