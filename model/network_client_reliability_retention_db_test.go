package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// client_reliability retention keeps 14 days in steady state, extends back to
// the next payout's window start (the last subsidy epoch end), and never keeps
// more than 30 days. Each test seeds one row per block at fixed day offsets from
// now, runs the real partition maintenance or legacy row delete, and checks
// which rows survive. The offsets sit days away from every cutoff, so the
// outcome does not depend on the time of day the test runs.

// adds one client_reliability row in the block holding the time
func addClientReliabilityRowForRetentionTest(ctx context.Context, rowTime time.Time) {
	AddClientReliabilityStats(
		ctx,
		server.NewId(),
		server.NewId(),
		[32]byte{1},
		rowTime,
		&ClientReliabilityStats{
			ReceiveMessageCount:        1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
		},
	)
}

// counts the client_reliability rows in the block holding the time
func countClientReliabilityRowsForRetentionTest(ctx context.Context, rowTime time.Time) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM client_reliability WHERE block_number = $1`,
			reliabilityBlockNumber(rowTime),
		).Scan(&count))
	})
	return
}

// records a completed subsidy epoch, which moves the next payout's reliability
// window start to its end
func addSubsidyEpochForRetentionTest(ctx context.Context, endTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO subsidy_payment (
				payment_plan_id, start_time, end_time,
				active_user_count, paid_user_count,
				net_payout_byte_count_paid, net_payout_byte_count_unpaid,
				net_revenue_nano_cents, net_payout_nano_cents
			) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)
			`,
			server.NewId(),
			endTime.Add(-4*24*time.Hour),
			endTime,
		))
	})
}

// converts the fresh plain table to daily partitions, then adds one row in each
// of the older times, creating the day partition when the cutover did not
func partitionClientReliabilityForRetentionTest(t testing.TB, ctx context.Context, now time.Time, olderTimes []time.Time) {
	addClientReliabilityRowForRetentionTest(ctx, now.Add(-1*time.Hour))
	err := MigrateClientReliabilityToPartitions(ctx, 1, false, false, t.Logf)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), true)
	for _, olderTime := range olderTimes {
		ensureClientReliabilityPartition(ctx, clientReliabilityTable, reliabilityPartitionDay(reliabilityBlockNumber(olderTime)))
		addClientReliabilityRowForRetentionTest(ctx, olderTime)
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, olderTime), int64(1))
	}
}

// With no payout behind, a day partition entirely older than 14 days is
// dropped and one inside the window is kept. Retention used to be 30 days,
// which kept the 20-day-old partition and with it about 9 days of daily
// partitions (about 480 GB on Main) that no recurring reader needs.
func TestClientReliabilityPartitionMaintenanceDropsDaysPastRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		expiredTime := now.Add(-20 * 24 * time.Hour)
		retainedTime := now.Add(-13 * 24 * time.Hour)
		partitionClientReliabilityForRetentionTest(t, ctx, now, []time.Time{expiredTime, retainedTime})

		_, droppedNames := MaintainClientReliabilityPartitions(ctx, now)

		expiredName := reliabilityPartitionName(reliabilityPartitionDay(reliabilityBlockNumber(expiredTime)))
		retainedName := reliabilityPartitionName(reliabilityPartitionDay(reliabilityBlockNumber(retainedTime)))
		connect.AssertEqual(t, slices.Contains(droppedNames, expiredName), true)
		connect.AssertEqual(t, slices.Contains(droppedNames, retainedName), false)
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, expiredTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, now.Add(-1*time.Hour)), int64(1))
	})
}

// A payout that is behind reads from the last subsidy epoch end, so retention
// keeps those blocks even past 14 days: a block missing from a payout window
// is scored as covered with no valid rows and silently cuts provider credit.
// Blocks entirely before that window (less a day of margin) are still dropped.
func TestClientReliabilityPartitionMaintenanceKeepsPayoutWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-20*24*time.Hour))

		beforeWindowTime := now.Add(-25 * 24 * time.Hour)
		inWindowTime := now.Add(-19 * 24 * time.Hour)
		partitionClientReliabilityForRetentionTest(t, ctx, now, []time.Time{beforeWindowTime, inWindowTime})

		MaintainClientReliabilityPartitions(ctx, now)

		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, beforeWindowTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, inWindowTime), int64(1))
	})
}

// However far behind the payout is, retention never keeps more than 30 days,
// so a stalled payout cannot grow the table without bound.
func TestClientReliabilityPartitionMaintenanceNeverKeepsPastThirtyDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-45*24*time.Hour))

		expiredTime := now.Add(-40 * 24 * time.Hour)
		retainedTime := now.Add(-29 * 24 * time.Hour)
		partitionClientReliabilityForRetentionTest(t, ctx, now, []time.Time{expiredTime, retainedTime})

		MaintainClientReliabilityPartitions(ctx, now)

		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, expiredTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
	})
}

// The legacy row delete (plain, unpartitioned table) applies the same
// retention as the partition drop.
func TestRemoveOldClientReliabilityStatsDeletesBlocksPastRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		expiredTime := now.Add(-20 * 24 * time.Hour)
		retainedTime := now.Add(-13 * 24 * time.Hour)
		addClientReliabilityRowForRetentionTest(ctx, expiredTime)
		addClientReliabilityRowForRetentionTest(ctx, retainedTime)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), false)

		removedCount := RemoveOldClientReliabilityStats(ctx, now, 1000)

		connect.AssertEqual(t, removedCount, int64(1))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, expiredTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
	})
}
