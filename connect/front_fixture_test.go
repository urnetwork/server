package connect

// Fixtures of the lb_alt front's tests: a synthetic route table on loopback
// interfaces, a fake nginx front with generated certificates, a fake of
// connect's QUIC handler, recording binds and clients. The fake hands the
// public servers to a real nginx front to configure, and tests that need the
// nginx front's handlers too start with the real one
// (front_nginx_fixture_test.go). Nothing here reaches Postgres or Redis: the
// front's sockets, dispatch, readiness and drain are all in-process.

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
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/qlogwriter"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/nginx"
)

// The bound on any one step of a front test: a dial, a request, a wait for
// a barrier. Generous for a loaded machine; tests never wait it out on
// success.
const testFrontProgressTimeout = 30 * time.Second

// Names of the shared route fixture (nginx/testdata/route_v1_example.json)
// the tests present.
const (
	testFrontConnectName   = "connect.service.example"
	testFrontConnectWild   = "g1.connect.service.example"
	testFrontApiName       = "api.service.example"
	testFrontForwardedName = "mcp.service.example"
	testFrontUnknownName   = "other.unknown.example"
	testFrontWhodisTld     = "whodis.example."
)

// The interfaces of the synthetic route table. eth1 has 127.0.0.1; eth2 has
// ::1, and 127.0.0.2 where the host can bind it (Linux; darwin configures
// only 127.0.0.1 on lo0), so every host tests two interfaces and both
// families.
const (
	testFrontInterfaceA = "eth1"
	testFrontInterfaceB = "eth2"
)

// A synthetic certificate authority and one leaf for every name the route
// table serves, wildcards included.
type testFrontIdentity struct {
	certificate tls.Certificate
	roots       *x509.CertPool
}

// Generates the authority and the leaf for names.
func newTestFrontIdentity(t testing.TB, names []string) *testFrontIdentity {
	t.Helper()
	notBefore := time.Now().Add(-time.Hour)
	notAfter := time.Now().Add(24 * time.Hour)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "front test authority"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDer, err := x509.CreateCertificate(cryptorand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caDer)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDer, err := x509.CreateCertificate(cryptorand.Reader, leafTemplate, caCertificate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	return &testFrontIdentity{
		certificate: tls.Certificate{
			Certificate: [][]byte{leafDer},
			PrivateKey:  leafKey,
		},
		roots: roots,
	}
}

// Whether this host can bind address over both protocols.
func testFrontCanBind(address netip.Addr) bool {
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

// Whether port is free over tcp and udp on every address.
func testFrontPortFree(addresses []netip.Addr, port int) bool {
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
		listener, err := net.Listen("tcp"+network, netip.AddrPortFrom(address, uint16(port)).String())
		if err != nil {
			return false
		}
		closers = append(closers, listener)
		packetConn, err := net.ListenPacket("udp"+network, netip.AddrPortFrom(address, uint16(port)).String())
		if err != nil {
			return false
		}
		closers = append(closers, packetConn)
	}
	return true
}

// count distinct ports free over tcp and udp on every address.
func testFrontFreePorts(t testing.TB, addresses []netip.Addr, count int) []int {
	t.Helper()
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
		if !slices.Contains(ports, port) && testFrontPortFree(addresses, port) {
			ports = append(ports, port)
		}
	}
	t.Fatalf("found %d of %d free ports", len(ports), count)
	return nil
}

