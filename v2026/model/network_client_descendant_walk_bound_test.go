package model

// The descendant walk reads a parent's device only from the parent's mint time
// on, less descendantMintClockSkew (readActiveChildClientIdsInTx). These pin
// that bound: what it is, that a child minted within the tolerated clock
// difference is still retired with its parent, and that a long-lived parent's
// whole history still goes with it.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// An active child of parentClientId on deviceId, minted at mintTime.
func descendantWalkTestInsertChild(
	ctx context.Context,
	networkId server.Id,
	deviceId server.Id,
	parentClientId server.Id,
	mintTime time.Time,
) (clientId server.Id) {
	clientId = descendantWalkTestIdAt(mintTime)
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
					source_client_id
				)
				VALUES ($1, $2, $3, 'window', $4, $4, $5)
			`,
			clientId,
			networkId,
			deviceId,
			mintTime,
			parentClientId,
		))
	})
	return
}

func descendantWalkTestCreateTime(ctx context.Context, clientId server.Id) (createTime time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT create_time FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&createTime))
	})
	return
}

func TestDescendantMinClientIdIsTheParentMintTimeLessTheSkew(t *testing.T) {
	mintTime := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	parentClientId := descendantWalkTestIdAt(mintTime)

	minClientId := descendantMinClientId(parentClientId, mintTime)
	connect.AssertEqual(t, idMintMillis(minClientId), mintTime.Add(-descendantMintClockSkew).UnixMilli())
	for _, b := range minClientId[6:] {
		if b != 0 {
			t.Fatalf("the bound %s carries entropy", minClientId)
		}
	}
	// a child minted at the edge of the tolerated skew sorts at or after the
	// bound, and one a millisecond earlier before it
	if descendantWalkTestIdAt(mintTime.Add(-descendantMintClockSkew)).Cmp(minClientId) < 0 {
		t.Fatal("a child minted at the tolerated skew sorts before the bound")
	}
	if 0 <= descendantWalkTestIdAt(mintTime.Add(-descendantMintClockSkew-time.Millisecond)).Cmp(minClientId) {
		t.Fatal("a child minted beyond the tolerated skew sorts at or after the bound")
	}

	// the earlier of the parent's id time and create time
	connect.AssertEqual(t, descendantMinClientId(parentClientId, mintTime.Add(time.Hour)), minClientId)
	earlierCreateTime := mintTime.Add(-time.Hour)
	connect.AssertEqual(
		t,
		idMintMillis(descendantMinClientId(parentClientId, earlierCreateTime)),
		earlierCreateTime.Add(-descendantMintClockSkew).UnixMilli(),
	)

	// an id or a create time that is not a mint time bounds nothing
	connect.AssertEqual(t, descendantMinClientId(server.Id{}, mintTime), server.Id{})
	connect.AssertEqual(t, descendantMinClientId(parentClientId, time.Time{}), server.Id{})
}

// A child minted on a host whose clock is behind its parent's sorts before its
// parent's mint time. Within descendantMintClockSkew it is retired with the
// parent. Beyond it the walk does not find it, and it stays to the child reap;
// that is the bound working, pinned so that a change to it is deliberate.
func TestDescendantWalkToleratesMintClockSkew(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		parentMintTime := time.UnixMilli(min(
			idMintMillis(parentClientId),
			descendantWalkTestCreateTime(ctx, parentClientId).UnixMilli(),
		)).UTC()
		withinSkewClientId := descendantWalkTestInsertChild(ctx, networkId, deviceId, parentClientId, parentMintTime.Add(-descendantMintClockSkew+time.Minute))
		beyondSkewClientId := descendantWalkTestInsertChild(ctx, networkId, deviceId, parentClientId, parentMintTime.Add(-descendantMintClockSkew-time.Minute))

		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: parentClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, clientTokenTestActive(ctx, parentClientId), false)
		if clientTokenTestActive(ctx, withinSkewClientId) {
			t.Fatal("a child minted within the tolerated clock skew is still active after its parent was removed")
		}
		if !clientTokenTestActive(ctx, beyondSkewClientId) {
			t.Fatal("a child minted beyond the tolerated clock skew was retired; the walk's bound changed")
		}
	})
}

// A long-lived parent's removal still retires every active child its device
// holds, however long ago each was minted after the parent. The parent's
// create time here is earlier than its id time, so the bound follows the
// earlier of the two.
func TestRemovingALongLivedParentRetiresItsWholeHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		parentClientId, deviceId := authClientTestClient(ctx, t, userSession)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET create_time = $2 WHERE client_id = $1`,
				parentClientId,
				server.NowUtc().Add(-72*time.Hour),
			))
		})
		history := descendantWalkTestSeedChildren(ctx, networkId, deviceId, parentClientId, server.NowUtc().Add(-48*time.Hour), 2000)
		windowClientId := descendantTestChild(ctx, t, userSession, parentClientId, deviceId)
		otherClientId, otherDeviceId := authClientTestClient(ctx, t, userSession)
		otherWindowClientId := descendantTestChild(ctx, t, userSession, otherClientId, otherDeviceId)

		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: parentClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, descendantTestActiveChildCount(ctx, parentClientId), 0)
		connect.AssertEqual(t, clientTokenTestActive(ctx, windowClientId), false)
		connect.AssertEqual(t, clientTokenTestActive(ctx, history[1]), false)
		// one deactivation, one clock, for the old children too
		parentDeactivateTime := descendantTestDeactivateTime(ctx, parentClientId)
		historyDeactivateTime := descendantTestDeactivateTime(ctx, history[1])
		if parentDeactivateTime == nil || historyDeactivateTime == nil || !historyDeactivateTime.Equal(*parentDeactivateTime) {
			t.Fatalf("an old child deactivated at %v, its parent at %v", historyDeactivateTime, parentDeactivateTime)
		}
		// another client of the network and its window are not touched
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherClientId), true)
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherWindowClientId), true)
	})
}
