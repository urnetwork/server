package connect

// The front's tcp sockets: TCP 443 terminates TLS and serves HTTP/1.1 and h2
// through the nginx front's HttpsHandler, and TCP 80 serves its HttpHandler
// (LBALT-IMPL.md §2.8).
//
// Each interface has one http.Server per port, serving every socket of that
// port on the interface's addresses, so the drain reaches each with one
// Shutdown. TLS is the nginx front's one long-lived config through
// tls.NewListener, never cloned: http.Server.ServeTLS would clone it, and
// session tickets would then stop resuming across the block's sockets (D6c).
//
// Every accepted connection is tracked from its accept to its close, under
// TLS and through a hijack, so the drain can wait for the websockets and H1+
// tunnels http.Server forgets once hijacked, and the forced close can cut
// them (D12). Once a write to one fails on its deadline, every later write
// fails at once, so TLS's close_notify can't hold a handler on a client that
// stopped reading; and a write left without a deadline is bounded by
// send_timeout, so crypto/tls's own writes, such as its answer to a client's
// KeyUpdate request, can't hold a connection for good (frontTrackedConn).
//
// net/http logs handshake failures and handler panics with the client's
// address, so each server's error log counts them instead; a panic is still
// logged, with the address cut out.

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog/v2026"
)

// One bound public tcp listener, served by its interface's server for the
// port.
type frontTcpRun struct {
	frontServer *frontServer
	socket      *frontSocket
	listener    net.Listener
}

// Binds one tcp socket.
func (self *frontServer) openTcpSocket(socket *frontSocket) (*frontTcpRun, error) {
	listen := self.settings.Listen
	if listen == nil {
		listen = listenFront
	}
	listener, err := listen(socket.key.network(), socket.key.bindAddress())
	if err != nil {
		// a non-nil result is the front's to close, even with an error
		if listener != nil {
			listener.Close()
		}
		return nil, err
	}
	if listener == nil {
		return nil, errors.New("the listen returned no listener")
	}
	return &frontTcpRun{
		frontServer: self,
		socket:      socket,
		listener:    listener,
	}, nil
}

// Serves the listener until it is lost or its server shuts down.
func (self *frontTcpRun) serve() error {
	var listener net.Listener = &frontTrackedListener{
		Listener:    self.listener,
		frontServer: self.frontServer,
	}
	if self.socket.key.portKind == frontPortHttps {
		listener = tls.NewListener(listener, self.frontServer.tlsConfig)
	}
	return self.socket.httpServer.Serve(listener)
}

// The tcp drain is its server's (frontServer.beginDrain), which reaches every
// socket of the port at once.
func (self *frontTcpRun) beginDrain(drainCtx context.Context) {
}

// Closes the listener. Accepted connections are not the run's: they outlive
// it, tracked by the front server.
func (self *frontTcpRun) close() {
	self.listener.Close()
}

// A server for one interface's port. connect sets what is its own: the
// handler, the error log that counts and the serving context. The nginx
// front sets the rest from its HttpSettings (nginx.Front.ConfigureServer,
// LBALT-IMPL.md §7), as the parity harness's servers get them: the header
// timeout, which covers the TLS handshake, request line and headers; the
// idle timeout; header bytes of a multiple of nginx's header buffers, twice
// by default, within which the nginx front's handlers apply nginx's own
// limits; the h2 stream limit; the h2 write byte timeout, send_timeout,
// which closes an h2 connection whose frame write stays blocked on the
// socket; and keepalive_requests and keepalive_time per connection. Nothing
// bounds a whole request or response here: bodies, uploads, websockets and
// streamed responses keep the per-read and per-write timeouts the nginx
// front applies.
//
// A parity difference (LBALT-IMPL.md §4): net/http reads at most the header
// bytes, twice nginx's 64 KiB of buffers by default, plus its own 4 KiB
// slack before the handler, so an HTTP/1.1 request line and headers over
// that in all, about 132 KiB, are answered 431 by net/http and never reach
// the nginx front, where nginx answers 400, or 414 for a long request line.
// Under that size the nginx front answers nginx's codes.
func (self *frontServer) newHttpServer(handler http.Handler, portKind string) *http.Server {
	httpServer := &http.Server{
		Handler: handler,
		ErrorLog: log.New(&frontHttpErrorLog{
			portKind: portKind,
			observer: self.settings.httpErrorObserverForTest,
		}, "", 0),
		// requests end with the front
		BaseContext: func(net.Listener) context.Context {
			return self.ctx
		},
	}
	self.front.ConfigureServer(httpServer)
	return httpServer
}

