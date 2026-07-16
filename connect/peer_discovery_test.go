package connect

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// peerDiscoveryEnv is a real exchange + connect handler + http server with one
// network, shared by the peer discovery integration tests. Each test uses its
// own ports since the h3/dns listeners release asynchronously on ctx cancel.
type peerDiscoveryEnv struct {
	t   testing.TB
	ctx context.Context

	port        int
	exchange    *Exchange
	httpServer  *http.Server
	networkId   server.Id
	userId      server.Id
	userSession *session.ClientSession
}

func testing_newPeerDiscoveryEnv(ctx context.Context, t testing.TB, port int, servicePort int) *peerDiscoveryEnv {
	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	const (
		service = "connect"
		block   = "test"
		host    = "host0"
	)

	routes := map[string]string{host: "127.0.0.1"}

	exchangeSettings := DefaultExchangeSettings()
	exchangeSettings.ExchangeResidentTtl = 5 * time.Second
	// network peers default off (PEERS2.md rollout); this suite tests them.
	// Poll-only delivery: fast ticks so the test's wait windows catch changes.
	exchangeSettings.EnableNetworkPeers = true
	exchangeSettings.NetworkPeersPollInterval = 200 * time.Millisecond
	exchangeSettings.StreamHopsPollInterval = 200 * time.Millisecond
	hostToServicePorts := map[int]int{servicePort: servicePort}
	exchange := NewExchange(ctx, host, service, block, hostToServicePorts, routes, exchangeSettings)

	handlerSettings := DefaultConnectHandlerSettings()
	handlerSettings.ListenH3Port = port + 443
	handlerSettings.ListenDnsPort = port + 53
	handlerSettings.EnableProxyProtocol = false
	handlerSettings.TransportTlsSettings.EnableSelfSign = true
	handlerSettings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	handlerSettings.ConnectionAnnounceTimeout = 0
	handlerSettings.ConnectionAnnounceSettings.EnableNetworkPeers = true
	handlerSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	connectHandler := NewConnectHandler(ctx, server.NewId(), exchange, handlerSettings)

	// NewConnectHandler must derive the announce registration ttl from the
	// exchange resident ttl: disconnect detection relies on the registration
	// expiring at the heartbeat cadence, and an independent knob here caused
	// a 5-minute disconnect-detection delay (found 2026-07-15). Assert the
	// derivation so a reintroduced knob fails fast with an exact message.
	if handlerSettings.ConnectionAnnounceSettings.PeerRegisterTtl != exchangeSettings.ExchangeResidentTtl {
		t.Fatalf("announce PeerRegisterTtl (%s) != ExchangeResidentTtl (%s) — disconnect detection will be delayed by the difference",
			handlerSettings.ConnectionAnnounceSettings.PeerRegisterTtl, exchangeSettings.ExchangeResidentTtl)
	}

	httpRoutes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", connectHandler.Connect),
	}
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router.NewRouter(ctx, httpRoutes),
	}
	go server.HandleError1(httpServer.ListenAndServe)
	select {
	case <-time.After(2 * time.Second):
	}

	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("peerdiscovery-%s", networkId), userId)
	// fund the network so escrowed contracts (e.g. the companion Stream
	// fallback to a provide-off peer) can settle
	err := model.AddBasicTransferBalance(
		ctx,
		networkId,
		model.ByteCount(1024*1024*1024*1024),
		server.NowUtc(),
		server.NowUtc().Add(365*24*time.Hour),
	)
	connect.AssertEqual(t, err, nil)
	userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})

	return &peerDiscoveryEnv{
		t:           t,
		ctx:         ctx,
		port:        port,
		exchange:    exchange,
		httpServer:  httpServer,
		networkId:   networkId,
		userId:      userId,
		userSession: userSession,
	}
}

func (self *peerDiscoveryEnv) Close() {
	self.httpServer.Close()
	self.exchange.Close()
}

func (self *peerDiscoveryEnv) authClient(args *model.AuthNetworkClientArgs) (server.Id, string) {
	result, err := model.AuthNetworkClient(args, self.userSession)
	connect.AssertEqual(self.t, err, nil)
	connect.AssertEqual(self.t, result.Error, nil)
	return *result.ClientId, *result.ByClientJwt
}

