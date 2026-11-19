package connect

// What a public socket of the lb_alt front answers and keeps, with connect's
// real handler and the real nginx front: the source of its udp replies,
// session resumption without early data and across the block's sockets
// (D6c, D9), and the one handshake admission limit behind every socket
// (§4.4.4).

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/server"
)

// The udp endpoints a client's h3 carriers opened, each recording the sources
// of what it read.
type testLbAltSourceConns struct {
	address netip.Addr

	stateLock   sync.Mutex
	sourceConns []*testLbAltSourceConn
}

// H3PacketConnFactory: a socket of the address's family, bound to
// testLbAltClientAddress.
func (self *testLbAltSourceConns) newPacketConn(ctx context.Context) (net.PacketConn, error) {
	packetConn, err := testLbAltListenClientPacket(self.address)
	if err != nil {
		return nil, err
	}
	sourceConn := newTestLbAltSourceConn(packetConn)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sourceConns = append(self.sourceConns, sourceConn)
	return sourceConn, nil
}

// Every source read on any endpoint, with its datagram count.
func (self *testLbAltSourceConns) readSources() map[netip.AddrPort]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sources := map[netip.AddrPort]int{}
	for _, sourceConn := range self.sourceConns {
		for source, count := range sourceConn.readSources() {
			sources[source] += count
		}
	}
	return sources
}

// A whodis exchange works on the whodis socket of every interface address,
// in both families, and every datagram the client's own dns carrier reads
// comes from exactly the address and port it sent to: the interface's bridge
// address and the whodis port. That is what conntrack needs to restore the
// public 53 or 4053 of the DNAT on each reply (§4.1); in process there is no
// DNAT, and both public ports reach this one socket. A socket bound to a
// wildcard would answer from whichever source routing picks, on Linux
// 127.0.0.1 for a client that dialed 127.0.0.2. The quic socket answers the
// same way. The resident sees each client at the address its socket binds
// (testLbAltClientAddress), not at the interface's.
func TestLbAltUdpRepliesComeFromTheAddressAndPortTheClientDialed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		interfaceAddresses := env.interfaceAddresses()
		for i, interfaceAddress := range interfaceAddresses {
			// a peer on the websocket, on another interface
			otherAddress := interfaceAddresses[(i+1)%len(interfaceAddresses)].address
			for _, c := range []struct {
				carrier  testLbAltCarrier
				portKind string
			}{
				{carrier: testLbAltCarrierWhodis, portKind: frontPortWhodis},
				{carrier: testLbAltCarrierQuic, portKind: frontPortQuic},
			} {
				sourceConns := &testLbAltSourceConns{
					address: interfaceAddress.address,
				}
				client := env.newClient(env.newPeer("udp"), c.carrier, &testLbAltDial{
					address:           interfaceAddress.address,
					packetConnFactory: sourceConns.newPacketConn,
				})
				other := env.newClient(env.newPeer("other"), testLbAltCarrierWebSocket, &testLbAltDial{address: otherAddress})
				if edge := env.waitResidentEdge(client.peer.clientId); edge.Carrier != c.carrier.residentCarrier || edge.ClientIp != testLbAltClientAddress(interfaceAddress.address) {
					t.Fatalf("%s %s: edge %+v, want the client at %s", interfaceAddress.address, c.carrier.name, edge, testLbAltClientAddress(interfaceAddress.address))
				}
				testLbAltExchangeMessages(t, client, other, 4)

				bindAddress := env.bindAddress(interfaceAddress.address, c.portKind)
				sources := sourceConns.readSources()
				if len(sources) != 1 || sources[bindAddress] == 0 {
					t.Fatalf("%s %s: replies came from %v, want only %s", interfaceAddress.address, c.carrier.name, sources, bindAddress)
				}
				client.close()
				other.close()
			}
		}
	})
}

// Signals each ticket the client stores, so a test resumes only with one.
type testLbAltSessionCache struct {
	tls.ClientSessionCache
	stored chan struct{}
}

// A cache of a few sessions.
func newTestLbAltSessionCache() *testLbAltSessionCache {
	return &testLbAltSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(8),
		stored:             make(chan struct{}, 64),
	}
}