// The shared route fixture with the test interfaces on loopback and free
// listen ports, encoded as warpctl writes it and parsed back, and the
// revision of its bytes.
func newTestFrontRouteTable(t testing.TB) (*nginx.RouteTable, string) {
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
	interfaceB := &nginx.RouteInterface{
		Name:      testFrontInterfaceB,
		Block:     routeTable.Host + "-" + testFrontInterfaceB,
		LbAlt:     true,
		Ipv4:      "",
		Ipv6:      "::1",
		RateLimit: rateLimit,
	}
	if testFrontCanBind(netip.MustParseAddr("127.0.0.2")) {
		interfaceB.Ipv4 = "127.0.0.2"
	}
	routeTable.Interfaces = []*nginx.RouteInterface{
		{
			Name:      testFrontInterfaceA,
			Block:     routeTable.Host + "-" + testFrontInterfaceA,
			LbAlt:     true,
			Ipv4:      "127.0.0.1",
			Ipv6:      "",
			RateLimit: rateLimit,
		},
		interfaceB,
	}
	addresses := []netip.Addr{}
	for _, routeInterface := range routeTable.Interfaces {
		for _, address := range []string{routeInterface.Ipv4, routeInterface.Ipv6} {
			if address != "" {
				addresses = append(addresses, netip.MustParseAddr(address))
			}
		}
	}
	ports := testFrontFreePorts(t, addresses, 3)
	routeTable.Listen = &nginx.RouteListen{
		HttpPort:   ports[0],
		HttpsPort:  ports[1],
		QuicPort:   ports[1],
		WhodisPort: ports[2],
	}
	routeTable.Front.WhodisTlds = []string{testFrontWhodisTld}
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

// A fake nginx front: name classes from the route table with nginx's
// wildcards, one long-lived TLS config of each kind with a generated
// certificate, alt's per-address QUIC cap, and handlers that answer with the
// interface they were built for. It records the calls connect makes.
type testFront struct {
	identity        *testFrontIdentity
	nameClasses     map[string]nginx.NameClass
	wildcardClasses map[string]nginx.NameClass
	tlsConfig       *tls.Config
	quicTlsConfig   *tls.Config

	// connections admitted per (interface, address); zero is no cap
	connectionCap int
	// replace the default handlers when set
	httpsHandler func(interfaceName string) http.Handler
	httpHandler  func(interfaceName string) http.Handler
	h3ApiHandler func(interfaceName string) http.Handler

	// signaled after each release of an admitted QUIC connection
	released chan struct{}
	// a real nginx front over the same route table and settings, which
	// configures the public servers as in production, so they get nginx's
	// limits
	configuringFront *nginx.Front

	stateLock        sync.Mutex
	connectionCounts map[string]int
	// "<interface> <address>" of every AcquireQuicConnection
	acquiredKeys []string
	events       []string
	// every server ConfigureServer was given, in order
	configuredServers []*http.Server
}

// A fake front for the route table's names, serving identity.
func newTestFront(routeTable *nginx.RouteTable, identity *testFrontIdentity) *testFront {
	front := &testFront{
		identity:         identity,
		nameClasses:      map[string]nginx.NameClass{},
		wildcardClasses:  map[string]nginx.NameClass{},
		released:         make(chan struct{}, 64),
		connectionCounts: map[string]int{},
	}
	addName := func(name string, nameClass nginx.NameClass) {
		if suffix, ok := strings.CutPrefix(name, "*."); ok {
			front.wildcardClasses[suffix] = nameClass
		} else {
			front.nameClasses[name] = nameClass
		}
	}
	for _, name := range routeTable.Lb.Names {
		addName(name, nginx.NameClassLb)
	}
	for _, service := range routeTable.Services {
		nameClass := nginx.NameClassForwarded
		switch service.Name {
		case routeTable.Front.ConnectService:
			nameClass = nginx.NameClassConnect
		case routeTable.Front.ApiService:
			nameClass = nginx.NameClassApi
		}
		for _, name := range service.Names {
			addName(name, nameClass)
		}
		for _, redirectAlias := range service.RedirectAliases {
			addName(redirectAlias.Name, nginx.NameClassRedirect)
		}
	}
	front.tlsConfig = &tls.Config{
		GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
			nextProtos := []string{"h2", "http/1.1"}
			if front.ClassifyName(clientHello.ServerName) == nginx.NameClassConnect {
				nextProtos = []string{"http/1.1"}
			}
			return &tls.Config{
				Certificates: []tls.Certificate{identity.certificate},
				NextProtos:   nextProtos,
			}, nil
		},
	}
	front.quicTlsConfig = &tls.Config{
		GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
			var nextProtos []string
			if slices.Contains(clientHello.SupportedProtos, http3.NextProtoH3) {
				nextProtos = []string{http3.NextProtoH3}
			}
			return &tls.Config{
				Certificates: []tls.Certificate{identity.certificate},
				NextProtos:   nextProtos,
			}, nil
		},
	}
	return front
}

