package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21's mail pass (ABUSE-M9-01): each minute, and at once when smtpedge's halt ladder asks for a suspension
// (model.KickAbuseMailPass), the feedback loops' complaints become cases, SMTP's halts become cases and actions,
// §21.4's rung suspends a network at its third halt in 30 days, and a halt a recheck lifted is lifted as wrong
// (controller.RunAbuseMailPass). The complaint cursor rides the task's args, so a chain resumes where it stood and
// one seeded again keeps a pending chain's cursor (RunOnce keeps the pending row's args). A binary that does not link
// MG5 neither schedules it nor, should an older row run, does anything; without MG9 it takes complaints alone.

type AbuseMailPassArgs struct {
	ComplaintCursor model.AbuseSmtpComplaintCursor `json:"complaint_cursor"`
}

type AbuseMailPassResult struct {
	Next                  model.AbuseSmtpComplaintCursor `json:"next"`
	ComplaintCaseCount    int                            `json:"complaint_case_count"`
	ComplaintRefusedCount int                            `json:"complaint_refused_count"`
	HaltRecordedCount     int                            `json:"halt_recorded_count"`
	SuspendedCount        int                            `json:"suspended_count"`
	RecheckLiftCount      int                            `json:"recheck_lift_count"`
	FailedCount           int                            `json:"failed_count"`
	More                  bool                           `json:"more"`
}

// Seeds the chain at taskworker's start, its first pass one PassInterval from now. A chain already pending keeps its
// cursor.
func ScheduleAbuseMailPass(clientSession *session.ClientSession, tx server.PgTx) {
	settings := controller.DefaultAbuseMailSettings()
	scheduleAbuseMailPass(clientSession, tx, &AbuseMailPassArgs{}, server.NowUtc().Add(settings.PassInterval))
}

func scheduleAbuseMailPass(clientSession *session.ClientSession, tx server.PgTx, args *AbuseMailPassArgs, runAt time.Time) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := controller.DefaultAbuseMailSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseMailPass,
		args,
		clientSession,
		task.RunOnce(model.AbuseMailPassRunOnceKey),
		task.RunAt(runAt),
		task.MaxTime(settings.PassTaskMaxTime),
	)
}

// One pass from args' cursor, bounded by PassMaxTime.
func AbuseMailPass(
	abuseMailPass *AbuseMailPassArgs,
	clientSession *session.ClientSession,
) (*AbuseMailPassResult, error) {
	result := controller.RunAbuseMailPass(clientSession.Ctx, abuseMailPass.ComplaintCursor, server.NowUtc(), controller.DefaultAbuseMailSettings())
	if 0 < result.HaltRecordedCount || 0 < result.SuspendedCount || 0 < result.RecheckLiftCount {
		glog.Infof("[abuse]the mail pass recorded %d halts, suspended %d networks and lifted %d rechecked halts\n", result.HaltRecordedCount, result.SuspendedCount, result.RecheckLiftCount)
	}
	return &AbuseMailPassResult{
		Next:                  result.Next,
		ComplaintCaseCount:    result.ComplaintCaseCount,
		ComplaintRefusedCount: result.ComplaintRefusedCount,
		HaltRecordedCount:     result.HaltRecordedCount,
		SuspendedCount:        result.SuspendedCount,
		RecheckLiftCount:      result.RecheckLiftCount,
		FailedCount:           result.FailedCount,
		More:                  result.More,
	}, nil
}

// Schedules the next pass at the cursor this one reached, sooner when it left work behind or failed.
func AbuseMailPassPost(
	abuseMailPass *AbuseMailPassArgs,
	abuseMailPassResult *AbuseMailPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := controller.DefaultAbuseMailSettings()
	next := abuseMailPass
	runAt := server.NowUtc().Add(settings.PassInterval)
	if abuseMailPassResult != nil {
		next = &AbuseMailPassArgs{ComplaintCursor: abuseMailPassResult.Next}
		if abuseMailPassResult.More {
			runAt = server.NowUtc().Add(settings.RetryInterval)
		}
	}
	scheduleAbuseMailPass(clientSession, tx, next, runAt)
	return nil
}
