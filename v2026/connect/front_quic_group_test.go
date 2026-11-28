package connect

// One QUIC transport per udp group (D22 (b), LBALT-IMPL.md §8): a flow that
// moves between a group's sockets keeps its connection, through a socket's
// loss and re-creation and through a NAT rebinding, on the quic and the
// whodis ports; a group's transport that ends is replaced; and a close still
// sends each api connection's close through the group's sockets.
//
// darwin delivers a reuseport group's datagrams to its newest socket, where
// Linux hashes each flow to one, so the tests move a flow deterministically:
// a group narrowed to one socket whose loss leaves only siblings, or sockets
// bound to distinct ports behind a relay that a test steers.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/urnetwork/quic-go/v2026"
	"github.com/urnetwork/quic-go/v2026/http3"
)

// An h3 api client of the front through dial, every connection it makes
// recorded on conns.
func (self *testFrontEnv) apiClientDialing(dial func(tlsConfig *tls.Config) (*quic.Conn, error)) (*http.Client, chan *quic.Conn) {
	conns := make(chan *quic.Conn, 16)
	roundTripper := &http3.Transport{
		TLSClientConfig: self.clientTlsConfig(testFrontApiName, nil),
		QUICConfig:      testFrontClientQuicConfig(),
		Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			conn, err := dial(tlsConfig)
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

// GETs path through an api client of interfaceName, failing the test unless
// the api handler answers.
func testFrontGetApi(t testing.TB, client *http.Client, interfaceName string, path string) {
	t.Helper()
	status, body, err := testFrontGet(t, client, testFrontApiName, path)
	if err != nil || status != http.StatusOK || body != "api "+interfaceName+" "+path {
		t.Fatalf("GET %s: %d %q %v", path, status, body, err)
	}
}

// Fails unless the client made exactly one connection.
func testFrontExpectOneConnection(t testing.TB, conns chan *quic.Conn) *quic.Conn {
	t.Helper()
	var conn *quic.Conn
	select {
	case conn = <-conns:
	default:
		t.Fatal("the client made no connection")
	}
	select {
	case <-conns:
		t.Fatal("the client connected again")
	default:
	}
	return conn
}

// Each udp group, an interface address's quic or whodis sockets, feeds one
// transport: the quic port's reads the group's quic.ConnGroup, and the whodis
// port's reads one translation over the group's fan-in, never a bound socket
// of its own. With a transport per socket, there were SocketsPerPort of each.
func TestFrontQuicGroupsFeedOneTransportEach(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	env.waitSocketsUp(len(env.frontServer.sockets))
	groupCount := 0
	boundConns := []net.PacketConn{}
	for _, socket := range env.frontServer.sockets {
		if socket.key.protocol != frontProtocolUdp {
			continue
		}
		if socket.key.index == 0 {
			groupCount += 1
		}
		boundConns = append(boundConns, env.binds.boundPacketConns(socket.key.bindAddress())...)
	}
	transportConns := env.connectQuic.builtTransportConns()
	if groupCount == 0 || len(transportConns) != groupCount {
		t.Fatalf("%d transports for %d udp groups", len(transportConns), groupCount)
	}
	connGroupCount := 0
	for _, transportConn := range transportConns {
		if slices.Contains(boundConns, transportConn) {
			t.Fatalf("a transport reads one bound socket, %s", transportConn.LocalAddr())
		}
		if _, ok := transportConn.(*quic.ConnGroup); ok {
			connGroupCount += 1
		}
	}
	if connGroupCount != groupCount/2 {
		t.Fatalf("%d transports read a ConnGroup, want the %d quic groups'", connGroupCount, groupCount/2)
	}
}

// An api connection whose socket is lost keeps working through the group's
// other sockets, on the quic and the whodis ports: the group is narrowed to
// one socket, the connection made on it, the others brought back, and then
// the connection's socket lost, so its next datagrams reach a sibling, or
// the socket's re-creation, whichever the platform delivers to. The sibling
// feeds the same transport, so the connection's next request is answered on
// the same connection; with a transport per socket it went unanswered until
// the client gave up.
func TestFrontQuicKeepsAConnectionThroughTheLossOfItsSocket(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Millisecond
	})
	socketCount := len(env.frontServer.sockets)
	socketsPerPort := env.settings.SocketsPerPort
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
		bindAddress := env.bindAddress(address, portKind)
		packetConns := env.binds.boundPacketConns(bindAddress)
		if len(packetConns) != socketsPerPort {
			t.Fatalf("%s: %d sockets bound", portKind, len(packetConns))
		}

		// narrowed to the first socket
		started, release := env.binds.holdNext(bindAddress)
		for _, packetConn := range packetConns[1:] {
			packetConn.Close()
		}
		testFrontWait(t, started, portKind+": a sibling's re-creation")
		env.waitSocketsUp(socketCount - (socketsPerPort - 1))
		client, conns := env.apiClient(address, portKind, nil)
		testFrontGetApi(t, client, testFrontInterfaceA, "/on-its-socket")

		// the siblings back, and the connection's socket lost
		release()
		env.binds.waitPacketConns(t, bindAddress, 2*socketsPerPort-1)
		env.waitSocketsUp(socketCount)
		packetConns[0].Close()
		env.binds.waitPacketConns(t, bindAddress, 2*socketsPerPort)
		env.waitSocketsUp(socketCount)

		testFrontGetApi(t, client, testFrontInterfaceA, "/after-its-socket-was-lost")
		testFrontExpectOneConnection(t, conns)
	}
}

