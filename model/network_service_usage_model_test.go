//go:build sidecar_pending

package model

// QUOTA-M2-12: GET /network/usage's body for a Community and a Pro network, paged, with the helper meters
// hidden and nothing past 13 months; RollupServiceUsage resuming from its cursor without double rows, a closed
// day's row never changed; and the usage tables' retention ending at each ttl in bounded passes.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func testingInsertUsageDay(ctx context.Context, networkId server.Id, day time.Time, meter string, quantity int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_service_usage (network_id, usage_day, meter, quantity) VALUES ($1, $2, $3, $4)`,
			networkId,
			networkUsageDay(day),
			meter,
			quantity,
		))
	})
}

func testingUsageQuantity(ctx context.Context, networkId server.Id, day time.Time, meter string) (quantity int64, found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT quantity FROM network_service_usage WHERE network_id = $1 AND usage_day = $2 AND meter = $3`,
			networkId,
			networkUsageDay(day),
			meter,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&quantity))
				found = true
			}
		})
	})
	return
}

func TestNetworkUsageBody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"storage": 1024 * 1024 * 1024, "sidecar_clients": 20})()
		now := server.NowUtc()
		free := server.NewId()
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, free, 1234), nil)
		yesterday := now.AddDate(0, 0, -1)
		testingInsertUsageDay(ctx, free, yesterday, "evs", 10)
		testingInsertUsageDay(ctx, free, yesterday, NetworkUsageStorageMeter, 1234)
		testingInsertUsageDay(ctx, free, yesterday, "team_credit_window", 99)
		testingInsertUsageDay(ctx, free, now.AddDate(0, 0, -2), "tls", 5)
		testingInsertUsageDay(ctx, free, now.AddDate(0, -14, 0), "tls", 7)

		usage, err := GetNetworkUsage(ctx, free, nil, 2, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, usage.Limits.Tier, "free")
		connect.AssertEqual(t, usage.StoredByteCount, int64(1234))
		connect.AssertEqual(t, usage.StorageOver, networkStorageOver(1234, NetworkStorageLimitByteCount(ctx, free)))
		connect.AssertEqual(t, usage.Spent, false)
		day := networkUsageDay(yesterday).Format(networkUsageDayLayout)
		connect.AssertEqual(t, usage.Days, []*NetworkUsageDay{
			{UsageDay: day, Meter: "evs", Quantity: 10},
			{UsageDay: day, Meter: NetworkUsageStorageMeter, Quantity: 1234},
		})
		connect.AssertEqual(t, usage.Next, &NetworkUsageCursor{UsageDay: day, Meter: NetworkUsageStorageMeter})
		next, err := GetNetworkUsage(ctx, free, usage.Next, 2, now)
		connect.AssertEqual(t, err, nil)
		// the helper meter is hidden and the 14-month-old row is past the window
		connect.AssertEqual(t, next.Days, []*NetworkUsageDay{{UsageDay: networkUsageDay(now.AddDate(0, 0, -2)).Format(networkUsageDayLayout), Meter: "tls", Quantity: 5}})
		connect.AssertEqual(t, next.Next == nil, true)
		if _, err := GetNetworkUsage(ctx, free, &NetworkUsageCursor{UsageDay: "yesterday"}, 2, now); err == nil {
			t.Error("a cursor whose day does not parse")
		}

		pro := server.NewId()
		testingAddTierBalance(t, ctx, pro, true, now.Add(-time.Hour), now.Add(time.Hour))
		UpdateProNetwork(ctx, pro)
		connect.AssertEqual(t, MarkNetworkSpent(ctx, pro, now), nil)
		usage, err = GetNetworkUsage(ctx, pro, nil, 0, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, usage.Limits.Tier, "pro")
		connect.AssertEqual(t, usage.Spent, true)
		connect.AssertEqual(t, len(usage.Days), 0)
	})
}

// A rollup killed before its cursor was handed on runs its page again and writes nothing twice; a closed
// day's row keeps what it first wrote.
func TestRollupServiceUsageResumesWithoutDoubleRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		day := now.AddDate(0, 0, -1)
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		for index, networkId := range networkIds {
			connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkId, int64(100*(index+1))), nil)
		}
		first, err := RollupServiceUsage(ctx, day, nil, 2)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, first.LookedCount, 2)
		connect.AssertEqual(t, first.WrittenCount, 2)
		connect.AssertEqual(t, first.LastNetworkId != nil, true)
		// killed before the next run was scheduled: the page again writes nothing
		again, err := RollupServiceUsage(ctx, day, nil, 2)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.WrittenCount, 0)
		rest, err := RollupServiceUsage(ctx, day, first.LastNetworkId, 2)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rest.LookedCount, 1)
		connect.AssertEqual(t, rest.WrittenCount, 1)
		connect.AssertEqual(t, rest.LastNetworkId == nil, true)
		// the count moves after the day closed: the day's row does not
		connect.AssertEqual(t, SetNetworkStoredByteCount(ctx, networkIds[0], 999), nil)
		rerun, err := RollupServiceUsage(ctx, day, nil, 10)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rerun.WrittenCount, 0)
		for index, networkId := range networkIds {
			quantity, found := testingUsageQuantity(ctx, networkId, day, NetworkUsageStorageMeter)
			connect.AssertEqual(t, found, true)
			connect.AssertEqual(t, quantity, int64(100*(index+1)))
		}
	})
}

// network_service_usage past 13 months and service_usage_batch past 24 h go, a bounded page a run, and
// nothing inside its ttl.
func TestRemoveExpiredServiceUsageEndsAtTheTtl(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		networkId := server.NewId()
		for _, meter := range []string{"evs", "tls", "mx"} {
			testingInsertUsageDay(ctx, networkId, now.AddDate(0, -13, -1), meter, 1)
		}
		testingInsertUsageDay(ctx, networkId, now.AddDate(0, -12, 0), "evs", 1)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, applied := range []time.Time{now.Add(-25 * time.Hour), now.Add(-24*time.Hour - time.Minute), now.Add(-time.Hour)} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO service_usage_batch (batch_id, producer, window_start, applied_time) VALUES ($1, 'evs', $2, $2)`,
					server.NewId(),
					applied,
				))
			}
		})
		first, err := RemoveExpiredServiceUsage(ctx, now, 2)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, first.UsageRemovedCount, 2)
		connect.AssertEqual(t, first.BatchRemovedCount, 2)
		connect.AssertEqual(t, first.More, true)
		second, err := RemoveExpiredServiceUsage(ctx, now, 2)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, second.UsageRemovedCount, 1)
		connect.AssertEqual(t, second.BatchRemovedCount, 0)
		connect.AssertEqual(t, second.More, false)
		var usageCount, batchCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_service_usage WHERE network_id = $1`, networkId).Scan(&usageCount))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM service_usage_batch`).Scan(&batchCount))
		})
		connect.AssertEqual(t, usageCount, 1)
		connect.AssertEqual(t, batchCount, 1)
	})
}
