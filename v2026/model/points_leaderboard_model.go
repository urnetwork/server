package model

import (
	"context"
	"github.com/urnetwork/sdk/v2026"
	"sort"
	"time"

	"github.com/urnetwork/server/v2026"
)

// All-time points leaderboard (android/POINTSLEADERBOARD.md).
//
// Every network with any points is ranked on three dimensions -- total points,
// finalized epochs ("blocks") with points, and the current streak of
// consecutive finalized epochs with points -- into a snapshot table that a
// rebuild task rewrites whenever points can have changed. Reads page the
// newest snapshot with a keyset cursor and join `network` for the name, the
// emoji tag and the two public flags, so a toggle shows on the next request
// rather than the next rebuild. The opt-in (`points_leaderboard_public`) only
// controls display: ranks are over everyone, so the visible list has gaps and
// a toggle never shifts anyone else's rank.

const (
	PointsLeaderboardSortPoints = "points"
	PointsLeaderboardSortBlocks = "blocks"
	PointsLeaderboardSortStreak = "streak"
)

// PointsLeaderboardRetainedSnapshots is how many snapshots a rebuild keeps: the
// new one and its predecessor, so a cursor minted just before the rebuild still
// resolves to the rows it was paging.
const PointsLeaderboardRetainedSnapshots = 2

// PointsEpochWindow is one finalized epoch's wall-clock [Start, End) window. A
// network has points in the epoch when the sum of its account points created
// inside the window is positive.
type PointsEpochWindow struct {
	Epoch uint64
	Start time.Time
	End   time.Time
}

type PointsLeaderboardSnapshot struct {
	SnapshotId  server.Id `json:"snapshot_id"`
	CreateTime  time.Time `json:"create_time"`
	LatestEpoch uint64    `json:"latest_epoch"`
	TotalRanked int64     `json:"total_ranked"`
}

// PointsLeaderboardEntry is one stored row. rank_* is the competition rank
// (1, 2, 2, 4); pos_* is the row_number of the same ordering, a total order
// the keyset cursor pages on.
type PointsLeaderboardEntry struct {
	NetworkId        server.Id
	TotalNanoPoints  NanoPoints
	BlocksWithPoints int
	Streak           int
	LongestStreak    int
	RankPoints       int64
	RankBlocks       int64
	RankStreak       int64
	PosPoints        int64
	PosBlocks        int64
	PosStreak        int64
}

// PointsLeaderboardRow is an entry joined with the network's display fields.
type PointsLeaderboardRow struct {
	PointsLeaderboardEntry
	NetworkName             string
	EmojiTag                string
	LeaderboardPublic       bool
	PointsLeaderboardPublic bool
	ContainsProfanity       bool
}

// PointsNetworkInput is the per-network aggregate the ranking is computed from.
type PointsNetworkInput struct {
	NetworkId       server.Id
	TotalNanoPoints NanoPoints
	CreateTime      time.Time
	// EpochsWithPoints holds the finalized epochs in which the network earned
	// points.
	EpochsWithPoints map[uint64]bool
}