// tls.ClientSessionCache
func (self *testLbAltSessionCache) Put(sessionKey string, state *tls.ClientSessionState) {
	self.ClientSessionCache.Put(sessionKey, state)
	if state != nil {
		select {
		case self.stored <- struct{}{}:
		default:
		}
	}
}

// Drops the signals of tickets stored so far.
func (self *testLbAltSessionCache) drain() {
	for {
		select {
		case <-self.stored:
		default:
			return
		}
	}
}

// A ticket only resumes (D9): the public QUIC config refuses early data
// though connect's own allows it, so a client that dials with 0-RTT on a
// ticket from the front resumes and reports Used0RTT false, for a connect
// name without ALPN and for an api name with h3, on the quic and the whodis
// sockets.
func TestLbAltQuicResumptionReportsNoEarlyData(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
			for _, c := range []struct {
				serverName string
				nextProtos []string
			}{
				{serverName: testLbAltConnectName, nextProtos: nil},
				{serverName: testLbAltApiName, nextProtos: []string{http3.NextProtoH3}},
			} {
				sessionCache := newTestLbAltSessionCache()
				tlsConfig := env.clientTlsConfig(c.serverName, c.nextProtos)
				tlsConfig.ClientSessionCache = sessionCache

				first, err := env.dialQuicFrom(env.clientCarrierConn(env.clientPacketConn(address), portKind), address, portKind, tlsConfig, testLbAltClientQuicConfig(), false)
				if err != nil {
					t.Fatalf("%s %s: %v", portKind, c.serverName, err)
				}
				testLbAltWait(t, sessionCache.stored, "a session ticket")
				first.CloseWithError(0, "")

				resumed, err := env.dialQuicFrom(env.clientCarrierConn(env.clientPacketConn(address), portKind), address, portKind, tlsConfig, testLbAltClientQuicConfig(), true)
				if err != nil {
					t.Fatalf("%s %s resumed: %v", portKind, c.serverName, err)
				}
				testLbAltWait(t, resumed.HandshakeComplete(), "the resumed handshake")
				state := resumed.ConnectionState()
				if !state.TLS.DidResume {
					t.Fatalf("%s %s: the second connection did not resume", portKind, c.serverName)
				}
				if state.Used0RTT {
					t.Fatalf("%s %s: the public QUIC config accepted early data", portKind, c.serverName)
				}
				resumed.CloseWithError(0, "")
			}
		}
	})
}

