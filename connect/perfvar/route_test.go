// This file composes the real server handler, resident, exchange, platform
// carriers, WebRTC lanes, and optional production extender over PERFVAR links.
package perfvar

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/transport/v4"
	clientconnect "github.com/urnetwork/connect"
	connectextender "github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Database-backed out-of-band control is identical to the server integration path.
type routeOutOfBandControl struct {
	ctx                     context.Context
	clientId                server.Id
	contractManagerSettings *clientconnect.ContractManagerSettings
	lifecycle               *routeAsyncLifecycle
}

// A lifecycle counter exposes an idle channel without adding a goroutine per
// wait. Callers stop admission before waiting, as ConnectHandler does.
type routeAsyncLifecycle struct {
	stateLock  sync.Mutex
	active     int
	activeZero chan struct{}
	closing    bool
}

// A new lifecycle begins idle.
func newRouteAsyncLifecycle() *routeAsyncLifecycle {
	activeZero := make(chan struct{})
	close(activeZero)
	return &routeAsyncLifecycle{activeZero: activeZero}
}

// Starting records one asynchronous owner before its goroutine is launched.
func (self *routeAsyncLifecycle) start() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closing {
		return false
	}
	if self.active == 0 {
		self.activeZero = make(chan struct{})
	}
	self.active += 1
	return true
}

// Completion releases one asynchronous owner after all deferred cleanup.
func (self *routeAsyncLifecycle) done() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.active -= 1
	if self.active < 0 {
		panic("route asynchronous lifecycle became negative")
	}
	if self.active == 0 {
		close(self.activeZero)
	}
}

// Closing stops admission and waits for the current lifecycle generation.
func (self *routeAsyncLifecycle) closeAndWait(ctx context.Context) bool {
	self.stateLock.Lock()
	self.closing = true
	activeZero := self.activeZero
	self.stateLock.Unlock()
	select {
	case <-ctx.Done():
		return false
	case <-activeZero:
		return true
	}
}

// The process-wide pool counter exposes checked-out ownership independently
// of the bounded free lists retained for reuse.
func routeMessagePoolOutstanding() int64 {
	poolTaken, poolReturned, _ := clientconnect.MessagePoolCounts()
	return int64(poolTaken) - int64(poolReturned)
}

// Per-class ownership makes a failed balance gate actionable without enabling
// expensive pool tags for the performance process.
func routeMessagePoolOutstandingClasses() map[int]int64 {
	outstanding := map[int]int64{}
	for _, stats := range clientconnect.GetMessagePoolClassStats() {
		classOutstanding := int64(stats.Taken) - int64(stats.Returned)
		if classOutstanding != 0 {
			outstanding[stats.Size] = classOutstanding
		}
	}
	return outstanding
}

// Pool reconciliation is sampled only after every fixture worker has joined.
// A mismatch fails at that exact lifecycle barrier instead of polling a
// process-global counter until unrelated activity can make it look balanced.
func routeMessagePoolBalance(poolOutstandingBefore int64) (int64, bool) {
	poolOutstandingAfter := routeMessagePoolOutstanding()
	return poolOutstandingAfter, poolOutstandingAfter == poolOutstandingBefore
}

// An outstanding fixture buffer must fail at the first lifecycle barrier; a
// later unrelated return must not be able to satisfy an internal polling loop.
func TestRouteMessagePoolBalanceUsesExactSnapshot(t *testing.T) {
	poolOutstandingBefore := routeMessagePoolOutstanding()
	message := clientconnect.MessagePoolGet(1)
	poolOutstandingAfter, poolBalanced := routeMessagePoolBalance(poolOutstandingBefore)
	if poolBalanced || poolOutstandingAfter != poolOutstandingBefore+1 {
		t.Fatalf(
			"outstanding pool buffer reported (%d,%t), want (%d,false)",
			poolOutstandingAfter,
			poolBalanced,
			poolOutstandingBefore+1,
		)
	}
	if !clientconnect.MessagePoolReturn(message) {
		t.Fatal("exact pool witness was not returned")
	}
	poolOutstandingAfter, poolBalanced = routeMessagePoolBalance(poolOutstandingBefore)
	if !poolBalanced || poolOutstandingAfter != poolOutstandingBefore {
		t.Fatalf(
			"returned pool buffer reported (%d,%t), want (%d,true)",
			poolOutstandingAfter,
			poolBalanced,
			poolOutstandingBefore,
		)
	}
}

// Control ownership is completed asynchronously, matching application clients.
func (self *routeOutOfBandControl) SendControl(
	frames []*protocol.Frame,
	callback clientconnect.OobResultFunction,
) {
	returnFrames := func(returnFrames []*protocol.Frame) {
		for _, frame := range returnFrames {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
		}
	}
	if !self.lifecycle.start() {
		returnFrames(frames)
		if callback != nil {
			callback(nil, context.Canceled)
		}
		return
	}
	go server.HandleError(func() {
		defer self.lifecycle.done()
		defer returnFrames(frames)
		resultFrames, err := controller.ConnectControlFrames(
			self.ctx,
			self.clientId,
			frames,
			self.contractManagerSettings,
		)
		defer returnFrames(resultFrames)
		if callback != nil {
			callback(resultFrames, err)
		}
	})
}

// One user-side node owns its TUN, strategy, client, and platform route.
type routeClient struct {
	clientId        server.Id
	clientJwt       string
	tun             *clientconnect.Tun
	strategy        *clientconnect.ClientStrategy
	client          *clientconnect.Client
	transport       *clientconnect.PlatformTransport
	stats           *clientconnect.P2pDataPlaneStats
	routeStateTrace *p2pRouteStateTrace
}

// One lifecycle bundle lets environment teardown join concrete clients while
// pure tests hold exact transport and client completion barriers.
type routeClientLifecycle struct {
	flush                 func()
	closeTransportAndWait func(context.Context) error
	closeClientAndWait    func(context.Context) error
}