// A relay standing in for the kernel's choice of socket, and for a NAT: a
// client sends to the relay, which forwards each datagram from its upstream
// socket to the socket it is steered to; whatever reaches the upstream
// socket, from any socket of the group, goes back to the client. A rebinding
// replaces the upstream socket, as a NAT rebinding changes a client's source
// port, and forgets the old one.
type testFrontRelay struct {
	t          testing.TB
	clientSide *net.UDPConn
	clientAddr atomic.Pointer[net.UDPAddr]
	target     atomic.Pointer[net.UDPAddr]
	upstream   atomic.Pointer[net.UDPConn]
}

// A relay on loopback forwarding to target.
func newTestFrontRelay(t testing.TB, target net.Addr) *testFrontRelay {
	relay := &testFrontRelay{
		t:          t,
		clientSide: newTestFrontRelaySocket(t),
	}
	relay.steer(target)
	relay.rebind()
	go relay.forwardFromClient()
	return relay
}

// A udp socket on loopback, closed with the test.
func newTestFrontRelaySocket(t testing.TB) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Forwards the client's next datagrams to target.
func (self *testFrontRelay) steer(target net.Addr) {
	self.target.Store(target.(*net.UDPAddr))
}

// Forwards the client's next datagrams from a new upstream socket, and closes
// the last one.
func (self *testFrontRelay) rebind() {
	upstream := newTestFrontRelaySocket(self.t)
	if previous := self.upstream.Swap(upstream); previous != nil {
		previous.Close()
	}
	go self.forwardToClient(upstream)
}

func (self *testFrontRelay) forwardFromClient() {
	b := make([]byte, 64*1024)
	for {
		n, addr, err := self.clientSide.ReadFromUDP(b)
		if err != nil {
			return
		}
		self.clientAddr.Store(addr)
		self.upstream.Load().WriteToUDP(b[:n], self.target.Load())
	}
}

func (self *testFrontRelay) forwardToClient(upstream *net.UDPConn) {
	b := make([]byte, 64*1024)
	for {
		n, _, err := upstream.ReadFromUDP(b)
		if err != nil {
			return
		}
		if clientAddr := self.clientAddr.Load(); clientAddr != nil {
			self.clientSide.WriteToUDP(b[:n], clientAddr)
		}
	}
}

// An api connection keeps working when its flow moves to another socket of
// its group, as the kernel moves flows when a reuseport group's size changes,
// and when a NAT rebinding also changes its source port, on the quic and the
// whodis ports. The group's sockets are bound to distinct ports here, behind
// a relay the test steers, so the flow moves when the test says. With a
// transport per socket, the moved flow reached a transport that didn't hold
// its connection.
func TestFrontQuicKeepsAConnectionWhoseFlowMovesToASiblingSocket(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
		for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
			env.binds.bindEphemerally(env.bindAddress(address, portKind))
		}
	})
	env.waitSocketsUp(len(env.frontServer.sockets))
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
		packetConns := env.binds.boundPacketConns(env.bindAddress(address, portKind))
		if len(packetConns) != env.settings.SocketsPerPort {
			t.Fatalf("%s: %d sockets bound", portKind, len(packetConns))
		}
		relay := newTestFrontRelay(t, packetConns[0].LocalAddr())
		target := relay.clientSide.LocalAddr().(*net.UDPAddr)
		client, conns := env.apiClientDialing(func(tlsConfig *tls.Config) (*quic.Conn, error) {
			return env.dialQuicTo(env.clientPacketConn(address), target, portKind, tlsConfig)
		})
		testFrontGetApi(t, client, testFrontInterfaceA, "/on-the-first-socket")

		relay.steer(packetConns[len(packetConns)-1].LocalAddr())
		testFrontGetApi(t, client, testFrontInterfaceA, "/moved-to-the-last-socket")

		relay.rebind()
		relay.steer(packetConns[1].LocalAddr())
		testFrontGetApi(t, client, testFrontInterfaceA, "/rebound-on-the-second-socket")
		testFrontExpectOneConnection(t, conns)
	}
}