// Sessions resume across the block's sockets (D6c): every socket of a group
// serves the front's one long-lived TLS config, so a ticket one socket issued
// resumes on the next, over TCP and over QUIC. Each step leaves exactly one
// socket of the four open, its siblings lost with their re-creation held, so
// whatever the kernel's SO_REUSEPORT policy it delivers to that one. A config
// cloned per socket would generate its own ticket keys, and the resumption
// would fall back to a full handshake.
func TestLbAltSessionsResumeAcrossTheBlocksSockets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			frontSettings: func(settings *FrontServerSettings) {
				// a socket lost again is re-created after this, so the steps
				// below wait no longer than a moment for each re-creation
				settings.SocketRestartInitialDelay = time.Millisecond
			},
		})
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		socketCount := len(env.frontServer.sockets)
		socketsPerPort := env.settings.SocketsPerPort
		lbName := env.routeTable.Lb.Names[0]
		statusPath := env.routeTable.Lb.RoutePrefixes[0] + "/status"

		for _, protocol := range []string{frontProtocolTcp, frontProtocolUdp} {
			portKind := frontPortHttps
			if protocol == frontProtocolUdp {
				portKind = frontPortQuic
			}
			bindAddress := env.bindAddress(address, portKind)
			// the group's sockets in bind order, and closes one
			boundCloses := func() []func() error {
				closes := []func() error{}
				if protocol == frontProtocolTcp {
					for _, listener := range env.binds.boundListeners(bindAddress) {
						closes = append(closes, listener.Close)
					}
				} else {
					for _, packetConn := range env.binds.boundPacketConns(bindAddress) {
						closes = append(closes, packetConn.Close)
					}
				}
				return closes
			}
			expectOneOpen := func(step int) {
				t.Helper()
				_, header := env.status()
				entry := env.socketsEntry(header, protocol, address, env.port(portKind))
				if want := fmt.Sprintf("%s/%s/%d=1/%d", protocol, address, env.port(portKind), socketsPerPort); entry != want {
					t.Fatalf("%s step %d: the group reads %q, want %q", protocol, step, entry, want)
				}
			}
			sessionCache := newTestLbAltSessionCache()
			// one connection on the group's one open socket, and whether it
			// resumed
			connect := func(step int) bool {
				t.Helper()
				if protocol == frontProtocolTcp {
					tlsConfig := env.clientTlsConfig(lbName, []string{"http/1.1"})
					tlsConfig.ClientSessionCache = sessionCache
					transport := &http.Transport{
						TLSClientConfig:   tlsConfig,
						DisableKeepAlives: true,
						DialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
							dialer := &net.Dialer{}
							return dialer.DialContext(ctx, "tcp", bindAddress.String())
						},
					}
					defer transport.CloseIdleConnections()
					client := &http.Client{
						Transport: transport,
						Timeout:   testLbAltProgressTimeout,
					}
					response, err := client.Get("https://" + lbName + statusPath)
					if err != nil {
						t.Fatalf("tcp step %d: %v", step, err)
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("tcp step %d: status %d", step, response.StatusCode)
					}
					return response.TLS.DidResume
				}
				tlsConfig := env.clientTlsConfig(testLbAltConnectName, nil)
				tlsConfig.ClientSessionCache = sessionCache
				conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, portKind, tlsConfig, testLbAltClientQuicConfig(), false)
				if err != nil {
					t.Fatalf("udp step %d: %v", step, err)
				}
				defer conn.CloseWithError(0, "")
				// the ticket follows the handshake, so the connection stays
				// open until it is stored
				testLbAltWait(t, sessionCache.stored, "a session ticket")
				return conn.ConnectionState().TLS.DidResume
			}

			hold := env.binds.hold(protocol, bindAddress)
			// a failed step leaves no supervisor waiting at the barrier
			defer hold.releaseAll()
			closes := boundCloses()
			if len(closes) != socketsPerPort {
				t.Fatalf("%s: %d sockets bound on %s", protocol, len(closes), bindAddress)
			}
			// every socket but the first is lost, and waits at the barrier
			for _, closeSocket := range closes[1:] {
				closeSocket()
			}
			hold.waitWaiting(t, socketsPerPort-1)
			env.waitSocketsUp(socketCount - (socketsPerPort - 1))
			expectOneOpen(0)
			sessionCache.drain()
			if connect(0) {
				t.Fatalf("%s: the first connection resumed", protocol)
			}
			if protocol == frontProtocolTcp {
				testLbAltWait(t, sessionCache.stored, "a session ticket")
			}

			openClose := closes[0]
			for step := 1; step < socketsPerPort; step += 1 {
				// one held re-creation completes, and the socket open before
				// it is lost in turn, so the new socket is the group's only one
				boundCount := len(boundCloses())
				hold.releaseOne()
				env.waitSocketsUp(socketCount - (socketsPerPort - 1) + 1)
				nextCloses := boundCloses()
				if len(nextCloses) != boundCount+1 {
					t.Fatalf("%s step %d: %d binds, want %d", protocol, step, len(nextCloses), boundCount+1)
				}
				openClose()
				openClose = nextCloses[len(nextCloses)-1]
				hold.waitWaiting(t, socketsPerPort-1)
				env.waitSocketsUp(socketCount - (socketsPerPort - 1))
				expectOneOpen(step)
				sessionCache.drain()
				if !connect(step) {
					t.Fatalf("%s step %d: the ticket from the previous socket did not resume on this one", protocol, step)
				}
			}
			hold.releaseAll()
			env.waitSocketsUp(socketCount)
		}
	})
}

// A client socket a test controls: quic-go reads nothing from it, which is
// all that makes a source spoofed to the server. retry closes on the first
// Retry it is sent, and replied on the first other packet, the server's
// handshake flight, which the server sends only after it created, and
// counted, the connection. Under whodis it sits above the client's dns
// translation, where the packets are QUIC.
type testLbAltSilentSource struct {
	net.PacketConn
	retry       chan struct{}
	retryOnce   sync.Once
	replied     chan struct{}
	repliedOnce sync.Once
}

