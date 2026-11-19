package connect

// Fixtures of the lb_alt integration tests (LBALT.md §9: carriers, readiness,
// drain and limits). Each test stands connect's lb_alt front up the way
// run.go does, through startWithRouter:
//
//   - the real nginx front, built from a route table on loopback interfaces,
//     with every served name's certificate loaded by nginx.LoadCertificateStore
//     from a synthetic vault in warpctl's findLatestTls layout;
//   - connect's real router and handler, over a real exchange, so a client
//     authenticates and its messages cross a resident;
//   - when a test asks for upstreams, the api's blocks and a websocket
//     service's block on the upstream address, where the front forwards.
//
// Clients are the real code of ../connect: the platform transport's
// WebSocket, H1+, H3 and whodis carriers, and the api's alt h3 and whodis
// dialers. Each is pointed at the front's public sockets the way a client
// reaches an interface: it presents the served name, and only the address it
// dials is the interface's bridge address, standing in for the DNAT. A test
// that checks the address the front sees a client at binds the client's
// sockets to the loopback of the family it dials (testLbAltClientAddress),
// which is not the interface's address where that is 127.0.0.2.
//
// Two layouts, since a darwin host can bind only 127.0.0.1 and ::1 on
// loopback, and the upstream address may be no interface's:
//   - both families: eth1 on 127.0.0.1 and eth2 on ::1 (and on 127.0.0.2
//     where the host can bind it), with the route fixture's TEST-NET upstream,
//     which nothing dials;
//   - upstreams: eth1 on ::1 (and on 127.0.0.2 where the host can bind it),
//     and the upstream on 127.0.0.1, where the api's and the websocket
//     service's blocks listen.
//
// The tests need Postgres and Redis.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/api"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/nginx"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// The bound on one step of an lb_alt test: a dial, an authentication, a
// message's delivery, a barrier. Generous for a loaded machine; a passing test
// never waits it out.
const testLbAltProgressTimeout = 90 * time.Second

// Names the tests present. The connect, api and forwarded names are the
// shared route fixture's (nginx/testdata/route_v1_example.json); the
// websocket service is the fixture's own, added in the upstreams layout.
const (
	testLbAltConnectName   = "connect.service.example"
	testLbAltApiName       = "api.service.example"
	testLbAltForwardedName = "mcp.service.example"
	testLbAltRedirectName  = "old-api.service.example"
	testLbAltWebsocketName = "relay.service.example"
	testLbAltUnknownName   = "other.unknown.example"
	testLbAltWhodisTld     = "whodis.example."
)

// The fixture's websocket service, which the front forwards (D12).
const testLbAltWebsocketService = "relay"

// The version directory of the synthetic vault.
const testLbAltTlsVersion = "2026.1.1"

// The interfaces of the route table.
const (
	testLbAltInterfaceA = "eth1"
	testLbAltInterfaceB = "eth2"
)

// How one lb_alt test env is built.
type testLbAltEnvSettings struct {
	// the upstreams layout: the api's and the websocket service's blocks
	// listen on the upstream address, 127.0.0.1, so no interface may use it
	upstreams bool
	// serves the api's blocks in the upstreams layout; nil serves the real api
	// router, api.Routes()
	apiHandler http.Handler
	// adjusts the route table before it is encoded and parsed again
	routeTable func(routeTable *nginx.RouteTable)
	// adjusts the exchange's settings, its handler's included, before the
	// exchange is built
	exchangeSettings func(settings *ExchangeSettings)
	// adjusts the front server's settings before the start
	frontSettings func(settings *FrontServerSettings)
	// hands the front its udp sockets in wrappers whose writes a test can fail
	// (testLbAltBinds.failable)
	failableUdp bool
}

// One connect block fronting its interfaces, with its exchange and, in the
// upstreams layout, its upstream blocks.
type testLbAltEnv struct {
	t      testing.TB
	ctx    context.Context
	cancel context.CancelFunc

	routeTable *nginx.RouteTable
	revision   string
	// verifies every served name's certificate
	roots *x509.CertPool

	exchange      *Exchange
	connectRouter *ConnectRouter
	warpStatus    *router.WarpStatusState
	settings      *FrontServerSettings
	frontServer   *frontServer
	binds         *testLbAltBinds
	// every accepted QUIC connection's "<port kind> <result>"
	quicResults chan string

	// signaled each time the websocket service's block sees one of its
	// upgraded websockets end
	websocketUpstreamEnds chan struct{}
}

