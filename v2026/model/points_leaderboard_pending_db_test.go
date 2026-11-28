package model

// The points leaderboard's streaks while the latest operator block waits for
// its award (owner decision 2026-11-14, loadPointsPendingEpochs), read through
// the award's real commit and the real rebuild.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Commits the block's award through the award's commit
// (CommitOperatorBlockPoints): a payment for each network, of a GiB served a
// day after the block opened, and the block's payout points shared among
// them. The award is planned from that traffic (planOperatorBlockPoints)
// rather than read from sweeps, so a block of any age can be awarded: the
// commit does not check reliability retention, which an award's reads of
// these blocks would fail.
func pointsTestCommitOperatorBlockAward(t testing.TB, ctx context.Context, block int64, networkIds ...server.Id) {
	t.Helper()
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil || transition == nil {
		t.Fatalf("no earning boundary: %v", err)
	}
	start, end, ok := OperatorBlockPointsWindow(block, transition.Cutoff)
	if !ok {
		t.Fatalf("block %d has no traffic past the cutoff", block)
	}
	traffic := newOperatorBlockTraffic()
	for _, networkId := range networkIds {
		traffic.add(&operatorBlockSweepRow{
			payeeNetworkId:  networkId,
			payoutByteCount: ByteCount(1024 * 1024 * 1024),
			minSweepTime:    start.Add(24 * time.Hour),
			sweepCount:      1,
		})
	}
	award, err := planOperatorBlockPoints(
		&operatorBlockPointsInputs{
			subsidyConfig:    operatorBlockPointsTestConfig(),
			block:            block,
			cutoff:           transition.Cutoff,
			policySha256:     transition.ConfigSha256,
			windowStart:      start,
			windowEnd:        end,
			seekerHolders:    map[server.Id]bool{},
			networkReferrals: map[server.Id][]server.Id{},
		},
		traffic,
		&operatorBlockReliability{networkWeights: map[server.Id]float64{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := CommitOperatorBlockPoints(ctx, award)
	if err != nil || !committed {
		t.Fatalf("block %d's award did not commit: %t %v", block, committed, err)
	}
}

// The root cause through the award's commit and the rebuild. With the earning
// boundary at the genesis, operator blocks 1 to 4 have completed, and the
// daily award has committed blocks 1 to 3 but not yet block 4: it runs about
// 12:00 UTC the Sunday a block closes. A network whose points all come from
// the awards keeps its streak of 3 meanwhile, where the rule before gave 0,
// and has a streak of 4 once block 4's award commits. A network missing an
// older block still breaks there, and one that served nothing in block 4
// keeps its streak only until block 4's award commits without it.
func TestPointsLeaderboardStreakWaitsForTheLatestBlocksAward(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useOperatorBlockPointsTransition(t, SubnetBlockGenesis)
		windows := pointsGenesisWindows(4)

		awardOnly, olderGap, lapsed := server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, awardOnly, "points_pending_award_only", server.NewId())
		Testing_CreateNetwork(ctx, olderGap, "points_pending_older_gap", server.NewId())
		Testing_CreateNetwork(ctx, lapsed, "points_pending_lapsed", server.NewId())
		pointsTestCommitOperatorBlockAward(t, ctx, 1, awardOnly, olderGap, lapsed)
		// olderGap served nothing in block 2
		pointsTestCommitOperatorBlockAward(t, ctx, 2, awardOnly, lapsed)
		pointsTestCommitOperatorBlockAward(t, ctx, 3, awardOnly, olderGap, lapsed)

		pending, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, pending.LatestEpoch, uint64(4))
		pointsTestRequireStreak(t, ctx, pending.SnapshotId, awardOnly, "awards in blocks 1 to 3, block 4 pending", 3, 3, 3)
		pointsTestRequireStreak(t, ctx, pending.SnapshotId, olderGap, "no award in block 2, block 4 pending", 2, 1, 1)
		pointsTestRequireStreak(t, ctx, pending.SnapshotId, lapsed, "awards in blocks 1 to 3, block 4 pending, no traffic in it", 3, 3, 3)

		// lapsed served nothing in block 4
		pointsTestCommitOperatorBlockAward(t, ctx, 4, awardOnly, olderGap)
		awarded, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, awarded.LatestEpoch, uint64(4))
		pointsTestRequireStreak(t, ctx, awarded.SnapshotId, awardOnly, "awards in blocks 1 to 4", 4, 4, 4)
		pointsTestRequireStreak(t, ctx, awarded.SnapshotId, olderGap, "no award in block 2", 3, 2, 2)
		pointsTestRequireStreak(t, ctx, awarded.SnapshotId, lapsed, "no award in block 4", 3, 0, 3)
	})
}