// Records one call the front saw.
func (self *testFront) record(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

// The calls the front saw, in order.
func (self *testFront) recordedEvents() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// nginxFront
func (self *testFront) ClassifyName(name string) nginx.NameClass {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if nameClass, ok := self.nameClasses[name]; ok {
		return nameClass
	}
	for suffix, nameClass := range self.wildcardClasses {
		if strings.HasSuffix(name, "."+suffix) {
			return nameClass
		}
	}
	return nginx.NameClassUnknown
}

// nginxFront
func (self *testFront) TlsConfig() *tls.Config {
	return self.tlsConfig
}

// nginxFront
func (self *testFront) QuicTlsConfig() *tls.Config {
	return self.quicTlsConfig
}

// nginxFront
func (self *testFront) HttpsHandler(interfaceName string) http.Handler {
	if self.httpsHandler != nil {
		return self.httpsHandler(interfaceName)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverName := ""
		if r.TLS != nil {
			serverName = r.TLS.ServerName
		}
		fmt.Fprintf(w, "https %s %s %s", interfaceName, r.Proto, serverName)
	})
}

// nginxFront
func (self *testFront) HttpHandler(interfaceName string) http.Handler {
	if self.httpHandler != nil {
		return self.httpHandler(interfaceName)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "http %s %t", interfaceName, r.TLS != nil)
	})
}

// nginxFront
func (self *testFront) H3ApiHandler(interfaceName string) http.Handler {
	if self.h3ApiHandler != nil {
		return self.h3ApiHandler(interfaceName)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "api %s %s", interfaceName, r.URL.Path)
	})
}

// nginxFront
func (self *testFront) AcquireQuicConnection(interfaceName string, addr netip.Addr) (func(), bool) {
	key := interfaceName + " " + addr.String()
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.acquiredKeys = append(self.acquiredKeys, key)
		if 0 < self.connectionCap && self.connectionCap <= self.connectionCounts[key] {
			return false
		}
		self.connectionCounts[key] += 1
		return true
	}()
	if !admitted {
		return nil, false
	}
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.connectionCounts[key] -= 1
			}()
			self.released <- struct{}{}
		})
	}, true
}

// "<interface> <address>" of every AcquireQuicConnection so far.
func (self *testFront) acquired() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.acquiredKeys)
}

// nginxFront: the real front's configuration, recorded apart from the
// events
func (self *testFront) ConfigureServer(httpServer *http.Server) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.configuredServers = append(self.configuredServers, httpServer)
	}()
	self.configuringFront.ConfigureServer(httpServer)
}

// Every server ConfigureServer was given, in order.
func (self *testFront) recordedConfiguredServers() []*http.Server {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.configuredServers)
}

// nginxFront
func (self *testFront) BeginDrain() {
	self.record("front begin drain")
}

// nginxFront
func (self *testFront) Close() error {
	self.record("front close")
	return nil
}

// One connection connect's fake handler received.
type testFrontHandledQuic struct {
	conn    *quic.Conn
	carrier ResidentCarrier
}

// A fake of connect's handler for QUIC. Its transports count against one
// unvalidated-handshake limit, as ConnectHandler.NewQuicTransport's do; its
// config allows early data, as connect's does; it holds each connection it
// handles until the connection ends or release closes.
type testFrontConnectQuic struct {
	handshakeLimit *connectQuicHandshakeLimit
	// signaled after each handshake the limit counted
	counted chan struct{}
	tracer  func(context.Context, bool, quic.ConnectionID) qlogwriter.Trace
	handled chan testFrontHandledQuic
	release chan struct{}
	// refuses each connection while set, as a closing handler does
	refusing atomic.Bool
	// the connections whose window grants the front returned so far
	releasedCount *connect.MonitorValue[int]

	stateLock sync.Mutex
	// every socket a transport was built on, in order
	transportConns []net.PacketConn
	// every connection whose window grants the front returned, and whether
	// it was closed by then
	releasedConns []testFrontReleasedQuic
}

// One connection whose window grants the front returned.
type testFrontReleasedQuic struct {
	conn   *quic.Conn
	closed bool
}

// A fake with no handshake limit and no tracer.
func newTestFrontConnectQuic() *testFrontConnectQuic {
	return &testFrontConnectQuic{
		releasedCount: connect.NewMonitorValue(0),
		counted:       make(chan struct{}, 64),
		handled:       make(chan testFrontHandledQuic, 64),
		release:       make(chan struct{}),
	}
}

