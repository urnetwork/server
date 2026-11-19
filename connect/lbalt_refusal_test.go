package connect

// The lb_alt front's refusals, through its public sockets with connect's real
// handler and the real nginx front (LBALT-IMPL.md §2.8, LBALT.md D3, D9,
// §4.4, §9 "Carriers"): names that are neither connect's nor the api's,
// names the front doesn't serve, the per-address QUIC cap, and a PROXY
// header a client sends.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/nginx"
)

// Every served name that is neither connect's nor the api's, a forwarded
// service's, an lb name and a redirect alias, completes its handshake on the
// quic and whodis sockets of both families and is closed with 0x1000, and
// the code arrives. Connect's handler never sees one: no client has a
// resident.
func TestLbAltQuicRefusesOtherServedNamesWithUnknownServerName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		serverNames := []string{testLbAltForwardedName, env.routeTable.Lb.Names[0], testLbAltRedirectName}
		for _, interfaceAddress := range env.interfaceAddresses() {
			for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
				for _, serverName := range serverNames {
					conn, err := env.dialQuic(interfaceAddress.address, portKind, serverName, []string{http3.NextProtoH3})
					quicClose := testLbAltCloseOf(conn, err)
					if !quicClose.applicationArrived || quicClose.applicationCode != frontQuicErrorCodeUnknownServerName {
						t.Fatalf("%s %s %s: closed %+v, want 0x1000 to arrive", interfaceAddress.address, portKind, serverName, quicClose)
					}
					if result := env.nextQuicResult(); result != portKind+" "+frontQuicResultUnknownName {
						t.Fatalf("%s %s %s: counted %q", interfaceAddress.address, portKind, serverName, result)
					}
				}
			}
		}
		if candidates := env.exchange.ResidentMoveCandidates(); len(candidates) != 0 {
			t.Fatalf("a refused connection reached connect: %d residents", len(candidates))
		}
	})
}

// A name the front doesn't serve, or none at all, fails the TLS handshake on
// QUIC (§2.8): the front holds no certificate for it, as alt held none, so the
// client reads a TLS alert as a crypto error and never an application close,
// and nothing is dispatched.
func TestLbAltQuicFailsTheHandshakeOfAnUnknownServerName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		for _, interfaceAddress := range env.interfaceAddresses() {
			for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
				for _, serverName := range []string{testLbAltUnknownName, ""} {
					tlsConfig := env.clientTlsConfig(serverName, []string{http3.NextProtoH3})
					// nothing could verify a name the front has no certificate for
					tlsConfig.InsecureSkipVerify = true
					packetConn := env.clientCarrierConn(env.clientPacketConn(interfaceAddress.address), portKind)
					conn, err := env.dialQuicFrom(packetConn, interfaceAddress.address, portKind, tlsConfig, testLbAltClientQuicConfig(), false)
					if err == nil {
						conn.CloseWithError(0, "")
						t.Fatalf("%s %s %q: the handshake completed", interfaceAddress.address, portKind, serverName)
					}
					quicClose := testLbAltCloseOf(nil, err)
					if !quicClose.transportErr || !quicClose.transportCode.IsCryptoError() {
						t.Fatalf("%s %s %q: failed %+v, want a TLS alert", interfaceAddress.address, portKind, serverName, quicClose)
					}
				}
			}
		}
		env.expectNoQuicResult("after the failed handshakes")
	})
}

