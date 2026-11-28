package work

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The provider work session heads whose clients have no event left in the
// journal, and the device audit state of devices offline for a day, are
// deleted (model.RemoveExpiredSessionState; owner 2026-11-16, DBDISK-STABILIZE
// S4), at the pace and under the off switch of the brake
// `{session_state_retention}control`. Each run continues both passes from
// where the last stopped; the schedule is the model settings'.

type RemoveExpiredSessionStateArgs struct {
	Cursor model.SessionStateRetentionCursor `json:"cursor"`
}

type RemoveExpiredSessionStateResult struct {
	Cursor model.SessionStateRetentionCursor `json:"cursor"`
	More   bool                              `json:"more"`
}

func ScheduleRemoveExpiredSessionState(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultSessionStateRetentionSettings()
	scheduleRemoveExpiredSessionState(clientSession, tx, &RemoveExpiredSessionStateArgs{}, settings, server.NowUtc().Add(settings.StartDelay))
}

func scheduleRemoveExpiredSessionState(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *RemoveExpiredSessionStateArgs,
	settings *model.SessionStateRetentionSettings,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredSessionState,
		args,
		clientSession,
		task.RunOnce("remove_expired_session_state"),
		task.RunAt(runAt),
		task.MaxTime(settings.TaskMaxTime),
	)
}

func RemoveExpiredSessionState(
	removeExpiredSessionState *RemoveExpiredSessionStateArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredSessionStateResult, error) {
	report, cursor := model.RemoveExpiredSessionState(
		clientSession.Ctx,
		server.NowUtc(),
		removeExpiredSessionState.Cursor,
		model.DefaultSessionStateRetentionSettings(),
	)
	if 0 < report.Head.RemovedCount || 0 < report.DeviceState.RemovedCount {
		glog.Infof(
			"[ssr]removed %d of %d heads examined (ended %s) and %d of %d device states examined (ended %s)\n",
			report.Head.RemovedCount,
			report.Head.ExaminedCount,
			report.Head.Stop,
			report.DeviceState.RemovedCount,
			report.DeviceState.ExaminedCount,
			report.DeviceState.Stop,
		)
	}
	return &RemoveExpiredSessionStateResult{
		Cursor: cursor,
		More:   report.More(),
	}, nil
}

// A run that left work behind continues after the settings' MoreInterval.
func RemoveExpiredSessionStatePost(
	removeExpiredSessionState *RemoveExpiredSessionStateArgs,
	removeExpiredSessionStateResult *RemoveExpiredSessionStateResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultSessionStateRetentionSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if removeExpiredSessionStateResult.More {
		runAt = server.NowUtc().Add(settings.MoreInterval)
	}
	scheduleRemoveExpiredSessionState(
		clientSession,
		tx,
		&RemoveExpiredSessionStateArgs{Cursor: removeExpiredSessionStateResult.Cursor},
		settings,
		runAt,
	)
	return nil
}
