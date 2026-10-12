package work

import (
	"context"
	"strconv"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One run reads at most this many bounded chunks of the clock's raw tail, two
// hundred thousand contracts: under two minutes at the slowest daily rate
// observed on Main, well inside the ten-minute task budget.
const backfillClockChunkCount = 100

type BackfillClockArgs struct {
	// The walk of an earlier run that did not finish.
	Progress *model.ClockBackfill `json:"progress,omitempty"`
}

type BackfillClockResult struct {
	// Set once the walk completes and the clock is reconciled.
	TotalTransferByteCount string `json:"total_transfer_byte_count"`
	// The walk to continue when this run did not finish it.
	Progress *model.ClockBackfill `json:"progress,omitempty"`
}

func ScheduleBackfillClock(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
) {
	scheduleBackfillClock(clientSession, tx, &BackfillClockArgs{}, runAt)
}

// This is an initialization/restart reconciliation, not a periodic claim that
// individual Redis increments can be repaired. RunOnce collapses concurrent
// taskworker startups into one pending walk; a startup during a walk keeps
// the walk's checkpoint.
func scheduleBackfillClock(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *BackfillClockArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		BackfillClock,
		args,
		clientSession,
		task.RunOnce("backfill_clock", model.ClockSinceBlock),
		task.RunAt(runAt),
		task.MaxTime(10*time.Minute),
	)
}

func BackfillClock(
	backfillClock *BackfillClockArgs,
	clientSession *session.ClientSession,
) (*BackfillClockResult, error) {
	return backfillClockRun(clientSession.Ctx, backfillClock, backfillClockChunkCount), nil
}

// Each run reads at most chunkCount chunks of the raw tail and hands the rest
// to the post hook. A resumed walk first extends its end to this run, so the
// reconciled candidate is current even when a startup wake replays an earlier
// checkpoint after the walk finished.
func backfillClockRun(
	ctx context.Context,
	backfillClock *BackfillClockArgs,
	chunkCount int,
) *BackfillClockResult {
	var backfill *model.ClockBackfill
	if backfillClock != nil {
		backfill = backfillClock.Progress
	}
	if backfill == nil || !backfill.Tail.TerminalOnly {
		backfill = model.NewClockBackfill(ctx)
	} else {
		backfill.ExtendTo(server.NowUtc())
	}
	if !backfill.Advance(ctx, chunkCount) {
		glog.Infof("[clock]block %d backfill: %d tail contracts read, continuing\n", model.ClockSinceBlock, backfill.Tail.ContractCount)
		return &BackfillClockResult{Progress: backfill}
	}
	result := backfill.Reconcile(ctx)
	glog.Infof("[clock]block %d backfill/reconcile: %d bytes\n", model.ClockSinceBlock, result)
	return &BackfillClockResult{
		TotalTransferByteCount: strconv.FormatInt(result, 10),
	}
}

func BackfillClockPost(
	backfillClock *BackfillClockArgs,
	backfillClockResult *BackfillClockResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if backfillClockResult.Progress != nil {
		scheduleBackfillClock(clientSession, tx, &BackfillClockArgs{Progress: backfillClockResult.Progress}, server.NowUtc())
	}
	return nil
}
