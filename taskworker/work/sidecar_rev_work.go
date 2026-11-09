package work

import (
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// taskworker's deletion of §7.8's rev/ objects (§2.8 evs-18's ttl): each goes
// 20 minutes after its last write, past a service token's 15, by a sweep every
// minute (model.SidecarRevStore.Sweep). A process with no first metadata
// cluster deletes nothing and sweeps again at its interval, so an object
// outlives its 20 minutes rather than going early.

type SweepSidecarRevArgs struct {
}

type SweepSidecarRevResult struct {
}

func ScheduleSweepSidecarRev(clientSession *session.ClientSession, tx server.PgTx) {
	settings := model.DefaultSidecarRevSettings()
	task.ScheduleTaskInTx(
		tx,
		SweepSidecarRev,
		&SweepSidecarRevArgs{},
		clientSession,
		task.RunOnce("sweep_sidecar_rev"),
		task.RunAt(server.NowUtc().Add(settings.SweepInterval)),
		task.MaxTime(settings.SweepMaxTime),
	)
}

func SweepSidecarRev(
	sweepSidecarRev *SweepSidecarRevArgs,
	clientSession *session.ClientSession,
) (*SweepSidecarRevResult, error) {
	deletedCount, err := model.SweepSidecarRevocations(clientSession.Ctx)
	if err != nil {
		// the next sweep reads again; nothing is lost by waiting
		if glog.V(1) {
			glog.Infof("[rev]sweep stopped after %d deletions: %s\n", deletedCount, err)
		}
		return &SweepSidecarRevResult{}, nil
	}
	if 0 < deletedCount && glog.V(1) {
		glog.Infof("[rev]deleted %d objects past their 20 minutes\n", deletedCount)
	}
	return &SweepSidecarRevResult{}, nil
}

func SweepSidecarRevPost(
	sweepSidecarRev *SweepSidecarRevArgs,
	sweepSidecarRevResult *SweepSidecarRevResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepSidecarRev(clientSession, tx)
	return nil
}
