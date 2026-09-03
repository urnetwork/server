package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func pointsTestWindows(now time.Time, count int) []PointsEpochWindow {
	// epochs 1..count, each one hour, the latest ending an hour ago
	windows := make([]PointsEpochWindow, 0, count)
	for i := 0; i < count; i += 1 {
		epoch := uint64(i + 1)
		end := now.Add(-time.Duration(count-i) * time.Hour)
		windows = append(windows, PointsEpochWindow{
			Epoch: epoch,
			Start: end.Add(-1 * time.Hour),
			End:   end,
		})
	}
	return windows
}

func pointsTestEntry(entries []PointsLeaderboardEntry, networkId server.Id) *PointsLeaderboardEntry {
	for i := range entries {
		if entries[i].NetworkId == networkId {
			return &entries[i]
		}
	}
	return nil
}

// The streak is the run ending at the latest finalized epoch; the longest
// streak is the best run anywhere; blocks counts every epoch with points.
func TestComputePointsLeaderboardStreaks(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	windows := pointsTestWindows(now, 6)
	networkId := server.NewId()

	cases := []struct {
		name    string
		epochs  []uint64
		blocks  int
		streak  int
		longest int
	}{
		{"none", nil, 0, 0, 0},
		{"latest only", []uint64{6}, 1, 1, 1},
		{"trailing run", []uint64{4, 5, 6}, 3, 3, 3},
		{"missed the latest", []uint64{1, 2, 3, 4, 5}, 5, 0, 5},
		{"gap then trailing", []uint64{1, 2, 3, 5, 6}, 5, 2, 3},
		{"all", []uint64{1, 2, 3, 4, 5, 6}, 6, 6, 6},
		{"unknown epoch ignored", []uint64{6, 99}, 1, 1, 1},
	}
	for _, c := range cases {
		epochsWithPoints := map[uint64]bool{}
		for _, epoch := range c.epochs {
			epochsWithPoints[epoch] = true
		}
		entries, latestEpoch := ComputePointsLeaderboard(
			[]PointsNetworkInput{{
				NetworkId:        networkId,
				TotalNanoPoints:  PointsToNanoPoints(10),
				CreateTime:       now,
				EpochsWithPoints: epochsWithPoints,
			}},
			windows,
		)
		connect.AssertEqual(t, latestEpoch, uint64(6))
		entry := pointsTestEntry(entries, networkId)
		if entry == nil {
			t.Fatalf("%s: no entry", c.name)
		}
		if entry.BlocksWithPoints != c.blocks || entry.Streak != c.streak || entry.LongestStreak != c.longest {
			t.Fatalf(
				"%s: blocks=%d streak=%d longest=%d, want %d/%d/%d",
				c.name, entry.BlocksWithPoints, entry.Streak, entry.LongestStreak, c.blocks, c.streak, c.longest,
			)
		}
	}
}

