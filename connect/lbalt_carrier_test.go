package connect

// Connect's carriers through the lb_alt front's public sockets, with real
// client code and a real exchange (LBALT.md D9, §9 "Carriers", Gate A item 5):
// connect's websocket and H1+ on TCP 443, raw QUIC on UDP 443, QUIC under
// whodis on the whodis socket, and the api over h3 and whodis, each reached
// as a client reaches an interface.

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Each of connect's four carriers authenticates through the front's public
// sockets, on both interfaces and in both families, and exchanges messages
// both ways with a client on another carrier, through the exchange. The
// resident sees each connection's carrier, from the client's own address,
// and the H1 carriers are the ones pinned: the websocket, and the
// urnetwork-framer/1 upgrade.
func TestLbAltCarriersAuthenticateAndExchangeMessagesBothWays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, nil)
		addressA, _ := env.address(testLbAltInterfaceA, frontFamilyIpv4)
		addressB, _ := env.address(testLbAltInterfaceB, frontFamilyIpv6)

		pairs := []struct {
			carrierA testLbAltCarrier
			carrierB testLbAltCarrier
		}{
			{carrierA: testLbAltCarrierWebSocket, carrierB: testLbAltCarrierQuic},
			{carrierA: testLbAltCarrierH1Plus, carrierB: testLbAltCarrierWhodis},
			{carrierA: testLbAltCarrierQuic, carrierB: testLbAltCarrierWebSocket},
			{carrierA: testLbAltCarrierWhodis, carrierB: testLbAltCarrierH1Plus},
		}
		for i, pair := range pairs {
			clientA := env.newClient(env.newPeer("a"), pair.carrierA, &testLbAltDial{address: addressA})
			clientB := env.newClient(env.newPeer("b"), pair.carrierB, &testLbAltDial{address: addressB})
			for _, c := range []struct {
				client  *testLbAltClient
				address netip.Addr
			}{
				{client: clientA, address: addressA},
				{client: clientB, address: addressB},
			} {
				edge := env.waitResidentEdge(c.client.peer.clientId)
				if edge.Carrier != c.client.carrier.residentCarrier {
					t.Fatalf("pair %d: %s reached its resident as carrier %d, want %d", i, c.client.carrier.name, edge.Carrier, c.client.carrier.residentCarrier)
				}
				// the h3 carriers' own sockets bind no address, and a dial to
				// 127.0.0.1 or ::1, as both addresses here are, leaves from it
				if edge.ClientIp != testLbAltClientAddress(c.address) {
					t.Fatalf("pair %d: %s's resident saw the client at %s, want %s", i, c.client.carrier.name, edge.ClientIp, testLbAltClientAddress(c.address))
				}
				h1Snapshot := c.client.h1Stats.Snapshot()
				switch c.client.carrier {
				case testLbAltCarrierWebSocket:
					if h1Snapshot.WebSocketConnectionCount != 1 || h1Snapshot.H1PlusConnectionCount != 0 {
						t.Fatalf("pair %d: the websocket client negotiated %+v", i, h1Snapshot)
					}
				case testLbAltCarrierH1Plus:
					if h1Snapshot.H1PlusConnectionCount != 1 || h1Snapshot.WebSocketConnectionCount != 0 {
						t.Fatalf("pair %d: the H1+ client negotiated %+v", i, h1Snapshot)
					}
				}
			}
			testLbAltExchangeMessages(t, clientA, clientB, 8)
			clientA.close()
			clientB.close()
		}
	})
}

// The api's alt h3 and whodis dialers, real client code, reach the api's
// blocks through the front's quic and whodis sockets: a client authenticates
// with its network's jwt, and its request and the api's answer cross the
// front both ways. The front counts each connection as the api's, on the
// socket the dialer pins.
func TestLbAltApiAuthenticatesOverH3AndWhodis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		env := newTestLbAltEnv(t, &testLbAltEnvSettings{
			upstreams: true,
		})
		address, _ := env.address(testLbAltInterfaceA, frontFamilyIpv6)
		peer := env.newPeer("api")
		for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
			clientStrategy := env.newApiStrategy(address, portKind)
			requestBytes, err := json.Marshal(map[string]any{
				"description": "lbalt " + portKind,
				"device_spec": "lbalt",
			})
			if err != nil {
				t.Fatal(err)
			}
			responseBytes, err := connect.HttpPostWithStrategyRaw(
				env.ctx,
				clientStrategy,
				"https://"+testLbAltApiName+"/network/auth-client",
				requestBytes,
				peer.networkByJwt,
			)
			if err != nil {
				t.Fatalf("%s: auth-client = %v", portKind, err)
			}
			var result struct {
				ByClientJwt string `json:"by_client_jwt"`
				Error       *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(responseBytes, &result); err != nil {
				t.Fatalf("%s: %q: %v", portKind, responseBytes, err)
			}
			if result.Error != nil || result.ByClientJwt == "" {
				t.Fatalf("%s: auth-client answered %s", portKind, responseBytes)
			}
			if _, err := connect.ParseByJwtUnverified(result.ByClientJwt); err != nil {
				t.Fatalf("%s: the client jwt does not parse: %v", portKind, err)
			}
			results := []string{}
			for len(env.quicResults) != 0 {
				results = append(results, env.nextQuicResult())
			}
			if !slices.Contains(results, portKind+" "+frontQuicResultApi) {
				t.Fatalf("%s: the front counted %v, want an api connection on the %s socket", portKind, results, portKind)
			}
			for _, result := range results {
				if result != portKind+" "+frontQuicResultApi {
					t.Fatalf("%s: the front counted %q", portKind, result)
				}
			}
			clientStrategy.Close()
		}
		if code, _ := env.status(); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
	})
}