// Builds and starts connect's lb_alt front over a real exchange. Runs inside a
// test env, with Postgres and Redis. Everything closes with the test.
func newTestLbAltEnv(t testing.TB, envSettings *testLbAltEnvSettings) *testLbAltEnv {
	t.Helper()
	if envSettings == nil {
		envSettings = &testLbAltEnvSettings{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	env := &testLbAltEnv{
		t:                     t,
		ctx:                   ctx,
		cancel:                cancel,
		warpStatus:            &router.WarpStatusState{},
		binds:                 newTestLbAltBinds(envSettings.failableUdp),
		quicResults:           make(chan string, 256),
		websocketUpstreamEnds: make(chan struct{}, 16),
	}
	t.Cleanup(cancel)

	// the upstream blocks listen before the route table names their ports
	upstreamListeners := map[string][]net.Listener{}
	if envSettings.upstreams {
		apiHandler := envSettings.apiHandler
		if apiHandler == nil {
			apiHandler = router.NewRouter(ctx, api.Routes())
		}
		upstreamHandlers := map[string]http.Handler{
			"api":                     apiHandler,
			testLbAltWebsocketService: env.websocketEchoHandler(),
		}
		for _, serviceName := range []string{"api", testLbAltWebsocketService} {
			blockCount := 1
			if serviceName == "api" {
				blockCount = 2
			}
			for range blockCount {
				listener, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				upstreamServer := &http.Server{
					Handler: upstreamHandlers[serviceName],
				}
				go upstreamServer.Serve(listener)
				t.Cleanup(func() { upstreamServer.Close() })
				upstreamListeners[serviceName] = append(upstreamListeners[serviceName], listener)
			}
		}
	}
	env.routeTable, env.revision = newTestLbAltRouteTable(t, envSettings, upstreamListeners)
	certificateStore, roots := newTestLbAltCertificateStore(t, env.routeTable)
	env.roots = roots

	// the exchange and connect's router, as run.go builds them, without the
	// trusted listeners: nothing here goes through nginx
	exchangeListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exchangePort := exchangeListener.Addr().(*net.TCPAddr).Port
	exchangeSettings := DefaultExchangeSettings()
	exchangeSettings.ConnectionAnnounceTimeout = 0
	exchangeSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	exchangeSettings.ConnectionTestConfig = V0TestConfig()
	// the clients send without contracts (AddNoContractPeer), as the perf
	// harness's nocontract mode does: the carriers are under test here
	exchangeSettings.ForwardEnforceActiveContracts = false
	exchangeSettings.ListenH3Port = 0
	exchangeSettings.ListenDnsPort = 0
	if envSettings.exchangeSettings != nil {
		envSettings.exchangeSettings(exchangeSettings)
	}
	env.exchange = NewExchangeWithListeners(
		ctx,
		"lbalt0",
		"connect",
		"test",
		map[int]int{exchangePort: exchangePort},
		map[string]string{"lbalt0": "127.0.0.1"},
		exchangeSettings,
		map[int]net.Listener{exchangePort: exchangeListener},
	)
	connectRouter, err := newConnectRouterFromExchange(ctx, cancel, env.exchange)
	if err != nil {
		t.Fatal(err)
	}
	env.connectRouter = connectRouter
	env.warpStatus.SetReady()

	env.settings = DefaultFrontServerSettings()
	env.settings.ListenPacket = env.binds.listenPacket
	env.settings.Listen = env.binds.listen
	env.settings.connectionResultObserverForTest = func(portKind string, result string) {
		select {
		case env.quicResults <- portKind + " " + result:
		default:
		}
	}
	if envSettings.frontSettings != nil {
		envSettings.frontSettings(env.settings)
	}
	frontServer, err := newFrontServer(&frontLoad{
		routeTable:        env.routeTable,
		revision:          env.revision,
		generation:        env.routeTable.Generation,
		certificateStore:  certificateStore,
		certificateExpiry: certificateStore.EarliestExpiry(),
	}, env.settings)
	if err != nil {
		t.Fatal(err)
	}
	env.frontServer = frontServer
	if err := frontServer.startWithRouter(ctx, connectRouter); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		frontServer.close()
		connectHandler := connectRouter.ConnectHandler()
		connectHandler.Close()
		env.exchange.Close()
		cancel()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), testLbAltProgressTimeout)
		defer closeCancel()
		if !connectHandler.WaitForIdle(closeCtx) {
			t.Errorf("connect's handler did not finish")
		}
	})
	return env
}

// Whether this host can bind address over tcp and udp.
func testLbAltCanBind(address netip.Addr) bool {
	network := "4"
	if address.Is6() {
		network = "6"
	}
	listener, err := net.Listen("tcp"+network, netip.AddrPortFrom(address, 0).String())
	if err != nil {
		return false
	}
	listener.Close()
	packetConn, err := net.ListenPacket("udp"+network, netip.AddrPortFrom(address, 0).String())
	if err != nil {
		return false
	}
	packetConn.Close()
	return true
}

// count distinct ports free over tcp and udp on every address, none of them
// in excludedPorts.
func testLbAltFreePorts(t testing.TB, addresses []netip.Addr, count int, excludedPorts []int) []int {
	t.Helper()
	portFree := func(port int) bool {
		closers := []io.Closer{}
		defer func() {
			for _, closer := range closers {
				closer.Close()
			}
		}()
		for _, address := range addresses {
			network := "4"
			if address.Is6() {
				network = "6"
			}
			bindAddress := netip.AddrPortFrom(address, uint16(port)).String()
			listener, err := net.Listen("tcp"+network, bindAddress)
			if err != nil {
				return false
			}
			closers = append(closers, listener)
			packetConn, err := net.ListenPacket("udp"+network, bindAddress)
			if err != nil {
				return false
			}
			closers = append(closers, packetConn)
		}
		return true
	}
	ports := []int{}
	for range 256 {
		if len(ports) == count {
			return ports
		}
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		probe.Close()
		if !slices.Contains(ports, port) && !slices.Contains(excludedPorts, port) && portFree(port) {
			ports = append(ports, port)
		}
	}
	t.Fatalf("found %d of %d free ports", len(ports), count)
	return nil
}

