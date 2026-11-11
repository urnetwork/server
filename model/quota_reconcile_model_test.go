//go:build sidecar_pending

package model

// QUOTA-M2-12's nightly reconcile: an injected storage_over corrected from the count and the limit, a
// connected set's expired member dropped and its live one kept, a spent mark no balance backs lifted, all
// refusing nothing, and the pass paged by network.

import (
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestReconcileSidecarCountersCorrectsAndRefusesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1024 * 1024 * 1024, "sidecar_clients": 20})()
		now := server.NowUtc()
		under := server.NewId()
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, under, 10), nil)
		// injected: over its limit by the flag, under it by the count
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_quota_state SET storage_over = true WHERE network_id = $1`, under))
		})
		// a spent mark with an in-window balance holding room
		spent := server.NewId()
		testingAddTierBalance(t, ctx, spent, false, now.Add(-time.Hour), now.Add(time.Hour))
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, spent, 0), nil)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, spent, now), nil)
		// a connected set with one expired member and one live one
		key := networkSidecarConnectedKey(under)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZAdd(ctx, key,
				redis.Z{Score: float64(now.Add(-time.Second).UnixMilli()), Member: "expired"},
				redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: "live"},
			).Err())
		})

		result, err := ReconcileSidecarCounters(ctx, now, nil, 1)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.LookedCount, 1)
		connect.AssertEqual(t, result.LastNetworkId != nil, true)
		connect.AssertEqual(t, result.SpentCleared, 1)
		for result.LastNetworkId != nil {
			next, err := ReconcileSidecarCounters(ctx, now, result.LastNetworkId, 1)
			connect.AssertEqual(t, err, nil)
			// only the first page runs the spent pass
			connect.AssertEqual(t, next.SpentCleared, 0)
			result.StorageOverCorrected += next.StorageOverCorrected
			result.ConnectedExpiredRemoved += next.ConnectedExpiredRemoved
			result.LastNetworkId = next.LastNetworkId
		}
		connect.AssertEqual(t, result.StorageOverCorrected, 1)
		connect.AssertEqual(t, result.ConnectedExpiredRemoved, int64(1))

		var storageOver bool
		var spentTime *time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT storage_over FROM network_quota_state WHERE network_id = $1`, under).Scan(&storageOver))
			server.Raise(conn.QueryRow(ctx, `SELECT spent_time FROM network_quota_state WHERE network_id = $1`, spent).Scan(&spentTime))
		})
		connect.AssertEqual(t, storageOver, false)
		connect.AssertEqual(t, spentTime == nil, true)
		server.Redis(ctx, func(client server.RedisClient) {
			members, err := client.ZRangeByScore(ctx, key, &redis.ZRangeBy{Min: "-inf", Max: "+inf"}).Result()
			server.Raise(err)
			connect.AssertEqual(t, members, []string{"live"})
		})
	})
}