// frontConnectQuic
func (self *testFrontConnectQuic) HandleQuicConn(conn *quic.Conn, carrier ResidentCarrier) bool {
	if self.refusing.Load() {
		conn.CloseWithError(0, "")
		return false
	}
	self.handled <- testFrontHandledQuic{conn: conn, carrier: carrier}
	select {
	case <-conn.Context().Done():
	case <-self.release:
	}
	conn.CloseWithError(0, "")
	return true
}

// frontConnectQuic
func (self *testFrontConnectQuic) NewQuicTransport(conn net.PacketConn) *quic.Transport {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.transportConns = append(self.transportConns, conn)
	}()
	transport := &quic.Transport{Conn: conn}
	if limit := self.handshakeLimit; limit != nil {
		transport.VerifySourceAddress = limit.verifySourceAddress
		transport.ConnContext = func(ctx context.Context, info *quic.ClientInfo) (context.Context, error) {
			ctx, err := limit.connContext(ctx, info)
			if !info.AddrVerified {
				self.counted <- struct{}{}
			}
			return ctx, err
		}
	}
	return transport
}

// frontConnectQuic
func (self *testFrontConnectQuic) releaseQuicWindow(conn *quic.Conn) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.releasedConns = append(self.releasedConns, testFrontReleasedQuic{
			conn:   conn,
			closed: conn.Context().Err() != nil,
		})
	}()
	self.releasedCount.Update(func(releasedCount int) int {
		return releasedCount + 1
	})
}

// The connections whose window grants the front returned.
func (self *testFrontConnectQuic) released() []testFrontReleasedQuic {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.releasedConns)
}

// The sockets transports were built on.
func (self *testFrontConnectQuic) builtTransportConns() []net.PacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.transportConns)
}

// frontConnectQuic
func (self *testFrontConnectQuic) NewQuicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: testFrontProgressTimeout,
		MaxIdleTimeout:       time.Minute,
		Allow0RTT:            true,
		EnableDatagrams:      true,
		InitialPacketSize:    connect.H3InitialPacketByteCount,
		Tracer:               self.tracer,
	}
}

// Wraps the front's real binds: records each socket by its bind address, in
// bind order, and lets a test fail or hold the next binds of an address.
type testFrontBinds struct {
	// successful binds so far, for tests to wait on
	boundCount *connect.MonitorValue[int]

	stateLock   sync.Mutex
	networks    map[string][]string
	packetConns map[string][]net.PacketConn
	listeners   map[string][]net.Listener
	failCounts  map[string]int
	holds       map[string]*testFrontBindHold
}

// A barrier on the binds of one address.
type testFrontBindHold struct {
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

// Binds that record and never fail or hold until told to.
func newTestFrontBinds() *testFrontBinds {
	return &testFrontBinds{
		boundCount:  connect.NewMonitorValue(0),
		networks:    map[string][]string{},
		packetConns: map[string][]net.PacketConn{},
		listeners:   map[string][]net.Listener{},
		failCounts:  map[string]int{},
		holds:       map[string]*testFrontBindHold{},
	}
}

// Fails the next count binds of address.
func (self *testFrontBinds) failNext(address string, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failCounts[address] = count
}

// Holds every later bind of address until the returned release: started
// closes when the first such bind begins.
func (self *testFrontBinds) holdNext(address string) (started <-chan struct{}, release func()) {
	hold := &testFrontBindHold{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holds[address] = hold
	return hold.started, func() {
		hold.releaseOnce.Do(func() {
			close(hold.release)
		})
	}
}

// Before each bind: records its network, waits out a hold and spends a
// planned failure.
func (self *testFrontBinds) beforeBind(network string, address string) error {
	hold, fail := func() (*testFrontBindHold, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.networks[address] = append(self.networks[address], network)
		if 0 < self.failCounts[address] {
			self.failCounts[address] -= 1
			return nil, true
		}
		return self.holds[address], false
	}()
	if fail {
		return errors.New("the test's bind failure")
	}
	if hold != nil {
		hold.startedOnce.Do(func() {
			close(hold.started)
		})
		<-hold.release
	}
	return nil
}

// FrontServerSettings.ListenPacket
func (self *testFrontBinds) listenPacket(network string, address string) (net.PacketConn, error) {
	if err := self.beforeBind(network, address); err != nil {
		return nil, err
	}
	packetConn, err := listenFrontPacket(network, address)
	if err != nil {
		return nil, err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.packetConns[address] = append(self.packetConns[address], packetConn)
	}()
	self.boundCount.Update(func(boundCount int) int {
		return boundCount + 1
	})
	return packetConn, nil
}

// FrontServerSettings.Listen
func (self *testFrontBinds) listen(network string, address string) (net.Listener, error) {
	if err := self.beforeBind(network, address); err != nil {
		return nil, err
	}
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

// The udp sockets bound on address, in bind order.
func (self *testFrontBinds) boundPacketConns(address string) []net.PacketConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.packetConns[address])
}

// The tcp listeners bound on address, in bind order.
func (self *testFrontBinds) boundListeners(address string) []net.Listener {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.listeners[address])
}

