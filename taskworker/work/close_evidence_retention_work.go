package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The close evidence row retention (server.RemoveExpiredCloseEvidence) as its
// own chain since 2026-11-23 (B' of DBDISK-RETENTION-REVIEW.md 4.5,
// CONTRACT-PARTITION-CUTOVER-NOW.md decision 7). It ran inside
// maintain_contract_partitions, a third of each run, and from the C1 cutover
// that task gives the contract days its run first. Each run reads the
// evidence brake once for the window (server.WithCloseEvidenceRetentionControl:
// 7 days while the brake is on, 91 otherwise), deletes for up to its budget at
// the brake's rate, read before every batch, and returns its cursor. Post
// hands the cursor to the next run: MaxBatchWait later while the pass has work
// left, 15 minutes later once it is done or the brake stops it.

type RemoveExpiredCloseEvidenceArgs struct {
	// where the previous run stopped; the zero value starts a pass from the
	// first key
	Cursor server.CloseEvidenceRemovalCursor `json:"cursor"`
}

type RemoveExpiredCloseEvidenceResult struct {
	// where this run stopped, handed back by Post as the next run's start
	Cursor server.CloseEvidenceRemovalCursor `json:"cursor"`
	// the pass has work left: the next run follows MaxBatchWait later
	More bool `json:"more"`
}

// A run's work budget, within the task's max time. Each batch is bounded by
// its statement timeout, so a run ends within a batch of its budget.
const removeExpiredCloseEvidenceBudget = 10 * time.Minute

// The next run after one that finished its pass, or that the brake stopped.
const removeExpiredCloseEvidenceIdle = 15 * time.Minute

func ScheduleRemoveExpiredCloseEvidence(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleRemoveExpiredCloseEvidence(clientSession, tx, &RemoveExpiredCloseEvidenceArgs{}, server.NowUtc().Add(1*time.Minute))
}

func scheduleRemoveExpiredCloseEvidence(clientSession *session.ClientSession, tx server.PgTx, args *RemoveExpiredCloseEvidenceArgs, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredCloseEvidence,
		args,
		clientSession,
		task.RunOnce("remove_expired_close_evidence"),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
	)
}

func RemoveExpiredCloseEvidence(
	removeExpiredCloseEvidence *RemoveExpiredCloseEvidenceArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredCloseEvidenceResult, error) {
	ctx := server.WithCloseEvidenceRetentionControl(clientSession.Ctx)
	report, cursor, more := server.RemoveExpiredCloseEvidence(
		ctx,
		server.NowUtc(),
		removeExpiredCloseEvidence.Cursor,
		time.Now().Add(removeExpiredCloseEvidenceBudget),
	)
	if 0 < report.Removed || report.Stop != server.CloseEvidenceRemovalStopDone {
		glog.Infof(
			"[cp]removed %d close evidence rows at a retention of %s (stop %s, full pass %t)\n",
			report.Removed,
			report.Retention,
			report.Stop,
			report.FullPass,
		)
	}
	return &RemoveExpiredCloseEvidenceResult{
		Cursor: cursor,
		More:   more,
	}, nil
}

// A run that left work behind continues MaxBatchWait later, so a slow brake's
// batches hold its rate (server.CloseEvidenceRetentionSettings.batchSizeAt).
func RemoveExpiredCloseEvidencePost(
	removeExpiredCloseEvidence *RemoveExpiredCloseEvidenceArgs,
	removeExpiredCloseEvidenceResult *RemoveExpiredCloseEvidenceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	runAt := server.NowUtc().Add(removeExpiredCloseEvidenceIdle)
	if removeExpiredCloseEvidenceResult.More {
		runAt = server.NowUtc().Add(server.DefaultCloseEvidenceRetentionSettings().MaxBatchWait)
	}
	scheduleRemoveExpiredCloseEvidence(
		clientSession,
		tx,
		&RemoveExpiredCloseEvidenceArgs{Cursor: removeExpiredCloseEvidenceResult.Cursor},
		runAt,
	)
	return nil
}