// ComputePointsLeaderboard ranks the inputs. It is pure so the streak and
// rank rules are testable without a database.
//
// The order per sort is the sdk's (ComparePointsLeaderboardKeys), the one
// definition shared with the view controller: points = (points, streak,
// blocks), blocks = (blocks, streak, points), streak = (streak, blocks,
// points), every key desc, then the network id asc -- a total order, so pages
// never overlap and every client renders the same sequence. The competition
// rank of an entry is the position of the first entry whose three values all
// tie with it (ComparePointsLeaderboardValues == 0).
func ComputePointsLeaderboard(
	inputs []PointsNetworkInput,
	windows []PointsEpochWindow,
) (entries []PointsLeaderboardEntry, latestEpoch uint64) {
	epochs := make([]uint64, 0, len(windows))
	for _, window := range windows {
		epochs = append(epochs, window.Epoch)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	if 0 < len(epochs) {
		latestEpoch = epochs[len(epochs)-1]
	}

	entries = make([]PointsLeaderboardEntry, 0, len(inputs))
	for _, input := range inputs {
		if input.TotalNanoPoints <= 0 {
			continue
		}
		blocks, streak, longest := pointsStreaks(input.EpochsWithPoints, epochs)
		entries = append(entries, PointsLeaderboardEntry{
			NetworkId:        input.NetworkId,
			TotalNanoPoints:  input.TotalNanoPoints,
			BlocksWithPoints: blocks,
			Streak:           streak,
			LongestStreak:    longest,
		})
	}

	keyOf := func(e *PointsLeaderboardEntry) *sdk.PointsLeaderboardKey {
		return &sdk.PointsLeaderboardKey{
			NanoPoints: int64(e.TotalNanoPoints),
			Blocks:     int64(e.BlocksWithPoints),
			Streak:     int64(e.Streak),
			NetworkId:  e.NetworkId.String(),
		}
	}

	assign := func(
		sortBy string,
		set func(*PointsLeaderboardEntry, int64, int64),
	) {
		order := make([]*PointsLeaderboardEntry, len(entries))
		keys := make(map[*PointsLeaderboardEntry]*sdk.PointsLeaderboardKey, len(entries))
		for i := range entries {
			order[i] = &entries[i]
			keys[order[i]] = keyOf(order[i])
		}
		sort.SliceStable(order, func(i, j int) bool {
			return sdk.ComparePointsLeaderboardKeys(sortBy, keys[order[i]], keys[order[j]]) < 0
		})
		rank := int64(0)
		var previous *PointsLeaderboardEntry
		for i, entry := range order {
			position := int64(i + 1)
			if previous == nil || sdk.ComparePointsLeaderboardValues(sortBy, keys[previous], keys[entry]) != 0 {
				rank = position
				previous = entry
			}
			set(entry, rank, position)
		}
	}
	assign(
		PointsLeaderboardSortPoints,
		func(e *PointsLeaderboardEntry, rank int64, pos int64) { e.RankPoints, e.PosPoints = rank, pos },
	)
	assign(
		PointsLeaderboardSortBlocks,
		func(e *PointsLeaderboardEntry, rank int64, pos int64) { e.RankBlocks, e.PosBlocks = rank, pos },
	)
	assign(
		PointsLeaderboardSortStreak,
		func(e *PointsLeaderboardEntry, rank int64, pos int64) { e.RankStreak, e.PosStreak = rank, pos },
	)
	return entries, latestEpoch
}

// pointsStreaks walks the finalized epochs in order. The current streak is
// the run of consecutive epochs with points that ends at the latest finalized
// epoch (0 when the latest epoch has none); the longest streak is the best
// run anywhere.
func pointsStreaks(epochsWithPoints map[uint64]bool, epochsAsc []uint64) (blocks int, streak int, longest int) {
	run := 0
	for _, epoch := range epochsAsc {
		if epochsWithPoints[epoch] {
			blocks += 1
			run += 1
			if longest < run {
				longest = run
			}
		} else {
			run = 0
		}
	}
	// run is the trailing run: non-zero only when the latest epoch has points
	streak = run
	return
}

// RebuildPointsLeaderboard aggregates every network's points, ranks them and
// writes a new snapshot, pruning to the retained count. `windows` are the
// finalized epochs (any order). Concurrent rebuilds serialize on an advisory
// lock at write time; each still produces a complete snapshot, so the worst
// case of a race is one extra snapshot that the next prune removes.
func RebuildPointsLeaderboard(ctx context.Context, windows []PointsEpochWindow) (*PointsLeaderboardSnapshot, error) {
	inputs, err := loadPointsNetworkInputs(ctx, windows)
	if err != nil {
		return nil, err
	}
	entries, latestEpoch := ComputePointsLeaderboard(inputs, windows)

	snapshot := &PointsLeaderboardSnapshot{
		SnapshotId:  server.NewId(),
		CreateTime:  server.NowUtc(),
		LatestEpoch: latestEpoch,
		TotalRanked: int64(len(entries)),
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"network_points_leaderboard_rebuild",
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_points_leaderboard_snapshot
				(snapshot_id, create_time, latest_epoch, total_ranked)
				VALUES ($1, $2, $3, $4)
			`,
			snapshot.SnapshotId,
			snapshot.CreateTime,
			int64(snapshot.LatestEpoch),
			snapshot.TotalRanked,
		))
		const chunkSize = 5000
		for start := 0; start < len(entries); start += chunkSize {
			end := start + chunkSize
			if len(entries) < end {
				end = len(entries)
			}
			chunk := entries[start:end]
			networkIds := make([]string, len(chunk))
			totals := make([]int64, len(chunk))
			blocks := make([]int32, len(chunk))
			streaks := make([]int32, len(chunk))
			longests := make([]int32, len(chunk))
			rankPoints := make([]int64, len(chunk))
			rankBlocks := make([]int64, len(chunk))
			rankStreaks := make([]int64, len(chunk))
			posPoints := make([]int64, len(chunk))
			posBlocks := make([]int64, len(chunk))
			posStreaks := make([]int64, len(chunk))
			for i, entry := range chunk {
				networkIds[i] = entry.NetworkId.String()
				totals[i] = int64(entry.TotalNanoPoints)
				blocks[i] = int32(entry.BlocksWithPoints)
				streaks[i] = int32(entry.Streak)
				longests[i] = int32(entry.LongestStreak)
				rankPoints[i] = entry.RankPoints
				rankBlocks[i] = entry.RankBlocks
				rankStreaks[i] = entry.RankStreak
				posPoints[i] = entry.PosPoints
				posBlocks[i] = entry.PosBlocks
				posStreaks[i] = entry.PosStreak
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_points_leaderboard (
						snapshot_id,
						network_id,
						total_nano_points,
						blocks_with_points,
						streak,
						longest_streak,
						rank_points,
						rank_blocks,
						rank_streak,
						pos_points,
						pos_blocks,
						pos_streak
					)
					SELECT
						$1,
						t.network_id,
						t.total_nano_points,
						t.blocks_with_points,
						t.streak,
						t.longest_streak,
						t.rank_points,
						t.rank_blocks,
						t.rank_streak,
						t.pos_points,
						t.pos_blocks,
						t.pos_streak
					FROM unnest(
						$2::uuid[],
						$3::bigint[],
						$4::int[],
						$5::int[],
						$6::int[],
						$7::bigint[],
						$8::bigint[],
						$9::bigint[],
						$10::bigint[],
						$11::bigint[],
						$12::bigint[]
					) AS t(
						network_id,
						total_nano_points,
						blocks_with_points,
						streak,
						longest_streak,
						rank_points,
						rank_blocks,
						rank_streak,
						pos_points,
						pos_blocks,
						pos_streak
					)
				`,
				snapshot.SnapshotId,
				networkIds,
				totals,
				blocks,
				streaks,
				longests,
				rankPoints,
				rankBlocks,
				rankStreaks,
				posPoints,
				posBlocks,
				posStreaks,
			))
		}
		// prune to the retained snapshots (newest first)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_points_leaderboard
				WHERE snapshot_id NOT IN (
					SELECT snapshot_id FROM network_points_leaderboard_snapshot
					ORDER BY create_time DESC, snapshot_id DESC
					LIMIT $1
				)
			`,
			PointsLeaderboardRetainedSnapshots,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_points_leaderboard_snapshot
				WHERE snapshot_id NOT IN (
					SELECT snapshot_id FROM network_points_leaderboard_snapshot
					ORDER BY create_time DESC, snapshot_id DESC
					LIMIT $1
				)
			`,
			PointsLeaderboardRetainedSnapshots,
		))
	})
	return snapshot, nil
}

