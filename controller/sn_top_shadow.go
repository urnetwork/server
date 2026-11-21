package controller

// The shadow board of rule (a) (sn_top_admission.go, owner decision
// 2026-11-20, "a one-epoch shadow run before the switch"): until the rule's
// first settlement epoch (model.SnTopUnitRuleFirstEpoch) each fold also ranks
// the board under the rule, and GET /sn/top?board=shadow pages it beside the
// live positions, as the live board pages its own: names only for networks
// that turned on points_leaderboard_public, never a network id. GET /sn/top/me
// adds the caller's unit status at the newest fold and its shadow row.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	snTopBoardLive   = "live"
	snTopBoardShadow = "shadow"
)

// The shadow board's snapshot header: its tempo and ranking, with the live
// board's fixed fields.
func snTopShadowSnapshot(board *model.SnTopShadowBoard, now time.Time) *SnTopSnapshot {
	return &SnTopSnapshot{
		Id:              fmt.Sprintf("shadow-%d", board.TempoIndex),
		TempoIndex:      board.TempoIndex,
		ComputedAt:      board.ComputedAt,
		SettlementEpoch: board.SettlementEpoch,
		TotalRanked:     board.TotalRanked,
		HeadSeats:       model.SnTopHeadSeats,
		BindingOpen:     snTopBindingOpen,
		BindingNote:     snTopBindingNote(snTopBindingOpen),
		Stale:           snTopStale(board.ComputedAt, now),
	}
}

// A shadow row on the wire, its network's display settings applied as the live
// board applies them.
func snTopShadowRowOf(row *model.SnTopShadowRow, display *model.SnTopNetworkDisplay, callerNetworkId *server.Id) SnTopRow {
	modelRow := &model.SnTopNetworkRow{
		SnTopNetworkProjection: model.SnTopNetworkProjection{
			NetworkId:      row.NetworkId,
			Position:       row.Position,
			ProjectedScore: row.ProjectedScore,
			RawLastTempo:   row.RawLastTempo,
			Prefixes:       row.Prefixes,
			Label:          row.Label,
			Claimed:        row.Claimed,
		},
	}
	if display != nil {
		modelRow.NetworkName = display.NetworkName
		modelRow.EmojiTag = display.EmojiTag
		modelRow.PointsLeaderboardPublic = display.PointsLeaderboardPublic
		modelRow.ContainsProfanity = display.ContainsProfanity
	}
	out := snTopRowFromModel(modelRow, nil, callerNetworkId)
	livePosition, livePrefixes, withheld := row.LivePosition, row.LivePrefixes, row.WithheldProviders
	out.LivePosition = &livePosition
	out.LivePrefixes = &livePrefixes
	out.WithheldProviders = &withheld
	return out
}

// Pages the shadow board after the cursor's position.
func getSnTopShadow(ctx context.Context, args *SnTopArgs, callerNetworkId *server.Id, now time.Time) (*SnTopResult, error) {
	result := &SnTopResult{
		Rows:           []SnTopRow{},
		Board:          snTopBoardShadow,
		RuleFirstEpoch: model.SnTopUnitRuleFirstEpoch,
	}
	board, err := model.GetSnTopShadowBoard(ctx)
	if err != nil {
		return nil, err
	}
	if board == nil {
		result.Snapshot = snTopEmptySnapshot()
		return result, nil
	}
	result.Snapshot = snTopShadowSnapshot(board, now)
	afterPosition := 0
	if args.Cursor != "" {
		cursor, err := decodeSnTopCursor(args.Cursor)
		if err != nil || cursor.Filter != snTopBoardShadow {
			return nil, errors.New("400 Invalid cursor.")
		}
		afterPosition = cursor.Position
	}
	limit := snTopLimit(args.Limit)
	page := []*model.SnTopShadowRow{}
	var me *model.SnTopShadowRow
	hasNext := false
	for i := range board.Rows {
		row := &board.Rows[i]
		if callerNetworkId != nil && row.NetworkId == *callerNetworkId {
			me = row
		}
		if row.Position <= afterPosition {
			continue
		}
		if len(page) == limit {
			hasNext = true
			continue
		}
		page = append(page, row)
	}
	networkIds := []server.Id{}
	for _, row := range page {
		networkIds = append(networkIds, row.NetworkId)
	}
	if me != nil {
		networkIds = append(networkIds, me.NetworkId)
	}
	displays := model.GetSnTopNetworkDisplays(ctx, networkIds)
	for _, row := range page {
		result.Rows = append(result.Rows, snTopShadowRowOf(row, displays[row.NetworkId], callerNetworkId))
	}
	if me != nil {
		meRow := snTopShadowRowOf(me, displays[me.NetworkId], callerNetworkId)
		result.Me = &meRow
	}
	if hasNext && 0 < len(page) {
		next := encodeSnTopCursor(snTopCursor{Filter: snTopBoardShadow, Position: page[len(page)-1].Position})
		result.Next = &next
	}
	return result, nil
}

// The network's unit status at the newest fold and its shadow row, nil each
// without one. A read failure leaves them out: the rest of the detail stands.
func getSnTopMeUnits(ctx context.Context, networkId server.Id) (units *model.SnTopNetworkUnits, shadow *SnTopRow) {
	server.HandleError(func() {
		var err error
		units, err = model.GetSnTopNetworkUnits(ctx, networkId)
		if err != nil {
			glog.Infof("[sntop]unit status of %s is unreadable: %s\n", networkId, err)
			units = nil
		}
		board, err := model.GetSnTopShadowBoard(ctx)
		if err != nil || board == nil {
			return
		}
		for i := range board.Rows {
			if board.Rows[i].NetworkId == networkId {
				displays := model.GetSnTopNetworkDisplays(ctx, []server.Id{networkId})
				row := snTopShadowRowOf(&board.Rows[i], displays[networkId], &networkId)
				shadow = &row
				return
			}
		}
	}, func(err error) {
		glog.Infof("[sntop]rule (a) detail of %s failed: %s\n", networkId, err)
	})
	return
}