// Concrete route clients expose the same teardown sequence without changing
// their production ownership fields.
func routeClientLifecycles(clients []*routeClient) []routeClientLifecycle {
	lifecycles := make([]routeClientLifecycle, 0, len(clients))
	for _, client := range clients {
		client := client
		lifecycles = append(lifecycles, routeClientLifecycle{
			flush: client.client.Flush,
			closeTransportAndWait: func(ctx context.Context) error {
				if client.transport == nil {
					return nil
				}
				return client.transport.CloseAndWait(ctx)
			},
			closeClientAndWait: client.client.CloseAndWait,
		})
	}
	return lifecycles
}

// Teardown flushes every source before stopping carriers, then joins each
// carrier before its client. Independent clients still close after an error.
func closeRouteClientLifecyclesAndWait(
	ctx context.Context,
	lifecycles []routeClientLifecycle,
) error {
	for _, lifecycle := range lifecycles {
		lifecycle.flush()
	}
	var closeErr error
	for clientIndex, lifecycle := range lifecycles {
		if err := lifecycle.closeTransportAndWait(ctx); err != nil {
			closeErr = errors.Join(
				closeErr,
				fmt.Errorf("join route client %d platform transport: %w", clientIndex, err),
			)
		}
		if err := lifecycle.closeClientAndWait(ctx); err != nil {
			closeErr = errors.Join(
				closeErr,
				fmt.Errorf("join route client %d: %w", clientIndex, err),
			)
		}
	}
	return closeErr
}

// The environment owns one real edge plus all userspace network endpoints.
type routeEnvironment struct {
	t      testing.TB
	ctx    context.Context
	cancel context.CancelFunc

	profile                 networkProfile
	accessProfile           networkProfile
	providerAccessProfile   networkProfile
	deviceAccessProfile     networkProfile
	internalExchangeProfile *networkProfile
	network                 *simulatedIPNetwork
	edgeTun                 *clientconnect.Tun
	edgeAddress             netip.Addr
	h1Port                  int
	h3Port                  int
	apiPort                 int
	deviceEdgeName          string
	providerEdgeName        string
	providerEdgeAddress     netip.Addr
	providerH1Port          int
	providerH3Port          int
	providerApiPort         int

	exchange   *connectserver.Exchange
	handler    *connectserver.ConnectHandler
	httpServer *http.Server
	apiServer  *http.Server

	networkId   server.Id
	userId      server.Id
	userSession *session.ClientSession

	stateLock      sync.Mutex
	nextNode       int
	extenders      []*connectextender.ExtenderServer
	clients        []*routeClient
	extenderErrors chan error
	announces      *routeAsyncLifecycle
	controls       *routeAsyncLifecycle

	poolOutstandingBefore        int64
	poolOutstandingClassesBefore map[int]int64
}

// The fixture uses gVisor TCP and UDP for client-edge traffic and real TLS.
func newRouteEnvironment(
	ctx context.Context,
	t testing.TB,
	profile networkProfile,
) *routeEnvironment {
	return newRouteEnvironmentWithNetworkPeers(ctx, t, profile, true)
}

// Full-TUN exchange fixtures can disable peer discovery to force the platform.
func newRouteEnvironmentWithNetworkPeers(
	ctx context.Context,
	t testing.TB,
	profile networkProfile,
	enableNetworkPeers bool,
) *routeEnvironment {
	return newRouteEnvironmentWithNetworkPeersAfterPoolBaseline(
		ctx,
		t,
		profile,
		enableNetworkPeers,
		nil,
	)
}

