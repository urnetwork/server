package connect

// connect's lb_alt front: connect serving the edge interfaces' public ports
// itself, in place of the lb's nginx and of alt (LBALT.md §4, LBALT-IMPL.md
// §2.8). server/nginx holds the nginx compatibility code: the route table,
// certificates, routing, forwarding and limits. This file and its siblings
// (front_*.go) hold what connect keeps (LBALT.md §4.5):
//
//   - the sockets. For every route table interface and present family, the
//     front binds listen.http_port and listen.https_port over tcp and
//     listen.quic_port and listen.whodis_port over udp, each SocketsPerPort
//     times with SO_REUSEPORT, on the interface's exact bridge address, never
//     a wildcard (D10, D11). No PROXY reader ever wraps a public socket (§4.4).
//   - the dispatch. TCP 443 terminates TLS with the nginx front's one
//     long-lived config and serves HTTP/1.1 and h2 through its HttpsHandler;
//     TCP 80 serves its HttpHandler. QUIC dispatches each connection by its
//     server name: connect names to connect's own handler, api names to an
//     h3 server of the front's H3ApiHandler, anything else closed with
//     0x1000; the per-address connection cap closes with 0x1001, and one
//     handshake admission limit stands behind every socket (§4.4.4). Whodis
//     is the same dispatch under decode53, with the route table's tlds.
//   - supervision (D13, D22). Each socket is supervised on its own: a lost one
//     is re-created at once, the backoff resetting after a healthy run, it
//     reads as missing until it is back, and its siblings never notice.
//     Losing a socket never ends the process.
//   - readiness (front_status.go) and the drain (front_drain.go, D12).
//
// The trusted listeners on the services bridge, the private H1 with /status,
// the PROXY-reading UDP 443 and 4053 and the exchange's sockets, are connect's
// own and stay for the block's lifetime (§4.6); the front never touches them.
//
// Lifecycle: newFrontServer builds the socket inventory from the route table,
// so readiness can report the expected set before anything binds. start binds
// and listens on every socket, failing on the first error, then supervises
// them. beginDrain, waitDrained and close end it, in drainConnect's order.
//
// Diagnostics carry no client address and no lb route prefix: refusals and
// server errors are counted, and verbose logs name only interfaces, ports and
// results.
//
// Goroutine safety: a frontServer's methods are safe for concurrent use,
// except that start runs at most once.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/nginx"
)

// The nginx.Front methods connect calls (LBALT-IMPL.md §2.7). *nginx.Front
// satisfies it, and tests use a fake.
type nginxFront interface {
	ClassifyName(name string) nginx.NameClass
	TlsConfig() *tls.Config
	QuicTlsConfig() *tls.Config
	HttpsHandler(interfaceName string) http.Handler
	HttpHandler(interfaceName string) http.Handler
	H3ApiHandler(interfaceName string) http.Handler
	AcquireQuicConnection(interfaceName string, addr netip.Addr) (release func(), ok bool)
	ConfigureServer(httpServer *http.Server)
	BeginDrain()
	Close() error
}

var _ nginxFront = (*nginx.Front)(nil)

// What the front needs of connect's handler for QUIC. *ConnectHandler
// satisfies it, so the front shares its admission, its handshake limit and its
// receive window budget, and tests use a fake.
type frontConnectQuic interface {
	// serves one connection whose name is connect's, on the caller's goroutine
	HandleQuicConn(conn *quic.Conn, carrier ResidentCarrier) bool
	// a transport counted against the handler's one unvalidated-handshake
	// limit
	NewQuicTransport(conn net.PacketConn) *quic.Transport
	// a new copy of the handler's server QUIC config, with its shared receive
	// window budget
	NewQuicConfig() *quic.Config
	// returns a closed connection's grants from that budget, for a
	// connection the handler did not serve
	releaseQuicWindow(conn *quic.Conn)
}

var _ frontConnectQuic = (*ConnectHandler)(nil)

