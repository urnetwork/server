package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// client_reliability retention keeps 21 days in steady state, extends back to
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

// With no payout behind, a day partition entirely older than 21 days is
// dropped and one inside the window is kept. Retention used to be 30 days,
// which kept the 25-day-old partition and with it 9 daily partitions (about
// 480 GB on Main) that no recurring reader needs.
func TestClientReliabilityPartitionMaintenanceDropsDaysPastRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		expiredTime := now.Add(-25 * 24 * time.Hour)
		retainedTime := now.Add(-20 * 24 * time.Hour)
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
// keeps those blocks even past 21 days: a block missing from a payout window
// is scored as covered with no valid rows and silently cuts provider credit.
// Blocks entirely before that window (less a day of margin) are still dropped.
func TestClientReliabilityPartitionMaintenanceKeepsPayoutWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-26*24*time.Hour))

		beforeWindowTime := now.Add(-29 * 24 * time.Hour)
		inWindowTime := now.Add(-24 * 24 * time.Hour)
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

		expiredTime := now.Add(-25 * 24 * time.Hour)
		retainedTime := now.Add(-20 * 24 * time.Hour)
		addClientReliabilityRowForRetentionTest(ctx, expiredTime)
		addClientReliabilityRowForRetentionTest(ctx, retainedTime)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), false)

		removedCount := RemoveOldClientReliabilityStats(ctx, now, 1000)

		connect.AssertEqual(t, removedCount, int64(1))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, expiredTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
	})
}

// The first pass after an operator exported and detached the 9 oldest day
// partitions, dropping some of them, leaves those days alone: no day below the
// cutoff is re-created, a detached table is not dropped, the partition that
// straddles the 21-day cutoff is kept, and nothing attached is dropped. The
// next day's pass drops exactly that straddling partition. The former 30-day
// pass re-created every dropped day, each under an ACCESS EXCLUSIVE lock on
// the parent.
func TestClientReliabilityPartitionMaintenanceLeavesDetachedDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		err := MigrateClientReliabilityToPartitions(ctx, 1, false, false, t.Logf)
		connect.AssertEqual(t, err, nil)
		// start without partitions, so only the days below are in play
		for _, name := range listClientReliabilityPartitions(ctx) {
			connect.AssertEqual(t, dropClientReliabilityPartition(ctx, name), true)
		}

		// midday, so the 21-day cutoff falls inside a day partition
		day := reliabilityPartitionDay(reliabilityBlockNumber(server.NowUtc()))
		now := time.Unix(day*24*60*60, 0).UTC().Add(12 * time.Hour)
		for d := day - 30; d <= day; d += 1 {
			connect.AssertEqual(t, ensureClientReliabilityPartition(ctx, clientReliabilityTable, d), true)
		}

		execDdl := func(sql string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, sql))
			}, server.OptReadWrite())
		}
		detachedNames := []string{}
		for d := day - 30; d <= day-22; d += 1 {
			name := reliabilityPartitionName(d)
			execDdl(fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`, clientReliabilityTable, name))
			detachedNames = append(detachedNames, name)
		}
		droppedNames := detachedNames[:5]
		keptNames := detachedNames[5:]
		for _, name := range droppedNames {
			execDdl(fmt.Sprintf(`DROP TABLE %s`, name))
		}

		createdNames, droppedPartitionNames := MaintainClientReliabilityPartitions(ctx, now)

		aheadNames := []string{}
		for d := day + 1; d <= day+reliabilityPartitionAheadDays; d += 1 {
			aheadNames = append(aheadNames, reliabilityPartitionName(d))
		}
		connect.AssertEqual(t, createdNames, aheadNames)
		connect.AssertEqual(t, len(droppedPartitionNames), 0)
		for _, name := range droppedNames {
			_, exists := pgRelkind(ctx, name)
			connect.AssertEqual(t, exists, false)
		}
		attachedNames := listClientReliabilityPartitions(ctx)
		for _, name := range keptNames {
			_, exists := pgRelkind(ctx, name)
			connect.AssertEqual(t, exists, true)
			connect.AssertEqual(t, slices.Contains(attachedNames, name), false)
		}
		connect.AssertEqual(t, attachedNames[0], reliabilityPartitionName(day-21))

		_, droppedPartitionNames = MaintainClientReliabilityPartitions(ctx, now.Add(24*time.Hour))
		connect.AssertEqual(t, droppedPartitionNames, []string{reliabilityPartitionName(day - 21)})
	})
}
