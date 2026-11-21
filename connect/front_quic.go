package connect

// The front's udp sockets: QUIC on listen.quic_port, and whodis, the same
// QUIC inside dns messages, on listen.whodis_port (LBALT-IMPL.md §2.8, §8).
// This is alt's dispatch (alt/alt.go), moved into connect:
//
//   - each group of SocketsPerPort sockets, one interface address and udp
//     port, feeds one quic.Transport from connect's handler (D22 (b),
//     LBALT-IMPL.md §8). The quic port's sockets feed it through a
//     quic.ConnGroup, which reads and writes each socket through quic-go's
//     own oob path, with batched reads, ECN, the DF bit and GSO. The whodis
//     port's feed one decode53 translation, with the route table's tlds,
//     through the whodis fan-in (front_whodis.go). So a flow the kernel
//     moves to another socket of its group, when a socket is lost or
//     re-created or a client's NAT rebinds, still reaches its connection;
//   - one unvalidated-handshake limit stands behind every transport of the
//     block (§4.4.4), and each listens with the nginx front's one QUIC TLS
//     config and connect's QUIC config with early data refused (D9);
//   - the bound sockets are read directly: no PROXY reader, which would trust
//     a header from any new source (§4.4.1);
//   - each accepted connection is dispatched by its server name: a connect
//     name to connect's handler with the transport's carrier, an api name to
//     an h3 server of the front's H3ApiHandler, each once the handshake
//     proves the source's address and under the nginx front's per-address
//     connection cap of its class, api or connect (0x1001); any other
//     served name closed with 0x1000. A name the front doesn't serve, or
//     none, never reaches the dispatch: the nginx front's QUIC TLS config
//     has no certificate for it, so its handshake fails, as alt's did, and
//     quic-go never accepts the connection. That config counts every
//     ClientHello by the class of its name
//     (urnetwork_front_quic_client_hellos_total);
//   - the dispatch counts each accepted connection's handshake once: a
//     completed one by QUIC version and resumption, and a failed one as
//     handshake_failed with its reason, read from quic-go's typed errors
//     (frontQuicHandshakeReason).
//
// A socket's run is its place in its group (frontQuicSocketRun), supervised
// like any socket (front_socket.go): a lost socket leaves its group, costing
// no connection, and its re-creation joins the group's current transport.
// Each group's transport (frontQuicRun) has a supervisor of its own, which
// replaces a transport that ends while the front serves, and otherwise closes
// it in the drain's order.
//
// An api connection is bounded before its first request reaches the nginx
// front's limits: the cap bounds how many one address holds, and the h3
// server resets a request stream whose HEADERS don't arrive within the
// client header timeout, reading them into a buffer that grows with what
// arrives (quic-go's URNETWORK.md, ../quic-go). Without the cap, an
// address could hold any number of connections; without the timeout, each
// could hold up to a hundred streams that never send their headers, for as
// long as its client acks the public config's PINGs, which keep the QUIC idle
// timeout from ever firing.
//
// Each api connection gets an http3.Server of its own. The drain shuts each
// down gracefully while its transport can still send, which sends one GOAWAY
// and refuses new streams; every close then closes the servers before the
// transport, and the transport before the group's sockets. Upstream http3's
// graceful accept loop spins, one GOAWAY writer per pass, for as long as a
// connection is closed but not done, whether its client closed it, its idle
// timeout did or its transport was destroyed: the old alt's exit burst of
// 700k goroutines (LBALT.md §3.3, D12). URnetwork's http3 ends the loop
// there instead (quic-go's URNETWORK.md), so each api connection
// gets one GOAWAY however it closes. With one server per connection the
// bounded close of one stuck handler holds nothing else, and no connection
// waits on another's server.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	quic "github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/http3"

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

// One udp group: the SocketsPerPort sockets of one interface address and udp
// port kind, and the transport they feed.
type frontQuicGroup struct {
	frontServer *frontServer
	// its first socket's key: the group's interface, family, port kind,
	// address and port
	key frontSocketKey
	// the interface's h3 api handler, set by start
	apiHandler http.Handler

	// the transport the group's sockets join, guarded by the front server's
	// stateLock: set by start, and replaced when it is lost
	run *frontQuicRun
}