// Only the latest completed block waits for its award, through the rebuild.
// Block 3 has neither an award nor an expiry record while block 4 is awarded:
// block 3 is no longer the latest, so it ends the run of a network that earned
// in every other block, as before. Once block 3 is recorded expired it is
// excused instead.
func TestPointsLeaderboardOnlyTheLatestBlockWaitsForItsAward(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useOperatorBlockPointsTransition(t, SubnetBlockGenesis)
		windows := pointsGenesisWindows(4)

		provider := server.NewId()
		Testing_CreateNetwork(ctx, provider, "points_pending_older_block", server.NewId())
		for _, block := range []int64{1, 2, 4} {
			pointsTestCommitOperatorBlockAward(t, ctx, block, provider)
		}

		before, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		pointsTestRequireStreak(t, ctx, before.SnapshotId, provider, "block 3 without an award, block 4 awarded", 3, 1, 2)

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
		pointsTestRequireStreak(t, ctx, after.SnapshotId, provider, "block 3 expired, block 4 awarded", 3, 3, 3)
	})
}

// The latest block recorded expired is excused, not pending. Without an
// earning boundary no award owes a block its points, so nothing is pending
// and a network without points in the latest block has no current streak, as
// before; under a boundary other than the recorded one, which stops the
// awards too, nothing is pending either, and the rebuild still succeeds.
func TestPointsLeaderboardLatestBlockExpiredOrWithoutABoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		windows := pointsGenesisWindows(4)
		posted := pointsBlockTime(5, time.Hour)

		provider := server.NewId()
		Testing_CreateNetwork(ctx, provider, "points_pending_no_boundary", server.NewId())
		for _, block := range []uint64{1, 2, 3} {
			Testing_InsertAccountPointForBlock(ctx, provider, PointsToNanoPoints(10), block, posted)
		}

		withoutBoundary, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		pointsTestRequireStreak(t, ctx, withoutBoundary.SnapshotId, provider, "no earning boundary, no points in block 4", 3, 0, 3)

		useOperatorBlockPointsTransition(t, SubnetBlockGenesis)
		pending, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		pointsTestRequireStreak(t, ctx, pending.SnapshotId, provider, "block 4 pending", 3, 3, 3)

		pushOperatorBlockPointsTransition(t, SubnetBlockGenesis.Add(SubnetBlockDuration))
		if _, err := server.LoadProviderPayoutEarningPolicy(ctx); err == nil {
			t.Fatalf("a boundary other than the recorded one was accepted")
		}
		unreadable, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatalf("a boundary other than the recorded one failed the rebuild: %s", err)
		}
		pointsTestRequireStreak(t, ctx, unreadable.SnapshotId, provider, "a boundary other than the recorded one, no points in block 4", 3, 0, 3)

		expiry, ok := OperatorBlockPointsExpiryAt(4, SubnetBlockGenesis, pointsBlockTime(6, 0))
		if !ok {
			t.Fatalf("block 4 is not expired at the start of block 6")
		}
		if recorded, err := recordOperatorBlockPointsExpiry(ctx, expiry); err != nil || !recorded {
			t.Fatalf("block 4's expiry was not recorded: %t %v", recorded, err)
		}
		expired, err := RebuildPointsLeaderboard(ctx, windows)
		if err != nil {
			t.Fatal(err)
		}
		pointsTestRequireStreak(t, ctx, expired.SnapshotId, provider, "block 4 expired", 3, 3, 3)
	})
}
