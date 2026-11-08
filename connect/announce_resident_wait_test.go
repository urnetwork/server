package connect

// A connection whose announce runs before its transport has a resident must
// still register the client once the resident exists (registerWhenResident).

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

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
		// refuse the client's nominations, as a failed first nomination does,
		// until the announce has looked
		var refusing atomic.Bool
		refusing.Store(true)
		exchange.refuseNominationForTest = func(server.Id) bool {
			return refusing.Load()
		}

		announced := make(chan bool, 4)
		handlerId := model.CreateNetworkClientHandler(ctx)
		settings := DefaultConnectHandlerSettings()
		settings.ConnectionAnnounceTimeout = 0
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		settings.ListenH3Port = 0
		settings.ListenDnsPort = 0
		settings.EnableProxyProtocol = false
		settings.TransportTlsSettings.EnableSelfSign = true
		settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
		settings.ConnectionAnnounceSettings.peerRegisterLookedUpForTest = func(found bool) {
			announced <- found
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

		clientId, byJwt := testServer.createClient()
		ws := testServer.dialH1(t, byJwt, false)
		_ = ws

		// the announce looked before any resident existed
		select {
		case found := <-announced:
			if found {
				t.Fatal("the announce found a resident the draining exchange never nominated")
			}
		case <-ctx.Done():
			t.Fatal("the connection never announced")
		}
		connect.AssertEqual(t, model.GetResidentForClient(ctx, clientId, 0) == nil, true)
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, testServer.networkId), 0)

		// the transport's next nomination admits the client, and the client is
		// registered
		refusing.Store(false)
		waitForEmbedCondition(t, ctx, "the client was admitted", func() bool {
			return model.GetResidentForClient(ctx, clientId, 0) != nil
		})
		deadline := time.Now().Add(20 * time.Second)
		for model.GetNetworkConnectedCount(ctx, testServer.networkId) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("the client admitted after its announce was never registered")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