// The shared route fixture with the layout's interfaces and upstream, free
// listen ports, and in the upstreams layout the api's and the websocket
// service's blocks at the listeners given, encoded as warpctl writes it and
// parsed back, with the revision of its bytes.
func newTestLbAltRouteTable(t testing.TB, envSettings *testLbAltEnvSettings, upstreamListeners map[string][]net.Listener) (*nginx.RouteTable, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "nginx", "testdata", "route_v1_example.json"))
	if err != nil {
		t.Fatal(err)
	}
	routeTable, err := nginx.ParseRouteTable(raw)
	if err != nil {
		t.Fatal(err)
	}
	rateLimit := routeTable.Interfaces[0].RateLimit
	newInterface := func(name string, ipv4 string, ipv6 string) *nginx.RouteInterface {
		return &nginx.RouteInterface{
			Name:  name,
			Block: routeTable.Host + "-" + name,
			LbAlt: true,
			Ipv4:  ipv4,
			Ipv6:  ipv6,
			RateLimit: &nginx.RouteRateLimit{
				RequestsPerMinute: rateLimit.RequestsPerMinute,
				Burst:             rateLimit.Burst,
				NetConnections:    rateLimit.NetConnections,
				ExcludeSubnets:    slices.Clone(rateLimit.ExcludeSubnets),
			},
		}
	}
	secondIpv4 := ""
	if testLbAltCanBind(netip.MustParseAddr("127.0.0.2")) {
		secondIpv4 = "127.0.0.2"
	}
	if envSettings.upstreams {
		routeTable.Upstream.Ipv4 = "127.0.0.1"
		routeTable.Interfaces = []*nginx.RouteInterface{
			newInterface(testLbAltInterfaceA, secondIpv4, "::1"),
		}

		blockPort := func(listener net.Listener) int {
			return listener.Addr().(*net.TCPAddr).Port
		}
		apiService := routeTable.ApiService()
		for i, block := range apiService.Blocks {
			block.Port = blockPort(upstreamListeners["api"][i])
		}
		connectService := routeTable.ConnectService()
		routeTable.Services = append(routeTable.Services, &nginx.RouteService{
			Name:               testLbAltWebsocketService,
			Exposed:            true,
			LbExposed:          true,
			Status:             "standard",
			Names:              []string{testLbAltWebsocketName},
			RedirectAliases:    []*nginx.RouteRedirectAlias{},
			CorsOrigins:        []string{},
			Websocket:          true,
			Streamable:         false,
			StreamablePaths:    []string{},
			Stateful:           false,
			RoutePrefixes:      []string{""},
			RouteRateLimits:    []*nginx.RouteRateLimitRule{},
			RouteRequestBodies: []*nginx.RouteRequestBody{},
			NoSuccessAccessLog: []string{},
			Keepalive: &nginx.RouteKeepalive{
				Connections:    connectService.Keepalive.Connections,
				Requests:       connectService.Keepalive.Requests,
				TimeSeconds:    connectService.Keepalive.TimeSeconds,
				TimeoutSeconds: connectService.Keepalive.TimeoutSeconds,
			},
			Blocks: []*nginx.RouteBlock{
				{Name: "g1", Weight: 1, Port: blockPort(upstreamListeners[testLbAltWebsocketService][0])},
			},
		})
	} else {
		interfaceB := newInterface(testLbAltInterfaceB, secondIpv4, "::1")
		routeTable.Interfaces = []*nginx.RouteInterface{
			newInterface(testLbAltInterfaceA, "127.0.0.1", ""),
			interfaceB,
		}
	}

	addresses := []netip.Addr{}
	for _, routeInterface := range routeTable.Interfaces {
		for _, address := range []string{routeInterface.Ipv4, routeInterface.Ipv6} {
			if address != "" {
				addresses = append(addresses, netip.MustParseAddr(address))
			}
		}
	}
	blockPorts := []int{}
	for _, service := range routeTable.Services {
		for _, block := range service.Blocks {
			blockPorts = append(blockPorts, block.Port)
		}
	}
	ports := testLbAltFreePorts(t, addresses, 3, blockPorts)
	routeTable.Listen = &nginx.RouteListen{
		HttpPort:   ports[0],
		HttpsPort:  ports[1],
		QuicPort:   ports[1],
		WhodisPort: ports[2],
	}
	routeTable.Front.WhodisTlds = []string{testLbAltWhodisTld}
	if envSettings.routeTable != nil {
		envSettings.routeTable(routeTable)
	}

	encoded, err := json.MarshalIndent(routeTable, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	parsed, err := nginx.ParseRouteTable(encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return parsed, hex.EncodeToString(digest[:])
}

// A synthetic authority and one leaf for every name the route table serves,
// written in warpctl's findLatestTls layout,
// <vault>/<env>/tls/<version>/<dir>/<dir>.pem and .key, with star.<base> for a
// wildcard, then loaded by nginx.LoadCertificateStore as connect loads its
// own. Returns the store and the roots that verify it.
func newTestLbAltCertificateStore(t testing.TB, routeTable *nginx.RouteTable) (*nginx.CertificateStore, *x509.CertPool) {
	t.Helper()
	notBefore := time.Now().Add(-time.Hour)
	notAfter := time.Now().Add(24 * time.Hour)
	authorityKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authorityTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lb_alt test authority"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	authorityDer, err := x509.CreateCertificate(cryptorand.Reader, authorityTemplate, authorityTemplate, &authorityKey.PublicKey, authorityKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := x509.ParseCertificate(authorityDer)
	if err != nil {
		t.Fatal(err)
	}
	servedNames := routeTable.ServedNames()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: servedNames[0]},
		DNSNames:     servedNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDer, err := x509.CreateCertificate(cryptorand.Reader, leafTemplate, authority, &leafKey.PublicKey, authorityKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDer, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDer})
	keyBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDer})

	vaultDir := t.TempDir()
	for _, name := range servedNames {
		dirName := name
		if base, ok := strings.CutPrefix(name, "*."); ok {
			dirName = "star." + base
		}
		pairDir := filepath.Join(vaultDir, routeTable.Env, "tls", testLbAltTlsVersion, dirName)
		if err := os.MkdirAll(pairDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pairDir, dirName+".pem"), pemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pairDir, dirName+".key"), keyBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	certificateStore, err := nginx.LoadCertificateStore(vaultDir, routeTable.Env, routeTable)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority)
	return certificateStore, roots
}

// The websocket service's block: it upgrades every request and echoes each
// message, and signals websocketUpstreamEnds when an upgraded websocket ends.
func (self *testLbAltEnv) websocketEchoHandler() http.Handler {
	upgrader := &websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer func() {
			select {
			case self.websocketUpstreamEnds <- struct{}{}:
			default:
			}
		}()
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, message); err != nil {
				return
			}
		}
	})
}

// The address of an interface's family in the route table, or false.
func (self *testLbAltEnv) address(interfaceName string, family string) (netip.Addr, bool) {
	routeInterface, ok := self.routeTable.Interface(interfaceName)
	if !ok {
		return netip.Addr{}, false
	}
	address := routeInterface.Ipv4
	if family == frontFamilyIpv6 {
		address = routeInterface.Ipv6
	}
	if address == "" {
		return netip.Addr{}, false
	}
	return netip.MustParseAddr(address), true
}

