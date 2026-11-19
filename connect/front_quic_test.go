package connect

// The front's QUIC dispatch, alt's moved into connect (LBALT-IMPL.md §2.8,
// D3, D9, §4.4).

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/server/nginx"
)

// Connect names reach connect's handler in-process, with the carrier of the
// socket they arrived on: h3 on the quic port, h3dns under whodis, which
// decodes with the route table's tld rather than connect's hard-coded one.
// Raw QUIC connect offers no ALPN, and a one-label wildcard name is connect's.
func TestFrontQuicDispatchesConnectNamesToConnect(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	cases := []struct {
		interfaceName string
		family        string
		portKind      string
		serverName    string
		carrier       ResidentCarrier
	}{
		{interfaceName: testFrontInterfaceA, family: frontFamilyIpv4, portKind: frontPortQuic, serverName: testFrontConnectName, carrier: ResidentCarrierH3},
		{interfaceName: testFrontInterfaceA, family: frontFamilyIpv4, portKind: frontPortWhodis, serverName: testFrontConnectWild, carrier: ResidentCarrierH3Dns},
		{interfaceName: testFrontInterfaceB, family: frontFamilyIpv6, portKind: frontPortQuic, serverName: testFrontConnectWild, carrier: ResidentCarrierH3},
		{interfaceName: testFrontInterfaceB, family: frontFamilyIpv6, portKind: frontPortWhodis, serverName: testFrontConnectName, carrier: ResidentCarrierH3Dns},
	}
	for _, c := range cases {
		address, _ := env.address(c.interfaceName, c.family)
		conn, err := env.dialQuic(address, c.portKind, c.serverName, nil)
		if err != nil {
			t.Fatalf("%s %s %s: %v", address, c.portKind, c.serverName, err)
		}
		if dispatch := env.nextDispatch(); dispatch.serverName != c.serverName || dispatch.nameClass != nginx.NameClassConnect {
			t.Fatalf("%s %s dispatch = %+v", address, c.portKind, dispatch)
		}
		handled := env.nextHandled()
		if handled.carrier != c.carrier {
			t.Fatalf("%s %s carrier = %d, want %d", address, c.portKind, handled.carrier, c.carrier)
		}
		if got := handled.conn.ConnectionState().TLS.NegotiatedProtocol; got != "" {
			t.Fatalf("raw QUIC connect negotiated %q", got)
		}
		conn.CloseWithError(0, "")
	}
	acquired := env.front.acquired()
	wantAcquired := []string{
		testFrontInterfaceA + " 127.0.0.1",
		testFrontInterfaceA + " 127.0.0.1",
		testFrontInterfaceB + " ::1",
		testFrontInterfaceB + " ::1",
	}
	if !slices.Equal(acquired, wantAcquired) {
		t.Fatalf("connection cap charged to %v, want %v", acquired, wantAcquired)
	}
}

// Api names are served over h3 by the front's api handler of the interface
// the connection arrived on, on both the quic and the whodis ports.
func TestFrontQuicServesApiNamesOverH3(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	for _, routeInterface := range env.routeTable.Interfaces {
		for _, family := range []string{frontFamilyIpv4, frontFamilyIpv6} {
			address, ok := env.address(routeInterface.Name, family)
			if !ok {
				continue
			}
			for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
				client, _ := env.apiClient(address, portKind, nil)
				status, body, err := testFrontGet(t, client, testFrontApiName, "/hello")
				if err != nil || status != http.StatusOK || body != "api "+routeInterface.Name+" /hello" {
					t.Fatalf("%s %s: %d %q %v", address, portKind, status, body, err)
				}
				if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassApi {
					t.Fatalf("%s %s dispatch = %+v", address, portKind, dispatch)
				}
			}
		}
	}
	select {
	case handled := <-env.connectQuic.handled:
		t.Fatalf("an api connection reached connect's handler: %v", handled.conn.RemoteAddr())
	default:
	}
}

// Every name that is neither connect's nor the api's, a forwarded service, an
// lb name, an unknown name or no name at all, is closed with 0x1000, and the
// code arrives: the front waits for the handshake before it refuses.
func TestFrontQuicRefusesOtherNamesWithUnknownServerName(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, serverName := range []string{testFrontForwardedName, env.routeTable.Lb.Names[0], testFrontUnknownName, ""} {
		tlsConfig := env.clientTlsConfig(serverName, []string{http3.NextProtoH3})
		// an unknown or missing name has no certificate to verify against
		tlsConfig.InsecureSkipVerify = true
		conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, frontPortQuic, tlsConfig)
		if code := testFrontApplicationCode(t, conn, err); code != frontQuicErrorCodeUnknownServerName {
			t.Fatalf("%q closed with %#x, want %#x", serverName, code, frontQuicErrorCodeUnknownServerName)
		}
		if dispatch := env.nextDispatch(); dispatch.serverName != serverName || dispatch.nameClass == nginx.NameClassConnect || dispatch.nameClass == nginx.NameClassApi {
			t.Fatalf("%q dispatch = %+v", serverName, dispatch)
		}
	}
	select {
	case handled := <-env.connectQuic.handled:
		t.Fatalf("a refused connection reached connect's handler: %v", handled.conn.RemoteAddr())
	default:
	}
	if acquired := env.front.acquired(); len(acquired) != 0 {
		t.Fatalf("a refused connection was charged to the connection cap: %v", acquired)
	}
}

