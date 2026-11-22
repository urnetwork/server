package connect

// The front's QUIC dispatch, alt's moved into connect (LBALT-IMPL.md §2.8,
// D3, D9, §4.4).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	quic "github.com/urnetwork/quic-go/v2026"
	"github.com/urnetwork/quic-go/v2026/http3"
	"github.com/urnetwork/quic-go/v2026/quicvarint"

	"github.com/urnetwork/server/v2026/nginx"
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
		testFrontInterfaceA + " 127.0.0.1 connect",
		testFrontInterfaceA + " 127.0.0.1 connect",
		testFrontInterfaceB + " ::1 connect",
		testFrontInterfaceB + " ::1 connect",
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

// Every served name that is neither connect's nor the api's, a forwarded
// service's, an lb name and a redirect alias, completes its handshake and is
// closed with 0x1000, and the code arrives: the front waits for the handshake
// before it refuses. A name the front doesn't serve never gets this far
// (TestFrontQuicFailsTheHandshakeOfAnUnknownServerName).
func TestFrontQuicRefusesOtherNamesWithUnknownServerName(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, serverName := range []string{testFrontForwardedName, env.routeTable.Lb.Names[0], testFrontRedirectName} {
		tlsConfig := env.clientTlsConfig(serverName, []string{http3.NextProtoH3})
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

// A name the real nginx front doesn't serve, or no name at all, fails the
// TLS handshake on QUIC, on the quic and whodis sockets, since its QUIC TLS
// config holds no certificate for it, as alt held none (LBALT-IMPL.md §2.8):
// the client reads a TLS alert as a crypto error, never an application
// close, and nothing is dispatched.
func TestFrontQuicFailsTheHandshakeOfAnUnknownServerName(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
		for _, serverName := range []string{testFrontUnknownName, ""} {
			tlsConfig := env.clientTlsConfig(serverName, []string{http3.NextProtoH3})
			// nothing could verify a name the front has no certificate for
			tlsConfig.InsecureSkipVerify = true
			conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, portKind, tlsConfig)
			if err == nil {
				conn.CloseWithError(0, "")
				t.Fatalf("%s %q: the handshake completed", portKind, serverName)
			}
			var transportErr *quic.TransportError
			if !errors.As(err, &transportErr) || !transportErr.Remote || !transportErr.ErrorCode.IsCryptoError() {
				t.Fatalf("%s %q: the dial failed with %v, want the front's TLS alert", portKind, serverName, err)
			}
		}
	}
	select {
	case dispatch := <-env.dispatched:
		t.Fatalf("a failed handshake was dispatched: %+v", dispatch)
	default:
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

// An api connection over the address's cap of api connections is closed
// with 0x1001 before any request, as a connect connection over its own cap
// is, but the two classes are capped apart (LBALTOPT2 R15): an address at its
// api cap still gets its connect connection, and one at its connect cap still
// gets its api connection. Once the admitted api connection ends, its slot
// admits the next.
func TestFrontQuicConnectionCapRefusesApiConnectionsWithRateLimited(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.connectionCap = 1
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	client, _ := env.apiClient(address, frontPortQuic, nil)
	if status, _, err := testFrontGet(t, client, testFrontApiName, "/hello"); err != nil || status != http.StatusOK {
		t.Fatalf("the admitted api request = %d %v", status, err)
	}
	env.nextDispatch()

	// at its api cap: a second api connection is refused
	refused, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if code := testFrontApplicationCode(t, refused, err); code != frontQuicErrorCodeRateLimited {
		t.Fatalf("an api connection over the cap closed with %#x, want %#x", code, frontQuicErrorCodeRateLimited)
	}
	env.nextDispatch()

	// the connect transport has a cap of its own
	connectConn, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if err != nil {
		t.Fatalf("a connect connection with the api cap full: %v", err)
	}
	defer connectConn.CloseWithError(0, "")
	if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassConnect {
		t.Fatalf("connect dispatch = %+v", dispatch)
	}
	env.nextHandled()

	// and is refused only past its own
	overConnect, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if code := testFrontApplicationCode(t, overConnect, err); code != frontQuicErrorCodeRateLimited {
		t.Fatalf("a connect connection over its cap closed with %#x, want %#x", code, frontQuicErrorCodeRateLimited)
	}
	env.nextDispatch()
	select {
	case <-env.connectQuic.handled:
		t.Fatal("an over-cap connection reached connect's handler")
	default:
	}

	// the admitted api connection ends and releases its slot
	client.Transport.(*http3.Transport).Close()
	select {
	case <-env.front.released:
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the admitted api connection's slot was never released")
	}
	next, _ := env.apiClient(address, frontPortQuic, nil)
	if status, _, err := testFrontGet(t, next, testFrontApiName, "/hello"); err != nil || status != http.StatusOK {
		t.Fatalf("the api request after the release = %d %v", status, err)
	}
	apiKey := testFrontInterfaceA + " 127.0.0.1 api"
	connectKey := testFrontInterfaceA + " 127.0.0.1 connect"
	if acquired := env.front.acquired(); !slices.Equal(acquired, []string{apiKey, apiKey, connectKey, connectKey, apiKey}) {
		t.Fatalf("connections charged to %v, want each to eth1's address under its own class", acquired)
	}
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

// Reads and drops every datagram while not delivering, noting a Retry.
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
// limit per socket would have counted them instead. Each group counts the
// Retry it asked for under its interface.
func TestFrontQuicSharesOneHandshakeAdmissionLimit(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.handshakeLimit = newConnectQuicHandshakeLimit(2)
	})
	addressA, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	addressB, _ := env.address(testFrontInterfaceB, frontFamilyIpv6)
	retriesA := frontQuicRetriesCounter.WithLabelValues(testFrontInterfaceA, frontPortQuic)
	retriesB := frontQuicRetriesCounter.WithLabelValues(testFrontInterfaceB, frontPortQuic)
	retriesBeforeA := testutil.ToFloat64(retriesA)
	retriesBeforeB := testutil.ToFloat64(retriesB)

	held := []*testFrontSilentSource{}
	for _, address := range []netip.Addr{addressA, addressB} {
		held = append(held, env.dialSilently(address))
		select {
		case <-env.connectQuic.counted:
		case <-time.After(testFrontProgressTimeout):
			t.Fatalf("the handshake from %s was not counted", address)
		}
	}
	if retries := testutil.ToFloat64(retriesA) + testutil.ToFloat64(retriesB) - retriesBeforeA - retriesBeforeB; retries != 0 {
		t.Fatalf("%v Retries counted under the maximum", retries)
	}
	for _, address := range []netip.Addr{addressA, addressB} {
		source := env.dialSilently(address)
		testFrontWait(t, source.retry, "a Retry at the shared maximum on "+address.String())
	}
	// at least the first Initial of each was refused with a Retry; the
	// source answers none, so its retransmissions may be refused again
	if testutil.ToFloat64(retriesA) <= retriesBeforeA || testutil.ToFloat64(retriesB) <= retriesBeforeB {
		t.Fatalf("Retries counted %v under %s and %v under %s", testutil.ToFloat64(retriesA)-retriesBeforeA, testFrontInterfaceA, testutil.ToFloat64(retriesB)-retriesBeforeB, testFrontInterfaceB)
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

// Prefixes packetConn's datagrams with a PROXY v2 udp-over-ipv4 header from
// source to destination.
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

// Writes the header and the datagram as one datagram.
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
// group's transport reads the bound socket itself, through the group's
// quic.ConnGroup (TestFrontQuicGroupSocketsAreReadAsBound).
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
	if acquired := env.front.acquired(); !slices.Equal(acquired, []string{testFrontInterfaceA + " 127.0.0.1 connect"}) {
		t.Fatalf("connections charged to %v, want only the plain client's", acquired)
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

// tls.ClientSessionCache
func (self *testFrontSessionCache) Put(sessionKey string, state *tls.ClientSessionState) {
	self.ClientSessionCache.Put(sessionKey, state)
	if state != nil {
		select {
		case self.stored <- struct{}{}:
		default:
		}
	}
}

// Every connection draws on connect's receive window budget through the
// public QUIC config, and connect's handler returns the grants only of a
// connection it serves: the front returns those of an api connection, a
// refused one and one connect's closing handler refused, each once closed.
func TestFrontQuicReturnsTheWindowOfConnectionsConnectDidNotServe(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	waitReleased := func(count int) []testFrontReleasedQuic {
		t.Helper()
		deadline := time.After(testFrontProgressTimeout)
		for {
			releasedCount, changed := env.connectQuic.releasedCount.Get()
			if count <= releasedCount {
				return env.connectQuic.released()
			}
			select {
			case <-changed:
			case <-deadline:
				t.Fatalf("%d of %d windows returned", releasedCount, count)
			}
		}
	}

	// served by connect's handler, which returns the window itself
	served, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if err != nil {
		t.Fatal(err)
	}
	env.nextDispatch()
	servedConn := env.nextHandled().conn
	served.CloseWithError(0, "")
	testFrontWait(t, servedConn.Context().Done(), "the served connection's close")

	// an api connection, closed by its client
	client, _ := env.apiClient(address, frontPortQuic, nil)
	if status, _, err := testFrontGet(t, client, testFrontApiName, "/hello"); err != nil || status != http.StatusOK {
		t.Fatalf("api request = %d %v", status, err)
	}
	env.nextDispatch()
	client.Transport.(*http3.Transport).Close()
	waitReleased(1)

	// a refused name
	tlsConfig := env.clientTlsConfig(testFrontForwardedName, []string{http3.NextProtoH3})
	refused, err := env.dialQuicFrom(env.clientPacketConn(address), address, frontPortQuic, tlsConfig)
	testFrontApplicationCode(t, refused, err)
	env.nextDispatch()
	waitReleased(2)

	// a connect name connect's closing handler refused
	env.connectQuic.refusing.Store(true)
	closing, err := env.dialQuic(address, frontPortQuic, testFrontConnectName, nil)
	if err == nil {
		defer closing.CloseWithError(0, "")
	}
	env.nextDispatch()
	released := waitReleased(3)

	if len(released) != 3 {
		t.Fatalf("%d windows returned, want 3", len(released))
	}
	for _, releasedQuic := range released {
		if !releasedQuic.closed {
			t.Fatal("a window was returned while its connection could still draw on it")
		}
		if releasedQuic.conn == servedConn {
			t.Fatal("the front returned the window of a connection connect's handler served")
		}
	}
}

// One accepted QUIC connection's result, as the front counted it.
type testFrontConnectionResult struct {
	interfaceName string
	portKind      string
	result        string
	reason        string
	class         string
}

// The results the env's QUIC connections are counted with.
func observeTestFrontConnectionResults(env *testFrontEnv) chan testFrontConnectionResult {
	results := make(chan testFrontConnectionResult, 64)
	env.settings.connectionResultObserverForTest = func(interfaceName string, portKind string, result string, reason string, class string) {
		select {
		case results <- testFrontConnectionResult{interfaceName: interfaceName, portKind: portKind, result: result, reason: reason, class: class}:
		default:
		}
	}
	return results
}

// The next result a QUIC connection was counted with.
func nextTestFrontConnectionResult(t testing.TB, results chan testFrontConnectionResult) testFrontConnectionResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("no QUIC connection was counted")
		return testFrontConnectionResult{}
	}
}

// A connection is counted with the result the front decided for it: a
// served name the front refuses on QUIC whose handshake never completes is a
// failed handshake, and only one whose handshake completed is refused with
// 0x1000 and counted as an unknown name. The first client refuses the
// handshake only once the front has dispatched its connection, so the front
// decides while the handshake is still under way; its refusal is the
// bad_certificate alert crypto/tls sends for a VerifyConnection error, so the
// failure's reason is certificate.
func TestFrontQuicCountsAnUnknownNameOnceItsHandshakeCompletes(t *testing.T) {
	var results chan testFrontConnectionResult
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		results = observeTestFrontConnectionResults(env)
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	refuse := make(chan struct{})
	tlsConfig := env.clientTlsConfig(testFrontForwardedName, []string{http3.NextProtoH3})
	tlsConfig.VerifyConnection = func(tls.ConnectionState) error {
		<-refuse
		return errors.New("the test's client refuses the handshake")
	}
	dialed := make(chan error, 1)
	go func() {
		conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, frontPortQuic, tlsConfig)
		if err == nil {
			conn.CloseWithError(0, "")
		}
		dialed <- err
	}()
	if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassForwarded {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	close(refuse)
	select {
	case err := <-dialed:
		if err == nil {
			t.Fatal("the refused handshake completed")
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the refusing dial never ended")
	}
	wantFailed := testFrontConnectionResult{interfaceName: testFrontInterfaceA, portKind: frontPortQuic, result: frontQuicResultHandshakeFailed, reason: frontQuicHandshakeReasonCertificate, class: frontQuicHandshakeClassNone}
	if result := nextTestFrontConnectionResult(t, results); result != wantFailed {
		t.Fatalf("a refused name whose handshake failed counted as %+v, want %+v", result, wantFailed)
	}

	tlsConfig = env.clientTlsConfig(testFrontForwardedName, []string{http3.NextProtoH3})
	conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, frontPortQuic, tlsConfig)
	if code := testFrontApplicationCode(t, conn, err); code != frontQuicErrorCodeUnknownServerName {
		t.Fatalf("closed with %#x", code)
	}
	env.nextDispatch()
	wantRefused := testFrontConnectionResult{interfaceName: testFrontInterfaceA, portKind: frontPortQuic, result: frontQuicResultUnknownName, reason: frontQuicResultReasonNone, class: frontQuicHandshakeClassNone}
	if result := nextTestFrontConnectionResult(t, results); result != wantRefused {
		t.Fatalf("a name refused with 0x1000 counted as %+v, want %+v", result, wantRefused)
	}
}

// An idle api connection is closed at nginx's keepalive timeout, as the h3
// server's idle timeout.
func TestFrontH3ApiClosesAnIdleConnectionAtTheKeepaliveTimeout(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.KeepaliveTimeout = 100 * time.Millisecond
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	env.nextDispatch()
	if code := testFrontApplicationCode(t, conn, nil); code != quic.ApplicationErrorCode(http3.ErrCodeNoError) {
		t.Fatalf("the idle api connection closed with %#x", code)
	}
}

// A request stream on an api connection whose HEADERS never arrive is reset
// with H3_REQUEST_INCOMPLETE at the client header timeout, as the tcp
// servers bound a request's headers, and its handler never runs. With no
// stream left, the connection then closes at the keepalive timeout, so the
// front's open count, which the drain waits for, returns to zero. Without
// the bound, the stream would hold the connection, and the drain, for as
// long as its client acks the public config's PINGs.
func TestFrontH3ApiResetsAStreamWhoseHeadersNeverArrive(t *testing.T) {
	var handled atomic.Int64
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.ClientHeaderTimeout = 200 * time.Millisecond
		env.settings.Front.Http.KeepaliveTimeout = 200 * time.Millisecond
		env.front.h3ApiHandler = func(string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handled.Add(1)
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	env.nextDispatch()
	env.waitOpen(1)

	stream, err := conn.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// a HEADERS frame's type and the largest length the header limit allows,
	// and nothing of its field section
	frameHead := quicvarint.Append(nil, 0x1)
	frameHead = quicvarint.Append(frameHead, uint64(env.settings.Front.Http.MaxRequestHeadersTotalBytes))
	if _, err := stream.Write(frameHead); err != nil {
		t.Fatal(err)
	}
	stream.SetReadDeadline(time.Now().Add(testFrontProgressTimeout))
	_, err = stream.Read(make([]byte, 1))
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != quic.StreamErrorCode(http3.ErrCodeRequestIncomplete) {
		t.Fatalf("the stream ended with %v, want a reset with H3_REQUEST_INCOMPLETE", err)
	}
	if code := testFrontApplicationCode(t, conn, nil); code != quic.ApplicationErrorCode(http3.ErrCodeNoError) {
		t.Fatalf("the api connection closed with %#x, want its idle timeout's H3_NO_ERROR", code)
	}
	env.waitOpen(0)
	if handled.Load() != 0 {
		t.Fatal("a request whose headers never arrived reached the api handler")
	}
}

// The h3 api server takes nginx's header buffers as its header limit, which
// its SETTINGS advertise as the largest field section a request may carry.
func TestFrontH3ApiAdvertisesTheHeaderLimit(t *testing.T) {
	const headerBytes = 4 * 1024
	// SETTINGS_MAX_FIELD_SECTION_SIZE (RFC 9114 §7.2.4.1)
	const settingMaxFieldSectionSize = 0x6
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.MaxRequestHeadersTotalBytes = headerBytes
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	env.nextDispatch()

	ctx, cancel := context.WithTimeout(env.ctx, testFrontProgressTimeout)
	defer cancel()
	stream, err := conn.AcceptUniStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.SetReadDeadline(time.Now().Add(testFrontProgressTimeout))
	reader := quicvarint.NewReader(stream)
	// the control stream (type 0x0) opens with its SETTINGS frame (type 0x4)
	if streamType, err := quicvarint.Read(reader); err != nil || streamType != 0x0 {
		t.Fatalf("the server's first stream is of type %d: %v", streamType, err)
	}
	if frameType, err := quicvarint.Read(reader); err != nil || frameType != 0x4 {
		t.Fatalf("the control stream opens with a frame of type %d: %v", frameType, err)
	}
	frameLength, err := quicvarint.Read(reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, frameLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	payloadReader := bytes.NewReader(payload)
	for 0 < payloadReader.Len() {
		id, err := quicvarint.Read(payloadReader)
		if err != nil {
			t.Fatal(err)
		}
		value, err := quicvarint.Read(payloadReader)
		if err != nil {
			t.Fatal(err)
		}
		if id == settingMaxFieldSectionSize {
			if value != headerBytes {
				t.Fatalf("the api server advertises a %d byte field section, want %d", value, headerBytes)
			}
			return
		}
	}
	t.Fatal("the api server advertises no field section limit")
}
