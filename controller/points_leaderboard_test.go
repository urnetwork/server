package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestPointsLeaderboardCursor(t *testing.T) {
	snapshotId := server.NewId()
	encoded := encodePointsLeaderboardCursor(pointsLeaderboardCursor{
		SnapshotId: snapshotId,
		Sort:       model.PointsLeaderboardSortBlocks,
		Position:   42,
	})
	cursor, err := decodePointsLeaderboardCursor(encoded)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, cursor.SnapshotId, snapshotId)
	connect.AssertEqual(t, cursor.Sort, model.PointsLeaderboardSortBlocks)
	connect.AssertEqual(t, cursor.Position, int64(42))

	// surrounding whitespace is tolerated; garbage, a bad sort and a negative
	// position are not
	_, err = decodePointsLeaderboardCursor(" " + encoded + "\n")
	connect.AssertEqual(t, err, nil)
	_, err = decodePointsLeaderboardCursor("not a cursor")
	connect.AssertNotEqual(t, err, nil)
	_, err = decodePointsLeaderboardCursor(encodePointsLeaderboardCursor(pointsLeaderboardCursor{
		SnapshotId: snapshotId,
		Sort:       "bogus",
		Position:   1,
	}))
	connect.AssertNotEqual(t, err, nil)
	_, err = decodePointsLeaderboardCursor(encodePointsLeaderboardCursor(pointsLeaderboardCursor{
		SnapshotId: snapshotId,
		Sort:       model.PointsLeaderboardSortPoints,
		Position:   -1,
	}))
	connect.AssertNotEqual(t, err, nil)
}

func TestPointsLeaderboardSortAndLimit(t *testing.T) {
	for input, want := range map[string]string{
		"":        model.PointsLeaderboardSortPoints,
		"points":  model.PointsLeaderboardSortPoints,
		" Blocks": model.PointsLeaderboardSortBlocks,
		"STREAK":  model.PointsLeaderboardSortStreak,
	} {
		sortBy, ok := pointsLeaderboardSort(input)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, sortBy, want)
	}
	_, ok := pointsLeaderboardSort("mib")
	connect.AssertEqual(t, ok, false)

	connect.AssertEqual(t, pointsLeaderboardLimit(0), pointsLeaderboardDefaultLimit)
	connect.AssertEqual(t, pointsLeaderboardLimit(-5), pointsLeaderboardDefaultLimit)
	connect.AssertEqual(t, pointsLeaderboardLimit(7), 7)
	connect.AssertEqual(t, pointsLeaderboardLimit(10000), pointsLeaderboardMaxLimit)
}

func TestPointsLeaderboardRowFromModel(t *testing.T) {
	networkId := server.NewId()
	hidden := pointsLeaderboardRowFromModel(&model.PointsLeaderboardRow{
		PointsLeaderboardEntry: model.PointsLeaderboardEntry{
			NetworkId:       networkId,
			TotalNanoPoints: model.PointsToNanoPoints(12.5),
			RankPoints:      3,
		},
		NetworkName:       "secret",
		EmojiTag:          "🦊",
		LeaderboardPublic: false,
		ContainsProfanity: true,
	})
	connect.AssertEqual(t, hidden.Anonymous, true)
	connect.AssertEqual(t, hidden.NetworkName, "")
	connect.AssertEqual(t, hidden.ContainsProfanity, false)
	connect.AssertEqual(t, hidden.EmojiTag, "🦊")
	connect.AssertEqual(t, hidden.TotalPoints, 12.5)
	connect.AssertEqual(t, hidden.RankPoints, int64(3))

	public := pointsLeaderboardRowFromModel(&model.PointsLeaderboardRow{
		PointsLeaderboardEntry: model.PointsLeaderboardEntry{NetworkId: networkId, TotalNanoPoints: 1},
		NetworkName:            "shown",
		LeaderboardPublic:      true,
	})
	connect.AssertEqual(t, public.Anonymous, false)
	connect.AssertEqual(t, public.NetworkName, "shown")
}

func TestPointsLeaderboardApiDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		// five finalized epochs, one hour each, the latest ending an hour ago
		windows := []model.PointsEpochWindow{}
		for i := 0; i < 5; i += 1 {
			end := now.Add(-time.Duration(5-i) * time.Hour)
			windows = append(windows, model.PointsEpochWindow{Epoch: uint64(i + 1), Start: end.Add(-1 * time.Hour), End: end})
		}
		inWindow := func(epoch uint64) time.Time { return windows[epoch-1].Start.Add(5 * time.Minute) }

		ids := []server.Id{}
		sessions := []*session.ClientSession{}
		for i := 0; i < 5; i += 1 {
			networkId := server.NewId()
			userId := server.NewId()
			name := string(rune('a' + i))
			model.Testing_CreateNetwork(ctx, networkId, "points_api_"+name, userId)
			ids = append(ids, networkId)
			sessions = append(sessions, session.Testing_CreateClientSession(ctx, jwt.NewByJwt(networkId, userId, name, false, false)))
			// network i earns 10*(5-i) points in every epoch up to i+1
			for epoch := uint64(1); epoch <= uint64(i+1); epoch += 1 {
				model.Testing_InsertAccountPoint(ctx, networkId, model.PointsToNanoPoints(float64(10*(5-i))), inWindow(epoch))
			}
		}
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()

		// before the first rebuild: empty, no error; a signed-in caller still
		// gets its settings
		empty, err := GetPointsLeaderboard(&PointsLeaderboardArgs{}, sessions[0])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, empty.Error, (*PointsLeaderboardError)(nil))
		connect.AssertEqual(t, len(empty.Rows), 0)
		connect.AssertEqual(t, empty.Me.Ranked, false)
		connect.AssertEqual(t, empty.Me.NetworkId, ids[0])

		// windows are faked: the rebuild never touches the chain
		pointsLeaderboardEpochWindowFunc = func(ctx context.Context, row *model.StEpoch) (time.Time, time.Time) {
			return windows[row.Epoch-1].Start, windows[row.Epoch-1].End
		}
		defer func() { pointsLeaderboardEpochWindowFunc = snEpochWindow }()
		for _, window := range windows {
			finalized := now
			model.UpsertStEpoch(ctx, &model.StEpoch{
				Epoch:         window.Epoch,
				StartBlock:    window.Epoch * 100,
				Status:        model.StEpochStatusFinalized,
				FinalizedTime: &finalized,
			})
		}
		rebuilt, err := RebuildPointsLeaderboard(&RebuildPointsLeaderboardArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rebuilt.TotalRanked, int64(5))
		connect.AssertEqual(t, rebuilt.LatestEpoch, uint64(5))

		// opt in everyone but network 2; network 0 shows its name
		for i, clientSession := range sessions {
			if i == 2 {
				continue
			}
			res, err := SetNetworkPointsLeaderboardPublic(&SetNetworkPointsRankingPublicArgs{Public: true}, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, res.PointsLeaderboardPublic, true)
		}
		_, err = SetNetworkLeaderboardRankingPublic(SetNetworkRankingPublicArgs{IsPublic: true}, sessions[0])
		connect.AssertEqual(t, err, nil)

		// emoji tag: valid, invalid, cleared
		tagRes, err := SetNetworkEmojiTag(&SetNetworkEmojiTagArgs{EmojiTag: " 🐬🔥 "}, sessions[1])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, tagRes.Error, (*SetNetworkEmojiTagError)(nil))
		connect.AssertEqual(t, tagRes.EmojiTag, "🐬🔥")
		badRes, err := SetNetworkEmojiTag(&SetNetworkEmojiTagArgs{EmojiTag: "hi🐬"}, sessions[1])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, badRes.Error.Message, pointsLeaderboardEmojiTagMessage)
		tooMany, err := SetNetworkEmojiTag(&SetNetworkEmojiTagArgs{EmojiTag: "🐬🐬🐬🐬🐬🐬🐬"}, sessions[1])
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, tooMany.Error, (*SetNetworkEmojiTagError)(nil))
		connect.AssertEqual(t, model.GetNetworkPointsLeaderboardSettings(ctx, ids[1]).EmojiTag, "🐬🔥")

		// signed-out page in points order. Network i earned 10*(5-i) points in
		// each of epochs 1..i+1: totals n0 = 50, n1 = 80, n2 = 90, n3 = 80,
		// n4 = 50; blocks n0..n4 = 1..5; only n4 has points in the latest
		// epoch (5), so its streak is 5 and every other streak is 0.
		page, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Limit: 2}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, page.Error, (*PointsLeaderboardError)(nil))
		connect.AssertEqual(t, page.Me, (*PointsLeaderboardMe)(nil))
		connect.AssertEqual(t, page.TotalRanked, int64(5))
		connect.AssertEqual(t, page.LatestEpoch, uint64(5))
		connect.AssertEqual(t, len(page.Rows), 2)
		// n2 (90) is ranked 1 but hidden; n1 and n3 tie at 80 -> rank 2 both,
		// and the blocks tie-break puts n3 (4 blocks) before n1 (2 blocks)
		connect.AssertEqual(t, page.Rows[0].NetworkId, ids[3])
		connect.AssertEqual(t, page.Rows[0].RankPoints, int64(2))
		connect.AssertEqual(t, page.Rows[0].TotalPoints, 80.0)
		connect.AssertEqual(t, page.Rows[0].Anonymous, true)
		connect.AssertEqual(t, page.Rows[0].NetworkName, "")
		connect.AssertEqual(t, page.Rows[1].NetworkId, ids[1])
		connect.AssertEqual(t, page.Rows[1].RankPoints, int64(2))
		connect.AssertEqual(t, page.Rows[1].EmojiTag, "🐬🔥")
		connect.AssertEqual(t, page.Rows[1].Anonymous, true)
		connect.AssertNotEqual(t, page.NextCursor, "")

		next, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Limit: 2, Cursor: page.NextCursor}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(next.Rows), 2)
		// n0 and n4 tie at 50 -> rank 4 (competition: 1, 2, 2, 4, 4); n4 has
		// more blocks so it comes first; n0 made its name public
		connect.AssertEqual(t, next.Rows[0].NetworkId, ids[4])
		connect.AssertEqual(t, next.Rows[0].RankPoints, int64(4))
		connect.AssertEqual(t, next.Rows[1].NetworkId, ids[0])
		connect.AssertEqual(t, next.Rows[1].RankPoints, int64(4))
		connect.AssertEqual(t, next.Rows[1].Anonymous, false)
		connect.AssertEqual(t, next.Rows[1].NetworkName, "points_api_a")
		connect.AssertEqual(t, next.NextCursor, "")

		// a cursor from another sort is refused
		wrongSort, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Sort: "streak", Cursor: page.NextCursor}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, wrongSort.Error, (*PointsLeaderboardError)(nil))
		bogus, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Cursor: "nope"}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, bogus.Error, (*PointsLeaderboardError)(nil))
		unknownSort, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Sort: "mib"}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, unknownSort.Error, (*PointsLeaderboardError)(nil))

		// streak order: n4 (5) is rank 1; everyone else ties at 0 -> rank 2,
		// ordered by total points then blocks (n2 90, n3 80/4, n1 80/2, n0 50).
		// n2 is hidden, so the caller (n2) sees n4, n3, n1, n0 and itself in `me`.
		streak, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Sort: "streak"}, sessions[2])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(streak.Rows), 4)
		connect.AssertEqual(t, streak.Rows[0].NetworkId, ids[4])
		connect.AssertEqual(t, streak.Rows[0].RankStreak, int64(1))
		connect.AssertEqual(t, streak.Rows[0].Streak, 5)
		connect.AssertEqual(t, streak.Rows[1].NetworkId, ids[3])
		connect.AssertEqual(t, streak.Rows[1].RankStreak, int64(2))
		connect.AssertEqual(t, streak.Rows[2].NetworkId, ids[1])
		connect.AssertEqual(t, streak.Rows[2].RankStreak, int64(2))
		connect.AssertEqual(t, streak.Rows[3].NetworkId, ids[0])
		// the hidden caller still sees itself: rank 2 by streak, rank 1 by points
		connect.AssertEqual(t, streak.Me.Ranked, true)
		connect.AssertEqual(t, streak.Me.NetworkId, ids[2])
		connect.AssertEqual(t, streak.Me.PointsLeaderboardPublic, false)
		connect.AssertEqual(t, streak.Me.RankStreak, int64(2))
		connect.AssertEqual(t, streak.Me.RankPoints, int64(1))
		connect.AssertEqual(t, streak.Me.RankBlocks, int64(3))
		connect.AssertEqual(t, streak.Me.NetworkName, "points_api_c")
		connect.AssertEqual(t, streak.Me.TotalPoints, 90.0)

		// /network/ranking carries the points fields
		ranking, err := GetNetworkLeaderboardRanking(sessions[1])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ranking.NetworkRanking.PointsLeaderboardPublic, true)
		connect.AssertEqual(t, ranking.NetworkRanking.EmojiTag, "🐬🔥")
		connect.AssertEqual(t, ranking.NetworkRanking.RankPoints, int64(2))
		connect.AssertEqual(t, ranking.NetworkRanking.RankBlocks, int64(4))
		connect.AssertEqual(t, ranking.NetworkRanking.RankStreak, int64(2))

		// a pruned snapshot asks the client to restart
		firstSnapshotCursor := page.NextCursor
		for i := 0; i < model.PointsLeaderboardRetainedSnapshots; i += 1 {
			_, err = RebuildPointsLeaderboard(&RebuildPointsLeaderboardArgs{}, anonymous)
			connect.AssertEqual(t, err, nil)
		}
		restart, err := GetPointsLeaderboard(&PointsLeaderboardArgs{Limit: 2, Cursor: firstSnapshotCursor}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, restart.Restart, true)
		connect.AssertEqual(t, len(restart.Rows), 0)

		// a fresh first page works after the prune
		fresh, err := GetPointsLeaderboard(&PointsLeaderboardArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(fresh.Rows), 4)
		connect.AssertEqual(t, fresh.NextCursor, "")
	})
}