// A connect connection over its interface address's QUIC cap is closed with
// 0x1001 on either QUIC socket before connect's handler sees it, while the
// real client holding the address's one slot keeps exchanging messages. Once
// that client is gone, a new real client from the address authenticates.
func TestLbAltQuicConnectionCapRefusesWithRateLimited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			routeTable: func(routeTable *nginx.RouteTable) {
				routeInterface, _ := routeTable.Interface(testLbAltInterfaceB)
				routeInterface.RateLimit.NetConnections = 1
			},
		})
		cappedAddress, _ := env.address(testLbAltInterfaceB, frontFamilyIpv6)
		otherAddress, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)

		holder := env.newClient(env.newPeer("holder"), testLbAltCarrierQuic, &testLbAltDial{address: cappedAddress})
		env.waitResidentEdge(holder.peer.clientId)
		if result := env.nextQuicResult(); result != frontPortQuic+" "+frontQuicResultConnect {
			t.Fatalf("the holder counted %q", result)
		}

		for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
			conn, err := env.dialQuic(cappedAddress, portKind, testLbAltConnectName, nil)
			quicClose := testLbAltCloseOf(conn, err)
			if !quicClose.applicationArrived || quicClose.applicationCode != frontQuicErrorCodeRateLimited {
				t.Fatalf("%s over the cap: closed %+v, want 0x1001 to arrive", portKind, quicClose)
			}
			if result := env.nextQuicResult(); result != portKind+" "+frontQuicResultRateLimited {
				t.Fatalf("%s over the cap counted %q", portKind, result)
			}
		}

		// the slot's holder still serves, both ways
		other := env.newClient(env.newPeer("other"), testLbAltCarrierWebSocket, &testLbAltDial{address: otherAddress})
		testLbAltExchangeMessages(t, holder, other, 4)

		holder.close()
		successor := env.newClient(env.newPeer("successor"), testLbAltCarrierWhodis, &testLbAltDial{address: cappedAddress})
		if edge := env.waitResidentEdge(successor.peer.clientId); edge.Carrier != ResidentCarrierH3Dns {
			t.Fatalf("the successor arrived as carrier %d", edge.Carrier)
		}
		testLbAltExchangeMessages(t, successor, other, 4)
	})
}

// A client socket that prepends a PROXY v2 header to every datagram, as
// nginx's stream proxy does, claiming source as the client's address.
type testLbAltProxyPrefixConn struct {
	net.PacketConn
	header []byte
}

// Prefixes packetConn's datagrams with a PROXY v2 udp header from source to
// destination, both of one family.
func newTestLbAltProxyPrefixConn(packetConn net.PacketConn, source netip.AddrPort, destination netip.AddrPort) *testLbAltProxyPrefixConn {
	// the v2 signature, then version 2 PROXY
	header := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A, 0x21}
	if source.Addr().Is4() {
		// udp over ipv4, 12 address bytes
		header = append(header, 0x12, 0x00, 0x0C)
		sourceBytes := source.Addr().As4()
		destinationBytes := destination.Addr().As4()
		header = append(header, sourceBytes[:]...)
		header = append(header, destinationBytes[:]...)
	} else {
		// udp over ipv6, 36 address bytes
		header = append(header, 0x22, 0x00, 0x24)
		sourceBytes := source.Addr().As16()
		destinationBytes := destination.Addr().As16()
		header = append(header, sourceBytes[:]...)
		header = append(header, destinationBytes[:]...)
	}
	header = binary.BigEndian.AppendUint16(header, source.Port())
	header = binary.BigEndian.AppendUint16(header, destination.Port())
	return &testLbAltProxyPrefixConn{
		PacketConn: packetConn,
		header:     header,
	}
}

// Writes the header and the datagram as one datagram.
func (self *testLbAltProxyPrefixConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if _, err := self.PacketConn.WriteTo(append(slices.Clone(self.header), p...), addr); err != nil {
		return 0, err
	}
	return len(p), nil
}