// Competition ranks (1, 2, 2, 4) per dimension; positions are a total order
// with the documented tie-breaks; zero-point networks are not ranked.
func TestComputePointsLeaderboardRanks(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	windows := pointsTestWindows(now, 4)
	a, b, c, d, e := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	set := func(epochs ...uint64) map[uint64]bool {
		m := map[uint64]bool{}
		for _, epoch := range epochs {
			m[epoch] = true
		}
		return m
	}
	inputs := []PointsNetworkInput{
		// a: most points, 2 blocks (3, 4), streak 2
		{NetworkId: a, TotalNanoPoints: PointsToNanoPoints(100), CreateTime: now.Add(-3 * time.Hour), EpochsWithPoints: set(3, 4)},
		// b: same points as c; 4 blocks, streak 4 -> ahead of c on the streak tie-break
		{NetworkId: b, TotalNanoPoints: PointsToNanoPoints(50), CreateTime: now.Add(-5 * time.Hour), EpochsWithPoints: set(1, 2, 3, 4)},
		// c: same points as b; 2 blocks (1, 2), streak 0
		{NetworkId: c, TotalNanoPoints: PointsToNanoPoints(50), CreateTime: now.Add(-1 * time.Hour), EpochsWithPoints: set(1, 2)},
		// d: fewest points, 1 block (4), streak 1
		{NetworkId: d, TotalNanoPoints: PointsToNanoPoints(1), CreateTime: now, EpochsWithPoints: set(4)},
		// e: no points -> not ranked
		{NetworkId: e, TotalNanoPoints: 0, CreateTime: now, EpochsWithPoints: set()},
	}
	entries, _ := ComputePointsLeaderboard(inputs, windows)
	connect.AssertEqual(t, len(entries), 4)
	connect.AssertEqual(t, pointsTestEntry(entries, e), (*PointsLeaderboardEntry)(nil))

	ea, eb, ec, ed := pointsTestEntry(entries, a), pointsTestEntry(entries, b), pointsTestEntry(entries, c), pointsTestEntry(entries, d)

	// points order is (points, streak, blocks): a(100) b(50, streak 4) c(50,
	// streak 0) d(1) -> b and c tie on points but not on streak, so the ranks
	// are 1, 2, 3, 4 (a rank is shared only when all three values tie)
	connect.AssertEqual(t, ea.RankPoints, int64(1))
	connect.AssertEqual(t, eb.RankPoints, int64(2))
	connect.AssertEqual(t, ec.RankPoints, int64(3))
	connect.AssertEqual(t, ed.RankPoints, int64(4))
	connect.AssertEqual(t, ea.PosPoints, int64(1))
	connect.AssertEqual(t, eb.PosPoints, int64(2))
	connect.AssertEqual(t, ec.PosPoints, int64(3))
	connect.AssertEqual(t, ed.PosPoints, int64(4))

	// blocks order is (blocks, streak, points): b(4) a(2, streak 2) c(2,
	// streak 0) d(1)
	connect.AssertEqual(t, eb.RankBlocks, int64(1))
	connect.AssertEqual(t, ea.RankBlocks, int64(2))
	connect.AssertEqual(t, ec.RankBlocks, int64(3))
	connect.AssertEqual(t, ed.RankBlocks, int64(4))
	connect.AssertEqual(t, eb.PosBlocks, int64(1))
	connect.AssertEqual(t, ea.PosBlocks, int64(2))
	connect.AssertEqual(t, ec.PosBlocks, int64(3))
	connect.AssertEqual(t, ed.PosBlocks, int64(4))

	// streak order is (streak, blocks, points): b(4) a(2) d(1) c(0)
	connect.AssertEqual(t, eb.RankStreak, int64(1))
	connect.AssertEqual(t, ea.RankStreak, int64(2))
	connect.AssertEqual(t, ed.RankStreak, int64(3))
	connect.AssertEqual(t, ec.RankStreak, int64(4))
	connect.AssertEqual(t, ec.PosStreak, int64(4))
}

// No finalized epochs: totals still rank, blocks and streaks are zero.
func TestComputePointsLeaderboardNoEpochs(t *testing.T) {
	a := server.NewId()
	entries, latestEpoch := ComputePointsLeaderboard(
		[]PointsNetworkInput{{NetworkId: a, TotalNanoPoints: PointsToNanoPoints(3), EpochsWithPoints: map[uint64]bool{}}},
		nil,
	)
	connect.AssertEqual(t, latestEpoch, uint64(0))
	connect.AssertEqual(t, len(entries), 1)
	connect.AssertEqual(t, entries[0].RankPoints, int64(1))
	connect.AssertEqual(t, entries[0].BlocksWithPoints, 0)
	connect.AssertEqual(t, entries[0].RankBlocks, int64(1))
	connect.AssertEqual(t, entries[0].RankStreak, int64(1))
}

func TestPointsLeaderboardDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		windows := pointsTestWindows(now, 3)
		inWindow := func(epoch uint64) time.Time {
			return windows[epoch-1].Start.Add(10 * time.Minute)
		}

		a, b, c, d := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, a, "points_a", server.NewId())
		Testing_CreateNetwork(ctx, b, "points_b", server.NewId())
		Testing_CreateNetwork(ctx, c, "points_c", server.NewId())
		Testing_CreateNetwork(ctx, d, "points_d", server.NewId())

		// a: 30 points across epochs 2 and 3 (streak 2); b: 20 points in epoch 3
		// only; c: 10 points in epoch 1 only (streak 0); d: no points
		Testing_InsertAccountPoint(ctx, a, PointsToNanoPoints(10), inWindow(2))
		Testing_InsertAccountPoint(ctx, a, PointsToNanoPoints(20), inWindow(3))
		Testing_InsertAccountPoint(ctx, b, PointsToNanoPoints(20), inWindow(3))
		Testing_InsertAccountPoint(ctx, c, PointsToNanoPoints(10), inWindow(1))

		// before any rebuild
		connect.AssertEqual(t, GetLatestPointsLeaderboardSnapshot(ctx), (*PointsLeaderboardSnapshot)(nil))

		snapshot, err := RebuildPointsLeaderboard(ctx, windows)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, snapshot.TotalRanked, int64(3))
		connect.AssertEqual(t, snapshot.LatestEpoch, uint64(3))
		latest := GetLatestPointsLeaderboardSnapshot(ctx)
		connect.AssertEqual(t, latest.SnapshotId, snapshot.SnapshotId)

		// nobody opted in: the page is empty, but every network with points has a row
		connect.AssertEqual(t, len(GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, PointsLeaderboardSortPoints, 0, 10)), 0)
		rowA := GetPointsLeaderboardNetworkRow(ctx, snapshot.SnapshotId, a)
		connect.AssertEqual(t, rowA.TotalNanoPoints, PointsToNanoPoints(30))
		connect.AssertEqual(t, rowA.BlocksWithPoints, 2)
		connect.AssertEqual(t, rowA.Streak, 2)
		connect.AssertEqual(t, rowA.RankPoints, int64(1))
		connect.AssertEqual(t, rowA.PointsLeaderboardPublic, false)
		connect.AssertEqual(t, GetPointsLeaderboardNetworkRow(ctx, snapshot.SnapshotId, d), (*PointsLeaderboardRow)(nil))

		// opt in a and c; set a's emoji and make c's name public
		SetNetworkPointsLeaderboardPublic(ctx, a, true)
		SetNetworkPointsLeaderboardPublic(ctx, c, true)
		SetNetworkEmojiTag(ctx, a, "🐬🔥")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET leaderboard_public = true WHERE network_id = $1`, c))
		})
		settings := GetNetworkPointsLeaderboardSettings(ctx, a)
		connect.AssertEqual(t, settings.PointsLeaderboardPublic, true)
		connect.AssertEqual(t, settings.EmojiTag, "🐬🔥")

		// the join reads the flags live: no rebuild needed. Points order: a, c
		// (b is ranked 2 but not listed -> the visible list has a gap)
		page := GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, PointsLeaderboardSortPoints, 0, 10)
		connect.AssertEqual(t, len(page), 2)
		connect.AssertEqual(t, page[0].NetworkId, a)
		connect.AssertEqual(t, page[0].EmojiTag, "🐬🔥")
		connect.AssertEqual(t, page[0].LeaderboardPublic, false)
		connect.AssertEqual(t, page[1].NetworkId, c)
		connect.AssertEqual(t, page[1].RankPoints, int64(3))
		connect.AssertEqual(t, page[1].LeaderboardPublic, true)
		connect.AssertEqual(t, page[1].NetworkName, "points_c")

		// keyset paging: one row at a time in blocks order (a: 2 blocks, c: 1)
		first := GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, PointsLeaderboardSortBlocks, 0, 1)
		connect.AssertEqual(t, len(first), 1)
		connect.AssertEqual(t, first[0].NetworkId, a)
		second := GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, PointsLeaderboardSortBlocks, first[0].Position(PointsLeaderboardSortBlocks), 1)
		connect.AssertEqual(t, len(second), 1)
		connect.AssertEqual(t, second[0].NetworkId, c)
		third := GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, PointsLeaderboardSortBlocks, second[0].Position(PointsLeaderboardSortBlocks), 1)
		connect.AssertEqual(t, len(third), 0)

		// streak order: a (2), b (1, in the latest epoch), c (0); b is not
		// listed, so the page shows a then c with c's rank 3 and position 3
		streakPage := GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, PointsLeaderboardSortStreak, 0, 10)
		connect.AssertEqual(t, len(streakPage), 2)
		connect.AssertEqual(t, streakPage[0].NetworkId, a)
		connect.AssertEqual(t, streakPage[1].NetworkId, c)
		connect.AssertEqual(t, streakPage[1].RankStreak, int64(3))
		connect.AssertEqual(t, streakPage[1].PosStreak, int64(3))

		// an unknown sort lists nothing rather than everything
		connect.AssertEqual(t, len(GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, "bogus", 0, 10)), 0)

		// clearing the emoji tag
		SetNetworkEmojiTag(ctx, a, "")
		connect.AssertEqual(t, GetNetworkPointsLeaderboardSettings(ctx, a).EmojiTag, "")

		// rebuilds retain the newest two snapshots and prune the rest
		snapshot2, err := RebuildPointsLeaderboard(ctx, windows)
		connect.AssertEqual(t, err, nil)
		snapshot3, err := RebuildPointsLeaderboard(ctx, windows)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, GetLatestPointsLeaderboardSnapshot(ctx).SnapshotId, snapshot3.SnapshotId)
		connect.AssertEqual(t, GetPointsLeaderboardSnapshot(ctx, snapshot2.SnapshotId).SnapshotId, snapshot2.SnapshotId)
		connect.AssertEqual(t, GetPointsLeaderboardSnapshot(ctx, snapshot.SnapshotId), (*PointsLeaderboardSnapshot)(nil))
		connect.AssertEqual(t, GetPointsLeaderboardNetworkRow(ctx, snapshot.SnapshotId, a), (*PointsLeaderboardRow)(nil))
		connect.AssertEqual(t, GetPointsLeaderboardNetworkRow(ctx, snapshot2.SnapshotId, a).RankPoints, int64(1))

		// a new finalized epoch with points for b only: b's streak becomes 1,
		// a's drops to 0 (missed the latest), blocks stay
		windows4 := append(windows, PointsEpochWindow{
			Epoch: 4,
			Start: now.Add(-30 * time.Minute),
			End:   now.Add(30 * time.Minute),
		})
		Testing_InsertAccountPoint(ctx, b, PointsToNanoPoints(5), now)
		snapshot4, err := RebuildPointsLeaderboard(ctx, windows4)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, snapshot4.LatestEpoch, uint64(4))
		rowA4 := GetPointsLeaderboardNetworkRow(ctx, snapshot4.SnapshotId, a)
		connect.AssertEqual(t, rowA4.Streak, 0)
		connect.AssertEqual(t, rowA4.LongestStreak, 2)
		connect.AssertEqual(t, rowA4.BlocksWithPoints, 2)
		rowB4 := GetPointsLeaderboardNetworkRow(ctx, snapshot4.SnapshotId, b)
		connect.AssertEqual(t, rowB4.Streak, 2)
		connect.AssertEqual(t, rowB4.BlocksWithPoints, 2)
		connect.AssertEqual(t, rowB4.TotalNanoPoints, PointsToNanoPoints(25))
		connect.AssertEqual(t, rowB4.RankStreak, int64(1))
	})
}

// Two networks that tie on all three values share every rank; the network id
// (ascending) still gives them distinct positions, so keyset paging never
// repeats or skips a row.
func TestComputePointsLeaderboardFullTie(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	windows := pointsTestWindows(now, 2)
	x, y := server.NewId(), server.NewId()
	if y.String() < x.String() {
		x, y = y, x
	}
	inputs := []PointsNetworkInput{
		{NetworkId: y, TotalNanoPoints: PointsToNanoPoints(7), CreateTime: now.Add(-time.Hour), EpochsWithPoints: map[uint64]bool{2: true}},
		{NetworkId: x, TotalNanoPoints: PointsToNanoPoints(7), CreateTime: now, EpochsWithPoints: map[uint64]bool{2: true}},
	}
	entries, _ := ComputePointsLeaderboard(inputs, windows)
	ex, ey := pointsTestEntry(entries, x), pointsTestEntry(entries, y)
	for _, pair := range [][2]int64{{ex.RankPoints, ey.RankPoints}, {ex.RankBlocks, ey.RankBlocks}, {ex.RankStreak, ey.RankStreak}} {
		connect.AssertEqual(t, pair[0], int64(1))
		connect.AssertEqual(t, pair[1], int64(1))
	}
	// x sorts before y on the id tie-break in every sort
	connect.AssertEqual(t, ex.PosPoints, int64(1))
	connect.AssertEqual(t, ey.PosPoints, int64(2))
	connect.AssertEqual(t, ex.PosBlocks, int64(1))
	connect.AssertEqual(t, ey.PosBlocks, int64(2))
	connect.AssertEqual(t, ex.PosStreak, int64(1))
	connect.AssertEqual(t, ey.PosStreak, int64(2))
}