// Admits an accepted tcp connection into the tracked set, or refuses it once
// the forced close began.
func (self *frontServer) trackTcpConn(conn net.Conn) (*frontTrackedConn, bool) {
	trackedConn := &frontTrackedConn{
		Conn:        conn,
		frontServer: self,
		sendTimeout: self.settings.Front.Http.SendTimeout,
	}
	tracked := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing {
			return false
		}
		self.tcpConns[trackedConn] = true
		return true
	}()
	if !tracked {
		return nil, false
	}
	self.openCount.Update(func(openCount int) int {
		return openCount + 1
	})
	return trackedConn, true
}

// Removes a closed tcp connection from the tracked set.
func (self *frontServer) untrackTcpConn(trackedConn *frontTrackedConn) {
	untracked := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.tcpConns[trackedConn] {
			return false
		}
		delete(self.tcpConns, trackedConn)
		return true
	}()
	if untracked {
		self.openCount.Update(func(openCount int) int {
			return openCount - 1
		})
	}
}

// Closes every tracked tcp connection: the forced close of what outlived
// the drain, hijacked ones included. The raw sockets close at once; TLS on
// top sends nothing more.
func (self *frontServer) closeTcpConns() {
	trackedConns := func() []*frontTrackedConn {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		trackedConns := make([]*frontTrackedConn, 0, len(self.tcpConns))
		for trackedConn := range self.tcpConns {
			trackedConns = append(trackedConns, trackedConn)
		}
		return trackedConns
	}()
	for _, trackedConn := range trackedConns {
		trackedConn.Close()
	}
}

// A public listener whose connections the front tracks.
type frontTrackedListener struct {
	net.Listener
	frontServer *frontServer
}

// Accepts the next connection, tracked. A connection accepted once the forced
// close began is closed at once.
func (self *frontTrackedListener) Accept() (net.Conn, error) {
	for {
		conn, err := self.Listener.Accept()
		if err != nil {
			return nil, err
		}
		trackedConn, ok := self.frontServer.trackTcpConn(conn)
		if ok {
			return trackedConn, nil
		}
		conn.Close()
	}
}

// One accepted public tcp connection, the raw socket below any TLS. It leaves
// the tracked set on its first close, whoever closes it: net/http, a hijacker
// or the forced close.
//
// Once a write fails on its deadline, every later write fails at once with
// that error, whatever deadline is set after it (LBALT-IMPL.md §7). The
// stream is cut mid-record then, and nginx sends nothing more on a
// connection whose send_timeout passed. The later write that matters is
// TLS's: when the nginx front's send_timeout cuts a blocked h1 write, or the
// h2 server's write byte timeout, which is send_timeout, cuts a blocked
// frame write, net/http closes the tls.Conn, which first sends close_notify
// under a write deadline of its own, 5 s. A client that stopped reading
// would hold the handler and its limit slot that much longer, and on h2 the
// connection's serving goroutine, which makes that close.
//
// A write its owner left without a deadline is bounded by send_timeout,
// hijacked or not (LBALT-IMPL.md §7): it fails once none of it has gone out
// for sendTimeout, as nginx's send_timeout bounds every blocked client write,
// an upgraded connection's included (ngx_http_upstream_process_upgraded). The
// writes it bounds are crypto/tls's own, made on the reading goroutine under
// its write lock, such as the answer to a client's KeyUpdate request, and
// net/http's few answers of its own, such as its 431. net/http clears the
// write deadline after each request, and gorilla's websocket upgrade and
// connect's H1+ one after their 101, while the read deadlines on an idle
// keepalive connection, a request's head or a websocket's auth frame never
// reach a goroutine blocked in a write. A TLS 1.3 client that sends KeyUpdate
// requests and stops reading would otherwise hold the connection, its
// goroutine and its buffers for good, and a websocket's limit slot before it
// authenticates, where nginx's timers close it (ngx_ssl_handle_recv leaves a
// blocked write to the read timer). Once the bounded write fails, the latch
// fails every later one, and the reader's error or deadline ends the
// connection. The bound lasts only as long as its write, so a connection idle
// between writes has none, and an owner's deadline set meanwhile takes over
// the write and stands: a websocket or tunnel that sets its own deadlines
// keeps them.
//
// It passes net/http's half-close to the socket (CloseWrite), which
// embedding net.Conn would hide. Safe for concurrent use as a net.Conn is.
type frontTrackedConn struct {
	net.Conn
	frontServer *frontServer
	// Front.Http.SendTimeout, the bound of a write left without a deadline.
	// Non-positive leaves such a write unbounded.
	sendTimeout time.Duration
	closeOnce   sync.Once
	// the error of the first write that failed on its deadline, once one has
	writeTimeoutErr atomic.Pointer[error]

	// the socket's write deadline is set only under stateLock, so it never
	// disagrees with the owner's and the bound's: a call out under the lock,
	// as SetWriteDeadline never blocks or calls back
	stateLock sync.Mutex
	// the write deadline the connection's owner set last, zero for none
	ownerWriteDeadline time.Time
	// writes in flight under the bound, and the deadline the latest was given
	boundWriteCount    int
	boundWriteDeadline time.Time
}