// The kinds of public port (LBALT-IMPL.md §2.3), as metric labels name them.
const (
	// tcp listen.http_port, behind public tcp 80
	frontPortHttp = "http"
	// tcp listen.https_port, behind public tcp 443
	frontPortHttps = "https"
	// udp listen.quic_port, behind public udp 443
	frontPortQuic = "quic"
	// udp listen.whodis_port, behind public udp 4053 and 53
	frontPortWhodis = "whodis"
)

// Protocols and families of public sockets, as metric labels and the
// readiness header name them.
const (
	frontProtocolTcp = "tcp"
	frontProtocolUdp = "udp"
	frontFamilyIpv4  = "ipv4"
	frontFamilyIpv6  = "ipv6"
)

// One public socket's place in the inventory, fixed for the front's life.
type frontSocketKey struct {
	interfaceName string
	family        string
	protocol      string
	portKind      string
	address       netip.Addr
	port          int
	// the socket's place among its group's SocketsPerPort
	index int
}

// The network a bind of the socket names, so each family binds its own
// socket and never a dual-stack one.
func (self frontSocketKey) network() string {
	if self.family == frontFamilyIpv6 {
		return self.protocol + "6"
	}
	return self.protocol + "4"
}

// The exact address and port the socket binds.
func (self frontSocketKey) bindAddress() string {
	return netip.AddrPortFrom(self.address, uint16(self.port)).String()
}

// The socket's group as X-UR-Front-Sockets names it: protocol/address/port.
func (self frontSocketKey) groupName() string {
	return fmt.Sprintf("%s/%s/%d", self.protocol, self.address, self.port)
}

// The bounded metric labels: interface, family, protocol and port kind.
func (self frontSocketKey) labels() []string {
	return []string{self.interfaceName, self.family, self.protocol, self.portKind}
}

// The socket in logs: interface, family, protocol, port kind and index, with
// no address.
func (self frontSocketKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s#%d", self.interfaceName, self.family, self.protocol, self.portKind, self.index)
}

// One public socket. The handlers are set once by start; up and run are the
// socket's state, guarded by the front server's stateLock.
type frontSocket struct {
	key frontSocketKey
	// the interface's server for the socket's tcp port, or nil
	httpServer *http.Server
	// the interface's h3 api handler, for a udp socket
	apiHandler http.Handler

	up  bool
	run frontSocketRun
}

// connect's lb_alt front server: the public sockets and their dispatch,
// supervision, readiness and drain.
type frontServer struct {
	generation        string
	revision          string
	certificateExpiry time.Time
	routeTable        *nginx.RouteTable
	certificateStore  *nginx.CertificateStore
	settings          *FrontServerSettings
	// the route table's whodis tlds, as decode53 reads them
	whodisTlds [][]byte
	// every expected socket, in route table order, fixed at construction
	sockets []*frontSocket

	// set once by start, before started is published under stateLock
	ctx           context.Context
	cancel        context.CancelFunc
	front         nginxFront
	connectQuic   frontConnectQuic
	tlsConfig     *tls.Config
	quicTlsConfig *tls.Config
	quicConfig    *quic.Config
	httpServers   []*http.Server

	// closed when the drain begins; supervisors stop re-creating sockets
	drainStarted chan struct{}
	// open tcp connections and h3 api connections: what the drain waits for
	openCount *connect.MonitorValue[int]
	// sockets up, for the readiness header and the tests that wait on it
	upCount *connect.MonitorValue[int]

	supervisors sync.WaitGroup
	closeOnce   sync.Once

	stateLock   sync.Mutex
	started     bool
	draining    bool
	closing     bool
	drainCtx    context.Context
	drainCancel context.CancelFunc
	tcpConns    map[*frontTrackedConn]bool
}

