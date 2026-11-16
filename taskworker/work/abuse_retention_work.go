package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21.10's retention (ABUSE-M5-16): each RetentionPassInterval, an hour, cases with their actions and appeals go 13
// months after the case last closes, and legal requests and the access log at 3 years, a held network's rows skipped
// until its holds end (model.RemoveExpiredAbuseRows). Holds go by AbuseHoldPass and standing by AbuseStandingPass. A
// binary that does not link MG5 neither schedules it nor, should an older row run, does anything.

type AbuseRetentionArgs struct {
}

type AbuseRetentionResult struct {
	CaseCount         int64 `json:"case_count"`
	ActionCount       int64 `json:"action_count"`
	AppealCount       int64 `json:"appeal_count"`
	LegalRequestCount int64 `json:"legal_request_count"`
	AccessLogCount    int64 `json:"access_log_count"`
	More              bool  `json:"more"`
}

// Schedules the next pass one RetentionPassInterval from now, once. A binary that does not link MG5 schedules
// nothing.
func ScheduleAbuseRetention(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := model.DefaultAbuseSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseRetention,
		&AbuseRetentionArgs{},
		clientSession,
		task.RunOnce("abuse_retention"),
		task.RunAt(server.NowUtc().Add(settings.RetentionPassInterval)),
		task.MaxTime(settings.RetentionPassTaskMaxTime),
	)
}

// One pass, bounded by RetentionPassMaxTime.
func AbuseRetention(
	abuseRetention *AbuseRetentionArgs,
	clientSession *session.ClientSession,
) (*AbuseRetentionResult, error) {
	result := model.RemoveExpiredAbuseRows(clientSession.Ctx, server.NowUtc(), model.DefaultAbuseSettings())
	if 0 < result.CaseCount || 0 < result.LegalRequestCount || 0 < result.AccessLogCount {
		glog.Infof(
			"[abuse]retention removed %d cases with %d actions and %d appeals, %d legal requests and %d access-log rows\n",
			result.CaseCount,
			result.ActionCount,
			result.AppealCount,
			result.LegalRequestCount,
			result.AccessLogCount,
		)
	}
	return &AbuseRetentionResult{
		CaseCount:         result.CaseCount,
		ActionCount:       result.ActionCount,
		AppealCount:       result.AppealCount,
		LegalRequestCount: result.LegalRequestCount,
		AccessLogCount:    result.AccessLogCount,
		More:              result.More,
	}, nil
}

// Schedules the next pass.
func AbuseRetentionPost(
	abuseRetention *AbuseRetentionArgs,
	abuseRetentionResult *AbuseRetentionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseRetention(clientSession, tx)
	return nil
}
