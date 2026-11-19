package connect

// The front's udp sockets: QUIC on listen.quic_port, and whodis, the same
// QUIC inside dns messages, on listen.whodis_port (LBALT-IMPL.md §2.8). This
// is alt's dispatch (alt/alt.go), moved into connect:
//
//   - each socket has its own quic.Transport from connect's handler, so one
//     unvalidated-handshake limit stands behind every socket of the block
//     (§4.4.4), and listens with the nginx front's one QUIC TLS config and
//     connect's QUIC config with early data refused (D9);
//   - the bound socket is read directly: no PROXY reader, which would trust a
//     header from any new source (§4.4.1). Whodis decodes it with decode53
//     under the route table's tlds;
//   - each accepted connection is dispatched by its server name: a connect
//     name, once the handshake proves the source's address, under the nginx
//     front's per-address connection cap (0x1001), to connect's handler with
//     the socket's carrier; an api name to an h3 server of the front's
//     H3ApiHandler; anything else closed with 0x1000.
//
// Each api connection gets an http3.Server of its own. The drain shuts each
// down gracefully while its transport can still send, which sends one GOAWAY
// and refuses new streams; every close then closes the servers before the
// transport. A graceful http3 shutdown still running when its transport is
// destroyed spins its accept loop, one GOAWAY writer per pass, for as long as a
// connection is closed but not done: the old alt's exit burst of 700k
// goroutines (LBALT.md §3.3, D12). With one server per connection the bounded
// close of one stuck handler holds nothing else, and no connection waits on
// another's server.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/nginx"
)

// The application errors a public QUIC connection is refused with: alt's,
// outside the http3 range, so an http3 client reports a refusal as an
// application error rather than a protocol violation.
const (
	// the name is not one the front serves on QUIC: only connect and api
	// names are (D3)
	frontQuicErrorCodeUnknownServerName quic.ApplicationErrorCode = 0x1000
	// the address is at its connection cap
	frontQuicErrorCodeRateLimited quic.ApplicationErrorCode = 0x1001
)

// One bound public udp socket and the QUIC it serves.
type frontQuicRun struct {
	frontServer *frontServer
	socket      *frontSocket
	packetConn  net.PacketConn
	// whodis's decode53 translation over packetConn, or nil
	translation net.PacketConn
	// the translation's lifetime, which ends after the transport's
	translationCancel context.CancelFunc
	quicTransport     *quic.Transport
	listener          *quic.EarlyListener

	stateLock sync.Mutex
	// the h3 servers of the api connections being served
	apiServers map[*http3.Server]bool
	// set once the drain began
	drainCtx context.Context
	closing  bool
}

// Binds one udp socket and listens for QUIC on it.
func (self *frontServer) openQuicSocket(socket *frontSocket) (*frontQuicRun, error) {
	listenPacket := self.settings.ListenPacket
	if listenPacket == nil {
		listenPacket = listenFrontPacket
	}
	packetConn, err := listenPacket(socket.key.network(), socket.key.bindAddress())
	if err != nil {
		// a non-nil result is the front's to close, even with an error
		if packetConn != nil {
			packetConn.Close()
		}
		return nil, err
	}
	if packetConn == nil {
		return nil, errors.New("the listen returned no socket")
	}

	// the translation outlives the transport, so it can't end the transport
	// under live connections: only close ends it
	translationCtx, translationCancel := context.WithCancel(context.Background())
	servedConn := packetConn
	var translation net.PacketConn
	if socket.key.portKind == frontPortWhodis {
		ptSettings := connect.DefaultPacketTranslationSettings()
		ptSettings.DnsTlds = self.whodisTlds
		translation, err = connect.NewPacketTranslation(
			translationCtx,
			connect.PacketTranslationModeDecode53,
			packetConn,
			ptSettings,
		)
		if err != nil {
			translationCancel()
			packetConn.Close()
			return nil, err
		}
		servedConn = translation
	}
	quicTransport := self.connectQuic.NewQuicTransport(servedConn)
	listener, err := quicTransport.ListenEarly(self.quicTlsConfig, self.quicConfig)
	if err != nil {
		quicTransport.Close()
		translationCancel()
		if translation != nil {
			translation.Close()
		}
		packetConn.Close()
		return nil, err
	}
	return &frontQuicRun{
		frontServer:       self,
		socket:            socket,
		packetConn:        packetConn,
		translation:       translation,
		translationCancel: translationCancel,
		quicTransport:     quicTransport,
		listener:          listener,
		apiServers:        map[*http3.Server]bool{},
	}, nil
}

// Accepts connections until the socket is lost or the front stops, each
// dispatched on its own goroutine.
func (self *frontQuicRun) serve() error {
	for {
		conn, err := self.listener.Accept(self.frontServer.ctx)
		if err != nil {
			return err
		}
		go server.HandleError(func() {
			self.dispatch(conn)
		})
	}
}