// newClient creates a connect client with the test controller out-of-band
// control, and control pings that keep its resident alive through the quiet
// phases of a test (an idle resident is canceled after `ExchangeResidentTtl`)
func (self *peerDiscoveryEnv) newClient(clientId server.Id) *connect.Client {
	clientSettings := connect.DefaultClientSettings()
	clientSettings.ControlPingTimeout = 1 * time.Second
	return connect.NewClient(
		self.ctx,
		connect.Id(clientId),
		Testing_NewControllerOutOfBandControl(self.ctx, clientId, clientSettings.ContractManagerSettings),
		clientSettings,
	)
}

func (self *peerDiscoveryEnv) newTransport(byClientJwt string, instanceId server.Id, routeManager *connect.RouteManager) *connect.PlatformTransport {
	auth := &connect.ClientAuth{
		ByJwt:      byClientJwt,
		InstanceId: connect.Id(instanceId),
		AppVersion: "0.0.0",
	}
	settings := connect.DefaultPlatformTransportSettings()
	settings.QuicTlsConfig.InsecureSkipVerify = true
	settings.H3Port = self.port + 443
	settings.DnsPort = self.port + 53
	return connect.NewPlatformTransportWithTargetMode(
		self.ctx,
		connect.NewClientStrategyWithDefaults(self.ctx),
		routeManager,
		fmt.Sprintf("ws://127.0.0.1:%d", self.port),
		auth,
		connect.TransportModeH1,
		settings,
	)
}

// setProvideModes announces the provide modes and waits for the platform ack
func (self *peerDiscoveryEnv) setProvideModes(client *connect.Client, provideModes map[protocol.ProvideMode]bool) {
	provideAck := make(chan error, 1)
	client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		provideModes,
		func(err error) {
			select {
			case provideAck <- err:
			default:
			}
		},
	)
	select {
	case err := <-provideAck:
		connect.AssertEqual(self.t, err, nil)
	case <-time.After(60 * time.Second):
		self.t.Fatal("timeout waiting for provide ack")
	}
}

// waitForPeers waits on the peers monitor until the condition holds
func waitForPeers(ctx context.Context, client *connect.Client, cond func(connected []*connect.NetworkPeer, disconnectedCount int) bool) bool {
	endTime := time.Now().Add(60 * time.Second)
	for {
		notify := client.PeerManager().PeersMonitor().NotifyChannel()
		connected, disconnectedCount := client.NetworkPeers()
		if cond(connected, disconnectedCount) {
			return true
		}
		if endTime.Before(time.Now()) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-notify:
		case <-time.After(1 * time.Second):
		}
	}
}

func findPeer(connected []*connect.NetworkPeer, clientId server.Id) *connect.NetworkPeer {
	for _, peer := range connected {
		if peer.ClientId == connect.Id(clientId) {
			return peer
		}
	}
	return nil
}

// recordPeerReceives records the `connect.Peer` identity of frames received
// from a source client
func recordPeerReceives(client *connect.Client, sourceClientId server.Id) chan connect.Peer {
	receives := make(chan connect.Peer, 1024)
	client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
		if source.SourceId == connect.Id(sourceClientId) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage {
					select {
					case receives <- peer:
					default:
					}
				}
			}
		}
	})
	return receives
}

func sendSimpleMessage(t testing.TB, from *connect.Client, toClientId server.Id, opts ...any) {
	frame, err := connect.ToFrame(&protocol.SimpleMessage{Content: "hello"}, connect.DefaultProtocolVersion)
	connect.AssertEqual(t, err, nil)
	sent := from.SendWithTimeout(frame, connect.DestinationId(connect.Id(toClientId)), func(err error) {}, 60*time.Second, opts...)
	connect.AssertEqual(t, sent, true)
}