// Every interface address of the route table, with its interface's name.
func (self *testLbAltEnv) interfaceAddresses() []testLbAltInterfaceAddress {
	interfaceAddresses := []testLbAltInterfaceAddress{}
	for _, routeInterface := range self.routeTable.Interfaces {
		for _, family := range []string{frontFamilyIpv4, frontFamilyIpv6} {
			if address, ok := self.address(routeInterface.Name, family); ok {
				interfaceAddresses = append(interfaceAddresses, testLbAltInterfaceAddress{
					interfaceName: routeInterface.Name,
					family:        family,
					address:       address,
				})
			}
		}
	}
	return interfaceAddresses
}

// One interface address of the route table.
type testLbAltInterfaceAddress struct {
	interfaceName string
	family        string
	address       netip.Addr
}

// The port of a port kind.
func (self *testLbAltEnv) port(portKind string) int {
	switch portKind {
	case frontPortHttp:
		return self.routeTable.Listen.HttpPort
	case frontPortHttps:
		return self.routeTable.Listen.HttpsPort
	case frontPortQuic:
		return self.routeTable.Listen.QuicPort
	default:
		return self.routeTable.Listen.WhodisPort
	}
}

// The address and port a port kind's sockets bind on address, which a client
// on the interface reaches through the DNAT.
func (self *testLbAltEnv) bindAddress(address netip.Addr, portKind string) netip.AddrPort {
	return netip.AddrPortFrom(address, uint16(self.port(portKind)))
}

// The private /status as warpctl reads it: connect's real handler, with the
// service's latch and the front.
func (self *testLbAltEnv) status() (int, http.Header) {
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	self.connectRouter.statusWithWarpStatus(w, r, self.warpStatus, self.frontServer)
	return w.Code, w.Header()
}

// The X-UR-Front-Sockets entry of one group, "<group>=<up>/<expected>".
func (self *testLbAltEnv) socketsEntry(header http.Header, protocol string, address netip.Addr, port int) string {
	groupName := fmt.Sprintf("%s/%s/%d", protocol, address, port)
	for _, entry := range strings.Split(header.Get(frontSocketsHeader), ",") {
		if strings.HasPrefix(entry, groupName+"=") {
			return entry
		}
	}
	return ""
}

// Waits until the number of sockets up is want.
func (self *testLbAltEnv) waitSocketsUp(want int) {
	self.t.Helper()
	deadline := time.After(testLbAltProgressTimeout)
	for {
		upCount, changed := self.frontServer.upCount.Get()
		if upCount == want {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			self.t.Fatalf("%d sockets up, want %d", upCount, want)
		}
	}
}

// The next QUIC connection result the front counted, "<port kind> <result>".
func (self *testLbAltEnv) nextQuicResult() string {
	self.t.Helper()
	select {
	case result := <-self.quicResults:
		return result
	case <-time.After(testLbAltProgressTimeout):
		self.t.Fatal("no QUIC connection was counted")
		return ""
	}
}

// Fails the test if a QUIC connection result is pending.
func (self *testLbAltEnv) expectNoQuicResult(what string) {
	self.t.Helper()
	select {
	case result := <-self.quicResults:
		self.t.Fatalf("%s: a QUIC connection was counted %q", what, result)
	default:
	}
}

// The client tls config of a raw QUIC or TLS dial that presents serverName,
// verified against the fixture's roots.
func (self *testLbAltEnv) clientTlsConfig(serverName string, nextProtos []string) *tls.Config {
	return &tls.Config{
		RootCAs:    self.roots,
		ServerName: serverName,
		NextProtos: nextProtos,
		MinVersion: tls.VersionTLS12,
	}
}

// The address a client's socket binds for a dial to address, and so the
// address the front accepts the client from: the loopback of address's
// family. That is the interface's own address only where the interface is on
// 127.0.0.1 or ::1, so a test compares a client address the front reports
// with this, never with the address dialed. Unbound, the socket would leave
// from whatever source the host picks: 127.0.0.1 for 127.0.0.2 on Linux, a
// loopback alias itself on darwin.
func testLbAltClientAddress(address netip.Addr) netip.Addr {
	if address.Is6() {
		return netip.IPv6Loopback()
	}
	return netip.AddrFrom4([4]byte{127, 0, 0, 1})
}

// A udp client socket of address's family, bound to testLbAltClientAddress.
func testLbAltListenClientPacket(address netip.Addr) (*net.UDPConn, error) {
	network := "udp4"
	if address.Is6() {
		network = "udp6"
	}
	return net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(testLbAltClientAddress(address), 0)))
}

// A udp client socket of address's family, bound to testLbAltClientAddress,
// closed with the test.
func (self *testLbAltEnv) clientPacketConn(address netip.Addr) *net.UDPConn {
	self.t.Helper()
	packetConn, err := testLbAltListenClientPacket(address)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { packetConn.Close() })
	return packetConn
}

// One network with one device and client: the client's jwt for connect, and
// the network's for the api.
type testLbAltPeer struct {
	networkId    server.Id
	userId       server.Id
	deviceId     server.Id
	clientId     server.Id
	instanceId   server.Id
	networkName  string
	clientByJwt  string
	networkByJwt string
}

// Creates a network with one client.
func (self *testLbAltEnv) newPeer(name string) *testLbAltPeer {
	peer := &testLbAltPeer{
		networkId:  server.NewId(),
		userId:     server.NewId(),
		deviceId:   server.NewId(),
		clientId:   server.NewId(),
		instanceId: server.NewId(),
	}
	peer.networkName = fmt.Sprintf("lbalt-%s-%s", name, peer.networkId)
	model.Testing_CreateNetwork(self.ctx, peer.networkId, peer.networkName, peer.userId)
	model.Testing_CreateDevice(self.ctx, peer.networkId, peer.deviceId, peer.clientId, name, name)
	byJwt := session.NewByJwt(peer.networkId, peer.userId, peer.networkName, false, false)
	peer.networkByJwt = byJwt.Testing_Sign()
	peer.clientByJwt = byJwt.Client(peer.deviceId, peer.clientId).Testing_Sign()
	return peer
}

