package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/task"
)

// §21.9's hold pass (ABUSE-M5-14): each PassInterval, an hour, it ends every hold past its expiry, queueing the hold's
// evs sweep pass (evsservice.EvsSweepHoldEndTaskRequest, QD-635) before it deletes the hold/ object and then the
// rows; puts back the object a live row lacks; and alarms on objects with no row (controller.RunAbuseHoldPass). The
// pass runs on this process's hold/ writer, which taskworker installs with its first metadata cluster; without one
// it ends nothing and only counts. A binary that does not link MG5 neither schedules it nor, should an older row
// run, does anything.

type AbuseHoldPassArgs struct {
}

type AbuseHoldPassResult struct {
	EndedCount           int `json:"ended_count"`
	UnrecordedEndedCount int `json:"unrecorded_ended_count"`
	RestoredCount        int `json:"restored_count"`
	RewrittenCount       int `json:"rewritten_count"`
	LiveCount            int `json:"live_count"`
	UnrecordedCount      int `json:"unrecorded_count"`
	UnreadableCount      int `json:"unreadable_count"`
	FailedCount          int `json:"failed_count"`
}

// Where the pass queues each ended hold's sweep: server/task's queue through sidecarhost, which once-keys a request,
// and which taskworker's evs host claims (evs.sweep). Scheduling needs no function bound here. A test replaces it.
var abuseHoldSweepTasks sidecar.Tasks = sidecarhost.NewTasks(nil)

// Schedules the next pass one PassInterval from now, once. A binary that does not link MG5 schedules nothing.
func ScheduleAbuseHoldPass(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg5Linked() {
		return
	}
	settings := model.DefaultAbuseHoldSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseHoldPass,
		&AbuseHoldPassArgs{},
		clientSession,
		task.RunOnce("abuse_hold_pass"),
		task.RunAt(server.NowUtc().Add(settings.PassInterval)),
		task.MaxTime(settings.PassTaskMaxTime),
	)
}

// One pass, bounded by PassMaxTime.
func AbuseHoldPass(
	abuseHoldPass *AbuseHoldPassArgs,
	clientSession *session.ClientSession,
) (*AbuseHoldPassResult, error) {
	store, _ := model.CurrentAbuseHoldStore()
	result := controller.RunAbuseHoldPass(clientSession.Ctx, &controller.AbuseHoldPassDeps{
		Store:    store,
		Tasks:    abuseHoldSweepTasks,
		Now:      server.NowUtc,
		Settings: model.DefaultAbuseHoldSettings(),
	})
	if 0 < result.EndedCount || 0 < result.UnrecordedEndedCount || 0 < result.RestoredCount || 0 < result.RewrittenCount {
		// counts only: no line names a network or a case (owner decisions 93, 184)
		glog.Infof(
			"[abuse]the hold pass ended %d holds and %d unrecorded objects, and put back %d objects and rewrote %d\n",
			result.EndedCount,
			result.UnrecordedEndedCount,
			result.RestoredCount,
			result.RewrittenCount,
		)
	}
	return &AbuseHoldPassResult{
		EndedCount:           result.EndedCount,
		UnrecordedEndedCount: result.UnrecordedEndedCount,
		RestoredCount:        result.RestoredCount,
		RewrittenCount:       result.RewrittenCount,
		LiveCount:            result.LiveCount,
		UnrecordedCount:      result.UnrecordedCount,
		UnreadableCount:      result.UnreadableCount,
		FailedCount:          result.FailedCount,
	}, nil
}

// Schedules the next pass.
func AbuseHoldPassPost(
	abuseHoldPass *AbuseHoldPassArgs,
	abuseHoldPassResult *AbuseHoldPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseHoldPass(clientSession, tx)
	return nil
}

// Replaces where the pass queues sweeps until the returned restore.
func Testing_SetAbuseHoldSweepTasks(tasks sidecar.Tasks) (restore func()) {
	previous := abuseHoldSweepTasks
	abuseHoldSweepTasks = tasks
	return func() {
		abuseHoldSweepTasks = previous
	}
}