// TestExchangePeerDiscovery drives two top-level clients of the same network
// through a real exchange and asserts the peer discovery flow end to end:
//
//   - each client's resident registers it in the network peer registry and
//     announces the peers over the control channel, so each client's
//     PeerManager sees the other peer with its identity metadata
//     (roles, principal, device name/spec)
//   - provide status changes propagate to the peers
//   - messages between network peers settle contracts at ProvideMode_Network
//     carrying the source's roles and principal to the receive callback
//   - the fast peer discovery api returns the peers for network and
//     top-level client sessions
//   - a disconnected client moves to a disconnect marker on the other peer
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestExchangePeerDiscovery(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8090, 9010)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
			DeviceSpec:  "spec a",
			Roles:       []string{"role-a1", "role-a2"},
			Principal:   "principal-a",
		})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device b",
			DeviceSpec:  "spec b",
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()

		receiveB := recordPeerReceives(clientB, clientIdA)

		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
		defer transportB.Close()

		// announce provide modes (network peers use the network mode)
		env.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})
		env.setProvideModes(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})

		// a sees b connected with provide enabled and identity metadata
		ok := waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			peerB := findPeer(connected, clientIdB)
			return peerB != nil && peerB.ProvideEnabled
		})
		connect.AssertEqual(t, ok, true)
		connectedA, disconnectedCountA := clientA.NetworkPeers()
		connect.AssertEqual(t, disconnectedCountA, 0)
		peerB := findPeer(connectedA, clientIdB)
		connect.AssertEqual(t, findPeer(connectedA, clientIdA), nil)
		connect.AssertEqual(t, peerB.DeviceName, "device b")
		connect.AssertEqual(t, peerB.DeviceSpec, "spec b")
		connect.AssertEqual(t, peerB.Principal, "")
		connect.AssertEqual(t, len(peerB.Roles), 0)
		connect.AssertEqual(t, slices.Contains(peerB.ProvideModes, protocol.ProvideMode_Network), true)

		// b sees a connected with its roles and principal
		ok = waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			peerA := findPeer(connected, clientIdA)
			return peerA != nil && peerA.ProvideEnabled
		})
		connect.AssertEqual(t, ok, true)
		connectedB, _ := clientB.NetworkPeers()
		peerA := findPeer(connectedB, clientIdA)
		connect.AssertEqual(t, findPeer(connectedB, clientIdB), nil)
		connect.AssertEqual(t, peerA.DeviceName, "device a")
		connect.AssertEqual(t, peerA.DeviceSpec, "spec a")
		connect.AssertEqual(t, peerA.Principal, "principal-a")
		connect.AssertEqual(t, peerA.Roles, []string{"role-a1", "role-a2"})

		// messages between network peers settle at ProvideMode_Network and
		// carry the source's roles and principal from the signed contract
		sendSimpleMessage(t, clientA, clientIdB)
		select {
		case peer := <-receiveB:
			connect.AssertEqual(t, peer.ProvideMode, protocol.ProvideMode_Network)
			connect.AssertEqual(t, peer.Roles, []string{"role-a1", "role-a2"})
			connect.AssertEqual(t, peer.Principal, "principal-a")
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for message from a to b")
		}

		// the fast peer discovery api: a network session sees both peers
		peersResult, err := model.GetNetworkPeersForSession(env.userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, peersResult.Error, nil)
		connect.AssertEqual(t, len(peersResult.Peers), 2)

		// a top-level client session sees the other peer, excluding itself
		byJwtA, err := jwt.ParseByJwt(ctx, byClientJwtA)
		connect.AssertEqual(t, err, nil)
		clientSessionA := session.Testing_CreateClientSession(ctx, byJwtA)
		peersResult, err = model.GetNetworkPeersForSession(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, peersResult.Error, nil)
		connect.AssertEqual(t, len(peersResult.Peers), 1)
		connect.AssertEqual(t, peersResult.Peers[0].ClientId, clientIdB)
		connect.AssertEqual(t, peersResult.DisconnectedCount, 0)

		// disconnect b; a sees b move to a disconnect marker
		clientB.Cancel()
		transportB.Close()

		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdB) == nil && 1 <= disconnectedCount
		})
		if !ok {
			// diagnostic: compare the registry truth to the client view
			eventId, registryPeers := model.GetNetworkPeers(ctx, env.networkId)
			for _, p := range registryPeers {
				t.Logf("registry peer %s disconnect=%v (eventId=%d)", p.ClientId, p.DisconnectTime, eventId)
			}
			connectedA, disconnectedCountA := clientA.NetworkPeers()
			for _, p := range connectedA {
				t.Logf("client a sees connected peer %s", p.ClientId)
			}
			t.Logf("client a sees disconnectedCount=%d", disconnectedCountA)
		}
		connect.AssertEqual(t, ok, true)
	})
}