// Waits until count udp sockets have been bound on address.
func (self *testFrontBinds) waitPacketConns(t testing.TB, address string, count int) {
	t.Helper()
	deadline := time.After(testFrontProgressTimeout)
	for {
		_, changed := self.boundCount.Get()
		boundCount := len(self.boundPacketConns(address))
		if count <= boundCount {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d udp sockets bound on %s, want %d", boundCount, address, count)
		}
	}
}

// Waits until count tcp listeners have been bound on address.
func (self *testFrontBinds) waitListeners(t testing.TB, address string, count int) {
	t.Helper()
	deadline := time.After(testFrontProgressTimeout)
	for {
		_, changed := self.boundCount.Get()
		boundCount := len(self.boundListeners(address))
		if count <= boundCount {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d tcp listeners bound on %s, want %d", boundCount, address, count)
		}
	}
}

// The networks of every bind attempt on address, failed ones included.
func (self *testFrontBinds) bindNetworks(address string) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.networks[address])
}

// Every address a bind named.
func (self *testFrontBinds) bindAddresses() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addresses := []string{}
	for address := range self.networks {
		addresses = append(addresses, address)
	}
	slices.Sort(addresses)
	return addresses
}

// One dispatch the front's observer saw.
type testFrontDispatch struct {
	serverName string
	nameClass  nginx.NameClass
}

// One started front server with its fakes.
type testFrontEnv struct {
	t           testing.TB
	ctx         context.Context
	routeTable  *nginx.RouteTable
	revision    string
	identity    *testFrontIdentity
	front       *testFront
	connectQuic *testFrontConnectQuic
	binds       *testFrontBinds
	settings    *FrontServerSettings
	frontServer *frontServer
	dispatched  chan testFrontDispatch
}

// The certificate expiry the fixture reports.
var testFrontCertificateExpiry = time.Date(2027, time.January, 15, 12, 30, 0, 0, time.UTC)

// Builds and starts a front server on the synthetic route table. mutate,
// when set, adjusts the fakes and settings before the start. The front
// closes with the test.
func newTestFrontEnv(t testing.TB, mutate func(env *testFrontEnv)) *testFrontEnv {
	t.Helper()
	env := newUnstartedTestFrontEnv(t, mutate)
	if err := env.frontServer.start(env.ctx, env.front, env.connectQuic); err != nil {
		t.Fatal(err)
	}
	return env
}

