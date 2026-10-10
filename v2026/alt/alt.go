// The alt service (EXTENDER.md L1).
//
// Alt is a full connect node and the api server in one process on the proxy
// hosts, with no load balancer in front. It terminates QUIC on the public udp
// sockets itself, with the real api and connect certificates selected by sni
// and without the proxy protocol header nginx would otherwise prepend, and
// dispatches every accepted connection by the name the client presented: a
// connect name to the connect handler, an api name to the api router mounted
// on an in-process http3 server, any other name refused with an application
// error. The whodis listener is the same dispatch under the decode53
// translation, so whodis reaches both fronts. The H1 websocket stays on the
// lb-fronted connect service.
//
// A client never presents an alt name, so alt loads no certificate of its
// own. Its own limits (L5) are enforced here because there is no nginx in
// front to enforce them.
package alt

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	connectcore "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/router"
)

// The tld the whodis listener decodes. It must be one of the tlds the
// client's packet translation encodes, `PlatformTransportSettings.DnsTlds`.
const DefaultDnsTld = "ur.xyz."

const (
	// the H3 listener on the public udp 443 socket
	ListenerTransportH3 = "h3"
	// the whodis listener on the public udp whodis socket
	ListenerTransportWhodis = "h3dns"
)

// The application error codes a refused connection carries. They are outside
// the http3 range so an http3 client reports them as an application error
// rather than as a protocol violation.
const (
	ErrorCodeUnknownServerName quic.ApplicationErrorCode = 0x1000
	ErrorCodeRateLimited       quic.ApplicationErrorCode = 0x1001
)

// The complete set of application protocols alt serves. The api front is
// http3; the connect transport offers no protocol at all, which is why the
// advertised list is negotiated per ClientHello (see tlsConfigForClient).
var NextProtos = []string{http3.NextProtoH3}

// The alt front's configuration. The connect handler and the exchange share
// one settings snapshot, as they do in the lb-fronted connect service.
type Settings struct {
	ExchangeSettings *connectserver.ExchangeSettings
	LimitsSettings   *LimitsSettings
	// the sni names dispatched to each front. They must not overlap.
	ApiHosts     []string
	ConnectHosts []string
	// the tlds the whodis listener decodes
	DnsTlds []string
	// how long a drain waits for the two fronts to finish
	DrainTimeout time.Duration
}

func DefaultSettings() *Settings {
	exchangeSettings := connectserver.DefaultExchangeSettings()
	// alt binds the public udp sockets itself and dispatches by sni, so the
	// connect handler owns no listener of its own
	exchangeSettings.ListenH3Port = 0
	exchangeSettings.ListenDnsPort = 0
	exchangeSettings.ListenDnsCompatibilityPorts = nil
	// there is no nginx in front of alt to prepend a proxy protocol header
	exchangeSettings.EnableProxyProtocol = false
	return &Settings{
		ExchangeSettings: exchangeSettings,
		LimitsSettings:   DefaultLimitsSettings(),
		DnsTlds:          []string{DefaultDnsTld},
		DrainTimeout:     30 * time.Second,
	}
}

// The sni dispatch over one or more bound udp sockets. Safe for concurrent
// use. The connect handler and the api handler are supplied already
// constructed: alt owns neither their lifecycle nor their state, only the
// connections it hands them.
type Alt struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *Settings

	apiHosts     *HostSet
	connectHosts *HostSet

	transportTls   *server.TransportTls
	quicConfig     *quic.Config
	connectHandler *connectserver.ConnectHandler
	apiServer      *http3.Server
	limits         *Limits

	listenerStateLock sync.RWMutex
	listenerStates    map[altListenerKey]bool

	// Nil outside package tests. Records the front one accepted connection
	// reached, after its name is known and before the front is entered.
	dispatchObserverForTest func(serverName string, front string)
}

// One registered udp socket. The address distinguishes the two families of
// one service port, which are separate listeners.
type altListenerKey struct {
	transport string
	address   string
}

func (self altListenerKey) String() string {
	return fmt.Sprintf("%s/%s", self.transport, self.address)
}

