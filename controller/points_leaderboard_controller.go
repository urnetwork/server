package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect/emoji"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// All-time points leaderboard api + rebuild task (android/POINTSLEADERBOARD.md).
// The model owns the ranking rules and the snapshot table; this file owns the
// wire shapes, the keyset cursor, the epoch windows and the scheduling.
//
// Every ranked network is listed, as one continuous list paged in chunks. A
// network's name appears only when it turned on `points_leaderboard_public`
// (the Points tab's "show my network name" switch); every other row is
// anonymous. The emoji tag shows on every row that set one.

const (
	pointsLeaderboardDefaultLimit = 50
	pointsLeaderboardMaxLimit     = 200

	// pointsLeaderboardRebuildInterval is the fallback cadence. Rebuilds are
	// also triggered when an epoch finalizes and when a payout plan commits,
	// the only times points change.
	pointsLeaderboardRebuildInterval = 1 * time.Hour

	// pointsLeaderboardEpochLimit bounds the finalized epochs a rebuild reads.
	// Epochs are days apart, so this is decades of history.
	pointsLeaderboardEpochLimit = 100000

	pointsLeaderboardEmojiTagMessage = "Use 1 to 6 emoji."
)

type PointsLeaderboardArgs struct {
	// Sort is one of "points" (default), "blocks", "streak".
	Sort string `json:"sort,omitempty"`
	// Cursor continues a previous page; omit for the first page.
	Cursor string `json:"cursor,omitempty"`
	// Limit is the page size, default 50, max 200.
	Limit int `json:"limit,omitempty"`
}

type PointsLeaderboardRow struct {
	NetworkId server.Id `json:"network_id"`
	// NetworkName is present only when the network turned on
	// points_leaderboard_public; otherwise Anonymous is true and the row is
	// still listed.
	NetworkName string `json:"network_name,omitempty"`
	// EmojiTag is shown whether or not the name is.
	EmojiTag  string `json:"emoji_tag,omitempty"`
	Anonymous bool   `json:"anonymous"`
	// ContainsProfanity flags a public name the apps may mask.
	ContainsProfanity bool    `json:"contains_profanity,omitempty"`
	TotalPoints       float64 `json:"total_points"`
	BlocksWithPoints  int     `json:"blocks_with_points"`
	Streak            int     `json:"streak"`
	LongestStreak     int     `json:"longest_streak"`
	RankPoints        int64   `json:"rank_points"`
	RankBlocks        int64   `json:"rank_blocks"`
	RankStreak        int64   `json:"rank_streak"`
}

// PointsLeaderboardMe is the caller's own row, named or not.
type PointsLeaderboardMe struct {
	PointsLeaderboardRow
	PointsLeaderboardPublic bool `json:"points_leaderboard_public"`
	// Ranked is false when the network has no points yet (the row fields are
	// then zero).
	Ranked bool `json:"ranked"`
}

