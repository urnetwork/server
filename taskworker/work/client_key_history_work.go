package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Signed key history of a client that is gone is deleted 91 days after it
// went (model.RemoveGoneStClientKeyHistory). Each run deletes what is due and
// continues the discovery pass of clients gone without a record.

// bounds a stranded claim; the retention's run budget is well under it
const removeGoneStClientKeyHistoryMaxTime = 30 * time.Minute

type RemoveGoneStClientKeyHistoryArgs struct {
	Cursor model.StClientKeyHistoryRetentionCursor `json:"cursor"`
}

type RemoveGoneStClientKeyHistoryResult struct {
	Cursor model.StClientKeyHistoryRetentionCursor `json:"cursor"`
	More   bool                                    `json:"more"`
}

func ScheduleRemoveGoneStClientKeyHistory(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleRemoveGoneStClientKeyHistory(
		clientSession,
		tx,
		&RemoveGoneStClientKeyHistoryArgs{},
		server.NowUtc().Add(5*time.Minute),
	)
}

func scheduleRemoveGoneStClientKeyHistory(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *RemoveGoneStClientKeyHistoryArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveGoneStClientKeyHistory,
		args,
		clientSession,
		task.RunOnce("remove_gone_st_client_key_history"),
		task.RunAt(runAt),
		task.MaxTime(removeGoneStClientKeyHistoryMaxTime),
	)
}

func RemoveGoneStClientKeyHistory(
	removeGoneStClientKeyHistory *RemoveGoneStClientKeyHistoryArgs,
	clientSession *session.ClientSession,
) (*RemoveGoneStClientKeyHistoryResult, error) {
	report, cursor := model.RemoveGoneStClientKeyHistory(
		clientSession.Ctx,
		server.NowUtc(),
		removeGoneStClientKeyHistory.Cursor,
		model.DefaultStClientKeyHistoryRetentionSettings(),
	)
	if 0 < report.RemovedClientCount || 0 < report.DiscoveredClientCount {
		glog.Infof(
			"[ckh]removed the key history of %d gone clients (%d registrations), recorded %d more as gone\n",
			report.RemovedClientCount,
			report.RemovedHistoryCount,
			report.DiscoveredClientCount,
		)
	}
	return &RemoveGoneStClientKeyHistoryResult{
		Cursor: cursor,
		More:   report.More,
	}, nil
}

// A run that left work behind continues a minute later.
func RemoveGoneStClientKeyHistoryPost(
	removeGoneStClientKeyHistory *RemoveGoneStClientKeyHistoryArgs,
	removeGoneStClientKeyHistoryResult *RemoveGoneStClientKeyHistoryResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	runAt := server.NowUtc().Add(15 * time.Minute)
	if removeGoneStClientKeyHistoryResult.More {
		runAt = server.NowUtc().Add(1 * time.Minute)
	}
	scheduleRemoveGoneStClientKeyHistory(
		clientSession,
		tx,
		&RemoveGoneStClientKeyHistoryArgs{Cursor: removeGoneStClientKeyHistoryResult.Cursor},
		runAt,
	)
	return nil
}
