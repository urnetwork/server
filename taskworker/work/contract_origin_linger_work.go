package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The lingering origins' side table sweep
// (model.RemoveExpiredContractOriginLinger): rows past their prune time can
// satisfy no origin read, and go in short batches, oldest first. One chain: a
// run that caught up starts again a minute later, one that left due rows soon.

type RemoveExpiredContractOriginLingerArgs struct {
}

type RemoveExpiredContractOriginLingerResult struct {
	// the run left rows past their prune time
	More bool `json:"more"`
}

// Starts the chain. A pending chain keeps its schedule: a run once key merges.
func ScheduleRemoveExpiredContractOriginLinger(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultContractOriginLingerSweepSettings()
	scheduleRemoveExpiredContractOriginLinger(clientSession, tx, server.NowUtc().Add(settings.StartDelay), settings)
}

func scheduleRemoveExpiredContractOriginLinger(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
	settings *model.ContractOriginLingerSweepSettings,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredContractOriginLinger,
		&RemoveExpiredContractOriginLingerArgs{},
		clientSession,
		task.RunOnce("remove_expired_contract_origin_linger"),
		task.RunAt(runAt),
		task.MaxTime(settings.TaskMaxTime),
	)
}

func RemoveExpiredContractOriginLinger(
	removeExpiredContractOriginLinger *RemoveExpiredContractOriginLingerArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredContractOriginLingerResult, error) {
	report, err := model.RemoveExpiredContractOriginLinger(
		clientSession.Ctx,
		model.DefaultContractOriginLingerSweepSettings(),
	)
	if err != nil {
		// every batch before the failed one committed; the retry continues
		return nil, err
	}
	if glog.V(1) {
		glog.Infof("[contract_origin_linger]swept %d rows in %d batches, stop=%s lag=%s\n", report.Deleted, report.Batches, report.Stop, report.Lag)
	}
	return &RemoveExpiredContractOriginLingerResult{
		More: report.More(),
	}, nil
}

// A run that left due rows continues after the settings' MoreInterval.
func RemoveExpiredContractOriginLingerPost(
	removeExpiredContractOriginLinger *RemoveExpiredContractOriginLingerArgs,
	removeExpiredContractOriginLingerResult *RemoveExpiredContractOriginLingerResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultContractOriginLingerSweepSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if removeExpiredContractOriginLingerResult.More {
		runAt = server.NowUtc().Add(settings.MoreInterval)
	}
	scheduleRemoveExpiredContractOriginLinger(clientSession, tx, runAt, settings)
	return nil
}