// Builds the front server for a loaded route table, with every expected
// socket registered and none bound.
func newFrontServer(load *frontLoad, settings *FrontServerSettings) (*frontServer, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	sockets, err := newFrontSockets(load.routeTable, settings.SocketsPerPort)
	if err != nil {
		return nil, err
	}
	whodisTlds := [][]byte{}
	for _, whodisTld := range load.routeTable.Front.WhodisTlds {
		whodisTlds = append(whodisTlds, []byte(whodisTld))
	}
	return &frontServer{
		generation:        load.generation,
		revision:          load.revision,
		certificateExpiry: load.certificateExpiry,
		routeTable:        load.routeTable,
		certificateStore:  load.certificateStore,
		settings:          settings,
		whodisTlds:        whodisTlds,
		sockets:           sockets,
		drainStarted:      make(chan struct{}),
		openCount:         connect.NewMonitorValue(0),
		upCount:           connect.NewMonitorValue(0),
		tcpConns:          map[*frontTrackedConn]bool{},
	}, nil
}

// Every expected socket of the route table: for each interface and present
// family, each public port, socketsPerPort times.
func newFrontSockets(routeTable *nginx.RouteTable, socketsPerPort int) ([]*frontSocket, error) {
	ports := []struct {
		portKind string
		protocol string
		port     int
	}{
		{portKind: frontPortHttp, protocol: frontProtocolTcp, port: routeTable.Listen.HttpPort},
		{portKind: frontPortHttps, protocol: frontProtocolTcp, port: routeTable.Listen.HttpsPort},
		{portKind: frontPortQuic, protocol: frontProtocolUdp, port: routeTable.Listen.QuicPort},
		{portKind: frontPortWhodis, protocol: frontProtocolUdp, port: routeTable.Listen.WhodisPort},
	}
	sockets := []*frontSocket{}
	for _, routeInterface := range routeTable.Interfaces {
		families := []struct {
			family  string
			address string
		}{
			{family: frontFamilyIpv4, address: routeInterface.Ipv4},
			{family: frontFamilyIpv6, address: routeInterface.Ipv6},
		}
		for _, family := range families {
			if family.address == "" {
				continue
			}
			address, err := netip.ParseAddr(family.address)
			if err != nil {
				return nil, fmt.Errorf("interface %s %s: %w", routeInterface.Name, family.family, err)
			}
			for _, port := range ports {
				for index := range socketsPerPort {
					sockets = append(sockets, &frontSocket{
						key: frontSocketKey{
							interfaceName: routeInterface.Name,
							family:        family.family,
							protocol:      port.protocol,
							portKind:      port.portKind,
							address:       address,
							port:          port.port,
							index:         index,
						},
					})
				}
			}
		}
	}
	return sockets, nil
}

// Builds the nginx front around connect's router and starts the server. The
// H1 Connect handler serves connect names in-process; the nginx front applies
// the ingress rewrite and the D8 limits around it. A failure releases what was
// built.
func (self *frontServer) startWithRouter(ctx context.Context, connectRouter *ConnectRouter) error {
	version, err := server.Version()
	if err != nil {
		return err
	}
	front, err := nginx.NewFront(
		ctx,
		self.routeTable,
		self.revision,
		self.certificateStore,
		http.HandlerFunc(connectRouter.Connect),
		version,
		self.settings.Front,
	)
	if err != nil {
		return fmt.Errorf("build the nginx front: %w", err)
	}
	if err := self.start(ctx, front, connectRouter.ConnectHandler()); err != nil {
		front.Close()
		return err
	}
	return nil
}