// The forced close sends each api connection's close through its group's
// sockets: the h3 servers close before their transport, and the transport
// before the sockets, so the client reads the server's H3_NO_ERROR at once,
// where with the sockets gone first it would wait out its idle timeout.
func TestFrontCloseSendsEachApiConnectionsCloseThroughItsSockets(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conns := []*quic.Conn{}
	for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
		conn, err := env.dialQuic(address, portKind, testFrontApiName, []string{http3.NextProtoH3})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		env.nextDispatch()
		conns = append(conns, conn)
	}
	env.waitOpen(len(conns))

	env.frontServer.close()
	for _, conn := range conns {
		if code := testFrontApplicationCode(t, conn, nil); code != quic.ApplicationErrorCode(http3.ErrCodeNoError) {
			t.Fatalf("the api connection was closed with %#x, want H3_NO_ERROR", code)
		}
	}
}

// A QUIC socket lost while the drain shuts its api connections down
// gracefully costs them nothing (D22 (b)): the group's other sockets feed the
// same transport, so a request in flight on a connection whose socket is lost
// still finishes, and its connection gets one GOAWAY. The group is narrowed to
// one socket while the connection is made, so the lost socket is the
// connection's whatever the platform's reuseport delivery; nothing is
// re-created once the drain began. With a transport per socket, the loss
// destroyed the connection and its request.
func TestFrontDrainKeepsApiConnectionsThroughTheLossOfTheirSocket(t *testing.T) {
	inFlight := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	var releaseHandlerOnce sync.Once
	defer releaseHandlerOnce.Do(func() { close(releaseHandler) })
	trace := newTestFrontConnectionTrace(false)
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.tracer = trace.tracer
		env.front.h3ApiHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inFlight <- struct{}{}
				<-releaseHandler
				fmt.Fprintf(w, "api %s %s", interfaceName, r.URL.Path)
			})
		}
	})
	socketCount := len(env.frontServer.sockets)
	socketsPerPort := env.settings.SocketsPerPort
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortQuic)
	packetConns := env.binds.boundPacketConns(bindAddress)

	// the connection made on the group's one socket
	started, release := env.binds.holdNext(bindAddress)
	defer release()
	for _, packetConn := range packetConns[1:] {
		packetConn.Close()
	}
	testFrontWait(t, started, "a sibling's re-creation")
	env.waitSocketsUp(socketCount - (socketsPerPort - 1))
	client, conns := env.apiClient(address, frontPortQuic, nil)
	type testFrontResponse struct {
		status int
		body   string
		err    error
	}
	responses := make(chan testFrontResponse, 1)
	go func() {
		status, body, err := testFrontGet(t, client, testFrontApiName, "/in-flight")
		responses <- testFrontResponse{status: status, body: body, err: err}
	}()
	testFrontWait(t, inFlight, "the api request")

	// the siblings back, the drain begun, and the connection's socket lost
	release()
	env.binds.waitPacketConns(t, bindAddress, 2*socketsPerPort-1)
	env.waitSocketsUp(socketCount)
	env.frontServer.beginDrain()
	trace.waitGoaways(t, 1)
	packetConns[0].Close()
	env.waitSocketsUp(socketCount - 1 - countTcpSockets(env))

	releaseHandlerOnce.Do(func() { close(releaseHandler) })
	select {
	case response := <-responses:
		if response.err != nil || response.status != http.StatusOK || response.body != "api "+testFrontInterfaceA+" /in-flight" {
			t.Fatalf("the request in flight across the loss: %d %q %v", response.status, response.body, response.err)
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the request in flight never finished")
	}
	testFrontExpectOneConnection(t, conns)
	if goaways := trace.goaways.Value(); goaways != 1 {
		t.Fatalf("%d GOAWAYs for one api connection", goaways)
	}
	if count := len(env.binds.boundPacketConns(bindAddress)); count != 2*socketsPerPort-1 {
		t.Fatalf("%d udp binds on %s: a socket lost during the drain was re-created", count, bindAddress)
	}
}
