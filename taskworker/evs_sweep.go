package taskworker

// EVSB-M10-02's one-off sweep passes among evs's jobs (evs_jobs.go; §11.13's sweep row), a sidecar job on the process's
// evs host. evs.sweep has no chain of its own: a hold's end and the purge's turn-on from observe queue its starts
// (evsservice.EvsSweepHoldEndTaskRequest, evsservice.EvsSweepTurnOnTaskRequest), each start queueing a scan or a pass
// per repo it covers. What queues a start is not yet settled (questions/evsb-m3-02-sweep-pass-triggers.md), so until
// it is nothing does, and what a hold or observe mode kept stays.
//
// A run reads §19.11's purge mode from the host's gates, as evacuation does, so a pass under observe deletes nothing. A
// pass that could not finish returns its error, and server/task's backoff reruns it; what it deleted stays deleted, and
// a rerun counts it absent.

import (
	"context"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

func init() {
	evsJobs = append(evsJobs, &evsJob{name: evsservice.EvsSweepTaskName, run: runEvsSweep})
}

// One run of evs.sweep: a start, a cluster's scan page, or one repo's pass.
func runEvsSweep(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	result, err := evsservice.RunSweepTask(ctx, host, &evsservice.SweepDeps{
		Log:     evsBudgetLogger{},
		Format2: evsservice.NewFormat2Gate(host),
	}, args)
	if result != nil && glog.V(1) {
		if swept := result.Swept; swept != nil {
			glog.Infof("[evsjobs]sweep pass: %d records, %d candidates, %d deleted, %d absent, %d held, %d kept\n",
				swept.RecordCount, swept.CandidateCount, swept.DeletedCount, swept.AbsentCount, swept.HeldCount, swept.KeptCount)
		} else {
			glog.Infof("[evsjobs]sweep: %d runs queued\n", result.QueuedCount)
		}
	}
	return err
}