// A connect connection over the address's cap is closed with 0x1001 before
// connect's handler sees it; once the admitted connection ends, its slot
// admits the next.
func TestFrontQuicConnectionCapRefusesWithRateLimited(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.connectionCap = 1
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	first, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.nextDispatch()
	env.nextHandled()

	second, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if code := testFrontApplicationCode(t, second, err); code != frontQuicErrorCodeRateLimited {
		t.Fatalf("over the cap closed with %#x, want %#x", code, frontQuicErrorCodeRateLimited)
	}
	if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassConnect {
		t.Fatalf("over-cap dispatch = %+v", dispatch)
	}
	select {
	case <-env.connectQuic.handled:
		t.Fatal("an over-cap connection reached connect's handler")
	default:
	}

	// the admitted connection ends and releases its slot
	first.CloseWithError(0, "")
	select {
	case <-env.front.released:
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the admitted connection's slot was never released")
	}
	third, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer third.CloseWithError(0, "")
	env.nextDispatch()
	env.nextHandled()
}

// A client socket a test controls: quic-go reads nothing from it while
// delivering is false, which is all that makes a source spoofed to the
// server; retry closes on the first Retry it is sent.
type testFrontSilentSource struct {
	net.PacketConn
	delivering atomic.Bool
	retry      chan struct{}
	retryOnce  sync.Once
}

func (self *testFrontSilentSource) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := self.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		// a QUIC v1 Retry has a long header, the fixed bit and type 3
		if 0 < n && p[0]&0xf0 == 0xf0 {
			self.retryOnce.Do(func() {
				close(self.retry)
			})
		}
		if self.delivering.Load() {
			return n, addr, nil
		}
	}
}

// Dials from a source that receives nothing, so its handshake never
// completes; the dial ends with the test.
func (self *testFrontEnv) dialSilently(address netip.Addr) *testFrontSilentSource {
	source := &testFrontSilentSource{
		PacketConn: self.clientPacketConn(address),
		retry:      make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(self.ctx)
	self.t.Cleanup(cancel)
	go func() {
		self.dialQuicFrom(source, address, frontPortQuic, self.clientTlsConfig(testFrontConnectName, nil))
	}()
	go func() {
		<-ctx.Done()
		source.Close()
	}()
	return source
}

// One unvalidated-handshake limit stands behind every socket of the block:
// with a maximum of two, two silent sources on two interfaces hold it, and a
// third and a fourth source, one on each interface, are sent a Retry. A
// limit per socket would have counted them instead.
func TestFrontQuicSharesOneHandshakeAdmissionLimit(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.handshakeLimit = newConnectQuicHandshakeLimit(2)
	})
	addressA, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	addressB, _ := env.address(testFrontInterfaceB, frontFamilyIpv6)

	held := []*testFrontSilentSource{}
	for _, address := range []netip.Addr{addressA, addressB} {
		held = append(held, env.dialSilently(address))
		select {
		case <-env.connectQuic.counted:
		case <-time.After(testFrontProgressTimeout):
			t.Fatalf("the handshake from %s was not counted", address)
		}
	}
	for _, address := range []netip.Addr{addressA, addressB} {
		source := env.dialSilently(address)
		testFrontWait(t, source.retry, "a Retry at the shared maximum on "+address.String())
	}
	for _, source := range held {
		select {
		case <-source.retry:
			t.Fatal("a source under the maximum was sent a Retry")
		default:
		}
	}
	select {
	case <-env.connectQuic.counted:
		t.Fatal("a handshake past the shared maximum was counted")
	default:
	}
}

// A client socket that prepends a PROXY v2 header to every datagram, as
// nginx's stream proxy does, claiming source as the client's address.
type testFrontProxyPrefixConn struct {
	net.PacketConn
	header []byte
}

func newTestFrontProxyPrefixConn(packetConn net.PacketConn, source netip.AddrPort, destination netip.AddrPort) *testFrontProxyPrefixConn {
	header := []byte{
		// the v2 signature, then version 2 PROXY, then udp over ipv4
		0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A,
		0x21, 0x12, 0x00, 0x0C,
	}
	sourceBytes := source.Addr().As4()
	destinationBytes := destination.Addr().As4()
	header = append(header, sourceBytes[:]...)
	header = append(header, destinationBytes[:]...)
	header = binary.BigEndian.AppendUint16(header, source.Port())
	header = binary.BigEndian.AppendUint16(header, destination.Port())
	return &testFrontProxyPrefixConn{
		PacketConn: packetConn,
		header:     header,
	}
}

