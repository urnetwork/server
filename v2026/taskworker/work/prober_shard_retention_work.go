package work

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Prober data retention by shard generation
// (model.RemoveProberShardGenerations): the newest generations of probe
// passes are kept, eight UTC days by default, and each older one is dropped as
// a unit. One chain: each run drops due passes for a bounded budget; Post
// schedules the next run soon while passes remain, and after the idle interval
// once none is due, the drop is off or dry run, or its control unreadable. Idle
// until the runtime control `{prober_retention}control` sets a mode.

type RemoveProberShardGenerationsArgs struct {
}

type RemoveProberShardGenerationsResult struct {
	// the next run waits the idle interval (proberShardRetentionRunIdle)
	Idle bool `json:"idle"`
}

// Starts the chain. A pending chain keeps its schedule: a run once key merges.
func ScheduleRemoveProberShardGenerations(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultProberShardRetentionSettings()
	scheduleRemoveProberShardGenerations(clientSession, tx, server.NowUtc().Add(settings.IdleInterval), settings)
}

func scheduleRemoveProberShardGenerations(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
	settings *model.ProberShardRetentionSettings,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveProberShardGenerations,
		&RemoveProberShardGenerationsArgs{},
		clientSession,
		task.RunOnce("remove_prober_shard_generations"),
		task.RunAt(runAt),
		task.MaxTime(settings.TaskMaxTime),
	)
}

func RemoveProberShardGenerations(
	removeProberShardGenerations *RemoveProberShardGenerationsArgs,
	clientSession *session.ClientSession,
) (*RemoveProberShardGenerationsResult, error) {
	report, err := model.RemoveProberShardGenerations(
		clientSession.Ctx,
		server.NowUtc(),
		model.DefaultProberShardRetentionSettings(),
	)
	if err != nil {
		// every pass before the failed statement finished: the retry resumes
		// with the oldest pass still due
		return nil, err
	}
	return &RemoveProberShardGenerationsResult{
		Idle: proberShardRetentionRunIdle(report),
	}, nil
}

// The next run waits the idle interval: no pass is due, the drop is off or its
// control unreadable, or it only dry runs, which changes nothing and would
// otherwise reread the same passes at once.
func proberShardRetentionRunIdle(report *model.ProberShardRetentionReport) bool {
	return report.CaughtUp || report.Idle() || report.Mode == model.ProberShardRetentionModeDryRun
}

func RemoveProberShardGenerationsPost(
	removeProberShardGenerations *RemoveProberShardGenerationsArgs,
	removeProberShardGenerationsResult *RemoveProberShardGenerationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultProberShardRetentionSettings()
	runAt := server.NowUtc().Add(settings.ResumeInterval)
	if removeProberShardGenerationsResult.Idle {
		runAt = server.NowUtc().Add(settings.IdleInterval)
	}
	scheduleRemoveProberShardGenerations(clientSession, tx, runAt, settings)
	return nil
}