func NewAlt(
	ctx context.Context,
	connectHandler *connectserver.ConnectHandler,
	apiHandler http.Handler,
	settings *Settings,
) (*Alt, error) {
	apiHosts := NewHostSet(settings.ApiHosts)
	connectHosts := NewHostSet(settings.ConnectHosts)
	if err := validateDisjointHosts(apiHosts, connectHosts); err != nil {
		return nil, err
	}
	if len(settings.DnsTlds) == 0 {
		return nil, fmt.Errorf("alt has no dns tlds")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	// one limiter behind both fronts: it already counts an address's api
	// requests and connect connections separately, which is how the two nginx
	// zones it replaces count them
	limits := NewLimits(cancelCtx, settings.LimitsSettings)
	return &Alt{
		ctx:          cancelCtx,
		cancel:       cancel,
		settings:     settings,
		apiHosts:     apiHosts,
		connectHosts: connectHosts,
		// the handler already loaded the api and connect certificates for
		// its own settings; one instance keeps one certificate cache and one
		// allowed-host policy behind both fronts
		transportTls:   connectHandler.TransportTls(),
		quicConfig:     connectHandler.NewQuicConfig(),
		connectHandler: connectHandler,
		apiServer: &http3.Server{
			Handler: NewLimitedHandler(apiHandler, limits),
		},
		limits:         limits,
		listenerStates: map[altListenerKey]bool{},
	}, nil
}

// One name must reach exactly one front. Overlapping lists would make the
// dispatch depend on the order of the checks rather than on the name.
func validateDisjointHosts(apiHosts *HostSet, connectHosts *HostSet) error {
	for _, name := range apiHosts.Names() {
		if connectHosts.Contains(name) {
			return fmt.Errorf("%s is both an api and a connect name", name)
		}
	}
	for _, name := range connectHosts.Names() {
		if apiHosts.Contains(name) {
			return fmt.Errorf("%s is both a connect and an api name", name)
		}
	}
	// a presented name never matches a wildcard entry, so two equal wildcards
	// have to be compared directly
	for _, suffix := range apiHosts.wildcardSuffixes {
		if slices.Contains(connectHosts.wildcardSuffixes, suffix) {
			return fmt.Errorf("*.%s is both an api and a connect name", suffix)
		}
	}
	return nil
}

// AddListener registers one already bound udp socket and returns the function
// that serves it until the context is canceled or the listener fails.
// Registration is synchronous, so readiness reports the complete listener set
// from the moment the sockets are bound, before any of them accepts.
//
// The transport decides how the socket is read: the H3 listener carries QUIC
// directly, and the whodis listener carries it inside dns messages, which the
// decode53 translation unwraps before the same sni dispatch, so whodis reaches
// the api as well as connect.
func (self *Alt) AddListener(transport string, packetConn net.PacketConn) func() error {
	key := altListenerKey{
		transport: transport,
		address:   packetConn.LocalAddr().String(),
	}
	transform := func(packetConn net.PacketConn) (net.PacketConn, error) {
		return packetConn, nil
	}
	if transport == ListenerTransportWhodis {
		transform = func(packetConn net.PacketConn) (net.PacketConn, error) {
			ptSettings := connectcore.DefaultPacketTranslationSettings()
			ptSettings.DnsTlds = [][]byte{}
			for _, dnsTld := range self.settings.DnsTlds {
				ptSettings.DnsTlds = append(ptSettings.DnsTlds, []byte(dnsTld))
			}
			return connectcore.NewPacketTranslation(
				self.ctx,
				connectcore.PacketTranslationModeDecode53,
				packetConn,
				ptSettings,
			)
		}
	}
	self.registerListener(key)
	return func() error {
		return self.listenQuic(key, packetConn, transform)
	}
}

// Registers and serves one H3 socket in one call.
func (self *Alt) ListenH3(packetConn net.PacketConn) error {
	return self.AddListener(ListenerTransportH3, packetConn)()
}

// Registers and serves one whodis socket in one call.
func (self *Alt) ListenWhodis(packetConn net.PacketConn) error {
	return self.AddListener(ListenerTransportWhodis, packetConn)()
}

func (self *Alt) registerListener(key altListenerKey) {
	self.listenerStateLock.Lock()
	defer self.listenerStateLock.Unlock()
	self.listenerStates[key] = false
}

func (self *Alt) setListenerUp(key altListenerKey, up bool) {
	self.listenerStateLock.Lock()
	defer self.listenerStateLock.Unlock()
	if _, registered := self.listenerStates[key]; registered {
		self.listenerStates[key] = up
	}
}

// ListenerReady reports whether every registered socket is accepting. Warp
// must not activate a replacement alt before both udp fronts are up, and a
// listener that exits makes this block unready again. A process with no
// listener at all is never ready.
func (self *Alt) ListenerReady() error {
	self.listenerStateLock.RLock()
	defer self.listenerStateLock.RUnlock()
	if len(self.listenerStates) == 0 {
		return fmt.Errorf("no QUIC listener")
	}
	down := make([]string, 0, len(self.listenerStates))
	for key, up := range self.listenerStates {
		if !up {
			down = append(down, key.String())
		}
	}
	if len(down) == 0 {
		return nil
	}
	slices.Sort(down)
	return fmt.Errorf("QUIC listener down: %s", strings.Join(down, ", "))
}

// Status is the warp deploy poll's readiness surface. Alt serves no other
// route over H1: its api and connect fronts are the udp listeners below.
func (self *Alt) Status(w http.ResponseWriter, r *http.Request) {
	if err := self.ListenerReady(); err != nil {
		http.Error(w, fmt.Sprintf("not ready: %s", err), http.StatusServiceUnavailable)
		return
	}
	router.WarpStatus(w, r)
}

func (self *Alt) listenQuic(
	key altListenerKey,
	packetConn net.PacketConn,
	transform func(net.PacketConn) (net.PacketConn, error),
) error {
	listenCtx, listenCancel := context.WithCancel(self.ctx)
	defer listenCancel()

	transport := key.transport
	listenAddress := key.address
	transformed, err := transform(packetConn)
	if err != nil {
		return fmt.Errorf("alt %s transform %s: %w", transport, listenAddress, err)
	}
	defer transformed.Close()

	quicTransport := &quic.Transport{
		Conn: transformed,
	}
	defer quicTransport.Close()
	listener, err := quicTransport.ListenEarly(
		&tls.Config{
			GetConfigForClient: self.tlsConfigForClient,
		},
		self.quicConfig,
	)
	if err != nil {
		return fmt.Errorf("alt %s listen %s: %w", transport, listenAddress, err)
	}
	defer listener.Close()
	self.setListenerUp(key, true)
	defer self.setListenerUp(key, false)
	glog.Infof("[alt]%s listener up address=%s\n", transport, listenAddress)
	defer glog.Infof("[alt]%s listener down address=%s\n", transport, listenAddress)

	for {
		conn, err := listener.Accept(listenCtx)
		if err != nil {
			if listenCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("alt %s accept %s: %w", transport, listenAddress, err)
		}
		go server.HandleError(func() {
			self.handleQuicConn(conn)
		})
	}
}

// The certificate and application protocol for one ClientHello.
//
// The certificate is the existing api or connect certificate for the
// presented name; alt has none of its own. Alt speaks h3 on the api front and
// the connect transport's raw QUIC on the connect front, and that transport
// offers no application protocol at all. RFC 9001 makes a QUIC server with
// any advertised protocol reject a client that offers none, so the advertised
// list is the intersection with this client's offer: a client offering h3
// negotiates h3, and a client offering nothing negotiates nothing, exactly as
// it does against the lb-fronted connect listener today.
func (self *Alt) tlsConfigForClient(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
	tlsConfig, err := self.transportTls.GetTlsConfigForClient(clientHello)
	if err != nil {
		return nil, err
	}
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = negotiableNextProtos(clientHello.SupportedProtos)
	return tlsConfig, nil
}

func negotiableNextProtos(offeredNextProtos []string) []string {
	nextProtos := []string{}
	for _, nextProto := range NextProtos {
		if slices.Contains(offeredNextProtos, nextProto) {
			nextProtos = append(nextProtos, nextProto)
		}
	}
	if len(nextProtos) == 0 {
		return nil
	}
	return nextProtos
}

// The fronts one accepted connection can reach.
const (
	frontApi     = "api"
	frontConnect = "connect"
	frontRefused = "refused"
)

// Routes one accepted connection to its front. Runs on its own goroutine and
// returns when the connection is finished.
func (self *Alt) handleQuicConn(conn *quic.Conn) {
	serverName := self.serverName(conn)
	front := frontRefused
	switch {
	case self.connectHosts.Contains(serverName):
		front = frontConnect
	case self.apiHosts.Contains(serverName):
		front = frontApi
	}
	if self.dispatchObserverForTest != nil {
		self.dispatchObserverForTest(serverName, front)
	}
	switch front {
	case frontConnect:
		self.handleConnectQuicConn(conn)
	case frontApi:
		self.handleApiQuicConn(conn)
	default:
		glog.Infof("[alt]refuse server name %q from %s\n", serverName, conn.RemoteAddr())
		conn.CloseWithError(ErrorCodeUnknownServerName, "unknown server name")
	}
}

// The name the client presented. An early connection is accepted as soon as
// its ClientHello is processed, so the name is normally already known; when
// it is not, the completed handshake decides it.
func (self *Alt) serverName(conn *quic.Conn) string {
	if serverName := conn.ConnectionState().TLS.ServerName; serverName != "" {
		return serverName
	}
	select {
	case <-conn.HandshakeComplete():
	case <-conn.Context().Done():
		return ""
	case <-self.ctx.Done():
		return ""
	}
	return conn.ConnectionState().TLS.ServerName
}

// Admits one connection against the per-address concurrent connection cap and
// hands it to the connect handler, which applies the exchange's own
// connection rate limit. An over-cap connection is refused before any stream
// is served.
func (self *Alt) handleConnectQuicConn(conn *quic.Conn) {
	addrPort, err := server.ParseClientAddress(conn.RemoteAddr().String())
	if err != nil {
		glog.Infof("[alt]connect address err = %s\n", err)
		conn.CloseWithError(ErrorCodeRateLimited, "unknown client address")
		return
	}
	release, ok := self.limits.AcquireConnection(addrPort.Addr(), time.Now())
	if !ok {
		if glog.V(1) {
			glog.Infof("[alt]connect connection limit %s\n", conn.RemoteAddr())
		}
		conn.CloseWithError(ErrorCodeRateLimited, "too many connections")
		return
	}
	defer release()
	if !self.connectHandler.HandleQuicConn(conn) {
		glog.Infof("[alt]connect closing, refused %s\n", conn.RemoteAddr())
	}
}

// Serves one connection as http3. The per-address request bucket and
// concurrent request cap are applied per request by the wrapped handler,
// which is where nginx applies them for the lb-fronted api.
func (self *Alt) handleApiQuicConn(conn *quic.Conn) {
	// the http3 server leaves the connection open when its own loop exits,
	// so this front closes what it accepted, as the connect front does
	defer conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
	if err := self.apiServer.ServeQUICConn(conn); err != nil {
		if glog.V(1) {
			glog.Infof("[alt]api connection exited %s err = %s\n", conn.RemoteAddr(), err)
		}
	}
}

// Stops accepting and releases both fronts, then waits for the connections
// they still own, bounded by the drain timeout. The exchange drain runs
// before this so residents migrate first.
func (self *Alt) Close() {
	self.cancel()
	drainTimeout := self.settings.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 30 * time.Second
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()
	if err := self.apiServer.Shutdown(drainCtx); err != nil {
		glog.Infof("[alt]api shutdown err = %s\n", err)
	}
	self.connectHandler.Close()
	if !self.connectHandler.WaitForIdle(drainCtx) {
		glog.Infof("[alt]connect did not finish within the drain timeout\n")
	}
}