func (self *testFrontProxyPrefixConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if _, err := self.PacketConn.WriteTo(append(slices.Clone(self.header), p...), addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

// A public socket has no PROXY reader (§4.4.1): a client that prefixes its
// datagrams with a PROXY v2 header claiming another address is not decoded,
// so its handshake never completes and no connection is ever charged to the
// claimed address, while a plain client on the same socket connects. The
// transport reads the bound socket itself.
func TestFrontPublicSocketIgnoresAProxyProtocolHeader(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := netip.MustParseAddrPort(env.bindAddress(address, frontPortQuic))
	claimed := netip.MustParseAddrPort("203.0.113.7:4242")

	prefixed := newTestFrontProxyPrefixConn(env.clientPacketConn(address), claimed, bindAddress)
	quicTransport := &quic.Transport{Conn: prefixed}
	t.Cleanup(func() { quicTransport.Close() })
	quicConfig := testFrontClientQuicConfig()
	quicConfig.HandshakeIdleTimeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(env.ctx, testFrontProgressTimeout)
	defer cancel()
	if conn, err := quicTransport.Dial(ctx, net.UDPAddrFromAddrPort(bindAddress), env.clientTlsConfig(testFrontConnectName, nil), quicConfig); err == nil {
		conn.CloseWithError(0, "")
		t.Fatal("a PROXY-prefixed handshake completed on a public socket")
	}

	plain, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.CloseWithError(0, "")
	env.nextDispatch()
	env.nextHandled()
	select {
	case dispatch := <-env.dispatched:
		t.Fatalf("a second connection was dispatched: %+v", dispatch)
	default:
	}
	if acquired := env.front.acquired(); !slices.Equal(acquired, []string{testFrontInterfaceA + " 127.0.0.1"}) {
		t.Fatalf("connections charged to %v, want only the plain client's", acquired)
	}

	// every quic socket's transport reads the bound socket itself, with no
	// reader in between
	transportConns := env.connectQuic.builtTransportConns()
	for _, bindAddressText := range env.binds.bindAddresses() {
		for _, packetConn := range env.binds.boundPacketConns(bindAddressText) {
			isQuic := packetConn.LocalAddr().(*net.UDPAddr).Port == env.routeTable.Listen.QuicPort
			if isQuic && !slices.Contains(transportConns, packetConn) {
				t.Fatalf("the transport of %s does not read the bound socket", bindAddressText)
			}
		}
	}
}

// A ticket only resumes: the public QUIC config refuses early data (D9), so a
// resumed api connection that attempts 0-RTT reports Used0RTT false. The
// ticket was issued on another interface's socket, which resumes because
// every socket shares the front's one QUIC TLS config (D6c).
func TestFrontQuicResumptionReportsNoEarlyData(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	addressA, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	addressB, _ := env.address(testFrontInterfaceB, frontFamilyIpv6)
	sessionCache := &testFrontSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		stored:             make(chan struct{}, 4),
	}

	client, _ := env.apiClient(addressA, frontPortQuic, sessionCache)
	status, body, err := testFrontGet(t, client, testFrontApiName, "/first")
	if err != nil || status != http.StatusOK || body != "api "+testFrontInterfaceA+" /first" {
		t.Fatalf("first request = %d %q %v", status, body, err)
	}
	testFrontWait(t, sessionCache.stored, "a session ticket")

	conns := make(chan *quic.Conn, 1)
	tlsConfig := env.clientTlsConfig(testFrontApiName, nil)
	tlsConfig.ClientSessionCache = sessionCache
	earlyClient := &http.Client{
		Transport: &http3.Transport{
			TLSClientConfig: tlsConfig,
			QUICConfig:      testFrontClientQuicConfig(),
			Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
				quicTransport := &quic.Transport{Conn: env.clientPacketConn(addressB)}
				t.Cleanup(func() { quicTransport.Close() })
				udpAddr := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(env.bindAddress(addressB, frontPortQuic)))
				conn, err := quicTransport.DialEarly(ctx, udpAddr, tlsConfig, quicConfig)
				if err == nil {
					conns <- conn
				}
				return conn, err
			},
		},
		Timeout: testFrontProgressTimeout,
	}
	request, err := http.NewRequestWithContext(env.ctx, http3.MethodGet0RTT, "https://"+testFrontApiName+"/early", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := earlyClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resumed request status %d", response.StatusCode)
	}
	conn := <-conns
	testFrontWait(t, conn.HandshakeComplete(), "the resumed handshake")
	state := conn.ConnectionState()
	if !state.TLS.DidResume {
		t.Fatal("the second connection did not resume the first's session on another socket")
	}
	if state.Used0RTT {
		t.Fatal("the public QUIC config accepted early data")
	}
}

// Signals each ticket the client stores, so a test resumes only with one.
type testFrontSessionCache struct {
	tls.ClientSessionCache
	stored chan struct{}
}

func (self *testFrontSessionCache) Put(sessionKey string, state *tls.ClientSessionState) {
	self.ClientSessionCache.Put(sessionKey, state)
	if state != nil {
		select {
		case self.stored <- struct{}{}:
		default:
		}
	}
}
