package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21.9's copied_rows (ABUSE-M5-15): each PassInterval, a minute, the copy of every live hold whose copy is not yet
// complete runs, resuming from its own parts (model.RunAbuseHoldCopies), so each case's copy runs once however often
// it is cut short. A placement kicks the pass at once (model.KickAbuseHoldCopy). While a hold's copy waits, the
// ranges its network has rows in from before its placement stay (model.AbuseHoldCopyPending). A binary that does not
// link MG5 neither schedules it nor, should an older row run, does anything.

type AbuseHoldCopyArgs struct {
}

type AbuseHoldCopyResult struct {
	CompletedCount int `json:"completed_count"`
	PendingCount   int `json:"pending_count"`
	FailedCount    int `json:"failed_count"`
}

// Schedules the next pass one PassInterval from now, once, under the key a placement kicks. A binary that does not
// link MG5 schedules nothing.
func ScheduleAbuseHoldCopy(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := model.DefaultAbuseHoldCopySettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseHoldCopy,
		&AbuseHoldCopyArgs{},
		clientSession,
		task.RunOnce(model.AbuseHoldCopyRunOnceKey),
		task.RunAt(server.NowUtc().Add(settings.PassInterval)),
		task.MaxTime(settings.PassTaskMaxTime),
	)
}

// One pass, bounded by PassMaxTime.
func AbuseHoldCopy(
	abuseHoldCopy *AbuseHoldCopyArgs,
	clientSession *session.ClientSession,
) (*AbuseHoldCopyResult, error) {
	result := model.RunAbuseHoldCopies(clientSession.Ctx, server.NowUtc(), model.DefaultAbuseHoldCopySettings())
	if 0 < result.CompletedCount || 0 < result.FailedCount {
		// counts only: no line names a network or a case (owner decisions 93, 184)
		glog.Infof("[abuse]the hold copy completed %d copies; %d wait and %d failed a table this time\n", result.CompletedCount, result.PendingCount, result.FailedCount)
	}
	return &AbuseHoldCopyResult{
		CompletedCount: result.CompletedCount,
		PendingCount:   result.PendingCount,
		FailedCount:    result.FailedCount,
	}, nil
}

// Schedules the next pass.
func AbuseHoldCopyPost(
	abuseHoldCopy *AbuseHoldCopyArgs,
	abuseHoldCopyResult *AbuseHoldCopyResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseHoldCopy(clientSession, tx)
	return nil
}