// A platform transport carrier and how it is pinned.
type testLbAltCarrier struct {
	// for messages
	name string
	mode connect.TransportMode
	// on h1: the urnetwork-framer/1 upgrade rather than the websocket
	h1Plus bool
	// the carrier connect's resident sees
	residentCarrier ResidentCarrier
}

// The four carriers of a public interface (D9): connect's websocket and H1+
// on TCP 443, raw QUIC on UDP 443, and QUIC under whodis on UDP 4053 and 53.
var (
	testLbAltCarrierWebSocket = testLbAltCarrier{name: "websocket", mode: connect.TransportModeH1, h1Plus: false, residentCarrier: ResidentCarrierH1}
	testLbAltCarrierH1Plus    = testLbAltCarrier{name: "h1+", mode: connect.TransportModeH1, h1Plus: true, residentCarrier: ResidentCarrierH1}
	testLbAltCarrierQuic      = testLbAltCarrier{name: "quic", mode: connect.TransportModeH3, h1Plus: false, residentCarrier: ResidentCarrierH3}
	testLbAltCarrierWhodis    = testLbAltCarrier{name: "whodis", mode: connect.TransportModeH3Dns, h1Plus: false, residentCarrier: ResidentCarrierH3Dns}
)

// A client's view of how its transport reaches the front, adjusted by a test
// before the transport is built.
type testLbAltDial struct {
	address netip.Addr
	// replaces the udp endpoint of the h3 carriers, when set
	packetConnFactory func(ctx context.Context) (net.PacketConn, error)
	// added to every websocket dial of the client's strategy
	extraHeaders http.Header
}

// One connect client on one platform transport through the front.
type testLbAltClient struct {
	peer        *testLbAltPeer
	carrier     testLbAltCarrier
	client      *connect.Client
	h1Stats     *connect.H1ConnectionStats
	received    chan *testLbAltMessage
	closeOnce   sync.Once
	closeClient func()
}

// One message a client received.
type testLbAltMessage struct {
	sourceId     connect.Id
	messageIndex uint32
	content      string
}

// Client settings that keep peer to peer on loopback, with no STUN server.
func testLbAltClientSettings() *connect.ClientSettings {
	clientSettings := connect.DefaultClientSettings()
	clientSettings.WebRtcSettings.Log = connect.NewNoopLogger()
	clientSettings.WebRtcSettings.IceServerUrls = nil
	clientSettings.WebRtcSettings.IceServerPoolUrls = nil
	clientSettings.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
	return clientSettings
}

// How a client reaches the front on one carrier: the platform url names the
// connect name and the public https port; the strategy's tcp dial goes to the
// interface address's https socket from testLbAltClientAddress, and the h3
// carriers' packets to its quic or whodis socket
// (PlatformTransportSettings.AltUrl), the name staying the sni. Returns the
// platform url and the settings of the client's strategy and transport, new
// on each call.
func (self *testLbAltEnv) clientDialSettings(carrier testLbAltCarrier, dial *testLbAltDial) (string, *connect.ClientStrategySettings, *connect.PlatformTransportSettings) {
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.ExpandExtenderProfileCount = 0
	strategySettings.ExtraHeaders = dial.extraHeaders
	strategySettings.ConnectSettings.TlsConfig = &tls.Config{
		RootCAs: self.roots,
	}
	httpsTarget := self.bindAddress(dial.address, frontPortHttps).String()
	clientAddress := net.TCPAddrFromAddrPort(netip.AddrPortFrom(testLbAltClientAddress(dial.address), 0))
	strategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				LocalAddr: clientAddress,
			}
			return dialer.DialContext(ctx, "tcp", httpsTarget)
		},
	}

	transportSettings := connect.DefaultPlatformTransportSettings()
	transportSettings.EnableH1Plus = carrier.h1Plus
	transportSettings.QuicTlsConfig = &tls.Config{
		RootCAs: self.roots,
	}
	transportSettings.DnsTlds = [][]byte{[]byte(testLbAltWhodisTld)}
	transportSettings.H3PacketConnFactory = dial.packetConnFactory
	switch carrier.mode {
	case connect.TransportModeH3:
		transportSettings.AltUrl = "https://" + self.bindAddress(dial.address, frontPortQuic).String()
	case connect.TransportModeH3Dns:
		transportSettings.AltUrl = "https://" + self.bindAddress(dial.address, frontPortWhodis).String()
	}
	budgetStats := connect.DefaultPlatformTransportBudget().Stats()
	transportSettings.PlatformTransportBudget = connect.NewPlatformTransportBudget(
		budgetStats.TotalByteCount,
		budgetStats.MaxTransportCount,
	)
	platformUrl := fmt.Sprintf("wss://%s", net.JoinHostPort(testLbAltConnectName, strconv.Itoa(self.routeTable.Listen.HttpsPort)))
	return platformUrl, strategySettings, transportSettings
}

