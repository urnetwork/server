package work

import (
	"fmt"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Signed key history of a client that is gone is deleted 7 days after it went,
// or at the narrower horizon its brake sets, never sooner than the guard's
// floor (model.RemoveGoneStClientKeyHistory), at the pace and under the off
// switch of its brake, `{client_key_history_retention}control`.
// Each run deletes what is due from where the last stopped, and continues the
// discovery pass of clients gone without a record.

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

// A run whose delete batch or discovery page ran into its bound and rolled
// back fails, so the task reports the failure (until 2026-11-23 such runs
// reported success, and the retention deleted nothing for two days unseen).
// Its retry resumes from the cursor of the batches that committed, after the
// task's ordinary error backoff.
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
	if 0 < report.ExaminedRecordCount || 0 < report.DiscoveredClientCount || report.Bounded() || report.DeletePassRestarted {
		glog.Infof(
			"[ckh]removed the key history of %d clients gone over %s (%d registrations) from %d records read, recorded %d more as gone, deletes ended %s, pass restarted %t, %d records due (at most %d counted), discovery bounded %t\n",
			report.RemovedClientCount,
			report.Horizon,
			report.RemovedHistoryCount,
			report.ExaminedRecordCount,
			report.DiscoveredClientCount,
			report.DeleteStop,
			report.DeletePassRestarted,
			report.DueRecordCount,
			model.DefaultStClientKeyHistoryRetentionSettings().DueProbeLimit,
			report.DiscoverBounded,
		)
	}
	if report.Bounded() {
		err := fmt.Errorf(
			"key history retention: a batch ran into its bound and rolled back (deletes ended %s, discovery bounded %t); %d registrations removed before it",
			report.DeleteStop,
			report.DiscoverBounded,
			report.RemovedHistoryCount,
		)
		return nil, task.WithRetryArgs(err, &RemoveGoneStClientKeyHistoryArgs{Cursor: cursor})
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
