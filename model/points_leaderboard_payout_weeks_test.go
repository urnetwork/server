package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The payout-week fallback of the points leaderboard (owner decision
// 2026-10-28). These tests use only the rebuild, the rollup and raw fixture
// rows, so they also run against the code before the fallback, where every
// one of them fails: a payment the history rollup has not finished made the
// whole snapshot's Blocks and Streak unavailable.

// The completed operator blocks 1..count.
func pointsGenesisWindows(count int) []PointsEpochWindow {
	windows := make([]PointsEpochWindow, 0, count)
	for i := 0; i < count; i += 1 {
		start := SubnetBlockGenesis.Add(time.Duration(i) * SubnetBlockDuration)
		windows = append(windows, PointsEpochWindow{Epoch: uint64(i + 1), Start: start, End: start.Add(SubnetBlockDuration)})
	}
	return windows
}

// The instant `offset` into the 1-based operator block.
func pointsBlockTime(block int64, offset time.Duration) time.Time {
	return SubnetBlockGenesis.Add(time.Duration(block-1)*SubnetBlockDuration + offset)
}

// One synthetic payout: a payment of its own plan, one point, and the sweeps
// it owns. All values are synthetic.
type pointsPayoutFixture struct {
	networkId  server.Id
	nanoPoints NanoPoints
	// the plan time; the point posts with it
	createTime time.Time
	// min_sweep_time
	minSweepTime time.Time
	// the plan's subsidy epoch; a zero end records none
	epochStart time.Time
	epochEnd   time.Time
	// paid sweeps the payment owns now
	paidSweepTimes []time.Time
	// sweeps without payout bytes the payment owns now
	unpaidSweepTimes []time.Time
	completed        bool
	canceled         bool
	// block_rollup_complete and the stored account_payment_block rows
	rolledUp bool
	blocks   []int64
}

func insertPointsPayoutFixture(t testing.TB, ctx context.Context, fixture pointsPayoutFixture) server.Id {
	t.Helper()
	paymentId, planId := server.NewId(), server.NewId()
	nanoPoints := fixture.nanoPoints
	if nanoPoints == 0 {
		nanoPoints = PointsToNanoPoints(1)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_payment (
				payment_id, payment_plan_id, network_id, payout_byte_count,
				payout_nano_cents, min_sweep_time, create_time,
				completed, complete_time, canceled, cancel_time, block_rollup_complete
			) VALUES (
				$1, $2, $3, 1024, 1, $4, $5,
				$6, CASE WHEN $6 THEN $5::timestamp END,
				$7, CASE WHEN $7 THEN $5::timestamp END, $8
			)
		`, paymentId, planId, fixture.networkId, fixture.minSweepTime.UTC(), fixture.createTime.UTC(),
			fixture.completed, fixture.canceled, fixture.rolledUp))
		if !fixture.epochEnd.IsZero() {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO subsidy_payment (
					payment_plan_id, start_time, end_time,
					active_user_count, paid_user_count,
					net_payout_byte_count_paid, net_payout_byte_count_unpaid,
					net_revenue_nano_cents, net_payout_nano_cents
				) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)
			`, planId, fixture.epochStart.UTC(), fixture.epochEnd.UTC()))
		}
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_point (
				account_point_id, network_id, event, point_value,
				payment_plan_id, account_payment_id, create_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, server.NewId(), fixture.networkId, AccountPointEventPayout, nanoPoints, planId, paymentId,
			fixture.createTime.UTC()))
		contractId := server.NewId()
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id,
				destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time
			) VALUES ($1, $2, $3, $2, $4, 1024, $5, $5)
		`, contractId, fixture.networkId, server.NewId(), server.NewId(), fixture.minSweepTime.UTC()))
		insertSweep := func(sweepTime time.Time, byteCount int64) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_escrow_sweep (
					contract_id, balance_id, network_id,
					payout_byte_count, payout_net_revenue_nano_cents,
					sweep_time, payment_id
				) VALUES ($1, $2, $3, $4, 1, $5, $6)
			`, contractId, server.NewId(), fixture.networkId, byteCount, sweepTime.UTC(), paymentId))
		}
		for _, sweepTime := range fixture.paidSweepTimes {
			insertSweep(sweepTime, 1024)
		}
		for _, sweepTime := range fixture.unpaidSweepTimes {
			insertSweep(sweepTime, 0)
		}
		for _, block := range fixture.blocks {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_payment_block (payment_id, block_number) VALUES ($1, $2)
			`, paymentId, block))
		}
	})
	return paymentId
}

func newPointsPayoutNetwork(ctx context.Context, name string) server.Id {
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, name, server.NewId())
	return networkId
}

type pointsBlockMetrics struct {
	blocks  int
	streak  int
	longest int
}

func requirePointsBlockMetrics(t testing.TB, ctx context.Context, snapshot *PointsLeaderboardSnapshot, networkId server.Id, name string, want pointsBlockMetrics) {
	t.Helper()
	row := GetPointsLeaderboardNetworkRow(ctx, snapshot.SnapshotId, networkId)
	if row == nil {
		t.Fatalf("%s is not ranked", name)
	}
	got := pointsBlockMetrics{blocks: row.BlocksWithPoints, streak: row.Streak, longest: row.LongestStreak}
	if got != want {
		t.Fatalf("%s: blocks=%d streak=%d longest=%d, want blocks=%d streak=%d longest=%d",
			name, got.blocks, got.streak, got.longest, want.blocks, want.streak, want.longest)
	}
}

func requirePointsBlockMetricsAvailable(t testing.TB, snapshot *PointsLeaderboardSnapshot, latest uint64) {
	t.Helper()
	if !snapshot.EpochMetricsAvailable || snapshot.LatestEpoch != latest {
		t.Fatalf("snapshot block metrics available=%t latest=%d, want available through block %d",
			snapshot.EpochMetricsAvailable, snapshot.LatestEpoch, latest)
	}
}

// Root cause: one payment the history rollup has not reached made every
// network's Blocks and Streak unavailable, and published them as zeroes. Its
// network now earns in the payout's weeks, and the snapshot is available.
func TestPointsLeaderboardPublishesPayoutWeeksWhileRollupPending(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pending := newPointsPayoutNetwork(ctx, "points_fallback_pending")
		measured := newPointsPayoutNetwork(ctx, "points_fallback_measured")
		// a weekly payout, planned on the Sunday after block 2: its slice paid
		// block 2's traffic, and the rollup has not visited it
		insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:      pending,
			createTime:     pointsBlockTime(3, 10*time.Hour),
			minSweepTime:   pointsBlockTime(2, time.Hour),
			epochStart:     pointsBlockTime(2, 0),
			epochEnd:       pointsBlockTime(3, 0),
			paidSweepTimes: []time.Time{pointsBlockTime(2, time.Hour), pointsBlockTime(2, 5*24*time.Hour)},
		})
		// another network's payouts are measured
		Testing_InsertAccountPointForBlocks(ctx, measured, PointsToNanoPoints(2), pointsBlockTime(3, time.Hour), 1, 2)

		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(2))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, snapshot, 2)
		requirePointsBlockMetrics(t, ctx, snapshot, pending, "payout-week network", pointsBlockMetrics{blocks: 1, streak: 1, longest: 1})
		requirePointsBlockMetrics(t, ctx, snapshot, measured, "measured network", pointsBlockMetrics{blocks: 2, streak: 2, longest: 2})
	})
}

// A network's exact payments end at block 2. Its next payout repays the
// sweeps of a canceled payment the rollup measured in block 2, with traffic
// through block 4, and its plan ran late, in block 6. The payout's weeks
// continue the exact blocks: no gap where they meet, and block 2, held by both,
// counts once. A rule at the plan time (block 5) or at the oldest sweep alone
// (block 2) leaves blocks 3 and 4 empty.
func TestPointsLeaderboardPayoutWeeksMeetExactBlocksWithoutGapOrDoubleCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := newPointsPayoutNetwork(ctx, "points_fallback_junction")
		Testing_InsertAccountPointForBlocks(ctx, networkId, PointsToNanoPoints(1), pointsBlockTime(2, 10*time.Hour), 1)
		insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:    networkId,
			createTime:   pointsBlockTime(3, 10*time.Hour),
			minSweepTime: pointsBlockTime(2, time.Hour),
			epochStart:   pointsBlockTime(2, 0),
			epochEnd:     pointsBlockTime(3, 0),
			canceled:     true,
			rolledUp:     true,
			blocks:       []int64{2},
		})
		insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:    networkId,
			createTime:   pointsBlockTime(6, 10*time.Hour),
			minSweepTime: pointsBlockTime(2, time.Hour),
			epochStart:   pointsBlockTime(3, 0),
			epochEnd:     pointsBlockTime(5, 0),
			paidSweepTimes: []time.Time{
				pointsBlockTime(2, time.Hour),
				pointsBlockTime(3, time.Hour),
				pointsBlockTime(4, 6*24*time.Hour),
			},
		})

		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(5))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, snapshot, 5)
		requirePointsBlockMetrics(t, ctx, snapshot, networkId, "junction network", pointsBlockMetrics{blocks: 4, streak: 0, longest: 4})
	})
}

// A payment's stored rows and its payout weeks are one set: while the rollup
// is part way through it, a block that both hold sums its points once. Here a
// correction of the same network in block 2 outweighs the payout's points
// counted once, but not counted twice.
func TestPointsLeaderboardPayoutWeeksSumEachPaymentOncePerBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := newPointsPayoutNetwork(ctx, "points_fallback_partial")
		Testing_InsertAccountPointForBlock(ctx, networkId, PointsToNanoPoints(20), 1, pointsBlockTime(2, 10*time.Hour))
		insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:      networkId,
			nanoPoints:     PointsToNanoPoints(10),
			createTime:     pointsBlockTime(4, 10*time.Hour),
			minSweepTime:   pointsBlockTime(2, time.Hour),
			epochStart:     pointsBlockTime(2, 0),
			epochEnd:       pointsBlockTime(4, 0),
			paidSweepTimes: []time.Time{pointsBlockTime(2, time.Hour), pointsBlockTime(3, time.Hour)},
			// the rollup stored block 2 and has not finished
			blocks: []int64{2},
		})
		Testing_InsertAccountPointForBlock(ctx, networkId, -PointsToNanoPoints(15), 2, pointsBlockTime(4, 11*time.Hour))

		// block 1: 20; block 2: 10 - 15; block 3: 10
		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(3))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, snapshot, 3)
		requirePointsBlockMetrics(t, ctx, snapshot, networkId, "partially measured network", pointsBlockMetrics{blocks: 2, streak: 1, longest: 1})
	})
}

// Pending and canceled payments earn as the exact rule would measure them now.
// A pending payment owns its sweeps. A canceled payment earns only while it
// still owns a paid sweep; once its sweeps moved it earns in no block. A
// rolled-up payment keeps its stored rows, even none, except a completed one
// whose sweeps contract reaping deleted before the rollup read them.
func TestPointsLeaderboardPayoutWeeksFollowPendingAndCanceledPayments(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		weekly := func(networkId server.Id) pointsPayoutFixture {
			return pointsPayoutFixture{
				networkId:      networkId,
				createTime:     pointsBlockTime(3, 10*time.Hour),
				minSweepTime:   pointsBlockTime(2, time.Hour),
				epochStart:     pointsBlockTime(2, 0),
				epochEnd:       pointsBlockTime(3, 0),
				paidSweepTimes: []time.Time{pointsBlockTime(2, time.Hour)},
			}
		}
		earns := map[string]server.Id{}
		earnsNothing := map[string]server.Id{}

		pending := newPointsPayoutNetwork(ctx, "points_fallback_state_pending")
		insertPointsPayoutFixture(t, ctx, weekly(pending))
		earns["pending"] = pending

		canceledOwning := newPointsPayoutNetwork(ctx, "points_fallback_state_canceled_owning")
		fixture := weekly(canceledOwning)
		fixture.canceled = true
		insertPointsPayoutFixture(t, ctx, fixture)
		earns["canceled, still owns a paid sweep"] = canceledOwning

		canceledMoved := newPointsPayoutNetwork(ctx, "points_fallback_state_canceled_moved")
		fixture = weekly(canceledMoved)
		fixture.canceled = true
		fixture.paidSweepTimes = nil
		insertPointsPayoutFixture(t, ctx, fixture)
		earnsNothing["canceled, sweeps moved"] = canceledMoved

		canceledUnpaid := newPointsPayoutNetwork(ctx, "points_fallback_state_canceled_unpaid")
		fixture = weekly(canceledUnpaid)
		fixture.canceled = true
		fixture.paidSweepTimes = nil
		fixture.unpaidSweepTimes = []time.Time{pointsBlockTime(2, time.Hour)}
		insertPointsPayoutFixture(t, ctx, fixture)
		earnsNothing["canceled, owns only sweeps without payout bytes"] = canceledUnpaid

		canceledMeasured := newPointsPayoutNetwork(ctx, "points_fallback_state_canceled_measured")
		fixture = weekly(canceledMeasured)
		fixture.canceled = true
		fixture.rolledUp = true
		fixture.paidSweepTimes = nil
		insertPointsPayoutFixture(t, ctx, fixture)
		earnsNothing["canceled, measured after its sweeps moved"] = canceledMeasured

		completedUnmeasured := newPointsPayoutNetwork(ctx, "points_fallback_state_completed")
		fixture = weekly(completedUnmeasured)
		fixture.completed = true
		fixture.paidSweepTimes = nil
		insertPointsPayoutFixture(t, ctx, fixture)
		earns["completed, not measured, sweeps reaped"] = completedUnmeasured

		completedReaped := newPointsPayoutNetwork(ctx, "points_fallback_state_reaped")
		fixture = weekly(completedReaped)
		fixture.completed = true
		fixture.rolledUp = true
		fixture.paidSweepTimes = nil
		insertPointsPayoutFixture(t, ctx, fixture)
		earns["completed, measured after its sweeps were reaped"] = completedReaped

		completedBeforeGenesis := newPointsPayoutNetwork(ctx, "points_fallback_state_pre_genesis")
		fixture = weekly(completedBeforeGenesis)
		fixture.completed = true
		fixture.rolledUp = true
		fixture.paidSweepTimes = nil
		// its oldest sweep is older than block 1, so a measurement without rows
		// can be exact: it may have paid only traffic before the genesis
		fixture.minSweepTime = SubnetBlockGenesis.Add(-time.Hour)
		insertPointsPayoutFixture(t, ctx, fixture)
		earnsNothing["completed before-genesis backlog, measured without rows"] = completedBeforeGenesis

		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(2))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, snapshot, 2)
		for name, networkId := range earns {
			requirePointsBlockMetrics(t, ctx, snapshot, networkId, name, pointsBlockMetrics{blocks: 1, streak: 1, longest: 1})
		}
		for name, networkId := range earnsNothing {
			requirePointsBlockMetrics(t, ctx, snapshot, networkId, name, pointsBlockMetrics{})
		}
	})
}

// The open block never counts: a payout whose traffic window reaches into it
// earns only in its completed blocks until that block closes.
func TestPointsLeaderboardPayoutWeeksExcludeTheOpenBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := newPointsPayoutNetwork(ctx, "points_fallback_open_block")
		insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:      networkId,
			createTime:     pointsBlockTime(4, 2*24*time.Hour),
			minSweepTime:   pointsBlockTime(3, time.Hour),
			epochStart:     pointsBlockTime(3, 0),
			epochEnd:       pointsBlockTime(4, 24*time.Hour),
			paidSweepTimes: []time.Time{pointsBlockTime(3, time.Hour), pointsBlockTime(4, time.Hour)},
		})

		// block 4 is open
		open, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(3))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, open, 3)
		requirePointsBlockMetrics(t, ctx, open, networkId, "payout into the open block", pointsBlockMetrics{blocks: 1, streak: 1, longest: 1})

		// block 4 closed
		closed, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(4))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, closed, 4)
		requirePointsBlockMetrics(t, ctx, closed, networkId, "payout after its block closed", pointsBlockMetrics{blocks: 2, streak: 2, longest: 2})
	})
}

// Once the rollup measures a payment, its stored rows replace its payout
// weeks on the next rebuild: here the payout's window reached block 3, but its
// paid sweeps were all in block 2.
func TestPointsLeaderboardPayoutWeeksConvergeOnceRolledUp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := newPointsPayoutNetwork(ctx, "points_fallback_converge")
		paymentId := insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:      networkId,
			createTime:     pointsBlockTime(4, 10*time.Hour),
			minSweepTime:   pointsBlockTime(2, time.Hour),
			epochStart:     pointsBlockTime(2, 0),
			epochEnd:       pointsBlockTime(4, 0),
			paidSweepTimes: []time.Time{pointsBlockTime(2, time.Hour), pointsBlockTime(2, 2*24*time.Hour)},
		})
		windows := pointsGenesisWindows(3)

		before, err := RebuildPointsLeaderboard(ctx, windows)
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, before, 3)
		requirePointsBlockMetrics(t, ctx, before, networkId, "payout weeks", pointsBlockMetrics{blocks: 2, streak: 2, longest: 2})

		batches, err := AdvancePointsBlockRollup(ctx, DefaultPointsBlockRollupSettings())
		connect.AssertEqual(t, err, nil)
		if batches < 1 {
			t.Fatal("the rollup did not measure the payment")
		}
		var complete bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT block_rollup_complete FROM account_payment WHERE payment_id = $1`, paymentId).Scan(&complete))
		})
		if !complete {
			t.Fatal("the rollup did not finish the payment")
		}

		after, err := RebuildPointsLeaderboard(ctx, windows)
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, after, 3)
		requirePointsBlockMetrics(t, ctx, after, networkId, "measured blocks", pointsBlockMetrics{blocks: 1, streak: 0, longest: 1})
	})
}

// A positive point without a payment row has no sweeps to measure. It earns
// in the last block that closed before it posted instead of making every
// network's Blocks and Streak unavailable.
func TestPointsLeaderboardUnlinkedPointEarnsInTheBlockBeforeItPosted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := newPointsPayoutNetwork(ctx, "points_fallback_unlinked")
		Testing_InsertAccountPoint(ctx, networkId, PointsToNanoPoints(3), pointsBlockTime(3, 10*time.Hour))

		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(2))
		connect.AssertEqual(t, err, nil)
		requirePointsBlockMetricsAvailable(t, snapshot, 2)
		requirePointsBlockMetrics(t, ctx, snapshot, networkId, "unlinked point", pointsBlockMetrics{blocks: 1, streak: 1, longest: 1})
	})
}
