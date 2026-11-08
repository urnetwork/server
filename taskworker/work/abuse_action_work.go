package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21's action pass (ABUSE-M5-05): each minute, finishes the actions and lifts
// whose effect or off-site record is owed, after a crash between commit and
// either step or a step that failed (controller.RetryOwedAbuseActions). It does
// nothing in a binary that does not link MG5.

type AbuseActionPassArgs struct {
}

type AbuseActionPassResult struct {
	AppliedCount int `json:"applied_count"`
	MailedCount  int `json:"mailed_count"`
	OwedCount    int `json:"owed_count"`
}

// Schedules the next pass one ActionPassInterval from now, once.
func ScheduleAbuseActionPass(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultAbuseSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseActionPass,
		&AbuseActionPassArgs{},
		clientSession,
		task.RunOnce("abuse_action_pass"),
		task.RunAt(server.NowUtc().Add(settings.ActionPassInterval)),
		task.MaxTime(settings.ActionPassTaskMaxTime),
	)
}

// One pass, bounded by ActionPassMaxTime.
func AbuseActionPass(
	abuseActionPass *AbuseActionPassArgs,
	clientSession *session.ClientSession,
) (*AbuseActionPassResult, error) {
	result := controller.RetryOwedAbuseActions(clientSession.Ctx, server.NowUtc(), model.DefaultAbuseSettings())
	if 0 < result.AppliedCount || 0 < result.MailedCount {
		glog.Infof("[abuse]the action pass applied %d effects and mailed %d records\n", result.AppliedCount, result.MailedCount)
	}
	return &AbuseActionPassResult{
		AppliedCount: result.AppliedCount,
		MailedCount:  result.MailedCount,
		OwedCount:    result.OwedCount,
	}, nil
}

// Schedules the next pass.
func AbuseActionPassPost(
	abuseActionPass *AbuseActionPassArgs,
	abuseActionPassResult *AbuseActionPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseActionPass(clientSession, tx)
	return nil
}