// TestExchangePeerDiscoveryProvideChanges covers the provide-status paths end
// to end: pause removes the public mode but keeps the network mode (network
// peers never fall back to stream on pause), provide-off drops the network
// mode from the peer list, a provide-off peer can still send under the network
// mode, and traffic to a provide-off peer rides the companion Stream fallback
// without carrying the source's identity.
func TestExchangePeerDiscoveryProvideChanges(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8091, 9011)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
			Roles:       []string{"role-a"},
			Principal:   "principal-a",
		})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device b",
			Roles:       []string{"role-b"},
			Principal:   "principal-b",
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()

		receiveA := recordPeerReceives(clientA, clientIdB)
		receiveB := recordPeerReceives(clientB, clientIdA)

		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
		defer transportB.Close()

		env.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})
		// b provides public (the sdk superset: public implies network)
		env.setProvideModes(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})

		hasMode := func(peer *connect.NetworkPeer, provideMode protocol.ProvideMode) bool {
			return slices.Contains(peer.ProvideModes, provideMode)
		}

		// a sees b providing public and network
		ok := waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			peerB := findPeer(connected, clientIdB)
			return peerB != nil && peerB.ProvideEnabled && hasMode(peerB, protocol.ProvideMode_Public)
		})
		connect.AssertEqual(t, ok, true)

		// pause stops public but keeps network: a sees the public mode drop
		// while provide stays enabled
		clientB.ContractManager().SetProvidePaused(true)

		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			peerB := findPeer(connected, clientIdB)
			return peerB != nil && peerB.ProvideEnabled && !hasMode(peerB, protocol.ProvideMode_Public)
		})
		connect.AssertEqual(t, ok, true)

		// traffic to the paused peer stays on the network mode and carries
		// the source's identity
		sendSimpleMessage(t, clientA, clientIdB)
		select {
		case peer := <-receiveB:
			connect.AssertEqual(t, peer.ProvideMode, protocol.ProvideMode_Network)
			connect.AssertEqual(t, peer.Roles, []string{"role-a"})
			connect.AssertEqual(t, peer.Principal, "principal-a")
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for message to paused b")
		}

		// unpause restores public
		clientB.ContractManager().SetProvidePaused(false)
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			peerB := findPeer(connected, clientIdB)
			return peerB != nil && hasMode(peerB, protocol.ProvideMode_Public)
		})
		connect.AssertEqual(t, ok, true)

		// provide-off removes the provide keys (stream only remains for
		// return traffic): a sees b with provide disabled
		env.setProvideModes(clientB, map[protocol.ProvideMode]bool{})
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			peerB := findPeer(connected, clientIdB)
			return peerB != nil && !peerB.ProvideEnabled
		})
		connect.AssertEqual(t, ok, true)

		// the provide-off peer can still send: a receives b's traffic under
		// the network mode with b's identity
		sendSimpleMessage(t, clientB, clientIdA)
		select {
		case peer := <-receiveA:
			connect.AssertEqual(t, peer.ProvideMode, protocol.ProvideMode_Network)
			connect.AssertEqual(t, peer.Roles, []string{"role-b"})
			connect.AssertEqual(t, peer.Principal, "principal-b")
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for message from provide-off b")
		}

		// a reply to the provide-off peer rides a companion Stream contract
		// (the return-traffic shape, riding the b -> a origin above; a
		// provide-off peer accepts only reply traffic) and carries no
		// identity
		sendSimpleMessage(t, clientA, clientIdB, connect.CompanionContract())
		select {
		case peer := <-receiveB:
			connect.AssertEqual(t, peer.ProvideMode, protocol.ProvideMode_Stream)
			connect.AssertEqual(t, len(peer.Roles), 0)
			connect.AssertEqual(t, peer.Principal, "")
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for message to provide-off b")
		}
	})
}

// TestExchangePeerDiscoveryResidentReplacement replaces a client's resident
// while it is connected and asserts the peer registry survives: the replaced
// resident's guarded remove must not remove the replacement's registration, so
// the peer stays connected with no disconnect marker. The peer flow keeps
// working through the replacement resident.
func TestExchangePeerDiscoveryResidentReplacement(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8092, 9012)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
		})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device b",
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()

		instanceIdB := server.NewId()
		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportB := env.newTransport(byClientJwtB, instanceIdB, clientB.RouteManager())
		defer transportB.Close()

		env.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})
		env.setProvideModes(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})

		ok := waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdB) != nil
		})
		connect.AssertEqual(t, ok, true)

		// replace b's resident in place
		residentB := model.GetResidentForClient(ctx, clientIdB, 0)
		connect.AssertNotEqual(t, residentB, nil)
		nominated := env.exchange.NominateLocalResident(clientIdB, instanceIdB, &residentB.ResidentId)
		connect.AssertEqual(t, nominated, true)

		// let the replacement settle: the old resident's cleanup runs its
		// guarded remove, which must not remove the replacement registration
		select {
		case <-time.After(3 * time.Second):
		}

		_, peers := model.GetNetworkPeers(ctx, env.networkId)
		foundConnectedB := false
		for _, peer := range peers {
			if peer.ClientId == clientIdB && peer.DisconnectTime == nil {
				foundConnectedB = true
			}
		}
		connect.AssertEqual(t, foundConnectedB, true)

		// a still sees b connected with no disconnect marker
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdB) != nil && disconnectedCount == 0
		})
		connect.AssertEqual(t, ok, true)

		// the peer flow keeps working through the replacement resident:
		// b (now on the new resident) sees a disconnect
		clientA.Cancel()
		transportA.Close()

		ok = waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdA) == nil && 1 <= disconnectedCount
		})
		connect.AssertEqual(t, ok, true)
	})
}