// The group in logs: interface, family, protocol and port kind, with no
// address.
func (self *frontQuicGroup) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", self.key.interfaceName, self.key.family, self.key.protocol, self.key.portKind)
}

// The group's transport its sockets join now, or nil.
func (self *frontQuicGroup) currentRun() *frontQuicRun {
	self.frontServer.stateLock.Lock()
	defer self.frontServer.stateLock.Unlock()
	return self.run
}

// Adds a bound socket to the group's current transport.
func (self *frontQuicGroup) join(packetConn net.PacketConn) (frontQuicGroupMember, error) {
	run := self.currentRun()
	if run == nil {
		return nil, errors.New("the socket's group has no transport")
	}
	return run.join(packetConn)
}

// One socket's place in its group's transport: a *quic.ConnGroupMember on the
// quic port, a *frontWhodisFanInMember on the whodis port.
type frontQuicGroupMember interface {
	// closed once the socket is no longer read: its read failed, or it or
	// its group's fan-in was closed
	Done() <-chan struct{}
	// the read error that ended the socket, nil for one that was closed
	Err() error
	// takes the socket out of its group and closes it
	Close() error
}

// One incarnation of a group's transport: the group's fan-in, whodis's
// translation over it, the quic.Transport and its listener, and the h3
// servers of its api connections.
type frontQuicRun struct {
	frontServer *frontServer
	group       *frontQuicGroup
	// the quic port's fan-in, or nil
	connGroup *quic.ConnGroup
	// the whodis port's fan-in, and the decode53 translation over it, or nil
	whodisFanIn *frontWhodisFanIn
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

// Opens a group's transport, with no socket yet: each socket joins it once
// bound.
func (self *frontServer) openQuicRun(group *frontQuicGroup) (*frontQuicRun, error) {
	localAddr := net.UDPAddrFromAddrPort(netip.AddrPortFrom(group.key.address, uint16(group.key.port)))
	run := &frontQuicRun{
		frontServer: self,
		group:       group,
		apiServers:  map[*http3.Server]bool{},
	}
	var servedConn net.PacketConn
	if group.key.portKind == frontPortWhodis {
		run.whodisFanIn = newFrontWhodisFanIn(localAddr, self.settings)
		// the translation outlives the transport, so it can't end the
		// transport under live connections: only close ends it
		translationCtx, translationCancel := context.WithCancel(context.Background())
		ptSettings := connect.DefaultPacketTranslationSettings()
		ptSettings.DnsTlds = self.whodisTlds
		translation, err := connect.NewPacketTranslation(
			translationCtx,
			connect.PacketTranslationModeDecode53,
			run.whodisFanIn,
			ptSettings,
		)
		if err != nil {
			translationCancel()
			run.whodisFanIn.Close()
			return nil, err
		}
		run.translation = translation
		run.translationCancel = translationCancel
		servedConn = translation
	} else {
		run.connGroup = quic.NewConnGroup(localAddr)
		servedConn = run.connGroup
	}
	run.quicTransport = self.connectQuic.NewQuicTransport(servedConn)
	listener, err := run.quicTransport.ListenEarly(self.quicTlsConfig, self.quicConfig)
	if err != nil {
		run.quicTransport.Close()
		run.closeFanIn()
		return nil, err
	}
	run.listener = listener
	return run, nil
}

// Adds a bound socket to the transport's fan-in.
func (self *frontQuicRun) join(packetConn net.PacketConn) (frontQuicGroupMember, error) {
	if self.whodisFanIn != nil {
		member, err := self.whodisFanIn.add(packetConn)
		if err != nil {
			return nil, err
		}
		return member, nil
	}
	member, err := self.connGroup.Add(packetConn)
	if err != nil {
		return nil, err
	}
	return member, nil
}

// One bound public udp socket, read into its group's transport.
type frontQuicSocketRun struct {
	member frontQuicGroupMember
}

// Binds one udp socket and adds it to its group's transport.
func (self *frontServer) openQuicSocket(socket *frontSocket) (*frontQuicSocketRun, error) {
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
	member, err := socket.quicGroup.join(packetConn)
	if err != nil {
		packetConn.Close()
		return nil, err
	}
	return &frontQuicSocketRun{
		member: member,
	}, nil
}

// Waits until the socket is no longer read: its reads failed, or its group's
// transport closed and closed it. The connections its flows carried are its
// transport's, not the socket's.
func (self *frontQuicSocketRun) serve() error {
	<-self.member.Done()
	return self.member.Err()
}

// The drain is the group's transport's (frontQuicRun.beginDrain).
func (self *frontQuicSocketRun) beginDrain(drainCtx context.Context) {
}

// Takes the socket out of its group and closes it.
func (self *frontQuicSocketRun) close() {
	self.member.Close()
}

// Accepts connections until the transport ends or the front stops, each
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

// The carrier connect's resident sees for a connection of this transport
// (MOVE.md §5.5).
func (self *frontQuicRun) carrier() ResidentCarrier {
	if self.group.key.portKind == frontPortWhodis {
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
	if !self.awaitHandshake(conn) {
		return
	}
	self.countConnection(frontQuicResultUnknownName)
	conn.CloseWithError(frontQuicErrorCodeUnknownServerName, "unknown server name")
}

// Waits for an accepted connection's handshake (AwaitQuicHandshake) and counts
// it: a completed one by QUIC version and whether it resumed a session, and a
// failed one as handshake_failed, by its reason, closing the connection.
// Reports whether the handshake completed.
func (self *frontQuicRun) awaitHandshake(conn *quic.Conn) bool {
	if err := AwaitQuicHandshake(self.frontServer.ctx, conn); err != nil {
		self.countConnectionReason(frontQuicResultHandshakeFailed, frontQuicHandshakeReason(err))
		conn.CloseWithError(0, "")
		return false
	}
	connectionState := conn.ConnectionState()
	version := frontQuicVersionLabel(connectionState.Version)
	resumed := strconv.FormatBool(connectionState.TLS.DidResume)
	frontQuicHandshakesCounter.WithLabelValues(self.group.key.portKind, version, resumed).Inc()
	if observer := self.frontServer.settings.quicHandshakeObserverForTest; observer != nil {
		observer(self.group.key.portKind, version, resumed)
	}
	return true
}

// The TLS alerts the QUIC handshake reasons read (RFC 8446 §6, and IANA's TLS
// Alerts registry). QUIC carries an alert as CRYPTO_ERROR 0x100 plus the alert
// (RFC 9001 §4.8).
const (
	frontTlsAlertBadCertificate               tls.AlertError = 42
	frontTlsAlertUnsupportedCertificate       tls.AlertError = 43
	frontTlsAlertCertificateRevoked           tls.AlertError = 44
	frontTlsAlertCertificateExpired           tls.AlertError = 45
	frontTlsAlertCertificateUnknown           tls.AlertError = 46
	frontTlsAlertUnknownCa                    tls.AlertError = 48
	frontTlsAlertProtocolVersion              tls.AlertError = 70
	frontTlsAlertCertificateUnobtainable      tls.AlertError = 111
	frontTlsAlertBadCertificateStatusResponse tls.AlertError = 113
	frontTlsAlertBadCertificateHashValue      tls.AlertError = 114
	frontTlsAlertCertificateRequired          tls.AlertError = 116
	frontTlsAlertNoApplicationProtocol        tls.AlertError = 120
)

// The reason an accepted connection's handshake failed, from the error
// AwaitQuicHandshake returned: the front's context error, or the connection's
// close cause, as quic-go types it. A CRYPTO_ERROR quic-go marks remote is
// the client's TLS alert, and a local one the front's own: the certificate
// alerts count only as the client's, the alerts about the application
// protocol and the version as either's. The certificate alerts are the ones
// whose text, as crypto/tls words it, names a certificate, the same the TCP
// reasons read (frontTlsHandshakeReason). The front's ErrTlsNoCertificate,
// which quic-go wraps in the CRYPTO_ERROR for internal_error, is
// unknown_name.
func frontQuicHandshakeReason(err error) string {
	var idleTimeoutErr *quic.IdleTimeoutError
	var handshakeTimeoutErr *quic.HandshakeTimeoutError
	var versionNegotiationErr *quic.VersionNegotiationError
	var statelessResetErr *quic.StatelessResetError
	var applicationErr *quic.ApplicationError
	var transportErr *quic.TransportError
	switch {
	case errors.As(err, &idleTimeoutErr):
		return frontQuicHandshakeReasonIdleTimeout
	case errors.As(err, &handshakeTimeoutErr):
		return frontQuicHandshakeReasonHandshakeTimeout
	case errors.Is(err, nginx.ErrTlsNoCertificate):
		return frontQuicHandshakeReasonUnknownName
	case errors.As(err, &versionNegotiationErr):
		return frontQuicHandshakeReasonVersion
	case errors.As(err, &transportErr):
		if transportErr.ErrorCode.IsCryptoError() {
			switch tls.AlertError(transportErr.ErrorCode - 0x100) {
			case frontTlsAlertBadCertificate,
				frontTlsAlertUnsupportedCertificate,
				frontTlsAlertCertificateRevoked,
				frontTlsAlertCertificateExpired,
				frontTlsAlertCertificateUnknown,
				frontTlsAlertUnknownCa,
				frontTlsAlertCertificateUnobtainable,
				frontTlsAlertBadCertificateStatusResponse,
				frontTlsAlertBadCertificateHashValue,
				frontTlsAlertCertificateRequired:
				if transportErr.Remote {
					return frontQuicHandshakeReasonCertificate
				}
			case frontTlsAlertNoApplicationProtocol:
				return frontQuicHandshakeReasonNoAlpn
			case frontTlsAlertProtocolVersion:
				return frontQuicHandshakeReasonVersion
			}
		}
		if transportErr.Remote {
			return frontQuicHandshakeReasonPeerClosed
		}
		return frontQuicHandshakeReasonOther
	case errors.As(err, &applicationErr) && applicationErr.Remote:
		return frontQuicHandshakeReasonPeerClosed
	case errors.As(err, &statelessResetErr):
		return frontQuicHandshakeReasonPeerClosed
	case errors.Is(err, quic.ErrTransportClosed),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return frontQuicHandshakeReasonCanceled
	default:
		return frontQuicHandshakeReasonOther
	}
}

// The version label of a completed QUIC handshake.
func frontQuicVersionLabel(version quic.Version) string {
	switch version {
	case quic.Version1:
		return frontQuicVersionV1
	case quic.Version2:
		return frontQuicVersionV2
	default:
		return frontQuicVersionOther
	}
}

// Admits one connect connection under the per-address cap of connect
// connections, which the address's api connections don't touch (LBALTOPT2
// R15), and hands it to connect's handler, which applies its own connection
// rate limit. The cap is charged only once the handshake proves the source
// receives packets at its address, so a spoofed source can't take a victim's
// slots.
// Reports whether connect's handler served the connection, and so owns
// returning its window.
//
// The connection counts as connect's when it is handed over, so the count
// follows accepts rather than each resident's lifetime. What the handler then
// does with it, serving it or refusing it while it drains or closes, is
// connect's to count.
func (self *frontQuicRun) serveConnect(conn *quic.Conn) bool {
	if !self.awaitHandshake(conn) {
		return false
	}
	addrPort, err := server.ParseClientAddress(conn.RemoteAddr().String())
	if err != nil {
		self.countConnection(frontQuicResultRateLimited)
		conn.CloseWithError(frontQuicErrorCodeRateLimited, "unknown client address")
		return false
	}
	release, ok := self.frontServer.front.AcquireQuicConnection(self.group.key.interfaceName, addrPort.Addr().Unmap(), nginx.NameClassConnect)
	if !ok {
		self.countConnection(frontQuicResultRateLimited)
		conn.CloseWithError(frontQuicErrorCodeRateLimited, "too many connections")
		return false
	}
	defer release()
	self.countConnection(frontQuicResultConnect)
	if !self.frontServer.connectQuic.HandleQuicConn(conn, self.carrier()) {
		if glog.V(1) {
			glog.Infof("[front]connect closing, refused a %s connection on %s\n", self.group.key.portKind, self.group.key.interfaceName)
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
// The public config refuses early data anyway (D9). The connection is charged
// to its address's cap of api connections, apart from its connect
// connections (LBALTOPT2 R15), once the handshake proves the address, so a
// spoofed source can't take a victim's slots. The nginx front's handler
// applies the limits per request and admits only api authorities (D3).
func (self *frontQuicRun) serveApi(conn *quic.Conn) {
	if !self.awaitHandshake(conn) {
		return
	}
	addrPort, err := server.ParseClientAddress(conn.RemoteAddr().String())
	if err != nil {
		self.countConnection(frontQuicResultRateLimited)
		conn.CloseWithError(frontQuicErrorCodeRateLimited, "unknown client address")
		return
	}
	release, ok := self.frontServer.front.AcquireQuicConnection(self.group.key.interfaceName, addrPort.Addr().Unmap(), nginx.NameClassApi)
	if !ok {
		self.countConnection(frontQuicResultRateLimited)
		conn.CloseWithError(frontQuicErrorCodeRateLimited, "too many connections")
		return
	}
	defer release()
	// the http3 server leaves a connection it served open, so the front closes
	// what it accepted, before its slot is let go
	defer conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
	apiServer := self.frontServer.newApiServer(self.group.apiHandler)
	if refusal := self.addApiServer(apiServer); refusal != "" {
		self.countConnection(refusal)
		return
	}
	defer self.removeApiServer(apiServer)
	self.countConnection(frontQuicResultApi)
	if err := apiServer.ServeQUICConn(conn); err != nil {
		if glog.V(1) {
			glog.Infof("[front]api connection on %s/%s exited: %s\n", self.group.key.interfaceName, self.group.key.portKind, err)
		}
	}
}

// The h3 server of one api connection, with nginx's client limits: the
// keepalive timeout as the h3 idle timeout, nginx's header buffers, and the
// client header timeout as the bound on each request stream's HEADERS. The
// public tcp listeners apply the same setting to h1 and h2 request headers.
func (self *frontServer) newApiServer(handler http.Handler) *http3.Server {
	httpSettings := self.settings.Front.Http
	return &http3.Server{
		Handler:           handler,
		IdleTimeout:       httpSettings.KeepaliveTimeout,
		MaxHeaderBytes:    httpSettings.MaxRequestHeadersTotalBytes,
		ReadHeaderTimeout: httpSettings.ClientHeaderTimeout,
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

// Releases the transport in the drain's order: no new connections; every api
// connection's server closed, bounded by ForcedCloseTimeout, so none is left
// shutting down gracefully; then the transport, which destroys what is left;
// then the translation, and last the fan-in, which closes the group's sockets,
// so the servers' closes still leave through them.
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
	self.closeFanIn()
}

// Closes the translation, if any, then the fan-in and the group's sockets.
func (self *frontQuicRun) closeFanIn() {
	if self.translation != nil {
		self.translationCancel()
		self.translation.Close()
	}
	if self.whodisFanIn != nil {
		self.whodisFanIn.Close()
	}
	if self.connGroup != nil {
		self.connGroup.Close()
	}
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

// Counts one accepted connection's result, which has no reason.
func (self *frontQuicRun) countConnection(result string) {
	self.countConnectionReason(result, frontQuicResultReasonNone)
}

// Counts one accepted connection's result and its reason: why the handshake
// failed for handshake_failed, - for every other result; and its result by
// the group's address family.
func (self *frontQuicRun) countConnectionReason(result string, reason string) {
	frontQuicConnectionsCounter.WithLabelValues(self.group.key.portKind, result, reason).Inc()
	frontQuicConnectionsByFamilyCounter.WithLabelValues(self.group.key.portKind, self.group.key.family, result).Inc()
	if observer := self.frontServer.settings.connectionResultObserverForTest; observer != nil {
		observer(self.group.key.portKind, result, reason)
	}
}
