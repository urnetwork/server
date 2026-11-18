package work

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The prober retention's window step (model.RemoveProberShardWindows): a probe
// pass's window clients go an hour after its deadline, once every contract it
// funded is resolved, instead of with its generation. One chain, beside the
// generation drop's (remove_prober_shard_generations) so that neither waits
// for the other's budget: each run takes the due passes in the order of their
// creation for a bounded budget and returns where it stopped; Post hands that
// on to the next run, soon while passes remain due, and after the idle
// interval once the step caught up, waits at a pass, is off or dry run, or its
// control is unreadable. A failed run hands its cursor to the retry. It runs
// in the mode of the runtime control `{prober_retention}control`, unless the
// control's `windows` field turns it off.

type RemoveProberShardWindowsArgs struct {
	// where the previous run stopped; the zero value starts at the oldest pass
	Cursor model.ProberShardWindowCursor `json:"cursor"`
}

type RemoveProberShardWindowsResult struct {
	// where this run stopped, handed back by Post as the next run's
	Cursor model.ProberShardWindowCursor `json:"cursor"`
	// the next run waits the idle interval
	Idle bool `json:"idle"`
}

// Starts the chain at the oldest pass. A pending chain keeps its own cursor: a
// run once key merges the schedule but preserves the pending args.
func ScheduleRemoveProberShardWindows(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultProberShardRetentionSettings()
	scheduleRemoveProberShardWindows(
		clientSession,
		tx,
		model.ProberShardWindowCursor{},
		server.NowUtc().Add(settings.ResumeInterval),
		settings,
	)
}

func scheduleRemoveProberShardWindows(
	clientSession *session.ClientSession,
	tx server.PgTx,
	cursor model.ProberShardWindowCursor,
	runAt time.Time,
	settings *model.ProberShardRetentionSettings,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveProberShardWindows,
		&RemoveProberShardWindowsArgs{
			Cursor: cursor,
		},
		clientSession,
		task.RunOnce("remove_prober_shard_windows"),
		task.RunAt(runAt),
		task.MaxTime(settings.TaskMaxTime),
	)
}

func RemoveProberShardWindows(
	removeProberShardWindows *RemoveProberShardWindowsArgs,
	clientSession *session.ClientSession,
) (*RemoveProberShardWindowsResult, error) {
	report, next, err := model.RemoveProberShardWindows(
		clientSession.Ctx,
		server.NowUtc(),
		removeProberShardWindows.Cursor,
		model.DefaultProberShardRetentionSettings(),
	)
	if err != nil {
		// every pass before the failed one was cleared or passed over: the
		// retry takes the failed one up again
		return nil, task.WithRetryArgs(err, &RemoveProberShardWindowsArgs{
			Cursor: next,
		})
	}
	return &RemoveProberShardWindowsResult{
		Cursor: next,
		Idle:   report.Idle(),
	}, nil
}

func RemoveProberShardWindowsPost(
	removeProberShardWindows *RemoveProberShardWindowsArgs,
	removeProberShardWindowsResult *RemoveProberShardWindowsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultProberShardRetentionSettings()
	runAt := server.NowUtc().Add(settings.ResumeInterval)
	if removeProberShardWindowsResult.Idle {
		runAt = server.NowUtc().Add(settings.IdleInterval)
	}
	scheduleRemoveProberShardWindows(
		clientSession,
		tx,
		removeProberShardWindowsResult.Cursor,
		runAt,
		settings,
	)
	return nil
}
