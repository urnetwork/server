package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21.7's standing pass (ABUSE-M5-07): every StandingPassInterval, an hour, each restricted network_abuse_state row
// whose until_time has passed is re-derived from the action history (model.RunAbuseStandingPass), so it goes, or a
// newer kill or halt keeps it: QD-194's daily re-evaluation and §21.10's "only while current". Readers already ignore a
// row past its until_time, so the pass is retention, never correctness. A binary that does not link MG5 neither
// schedules it nor, should an older row run, does anything.

type AbuseStandingPassArgs struct {
}

type AbuseStandingPassResult struct {
	ReevaluatedCount int `json:"reevaluated_count"`
	RemovedCount     int `json:"removed_count"`
	ExtendedCount    int `json:"extended_count"`
}

// Schedules the next pass one StandingPassInterval from now, once. A binary that does not link MG5 schedules nothing.
func ScheduleAbuseStandingPass(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := model.DefaultAbuseSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseStandingPass,
		&AbuseStandingPassArgs{},
		clientSession,
		task.RunOnce("abuse_standing_pass"),
		task.RunAt(server.NowUtc().Add(settings.StandingPassInterval)),
		task.MaxTime(settings.StandingPassTaskMaxTime),
	)
}

// One pass, bounded by StandingPassMaxTime.
func AbuseStandingPass(
	abuseStandingPass *AbuseStandingPassArgs,
	clientSession *session.ClientSession,
) (*AbuseStandingPassResult, error) {
	result := model.RunAbuseStandingPass(clientSession.Ctx, server.NowUtc(), model.DefaultAbuseSettings())
	if 0 < result.RemovedCount || 0 < result.ExtendedCount {
		glog.Infof("[abuse]the standing pass ended %d restrictions and kept %d\n", result.RemovedCount, result.ExtendedCount)
	}
	return &AbuseStandingPassResult{
		ReevaluatedCount: result.ReevaluatedCount,
		RemovedCount:     result.RemovedCount,
		ExtendedCount:    result.ExtendedCount,
	}, nil
}

// Schedules the next pass.
func AbuseStandingPassPost(
	abuseStandingPass *AbuseStandingPassArgs,
	abuseStandingPassResult *AbuseStandingPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseStandingPass(clientSession, tx)
	return nil
}
