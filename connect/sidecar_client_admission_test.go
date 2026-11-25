//go:build sidecar_pending

package connect

// sidecar_clients' admission at the exchange (DESIGN.md 20.7, 20.8): nominations of a
// network's sidecar clients that run together never admit more than its limit, however
// they interleave. The owner row is MG0's, so these run under -tags sidecar_pending,
// through .stack/server-test.sh.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// An exchange of its own, with no listener, whose nominations build residents in process.
func testingSidecarAdmissionExchange(t testing.TB, ctx context.Context) *Exchange {
	t.Helper()
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
	return exchange
}

// count top-level clients of a new network, each a declared sidecar's own client.
func testingSidecarAdmissionClients(t testing.TB, ctx context.Context, count int) (server.Id, []server.Id) {
	t.Helper()
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("admission%s", networkId), server.NewId())
	clientIds := []server.Id{}
	for range count {
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client_owner (client_id, admin_refused, strict_exp, declares_no_egress)
					VALUES ($1, true, true, true)
				`,
				clientId,
			))
		})
		clientIds = append(clientIds, clientId)
	}
	model.Testing_ClearNetworkPeersEnabledCache()
	return networkId, clientIds
}

// The e2e case's shape at the exchange: twelve sidecars of one network nominate at once
// against a limit of three. Exactly three are nominated and counted, and each other one
// is refused as over the limit. Before the gate counted and added in one step, every
// nomination here passed: each read the set before any of them was in it, since a
// connection joined the set only at its announce or its resident's poll.
func TestExchangeSidecarClientNominationsNeverPastLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		limit := 3
		count := 12
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"sidecar_clients": int64(limit)})()
		model.Testing_ForgetSidecarClientMarks()
		exchange := testingSidecarAdmissionExchange(t, ctx)
		networkId, clientIds := testingSidecarAdmissionClients(t, ctx, count)

		nominated := make([]bool, count)
		start := make(chan struct{})
		var group sync.WaitGroup
		for index, clientId := range clientIds {
			group.Go(func() {
				<-start
				nominated[index] = exchange.NominateLocalResident(clientId, server.NewId(), nil)
			})
		}
		close(start)
		group.Wait()

		nominatedCount := 0
		for _, ok := range nominated {
			if ok {
				nominatedCount += 1
			}
		}
		connect.AssertEqual(t, nominatedCount, limit)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(limit))
	})
}

// A nomination that fails after its gate added the client's entry releases it: here the
// client's resident record already names the same instance, so NominateResident refuses,
// and the network's one slot is free again for the next sidecar.
func TestExchangeSidecarClientFailedNominationReleasesItsEntry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"sidecar_clients": 1})()
		model.Testing_ForgetSidecarClientMarks()
		exchange := testingSidecarAdmissionExchange(t, ctx)
		networkId, clientIds := testingSidecarAdmissionClients(t, ctx, 2)

		instanceId := server.NewId()
		standing := &model.NetworkClientResident{
			ClientId:        clientIds[0],
			InstanceId:      instanceId,
			ResidentId:      server.NewId(),
			ResidentHost:    "127.0.0.1",
			ResidentService: "connect",
			ResidentBlock:   "sidecar-admission",
		}
		if !model.NominateResident(ctx, nil, standing, exchange.settings.ExchangeResidentTtl) {
			t.Fatal("the standing resident record was not written")
		}
		connect.AssertEqual(t, exchange.NominateLocalResident(clientIds[0], instanceId, nil), false)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(0))

		connect.AssertEqual(t, exchange.NominateLocalResident(clientIds[1], server.NewId(), nil), true)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
	})
}

// A resident's poll, while it holds a transport: an entry lost while its last write was
// live is put back with the network full, and one that lapsed is refused with the network
// full, the poll answering false so the resident closes, and admitted again with room.
func TestResidentPollKeepsLostAndRefusesLapsedSidecarEntry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceSidecarLimits(true)()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, map[string]int64{"sidecar_clients": 1})()
		model.Testing_ForgetSidecarClientMarks()
		settings := DefaultExchangeSettingsWithBufferSize(4)
		networkId, clientIds := testingSidecarAdmissionClients(t, ctx, 1)
		occupant := server.NewId()

		poll := func(heldExpiryMs int64) (bool, int64) {
			resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
			resident.clientId, resident.residentId, resident.instanceId = clientIds[0], server.NewId(), server.NewId()
			resident.sidecarCountNetworkId = &networkId
			resident.sidecarCountExpiryMs.Store(heldExpiryMs)
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
				ResidentBlock:   "sidecar-admission",
			}
			if !model.NominateResident(ctx, nil, nominee, settings.ExchangeResidentTtl) {
				t.Fatal("resident nomination failed")
			}
			held := resident.exchange.refreshResidentRegistration(resident)
			model.RemoveResidentForClient(ctx, resident.clientId, resident.residentId)
			return held, resident.sidecarCountExpiryMs.Load()
		}

		// lost while live, the network full of another: put back, past the limit
		model.Testing_AddNetworkSidecarClient(ctx, networkId, occupant, time.Hour)
		held, expiryMs := poll(server.NowUtc().Add(time.Hour).UnixMilli())
		connect.AssertEqual(t, held, true)
		connect.AssertEqual(t, server.NowUtc().UnixMilli() < expiryMs, true)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(2))

		// lapsed, the network full: refused, and nothing written
		model.RemoveNetworkSidecarClient(ctx, networkId, clientIds[0], expiryMs)
		held, _ = poll(server.NowUtc().Add(-time.Second).UnixMilli())
		connect.AssertEqual(t, held, false)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))

		// lapsed, with room: admitted again
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, fmt.Sprintf("{sq_%s}connected_sidecar", networkId))
		})
		held, expiryMs = poll(server.NowUtc().Add(-time.Second).UnixMilli())
		connect.AssertEqual(t, held, true)
		connect.AssertEqual(t, server.NowUtc().UnixMilli() < expiryMs, true)
		connect.AssertEqual(t, model.GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
	})
}
