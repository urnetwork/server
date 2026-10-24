// The weekly bloat measurement (server.MeasureDbBloat,
// PARTITION-MAINTENANCE-PLAN.md A4) as one run-once chain. Each run carries its
// week's cursor to the next: a run that finishes its week's work schedules the
// next week's slot, a load deferral retries the same week later, and an
// interruption continues it after a drain wave.
package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type MeasureDbBloatArgs struct {
	Cursor server.DbBloatMeasureCursor `json:"cursor"`
}

type MeasureDbBloatResult struct {
	Cursor  server.DbBloatMeasureCursor `json:"cursor"`
	Stopped server.DbBloatMeasureStop   `json:"stopped,omitempty"`
	Reason  string                      `json:"reason,omitempty"`
}

// Seeds the chain only when it has no task, so a deploy neither moves a
// scheduled run nor resets its cursor: a new chain's first run starts after
// FirstRunDelay, and every later run is scheduled by its predecessor.
func ScheduleMeasureDbBloat(clientSession *session.ClientSession, tx server.PgTx) {
	settings := server.DefaultDbBloatMeasureSettings()
	task.ScheduleTaskInTxIfAbsent(
		tx,
		MeasureDbBloat,
		&MeasureDbBloatArgs{},
		clientSession,
		task.RunOnce("measure_db_bloat"),
		task.RunAt(server.NowUtc().Add(settings.FirstRunDelay)),
		task.MaxTime(settings.MaxRunTime),
	)
}

func scheduleMeasureDbBloat(clientSession *session.ClientSession, tx server.PgTx, args *MeasureDbBloatArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		MeasureDbBloat,
		args,
		clientSession,
		task.RunOnce("measure_db_bloat"),
		task.RunAt(runAt),
		task.MaxTime(server.DefaultDbBloatMeasureSettings().MaxRunTime),
	)
}

// The next task of the chain: the same week shortly after an interruption, the
// same week later after a deferral, and the next week's slot once the week's
// work is done.
func measureDbBloatNextTask(result *MeasureDbBloatResult, now time.Time, settings *server.DbBloatMeasureSettings) (*MeasureDbBloatArgs, time.Time) {
	next := &MeasureDbBloatArgs{Cursor: result.Cursor}
	switch result.Stopped {
	case server.DbBloatMeasureInterrupted:
		return next, now.Add(settings.InterruptedRetryDelay)
	case server.DbBloatMeasureDeferred:
		return next, now.Add(settings.DeferredRetryDelay)
	default:
		return next, settings.NextWeekStart(now)
	}
}

func MeasureDbBloat(measureDbBloat *MeasureDbBloatArgs, clientSession *session.ClientSession) (*MeasureDbBloatResult, error) {
	report, cursor := server.MeasureDbBloat(
		clientSession.Ctx,
		server.NowUtc(),
		measureDbBloat.Cursor,
		server.DefaultDbBloatMeasureSettings(),
	)
	return &MeasureDbBloatResult{
		Cursor:  cursor,
		Stopped: report.Stopped,
		Reason:  report.Reason,
	}, nil
}

func MeasureDbBloatPost(
	measureDbBloat *MeasureDbBloatArgs,
	measureDbBloatResult *MeasureDbBloatResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	args, runAt := measureDbBloatNextTask(measureDbBloatResult, server.NowUtc(), server.DefaultDbBloatMeasureSettings())
	scheduleMeasureDbBloat(clientSession, tx, args, runAt)
	return nil
}
