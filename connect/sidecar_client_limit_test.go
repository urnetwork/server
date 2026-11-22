//go:build sidecar_pending

package connect

// sidecar_clients at connect (DESIGN.md 20.7, 20.8): a sidecar client over its network's
// limit is refused at nomination and closed with 4001, on WebSocket and H3, in the shape
// of provider_intent_test.go; and the resident's poll heartbeats a marked client in its
// network's connected sidecar set, a declared one in no {np_} zset, while a sidecar's window
// clients connect past the limit uncounted (§20.8's forwarding row). The owner row is MG0's,
// so these run under -tags sidecar_pending, through .stack/server-test.sh.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Marks a client of the test server's network as a sidecar's.
func (self *providerIntentTestServer) markSidecar(t testing.TB, clientId server.Id, declaresNoEgress bool) {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`
				INSERT INTO network_client_owner (client_id, admin_refused, strict_exp, declares_no_egress)
				VALUES ($1, true, true, $2)
			`,
			clientId,
			declaresNoEgress,
		))
	})
}

// A network at its one sidecar: a second sidecar's connection is refused at nomination
// and closed with the client limit signal, as the concurrent limit closes one.
func testingSidecarLimitServer(t testing.TB, ctx context.Context) (*providerIntentTestServer, string) {
	t.Helper()
	t.Cleanup(model.Testing_SetEnforceSidecarLimits(true))
	t.Cleanup(model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"sidecar_clients": 1}))
	testServer := newProviderIntentTestServer(t, ctx)
	occupant, _ := testServer.createClient()
	testServer.markSidecar(t, occupant, true)
	model.AddNetworkSidecarClient(ctx, testServer.networkId, occupant, time.Hour)
	clientId, byJwt := testServer.createClient()
	testServer.markSidecar(t, clientId, true)
	return testServer, byJwt
}

func TestConnectSidecarClientLimitRefusalKicksH1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		testServer, byJwt := testingSidecarLimitServer(t, ctx)

		refusals := nominationRefusedCounter.WithLabelValues("sidecar_client_limit")
		refusalsBefore := testutil.ToFloat64(refusals)
		kicks := clientLimitKicksCounter.WithLabelValues(connectTransportH1, clientLimitKickCauseConcurrentClientLimit, "enforced")
		kicksBefore := testutil.ToFloat64(kicks)
		ws := testServer.dialH1(t, byJwt, false)
		testingReadClientLimitExceededControl(t, ws)
		testingRequireClientLimitExceededClose(t, ws)
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+1)
		if testutil.ToFloat64(refusals) <= refusalsBefore {
			t.Fatal("the refusal was not the sidecar client limit's")
		}
	})
}

func TestConnectSidecarClientLimitRefusalKicksH3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		testServer, byJwt := testingSidecarLimitServer(t, ctx)

		refusals := nominationRefusedCounter.WithLabelValues("sidecar_client_limit")
		refusalsBefore := testutil.ToFloat64(refusals)
		h3 := testServer.dialH3(t, byJwt, false)
		testingRequireQuicClientLimitExceeded(t, h3.conn)
		if testutil.ToFloat64(refusals) <= refusalsBefore {
			t.Fatal("the refusal was not the sidecar client limit's")
		}
	})
}

// Dark, the same client connects: nothing is counted or refused.
func TestConnectSidecarClientLimitDarkServes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		testServer, byJwt := testingSidecarLimitServer(t, ctx)
		model.Testing_SetEnforceSidecarLimits(false)

		ws := testServer.dialH1(t, byJwt, false)
		ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, message, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("the connection closed while dark: %s", err)
		}
		if len(message) == 5 && message[0] == transportControlClose {
			t.Fatal("a dark limit sent the close")
		}
	})
}

// The resident's poll heartbeats a marked client in its network's connected sidecar set,
// whatever its category: a declared top-level one registers in no {np_} zset and is no
// peer, and a child, which is in no registry at all, counts too.
func TestResidentHeartbeatRegistersSidecarClientApart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettingsWithBufferSize(4)
		settings.EnableNetworkPeers = true
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "testSidecarHeartbeat", server.NewId())

		heartbeat := func(category model.NetworkPeerCategory, topLevel bool) {
			resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
			resident.residentId, resident.instanceId = server.NewId(), server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), resident.clientId, "sidecar", "synthetic")
			if topLevel {
				resident.peerNetworkId = &networkId
				resident.peerCategory = category
			}
			resident.sidecarCountNetworkId = &networkId
			send, receive, detach, err := resident.AddTransport()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				detach()
				if err := resident.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
				returnReadyPooledMessages(send)
				returnReadyPooledMessages(receive)
			}()
			nominee := &model.NetworkClientResident{
				ClientId:        resident.clientId,
				InstanceId:      resident.instanceId,
				ResidentId:      resident.residentId,
				ResidentHost:    "127.0.0.1",
				ResidentService: "connect",
				ResidentBlock:   "sidecar-heartbeat",
			}
			if !model.NominateResident(ctx, nil, nominee, settings.ExchangeResidentTtl) {
				t.Fatal("resident nomination failed")
			}
			if !resident.exchange.refreshResidentRegistration(resident) {
				t.Fatal("the heartbeat dropped a current resident")
			}
		}

		heartbeat(model.NetworkPeerCategorySidecar, true)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, networkId), 0)
		_, peers := model.GetNetworkPeers(ctx, networkId)
		connect.AssertEqual(t, len(peers), 0)

		heartbeat(model.NetworkPeerCategoryClient, false)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(2))
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, networkId), 0)
	})
}