// As newTestFrontEnv, without the start.
func newUnstartedTestFrontEnv(t testing.TB, mutate func(env *testFrontEnv)) *testFrontEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	routeTable, revision := newTestFrontRouteTable(t)
	identity := newTestFrontIdentity(t, routeTable.ServedNames())
	binds := newTestFrontBinds()
	settings := DefaultFrontServerSettings()
	settings.ListenPacket = binds.listenPacket
	settings.Listen = binds.listen
	env := &testFrontEnv{
		t:           t,
		ctx:         ctx,
		routeTable:  routeTable,
		revision:    revision,
		identity:    identity,
		front:       newTestFront(routeTable, identity),
		connectQuic: newTestFrontConnectQuic(),
		binds:       binds,
		settings:    settings,
		dispatched:  make(chan testFrontDispatch, 64),
	}
	settings.dispatchObserverForTest = func(serverName string, nameClass nginx.NameClass) {
		select {
		case env.dispatched <- testFrontDispatch{serverName: serverName, nameClass: nameClass}:
		default:
		}
	}
	if mutate != nil {
		mutate(env)
	}
	certificateStore := loadTestFrontCertificateStore(t, routeTable, identity)
	env.front.configuringFront = newTestNginxFront(t, env, certificateStore, http.NotFoundHandler())
	frontServer, err := newFrontServer(&frontLoad{
		routeTable:        routeTable,
		revision:          revision,
		generation:        routeTable.Generation,
		certificateStore:  certificateStore,
		certificateExpiry: testFrontCertificateExpiry,
	}, settings)
	if err != nil {
		t.Fatal(err)
	}
	env.frontServer = frontServer
	t.Cleanup(func() {
		frontServer.close()
		cancel()
	})
	return env
}

// The address of an interface's family in the route table, or false.
func (self *testFrontEnv) address(interfaceName string, family string) (netip.Addr, bool) {
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

// The bind address of the sockets of a port kind on address.
func (self *testFrontEnv) bindAddress(address netip.Addr, portKind string) string {
	port := 0
	switch portKind {
	case frontPortHttp:
		port = self.routeTable.Listen.HttpPort
	case frontPortHttps:
		port = self.routeTable.Listen.HttpsPort
	case frontPortQuic:
		port = self.routeTable.Listen.QuicPort
	case frontPortWhodis:
		port = self.routeTable.Listen.WhodisPort
	}
	return netip.AddrPortFrom(address, uint16(port)).String()
}

// The next dispatch the front observed.
func (self *testFrontEnv) nextDispatch() testFrontDispatch {
	self.t.Helper()
	select {
	case dispatch := <-self.dispatched:
		return dispatch
	case <-time.After(testFrontProgressTimeout):
		self.t.Fatal("no connection was dispatched")
		return testFrontDispatch{}
	}
}

// The next connection connect's fake handler received.
func (self *testFrontEnv) nextHandled() testFrontHandledQuic {
	self.t.Helper()
	select {
	case handled := <-self.connectQuic.handled:
		return handled
	case <-time.After(testFrontProgressTimeout):
		self.t.Fatal("connect's handler received no connection")
		return testFrontHandledQuic{}
	}
}

// Waits until the number of sockets up is want.
func (self *testFrontEnv) waitSocketsUp(want int) {
	self.t.Helper()
	deadline := time.After(testFrontProgressTimeout)
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

// A udp client socket of address's family on loopback.
func (self *testFrontEnv) clientPacketConn(address netip.Addr) net.PacketConn {
	self.t.Helper()
	network, local := "udp4", "127.0.0.1:0"
	if address.Is6() {
		network, local = "udp6", "[::1]:0"
	}
	packetConn, err := net.ListenPacket(network, local)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { packetConn.Close() })
	return packetConn
}

// The client tls config for serverName, verifying the fixture's identity.
func (self *testFrontEnv) clientTlsConfig(serverName string, nextProtos []string) *tls.Config {
	return &tls.Config{
		RootCAs:    self.identity.roots,
		ServerName: serverName,
		NextProtos: nextProtos,
		MinVersion: tls.VersionTLS12,
	}
}

// The client QUIC config of the fixture's dials.
func testFrontClientQuicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: testFrontProgressTimeout,
		MaxIdleTimeout:       time.Minute,
		InitialPacketSize:    connect.H3InitialPacketByteCount,
	}
}

// Dials QUIC to the port kind's socket on address from packetConn,
// presenting serverName and nextProtos. A whodis dial goes through the
// client half of the dns translation with the route table's tld.
func (self *testFrontEnv) dialQuicFrom(
	packetConn net.PacketConn,
	address netip.Addr,
	portKind string,
	tlsConfig *tls.Config,
) (*quic.Conn, error) {
	if portKind == frontPortWhodis {
		ptSettings := connect.DefaultPacketTranslationSettings()
		ptSettings.DnsTlds = [][]byte{[]byte(testFrontWhodisTld)}
		translation, err := connect.NewPacketTranslation(
			self.ctx,
			connect.PacketTranslationModeDns,
			packetConn,
			ptSettings,
		)
		if err != nil {
			return nil, err
		}
		self.t.Cleanup(func() { translation.Close() })
		packetConn = translation
	}
	quicTransport := &quic.Transport{Conn: packetConn}
	self.t.Cleanup(func() { quicTransport.Close() })
	udpAddr := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(self.bindAddress(address, portKind)))
	ctx, cancel := context.WithTimeout(self.ctx, testFrontProgressTimeout)
	defer cancel()
	return quicTransport.Dial(ctx, udpAddr, tlsConfig, testFrontClientQuicConfig())
}

