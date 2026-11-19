package connect

// The lb_alt front's public udp sockets under loss and failure, with real
// clients and a real exchange (LBALT.md D13, D22, §4.4.4, §9 "Carriers"):
// a lost socket is rebuilt on its own while its siblings serve, a socket
// survives datagrams it can't write, and a sibling never resets a connection
// it doesn't hold.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"syscall"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/server"
)

// The number of times each socket of the group the front holds on address
// for a port kind was bound, the re-creations included.
func (self *testLbAltEnv) groupBindCount(address netip.Addr, portKind string) int {
	bindAddress := self.bindAddress(address, portKind)
	if portKind == frontPortHttp || portKind == frontPortHttps {
		return len(self.binds.boundListeners(bindAddress))
	}
	return len(self.binds.boundPacketConns(bindAddress))
}

// Fails the test unless readiness reads ready, with every group whole.
func (self *testLbAltEnv) expectReady(what string) {
	self.t.Helper()
	code, header := self.status()
	if code != http.StatusOK || header.Get(frontReadyHeader) != "1" {
		self.t.Fatalf("%s: status %d, %s %q, %s %q", what, code, frontReadyHeader, header.Get(frontReadyHeader), frontSocketsHeader, header.Get(frontSocketsHeader))
	}
}

// A public udp socket lost while serving is rebuilt on its own, as the
// extender rebuilds a lost udp carrier (TestExtenderRebuildsALostUdpCarrier,
// ../connect/extender): the socket's reads fail, and until its re-creation
// completes, held here, readiness shows its group one socket short and the
// front not ready (D22), while the carriers on every sibling socket keep
// serving real clients both ways, and a new client connects through the
// group's other sockets. Losing it never ends the process. Once it is back,
// readiness is whole again, and only that socket was bound again. The quic
// and the whodis sockets each in turn.
func TestLbAltRebuildsALostUdpSocket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			failableUdp: true,
		})
		lostAddress, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		siblingAddress, _ := env.address(testLbAltInterfaceB, frontFamilyIpv6)
		socketCount := len(env.frontServer.sockets)
		socketsPerPort := env.settings.SocketsPerPort

		// established before any loss: a websocket on the lost socket's
		// interface, and both udp carriers on the other interface
		websocketClient := env.newClient(env.newPeer("websocket"), testLbAltCarrierWebSocket, &testLbAltDial{address: lostAddress})
		quicClient := env.newClient(env.newPeer("quic"), testLbAltCarrierQuic, &testLbAltDial{address: siblingAddress})
		whodisClient := env.newClient(env.newPeer("whodis"), testLbAltCarrierWhodis, &testLbAltDial{address: siblingAddress})
		testLbAltExchangeMessages(t, websocketClient, quicClient, 4)
		testLbAltExchangeMessages(t, quicClient, whodisClient, 4)
		env.expectReady("before the loss")

		for _, c := range []struct {
			portKind string
			carrier  testLbAltCarrier
		}{
			{portKind: frontPortQuic, carrier: testLbAltCarrierQuic},
			{portKind: frontPortWhodis, carrier: testLbAltCarrierWhodis},
		} {
			bindAddress := env.bindAddress(lostAddress, c.portKind)
			bindCounts := map[string]int{}
			for _, portKind := range []string{frontPortHttp, frontPortHttps, frontPortQuic, frontPortWhodis} {
				bindCounts[portKind] = env.groupBindCount(lostAddress, portKind)
			}
			hold := env.binds.hold(frontProtocolUdp, bindAddress)
			defer hold.releaseAll()
			failingConns := env.binds.boundFailingConns(bindAddress)
			// the first socket of the group: on darwin the kernel delivers to
			// the newest, so the clients' flows are elsewhere either way
			failingConns[len(failingConns)-socketsPerPort].failReads(errTestLbAltSocket)

			hold.waitWaiting(t, 1)
			env.waitSocketsUp(socketCount - 1)
			code, header := env.status()
			if code != http.StatusOK || header.Get(frontReadyHeader) != "0" {
				t.Fatalf("%s lost: status %d, %s %q", c.portKind, code, frontReadyHeader, header.Get(frontReadyHeader))
			}
			if entry, want := env.socketsEntry(header, frontProtocolUdp, lostAddress, env.port(c.portKind)), fmt.Sprintf("udp/%s/%d=%d/%d", lostAddress, env.port(c.portKind), socketsPerPort-1, socketsPerPort); entry != want {
				t.Fatalf("%s lost: the group reads %q, want %q", c.portKind, entry, want)
			}

			// the siblings serve: the established clients, both ways, and a
			// new client on the group's other sockets
			testLbAltExchangeMessages(t, websocketClient, quicClient, 4)
			testLbAltExchangeMessages(t, whodisClient, websocketClient, 4)
			newClient := env.newClient(env.newPeer("new"), c.carrier, &testLbAltDial{address: lostAddress})
			if edge := env.waitResidentEdge(newClient.peer.clientId); edge.Carrier != c.carrier.residentCarrier {
				t.Fatalf("%s lost: the new client arrived as carrier %d", c.portKind, edge.Carrier)
			}
			testLbAltExchangeMessages(t, newClient, quicClient, 4)
			if err := env.frontServer.ctx.Err(); err != nil {
				t.Fatalf("%s lost: the front ended: %v", c.portKind, err)
			}

			hold.releaseAll()
			env.waitSocketsUp(socketCount)
			env.expectReady(c.portKind + " back")
			for portKind, bindCount := range bindCounts {
				want := bindCount
				if portKind == c.portKind {
					want += 1
				}
				if got := env.groupBindCount(lostAddress, portKind); got != want {
					t.Fatalf("%s lost: %s binds %d, want %d", c.portKind, portKind, got, want)
				}
			}
			testLbAltExchangeMessages(t, newClient, whodisClient, 4)
			newClient.close()
		}
	})
}