// Binds and listens on every public socket, then serves each under its own
// supervisor until ctx ends or close. Every socket is up when it returns, so
// the first answer of the private /status already shows the complete set; a
// socket that can't be bound fails the start, with every socket bound so far
// released. Runs at most once.
func (self *frontServer) start(ctx context.Context, front nginxFront, connectQuic frontConnectQuic) error {
	cancelCtx, cancel := context.WithCancel(ctx)
	self.ctx = cancelCtx
	self.cancel = cancel
	self.front = front
	self.connectQuic = connectQuic
	// one long-lived config of each kind for every socket, never cloned, so
	// session tickets resume across the block's sockets (D6c)
	self.tlsConfig = front.TlsConfig()
	self.quicTlsConfig = front.QuicTlsConfig()
	// connect's own QUIC config, with its shared receive window budget, but
	// refusing early data: Allow0RTT is a separate control from waiting for
	// the handshake, and connect's config allows it (D9, §4.4)
	quicConfig := connectQuic.NewQuicConfig()
	quicConfig.Allow0RTT = false
	self.quicConfig = quicConfig

	interfaceNameHttpsServers := map[string]*http.Server{}
	interfaceNameHttpServers := map[string]*http.Server{}
	interfaceNameApiHandlers := map[string]http.Handler{}
	for _, routeInterface := range self.routeTable.Interfaces {
		httpsServer := self.newHttpServer(front.HttpsHandler(routeInterface.Name), frontPortHttps)
		httpServer := self.newHttpServer(front.HttpHandler(routeInterface.Name), frontPortHttp)
		interfaceNameHttpsServers[routeInterface.Name] = httpsServer
		interfaceNameHttpServers[routeInterface.Name] = httpServer
		interfaceNameApiHandlers[routeInterface.Name] = front.H3ApiHandler(routeInterface.Name)
		self.httpServers = append(self.httpServers, httpsServer, httpServer)
	}
	for _, socket := range self.sockets {
		switch socket.key.portKind {
		case frontPortHttps:
			socket.httpServer = interfaceNameHttpsServers[socket.key.interfaceName]
		case frontPortHttp:
			socket.httpServer = interfaceNameHttpServers[socket.key.interfaceName]
		default:
			socket.apiHandler = interfaceNameApiHandlers[socket.key.interfaceName]
		}
	}

	runs := make([]frontSocketRun, 0, len(self.sockets))
	for _, socket := range self.sockets {
		run, err := self.openSocket(socket)
		if err != nil {
			for _, run := range runs {
				run.close()
			}
			cancel()
			return fmt.Errorf("bind %s %s: %w", socket.key, socket.key.bindAddress(), err)
		}
		runs = append(runs, run)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for i, socket := range self.sockets {
			self.setSocketRunWithLock(socket, runs[i])
		}
		self.started = true
	}()
	for i, socket := range self.sockets {
		self.supervisors.Add(1)
		go self.superviseSocket(socket, runs[i])
	}
	// the process's cancel without a drain, such as a confirmed handler
	// retirement, still closes the front in its order
	context.AfterFunc(cancelCtx, self.close)
	if glog.V(1) {
		glog.Infof("[front]serving %d public sockets on %d interfaces\n", len(self.sockets), len(self.routeTable.Interfaces))
	}
	return nil
}

// Binds one socket and readies it to serve.
func (self *frontServer) openSocket(socket *frontSocket) (run frontSocketRun, err error) {
	if r := server.HandleError(func() {
		if socket.key.protocol == frontProtocolTcp {
			run, err = self.openTcpSocket(socket)
		} else {
			run, err = self.openQuicSocket(socket)
		}
	}); r != nil {
		return nil, fmt.Errorf("panic: %v", r)
	}
	return run, err
}

// Installs or clears a socket's run; a socket with a run is up.
func (self *frontServer) setSocketRunWithLock(socket *frontSocket, run frontSocketRun) {
	socket.run = run
	up := run != nil
	if socket.up == up {
		return
	}
	socket.up = up
	delta := 1
	if !up {
		delta = -1
	}
	frontSocketsUpGauge.WithLabelValues(socket.key.labels()...).Add(float64(delta))
	self.upCount.Update(func(upCount int) int {
		return upCount + delta
	})
}

// Whether the front has stopped re-creating sockets: closing, draining, or
// its context done.
func (self *frontServer) socketsStopped() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.socketsStoppedWithLock()
}

// As socketsStopped, under stateLock.
func (self *frontServer) socketsStoppedWithLock() bool {
	return self.closing || self.draining || self.ctx.Err() != nil
}

// Whether start completed.
func (self *frontServer) isStarted() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.started
}