// The carrier connect's resident sees for a connection of this socket
// (MOVE.md §5.5).
func (self *frontQuicRun) carrier() ResidentCarrier {
	if self.socket.key.portKind == frontPortWhodis {
		return residentCarrierForQuicListener(connectListenerTransportH3Dns)
	}
	return residentCarrierForQuicListener(connectListenerTransportH3)
}

// Routes one accepted connection by the name it presented. Returns when the
// connection is finished.
//
// Every connection shares connect's receive window budget through the
// public QUIC config. Connect's handler returns what a connection it served
// was granted; every other connection, api or refused, returns its own here,
// once closed, so no later increase can be charged to it.
func (self *frontQuicRun) dispatch(conn *quic.Conn) {
	servedByConnect := false
	defer func() {
		if !servedByConnect {
			self.frontServer.connectQuic.releaseQuicWindow(conn)
		}
	}()
	serverName := self.serverName(conn)
	nameClass := self.frontServer.front.ClassifyName(serverName)
	if self.frontServer.settings.dispatchObserverForTest != nil {
		self.frontServer.settings.dispatchObserverForTest(serverName, nameClass)
	}
	switch nameClass {
	case nginx.NameClassConnect:
		servedByConnect = self.serveConnect(conn)
	case nginx.NameClassApi:
		self.serveApi(conn)
	default:
		self.refuseUnknownName(conn)
	}
	// every path above closed the connection already, and a second close is
	// a no-op: the window is returned only once nothing can draw on it
	conn.CloseWithError(0, "")
}

// The name the client presented. An early connection is accepted once its
// ClientHello is processed, so the name is normally known; when it is not,
// the completed handshake decides it.
func (self *frontQuicRun) serverName(conn *quic.Conn) string {
	if serverName := conn.ConnectionState().TLS.ServerName; serverName != "" {
		return serverName
	}
	select {
	case <-conn.HandshakeComplete():
	case <-conn.Context().Done():
		return ""
	case <-self.frontServer.ctx.Done():
		return ""
	}
	return conn.ConnectionState().TLS.ServerName
}

// Closes a connection whose name the front doesn't serve on QUIC with 0x1000,
// once its handshake completes: an application close the client reads before
// the server's handshake flight arrives without its code (RFC 9000 §10.2.3),
// and the code is what tells a client this refusal from a failure.
func (self *frontQuicRun) refuseUnknownName(conn *quic.Conn) {
	self.countConnection(frontQuicResultUnknownName)
	if err := AwaitQuicHandshake(self.frontServer.ctx, conn); err != nil {
		conn.CloseWithError(0, "")
		return
	}
	conn.CloseWithError(frontQuicErrorCodeUnknownServerName, "unknown server name")
}

// Admits one connect connection under the per-address connection cap and
// hands it to connect's handler, which applies its own connection rate
// limit. The cap is charged only once the handshake proves the source receives
// packets at its address, so a spoofed source can't take a victim's slots.
// Reports whether connect's handler served the connection, and so owns
// returning its window.
func (self *frontQuicRun) serveConnect(conn *quic.Conn) bool {
	if err := AwaitQuicHandshake(self.frontServer.ctx, conn); err != nil {
		self.countConnection(frontQuicResultHandshakeFailed)
		conn.CloseWithError(0, "")
		return false
	}
	addrPort, err := server.ParseClientAddress(conn.RemoteAddr().String())
	if err != nil {
		self.countConnection(frontQuicResultRateLimited)
		conn.CloseWithError(frontQuicErrorCodeRateLimited, "unknown client address")
		return false
	}
	release, ok := self.frontServer.front.AcquireQuicConnection(self.socket.key.interfaceName, addrPort.Addr().Unmap())
	if !ok {
		self.countConnection(frontQuicResultRateLimited)
		conn.CloseWithError(frontQuicErrorCodeRateLimited, "too many connections")
		return false
	}
	defer release()
	self.countConnection(frontQuicResultConnect)
	if !self.frontServer.connectQuic.HandleQuicConn(conn, self.carrier()) {
		if glog.V(1) {
			glog.Infof("[front]connect closing, refused a %s connection on %s\n", self.socket.key.portKind, self.socket.key.interfaceName)
		}
		return false
	}
	return true
}

// Returns everything one connection was granted from the handler's receive
// window budget. The caller runs it once the connection is closed, so no
// later increase can be charged to it. A nil budget, the default, has nothing
// to return.
func (self *ConnectHandler) releaseQuicWindow(conn *quic.Conn) {
	self.quicWindowBudget.release(conn)
}

