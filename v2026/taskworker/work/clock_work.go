package work

import (
	"strconv"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type BackfillClockArgs struct{}

type BackfillClockResult struct {
	TotalTransferByteCount string `json:"total_transfer_byte_count"`
}

func ScheduleBackfillClock(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
) {
	// This is an initialization/restart reconciliation, not a periodic claim
	// that individual Redis increments can be repaired. RunOnce collapses
	// concurrent taskworker startups into one pending scan.
	task.ScheduleTaskInTx(
		tx,
		BackfillClock,
		&BackfillClockArgs{},
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
	result := model.BackfillClock(clientSession.Ctx)
	glog.Infof("[clock]block %d backfill/reconcile: %d bytes\n", model.ClockSinceBlock, result)
	return &BackfillClockResult{
		TotalTransferByteCount: strconv.FormatInt(result, 10),
	}, nil
}
