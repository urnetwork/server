package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21.8's clock pass (ABUSE-M5-10; FS-49): each minute, counts the open cases
// past their review clock into urnetwork_abuse_review_overdue, a
// business-hours warning and never a page, and pages again every notice whose
// case is still open, under its case's key, so a page lost between a notice's
// commit and the pager is sent within the minute
// (controller.RunAbuseClockPass). The pass writes nothing: a clock never lifts
// an action. A binary that does not link MG5 neither schedules it nor, should
// an older row run, does anything.

type AbuseClockPassArgs struct {
}

type AbuseClockPassResult struct {
	OverdueCount     int64 `json:"overdue_count"`
	NoticePagedCount int   `json:"notice_paged_count"`
	NoticeOwedCount  int   `json:"notice_owed_count"`
}

// Schedules the next pass one ClockPassInterval from now, once. A binary that
// does not link MG5 schedules nothing.
func ScheduleAbuseClockPass(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := model.DefaultAbuseSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseClockPass,
		&AbuseClockPassArgs{},
		clientSession,
		task.RunOnce("abuse_clock_pass"),
		task.RunAt(server.NowUtc().Add(settings.ClockPassInterval)),
		task.MaxTime(settings.ClockPassTaskMaxTime),
	)
}

// One pass, bounded by ClockPassMaxTime.
func AbuseClockPass(
	abuseClockPass *AbuseClockPassArgs,
	clientSession *session.ClientSession,
) (*AbuseClockPassResult, error) {
	settings := model.DefaultAbuseSettings()
	model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
	result := controller.RunAbuseClockPass(clientSession.Ctx, server.NowUtc(), settings)
	if 0 < result.NoticeOwedCount {
		glog.Infof("[abuse]the clock pass left %d notices' pages owed\n", result.NoticeOwedCount)
	}
	return &AbuseClockPassResult{
		OverdueCount:     result.OverdueCount,
		NoticePagedCount: result.NoticePagedCount,
		NoticeOwedCount:  result.NoticeOwedCount,
	}, nil
}

// Schedules the next pass.
func AbuseClockPassPost(
	abuseClockPass *AbuseClockPassArgs,
	abuseClockPassResult *AbuseClockPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseClockPass(clientSession, tx)
	return nil
}