// loadPointsNetworkInputs reads every network's all-time total and, per
// finalized epoch window, whether it earned points inside the window.
func loadPointsNetworkInputs(ctx context.Context, windows []PointsEpochWindow) (inputs []PointsNetworkInput, returnErr error) {
	byNetwork := map[server.Id]*PointsNetworkInput{}
	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					account_point.network_id,
					SUM(account_point.point_value)::bigint AS total_nano_points,
					network.create_time
				FROM account_point
				INNER JOIN network ON network.network_id = account_point.network_id
				GROUP BY account_point.network_id, network.create_time
				HAVING 0 < SUM(account_point.point_value)
			`,
		)
		server.WithPgResult(result, err, func() {
			if err != nil {
				returnErr = err
				return
			}
			for result.Next() {
				input := &PointsNetworkInput{
					EpochsWithPoints: map[uint64]bool{},
				}
				server.Raise(result.Scan(
					&input.NetworkId,
					&input.TotalNanoPoints,
					&input.CreateTime,
				))
				byNetwork[input.NetworkId] = input
			}
		})
	})
	if returnErr != nil {
		return nil, returnErr
	}
	if 0 < len(windows) && 0 < len(byNetwork) {
		epochs := make([]int64, len(windows))
		starts := make([]time.Time, len(windows))
		ends := make([]time.Time, len(windows))
		for i, window := range windows {
			epochs[i] = int64(window.Epoch)
			starts[i] = window.Start.UTC()
			ends[i] = window.End.UTC()
		}
		server.ReplicaDb(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT w.epoch, account_point.network_id
					FROM unnest($1::bigint[], $2::timestamp[], $3::timestamp[]) AS w(epoch, start_time, end_time)
					INNER JOIN account_point ON
						w.start_time <= account_point.create_time AND
						account_point.create_time < w.end_time
					GROUP BY w.epoch, account_point.network_id
					HAVING 0 < SUM(account_point.point_value)
				`,
				epochs,
				starts,
				ends,
			)
			server.WithPgResult(result, err, func() {
				if err != nil {
					returnErr = err
					return
				}
				for result.Next() {
					var epoch int64
					var networkId server.Id
					server.Raise(result.Scan(&epoch, &networkId))
					if input, ok := byNetwork[networkId]; ok {
						input.EpochsWithPoints[uint64(epoch)] = true
					}
				}
			})
		})
		if returnErr != nil {
			return nil, returnErr
		}
	}
	inputs = make([]PointsNetworkInput, 0, len(byNetwork))
	for _, input := range byNetwork {
		inputs = append(inputs, *input)
	}
	return inputs, nil
}

