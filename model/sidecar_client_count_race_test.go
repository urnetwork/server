//go:build sidecar_pending

package model

// sidecar_clients as a hard cap at connect (sidecar-clients-quota-race-connect-admission):
// the gate counts and adds in one step, so gates that run before any of their connections
// is marked, one after another or all at once, never admit more than the limit; a failed
// connect releases what its gate added; a resident's poll keeps its entry, puts back one
// lost while live and admits again one that lapsed; and a resident's close keeps a newer
// connection's entry. MG0 holds the owner row, so these run under -tags sidecar_pending,
// through .stack/server-test.sh.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A network of count declared sidecar clients, and none connected.
func testingSidecarRaceClients(t testing.TB, ctx context.Context, count int) (server.Id, []server.Id) {
	t.Helper()
	kinds := testingSidecarCountKinds(t, ctx)
	clientIds := []server.Id{}
	for range count {
		clientIds = append(clientIds, testingSidecarCountClient(t, ctx, kinds.networkId, nil, true, true, true))
	}
	return kinds.networkId, clientIds
}

// The root cause, in order: every gate runs before any connection's announce or poll. The
// gate used to read the count and write nothing, so all twelve passed here; it now adds
// each admitted client as it counts, so the fourth and every later one is refused, with
// the limit full and nothing added.
func TestSidecarClientGatesBeforeAnyMarkNeverPassLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 3})()
		networkId, clientIds := testingSidecarRaceClients(t, ctx, 12)

		for index, clientId := range clientIds {
			admission := AdmitSidecarClient(ctx, clientId, time.Hour)
			if index < 3 {
				connect.AssertEqual(t, admission.Admitted, true)
				connect.AssertEqual(t, admission.Added, true)
				connect.AssertEqual(t, admission.UsedCount, int64(index+1))
			} else {
				connect.AssertEqual(t, admission.Admitted, false)
				connect.AssertEqual(t, admission.Added, false)
				connect.AssertEqual(t, admission.Limit, int64(3))
				connect.AssertEqual(t, admission.UsedCount, int64(3))
			}
		}
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(3))
	})
}

// The e2e case's burst: twelve gates of one network released together by one barrier,
// round after round on a fresh network, against a limit of three: each round admits
// exactly three, the set holds exactly three, and every refusal found the limit full.
func TestSidecarClientConcurrentGatesNeverPassLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 3})()

		for round := range 8 {
			networkId, clientIds := testingSidecarRaceClients(t, ctx, 12)
			// each mark read and kept first, so the barrier releases the gates' Redis
			// steps alone
			for _, clientId := range clientIds {
				getSidecarClientMark(ctx, clientId)
			}
			admissions := make([]*SidecarClientAdmission, len(clientIds))
			start := make(chan struct{})
			var group sync.WaitGroup
			for index, clientId := range clientIds {
				group.Go(func() {
					<-start
					admissions[index] = AdmitSidecarClient(ctx, clientId, time.Hour)
				})
			}
			close(start)
			group.Wait()

			admitted := 0
			for _, admission := range admissions {
				if admission.Admitted {
					admitted += 1
					connect.AssertEqual(t, admission.Added, true)
				} else {
					connect.AssertEqual(t, admission.UsedCount, int64(3))
				}
			}
			if admitted != 3 {
				t.Fatalf("round %d: %d of 12 gates admitted against a limit of 3", round, admitted)
			}
			connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(3))
		}
	})
}

// A connect that fails after its gate releases the entry the gate added, and the slot is
// the next client's; a release keeps an entry a newer connection of the client wrote since;
// a re-nomination's admission, which added nothing, releases nothing; and two gates of one
// client in the same millisecond keep distinct expiries, so neither's release removes the
// other's entry.
func TestSidecarClientReleaseOfAFailedConnect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 1})()
		networkId, clientIds := testingSidecarRaceClients(t, ctx, 4)

		failed := AdmitSidecarClient(ctx, clientIds[0], time.Hour)
		connect.AssertEqual(t, failed.Added, true)
		connect.AssertEqual(t, AdmitSidecarClient(ctx, clientIds[1], time.Hour).Admitted, false)
		ReleaseSidecarClientAdmission(ctx, failed)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(0))
		next := AdmitSidecarClient(ctx, clientIds[1], time.Hour)
		connect.AssertEqual(t, next.Admitted && next.Added, true)

		// a newer connection of the same client wrote a later expiry: the failed one's
		// release leaves it
		renominated := AdmitSidecarClient(ctx, clientIds[1], 2*time.Hour)
		connect.AssertEqual(t, renominated.Admitted, true)
		connect.AssertEqual(t, renominated.Added, false)
		connect.AssertEqual(t, next.ExpiryMs < renominated.ExpiryMs, true)
		ReleaseSidecarClientAdmission(ctx, next)
		ReleaseSidecarClientAdmission(ctx, renominated)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[1], renominated.ExpiryMs)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(0))

		// two gates of one client on the same clock, as two transports of one client
		// nominate at once within a millisecond: the second keeps the entry a millisecond
		// later, so the first's release, its nomination having lost, keeps the second's
		now := server.NowUtc()
		expiryMs := now.Add(time.Hour).UnixMilli()
		gate := func() []int64 {
			var values []int64
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				values, err = sidecarClientAdmitScript.Run(
					ctx,
					r,
					[]string{networkSidecarConnectedKey(networkId)},
					now.UnixMilli(),
					int64(64),
					string(clientIds[2].Bytes()),
					expiryMs,
					int64(1),
					int64(3600),
				).Int64Slice()
				if err != nil {
					panic(err)
				}
			})
			return values
		}
		first := gate()
		second := gate()
		connect.AssertEqual(t, first[0] == 1 && first[2] == 1 && first[3] == expiryMs, true)
		connect.AssertEqual(t, second[0] == 1 && second[2] == 0 && second[3] == expiryMs+1, true)
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[2], first[3])
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[2], second[3])
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(0))
	})
}