// A test callback can create ownership after the exact pre-construction pool
// snapshot without adding shared state or a production timing dependency.
func newRouteEnvironmentWithNetworkPeersAfterPoolBaseline(
	ctx context.Context,
	t testing.TB,
	profile networkProfile,
	enableNetworkPeers bool,
	afterPoolBaseline func(),
) *routeEnvironment {
	poolOutstandingBefore := routeMessagePoolOutstanding()
	poolOutstandingClassesBefore := routeMessagePoolOutstandingClasses()
	if afterPoolBaseline != nil {
		afterPoolBaseline()
	}
	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")
	environmentCtx, cancel := context.WithCancel(ctx)
	network := newSimulatedIPNetwork(environmentCtx)
	edgeSettings := clientconnect.DefaultTunSettingsWithBufferSize(4096)
	edgeSettings.Mtu = profile.InnerMtu
	edgeTun, err := clientconnect.CreateTun(environmentCtx, edgeSettings)
	if err != nil {
		cancel()
		t.Fatalf("create route edge TUN: %v", err)
	}
	if err := network.addTun("edge", edgeTun); err != nil {
		edgeTun.Close()
		cancel()
		t.Fatalf("add route edge TUN: %v", err)
	}
	edgeAddress := edgeTun.LocalAddresses()[0]
	edgeIP := net.IP(edgeAddress.AsSlice())

	exchangeListener, err := edgeTun.ListenTCP(&net.TCPAddr{IP: edgeIP, Port: 0})
	if err != nil {
		network.close()
		cancel()
		t.Fatalf("listen route exchange: %v", err)
	}
	exchangePort := exchangeListener.Addr().(*net.TCPAddr).Port
	host := "perfvar-edge"
	routes := map[string]string{host: edgeAddress.String()}
	exchangeSettings := connectserver.DefaultExchangeSettings()
	exchangeSettings.ExchangeResidentTtl = 10 * time.Second
	exchangeSettings.EnableNetworkPeers = enableNetworkPeers
	exchangeSettings.KeyEventDelivery.Enabled = false
	exchangeSettings.NetworkPeersPollInterval = 100 * time.Millisecond
	exchangeSettings.StreamHopsPollInterval = 100 * time.Millisecond
	exchangeSettings.ConnectionAnnounceTimeout = 0
	exchangeSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	exchangeSettings.ConnectionTestConfig = connectserver.V0TestConfig()
	exchangeSettings.DialContext = edgeTun.DialContext
	exchange := connectserver.NewExchangeWithListeners(
		environmentCtx,
		host,
		"connect",
		"test",
		map[int]int{exchangePort: exchangePort},
		routes,
		exchangeSettings,
		map[int]net.Listener{exchangePort: exchangeListener},
	)

	h1Listener, err := edgeTun.ListenTCP(&net.TCPAddr{IP: edgeIP, Port: 0})
	if err != nil {
		exchange.Close()
		network.close()
		cancel()
		t.Fatalf("listen route H1: %v", err)
	}
	h1Port := h1Listener.Addr().(*net.TCPAddr).Port
	h3PacketConn, err := edgeTun.ListenUDP(&net.UDPAddr{IP: edgeIP, Port: 0})
	if err != nil {
		h1Listener.Close()
		exchange.Close()
		network.close()
		cancel()
		t.Fatalf("listen route H3: %v", err)
	}
	h3Port := h3PacketConn.LocalAddr().(*net.UDPAddr).Port
	apiListener, err := edgeTun.ListenTCP(&net.TCPAddr{IP: edgeIP, Port: 0})
	if err != nil {
		h3PacketConn.Close()
		h1Listener.Close()
		exchange.Close()
		network.close()
		cancel()
		t.Fatalf("listen route API: %v", err)
	}
	apiPort := apiListener.Addr().(*net.TCPAddr).Port
	handlerSettings := connectserver.DefaultConnectHandlerSettings()
	handlerSettings.ListenH3Port = 0
	handlerSettings.ListenDnsPort = 0
	handlerSettings.EnableProxyProtocol = false
	handlerSettings.TransportTlsSettings.EnableSelfSign = true
	handlerSettings.TransportTlsSettings.DefaultHostName = edgeAddress.String()
	handlerSettings.ConnectionAnnounceTimeout = 0
	handlerSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	handlerSettings.ConnectionTestConfig = connectserver.V0TestConfig()
	announces := newRouteAsyncLifecycle()
	handlerSettings.ConnectionAnnounceSettings.LifecycleStarted = func() {
		if !announces.start() {
			panic("route connection announcement started during teardown")
		}
	}
	handlerSettings.ConnectionAnnounceSettings.LifecycleDone = announces.done
	handler := connectserver.NewConnectHandlerWithPacketConns(
		environmentCtx,
		server.NewId(),
		exchange,
		handlerSettings,
		connectserver.ConnectHandlerPacketConns{H3: h3PacketConn},
	)
	httpRoutes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", handler.Connect),
	}
	serverTlsConfig, _, err := newWorkloadTlsConfigs()
	if err != nil {
		handler.Close()
		exchange.Close()
		network.close()
		cancel()
		t.Fatalf("create route TLS: %v", err)
	}
	serverTlsConfig.NextProtos = []string{"http/1.1"}
	httpServer := &http.Server{Handler: router.NewRouter(environmentCtx, httpRoutes)}
	go func() {
		_ = httpServer.Serve(tls.NewListener(h1Listener, serverTlsConfig))
	}()
	apiServer := &http.Server{Handler: router.NewRouter(environmentCtx, api.Routes())}
	go func() {
		_ = apiServer.Serve(apiListener)
	}()

	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(
		environmentCtx,
		networkId,
		fmt.Sprintf("perfvar-%s", networkId),
		userId,
	)
	err = model.AddBasicTransferBalance(
		environmentCtx,
		networkId,
		model.ByteCount(1024*1024*1024*1024),
		server.NowUtc(),
		server.NowUtc().Add(365*24*time.Hour),
	)
	if err != nil {
		apiServer.Close()
		httpServer.Close()
		handler.Close()
		exchange.Close()
		network.close()
		cancel()
		t.Fatalf("fund route network: %v", err)
	}
	userSession := session.Testing_CreateClientSession(environmentCtx, jwt.NewByJwt(
		networkId,
		userId,
		fmt.Sprintf("perfvar-%s", networkId),
		false,
		false,
	))
	return &routeEnvironment{
		t:                            t,
		ctx:                          environmentCtx,
		cancel:                       cancel,
		profile:                      profile,
		accessProfile:                profile,
		providerAccessProfile:        profile,
		deviceAccessProfile:          profile,
		network:                      network,
		edgeTun:                      edgeTun,
		edgeAddress:                  edgeAddress,
		h1Port:                       h1Port,
		h3Port:                       h3Port,
		apiPort:                      apiPort,
		deviceEdgeName:               "edge",
		providerEdgeName:             "edge",
		providerEdgeAddress:          edgeAddress,
		providerH1Port:               h1Port,
		providerH3Port:               h3Port,
		providerApiPort:              apiPort,
		exchange:                     exchange,
		handler:                      handler,
		httpServer:                   httpServer,
		apiServer:                    apiServer,
		networkId:                    networkId,
		userId:                       userId,
		userSession:                  userSession,
		extenderErrors:               make(chan error, 32),
		announces:                    announces,
		controls:                     newRouteAsyncLifecycle(),
		poolOutstandingBefore:        poolOutstandingBefore,
		poolOutstandingClassesBefore: poolOutstandingClassesBefore,
	}
}

// Segment profiles preserve end-to-end constant delay across extender hops.
func dividedRouteProfile(profile networkProfile, segmentCount int, seedOffset int64) networkProfile {
	divided := profile
	divided.Seed += seedOffset
	divided.Forward.BaseDelay /= time.Duration(segmentCount)
	divided.Reverse.BaseDelay /= time.Duration(segmentCount)
	divided.Forward.ProcessingDelay /= time.Duration(segmentCount)
	divided.Reverse.ProcessingDelay /= time.Duration(segmentCount)
	return divided
}

// A fresh client TUN reaches the edge directly or through one real extender.
func (self *routeEnvironment) newClientNode(useExtender bool) (*clientconnect.Tun, *clientconnect.ClientStrategy) {
	return self.newClientNodeWithProfile(useExtender, self.accessProfile)
}

// Full-TUN fixtures can condition the application and provider access paths
// independently while generic carrier fixtures retain one shared profile.
func (self *routeEnvironment) newClientNodeWithProfile(
	useExtender bool,
	accessProfile networkProfile,
) (*clientconnect.Tun, *clientconnect.ClientStrategy) {
	return self.newClientNodeWithProfileAt(
		useExtender,
		accessProfile,
		self.deviceEdgeName,
	)
}

