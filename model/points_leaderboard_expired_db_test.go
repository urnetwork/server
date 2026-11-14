package model

// The points leaderboard's streaks through an operator block the daily award
// recorded expired (owner decision 2026-11-14, pointsStreaks), read through
// the real rebuild from operator_block_points_expired.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The network's Blocks, Streak and Longest in the snapshot.
func pointsTestRequireStreak(
	t testing.TB,
	ctx context.Context,
	snapshotId server.Id,
	networkId server.Id,
	name string,
	blocks int,
	streak int,
	longest int,
) {
	t.Helper()
	row := GetPointsLeaderboardNetworkRow(ctx, snapshotId, networkId)
	if row == nil {
		t.Fatalf("%s: not ranked", name)
	}
	if row.BlocksWithPoints != blocks || row.Streak != streak || row.LongestStreak != longest {
		t.Errorf(
			"%s: blocks=%d streak=%d longest=%d, want %d/%d/%d",
			name, row.BlocksWithPoints, row.Streak, row.LongestStreak, blocks, streak, longest,
		)
	}
}

// The root cause through the rebuild: operator blocks 1 to 4 have completed,
// and the daily award recorded block 3 expired at a clock past its deadline
// (recordOperatorBlockPointsExpiry), so block 3 never gets its points. A
// network that earned in blocks 1, 2 and 4 keeps its streak through block 3:
// 3, over blocks 1, 2 and 4, with a longest of 3. The rebuild reads the record
// from operator_block_points_expired; before it is written the same points
// rank by the rule before, a streak of 1 with a longest of 2. A gap in a block
// that did not expire still ends a streak, and points earned in the expired
// block, from another source than its award, count as any.
func TestPointsLeaderboardExpiredBlockKeepsTheStreak(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		windows := pointsGenesisWindows(4)
		posted := pointsBlockTime(5, time.Hour)

		excused, gap, earned := server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, excused, "points_expired_excused", server.NewId())
		Testing_CreateNetwork(ctx, gap, "points_expired_gap", server.NewId())
		Testing_CreateNetwork(ctx, earned, "points_expired_earned", server.NewId())
		for _, block := range []uint64{1, 2, 4} {
			Testing_InsertAccountPointForBlock(ctx, excused, PointsToNanoPoints(10), block, posted)
		}
		// no points in block 2, which does not expire, nor in block 3
		for _, block := range []uint64{1, 4} {
			Testing_InsertAccountPointForBlock(ctx, gap, PointsToNanoPoints(10), block, posted)
		}
		for _, block := range []uint64{1, 2, 3, 4} {
			Testing_InsertAccountPointForBlock(ctx, earned, PointsToNanoPoints(10), block, posted)
		}

		// no block has expired: the rule before
		before, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, before.LatestEpoch, uint64(4))
		pointsTestRequireStreak(t, ctx, before.SnapshotId, excused, "no record, no points in block 3", 3, 1, 2)
		pointsTestRequireStreak(t, ctx, before.SnapshotId, gap, "no record, no points in blocks 2 and 3", 2, 1, 1)
		pointsTestRequireStreak(t, ctx, before.SnapshotId, earned, "no record, points in every block", 4, 4, 4)

		// the daily award's record of block 3, two weeks after it opened: its
		// reliability evidence is past retention
		expiry, ok := OperatorBlockPointsExpiryAt(3, SubnetBlockGenesis, pointsBlockTime(5, 0))
		if !ok {
			t.Fatalf("block 3 is not expired at the start of block 5")
		}
		if recorded, err := recordOperatorBlockPointsExpiry(ctx, expiry); err != nil || !recorded {
			t.Fatalf("block 3's expiry was not recorded: %t %v", recorded, err)
		}

		after, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, after.LatestEpoch, uint64(4))
		pointsTestRequireStreak(t, ctx, after.SnapshotId, excused, "points in blocks 1, 2 and 4, block 3 expired", 3, 3, 3)
		pointsTestRequireStreak(t, ctx, after.SnapshotId, gap, "no points in block 2, which did not expire", 2, 1, 1)
		pointsTestRequireStreak(t, ctx, after.SnapshotId, earned, "points in expired block 3 too", 4, 4, 4)
	})
}

// End to end, through the daily award and the rebuild: the award awards the
// previous block on time; its first run after the next block closes records
// the block between them expired, as its deadline passed with no award, and
// awards the next block. The provider earned in the previous and next blocks
// from their awards, and in no other, so its streak runs through the expired
// block: 2, with a longest of 2. Until 2026-11-14 the expired block ended the
// run: a streak and a longest of 1.
func TestPointsLeaderboardStreakRunsThroughABlockTheDailyAwardExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newOperatorBlockPointsExpiryFixture(t, ctx)
		awards, err := AwardDueOperatorBlockPoints(ctx, f.config, operatorBlockPointsSettled(f.previous), f.previous)
		if err != nil || len(awards) != 1 || awards[0].Block != f.previous {
			t.Fatalf("the previous block %d was not awarded on time: %v %v", f.previous, awards, err)
		}
		awards, err = AwardDueOperatorBlockPoints(ctx, f.config, operatorBlockPointsSettled(f.next), f.previous)
		if err != nil || len(awards) != 1 || awards[0].Block != f.next {
			t.Fatalf("the first run after block %d closed: %v %v", f.next, awards, err)
		}
		if !GetOperatorBlockPointsExpired(ctx, []int64{f.block})[f.block] {
			t.Fatalf("block %d was not recorded expired", f.block)
		}

		// every completed block from block 1 through the next one, as the
		// rebuild task lists them
		snapshot, err := RebuildPointsLeaderboard(ctx, pointsGenesisWindows(int(f.next)))
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, snapshot.LatestEpoch, uint64(f.next))
		pointsTestRequireStreak(t, ctx, snapshot.SnapshotId, f.provider, "awards in the blocks around an expired one", 2, 2, 2)
	})
}