// Writes p, or, after a write has failed on its deadline, fails at once.
func (self *frontTrackedConn) Write(p []byte) (int, error) {
	if writeTimeoutErr := self.writeTimeoutErr.Load(); writeTimeoutErr != nil {
		return 0, *writeTimeoutErr
	}
	n := 0
	for {
		// bounds the write when its owner left it without a deadline
		bounded := func() bool {
			if self.sendTimeout <= 0 {
				return false
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if !self.ownerWriteDeadline.IsZero() {
				return false
			}
			self.boundWriteCount += 1
			self.boundWriteDeadline = time.Now().Add(self.sendTimeout)
			self.Conn.SetWriteDeadline(self.boundWriteDeadline)
			return true
		}()
		written, err := self.Conn.Write(p[n:])
		n += written
		if bounded {
			// the socket's deadline is the owner's again once no bounded
			// write is in flight: none, unless the owner set one meanwhile
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.boundWriteCount -= 1
				if self.boundWriteCount == 0 && self.ownerWriteDeadline.IsZero() {
					self.Conn.SetWriteDeadline(time.Time{})
				}
			}()
		}
		if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			// a bounded write that made progress gets a new bound, as
			// send_timeout bounds only a write that makes none
			if bounded && 0 < written {
				continue
			}
			self.writeTimeoutErr.CompareAndSwap(nil, &err)
		}
		return n, err
	}
}

// Records the owner's deadline and gives it to the socket, except that a
// zero one leaves a bounded write in flight its bound.
func (self *frontTrackedConn) SetWriteDeadline(deadline time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ownerWriteDeadline = deadline
	if deadline.IsZero() && 0 < self.boundWriteCount {
		return self.Conn.SetWriteDeadline(self.boundWriteDeadline)
	}
	return self.Conn.SetWriteDeadline(deadline)
}

// The read deadline, and the write deadline as SetWriteDeadline records it.
// net/http's hijack clears both through it.
func (self *frontTrackedConn) SetDeadline(deadline time.Time) error {
	if err := self.Conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	return self.SetWriteDeadline(deadline)
}

// Closes the socket's write side, when it has one. net/http's h1 server
// half-closes a connection it ends with the request's body unread, then waits
// 500 ms before the close (closeWriteAndWait), so a client still sending
// reads the answer and the FIN before the close resets the connection; on
// port 80 this is that FIN. Under TLS nothing calls it: crypto/tls's own
// CloseWrite is a close_notify.
func (self *frontTrackedConn) CloseWrite() error {
	closeWriter, ok := self.Conn.(interface{ CloseWrite() error })
	if !ok {
		return nil
	}
	return closeWriter.CloseWrite()
}

// Closes the socket and leaves the tracked set, once.
func (self *frontTrackedConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(func() {
		self.frontServer.untrackTcpConn(self)
	})
	return err
}

// One public server's error log. net/http writes one message per Write, and
// its messages quote client addresses, so each is counted by kind and none is
// logged, except a handler panic, logged without the address.
type frontHttpErrorLog struct {
	portKind string
	// FrontServerSettings.httpErrorObserverForTest
	observer func(portKind string, kind string, logged string)
}

// Counts one message by its kind, logging only a panic, scrubbed.
func (self *frontHttpErrorLog) Write(message []byte) (int, error) {
	text := string(message)
	kind := frontHttpErrorOther
	switch {
	case strings.Contains(text, "TLS handshake error"):
		kind = frontHttpErrorTlsHandshake
	case strings.Contains(text, "panic serving"):
		kind = frontHttpErrorPanic
	case strings.Contains(text, "Accept error"):
		kind = frontHttpErrorAccept
	}
	frontHttpServerErrorsCounter.WithLabelValues(self.portKind, kind).Inc()
	logged := ""
	if kind == frontHttpErrorPanic {
		logged = scrubFrontHttpPanic(text)
		glog.Errorf("[front]%s handler panic: %s\n", self.portKind, logged)
	}
	if self.observer != nil {
		self.observer(self.portKind, kind, logged)
	}
	return len(message), nil
}

// A net/http panic message without the client address it quotes: the
// "http: panic serving <address>: " prefix becomes "http: panic serving: ".
func scrubFrontHttpPanic(text string) string {
	const panicPrefix = "panic serving "
	before, after, found := strings.Cut(text, panicPrefix)
	if !found {
		return text
	}
	_, rest, found := strings.Cut(after, ": ")
	if !found {
		return before + "panic serving"
	}
	return before + "panic serving: " + rest
}