// Split-edge fixtures pin an endpoint to its own logical server while the
// ordinary route keeps both endpoint names on the one production edge.
func (self *routeEnvironment) newClientNodeWithProfileAt(
	useExtender bool,
	accessProfile networkProfile,
	edgeName string,
) (*clientconnect.Tun, *clientconnect.ClientStrategy) {
	if edgeName == "" {
		edgeName = "edge"
	}
	self.stateLock.Lock()
	self.nextNode += 1
	nodeIndex := self.nextNode
	self.stateLock.Unlock()
	name := fmt.Sprintf("client-%d", nodeIndex)
	settings := clientconnect.DefaultTunSettingsWithBufferSize(4096)
	settings.Mtu = accessProfile.InnerMtu
	clientTun, err := clientconnect.CreateTun(self.ctx, settings)
	if err != nil {
		self.t.Fatalf("create %s TUN: %v", name, err)
	}
	if err := self.network.addTun(name, clientTun); err != nil {
		clientTun.Close()
		self.t.Fatalf("add %s TUN: %v", name, err)
	}

	strategySettings := clientconnect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategySettings.ConnectSettings.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}
	strategySettings.ConnectSettings.DialContextSettings = &clientconnect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			connection, dialErr := clientTun.DialContext(ctx, network, address)
			if useExtender {
				self.t.Logf("[perfvar] extender client dial network=%s address=%s err=%v", network, address, dialErr)
			}
			return connection, dialErr
		},
	}

	if !useExtender {
		if _, _, err := self.network.addBidirectionalLink(
			name,
			edgeName,
			dividedRouteProfile(accessProfile, 1, int64(10*nodeIndex)),
		); err != nil {
			self.t.Fatalf("add %s edge link: %v", name, err)
		}
		return clientTun, clientconnect.NewClientStrategy(self.ctx, strategySettings)
	}

	extenderName := fmt.Sprintf("extender-%d", nodeIndex)
	extenderTun, err := clientconnect.CreateTun(self.ctx, settings)
	if err != nil {
		self.t.Fatalf("create %s TUN: %v", extenderName, err)
	}
	if err := self.network.addTun(extenderName, extenderTun); err != nil {
		extenderTun.Close()
		self.t.Fatalf("add %s TUN: %v", extenderName, err)
	}
	segmentProfile := dividedRouteProfile(accessProfile, 2, int64(10*nodeIndex))
	// The API fixture is plain HTTP and therefore cannot use the TLS-only
	// extender protocol. Its direct control link is not used by the forced H1
	// carrier, whose strategy contains only the exact extender dialer.
	if _, _, err := self.network.addBidirectionalLink(name, edgeName, accessProfile); err != nil {
		self.t.Fatalf("add %s API control link: %v", name, err)
	}
	if _, _, err := self.network.addBidirectionalLink(name, extenderName, segmentProfile); err != nil {
		self.t.Fatalf("add client-extender link: %v", err)
	}
	segmentProfile.Seed += 2
	if _, _, err := self.network.addBidirectionalLink(extenderName, edgeName, segmentProfile); err != nil {
		self.t.Fatalf("add extender-edge link: %v", err)
	}
	extenderIP := net.IP(extenderTun.LocalAddresses()[0].AsSlice())
	extenderListener, err := extenderTun.ListenTCP(&net.TCPAddr{IP: extenderIP, Port: 0})
	if err != nil {
		self.t.Fatalf("listen %s: %v", extenderName, err)
	}
	extenderPort := extenderListener.Addr().(*net.TCPAddr).Port
	extenderSettings := connectextender.DefaultExtenderSettings()
	extenderSettings.Listen = func(network string, address string) (net.Listener, error) {
		return extenderListener, nil
	}
	extenderSettings.DialContext = extenderTun.DialContext
	extenderSettings.ErrorHandler = func(stage string, err error) {
		self.t.Logf("[perfvar] extender stage=%s err=%v", stage, err)
		select {
		case self.extenderErrors <- fmt.Errorf("%s: %w", stage, err):
		default:
		}
	}
	secret := fmt.Sprintf("perfvar-secret-%d", nodeIndex)
	extenderServer := connectextender.NewExtenderServer(
		self.ctx,
		[]string{secret},
		[]string{self.edgeAddress.String()},
		map[int][]clientconnect.ExtenderConnectMode{
			extenderPort: {clientconnect.ExtenderConnectModeTcpTls},
		},
		&net.Dialer{},
		extenderSettings,
	)
	self.extenders = append(self.extenders, extenderServer)
	go func() {
		_ = extenderServer.ListenAndServe()
	}()
	strategySettings.EnableNormal = false
	strategySettings.EnableResilient = false
	strategySettings.ExpandExtenderProfileCount = 0
	strategySettings.ExtenderConfigs = []*clientconnect.ExtenderConfig{
		{
			Profile: clientconnect.ExtenderProfile{
				ConnectMode: clientconnect.ExtenderConnectModeTcpTls,
				ServerName:  "perfvar-extender.invalid",
				Port:        extenderPort,
			},
			Ip:     extenderTun.LocalAddresses()[0],
			Secret: secret,
		},
	}
	return clientTun, clientconnect.NewClientStrategy(self.ctx, strategySettings)
}

// Authentication creates the by-client JWT validated by the real handler.
func (self *routeEnvironment) authClient(description string) (server.Id, string) {
	result, err := model.AuthNetworkClient(
		&model.AuthNetworkClientArgs{Description: description},
		self.userSession,
	)
	if err != nil {
		self.t.Fatalf("auth client %q: %v", description, err)
	}
	if result.Error != nil {
		self.t.Fatalf("auth client %q: %s", description, result.Error.Message)
	}
	return *result.ClientId, *result.ByClientJwt
}