// Serves one api connection as h3, once its handshake completes: a request
// sent in 0-RTT could otherwise arrive before the source's address is proved.
// The public config refuses early data anyway (D9). The nginx front's handler
// applies the limits per request and admits only api authorities (D3).
func (self *frontQuicRun) serveApi(conn *quic.Conn) {
	// the http3 server leaves a connection it served open, so the front closes
	// what it accepted
	defer conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
	if err := AwaitQuicHandshake(self.frontServer.ctx, conn); err != nil {
		self.countConnection(frontQuicResultHandshakeFailed)
		return
	}
	apiServer := self.frontServer.newApiServer(self.socket.apiHandler)
	if refusal := self.addApiServer(apiServer); refusal != "" {
		self.countConnection(refusal)
		return
	}
	defer self.removeApiServer(apiServer)
	self.countConnection(frontQuicResultApi)
	if err := apiServer.ServeQUICConn(conn); err != nil {
		if glog.V(1) {
			glog.Infof("[front]api connection on %s/%s exited: %s\n", self.socket.key.interfaceName, self.socket.key.portKind, err)
		}
	}
}

// The h3 server of one api connection, with nginx's client limits: the
// keepalive timeout as the h3 idle timeout and nginx's header buffers.
func (self *frontServer) newApiServer(handler http.Handler) *http3.Server {
	httpSettings := self.settings.Front.Http
	return &http3.Server{
		Handler:        handler,
		IdleTimeout:    httpSettings.KeepaliveTimeout,
		MaxHeaderBytes: httpSettings.MaxRequestHeadersTotalBytes,
	}
}

// Registers an api connection's server, or names the result that refuses
// it: closing, or draining, when the front stopped admitting new HTTP work.
func (self *frontQuicRun) addApiServer(apiServer *http3.Server) string {
	refusal := func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing {
			return frontQuicResultClosing
		}
		if self.drainCtx != nil {
			return frontQuicResultDraining
		}
		self.apiServers[apiServer] = true
		return ""
	}()
	if refusal == "" {
		self.frontServer.openCount.Update(func(openCount int) int {
			return openCount + 1
		})
	}
	return refusal
}

// Removes a finished api connection's server.
func (self *frontQuicRun) removeApiServer(apiServer *http3.Server) {
	removed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.apiServers[apiServer] {
			return false
		}
		delete(self.apiServers, apiServer)
		return true
	}()
	if removed {
		self.frontServer.openCount.Update(func(openCount int) int {
			return openCount - 1
		})
	}
}

// Shuts each api connection down gracefully while the transport can still
// send: one GOAWAY per connection, new streams refused, the requests in
// flight finishing (D12). Each shutdown ends when its client closes the
// connection, its idle timeout passes, or drainCtx ends.
func (self *frontQuicRun) beginDrain(drainCtx context.Context) {
	apiServers := func() []*http3.Server {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing || self.drainCtx != nil {
			return nil
		}
		self.drainCtx = drainCtx
		apiServers := make([]*http3.Server, 0, len(self.apiServers))
		for apiServer := range self.apiServers {
			apiServers = append(apiServers, apiServer)
		}
		return apiServers
	}()
	for _, apiServer := range apiServers {
		go server.HandleError(func() {
			apiServer.Shutdown(drainCtx)
		})
	}
}

// Releases the socket in the drain's order: no new connections; every api
// connection's server closed, bounded by ForcedCloseTimeout, so none is left
// shutting down gracefully; then the transport, which destroys what is left;
// then the translation and the socket.
func (self *frontQuicRun) close() {
	apiServers := func() []*http3.Server {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closing = true
		apiServers := make([]*http3.Server, 0, len(self.apiServers))
		for apiServer := range self.apiServers {
			apiServers = append(apiServers, apiServer)
		}
		return apiServers
	}()
	self.listener.Close()
	closeFrontApiServers(apiServers, self.frontServer.settings.Front.Http.ForcedCloseTimeout)
	self.quicTransport.Close()
	self.translationCancel()
	if self.translation != nil {
		self.translation.Close()
	}
	self.packetConn.Close()
}

// Closes api connections' servers together and waits for them at most
// timeout. Close cancels each server's requests and stops its accept loop at
// once, then waits for the handlers; a handler that ignores its canceled
// request holds the wait only until timeout, past which the process goes on.
// Non-positive doesn't wait, and then a server still shutting down gracefully
// can meet its destroyed transport.
func closeFrontApiServers(apiServers []*http3.Server, timeout time.Duration) {
	if len(apiServers) == 0 {
		return
	}
	var closing sync.WaitGroup
	for _, apiServer := range apiServers {
		closing.Add(1)
		go server.HandleError(func() {
			defer closing.Done()
			apiServer.Close()
		})
	}
	if timeout <= 0 {
		return
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		closing.Wait()
	}()
	select {
	case <-closed:
	case <-time.After(timeout):
		glog.Infof("[front]%d api connections did not close within %s\n", len(apiServers), timeout)
	}
}

// Counts one accepted connection's result.
func (self *frontQuicRun) countConnection(result string) {
	frontQuicConnectionsCounter.WithLabelValues(self.socket.key.portKind, result).Inc()
}
