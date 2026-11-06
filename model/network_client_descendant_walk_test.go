package model

// Retiring a window must not read its device's history. The descendant walk
// (deactivateLockedNetworkClientsInTx) runs on every client a parent retires,
// and nearly every one is a leaf window: a hosted proxy or an app retires its
// windows through its own credential (localclient.Authority or
// /network/remove-client), which lands in deactivateOwnedNetworkClientInTx.
// The walk found a parent's children through network_client_device_id over the
// parent's whole device, every window the device ever minted, active or
// retired and kept for the reap. On 2026-11-06 proxy 4812cbe46 ran it on every
// window retirement and saturated the Main primary: 309 walks at once at about
// 9 s each, about 56k buffers a call, for 1,524 children in 1.33M calls.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// An id minted at mintTime: the millisecond prefix server.NewId writes, with
// random entropy after it.
func descendantWalkTestIdAt(mintTime time.Time) server.Id {
	id := server.NewId()
	ms := mintTime.UnixMilli()
	id[0] = byte(ms >> 40)
	id[1] = byte(ms >> 32)
	id[2] = byte(ms >> 24)
	id[3] = byte(ms >> 16)
	id[4] = byte(ms >> 8)
	id[5] = byte(ms)
	return id
}

// Adds count children of parentClientId on deviceId, minted at mintTime, every
// other one retired and kept, as the retired hosted reap keeps windows a day.
func descendantWalkTestSeedChildren(
	ctx context.Context,
	networkId server.Id,
	deviceId server.Id,
	parentClientId server.Id,
	mintTime time.Time,
	count int,
) []server.Id {
	clientIds := make([]server.Id, count)
	for i := range clientIds {
		clientIds[i] = descendantWalkTestIdAt(mintTime.Add(time.Duration(i) * time.Millisecond))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client (
					client_id,
					network_id,
					device_id,
					description,
					create_time,
					auth_time,
					source_client_id,
					active,
					deactivate_time
				)
				SELECT
					seeded.client_id,
					$2,
					$3,
					'window',
					$4,
					$4,
					$5,
					seeded.n % 2 = 0,
					CASE WHEN seeded.n % 2 = 0 THEN NULL ELSE $4::timestamp END
				FROM unnest($1::uuid[]) WITH ORDINALITY AS seeded(client_id, n)
			`,
			clientIds,
			networkId,
			deviceId,
			mintTime,
			parentClientId,
		))
	})
	return clientIds
}

// The network_client rows this transaction has read so far: rows of sequential
// scans and entries of every scan of a network_client index (index, index-only
// and bitmap), read before the transaction commits (pg_stat_get_xact_*).
func descendantWalkTestRowsRead(ctx context.Context, tx server.PgTx) (rowsRead int64) {
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT (
				pg_stat_get_xact_tuples_returned('network_client'::regclass) +
				COALESCE((
					SELECT sum(pg_stat_get_xact_tuples_returned(pg_index.indexrelid))
					FROM pg_index
					WHERE pg_index.indrelid = 'network_client'::regclass
				), 0)
			)::bigint
		`,
	).Scan(&rowsRead))
	return
}

// The owner retires its child as RemoveNetworkClient does for a client token,
// and the network_client rows the retirement read. The test table is small
// enough that the planner may scan it sequentially, which the production table
// never is; without that, either version reads through its indexes, as on Main.
func descendantWalkTestRetire(
	ctx context.Context,
	clientId server.Id,
	networkId server.Id,
	ownerClientId server.Id,
) (removed bool, rowsRead int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
		before := descendantWalkTestRowsRead(ctx, tx)
		var err error
		removed, err = deactivateOwnedNetworkClientInTx(ctx, tx, clientId, networkId, ownerClientId)
		server.Raise(err)
		rowsRead = descendantWalkTestRowsRead(ctx, tx) - before
	}, server.TxReadCommitted)
	return
}

// A window's handful of own reads (its lock, its ancestry for the revocation
// record, its update) stay far under this; the device history below is 2,000.
const descendantWalkTestRowReadLimit = 100

func TestRetiringAWindowDoesNotReadItsDeviceHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		// the device's history: windows minted two days ago, half retired
		history := descendantWalkTestSeedChildren(ctx, networkId, deviceId, parentClientId, server.NowUtc().Add(-48*time.Hour), 2000)
		// the window the parent retires now, a leaf like nearly every one
		windowClientId := descendantTestChild(ctx, t, userSession, parentClientId, deviceId)

		removed, rowsRead := descendantWalkTestRetire(ctx, windowClientId, networkId, parentClientId)
		t.Logf("the retirement read %d network_client rows", rowsRead)
		connect.AssertEqual(t, removed, true)
		connect.AssertEqual(t, clientTokenTestActive(ctx, windowClientId), false)
		if descendantWalkTestRowReadLimit <= rowsRead {
			t.Fatalf("retiring one leaf window read %d network_client rows; the device's history is %d", rowsRead, len(history))
		}
		// the parent and its other windows are not touched
		connect.AssertEqual(t, clientTokenTestActive(ctx, parentClientId), true)
		connect.AssertEqual(t, clientTokenTestActive(ctx, history[1]), true)
	})
}

// The walk reads the parent's device only. A network's active clients on its
// other devices, however recent, are not read: a hosted proxy network holds
// hundreds of proxies, each with its own device and windows.
func TestRetiringAWindowDoesNotReadOtherDevices(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		windowClientId := descendantTestChild(ctx, t, userSession, parentClientId, deviceId)
		// another proxy of the network with windows minted after this window
		otherClientId, otherDeviceId := authClientTestClient(ctx, t, userSession)
		otherWindows := descendantWalkTestSeedChildren(ctx, networkId, otherDeviceId, otherClientId, server.NowUtc().Add(time.Second), 2000)

		removed, rowsRead := descendantWalkTestRetire(ctx, windowClientId, networkId, parentClientId)
		t.Logf("the retirement read %d network_client rows", rowsRead)
		connect.AssertEqual(t, removed, true)
		if descendantWalkTestRowReadLimit <= rowsRead {
			t.Fatalf("retiring one leaf window read %d network_client rows; other devices hold %d", rowsRead, len(otherWindows))
		}
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherWindows[1]), true)
	})
}

// A retired window's own child goes with it, and the walk still reads only
// what the window's device minted since the window.
func TestRetiringAWindowWithAChildReadsOnlySinceTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		descendantWalkTestSeedChildren(ctx, networkId, deviceId, parentClientId, server.NowUtc().Add(-48*time.Hour), 2000)
		windowClientId := descendantTestChild(ctx, t, userSession, parentClientId, deviceId)
		childClientId := descendantTestChild(ctx, t, userSession, windowClientId, deviceId)
		grandchildClientId := descendantTestChild(ctx, t, userSession, childClientId, deviceId)

		removed, rowsRead := descendantWalkTestRetire(ctx, windowClientId, networkId, parentClientId)
		t.Logf("the retirement read %d network_client rows", rowsRead)
		connect.AssertEqual(t, removed, true)
		for _, clientId := range []server.Id{windowClientId, childClientId, grandchildClientId} {
			if clientTokenTestActive(ctx, clientId) {
				t.Fatalf("client %s is still active after its ancestor window was retired", clientId)
			}
		}
		connect.AssertEqual(t, clientTokenTestActive(ctx, parentClientId), true)
		if descendantWalkTestRowReadLimit <= rowsRead {
			t.Fatalf("retiring a window and its two descendants read %d network_client rows", rowsRead)
		}
	})
}