const pointsLeaderboardSnapshotSelect = `
	SELECT snapshot_id, create_time, latest_epoch, total_ranked
	FROM network_points_leaderboard_snapshot
`

func scanPointsLeaderboardSnapshot(result server.PgResult) *PointsLeaderboardSnapshot {
	snapshot := &PointsLeaderboardSnapshot{}
	var latestEpoch int64
	server.Raise(result.Scan(
		&snapshot.SnapshotId,
		&snapshot.CreateTime,
		&latestEpoch,
		&snapshot.TotalRanked,
	))
	snapshot.LatestEpoch = uint64(latestEpoch)
	return snapshot
}

// GetLatestPointsLeaderboardSnapshot is nil before the first rebuild.
func GetLatestPointsLeaderboardSnapshot(ctx context.Context) (snapshot *PointsLeaderboardSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			pointsLeaderboardSnapshotSelect+`ORDER BY create_time DESC, snapshot_id DESC LIMIT 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				snapshot = scanPointsLeaderboardSnapshot(result)
			}
		})
	})
	return
}

// GetPointsLeaderboardSnapshot is nil when the snapshot was pruned.
func GetPointsLeaderboardSnapshot(ctx context.Context, snapshotId server.Id) (snapshot *PointsLeaderboardSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			pointsLeaderboardSnapshotSelect+`WHERE snapshot_id = $1`,
			snapshotId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				snapshot = scanPointsLeaderboardSnapshot(result)
			}
		})
	})
	return
}

func pointsLeaderboardPositionColumn(sortBy string) (string, bool) {
	switch sortBy {
	case PointsLeaderboardSortPoints:
		return "pos_points", true
	case PointsLeaderboardSortBlocks:
		return "pos_blocks", true
	case PointsLeaderboardSortStreak:
		return "pos_streak", true
	}
	return "", false
}

const pointsLeaderboardRowSelect = `
	SELECT
		network_points_leaderboard.network_id,
		network_points_leaderboard.total_nano_points,
		network_points_leaderboard.blocks_with_points,
		network_points_leaderboard.streak,
		network_points_leaderboard.longest_streak,
		network_points_leaderboard.rank_points,
		network_points_leaderboard.rank_blocks,
		network_points_leaderboard.rank_streak,
		network_points_leaderboard.pos_points,
		network_points_leaderboard.pos_blocks,
		network_points_leaderboard.pos_streak,
		network.network_name,
		COALESCE(network.emoji_tag, ''),
		network.leaderboard_public,
		network.points_leaderboard_public,
		network.contains_profanity
	FROM network_points_leaderboard
	INNER JOIN network ON network.network_id = network_points_leaderboard.network_id
`

func scanPointsLeaderboardRow(result server.PgResult) *PointsLeaderboardRow {
	row := &PointsLeaderboardRow{}
	var total int64
	server.Raise(result.Scan(
		&row.NetworkId,
		&total,
		&row.BlocksWithPoints,
		&row.Streak,
		&row.LongestStreak,
		&row.RankPoints,
		&row.RankBlocks,
		&row.RankStreak,
		&row.PosPoints,
		&row.PosBlocks,
		&row.PosStreak,
		&row.NetworkName,
		&row.EmojiTag,
		&row.LeaderboardPublic,
		&row.PointsLeaderboardPublic,
		&row.ContainsProfanity,
	))
	row.TotalNanoPoints = NanoPoints(total)
	return row
}

// Position returns the row's position in the given sort.
func (self *PointsLeaderboardEntry) Position(sortBy string) int64 {
	switch sortBy {
	case PointsLeaderboardSortBlocks:
		return self.PosBlocks
	case PointsLeaderboardSortStreak:
		return self.PosStreak
	default:
		return self.PosPoints
	}
}

// GetPointsLeaderboardPage lists the opted-in networks of a snapshot after
// `afterPosition` in the given sort (0 = from the top), at most `limit` rows.
// `sortBy` must be one of the PointsLeaderboardSort* constants.
func GetPointsLeaderboardPage(
	ctx context.Context,
	snapshotId server.Id,
	sortBy string,
	afterPosition int64,
	limit int,
) (rows []*PointsLeaderboardRow) {
	rows = []*PointsLeaderboardRow{}
	column, ok := pointsLeaderboardPositionColumn(sortBy)
	if !ok || limit <= 0 {
		return
	}
	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			pointsLeaderboardRowSelect+`
				WHERE
					network_points_leaderboard.snapshot_id = $1 AND
					network.points_leaderboard_public = true AND
					$2 < network_points_leaderboard.`+column+`
				ORDER BY network_points_leaderboard.`+column+` ASC
				LIMIT $3
			`,
			snapshotId,
			afterPosition,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanPointsLeaderboardRow(result))
			}
		})
	})
	return
}

// GetPointsLeaderboardNetworkRow is the network's own row in the snapshot,
// opted in or not; nil when the network has no points.
func GetPointsLeaderboardNetworkRow(
	ctx context.Context,
	snapshotId server.Id,
	networkId server.Id,
) (row *PointsLeaderboardRow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			pointsLeaderboardRowSelect+`
				WHERE
					network_points_leaderboard.snapshot_id = $1 AND
					network_points_leaderboard.network_id = $2
			`,
			snapshotId,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = scanPointsLeaderboardRow(result)
			}
		})
	})
	return
}

// NetworkPointsLeaderboardSettings are the network's own display settings.
type NetworkPointsLeaderboardSettings struct {
	PointsLeaderboardPublic bool
	EmojiTag                string
}

func GetNetworkPointsLeaderboardSettings(ctx context.Context, networkId server.Id) (settings NetworkPointsLeaderboardSettings) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT points_leaderboard_public, COALESCE(emoji_tag, '')
				FROM network
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&settings.PointsLeaderboardPublic,
					&settings.EmojiTag,
				))
			}
		})
	})
	return
}

// SetNetworkPointsLeaderboardPublic is the points leaderboard opt-in.
func SetNetworkPointsLeaderboardPublic(ctx context.Context, networkId server.Id, isPublic bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network
				SET points_leaderboard_public = $1
				WHERE network_id = $2
			`,
			isPublic,
			networkId,
		))
	})
}

// SetNetworkEmojiTag stores an already-validated tag; "" clears it.
func SetNetworkEmojiTag(ctx context.Context, networkId server.Id, emojiTag string) {
	server.Tx(ctx, func(tx server.PgTx) {
		var value *string
		if emojiTag != "" {
			value = &emojiTag
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network
				SET emoji_tag = $1
				WHERE network_id = $2
			`,
			value,
			networkId,
		))
	})
}

// Testing_InsertAccountPoint writes one point row at an explicit create time,
// so a test can place points inside or outside an epoch window.
func Testing_InsertAccountPoint(ctx context.Context, networkId server.Id, nanoPoints NanoPoints, createTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO account_point (account_point_id, network_id, event, point_value, create_time)
				VALUES ($1, $2, $3, $4, $5)
			`,
			server.NewId(),
			networkId,
			AccountPointEventPayout,
			nanoPoints,
			createTime.UTC(),
		))
	})
}