// Client settings force the requested P2P lane and inject native Pion vnet.
func (self *routeEnvironment) newClient(
	description string,
	mode clientconnect.P2pDataPlaneMode,
	webRtcNetwork transport.Net,
	useExtender bool,
) *routeClient {
	clientId, clientJwt := self.authClient(description)
	tun, strategy := self.newClientNode(useExtender)
	settings := clientconnect.DefaultClientSettings()
	settings.ControlPingTimeout = max(
		time.Second,
		4*(self.accessProfile.Forward.BaseDelay+self.accessProfile.Reverse.BaseDelay),
	)
	settings.WebRtcSettings.IceServerUrls = nil
	stats := &clientconnect.P2pDataPlaneStats{}
	routeStateTrace := newP2pRouteStateTrace()
	p2pSettings := settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings
	p2pSettings.DataPlaneMode = mode
	p2pSettings.DataPlaneStats = stats
	p2pSettings.RouteStateObserver = routeStateTrace.Observe
	if webRtcNetwork == nil {
		settings.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
	} else {
		settings.WebRtcSettings.Network = webRtcNetwork
	}
	client := clientconnect.NewClient(
		self.ctx,
		clientconnect.Id(clientId),
		&routeOutOfBandControl{
			ctx:                     self.ctx,
			clientId:                clientId,
			contractManagerSettings: settings.ContractManagerSettings,
			lifecycle:               self.controls,
		},
		settings,
	)
	routeClient := &routeClient{
		clientId:        clientId,
		clientJwt:       clientJwt,
		tun:             tun,
		strategy:        strategy,
		client:          client,
		stats:           stats,
		routeStateTrace: routeStateTrace,
	}
	self.clients = append(self.clients, routeClient)
	return routeClient
}

// Platform construction forces H1 or H3 while retaining real auth and TLS.
func (self *routeEnvironment) connectPlatform(
	client *routeClient,
	mode clientconnect.TransportMode,
) {
	settings := clientconnect.DefaultPlatformTransportSettings()
	settings.QuicTlsConfig.InsecureSkipVerify = true
	settings.H3Port = self.h3Port
	settings.DnsPort = 0
	settings.H3PacketConnFactory = func(ctx context.Context) (net.PacketConn, error) {
		return client.tun.ListenUDP(&net.UDPAddr{
			IP:   net.IP(client.tun.LocalAddresses()[0].AsSlice()),
			Port: 0,
		})
	}
	client.transport = clientconnect.NewPlatformTransportWithTargetMode(
		self.ctx,
		client.strategy,
		client.client.RouteManager(),
		fmt.Sprintf("wss://%s:%d", self.edgeAddress, self.h1Port),
		&clientconnect.ClientAuth{
			ByJwt:      client.clientJwt,
			InstanceId: clientconnect.NewId(),
			AppVersion: "perfvar",
		},
		mode,
		settings,
	)
}

// Platform readiness is observed without polling a private route structure.
func waitForPlatform(
	ctx context.Context,
	transport *clientconnect.PlatformTransport,
) bool {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for !transport.IsConnected() {
		notify := transport.ConnectedNotify()
		if transport.IsConnected() {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-notify:
		}
	}
	return true
}

// Extender failures end a route wait with an attributable stage and error.
func waitForPlatformOrExtenderError(
	ctx context.Context,
	transport *clientconnect.PlatformTransport,
	extenderErrors <-chan error,
) error {
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for !transport.IsConnected() {
		notify := transport.ConnectedNotify()
		if transport.IsConnected() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("platform transport timed out")
		case err := <-extenderErrors:
			return err
		case <-notify:
		}
	}
	return nil
}

// Network provide registration is complete only after its controller ack.
func setRouteProvide(
	ctx context.Context,
	client *clientconnect.Client,
) error {
	result := make(chan error, 1)
	client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true},
		func(err error) {
			select {
			case result <- err:
			default:
			}
		},
	)
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-result:
		return err
	case <-timeout.C:
		return fmt.Errorf("provide registration timed out")
	}
}

// A forced stream probe waits for actual peer delivery before measurement.
func waitForP2pRoute(
	ctx context.Context,
	source *routeClient,
	destination *routeClient,
) error {
	received := make(chan struct{}, 1)
	unsub := destination.client.AddReceiveCallback(func(
		path clientconnect.TransferPath,
		frames []*protocol.Frame,
		peer clientconnect.Peer,
	) {
		if path.SourceId != clientconnect.Id(source.clientId) {
			return
		}
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				select {
				case received <- struct{}{}:
				default:
				}
			}
		}
	})
	defer unsub()
	frame, err := clientconnect.ToFrame(
		&protocol.SimpleMessage{Content: "perfvar p2p setup"},
		clientconnect.DefaultProtocolVersion,
	)
	if err != nil {
		return err
	}
	if !source.client.SendWithTimeout(
		frame,
		clientconnect.Id(destination.clientId),
		nil,
		60*time.Second,
		clientconnect.ForceStream(),
	) {
		clientconnect.MessagePoolReturn(frame.MessageBytes)
		return fmt.Errorf("P2P setup send failed")
	}
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-received:
		return nil
	case <-timeout.C:
		return fmt.Errorf("P2P setup receive timed out")
	}
}

// Route count distinguishes completed P2P promotion from exchange delivery.
// The observer records every publication, so readiness does not depend on a
// scheduler tick happening while the desired state remains visible.
func waitForRouteCount(
	ctx context.Context,
	observer *clientconnect.TestingMultiRouteWriterRouteStateObserver,
	routeCount int,
) error {
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	defer waitCancel()
	state := observer.Snapshot()
	if state.ActiveRouteCount == routeCount {
		return nil
	}
	state, err := observer.WaitForActiveRouteCountAfter(waitCtx, state.Generation, routeCount)
	if err != nil {
		return fmt.Errorf(
			"route count=%d generation=%d, expected=%d: %w",
			observer.Snapshot().ActiveRouteCount,
			observer.Snapshot().Generation,
			routeCount,
			err,
		)
	}
	return nil
}