// Connects one client to the front on one carrier (clientDialSettings). Closes
// with the test.
func (self *testLbAltEnv) newClient(peer *testLbAltPeer, carrier testLbAltCarrier, dial *testLbAltDial) *testLbAltClient {
	self.t.Helper()
	clientSettings := testLbAltClientSettings()
	client := connect.NewClient(
		self.ctx,
		connect.Id(peer.clientId),
		Testing_NewControllerOutOfBandControl(self.ctx, peer.clientId, clientSettings.ContractManagerSettings),
		clientSettings,
	)
	platformUrl, strategySettings, transportSettings := self.clientDialSettings(carrier, dial)
	clientStrategy := connect.NewClientStrategy(self.ctx, strategySettings)
	h1Stats := &connect.H1ConnectionStats{}
	transportSettings.H1ConnectionStats = h1Stats
	transport := connect.NewPlatformTransportWithTargetMode(
		self.ctx,
		clientStrategy,
		client.RouteManager(),
		platformUrl,
		&connect.ClientAuth{
			ByJwt:      peer.clientByJwt,
			InstanceId: connect.Id(peer.instanceId),
			AppVersion: "0.0.0",
		},
		carrier.mode,
		transportSettings,
	)

	lbAltClient := &testLbAltClient{
		peer:     peer,
		carrier:  carrier,
		client:   client,
		h1Stats:  h1Stats,
		received: make(chan *testLbAltMessage, 4096),
	}
	client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, receivePeer connect.Peer) {
		for _, frame := range frames {
			message, err := connect.FromFrame(frame)
			if err != nil {
				continue
			}
			simpleMessage, ok := message.(*protocol.SimpleMessage)
			if !ok {
				continue
			}
			select {
			case lbAltClient.received <- &testLbAltMessage{
				sourceId:     source.SourceId,
				messageIndex: simpleMessage.MessageIndex,
				content:      simpleMessage.Content,
			}:
			default:
			}
		}
	})
	lbAltClient.closeClient = func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), testLbAltProgressTimeout)
		defer closeCancel()
		transport.Close()
		client.Close()
		transport.CloseAndWait(closeCtx)
		client.CloseAndWait(closeCtx)
		clientStrategy.Close()
	}
	self.t.Cleanup(lbAltClient.close)
	return lbAltClient
}

// Closes the client and its transport, once.
func (self *testLbAltClient) close() {
	self.closeOnce.Do(self.closeClient)
}

// The resident edges of a client: what each of its transports declared and
// how it arrived.
func (self *testLbAltEnv) residentEdges(clientId server.Id) []ResidentTransportEdge {
	for _, candidate := range self.exchange.ResidentMoveCandidates() {
		if candidate.ClientId == clientId {
			return candidate.Transports
		}
	}
	return nil
}

