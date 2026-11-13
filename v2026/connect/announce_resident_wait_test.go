package connect

// A connection whose announce runs before its transport has a resident must
// still register the client once the resident exists, and only while the
// client is still an active device of the network (registerWhenResident).

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A connect handler whose exchange refuses every nomination while refusing is
// set, as a failed first nomination would, and whose announce reports its
// resident lookup on announced and the late registration's outcome on found.
type announceResidentWaitServer struct {
	*providerIntentTestServer
	refusing  *atomic.Bool
	announced chan bool
	found     chan bool
}

func newAnnounceResidentWaitServer(t testing.TB, ctx context.Context) *announceResidentWaitServer {
	exchangeSettings := DefaultExchangeSettings()
	exchangeSettings.KeyEventDelivery.Enabled = false
	exchange := NewExchange(
		ctx,
		"host0",
		"test",
		"test",
		map[int]int{},
		map[string]string{"host0": "127.0.0.1"},
		exchangeSettings,
	)
	t.Cleanup(exchange.Close)
	refusing := &atomic.Bool{}
	refusing.Store(true)
	exchange.refuseNominationForTest = func(server.Id) bool {
		return refusing.Load()
	}

	announced := make(chan bool, 4)
	found := make(chan bool, 4)
	handlerId := model.CreateNetworkClientHandler(ctx)
	settings := DefaultConnectHandlerSettings()
	settings.ConnectionAnnounceTimeout = 0
	settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	settings.ConnectionAnnounceSettings.peerRegisterLookedUpForTest = func(lookedUpFound bool) {
		announced <- lookedUpFound
	}
	settings.ConnectionAnnounceSettings.peerRegisterResidentFoundForTest = func(registered bool) {
		found <- registered
	}
	h3PacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewConnectHandlerWithPacketConns(ctx, handlerId, exchange, settings, ConnectHandlerPacketConns{
		H3: h3PacketConn,
	})
	t.Cleanup(handler.Close)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: http.HandlerFunc(handler.Connect)}
	go httpServer.Serve(listener)
	t.Cleanup(func() {
		httpServer.Close()
	})
	testServer := &providerIntentTestServer{
		ctx:         ctx,
		h1Address:   listener.Addr().String(),
		h3Address:   h3PacketConn.LocalAddr().String(),
		networkId:   server.NewId(),
		userId:      server.NewId(),
		networkName: "announceResidentWait",
	}
	model.Testing_CreateNetwork(ctx, testServer.networkId, testServer.networkName, testServer.userId)
	model.Testing_ClearNetworkPeersEnabledCache()
	return &announceResidentWaitServer{
		providerIntentTestServer: testServer,
		refusing:                 refusing,
		announced:                announced,
		found:                    found,
	}
}

// Waits for the announce's resident lookup, which must have found none.
func (self *announceResidentWaitServer) requireAnnouncedWithoutResident(t testing.TB, ctx context.Context, clientId server.Id) {
	t.Helper()
	select {
	case lookedUpFound := <-self.announced:
		if lookedUpFound {
			t.Fatal("the announce found a resident the exchange never nominated")
		}
	case <-ctx.Done():
		t.Fatal("the connection never announced")
	}
	connect.AssertEqual(t, model.GetResidentForClient(ctx, clientId, 0) == nil, true)
	connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, self.networkId), 0)
}

// Waits for the late registration's outcome once the resident exists.
func (self *announceResidentWaitServer) requireLateRegistration(t testing.TB, ctx context.Context, want bool) {
	t.Helper()
	select {
	case registered := <-self.found:
		connect.AssertEqual(t, registered, want)
	case <-ctx.Done():
		t.Fatal("the late registration never found the resident")
	}
}

// The announce registered the client under the resident it found, and with
// none (the transport's first nomination refused or failed, the next one a
// reconnect interval later) it left the registration to the resident's first
// heartbeat, up to ExchangeResidentTtl/4 (75 s) after the resident started:
// the client was admitted and connected but uncounted, so the network's
// concurrent client limit let one more client in. That is the remaining
// intermittent count of TestConnectEmbedPlanAllowanceLiftsTheConcurrentLimit
// (delta review D-S3), whose announce runs at once (ConnectionAnnounceTimeout
// 0) beside the transport's nomination. Here the exchange refuses the first
// nominations, as a failed first nomination would, so the announce
// deterministically finds no resident; once a nomination admits the client,
// it must be registered within seconds.
func TestAnnounceRegistersAClientAdmittedAfterIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		testServer := newAnnounceResidentWaitServer(t, ctx)

		clientId, byJwt := testServer.createClient()
		testServer.dialH1(t, byJwt, false)
		testServer.requireAnnouncedWithoutResident(t, ctx, clientId)

		// the transport's next nomination admits the client, and the client is
		// registered
		testServer.refusing.Store(false)
		waitForEmbedCondition(t, ctx, "the client was admitted", func() bool {
			return model.GetResidentForClient(ctx, clientId, 0) != nil
		})
		testServer.requireLateRegistration(t, ctx, true)
		deadline := time.Now().Add(20 * time.Second)
		for model.GetNetworkConnectedCount(ctx, testServer.networkId) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("the client admitted after its announce was never registered")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// A device removed from the network while its connection's late registration
// waits must not be registered when a nomination admits it before the
// removal's close reaches its transport: the removal frees its slot at once
// (releaseRemovedClientSlot), and registering it again with the profile read
// before the wait counted it for about 5 minutes, until its resident's idle
// teardown, so the device the user meant to add was refused into a new hold
// (delta review 2, R-S1). The late registration reads the profile again, as
// the heartbeat's re-add does, and registers only an active device.
func TestAnnounceDoesNotRegisterADeviceRemovedWhileItWaited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		testServer := newAnnounceResidentWaitServer(t, ctx)

		clientId, byJwt := testServer.createClient()
		testServer.dialH1(t, byJwt, false)
		testServer.requireAnnouncedWithoutResident(t, ctx, clientId)

		// removed from the device list with the network's credential
		networkSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(
			testServer.networkId,
			testServer.userId,
			testServer.networkName,
			false,
			false,
		))
		removed, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: clientId}, networkSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error, (*model.RemoveNetworkClientError)(nil))
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, testServer.networkId), 0)

		// a nomination still admits it before the close reaches its transport:
		// the late registration finds the resident and must not count it
		testServer.refusing.Store(false)
		testServer.requireLateRegistration(t, ctx, false)
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, testServer.networkId), 0)
	})
}
