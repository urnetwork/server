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
// a unit: its contracts by one walk of the primary key, then its passes. One
// chain: each run drops due passes and walks for a bounded budget and returns
// where its walk stopped; Post hands that on to the next run, soon while
// passes or a walk remain, and after the idle interval once none is due, the
// drop is off or dry run, or its control unreadable. A failed run hands its
// last committed walk to the retry. Idle until the runtime control
// `{prober_retention}control` sets a mode.

type RemoveProberShardGenerationsArgs struct {
	// where the previous run's walk stopped; the zero value walks the oldest
	// dropped generation that has a pass to drop from its start
	Walk model.ProberShardContractWalk `json:"walk"`
}

type RemoveProberShardGenerationsResult struct {
	// where this run's walk stopped, handed back by Post as the next run's
	Walk model.ProberShardContractWalk `json:"walk"`
	// the next run waits the idle interval (proberShardRetentionRunIdle)
	Idle bool `json:"idle"`
}

// Starts the chain at no walk. A pending chain keeps its own walk: a run once
// key merges the schedule but preserves the pending args.
func ScheduleRemoveProberShardGenerations(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultProberShardRetentionSettings()
	scheduleRemoveProberShardGenerations(
		clientSession,
		tx,
		model.ProberShardContractWalk{},
		server.NowUtc().Add(settings.IdleInterval),
		settings,
	)
}

func scheduleRemoveProberShardGenerations(
	clientSession *session.ClientSession,
	tx server.PgTx,
	walk model.ProberShardContractWalk,
	runAt time.Time,
	settings *model.ProberShardRetentionSettings,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveProberShardGenerations,
		&RemoveProberShardGenerationsArgs{
			Walk: walk,
		},
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
	report, next, err := model.RemoveProberShardGenerations(
		clientSession.Ctx,
		server.NowUtc(),
		removeProberShardGenerations.Walk,
		model.DefaultProberShardRetentionSettings(),
	)
	if err != nil {
		// every pass and walk batch before the failed statement finished: the
		// retry resumes with the oldest pass still due and after the last
		// committed batch
		return nil, task.WithRetryArgs(err, &RemoveProberShardGenerationsArgs{
			Walk: next,
		})
	}
	return &RemoveProberShardGenerationsResult{
		Walk: next,
		Idle: proberShardRetentionRunIdle(report),
	}, nil
}

// The next run waits the idle interval: no pass is due and no generation is
// due a walk, the drop is off or its control unreadable, or it only dry runs,
// which changes nothing and would otherwise reread the same passes and walk
// at once.
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
	scheduleRemoveProberShardGenerations(
		clientSession,
		tx,
		removeProberShardGenerationsResult.Walk,
		runAt,
		settings,
	)
	return nil
}