// A caller-captured generation makes a post-action route assertion immune to
// both a stale pre-action match and a fast transition that has since moved on.
func waitForRouteCountAfter(
	ctx context.Context,
	observer *clientconnect.TestingMultiRouteWriterRouteStateObserver,
	barrier clientconnect.TestingMultiRouteWriterRouteState,
	routeCount int,
) (clientconnect.TestingMultiRouteWriterRouteState, error) {
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	defer waitCancel()
	state, err := observer.WaitForActiveRouteCountAfter(
		waitCtx,
		barrier.Generation,
		routeCount,
	)
	if err != nil {
		latest := observer.Snapshot()
		return clientconnect.TestingMultiRouteWriterRouteState{}, fmt.Errorf(
			"route count=%d generation=%d, expected=%d after generation %d: %w",
			latest.ActiveRouteCount,
			latest.Generation,
			routeCount,
			barrier.Generation,
			err,
		)
	}
	return state, nil
}

// Raw route traffic verifies unique indexes and every payload byte.
func measureProductionRoute(
	ctx context.Context,
	source *routeClient,
	destination *routeClient,
	packetCount int,
	transferOptions ...any,
) (workloadResult, error) {
	const payloadByteCount = 1200
	received := make(chan uint64, packetCount)
	var invalidPacketCount atomic.Uint64
	unsub := destination.client.AddReceiveCallback(func(
		path clientconnect.TransferPath,
		frames []*protocol.Frame,
		peer clientconnect.Peer,
	) {
		if path.SourceId != clientconnect.Id(source.clientId) {
			return
		}
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage ||
				len(frame.MessageBytes) != payloadByteCount {
				continue
			}
			sequence := binary.BigEndian.Uint64(frame.MessageBytes[:8])
			valid := sequence < uint64(packetCount)
			for byteIndex := 8; valid && byteIndex < len(frame.MessageBytes); byteIndex += 1 {
				valid = frame.MessageBytes[byteIndex] == byte((int(sequence)+byteIndex)%251)
			}
			if !valid {
				invalidPacketCount.Add(1)
				continue
			}
			select {
			case received <- sequence:
			default:
				invalidPacketCount.Add(1)
			}
		}
	})
	defer unsub()
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	startTime := time.Now()
	for packetIndex := range packetCount {
		packetBytes := clientconnect.MessagePoolGet(payloadByteCount)
		binary.BigEndian.PutUint64(packetBytes[:8], uint64(packetIndex))
		for byteIndex := 8; byteIndex < len(packetBytes); byteIndex += 1 {
			packetBytes[byteIndex] = byte((packetIndex + byteIndex) % 251)
		}
		frame := &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: packetBytes,
			Raw:          true,
		}
		if !source.client.SendWithTimeout(
			frame,
			clientconnect.Id(destination.clientId),
			nil,
			60*time.Second,
			transferOptions...,
		) {
			clientconnect.MessagePoolReturn(packetBytes)
			return workloadResult{}, fmt.Errorf("route send %d/%d failed", packetIndex, packetCount)
		}
	}
	seen := make([]bool, packetCount)
	uniquePacketCount := 0
	duplicatePacketCount := int64(0)
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for uniquePacketCount < packetCount {
		select {
		case <-ctx.Done():
			return workloadResult{}, ctx.Err()
		case <-deadline.C:
			return workloadResult{}, fmt.Errorf("route received %d/%d packets", uniquePacketCount, packetCount)
		case sequence := <-received:
			if seen[sequence] {
				duplicatePacketCount += 1
				continue
			}
			seen[sequence] = true
			uniquePacketCount += 1
		}
	}
	duration := time.Since(startTime)
	if invalidPacketCount.Load() != 0 {
		return workloadResult{}, fmt.Errorf("route invalid packet count=%d", invalidPacketCount.Load())
	}
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	return finishWorkloadResult(workloadResult{
		UsefulByteCount:        int64(packetCount * payloadByteCount),
		DeliveredPacketCount:   int64(uniquePacketCount),
		DuplicatePacketCount:   duplicatePacketCount,
		Duration:               duration,
		AllocatedByteCount:     memoryAfter.TotalAlloc - memoryBefore.TotalAlloc,
		AllocationCount:        memoryAfter.Mallocs - memoryBefore.Mallocs,
		GarbageCollectionCount: memoryAfter.NumGC - memoryBefore.NumGC,
		GarbageCollectionPause: time.Duration(memoryAfter.PauseTotalNs - memoryBefore.PauseTotalNs),
	}), nil
}

// Exchange H1 or H3 retains both client carriers and the resident path.
func measureExchangeRoute(
	t testing.TB,
	environment *routeEnvironment,
	mode clientconnect.TransportMode,
	useExtender bool,
	packetCount int,
) workloadResult {
	if useExtender && mode != clientconnect.TransportModeH1 {
		t.Fatal("the production extender carries TCP/TLS H1, not H3 UDP")
	}
	source := environment.newClient("exchange source", clientconnect.P2pDataPlaneModeAuto, nil, useExtender)
	destination := environment.newClient("exchange destination", clientconnect.P2pDataPlaneModeAuto, nil, useExtender)
	environment.connectPlatform(source, mode)
	environment.connectPlatform(destination, mode)
	if useExtender {
		if err := waitForPlatformOrExtenderError(
			environment.ctx,
			source.transport,
			environment.extenderErrors,
		); err != nil {
			t.Fatalf("exchange source platform transport did not connect: %v", err)
		}
		if err := waitForPlatformOrExtenderError(
			environment.ctx,
			destination.transport,
			environment.extenderErrors,
		); err != nil {
			t.Fatalf("exchange destination platform transport did not connect: %v", err)
		}
	} else if !waitForPlatform(environment.ctx, source.transport) ||
		!waitForPlatform(environment.ctx, destination.transport) {
		t.Fatal("exchange platform transport did not connect")
	}
	if err := setRouteProvide(environment.ctx, source.client); err != nil {
		t.Fatalf("source provide: %v", err)
	}
	if err := setRouteProvide(environment.ctx, destination.client); err != nil {
		t.Fatalf("destination provide: %v", err)
	}
	result, err := measureProductionRoute(environment.ctx, source, destination, packetCount)
	if err != nil {
		t.Fatalf("exchange %s route: %v", mode, err)
	}
	return result
}