// Reads and drops every datagram, noting a Retry and a handshake flight.
func (self *testLbAltSilentSource) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, _, err := self.PacketConn.ReadFrom(p)
		if err != nil {
			return n, nil, err
		}
		if n == 0 {
			continue
		}
		// a QUIC v1 Retry: a long header, the fixed bit and type 3
		if p[0]&0xf0 == 0xf0 {
			self.retryOnce.Do(func() {
				close(self.retry)
			})
		} else {
			self.repliedOnce.Do(func() {
				close(self.replied)
			})
		}
	}
}

// Dials connect's name on a port kind's socket from a source that receives
// nothing, so its handshake never completes. The dial ends with the test.
func (self *testLbAltEnv) dialSilently(address netip.Addr, portKind string) *testLbAltSilentSource {
	source := &testLbAltSilentSource{
		PacketConn: self.clientCarrierConn(self.clientPacketConn(address), portKind),
		retry:      make(chan struct{}),
		replied:    make(chan struct{}),
	}
	quicConfig := testLbAltClientQuicConfig()
	quicConfig.Versions = []quic.Version{quic.Version1}
	tlsConfig := self.clientTlsConfig(testLbAltConnectName, nil)
	go self.dialQuicFrom(source, address, portKind, tlsConfig, quicConfig, false)
	return source
}

// One handshake admission limit stands behind every public socket (§4.4.4):
// with connect's handler allowing two unvalidated handshakes, two silent
// sources, one on an interface's quic socket and one on another interface's
// whodis socket, hold it; new silent sources on the two other sockets are
// sent a Retry, which keeps no state; and a real client, which answers the
// Retry, still connects. A limit per socket would have admitted each new
// source's handshake instead.
func TestLbAltHandshakeAdmissionLimitIsSharedByEverySocket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			exchangeSettings: func(settings *ExchangeSettings) {
				settings.H3MaxUnvalidatedHandshakeCount = 2
				// the held handshakes outlast the test
				settings.QuicConnectTimeout = testLbAltProgressTimeout
				settings.QuicHandshakeTimeout = testLbAltProgressTimeout
			},
		})
		addressA, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		addressB, _ := env.address(testLbAltInterfaceB, frontFamilyIpv6)
		handshakeLimit := env.connectRouter.ConnectHandler().quicHandshakeLimit

		held := []*testLbAltSilentSource{}
		for _, c := range []struct {
			address  netip.Addr
			portKind string
		}{
			{address: addressA, portKind: frontPortQuic},
			{address: addressB, portKind: frontPortWhodis},
		} {
			source := env.dialSilently(c.address, c.portKind)
			testLbAltWait(t, source.replied, fmt.Sprintf("the handshake flight on %s %s", c.address, c.portKind))
			held = append(held, source)
		}
		if count := handshakeLimit.count.Load(); count != 2 {
			t.Fatalf("the limit counts %d handshakes, want the two held", count)
		}
		for _, c := range []struct {
			address  netip.Addr
			portKind string
		}{
			{address: addressA, portKind: frontPortWhodis},
			{address: addressB, portKind: frontPortQuic},
		} {
			source := env.dialSilently(c.address, c.portKind)
			testLbAltWait(t, source.retry, fmt.Sprintf("a Retry at the shared maximum on %s %s", c.address, c.portKind))
		}
		for _, source := range held {
			select {
			case <-source.retry:
				t.Fatal("a source under the maximum was sent a Retry")
			default:
			}
		}
		if count := handshakeLimit.count.Load(); count != 2 {
			t.Fatalf("the limit counts %d handshakes past its maximum", count)
		}

		client := env.newClient(env.newPeer("validated"), testLbAltCarrierQuic, &testLbAltDial{address: addressB})
		if edge := env.waitResidentEdge(client.peer.clientId); edge.Carrier != ResidentCarrierH3 {
			t.Fatalf("the validated client arrived as carrier %d", edge.Carrier)
		}
	})
}