// Waits until the client's resident has exactly one transport and returns it:
// the client authenticated, and connect's handler attached the connection.
func (self *testLbAltEnv) waitResidentEdge(clientId server.Id) ResidentTransportEdge {
	self.t.Helper()
	deadline := time.Now().Add(testLbAltProgressTimeout)
	for {
		if edges := self.residentEdges(clientId); len(edges) == 1 {
			return edges[0]
		}
		if deadline.Before(time.Now()) {
			self.t.Fatalf("client %s has edges %v, want one", clientId, self.residentEdges(clientId))
		}
		select {
		case <-self.ctx.Done():
			self.t.Fatal("the env ended")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Sends count messages from one client to another, each acknowledged, and
// waits until every one has arrived in order of index and been acknowledged.
func testLbAltSendMessages(t testing.TB, from *testLbAltClient, to *testLbAltClient, count int, content string) {
	t.Helper()
	acked := make(chan error, count)
	destinationId := connect.Id(to.peer.clientId)
	for i := range count {
		frame, err := connect.ToFrame(&protocol.SimpleMessage{
			MessageIndex: uint32(i),
			MessageCount: uint32(count),
			Content:      content,
		}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		TestingSendConnectFrame(from.client, frame, destinationId, func(err error) {
			acked <- err
		})
	}
	deadline := time.After(testLbAltProgressTimeout)
	receivedIndexes := map[uint32]bool{}
	for len(receivedIndexes) < count {
		select {
		case message := <-to.received:
			if message.sourceId != connect.Id(from.peer.clientId) || message.content != content {
				t.Fatalf("%s received %d from %s with %q", to.carrier.name, message.messageIndex, message.sourceId, message.content)
			}
			receivedIndexes[message.messageIndex] = true
		case <-deadline:
			t.Fatalf("%s to %s: %d of %d messages arrived", from.carrier.name, to.carrier.name, len(receivedIndexes), count)
		}
	}
	for range count {
		select {
		case err := <-acked:
			if err != nil {
				t.Fatalf("%s to %s: a message was not acknowledged: %v", from.carrier.name, to.carrier.name, err)
			}
		case <-deadline:
			t.Fatalf("%s to %s: the acknowledgements did not arrive", from.carrier.name, to.carrier.name)
		}
	}
}

// Lets two clients send each other messages without contracts, and exchanges
// count messages each way.
func testLbAltExchangeMessages(t testing.TB, a *testLbAltClient, b *testLbAltClient, count int) {
	t.Helper()
	a.client.ContractManager().AddNoContractPeer(connect.Id(b.peer.clientId))
	b.client.ContractManager().AddNoContractPeer(connect.Id(a.peer.clientId))
	testLbAltSendMessages(t, a, b, count, a.carrier.name+" to "+b.carrier.name)
	testLbAltSendMessages(t, b, a, count, b.carrier.name+" to "+a.carrier.name)
}

// An api client of real client code: a strategy with only the api's alt
// dialers (EXTENDER.md L4), whose alt url pins the address's quic or whodis
// socket, as a client reaches alt.* on an interface.
func (self *testLbAltEnv) newApiStrategy(address netip.Addr, portKind string) *connect.ClientStrategy {
	settings := connect.DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExpandExtenderProfileCount = 0
	settings.AltUrl = "https://" + self.bindAddress(address, portKind).String()
	settings.DnsTlds = [][]byte{[]byte(testLbAltWhodisTld)}
	settings.ConnectSettings.TlsConfig = &tls.Config{
		RootCAs:    self.roots,
		MinVersion: tls.VersionTLS13,
	}
	clientStrategy := connect.NewClientStrategy(self.ctx, settings)
	self.t.Cleanup(clientStrategy.Close)
	return clientStrategy
}

// Records the front's binds in order by bind address, and lets a test hold
// the binds of one address and fail the writes of the udp sockets it hands
// the front.
type testLbAltBinds struct {
	// hands the front each udp socket in a wrapper whose writes a test can
	// fail. A wrapped socket is no *net.UDPConn, so quic-go then reads and
	// writes it without GSO, ECN or OOB; unwrapped, the front gets the raw
	// socket, as in production.
	failable bool
	// successful binds so far, for tests to wait on
	boundCount *connect.MonitorValue[int]
	// the writes failed so far on every failable socket
	failedWriteCount *connect.MonitorValue[int]

	stateLock   sync.Mutex
	packetConns map[string][]net.PacketConn
	// the wrappers, when failable
	failingConns map[string][]*testLbAltPacketConn
	listeners    map[string][]net.Listener
	holds        map[string]*testLbAltBindHold
}

// Binds that record and never hold until told to.
func newTestLbAltBinds(failable bool) *testLbAltBinds {
	return &testLbAltBinds{
		failable:         failable,
		boundCount:       connect.NewMonitorValue(0),
		failedWriteCount: connect.NewMonitorValue(0),
		packetConns:      map[string][]net.PacketConn{},
		failingConns:     map[string][]*testLbAltPacketConn{},
		listeners:        map[string][]net.Listener{},
		holds:            map[string]*testLbAltBindHold{},
	}
}

// A barrier on the binds of one address: each waiting bind takes one permit.
type testLbAltBindHold struct {
	// binds waiting at the barrier
	waitingCount *connect.MonitorValue[int]
	permits      chan struct{}
	released     chan struct{}
	releaseOnce  sync.Once
}

// Holds every later bind of protocol on address until it is let through.
func (self *testLbAltBinds) hold(protocol string, address netip.AddrPort) *testLbAltBindHold {
	hold := &testLbAltBindHold{
		waitingCount: connect.NewMonitorValue(0),
		permits:      make(chan struct{}, 64),
		released:     make(chan struct{}),
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holds[testLbAltHoldKey(protocol, address.String())] = hold
	return hold
}

// The key of a hold: tcp and udp 443 share an address and port.
func testLbAltHoldKey(protocol string, address string) string {
	return protocol + " " + address
}

// Waits until count binds wait at the barrier.
func (self *testLbAltBindHold) waitWaiting(t testing.TB, count int) {
	t.Helper()
	deadline := time.After(testLbAltProgressTimeout)
	for {
		waitingCount, changed := self.waitingCount.Get()
		if count <= waitingCount {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d binds wait at the barrier, want %d", waitingCount, count)
		}
	}
}

// Lets one waiting or later bind through.
func (self *testLbAltBindHold) releaseOne() {
	self.permits <- struct{}{}
}

// Lets every bind through, now and later.
func (self *testLbAltBindHold) releaseAll() {
	self.releaseOnce.Do(func() {
		close(self.released)
	})
}

// Waits out a hold on the protocol and address, if any.
func (self *testLbAltBinds) beforeBind(protocol string, address string) {
	hold := func() *testLbAltBindHold {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.holds[testLbAltHoldKey(protocol, address)]
	}()
	if hold == nil {
		return
	}
	hold.waitingCount.Update(func(waitingCount int) int {
		return waitingCount + 1
	})
	defer hold.waitingCount.Update(func(waitingCount int) int {
		return waitingCount - 1
	})
	select {
	case <-hold.permits:
	case <-hold.released:
	}
}

// FrontServerSettings.ListenPacket: the front's own SO_REUSEPORT bind,
// recorded, and wrapped when failable.
func (self *testLbAltBinds) listenPacket(network string, address string) (net.PacketConn, error) {
	self.beforeBind(frontProtocolUdp, address)
	packetConn, err := listenFrontPacket(network, address)
	if err != nil {
		return nil, err
	}
	var failingConn *testLbAltPacketConn
	if self.failable {
		failingConn = &testLbAltPacketConn{
			PacketConn:  packetConn,
			failedCount: self.failedWriteCount,
		}
		packetConn = failingConn
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.packetConns[address] = append(self.packetConns[address], packetConn)
		if failingConn != nil {
			self.failingConns[address] = append(self.failingConns[address], failingConn)
		}
	}()
	self.boundCount.Update(func(boundCount int) int {
		return boundCount + 1
	})
	return packetConn, nil
}

// FrontServerSettings.Listen: the front's own SO_REUSEPORT bind, recorded.
func (self *testLbAltBinds) listen(network string, address string) (net.Listener, error) {
	self.beforeBind(frontProtocolTcp, address)
	listener, err := listenFront(network, address)
	if err != nil {
		return nil, err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.listeners[address] = append(self.listeners[address], listener)
	}()
	self.boundCount.Update(func(boundCount int) int {
		return boundCount + 1
	})
	return listener, nil
}

// The udp sockets bound on address, in bind order, as the front got them.
func (self *testLbAltBinds) boundPacketConns(address netip.AddrPort) []net.PacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.packetConns[address.String()])
}

// The wrappers of the udp sockets bound on address, in bind order, when
// failable.
func (self *testLbAltBinds) boundFailingConns(address netip.AddrPort) []*testLbAltPacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.failingConns[address.String()])
}

// The tcp listeners bound on address, in bind order.
func (self *testLbAltBinds) boundListeners(address netip.AddrPort) []net.Listener {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.listeners[address.String()])
}

// One public udp socket the front was handed. Its writes fail while failures
// are queued, each failure costing one datagram, as a socket fails to write
// to one address or for a moment; its next read fails when a test says so,
// as a socket fails for good.
type testLbAltPacketConn struct {
	net.PacketConn
	// the writes failed so far on every socket of the binds
	failedCount *connect.MonitorValue[int]

	stateLock sync.Mutex
	writeErrs []error
	readErr   error
}

// Queues errors for the next writes.
func (self *testLbAltPacketConn) failWrites(errs ...error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeErrs = append(self.writeErrs, errs...)
}

// Drops the failures still queued.
func (self *testLbAltPacketConn) clearWriteFailures() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeErrs = nil
}