// A public udp socket survives datagrams it can't write, as alt's did after
// connect 6203a4e8 (transport_pt_server_failure_test.go, carried over; LBALT.md
// §4.4.4): a socket that refuses a write for one address or for a moment,
// with the errors a socket answers, costs those datagrams and nothing more.
// Under whodis the dns translation drops each one and the client's QUIC
// connection recovers it, so the client never reconnects; on the quic socket
// quic-go ends at most the connection that wrote, and the client reconnects.
// Neither socket is lost or re-created, readiness stays whole, and every
// client keeps exchanging messages both ways.
func TestLbAltUdpSocketsSurviveDatagramsTheyCannotWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			failableUdp: true,
		})
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		peerAddress, _ := env.address(testLbAltInterfaceB, frontFamilyIpv6)
		socketCount := len(env.frontServer.sockets)
		writeErrs := []error{
			syscall.EINVAL,
			syscall.EPERM,
			syscall.ENETUNREACH,
			syscall.EHOSTUNREACH,
			syscall.ENOBUFS,
			errors.New("a failure this does not know"),
		}

		peer := env.newClient(env.newPeer("peer"), testLbAltCarrierWebSocket, &testLbAltDial{address: peerAddress})
		for _, c := range []struct {
			portKind string
			carrier  testLbAltCarrier
		}{
			{portKind: frontPortWhodis, carrier: testLbAltCarrierWhodis},
			{portKind: frontPortQuic, carrier: testLbAltCarrierQuic},
		} {
			client := env.newClient(env.newPeer(c.carrier.name), c.carrier, &testLbAltDial{address: address})
			testLbAltExchangeMessages(t, client, peer, 4)
			for len(env.quicResults) != 0 {
				env.nextQuicResult()
			}
			bindCount := env.groupBindCount(address, c.portKind)

			// whichever socket of the group carries the client's flow fails
			// its next writes
			failingConns := env.binds.boundFailingConns(env.bindAddress(address, c.portKind))
			failedCount := env.binds.failedWriteCount.Value()
			for _, failingConn := range failingConns {
				failingConn.failWrites(writeErrs...)
			}
			// the client's traffic makes the front write, until every queued
			// failure was answered
			deadline := time.Now().Add(testLbAltProgressTimeout)
			for {
				testLbAltExchangeMessages(t, client, peer, 4)
				count := env.binds.failedWriteCount.Value()
				if failedCount+len(writeErrs) <= count {
					break
				}
				if deadline.Before(time.Now()) {
					t.Fatalf("%s: %d of %d queued writes failed", c.portKind, count-failedCount, len(writeErrs))
				}
			}
			for _, failingConn := range failingConns {
				failingConn.clearWriteFailures()
			}

			testLbAltExchangeMessages(t, client, peer, 4)
			if got := env.groupBindCount(address, c.portKind); got != bindCount {
				t.Fatalf("%s: %d binds after the failed writes, want %d: the socket was lost", c.portKind, got, bindCount)
			}
			if upCount := env.frontServer.upCount.Value(); upCount != socketCount {
				t.Fatalf("%s: %d sockets up after the failed writes, want %d", c.portKind, upCount, socketCount)
			}
			env.expectReady(c.portKind + " after the failed writes")
			if c.portKind == frontPortWhodis {
				for len(env.quicResults) != 0 {
					if result := env.nextQuicResult(); result == c.portKind+" "+frontQuicResultConnect {
						t.Fatalf("the whodis client reconnected: the translation did not absorb the failed writes")
					}
				}
			}
			client.close()
		}
	})
}

