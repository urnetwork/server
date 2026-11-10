package taskworker

// EVSB-M2-11's hourly evacuation among evs's jobs (evs_jobs.go; §11.13's when row, owner decision 136), a sidecar
// job on the process's evs host:
//
//   - evs.evacuate with no args is the hourly start: it queues itself an hour on, then one run per metadata cluster
//     the process holds, each once-keyed by its cluster, so a start and a rerun of one cluster coalesce.
//   - evs.evacuate with a cluster's id is that cluster's run (evsservice.Evacuator), which empties each minute
//     directory over 25 h old and queues itself again at once while due minutes remain past one run's bounds.
//
// A run that could not settle everything returns its first error, so server/task's backoff reruns it; what it did
// settle stays settled, and the hourly start comes round regardless.

import (
	"context"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

func init() {
	evsJobs = append(evsJobs, &evsJob{name: evsservice.EvsEvacuateTaskName, run: runEvsEvacuate, first: firstEvsEvacuate})
}

// The hourly start's chain, from the runtime's start.
func firstEvsEvacuate(host *sidecar.ServiceHost) *sidecar.TaskRequest {
	return evsservice.EvsEvacuateStartTaskRequest(host.ClockOrReal().Now())
}

// One run: the hourly start, or one cluster's evacuation. Format 2's sweep reads §19.11's purge mode from the host's
// gates (EVSB-M10-02), observe while sidecar.yml sets none. The staged content a run drops reaches no usage batch
// yet: taskworker hosts no producer of its own, so those deltas alarm and the nightly count corrects them.
func runEvsEvacuate(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	result, err := evsservice.RunEvacuation(ctx, host, &evsservice.EvacuatorDeps{
		Log:     evsBudgetLogger{},
		Format2: evsservice.NewFormat2Gate(host),
	}, args)
	if result != nil && glog.V(1) {
		glog.Infof("[evsjobs]evacuation of %s: %d minutes read, %d deleted, %d waiting; %d kept, %d dead, %d fences; oldest batch %s\n",
			result.ClusterId, result.MinuteCount, result.MinuteDeletedCount, result.MinuteWaitingCount,
			result.KeptCount, result.DeadCount, result.FenceCount, result.OldestBatchAge)
	}
	return err
}