// With push off and sidecar clients counted, a resident reads its client's marks with its
// profile, so a strict_exp client's resident joins the connected sidecar set; dark, it
// reads none, as TestResidentReadsNoMarkWhilePushIsOff holds.
func TestResidentReadsTheMarkWhileSidecarClientsAreCounted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer server.Testing_PushSidecarConfig("sidecar:\n  push: {enabled: false}\n")()
	for _, enforced := range []bool{false, true} {
		func() {
			defer model.Testing_SetEnforceSidecarLimits(enforced)()
			exchange := newTestSidecarPushExchange(ctx, false)
			defer exchange.Close()
			networkId := server.NewId()
			exchange.residentProfileForTest = func(context.Context, server.Id) (server.Id, bool, model.NetworkPeerCategory, *model.NetworkPeer, bool) {
				return networkId, false, model.NetworkPeerCategoryClient, &model.NetworkPeer{}, true
			}
			marksRead := 0
			exchange.residentPushMarksForTest = func(server.Id) model.SidecarPushMarks {
				marksRead += 1
				return model.SidecarPushMarks{StrictExp: true}
			}
			_, _, _, _, _, marks := exchange.loadResidentProfile(ctx, server.NewId())
			connect.AssertEqual(t, marksRead == 1, enforced)
			connect.AssertEqual(t, marks.StrictExp, enforced)
		}()
	}
}

// §20.8's forwarding row on a real exchange: a network at its sidecar_clients limit of one,
// its one sidecar connected and counted, still connects the window client of that
// sidecar's link, which the sidecar's own token mints as its child and which inherits its
// strict_exp, and the window client's resident never counts it, its heartbeat included;
// a second sidecar is still refused at nomination.
func TestExchangeSidecarClientLimitServesWindowClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
			// the self-signed test identity, not the environment's tls.yml
			settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
				return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
			}
		})
		defer env.Close()
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", env.networkId))()
		defer model.Testing_SetEnforceSidecarLimits(true)()
		for _, tier := range []model.NetworkTier{model.NetworkTierFree, model.NetworkTierPro, model.NetworkTierTeam} {
			defer model.Testing_SetSidecarLimits(tier, map[string]int64{"sidecar_clients": 1})()
		}
		model.Testing_ForgetSidecarClientMarks()

		connectClient := func(clientId server.Id, byJwt string) {
			client := env.newClient(clientId)
			transport := env.newTransport(byJwt, server.NewId(), client.RouteManager())
			t.Cleanup(func() {
				transport.Close()
				client.Close()
			})
		}
		residentOf := func(clientId server.Id) *Resident {
			if !waitFor(ctx, 30*time.Second, func() bool { return env.exchange.testingResident(clientId) != nil }) {
				return nil
			}
			return env.exchange.testingResident(clientId)
		}

		sidecarClientId, sidecarJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true, DeclaresNoEgress: true})
		connectClient(sidecarClientId, sidecarJwt)
		sidecarResident := residentOf(sidecarClientId)
		if sidecarResident == nil {
			t.Fatal("the sidecar has no resident")
		}
		if sidecarResident.sidecarCountNetworkId == nil || *sidecarResident.sidecarCountNetworkId != env.networkId {
			t.Fatal("the sidecar's resident does not count it")
		}
		if !waitFor(ctx, 30*time.Second, func() bool { return model.GetNetworkSidecarConnectedCount(ctx, env.networkId) == 1 }) {
			t.Fatal("the sidecar is not counted")
		}

		sidecarByJwt, err := session.ParseByJwt(ctx, sidecarJwt)
		if err != nil {
			t.Fatal(err)
		}
		window, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
			SourceClientId: &sidecarClientId,
			Description:    "window",
			DeviceSpec:     "test",
		}, session.Testing_CreateClientSession(ctx, sidecarByJwt))
		if err != nil {
			t.Fatal(err)
		}
		if window.Error != nil || window.ClientId == nil || window.ByClientJwt == nil || !window.StrictExp {
			t.Fatalf("the window client's mint: %+v %+v", window, window.Error)
		}
		windowClientId := *window.ClientId
		connectClient(windowClientId, *window.ByClientJwt)
		windowResident := residentOf(windowClientId)
		if windowResident == nil {
			t.Fatal("the sidecar's window client was refused at its network's sidecar_clients limit")
		}
		connect.AssertEqual(t, windowResident.sidecarCountNetworkId == nil, true)
		// one heartbeat, as the resident's poll runs it, while the window client holds its transport
		if !waitFor(ctx, 30*time.Second, func() bool { return 0 < windowResident.TransportCount() }) {
			t.Fatal("the window client holds no transport")
		}
		connect.AssertEqual(t, env.exchange.refreshResidentRegistration(windowResident), true)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, env.networkId), int64(1))

		refusals := nominationRefusedCounter.WithLabelValues("sidecar_client_limit")
		refusalsBefore := testutil.ToFloat64(refusals)
		furtherClientId, furtherJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "second sidecar", DeviceSpec: "test", StrictExp: true, DeclaresNoEgress: true})
		connectClient(furtherClientId, furtherJwt)
		if !waitFor(ctx, 30*time.Second, func() bool { return refusalsBefore < testutil.ToFloat64(refusals) }) {
			t.Fatal("a second sidecar was not refused at its network's sidecar_clients limit")
		}
		connect.AssertEqual(t, env.exchange.testingResident(furtherClientId) == nil, true)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, env.networkId), int64(1))
	})
}