// A QUIC connection whose packets reach a sibling socket of its group, as a
// re-creation moves most of a group's flows (D22), is never answered with a
// stateless reset: the sibling doesn't hold the connection and keeps no reset
// key, so it stays silent, the connection ends by its idle timeout, and the
// client then connects again through the group, the one reconnect D22 (a)
// accepts. Transports that shared one key would each answer a connection they
// don't hold with that connection's own token, so anyone who saw a connection
// id could fetch its token from another socket of the block, or from the same
// port by another source port, and end the connection with it (RFC 9000
// §21.11). Here the group is narrowed to one socket, the connection made on
// it, and the socket lost after a sibling is back, so the connection's next
// packets can only reach the sibling.
func TestLbAltSiblingSocketsNeverResetAMovedQuicConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			frontSettings: func(settings *FrontServerSettings) {
				settings.SocketRestartInitialDelay = time.Millisecond
			},
		})
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		bindAddress := env.bindAddress(address, frontPortQuic)
		socketCount := len(env.frontServer.sockets)
		socketsPerPort := env.settings.SocketsPerPort

		hold := env.binds.hold(frontProtocolUdp, bindAddress)
		defer hold.releaseAll()
		packetConns := env.binds.boundPacketConns(bindAddress)
		for _, packetConn := range packetConns[1:] {
			packetConn.Close()
		}
		hold.waitWaiting(t, socketsPerPort-1)
		env.waitSocketsUp(socketCount - (socketsPerPort - 1))

		quicConfig := testLbAltClientQuicConfig()
		// the moved connection ends here unless a reset ends it first, which
		// would come within a round trip of its next packet
		quicConfig.MaxIdleTimeout = 10 * time.Second
		conn, err := env.dialQuicFrom(
			env.clientPacketConn(address),
			address,
			frontPortQuic,
			env.clientTlsConfig(testLbAltApiName, []string{http3.NextProtoH3}),
			quicConfig,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")

		// a sibling comes back, and the connection's socket is lost
		hold.releaseOne()
		env.waitSocketsUp(socketCount - (socketsPerPort - 1) + 1)
		packetConns[0].Close()
		hold.waitWaiting(t, socketsPerPort-1)
		env.waitSocketsUp(socketCount - (socketsPerPort - 1))

		// packets of data, each larger than a stateless reset, which quic-go
		// answers with one when its transport has a reset key; a connection
		// already ended fails here rather than passing below
		stream, err := conn.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write(make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
		testLbAltWait(t, conn.Context().Done(), "the moved connection's end")
		var idleTimeoutErr *quic.IdleTimeoutError
		if err := context.Cause(conn.Context()); !errors.As(err, &idleTimeoutErr) {
			t.Fatalf("the moved connection ended with %v, want its idle timeout", err)
		}

		// the client connects again, through the group's live socket
		reconnected, err := env.dialQuic(address, frontPortQuic, testLbAltApiName, []string{http3.NextProtoH3})
		if err != nil {
			t.Fatalf("the reconnect: %v", err)
		}
		reconnected.CloseWithError(0, "")
	})
}