// No public socket honors a PROXY header a client sends (§4.4.1, §9). A
// client that prefixes its QUIC or whodis datagrams with a PROXY v2 header
// claiming another address never completes a handshake, in either family;
// one that opens TCP 443 or 80 with a PROXY line fails its TLS handshake or
// gets a 400. A real client on the same sockets connects, and its resident
// sees it at the address its socket binds, never the claimed one.
func TestLbAltPublicSocketsDoNotHonorAProxyHeader(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		claimedAddresses := map[string]netip.AddrPort{
			frontFamilyIpv4: netip.MustParseAddrPort("203.0.113.7:4242"),
			frontFamilyIpv6: netip.MustParseAddrPort("[2001:db8::7]:4242"),
		}
		for _, interfaceAddress := range env.interfaceAddresses() {
			claimed := claimedAddresses[interfaceAddress.family]
			for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
				bindAddress := env.bindAddress(interfaceAddress.address, portKind)
				// the header goes below the dns translation, as nginx's would
				prefixed := newTestLbAltProxyPrefixConn(env.clientPacketConn(interfaceAddress.address), claimed, bindAddress)
				quicConfig := testLbAltClientQuicConfig()
				quicConfig.HandshakeIdleTimeout = 3 * time.Second
				conn, err := env.dialQuicFrom(
					env.clientCarrierConn(prefixed, portKind),
					interfaceAddress.address,
					portKind,
					env.clientTlsConfig(testLbAltConnectName, nil),
					quicConfig,
					false,
				)
				if err == nil {
					conn.CloseWithError(0, "")
					t.Fatalf("%s %s: a PROXY-prefixed handshake completed", bindAddress, portKind)
				}
			}
			env.expectNoQuicResult("after the PROXY-prefixed dials on " + interfaceAddress.address.String())

			// tcp 443: the PROXY line is no TLS record, so no handshake
			httpsConn, err := net.Dial("tcp", env.bindAddress(interfaceAddress.address, frontPortHttps).String())
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(httpsConn, "PROXY TCP4 203.0.113.7 198.51.100.1 4242 443\r\n")
			tlsConn := tls.Client(httpsConn, env.clientTlsConfig(testLbAltConnectName, []string{"http/1.1"}))
			handshakeCtx, handshakeCancel := context.WithTimeout(env.ctx, testLbAltProgressTimeout)
			err = tlsConn.HandshakeContext(handshakeCtx)
			handshakeCancel()
			tlsConn.Close()
			if err == nil {
				t.Fatalf("%s: a TLS handshake after a PROXY line completed", interfaceAddress.address)
			}

			// tcp 80: the PROXY line is a malformed request
			httpConn, err := net.Dial("tcp", env.bindAddress(interfaceAddress.address, frontPortHttp).String())
			if err != nil {
				t.Fatal(err)
			}
			httpConn.SetDeadline(time.Now().Add(testLbAltProgressTimeout))
			fmt.Fprintf(
				httpConn,
				"PROXY TCP4 203.0.113.7 198.51.100.1 4242 80\r\nGET %s/status HTTP/1.1\r\nHost: %s\r\n\r\n",
				env.routeTable.Lb.RoutePrefixes[0],
				env.routeTable.Lb.Names[0],
			)
			response, err := http.ReadResponse(bufio.NewReader(httpConn), nil)
			httpConn.Close()
			if err != nil {
				t.Fatalf("%s: port 80 after a PROXY line: %v", interfaceAddress.address, err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: port 80 answered a PROXY line %d", interfaceAddress.address, response.StatusCode)
			}

			// a real client on the same sockets connects as itself
			sourceConns := &testLbAltSourceConns{
				address: interfaceAddress.address,
			}
			client := env.newClient(env.newPeer("plain"), testLbAltCarrierQuic, &testLbAltDial{
				address:           interfaceAddress.address,
				packetConnFactory: sourceConns.newPacketConn,
			})
			edge := env.waitResidentEdge(client.peer.clientId)
			if edge.ClientIp != testLbAltClientAddress(interfaceAddress.address) {
				t.Fatalf("%s: the resident saw the client at %s, want %s", interfaceAddress.address, edge.ClientIp, testLbAltClientAddress(interfaceAddress.address))
			}
			if result := env.nextQuicResult(); result != frontPortQuic+" "+frontQuicResultConnect {
				t.Fatalf("%s: the real client counted %q", interfaceAddress.address, result)
			}
			client.close()
		}
	})
}

// The ingress headers a client sends on TCP 443 are not honored (§4.4.2):
// connect's H1 path resolves a client from X-UR-Forwarded-For, which the
// front derives from the accepted peer, so a real client that sends its own
// X-UR-Forwarded-For and X-Forwarded-For still reaches its resident at the
// address its socket binds, on the websocket and on H1+.
func TestLbAltH1IngressHeadersComeFromTheAcceptedPeer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		for _, interfaceAddress := range env.interfaceAddresses() {
			for _, carrier := range []testLbAltCarrier{testLbAltCarrierWebSocket, testLbAltCarrierH1Plus} {
				client := env.newClient(env.newPeer("spoof"), carrier, &testLbAltDial{
					address: interfaceAddress.address,
					extraHeaders: http.Header{
						"X-UR-Forwarded-For":      []string{"203.0.113.9:4242"},
						"X-Forwarded-For":         []string{"203.0.113.10"},
						"X-Forwarded-Source-Port": []string{"4343"},
					},
				})
				edge := env.waitResidentEdge(client.peer.clientId)
				if edge.ClientIp != testLbAltClientAddress(interfaceAddress.address) {
					t.Fatalf("%s %s: the resident saw the client at %s, want %s", interfaceAddress.address, carrier.name, edge.ClientIp, testLbAltClientAddress(interfaceAddress.address))
				}
				client.close()
			}
		}
	})
}
