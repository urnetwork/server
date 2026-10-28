package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

type pointsPayoutWeeksCase struct {
	name         string
	minSweepTime time.Time
	epochEnd     *time.Time
	createTime   time.Time
	first        int64
	last         int64
	ok           bool
}

func pointsPayoutWeeksCases() []pointsPayoutWeeksCase {
	at := func(t time.Time) *time.Time { return &t }
	day := 24 * time.Hour
	return []pointsPayoutWeeksCase{
		{name: "weekly payout of the block that just closed",
			minSweepTime: pointsBlockTime(2, time.Hour), epochEnd: at(pointsBlockTime(3, 0)), createTime: pointsBlockTime(3, 10*time.Hour),
			first: 2, last: 2, ok: true},
		{name: "window ending minutes into the next block",
			minSweepTime: pointsBlockTime(2, time.Hour), epochEnd: at(pointsBlockTime(3, 18*time.Minute)), createTime: pointsBlockTime(3, 10*time.Hour),
			first: 2, last: 3, ok: true},
		{name: "window ending a microsecond into the next block",
			minSweepTime: pointsBlockTime(2, time.Hour), epochEnd: at(pointsBlockTime(3, time.Microsecond)), createTime: pointsBlockTime(3, 10*time.Hour),
			first: 2, last: 3, ok: true},
		{name: "late plan pays the window, not the plan's week",
			minSweepTime: pointsBlockTime(3, time.Hour), epochEnd: at(pointsBlockTime(5, 0)), createTime: pointsBlockTime(6, 10*time.Hour),
			first: 3, last: 4, ok: true},
		{name: "backlog repays older sweeps",
			minSweepTime: pointsBlockTime(1, time.Hour), epochEnd: at(pointsBlockTime(5, 0)), createTime: pointsBlockTime(5, 10*time.Hour),
			first: 1, last: 4, ok: true},
		{name: "plan without a subsidy epoch ends at its own block",
			minSweepTime: pointsBlockTime(2, time.Hour), createTime: pointsBlockTime(4, 10*time.Hour),
			first: 2, last: 3, ok: true},
		{name: "plan without an epoch whose sweeps are in its own block",
			minSweepTime: pointsBlockTime(4, time.Hour), createTime: pointsBlockTime(4, 10*time.Hour),
			first: 4, last: 4, ok: true},
		{name: "oldest sweep after the window end still earns",
			minSweepTime: pointsBlockTime(4, time.Hour), epochEnd: at(pointsBlockTime(4, 0)), createTime: pointsBlockTime(4, 10*time.Hour),
			first: 4, last: 4, ok: true},
		{name: "traffic before the genesis only",
			minSweepTime: SubnetBlockGenesis.Add(-30 * day), epochEnd: at(SubnetBlockGenesis), createTime: pointsBlockTime(2, 10*time.Hour),
			ok: false},
		{name: "backlog from before the genesis into block 2",
			minSweepTime: SubnetBlockGenesis.Add(-30 * day), epochEnd: at(pointsBlockTime(2, time.Hour)), createTime: pointsBlockTime(2, 10*time.Hour),
			first: 1, last: 2, ok: true},
		{name: "window ending as block 2 opens",
			minSweepTime: pointsBlockTime(1, time.Hour), epochEnd: at(pointsBlockTime(2, 0)), createTime: pointsBlockTime(2, 10*time.Hour),
			first: 1, last: 1, ok: true},
		{name: "plan before the genesis without an epoch",
			minSweepTime: SubnetBlockGenesis.Add(-2 * day), createTime: SubnetBlockGenesis.Add(-day),
			ok: false},
	}
}

func TestPointsPayoutWeeksRule(t *testing.T) {
	for _, c := range pointsPayoutWeeksCases() {
		first, last, ok := pointsPayoutWeeks(c.minSweepTime, c.epochEnd, c.createTime)
		if ok != c.ok || (ok && (first != c.first || last != c.last)) {
			t.Fatalf("%s: got [%d, %d] ok=%t, want [%d, %d] ok=%t", c.name, first, last, ok, c.first, c.last, c.ok)
		}
	}
}

func TestPointsBlockOfBoundaries(t *testing.T) {
	for _, c := range []struct {
		t     time.Time
		block int64
	}{
		{SubnetBlockGenesis, 1},
		{SubnetBlockGenesis.Add(-time.Microsecond), 0},
		{SubnetBlockGenesis.Add(SubnetBlockDuration - time.Microsecond), 1},
		{SubnetBlockGenesis.Add(SubnetBlockDuration), 2},
		{SubnetBlockGenesis.Add(-SubnetBlockDuration), 0},
		{SubnetBlockGenesis.Add(-SubnetBlockDuration - time.Microsecond), -1},
	} {
		if got := pointsBlockOf(c.t); got != c.block {
			t.Fatalf("block of %s: got %d, want %d", c.t, got, c.block)
		}
		if int64(SubnetBlockNumber(c.t)) != c.block && !c.t.Before(SubnetBlockGenesis) {
			t.Fatalf("block of %s disagrees with SubnetBlockNumber", c.t)
		}
	}
}

