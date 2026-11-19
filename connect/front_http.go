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
// them (D12).
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
	"strings"
	"sync"

	"github.com/urnetwork/glog"
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
// idle timeout; header bytes of twice nginx's header buffers, within which
// the nginx front's handlers apply nginx's own limits; the h2 stream limit;
// and keepalive_requests and keepalive_time per connection. Nothing bounds a
// whole request or response here: bodies, uploads, websockets and streamed
// responses keep the per-read and per-write timeouts the nginx front applies.
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
type frontTrackedConn struct {
	net.Conn
	frontServer *frontServer
	closeOnce   sync.Once
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
