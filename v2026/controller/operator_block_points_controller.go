package controller

// The daily operator block points task (model/operator_block_points.go):
// every completed operator block past the provider earning boundary gets the
// points of its traffic, in parallel with the SN pool that pays for it. Owner
// decision 2026-10-28.

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type AwardOperatorBlockPointsArgs struct {
	// The block that was open when the task was first scheduled. Before the
	// first award, the task awards the blocks from it on, so the blocks that
	// completed earlier wait for the backfill and its explicit go
	// (bringyourctl account-points backfill-blocks). Zero, for a run a payout
	// queued before any was scheduled: only the latest due block.
	FromBlock int64 `json:"from_block,omitempty"`
}

type AwardOperatorBlockPointsResult struct {
	AwardedBlocks []int64 `json:"awarded_blocks"`
}

const awardOperatorBlockPointsKey = "award_operator_block_points"

// The daily run. A block closes Sunday 00:00 UTC and its traffic is read after
// model.OperatorBlockPointsSettleTime; the Sunday payout also asks for a run.
const operatorBlockPointsHourUtc = 12

// After a taskworker starts, so a deploy picks up a missed run promptly.
const operatorBlockPointsStartupDelay = 15 * time.Minute

// A block's reads take about an hour on Main; a run that catches up several
// blocks takes longer.
const operatorBlockPointsMaxTime = 12 * time.Hour

func nextOperatorBlockPointsTime(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), operatorBlockPointsHourUtc, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// ScheduleAwardOperatorBlockPoints seeds the daily task at startup. A pending
// run keeps its arguments and runs no later than it was due.
func ScheduleAwardOperatorBlockPoints(clientSession *session.ClientSession, tx server.PgTx) {
	now := server.NowUtc()
	scheduleAwardOperatorBlockPoints(
		clientSession,
		tx,
		&AwardOperatorBlockPointsArgs{
			FromBlock: int64(model.SubnetBlockNumber(now)),
		},
		now.Add(operatorBlockPointsStartupDelay),
	)
}

// TriggerAwardOperatorBlockPointsInTx asks for a run now: a payout run's
// points are due when it runs. The run is its own task, with its own retries.
func TriggerAwardOperatorBlockPointsInTx(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleAwardOperatorBlockPoints(clientSession, tx, &AwardOperatorBlockPointsArgs{}, server.NowUtc())
}

func scheduleAwardOperatorBlockPoints(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *AwardOperatorBlockPointsArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		AwardOperatorBlockPoints,
		args,
		clientSession,
		task.RunOnce(awardOperatorBlockPointsKey),
		task.RunAt(runAt),
		task.MaxTime(operatorBlockPointsMaxTime),
	)
}

// AwardOperatorBlockPoints awards every due operator block the task owns
// (model.AwardDueOperatorBlockPoints) and re-ranks the points leaderboard when
// it awarded any.
func AwardOperatorBlockPoints(
	args *AwardOperatorBlockPointsArgs,
	clientSession *session.ClientSession,
) (*AwardOperatorBlockPointsResult, error) {
	awards, err := model.AwardDueOperatorBlockPoints(
		clientSession.Ctx,
		model.EnvSubsidyConfig(),
		server.NowUtc(),
		args.FromBlock,
	)
	result := &AwardOperatorBlockPointsResult{
		AwardedBlocks: []int64{},
	}
	for _, award := range awards {
		result.AwardedBlocks = append(result.AwardedBlocks, award.Block)
	}
	if 0 < len(awards) {
		// awarded blocks are durable even when a later block failed
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			TriggerRebuildPointsLeaderboardInTx(clientSession, tx)
		})
	}
	if err != nil {
		glog.Errorf("[obp]awarded blocks %v before an error: %s\n", result.AwardedBlocks, err)
		return nil, err
	}
	return result, nil
}

func AwardOperatorBlockPointsPost(
	args *AwardOperatorBlockPointsArgs,
	result *AwardOperatorBlockPointsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleAwardOperatorBlockPoints(clientSession, tx, args, nextOperatorBlockPointsTime(server.NowUtc()))
	return nil
}
