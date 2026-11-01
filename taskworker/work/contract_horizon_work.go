package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Contract row retention past the reaper's horizon
// (model.RemoveContractsPastHorizon): the owner's 91 days, or as few as 45
// once the runtime control's horizonDays shortens it. One chain: each run
// reaps from its cursor for a bounded budget and returns where it stopped;
// Post hands that cursor to the next run, soon while the pass has work left,
// and after the idle interval once it reaches the horizon or finds the reaper
// off. A failed batch hands its last committed cursor to the retry. Idle until
// the runtime control `{contract_horizon}control` sets a mode.

type RemoveContractsPastHorizonArgs struct {
	// where the previous run stopped; the zero value starts a pass at the
	// oldest contract
	Cursor model.ContractHorizonCursor `json:"cursor"`
}

type RemoveContractsPastHorizonResult struct {
	// where this run stopped, handed back by Post as the next run's start
	Cursor model.ContractHorizonCursor `json:"cursor"`
	// the next run waits the idle interval (contractHorizonRunIdle)
	Idle bool `json:"idle"`
}

// Starts the chain at a pass's beginning. A pending chain keeps its own cursor:
// a run once key merges the schedule but preserves the pending args.
func ScheduleRemoveContractsPastHorizon(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultContractHorizonSettings()
	scheduleRemoveContractsPastHorizon(
		clientSession,
		tx,
		model.ContractHorizonCursor{},
		server.NowUtc().Add(settings.IdleInterval),
		settings,
	)
}

func scheduleRemoveContractsPastHorizon(
	clientSession *session.ClientSession,
	tx server.PgTx,
	cursor model.ContractHorizonCursor,
	runAt time.Time,
	settings *model.ContractHorizonSettings,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveContractsPastHorizon,
		&RemoveContractsPastHorizonArgs{
			Cursor: cursor,
		},
		clientSession,
		task.RunOnce("remove_contracts_past_horizon"),
		task.RunAt(runAt),
		task.MaxTime(settings.TaskMaxTime),
	)
}

func RemoveContractsPastHorizon(
	removeContractsPastHorizon *RemoveContractsPastHorizonArgs,
	clientSession *session.ClientSession,
) (*RemoveContractsPastHorizonResult, error) {
	report, next, err := model.RemoveContractsPastHorizon(
		clientSession.Ctx,
		server.NowUtc(),
		removeContractsPastHorizon.Cursor,
		model.DefaultContractHorizonSettings(),
	)
	if err != nil {
		// every batch before the failed one committed: resume after them
		return nil, task.WithRetryArgs(err, &RemoveContractsPastHorizonArgs{
			Cursor: next,
		})
	}
	return &RemoveContractsPastHorizonResult{
		Cursor: next,
		Idle:   contractHorizonRunIdle(report),
	}, nil
}

// The next run waits the idle interval: the pass reached the horizon, the
// reaper is off or its control unreadable, or it only dry runs, which never
// advances its cursor and would otherwise reread the same contracts at once.
func contractHorizonRunIdle(report *model.ContractHorizonReport) bool {
	return report.CaughtUp || report.Idle() || report.Mode == model.ContractHorizonModeDryRun
}

func RemoveContractsPastHorizonPost(
	removeContractsPastHorizon *RemoveContractsPastHorizonArgs,
	removeContractsPastHorizonResult *RemoveContractsPastHorizonResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultContractHorizonSettings()
	runAt := server.NowUtc().Add(settings.ResumeInterval)
	if removeContractsPastHorizonResult.Idle {
		runAt = server.NowUtc().Add(settings.IdleInterval)
	}
	scheduleRemoveContractsPastHorizon(
		clientSession,
		tx,
		removeContractsPastHorizonResult.Cursor,
		runAt,
		settings,
	)
	return nil
}
