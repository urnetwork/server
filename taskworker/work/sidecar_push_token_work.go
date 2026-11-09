package work

// §18.7's steps 5 and 6 (SDK-M8-11): taskworker's token pass, every 60 s (model.RunPushTokenPass). It
// reconciles each hash slot whose {tokgen_<n>} sentinel is absent or names another master's run_id 90 s
// after the pass that found it, re-applies the rows changed since 2 minutes before the last completed pass
// began, and deletes rows 24 h after their client's removal. Each run carries the pass's state to the next
// in its args, so found slots and an unfinished sweep survive a restart, and a run that left work behind
// is followed sooner. With sidecar.yml's wake switch off a run only deletes rows past their 24 h, which is
// their ttl. A binary that does not link MG8 neither schedules the chain nor, should an older row run,
// touches anything. A run logs counts and errors, never a token.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The chain's own tunables, beside the pass's (model.SidecarPushTokenPassSettings).
type PushTokenPassWorkSettings struct {
	// Each run's task max time, past the pass's run budget: 5 min.
	MaxTime time.Duration
}

func DefaultPushTokenPassWorkSettings() *PushTokenPassWorkSettings {
	return &PushTokenPassWorkSettings{
		MaxTime: 5 * time.Minute,
	}
}

type PushTokenPassArgs struct {
	// What the last run left; nil for the first.
	State *model.PushTokenPassState `json:"state,omitempty"`
}

type PushTokenPassResult struct {
	State  *model.PushTokenPassState  `json:"state"`
	Report *model.PushTokenPassReport `json:"report"`
}

// Schedules the chain's first run, a run interval from now. A binary that does not link MG8 schedules
// nothing; a queued run keeps its args, so a restart's schedule never clears the state a run carries.
func SchedulePushTokenPass(clientSession *session.ClientSession, tx server.PgTx) {
	schedulePushTokenPass(clientSession, tx, &PushTokenPassArgs{}, server.NowUtc().Add(model.DefaultSidecarPushTokenPassSettings().RunInterval))
}

func schedulePushTokenPass(clientSession *session.ClientSession, tx server.PgTx, args *PushTokenPassArgs, runAt time.Time) {
	if !server.SidecarMg8Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		PushTokenPass,
		args,
		clientSession,
		task.RunOnce("push_token_pass"),
		task.RunAt(runAt),
		task.MaxTime(DefaultPushTokenPassWorkSettings().MaxTime),
	)
}

// One pass on server's Redis.
func PushTokenPass(
	pushTokenPass *PushTokenPassArgs,
	clientSession *session.ClientSession,
) (*PushTokenPassResult, error) {
	if !server.SidecarMg8Linked() {
		return &PushTokenPassResult{State: pushTokenPass.State, Report: &model.PushTokenPassReport{}}, nil
	}
	ctx := clientSession.Ctx
	store, topology := model.NewPushTokenServerRedis(ctx)
	state, report, err := model.RunPushTokenPass(
		ctx,
		pushTokenPass.State,
		server.NowUtc(),
		server.CurrentSidecarConfig().Wake(),
		store,
		topology,
		model.DefaultSidecarPushTokenPassSettings(),
		model.DefaultSidecarPushTokenSettings(),
	)
	if err != nil {
		// the next run tries again from the state this one returns
		glog.Infof("[pt]pass: %s\n", err)
	}
	if 0 < report.FoundSlotCount+report.ReconciledSlotCount+report.DeletedRowCount && glog.V(1) {
		glog.Infof("[pt]pass found %d slots, reconciled %d (%d rows, %d unrowed clients), %d waiting; swept %d rows; deleted %d removed rows\n", report.FoundSlotCount, report.ReconciledSlotCount, report.ReconciledRowCount, report.DeletedClientCount, report.WaitingSlotCount, report.SweptRowCount, report.DeletedRowCount)
	}
	return &PushTokenPassResult{State: state, Report: report}, nil
}

// Schedules the next run with this one's state, sooner when this one left work behind.
func PushTokenPassPost(
	pushTokenPass *PushTokenPassArgs,
	pushTokenPassResult *PushTokenPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultSidecarPushTokenPassSettings()
	runAt := server.NowUtc().Add(settings.RunInterval)
	if pushTokenPassResult.Report != nil && pushTokenPassResult.Report.More {
		runAt = server.NowUtc().Add(settings.RetryInterval)
	}
	schedulePushTokenPass(clientSession, tx, &PushTokenPassArgs{State: pushTokenPassResult.State}, runAt)
	return nil
}
