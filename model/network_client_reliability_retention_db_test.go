package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// client_reliability retention keeps 10 days. A subsidy epoch, however
// recent or stale, no longer extends it (owner decision 2026-10-31: subsidy
// payouts ended with the epoch that ended 2026-10-05). Each test seeds one row
// per block at fixed day offsets from now, runs the real partition maintenance
// or legacy row delete, and checks which rows survive. The offsets sit days
// away from every cutoff, so the outcome does not depend on the time of day
// the test runs.

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

// records a completed subsidy epoch ending at endTime, the frontier that used
// to hold retention back to its end
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

// With no payout behind, a day partition entirely older than 10 days is
// dropped and one inside the window is kept. Retention used to be 30 days,
// which kept the 25-day-old partition and with it 9 daily partitions (about
// 480 GB on Main) that no recurring reader needs; until 2026-11-10 it was 14
// days, which kept the 12-day-old partition.
func TestClientReliabilityPartitionMaintenanceDropsDaysPastRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		expiredTime := now.Add(-12 * 24 * time.Hour)
		retainedTime := now.Add(-8 * 24 * time.Hour)
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

// A subsidy epoch that ended 26 days ago used to hold retention back to its
// end less a day, keeping the 24-day-old partition (on Main, the 10-05
// frontier held every day from 10-04 and stopped the nightly drops from
// 10-25). The cutoff is now 10 days whatever the frontier.
func TestClientReliabilityPartitionMaintenanceIgnoresSubsidyFrontier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-26*24*time.Hour))

		pastRetentionTime := now.Add(-24 * 24 * time.Hour)
		retainedTime := now.Add(-8 * 24 * time.Hour)
		partitionClientReliabilityForRetentionTest(t, ctx, now, []time.Time{pastRetentionTime, retainedTime})

		_, droppedNames := MaintainClientReliabilityPartitions(ctx, now)

		pastRetentionName := reliabilityPartitionName(reliabilityPartitionDay(reliabilityBlockNumber(pastRetentionTime)))
		retainedName := reliabilityPartitionName(reliabilityPartitionDay(reliabilityBlockNumber(retainedTime)))
		connect.AssertEqual(t, slices.Contains(droppedNames, pastRetentionName), true)
		connect.AssertEqual(t, slices.Contains(droppedNames, retainedName), false)
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, pastRetentionTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
	})
}

// A frontier older than 30 days used to keep exactly 30 days, so the
// 25-day-old partition stayed. It is past the 10-day retention, so it goes.
func TestClientReliabilityPartitionMaintenanceStaleFrontierKeepsTheRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-45*24*time.Hour))

		expiredTime := now.Add(-12 * 24 * time.Hour)
		retainedTime := now.Add(-8 * 24 * time.Hour)
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

		expiredTime := now.Add(-12 * 24 * time.Hour)
		retainedTime := now.Add(-8 * 24 * time.Hour)
		addClientReliabilityRowForRetentionTest(ctx, expiredTime)
		addClientReliabilityRowForRetentionTest(ctx, retainedTime)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), false)

		removedCount := RemoveOldClientReliabilityStats(ctx, now, 1000)

		connect.AssertEqual(t, removedCount, int64(1))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, expiredTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
	})
}

// The legacy row delete ignores the subsidy frontier too: an epoch that ended
// 26 days ago no longer keeps the 24-day-old block.
func TestRemoveOldClientReliabilityStatsIgnoresSubsidyFrontier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-26*24*time.Hour))

		pastRetentionTime := now.Add(-24 * 24 * time.Hour)
		retainedTime := now.Add(-8 * 24 * time.Hour)
		addClientReliabilityRowForRetentionTest(ctx, pastRetentionTime)
		addClientReliabilityRowForRetentionTest(ctx, retainedTime)
		connect.AssertEqual(t, IsClientReliabilityPartitioned(ctx), false)

		removedCount := RemoveOldClientReliabilityStats(ctx, now, 1000)

		connect.AssertEqual(t, removedCount, int64(1))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, pastRetentionTime), int64(0))
		connect.AssertEqual(t, countClientReliabilityRowsForRetentionTest(ctx, retainedTime), int64(1))
	})
}

// Operator block points refuse a reliability window that starts before the
// 10-day cutoff, as a window whose evidence the drop pass removes. A recent
// subsidy epoch used to move the cutoff back and let such a window read
// blocks that a later pass would drop.
func TestOperatorBlockReliabilityExpiresAtRetentionDespiteSubsidyFrontier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addSubsidyEpochForRetentionTest(ctx, now.Add(-26*24*time.Hour))

		expiredStart := now.Add(-12 * 24 * time.Hour)
		_, err := readOperatorBlockReliability(ctx, expiredStart, expiredStart.Add(SubnetBlockDuration), now)
		connect.AssertEqual(t, errors.Is(err, ErrOperatorBlockPointsReliabilityExpired), true)

		retainedStart := now.Add(-8 * 24 * time.Hour)
		_, err = readOperatorBlockReliability(ctx, retainedStart, retainedStart.Add(SubnetBlockDuration), now)
		connect.AssertEqual(t, errors.Is(err, ErrOperatorBlockPointsReliabilityExpired), false)
	})
}

// The first pass after an operator exported and detached the 9 oldest day
// partitions, dropping some of them, leaves those days alone: no day below the
// cutoff is re-created, a detached table is not dropped, the partition that
// straddles the 10-day cutoff is kept, and nothing attached is dropped. The
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

		// midday, so the 10-day cutoff falls inside a day partition
		day := reliabilityPartitionDay(reliabilityBlockNumber(server.NowUtc()))
		now := time.Unix(day*24*60*60, 0).UTC().Add(12 * time.Hour)
		for d := day - 19; d <= day; d += 1 {
			connect.AssertEqual(t, ensureClientReliabilityPartition(ctx, clientReliabilityTable, d), true)
		}

		execDdl := func(sql string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, sql))
			}, server.OptReadWrite())
		}
		detachedNames := []string{}
		for d := day - 19; d <= day-11; d += 1 {
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
		connect.AssertEqual(t, attachedNames[0], reliabilityPartitionName(day-10))

		_, droppedPartitionNames = MaintainClientReliabilityPartitions(ctx, now.Add(24*time.Hour))
		connect.AssertEqual(t, droppedPartitionNames, []string{reliabilityPartitionName(day - 10)})
	})
}