// P2P setup uses platform signaling, then closes both fallback carriers.
func measureP2pRoute(
	t testing.TB,
	environment *routeEnvironment,
	mode clientconnect.P2pDataPlaneMode,
	packetCount int,
) workloadResult {
	p2pNetwork, err := newP2pNetwork(environment.profile)
	if err != nil {
		t.Fatalf("create P2P network: %v", err)
	}
	defer p2pNetwork.close()
	source := environment.newClient("p2p source", mode, p2pNetwork.left, false)
	destination := environment.newClient("p2p destination", mode, p2pNetwork.right, false)
	environment.connectPlatform(source, clientconnect.TransportModeH1)
	environment.connectPlatform(destination, clientconnect.TransportModeH1)
	if !waitForPlatform(environment.ctx, source.transport) || !waitForPlatform(environment.ctx, destination.transport) {
		t.Fatal("P2P signaling transport did not connect")
	}
	if err := setRouteProvide(environment.ctx, source.client); err != nil {
		t.Fatalf("source provide: %v", err)
	}
	if err := setRouteProvide(environment.ctx, destination.client); err != nil {
		t.Fatalf("destination provide: %v", err)
	}
	writer := source.client.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.Id(destination.clientId)),
	)
	defer source.client.RouteManager().CloseMultiRouteWriter(writer)
	routeStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
	defer routeStateObserver.Close()
	if err := waitForP2pRoute(environment.ctx, source, destination); err != nil {
		t.Fatal(err)
	}
	if err := waitForRouteCount(environment.ctx, routeStateObserver, 2); err != nil {
		t.Fatalf("wait for P2P promotion: %v", err)
	}
	source.client.ContractManager().AddNoContractPeer(clientconnect.Id(destination.clientId))
	destination.client.ContractManager().AddNoContractPeer(clientconnect.Id(source.clientId))
	forcedRouteBarrier := routeStateObserver.Snapshot()
	if forcedRouteBarrier.ActiveRouteCount != 2 {
		t.Fatalf("forced P2P transition started from route state=%+v", forcedRouteBarrier)
	}
	source.transport.Close()
	destination.transport.Close()
	if _, err := waitForRouteCountAfter(
		environment.ctx,
		routeStateObserver,
		forcedRouteBarrier,
		1,
	); err != nil {
		t.Fatalf("wait for forced P2P route: %v", err)
	}
	result, err := measureProductionRoute(
		environment.ctx,
		source,
		destination,
		packetCount,
		clientconnect.NoAck(),
	)
	if err != nil {
		t.Fatalf("P2P route: %v", err)
	}
	sourceSnapshot := source.stats.Snapshot()
	destinationSnapshot := destination.stats.Snapshot()
	if sourceSnapshot.FastFallbackCount != 0 || destinationSnapshot.FastFallbackCount != 0 {
		t.Fatalf("forced P2P route fell back: source=%+v destination=%+v", sourceSnapshot, destinationSnapshot)
	}
	if mode == clientconnect.P2pDataPlaneModeFastOnly &&
		(sourceSnapshot.FastSendMessageCount == 0 || destinationSnapshot.FastReceiveMessageCount == 0) {
		t.Fatalf("forced fast P2P did not use fast lane: source=%+v destination=%+v", sourceSnapshot, destinationSnapshot)
	}
	if mode == clientconnect.P2pDataPlaneModeLegacyOnly &&
		(sourceSnapshot.LegacySendMessageCount == 0 || destinationSnapshot.LegacyReceiveMessageCount == 0) {
		t.Fatalf("forced legacy P2P did not use legacy lane: source=%+v destination=%+v", sourceSnapshot, destinationSnapshot)
	}
	return result
}

// Close orders client, server, exchange, and network teardown explicitly.
func (self *routeEnvironment) close() {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer closeCancel()
	if err := closeRouteClientLifecyclesAndWait(
		closeCtx,
		routeClientLifecycles(self.clients),
	); err != nil {
		self.t.Errorf("route clients did not close: %v", err)
	}
	// Client close submits its final contract controls. Join them while the
	// fixture context is still live; canceling first turns valid database
	// cleanup into a teardown-only timeout under race instrumentation.
	if !self.controls.closeAndWait(closeCtx) {
		self.t.Errorf("route out-of-band controls did not become idle")
	}
	for _, extenderServer := range self.extenders {
		extenderServer.CloseAndWait()
	}
	self.httpServer.Close()
	self.apiServer.Close()
	self.handler.Close()
	self.exchange.Close()
	self.cancel()
	if !self.handler.WaitForIdle(closeCtx) {
		self.t.Errorf("route handler did not become idle")
	}
	if !self.announces.closeAndWait(closeCtx) {
		self.t.Errorf("route connection announcements did not become idle")
	}
	if !self.exchange.WaitForIdle(closeCtx) {
		self.t.Errorf("route exchange did not become idle")
	}
	self.network.close()
	poolOutstandingAfter, poolBalanced := routeMessagePoolBalance(self.poolOutstandingBefore)
	if !poolBalanced {
		self.t.Errorf(
			"route message-pool ownership did not reconcile: %d -> %d classes=%v -> %v",
			self.poolOutstandingBefore,
			poolOutstandingAfter,
			self.poolOutstandingClassesBefore,
			routeMessagePoolOutstandingClasses(),
		)
	}
}

