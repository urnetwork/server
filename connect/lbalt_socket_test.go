package connect

// The lb_alt front's public udp sockets under loss and failure, with real
// clients and a real exchange (LBALT.md D13, D22, §4.4.4, §9 "Carriers"):
// a lost socket is rebuilt on its own while its siblings serve, and the
// connections whose flows move between a group's sockets keep their
// transport (D22 (b)); a socket survives datagrams it can't write.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"syscall"
	"testing"
	"time"

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
// readiness is whole again, and only that socket was bound again. The
// re-creation changes the group's size, which moves flows between its
// sockets (on darwin the newest socket takes them all), but every socket
// feeds the group's one transport, so the new client keeps its connection
// and never connects again (D22 (b)); with a transport per socket it
// reconnected after its idle timeout. The quic and the whodis sockets each in
// turn.
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
			for len(env.quicResults) != 0 {
				env.nextQuicResult()
			}
			newClient := env.newClient(env.newPeer("new"), c.carrier, &testLbAltDial{address: lostAddress})
			if edge := env.waitResidentEdge(newClient.peer.clientId); edge.Carrier != c.carrier.residentCarrier {
				t.Fatalf("%s lost: the new client arrived as carrier %d", c.portKind, edge.Carrier)
			}
			testLbAltExchangeMessages(t, newClient, quicClient, 4)
			if err := env.frontServer.ctx.Err(); err != nil {
				t.Fatalf("%s lost: the front ended: %v", c.portKind, err)
			}
			if result := env.nextQuicResult(); result != c.portKind+" "+frontQuicResultConnect {
				t.Fatalf("%s lost: the new client's connection counted %q", c.portKind, result)
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
			env.expectNoQuicResult(c.portKind + " back: the new client connected again")
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
// re-creation moves most of a group's flows (D22), keeps working (D22 (b)):
// every socket of the group feeds one transport, which holds the connection,
// so its next request is answered at once on the same connection. With a
// transport per socket the sibling's transport didn't hold it, stayed silent,
// and the connection ended by its idle timeout. No transport answers with a
// stateless reset: none has a reset key, and each keeps quic-go's own random
// token key, never shared, so no transport of the block is a reset oracle for
// another (RFC 9000 §21.11). Here the group is narrowed to one socket, the
// connection made on it, and the socket lost after a sibling is back, so the
// connection's next packets can only reach the sibling.
func TestLbAltQuicConnectionKeepsItsTransportWhenItsSocketIsLost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			upstreams: true,
			apiHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "api %s", r.URL.Path)
			}),
			frontSettings: func(settings *FrontServerSettings) {
				settings.SocketRestartInitialDelay = time.Millisecond
			},
		})
		// the upstreams layout keeps 127.0.0.1 for the blocks
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv6)
		bindAddress := env.bindAddress(address, frontPortQuic)
		socketCount := len(env.frontServer.sockets)
		socketsPerPort := env.settings.SocketsPerPort

		hold := env.binds.hold(frontProtocolUdp, bindAddress)
		defer hold.releaseAll()
		packetConns := env.binds.boundPacketConns(bindAddress)
		if len(packetConns) != socketsPerPort {
			t.Fatalf("%d quic sockets bound on %s", len(packetConns), bindAddress)
		}
		for _, packetConn := range packetConns[1:] {
			packetConn.Close()
		}
		hold.waitWaiting(t, socketsPerPort-1)
		env.waitSocketsUp(socketCount - (socketsPerPort - 1))

		quicConfig := testLbAltClientQuicConfig()
		// a connection the move lost ends here, or sooner by a reset
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
		clientConn := (&http3.Transport{}).NewClientConn(conn)
		get := func(path string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(env.ctx, testLbAltProgressTimeout)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testLbAltApiName+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := clientConn.RoundTrip(request)
			if err != nil {
				t.Fatalf("GET %s: %v (the connection: %v)", path, err, context.Cause(conn.Context()))
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != "api "+path {
				t.Fatalf("GET %s: %d %q %v", path, response.StatusCode, body, err)
			}
		}
		get("/on-its-socket")

		// a sibling comes back, and the connection's socket is lost
		hold.releaseOne()
		env.waitSocketsUp(socketCount - (socketsPerPort - 1) + 1)
		packetConns[0].Close()
		hold.waitWaiting(t, socketsPerPort-1)
		env.waitSocketsUp(socketCount - (socketsPerPort - 1))

		get("/on-the-sibling")
		if err := conn.Context().Err(); err != nil {
			t.Fatalf("the moved connection ended: %v", context.Cause(conn.Context()))
		}
	})
}
