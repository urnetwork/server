package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21.7's closure growth (ABUSE-M5-19, QD-328): once a UTC day, late, a network whose closure bytes taken today are
// above zero, whose storage_over is set and whose stored count is past its storage limit gets one case with no
// machine action, and none while such a case is open (model.RunAbuseClosureGrowth). Review may then tighten its
// erasers' allowances and its day's statements (limit_tighten). A binary that does not link MG2 and MG5 neither
// schedules it nor, should an older row run, does anything.

type AbuseClosureGrowthArgs struct {
}

type AbuseClosureGrowthResult struct {
	CheckedCount int `json:"checked_count"`
	OpenedCount  int `json:"opened_count"`
	FailedCount  int `json:"failed_count"`
}

// Schedules the next pass at the next ClosureGrowthPassTime, once. A binary that does not link MG2 and MG5
// schedules nothing.
func ScheduleAbuseClosureGrowth(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
		return
	}
	settings := model.DefaultAbuseSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseClosureGrowth,
		&AbuseClosureGrowthArgs{},
		clientSession,
		task.RunOnce("abuse_closure_growth"),
		task.RunAt(model.NextAbuseClosureGrowthTime(server.NowUtc(), settings)),
		task.MaxTime(settings.ClosureGrowthPassTaskMaxTime),
	)
}

// One pass, bounded by ClosureGrowthPassMaxTime.
func AbuseClosureGrowth(
	abuseClosureGrowth *AbuseClosureGrowthArgs,
	clientSession *session.ClientSession,
) (*AbuseClosureGrowthResult, error) {
	result := model.RunAbuseClosureGrowth(clientSession.Ctx, server.NowUtc(), model.DefaultAbuseSettings())
	if 0 < result.OpenedCount || 0 < result.FailedCount {
		glog.Infof("[abuse]closure growth opened %d cases of %d networks over their storage limit; %d could not be judged\n", result.OpenedCount, result.CheckedCount, result.FailedCount)
	}
	return &AbuseClosureGrowthResult{
		CheckedCount: result.CheckedCount,
		OpenedCount:  result.OpenedCount,
		FailedCount:  result.FailedCount,
	}, nil
}

// Schedules the next pass.
func AbuseClosureGrowthPost(
	abuseClosureGrowth *AbuseClosureGrowthArgs,
	abuseClosureGrowthResult *AbuseClosureGrowthResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseClosureGrowth(clientSession, tx)
	return nil
}