// Dials QUIC from a new client socket.
func (self *testFrontEnv) dialQuic(address netip.Addr, portKind string, serverName string, nextProtos []string) (*quic.Conn, error) {
	return self.dialQuicFrom(self.clientPacketConn(address), address, portKind, self.clientTlsConfig(serverName, nextProtos))
}

// An h3 client for an api name on address's quic or whodis socket, each
// connection from a new client socket.
func (self *testFrontEnv) apiClient(address netip.Addr, portKind string, sessionCache tls.ClientSessionCache) (*http.Client, chan *quic.Conn) {
	conns := make(chan *quic.Conn, 16)
	tlsConfig := self.clientTlsConfig(testFrontApiName, nil)
	tlsConfig.ClientSessionCache = sessionCache
	roundTripper := &http3.Transport{
		TLSClientConfig: tlsConfig,
		QUICConfig:      testFrontClientQuicConfig(),
		Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			conn, err := self.dialQuicFrom(self.clientPacketConn(address), address, portKind, tlsConfig)
			if err == nil {
				select {
				case conns <- conn:
				default:
				}
			}
			return conn, err
		},
	}
	self.t.Cleanup(func() { roundTripper.Close() })
	return &http.Client{
		Transport: roundTripper,
		Timeout:   testFrontProgressTimeout,
	}, conns
}

// GETs path from an api client and returns the status and body.
func testFrontGet(t testing.TB, client *http.Client, serverName string, path string) (int, string, error) {
	t.Helper()
	response, err := client.Get("https://" + serverName + path)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, string(body), err
}

// An https client for serverName on address, each connection a new tcp
// connection to the socket's port.
func (self *testFrontEnv) httpsClient(address netip.Addr, serverName string, http2 bool, sessionCache tls.ClientSessionCache) *http.Client {
	tlsConfig := self.clientTlsConfig(serverName, nil)
	tlsConfig.ClientSessionCache = sessionCache
	target := self.bindAddress(address, frontPortHttps)
	transport := &http.Transport{
		TLSClientConfig:   tlsConfig,
		ForceAttemptHTTP2: http2,
		DialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, "tcp", target)
		},
	}
	if !http2 {
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	self.t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport,
		Timeout:   testFrontProgressTimeout,
	}
}

// How the server closed one QUIC connection: the dial's error or, for a
// dial that returned a connection, the error its next accept sees. The
// second result says whether the server's application code arrived.
func testFrontCloseCode(conn *quic.Conn, dialErr error) (quic.ApplicationErrorCode, bool, error) {
	err := dialErr
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), testFrontProgressTimeout)
		defer cancel()
		_, err = conn.AcceptStream(ctx)
		if err == nil {
			return 0, false, errors.New("the connection was not closed")
		}
	}
	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.Remote {
		return applicationErr.ErrorCode, true, nil
	}
	var transportErr *quic.TransportError
	if errors.As(err, &transportErr) && transportErr.Remote && transportErr.ErrorCode == quic.ApplicationErrorErrorCode {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("close err = %w (%T), want a remote application close", err, err)
}

// The application code the server closed one connection with, which must
// have arrived.
func testFrontApplicationCode(t testing.TB, conn *quic.Conn, dialErr error) quic.ApplicationErrorCode {
	t.Helper()
	code, arrived, err := testFrontCloseCode(conn, dialErr)
	if err != nil {
		t.Fatal(err)
	}
	if !arrived {
		t.Fatal("the connection was closed without its application code")
	}
	return code
}

// Waits for a channel or fails the test.
func testFrontWait(t testing.TB, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testFrontProgressTimeout):
		t.Fatalf("%s did not happen", what)
	}
}