// Fails the next read with err, and every read after it. A read waiting now
// is woken by a deadline in the past, and answers err too.
func (self *testLbAltPacketConn) failReads(err error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readErr = err
	}()
	self.PacketConn.SetReadDeadline(time.Now())
}

// Writes the datagram, or answers the next queued failure.
func (self *testLbAltPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.writeErrs) == 0 {
			return nil
		}
		err := self.writeErrs[0]
		self.writeErrs = self.writeErrs[1:]
		return err
	}()
	if err != nil {
		self.failedCount.Update(func(failedCount int) int {
			return failedCount + 1
		})
		return 0, err
	}
	return self.PacketConn.WriteTo(p, addr)
}

// Reads a datagram, or answers the read failure.
func (self *testLbAltPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	readFailure := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.readErr
	}
	if readErr := readFailure(); readErr != nil {
		return 0, nil, readErr
	}
	n, addr, err := self.PacketConn.ReadFrom(p)
	if readErr := readFailure(); readErr != nil {
		return 0, nil, readErr
	}
	return n, addr, err
}

// A client udp socket that records the source of every datagram it reads, as
// quic-go never checks it: it routes by connection id.
type testLbAltSourceConn struct {
	net.PacketConn

	stateLock sync.Mutex
	sources   map[netip.AddrPort]int
}

// Wraps packetConn.
func newTestLbAltSourceConn(packetConn net.PacketConn) *testLbAltSourceConn {
	return &testLbAltSourceConn{
		PacketConn: packetConn,
		sources:    map[netip.AddrPort]int{},
	}
}

// Reads a datagram and records its source.
func (self *testLbAltSourceConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := self.PacketConn.ReadFrom(p)
	if err == nil {
		if udpAddr, ok := addr.(*net.UDPAddr); ok {
			source := udpAddr.AddrPort()
			source = netip.AddrPortFrom(source.Addr().Unmap(), source.Port())
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.sources[source] += 1
			}()
		}
	}
	return n, addr, err
}

// The sources read so far, with their datagram counts.
func (self *testLbAltSourceConn) readSources() map[netip.AddrPort]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sources := map[netip.AddrPort]int{}
	for source, count := range self.sources {
		sources[source] = count
	}
	return sources
}

// The client QUIC config of the fixture's raw dials.
func testLbAltClientQuicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: testLbAltProgressTimeout,
		MaxIdleTimeout:       time.Minute,
		InitialPacketSize:    connect.H3InitialPacketByteCount,
	}
}

// Wraps a client udp socket for a port kind: a whodis dial goes through the
// client half of the dns translation, the client's own code, with the route
// table's tld. The translation closes with the test.
func (self *testLbAltEnv) clientCarrierConn(packetConn net.PacketConn, portKind string) net.PacketConn {
	self.t.Helper()
	if portKind != frontPortWhodis {
		return packetConn
	}
	ptSettings := connect.DefaultPacketTranslationSettings()
	ptSettings.DnsTlds = [][]byte{[]byte(testLbAltWhodisTld)}
	translation, err := connect.NewPacketTranslation(
		self.ctx,
		connect.PacketTranslationModeDns,
		packetConn,
		ptSettings,
	)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { translation.Close() })
	return translation
}

// Dials QUIC to a port kind's socket on address from packetConn, already
// wrapped for the port kind. early dials with 0-RTT where a ticket allows it.
func (self *testLbAltEnv) dialQuicFrom(
	packetConn net.PacketConn,
	address netip.Addr,
	portKind string,
	tlsConfig *tls.Config,
	quicConfig *quic.Config,
	early bool,
) (*quic.Conn, error) {
	quicTransport := &quic.Transport{Conn: packetConn}
	self.t.Cleanup(func() { quicTransport.Close() })
	udpAddr := net.UDPAddrFromAddrPort(self.bindAddress(address, portKind))
	ctx, cancel := context.WithTimeout(self.ctx, testLbAltProgressTimeout)
	defer cancel()
	if early {
		return quicTransport.DialEarly(ctx, udpAddr, tlsConfig, quicConfig)
	}
	return quicTransport.Dial(ctx, udpAddr, tlsConfig, quicConfig)
}

// Dials QUIC from a new client socket, presenting serverName with nextProtos.
func (self *testLbAltEnv) dialQuic(address netip.Addr, portKind string, serverName string, nextProtos []string) (*quic.Conn, error) {
	packetConn := self.clientCarrierConn(self.clientPacketConn(address), portKind)
	return self.dialQuicFrom(packetConn, address, portKind, self.clientTlsConfig(serverName, nextProtos), testLbAltClientQuicConfig(), false)
}

// How the server closed one QUIC connection: the dial's error or, for a dial
// that returned a connection, the error its next accept sees.
type testLbAltQuicClose struct {
	// the application code, when an application close arrived
	applicationCode    quic.ApplicationErrorCode
	applicationArrived bool
	// the transport error code, when the server closed with one
	transportCode quic.TransportErrorCode
	transportErr  bool
	err           error
}

// Reads how the server closed conn, or failed its dial.
func testLbAltCloseOf(conn *quic.Conn, dialErr error) testLbAltQuicClose {
	err := dialErr
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), testLbAltProgressTimeout)
		defer cancel()
		_, err = conn.AcceptStream(ctx)
		if err == nil {
			return testLbAltQuicClose{err: errors.New("the connection was not closed")}
		}
	}
	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.Remote {
		return testLbAltQuicClose{
			applicationCode:    applicationErr.ErrorCode,
			applicationArrived: true,
			err:                err,
		}
	}
	var transportErr *quic.TransportError
	if errors.As(err, &transportErr) && transportErr.Remote {
		return testLbAltQuicClose{
			transportCode: transportErr.ErrorCode,
			transportErr:  true,
			err:           err,
		}
	}
	return testLbAltQuicClose{err: err}
}

// Waits for a channel or fails the test.
func testLbAltWait(t testing.TB, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testLbAltProgressTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

// An error that a test's failing socket answers.
var errTestLbAltSocket = errors.New("the test's socket failed")
