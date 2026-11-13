package connect

// A resident replaced by another for the same client must leave the client's
// peer registration to the replacement (residentReplaced).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The client's connection registers it once, at announce, under the resident
// it finds then. When a nomination replaces that resident (the client's
// transport re-nominating, a reconnect from another host), the replaced
// resident's teardown found the registration still naming it and removed it,
// so the connected client went uncounted until the replacement's first
// heartbeat re-registered it, up to ExchangeResidentTtl/4 (75 s), and the
// network's concurrent client limit let one more client in. That is the
// intermittent count of TestConnectEmbedPlanAllowanceLiftsTheConcurrentLimit
// (delta review D-S3), whose test exchange has no internal ports, so the
// client's transport replaces its own resident in a loop. Now the replaced
// resident's teardown keeps the registration, and a resident that leaves with
// no replacement still removes it.
func TestResidentReplacementKeepsTheClientRegistered(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
		torndown := make(chan server.Id, 8)
		exchange.afterResidentTeardownForTest = func(resident *Resident) {
			torndown <- resident.residentId
		}
		defer func() {
			exchange.Close()
			if !exchange.WaitForIdle(ctx) {
				t.Error("the exchange did not join its residents")
			}
		}()
		waitTeardown := func(residentId server.Id) {
			t.Helper()
			for {
				select {
				case torndownId := <-torndown:
					if torndownId == residentId {
						return
					}
				case <-ctx.Done():
					t.Fatalf("resident %s was never torn down", residentId)
				}
			}
		}
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "residentReplacement", userId)
		model.Testing_ClearNetworkPeersEnabledCache()
		connected := func() []*model.NetworkPeer {
			_, peers := model.GetNetworkPeers(ctx, networkId)
			live := []*model.NetworkPeer{}
			for _, peer := range peers {
				if peer.DisconnectTime == nil {
					live = append(live, peer)
				}
			}
			return live
		}

		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
		instanceId := server.NewId()

		if !exchange.NominateLocalResidentWithContext(ctx, clientId, instanceId, nil) {
			t.Fatal("the client's first resident was not nominated")
		}
		first := model.GetResidentForClient(ctx, clientId, 0)
		if first == nil {
			t.Fatal("no resident record for the client")
		}
		// as the client's announce does: registered under the resident it finds
		model.AddNetworkPeer(ctx, networkId, &model.NetworkPeer{ClientId: clientId}, first.ResidentId, time.Hour)
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, networkId), 1)

		// the client's transport nominates again, replacing that resident
		if !exchange.NominateLocalResidentWithContext(ctx, clientId, instanceId, &first.ResidentId) {
			t.Fatal("the replacement was not nominated")
		}
		waitTeardown(first.ResidentId)
		second := model.GetResidentForClient(ctx, clientId, 0)
		if second == nil || second.ResidentId == first.ResidentId {
			t.Fatal("the replacement does not serve the client")
		}
		// still counted, and listed as connected, not as a disconnect
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, networkId), 1)
		connect.AssertEqual(t, len(connected()), 1)

		// as the replacement's heartbeat re-registers it under its own id
		model.AddNetworkPeer(ctx, networkId, &model.NetworkPeer{ClientId: clientId}, second.ResidentId, time.Hour)
		// the client leaves: its resident ends with no replacement, and takes
		// the registration
		exchange.stateLock.Lock()
		resident := exchange.residents[clientId]
		exchange.stateLock.Unlock()
		if resident == nil {
			t.Fatal("no local resident for the client")
		}
		resident.Cancel()
		waitTeardown(second.ResidentId)
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, networkId), 0)
		connect.AssertEqual(t, len(connected()), 0)
	})
}