type PointsLeaderboardResult struct {
	Rows []PointsLeaderboardRow `json:"rows"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// Restart is true when the cursor's snapshot is gone; the client reloads
	// from the top.
	Restart      bool                    `json:"restart,omitempty"`
	TotalRanked  int64                   `json:"total_ranked"`
	SnapshotTime *time.Time              `json:"snapshot_time,omitempty"`
	LatestEpoch  uint64                  `json:"latest_epoch"`
	Me           *PointsLeaderboardMe    `json:"me,omitempty"`
	Error        *PointsLeaderboardError `json:"error,omitempty"`
}

type PointsLeaderboardError struct {
	Message string `json:"message"`
}

// pointsLeaderboardCursor pins the snapshot and the sort so a page sequence
// stays consistent across rebuilds and cannot be replayed under another sort.
type pointsLeaderboardCursor struct {
	SnapshotId server.Id `json:"s"`
	Sort       string    `json:"o"`
	Position   int64     `json:"p"`
}

func encodePointsLeaderboardCursor(cursor pointsLeaderboardCursor) string {
	cursorJson, err := json.Marshal(cursor)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(cursorJson)
}

func decodePointsLeaderboardCursor(s string) (pointsLeaderboardCursor, error) {
	cursorJson, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return pointsLeaderboardCursor{}, err
	}
	var cursor pointsLeaderboardCursor
	if err := json.Unmarshal(cursorJson, &cursor); err != nil {
		return pointsLeaderboardCursor{}, err
	}
	if _, ok := pointsLeaderboardSort(cursor.Sort); !ok || cursor.Position < 0 {
		return pointsLeaderboardCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func pointsLeaderboardSort(sortBy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(sortBy)) {
	case "", model.PointsLeaderboardSortPoints:
		return model.PointsLeaderboardSortPoints, true
	case model.PointsLeaderboardSortBlocks:
		return model.PointsLeaderboardSortBlocks, true
	case model.PointsLeaderboardSortStreak:
		return model.PointsLeaderboardSortStreak, true
	}
	return "", false
}

func pointsLeaderboardLimit(limit int) int {
	if limit <= 0 {
		return pointsLeaderboardDefaultLimit
	}
	if pointsLeaderboardMaxLimit < limit {
		return pointsLeaderboardMaxLimit
	}
	return limit
}

func pointsLeaderboardRowFromModel(row *model.PointsLeaderboardRow) PointsLeaderboardRow {
	out := PointsLeaderboardRow{
		NetworkId:        row.NetworkId,
		EmojiTag:         row.EmojiTag,
		Anonymous:        !row.PointsLeaderboardPublic,
		TotalPoints:      float64(row.TotalNanoPoints) / snNanoPointsPerPoint,
		BlocksWithPoints: row.BlocksWithPoints,
		Streak:           row.Streak,
		LongestStreak:    row.LongestStreak,
		RankPoints:       row.RankPoints,
		RankBlocks:       row.RankBlocks,
		RankStreak:       row.RankStreak,
	}
	if row.PointsLeaderboardPublic {
		out.NetworkName = row.NetworkName
		out.ContainsProfanity = row.ContainsProfanity
	}
	return out
}

func pointsLeaderboardError(message string) *PointsLeaderboardResult {
	return &PointsLeaderboardResult{
		Rows:  []PointsLeaderboardRow{},
		Error: &PointsLeaderboardError{Message: message},
	}
}

// GetPointsLeaderboard pages the newest snapshot: every ranked network, in
// the sort's order, named only where the network turned on
// points_leaderboard_public. A signed-in caller also gets `me`, its own row
// with its own name whether or not that switch is on.
func GetPointsLeaderboard(
	args *PointsLeaderboardArgs,
	clientSession *session.ClientSession,
) (*PointsLeaderboardResult, error) {
	ctx := clientSession.Ctx
	sortBy, ok := pointsLeaderboardSort(args.Sort)
	if !ok {
		return pointsLeaderboardError("Unknown sort."), nil
	}
	limit := pointsLeaderboardLimit(args.Limit)

	var snapshot *model.PointsLeaderboardSnapshot
	afterPosition := int64(0)
	if args.Cursor != "" {
		cursor, err := decodePointsLeaderboardCursor(args.Cursor)
		if err != nil {
			return pointsLeaderboardError("Invalid cursor."), nil
		}
		if cursor.Sort != sortBy {
			return pointsLeaderboardError("The cursor belongs to another sort."), nil
		}
		snapshot = model.GetPointsLeaderboardSnapshot(ctx, cursor.SnapshotId)
		if snapshot == nil {
			// pruned: the client reloads from the top
			return &PointsLeaderboardResult{
				Rows:    []PointsLeaderboardRow{},
				Restart: true,
			}, nil
		}
		afterPosition = cursor.Position
	} else {
		snapshot = model.GetLatestPointsLeaderboardSnapshot(ctx)
	}

	result := &PointsLeaderboardResult{
		Rows: []PointsLeaderboardRow{},
	}
	if snapshot == nil {
		// before the first rebuild: an empty board, not an error
		result.Me = pointsLeaderboardMe(ctx, nil, clientSession)
		return result, nil
	}
	result.TotalRanked = snapshot.TotalRanked
	snapshotTime := snapshot.CreateTime
	result.SnapshotTime = &snapshotTime
	result.LatestEpoch = snapshot.LatestEpoch

	// one extra row tells whether there is a next page
	rows := model.GetPointsLeaderboardPage(ctx, snapshot.SnapshotId, sortBy, afterPosition, limit+1)
	hasMore := limit < len(rows)
	if hasMore {
		rows = rows[:limit]
	}
	for _, row := range rows {
		result.Rows = append(result.Rows, pointsLeaderboardRowFromModel(row))
	}
	if hasMore && 0 < len(rows) {
		last := rows[len(rows)-1]
		result.NextCursor = encodePointsLeaderboardCursor(pointsLeaderboardCursor{
			SnapshotId: snapshot.SnapshotId,
			Sort:       sortBy,
			Position:   last.Position(sortBy),
		})
	}
	result.Me = pointsLeaderboardMe(ctx, snapshot, clientSession)
	return result, nil
}

// pointsLeaderboardMe is nil for signed-out callers.
func pointsLeaderboardMe(
	ctx context.Context,
	snapshot *model.PointsLeaderboardSnapshot,
	clientSession *session.ClientSession,
) *PointsLeaderboardMe {
	if clientSession == nil || clientSession.ByJwt == nil {
		return nil
	}
	networkId := clientSession.ByJwt.NetworkId
	settings := model.GetNetworkPointsLeaderboardSettings(ctx, networkId)
	// the network always sees its own name, ranked or not; Anonymous says
	// how everyone else sees it
	me := &PointsLeaderboardMe{
		PointsLeaderboardRow: PointsLeaderboardRow{
			NetworkId:   networkId,
			NetworkName: clientSession.ByJwt.NetworkName,
			EmojiTag:    settings.EmojiTag,
			Anonymous:   !settings.PointsLeaderboardPublic,
		},
		PointsLeaderboardPublic: settings.PointsLeaderboardPublic,
	}
	if snapshot != nil {
		if row := model.GetPointsLeaderboardNetworkRow(ctx, snapshot.SnapshotId, networkId); row != nil {
			me.PointsLeaderboardRow = pointsLeaderboardRowFromModel(row)
			me.NetworkName = row.NetworkName
			me.Anonymous = !row.PointsLeaderboardPublic
			me.Ranked = true
		}
	}
	return me
}

/**
 * Points leaderboard settings
 */

type SetNetworkPointsRankingPublicArgs struct {
	Public bool `json:"public"`
}

type SetNetworkPointsRankingPublicResult struct {
	PointsLeaderboardPublic bool                                `json:"points_leaderboard_public"`
	Error                   *SetNetworkPointsRankingPublicError `json:"error,omitempty"`
}

type SetNetworkPointsRankingPublicError struct {
	Message string `json:"message"`
}

// SetNetworkPointsLeaderboardPublic reveals (or hides) the network's name on
// the points leaderboard; the row is listed either way.
func SetNetworkPointsLeaderboardPublic(
	args *SetNetworkPointsRankingPublicArgs,
	clientSession *session.ClientSession,
) (*SetNetworkPointsRankingPublicResult, error) {
	model.SetNetworkPointsLeaderboardPublic(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.Public)
	return &SetNetworkPointsRankingPublicResult{
		PointsLeaderboardPublic: args.Public,
	}, nil
}

type SetNetworkEmojiTagArgs struct {
	// EmojiTag is 1 to 6 emoji; "" clears the tag.
	EmojiTag string `json:"emoji_tag"`
}

type SetNetworkEmojiTagResult struct {
	EmojiTag string                   `json:"emoji_tag"`
	Error    *SetNetworkEmojiTagError `json:"error,omitempty"`
}

type SetNetworkEmojiTagError struct {
	Message string `json:"message"`
}

// SetNetworkEmojiTag stores the network's emoji tag after the shared
// connect/emoji validation (the sdk runs the same function before sending).
func SetNetworkEmojiTag(
	args *SetNetworkEmojiTagArgs,
	clientSession *session.ClientSession,
) (*SetNetworkEmojiTagResult, error) {
	tag := strings.TrimSpace(args.EmojiTag)
	if tag != "" {
		normalized, _, err := emoji.ValidateTag(tag)
		if err != nil {
			return &SetNetworkEmojiTagResult{
				Error: &SetNetworkEmojiTagError{Message: pointsLeaderboardEmojiTagMessage},
			}, nil
		}
		tag = normalized
	}
	model.SetNetworkEmojiTag(clientSession.Ctx, clientSession.ByJwt.NetworkId, tag)
	return &SetNetworkEmojiTagResult{EmojiTag: tag}, nil
}

/**
 * Rebuild task
 */

type RebuildPointsLeaderboardArgs struct {
}

type RebuildPointsLeaderboardResult struct {
	SnapshotId  server.Id `json:"snapshot_id"`
	TotalRanked int64     `json:"total_ranked"`
	LatestEpoch uint64    `json:"latest_epoch"`
}

const (
	rebuildPointsLeaderboardKey        = "rebuild_points_leaderboard"
	rebuildPointsLeaderboardTriggerKey = "rebuild_points_leaderboard_trigger"
)

// ScheduleRebuildPointsLeaderboard is the hourly fallback, registered at
// startup and re-armed by the post function. RunOnce merges into a pending
// run (keeping the earlier run_at), so re-arming never duplicates.
func ScheduleRebuildPointsLeaderboard(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RebuildPointsLeaderboard,
		&RebuildPointsLeaderboardArgs{},
		clientSession,
		task.RunOnce(rebuildPointsLeaderboardKey),
		task.RunAt(server.NowUtc().Add(pointsLeaderboardRebuildInterval)),
		task.MaxTime(30*time.Minute),
	)
}

// TriggerRebuildPointsLeaderboardInTx asks for a rebuild now: after an epoch
// finalizes or a payout plan commits. It uses its own run-once key so a
// trigger that lands while a rebuild is running still queues one more run
// instead of merging into the row being executed and getting lost.
func TriggerRebuildPointsLeaderboardInTx(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RebuildPointsLeaderboard,
		&RebuildPointsLeaderboardArgs{},
		clientSession,
		task.RunOnce(rebuildPointsLeaderboardTriggerKey),
		task.RunAt(server.NowUtc()),
		task.MaxTime(30*time.Minute),
	)
}

// TriggerRebuildPointsLeaderboard is TriggerRebuildPointsLeaderboardInTx for
// callers that only hold a context.
func TriggerRebuildPointsLeaderboard(ctx context.Context) {
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	server.Tx(ctx, func(tx server.PgTx) {
		TriggerRebuildPointsLeaderboardInTx(clientSession, tx)
	})
}

// RebuildPointsLeaderboard writes a new snapshot from the current points and
// finalized epochs.
func RebuildPointsLeaderboard(
	args *RebuildPointsLeaderboardArgs,
	clientSession *session.ClientSession,
) (*RebuildPointsLeaderboardResult, error) {
	ctx := clientSession.Ctx
	windows := pointsLeaderboardEpochWindows(ctx)
	snapshot, err := model.RebuildPointsLeaderboard(ctx, windows)
	if err != nil {
		return nil, err
	}
	glog.Infof(
		"[points]leaderboard snapshot %s: %d ranked networks, %d finalized epochs, latest epoch %d\n",
		snapshot.SnapshotId,
		snapshot.TotalRanked,
		len(windows),
		snapshot.LatestEpoch,
	)
	return &RebuildPointsLeaderboardResult{
		SnapshotId:  snapshot.SnapshotId,
		TotalRanked: snapshot.TotalRanked,
		LatestEpoch: snapshot.LatestEpoch,
	}, nil
}

func RebuildPointsLeaderboardPost(
	args *RebuildPointsLeaderboardArgs,
	result *RebuildPointsLeaderboardResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRebuildPointsLeaderboard(clientSession, tx)
	return nil
}

// pointsLeaderboardEpochWindowFunc resolves one finalized epoch's window;
// tests replace it so the rebuild never touches the chain.
var pointsLeaderboardEpochWindowFunc = snEpochWindow
var pointsLeaderboardDeploymentKeyFunc = StDeploymentKey

// pointsLeaderboardEpochWindows lists every finalized epoch with its
// wall-clock window.
func pointsLeaderboardEpochWindows(ctx context.Context) []model.PointsEpochWindow {
	deploymentKey, ok := pointsLeaderboardDeploymentKeyFunc()
	if !ok {
		return nil
	}
	rows := model.GetFinalizedStEpochs(ctx, deploymentKey, pointsLeaderboardEpochLimit)
	windows := make([]model.PointsEpochWindow, 0, len(rows))
	for _, row := range rows {
		start, end := pointsLeaderboardEpochWindowFunc(ctx, row)
		if !start.Before(end) {
			glog.Warningf("[points]epoch %d has an empty window [%s, %s); skipping\n", row.Epoch, start, end)
			continue
		}
		windows = append(windows, model.PointsEpochWindow{
			Epoch: row.Epoch,
			Start: start,
			End:   end,
		})
	}
	return windows
}