// A resident's poll: a live entry is kept at the later expiry; entries Redis lost while
// their residents' last writes were live come back whatever the count, so connected clients
// stay, past the limit by the clients the gate admitted meanwhile, the soft window the
// header states; and an entry that lapsed is a new connection, admitted again while there
// is room and refused, writing nothing, once the network is full.
func TestSidecarClientRefreshKeepsRestoresAndReadmits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 2})()
		networkId, clientIds := testingSidecarRaceClients(t, ctx, 6)

		first := AdmitSidecarClient(ctx, clientIds[0], time.Hour)
		kept := RefreshNetworkSidecarClient(ctx, networkId, clientIds[0], 2*time.Hour, first.ExpiryMs)
		connect.AssertEqual(t, kept.Held && !kept.Restored && !kept.Lapsed, true)
		connect.AssertEqual(t, first.ExpiryMs < kept.ExpiryMs, true)
		second := AdmitSidecarClient(ctx, clientIds[1], time.Hour)
		connect.AssertEqual(t, second.Added, true)

		// Redis loses the set; two new sidecars take the slots before the residents poll
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, networkSidecarConnectedKey(networkId))
		})
		third := AdmitSidecarClient(ctx, clientIds[2], time.Hour)
		fourth := AdmitSidecarClient(ctx, clientIds[3], time.Hour)
		connect.AssertEqual(t, third.Admitted && fourth.Admitted, true)
		restoredExpiryMs := map[server.Id]int64{}
		for _, c := range []struct {
			clientId     server.Id
			heldExpiryMs int64
		}{
			{clientId: clientIds[0], heldExpiryMs: kept.ExpiryMs},
			{clientId: clientIds[1], heldExpiryMs: second.ExpiryMs},
		} {
			restored := RefreshNetworkSidecarClient(ctx, networkId, c.clientId, time.Hour, c.heldExpiryMs)
			connect.AssertEqual(t, restored.Held && restored.Restored && !restored.Lapsed, true)
			restoredExpiryMs[c.clientId] = restored.ExpiryMs
		}
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(4))

		// a resident with no transport for a whole ttl: its entry lapsed, and the
		// network is full, so its poll is refused and writes nothing
		lapsedExpiryMs := server.NowUtc().Add(-time.Second).UnixMilli()
		Testing_AddNetworkSidecarClient(ctx, networkId, clientIds[4], -time.Second)
		refused := RefreshNetworkSidecarClient(ctx, networkId, clientIds[4], time.Hour, lapsedExpiryMs)
		connect.AssertEqual(t, refused.Lapsed && !refused.Held && !refused.Restored, true)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(4))

		// three leave, each resident's close removing the entry it last wrote; the same
		// poll, with room, admits it again
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[1], restoredExpiryMs[clientIds[1]])
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[2], third.ExpiryMs)
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[3], fourth.ExpiryMs)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		readmitted := RefreshNetworkSidecarClient(ctx, networkId, clientIds[4], time.Hour, lapsedExpiryMs)
		connect.AssertEqual(t, readmitted.Lapsed && readmitted.Held, true)
		connect.AssertEqual(t, lapsedExpiryMs < readmitted.ExpiryMs, true)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(2))
		connect.AssertEqual(t, AdmitSidecarClient(ctx, clientIds[5], time.Hour).Admitted, false)
	})
}

// A resident replaced by a newer one of the same client: the newer gate keeps the client's
// entry at its later expiry, and the replaced resident's close, which removes only the
// entry its own last write left, keeps it; the newer resident's close removes it; and a
// resident that held no entry removes nothing.
func TestSidecarClientRemovalKeepsANewerConnectionsEntry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 1})()
		networkId, clientIds := testingSidecarRaceClients(t, ctx, 2)

		replaced := AdmitSidecarClient(ctx, clientIds[0], time.Hour)
		replacing := AdmitSidecarClient(ctx, clientIds[0], 2*time.Hour)
		connect.AssertEqual(t, replacing.Admitted && !replacing.Added, true)
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[0], replaced.ExpiryMs)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		// the slot stays the client's: a further sidecar is refused
		connect.AssertEqual(t, AdmitSidecarClient(ctx, clientIds[1], time.Hour).Admitted, false)

		RemoveNetworkSidecarClient(ctx, networkId, clientIds[0], 0)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		RemoveNetworkSidecarClient(ctx, networkId, clientIds[0], replacing.ExpiryMs)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(0))
		connect.AssertEqual(t, AdmitSidecarClient(ctx, clientIds[1], time.Hour).Admitted, true)
	})
}