// Environment teardown cannot reach client close, later owners, or pool
// reconciliation while an earlier platform transport still owns a worker.
// An independent transport error is retained without skipping later joins.
func TestCloseRouteClientLifecyclesWaitsAndContinuesAfterError(t *testing.T) {
	transportEntered := make(chan struct{})
	releaseTransport := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseTransport) })
	firstClientJoined := make(chan struct{})
	secondTransportJoined := make(chan struct{})
	secondClientJoined := make(chan struct{})
	expectedErr := errors.New("held transport close failed")
	var flushCount atomic.Int64
	lifecycles := []routeClientLifecycle{
		{
			flush: func() { flushCount.Add(1) },
			closeTransportAndWait: func(context.Context) error {
				close(transportEntered)
				<-releaseTransport
				return expectedErr
			},
			closeClientAndWait: func(context.Context) error {
				close(firstClientJoined)
				return nil
			},
		},
		{
			flush: func() { flushCount.Add(1) },
			closeTransportAndWait: func(context.Context) error {
				close(secondTransportJoined)
				return nil
			},
			closeClientAndWait: func(context.Context) error {
				close(secondClientJoined)
				return nil
			},
		},
	}
	closeResult := make(chan error, 1)
	go func() {
		closeResult <- closeRouteClientLifecyclesAndWait(t.Context(), lifecycles)
	}()
	waitForPlatformRouteControllerSignal(t, transportEntered, "first route transport did not begin closing")
	if flushCount.Load() != 2 {
		t.Fatalf("route lifecycle flush count=%d, want 2", flushCount.Load())
	}
	select {
	case err := <-closeResult:
		t.Fatalf("route lifecycle returned before held transport release: %v", err)
	case <-firstClientJoined:
		t.Fatal("route client closed before its transport joined")
	case <-secondTransportJoined:
		t.Fatal("later route transport closed before the first transport joined")
	default:
	}
	releaseOnce.Do(func() { close(releaseTransport) })
	var closeErr error
	select {
	case closeErr = <-closeResult:
	case <-time.After(5 * time.Second):
		t.Fatal("route lifecycle did not join after transport release")
	}
	if !errors.Is(closeErr, expectedErr) {
		t.Fatalf("route lifecycle error=%v, want %v", closeErr, expectedErr)
	}
	for name, signal := range map[string]<-chan struct{}{
		"first client":     firstClientJoined,
		"second transport": secondTransportJoined,
		"second client":    secondClientJoined,
	} {
		select {
		case <-signal:
		default:
			t.Errorf("%s was not joined after independent error", name)
		}
	}
}

// The pool baseline precedes fixture construction, and teardown joins database
// cleanup before TestEnv can remove its temporary schema.
func TestRouteEnvironmentConstructionAndTeardownReconcileAsyncOwnership(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3011)["clean-lan"]
		poolOutstandingBeforeConstruction := routeMessagePoolOutstanding()
		var constructionMessage []byte
		defer func() {
			if constructionMessage != nil {
				clientconnect.MessagePoolReturn(constructionMessage)
			}
		}()
		environment := newRouteEnvironmentWithNetworkPeersAfterPoolBaseline(
			ctx,
			t,
			profile,
			false,
			func() {
				constructionMessage = clientconnect.MessagePoolGet(1)
			},
		)
		if environment.poolOutstandingBefore != poolOutstandingBeforeConstruction {
			t.Fatalf(
				"route construction normalized owned buffer into baseline: got=%d want=%d",
				environment.poolOutstandingBefore,
				poolOutstandingBeforeConstruction,
			)
		}
		if constructionMessage == nil {
			t.Fatal("route construction boundary was not observed")
		}
		if !clientconnect.MessagePoolReturn(constructionMessage) {
			t.Fatal("route construction witness was not returned")
		}
		constructionMessage = nil
		closed := false
		defer func() {
			if !closed {
				environment.close()
			}
		}()
		result := measureExchangeRoute(t, environment, clientconnect.TransportModeH1, false, 8)
		if result.DeliveredPacketCount != 8 {
			t.Fatalf("teardown route delivered %d/8 packets", result.DeliveredPacketCount)
		}
		environment.close()
		closed = true
	})
}

// Every forced production carrier has an always-on exact-delivery gate.
func TestRouteForcedCarrierCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(3001)["clean-lan"]
		measure := func(measurement func(*routeEnvironment) workloadResult) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			environment := newRouteEnvironment(ctx, t, profile)
			defer environment.close()
			result := measurement(environment)
			if result.UsefulByteCount == 0 || result.DeliveredPacketCount == 0 {
				t.Fatalf("empty route result: %+v", result)
			}
		}
		measure(func(environment *routeEnvironment) workloadResult {
			return measureExchangeRoute(t, environment, clientconnect.TransportModeH1, false, 32)
		})
		measure(func(environment *routeEnvironment) workloadResult {
			return measureExchangeRoute(t, environment, clientconnect.TransportModeH3, false, 32)
		})
		measure(func(environment *routeEnvironment) workloadResult {
			return measureP2pRoute(t, environment, clientconnect.P2pDataPlaneModeLegacyOnly, 32)
		})
		measure(func(environment *routeEnvironment) workloadResult {
			return measureP2pRoute(t, environment, clientconnect.P2pDataPlaneModeFastOnly, 32)
		})
	})
}

// A production extender remains functional with 500 ms user-to-edge RTT.
func TestRouteSingleRegionExtenderCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3002)["single-region-500ms-rtt"]
		environment := newRouteEnvironment(ctx, t, profile)
		defer environment.close()
		result := measureExchangeRoute(t, environment, clientconnect.TransportModeH1, true, 8)
		if result.UsefulByteCount == 0 {
			t.Fatalf("empty extender result: %+v", result)
		}
	})
}

// A production extender remains functional with 1 s user-to-edge RTT.
func TestRouteOneSecondSingleRegionExtenderCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3004)["single-region-1000ms-rtt"]
		environment := newRouteEnvironment(ctx, t, profile)
		defer environment.close()
		result := measureExchangeRoute(t, environment, clientconnect.TransportModeH1, true, 4)
		if result.UsefulByteCount == 0 {
			t.Fatalf("empty one-second extender result: %+v", result)
		}
	})
}

// A production extender has a low-latency correctness control for attribution.
func TestRouteCleanExtenderCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3003)["clean-lan"]
		environment := newRouteEnvironment(ctx, t, profile)
		defer environment.close()
		result := measureExchangeRoute(t, environment, clientconnect.TransportModeH1, true, 8)
		if result.UsefulByteCount == 0 {
			t.Fatalf("empty clean extender result: %+v", result)
		}
	})
}