// TestExchangePeerDiscoveryDerivativeClient asserts that derivative clients
// (clients with a source client id) are not network peers: they never appear
// in the peer list, they receive no peer announcements, and the fast peer
// discovery api rejects their sessions.
func TestExchangePeerDiscoveryDerivativeClient(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8093, 9013)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
		})
		clientIdD, byClientJwtD := env.authClient(&model.AuthNetworkClientArgs{
			Description:    "derived device",
			SourceClientId: &clientIdA,
		})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientD := env.newClient(clientIdD)
		defer clientD.Close()

		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportD := env.newTransport(byClientJwtD, server.NewId(), clientD.RouteManager())
		defer transportD.Close()

		env.setProvideModes(clientA, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})
		env.setProvideModes(clientD, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})

		// let the residents settle and announce
		select {
		case <-time.After(5 * time.Second):
		}

		// the derivative client is not in the registry and receives no peer
		// announcements
		_, peers := model.GetNetworkPeers(ctx, env.networkId)
		for _, peer := range peers {
			connect.AssertNotEqual(t, peer.ClientId, clientIdD)
		}
		connectedA, _ := clientA.NetworkPeers()
		connect.AssertEqual(t, findPeer(connectedA, clientIdD), nil)
		connectedD, disconnectedCountD := clientD.NetworkPeers()
		connect.AssertEqual(t, len(connectedD), 0)
		connect.AssertEqual(t, disconnectedCountD, 0)

		// the fast peer discovery api rejects a derivative client session
		byJwtD, err := jwt.ParseByJwt(ctx, byClientJwtD)
		connect.AssertEqual(t, err, nil)
		clientSessionD := session.Testing_CreateClientSession(ctx, byJwtD)
		peersResult, err := model.GetNetworkPeersForSession(clientSessionD)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, peersResult.Error, nil)
	})
}

// TestExchangePeerDiscoveryLargeNetwork registers more peers than one
// `NetworkPeersUpdate` frame carries (the update batch size) and asserts a
// connecting client converges to the complete list through the batched reset
// replay, and then to diffs added after.
func TestExchangePeerDiscoveryLargeNetwork(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8094, 9014)
		defer env.Close()

		// more peers than one update frame carries
		initialPeerCount := networkPeersUpdateBatchSize + 10
		registerPeer := func(i int) server.Id {
			clientId := server.NewId()
			model.AddNetworkPeer(
				ctx,
				env.networkId,
				&model.NetworkPeer{
					ClientId:  clientId,
					Principal: fmt.Sprintf("svc-%d", i),
				},
				server.NewId(),
				10*time.Minute,
			)
			return clientId
		}
		peerClientIds := map[server.Id]bool{}
		for i := range initialPeerCount {
			peerClientIds[registerPeer(i)] = true
		}

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{
			Description: "device a",
		})
		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()

		// the reset replay batches the complete list
		ok := waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return len(connected) == initialPeerCount
		})
		connect.AssertEqual(t, ok, true)
		connectedA, _ := clientA.NetworkPeers()
		for _, peer := range connectedA {
			connect.AssertEqual(t, peerClientIds[server.Id(peer.ClientId)], true)
		}

		// diffs after the reset converge too
		diffPeerCount := 20
		for i := range diffPeerCount {
			peerClientIds[registerPeer(initialPeerCount+i)] = true
		}
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return len(connected) == initialPeerCount+diffPeerCount
		})
		connect.AssertEqual(t, ok, true)
	})
}