// The SQL of the rule and the Go rule agree on the cases and on a grid of
// instants around block boundaries.
func TestPointsPayoutWeeksSqlMatchesRule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases := pointsPayoutWeeksCases()
		offsets := []time.Duration{-time.Second, -time.Microsecond, 0, time.Microsecond, time.Hour}
		for _, block := range []int64{-1, 0, 1, 2, 3} {
			for _, sweepOffset := range offsets {
				for _, endOffset := range offsets {
					minSweepTime := pointsBlockTime(block, sweepOffset)
					epochEnd := pointsBlockTime(block+1, endOffset)
					cases = append(cases,
						pointsPayoutWeeksCase{name: "grid with epoch", minSweepTime: minSweepTime, epochEnd: &epochEnd, createTime: epochEnd.Add(time.Hour)},
						pointsPayoutWeeksCase{name: "grid without epoch", minSweepTime: minSweepTime, createTime: epochEnd},
					)
				}
			}
		}
		minSweepTimes := make([]time.Time, len(cases))
		epochEnds := make([]time.Time, len(cases))
		hasEpochs := make([]bool, len(cases))
		createTimes := make([]time.Time, len(cases))
		for i, c := range cases {
			minSweepTimes[i] = c.minSweepTime.UTC()
			createTimes[i] = c.createTime.UTC()
			if c.epochEnd != nil {
				epochEnds[i] = c.epochEnd.UTC()
				hasEpochs[i] = true
			} else {
				epochEnds[i] = c.createTime.UTC()
			}
		}
		query := fmt.Sprintf(`
			SELECT
				%s,
				%s
			FROM (
				SELECT
					payment.ordinality,
					payment.min_sweep_time,
					payment.create_time,
					CASE WHEN payment.has_epoch THEN payment.end_time END AS end_time
				FROM unnest($3::timestamp[], $4::timestamp[], $5::boolean[], $6::timestamp[])
					WITH ORDINALITY AS payment(min_sweep_time, end_time, has_epoch, create_time, ordinality)
			) AS payment
			ORDER BY payment.ordinality
		`,
			pointsPayoutFirstBlockSql("payment", "$1", "$2"),
			pointsPayoutLastBlockSql("payment", "payment", "$1", "$2"),
		)
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, query,
				SubnetBlockGenesis, int64(SubnetBlockDuration/time.Second),
				minSweepTimes, epochEnds, hasEpochs, createTimes)
			server.WithPgResult(result, err, func() {
				i := 0
				for result.Next() {
					var first, last int64
					server.Raise(result.Scan(&first, &last))
					c := cases[i]
					wantFirst, wantLast, ok := pointsPayoutWeeks(c.minSweepTime, c.epochEnd, c.createTime)
					if ok != (first <= last) || (ok && (first != wantFirst || last != wantLast)) {
						t.Fatalf("%s %d: sql [%d, %d], rule [%d, %d] ok=%t", c.name, i, first, last, wantFirst, wantLast, ok)
					}
					i += 1
				}
				if i != len(cases) {
					t.Fatalf("sql returned %d rows for %d cases", i, len(cases))
				}
			})
		})
	})
}

// The snapshot's approximation counts the payments and points that earn in
// payout weeks of a completed block, and the rollup's remaining work.
func TestPointsLeaderboardApproximationCountsPayoutWeekInputs(t *testing.T) {
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
		// a payout the rollup has not finished
		pending := newPointsPayoutNetwork(ctx, "points_approximation_pending")
		insertPointsPayoutFixture(t, ctx, weekly(pending))
		// a completed payout measured after reaping
		reaped := newPointsPayoutNetwork(ctx, "points_approximation_reaped")
		fixture := weekly(reaped)
		fixture.completed, fixture.rolledUp, fixture.paidSweepTimes = true, true, nil
		insertPointsPayoutFixture(t, ctx, fixture)
		// a canceled payout whose sweeps moved: the rollup's work, no weeks
		moved := newPointsPayoutNetwork(ctx, "points_approximation_moved")
		fixture = weekly(moved)
		fixture.canceled, fixture.paidSweepTimes = true, nil
		insertPointsPayoutFixture(t, ctx, fixture)
		// a measured payout
		measured := newPointsPayoutNetwork(ctx, "points_approximation_measured")
		Testing_InsertAccountPointForBlock(ctx, measured, PointsToNanoPoints(1), 2, pointsBlockTime(3, time.Hour))
		// a point without a payment row
		unlinked := newPointsPayoutNetwork(ctx, "points_approximation_unlinked")
		Testing_InsertAccountPoint(ctx, unlinked, PointsToNanoPoints(1), pointsBlockTime(3, time.Hour))
		// a payout of the open block only
		open := newPointsPayoutNetwork(ctx, "points_approximation_open")
		insertPointsPayoutFixture(t, ctx, pointsPayoutFixture{
			networkId:      open,
			createTime:     pointsBlockTime(3, 2*24*time.Hour),
			minSweepTime:   pointsBlockTime(3, time.Hour),
			epochStart:     pointsBlockTime(3, 0),
			epochEnd:       pointsBlockTime(3, 24*time.Hour),
			paidSweepTimes: []time.Time{pointsBlockTime(3, time.Hour)},
		})

		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(2))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, snapshot.Approximation, PointsLeaderboardApproximation{
			RollupPendingPayments:   1,
			ReapedPayments:          1,
			UnlinkedPoints:          1,
			ApproximateNetworks:     3,
			BackfillPendingPayments: 3,
		})
		connect.AssertEqual(t, snapshot.Approximation.PayoutWeekPayments(), int64(2))

		// nothing is approximate without a completed block
		none, err := RebuildPointsLeaderboard(ctx, nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none.Approximation, PointsLeaderboardApproximation{})
	})
}
