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
//
// Format 2's staged content a run drops, or writes to seg/ only for a hold, leaves its network's stored count by
// negative deltas under producer taskworker (§11.13's staged row, §20.6): a cluster's run reports them through a usage
// batcher of its own over the host's Quota, drained before the run returns, so a run's deltas go out whatever runs
// next. A host with no Quota reports none, the evacuator alarming each, and the nightly count corrects the drift.

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
// gates (EVSB-M10-02), observe while sidecar.yml sets none.
func runEvsEvacuate(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	deps := &evsservice.EvacuatorDeps{
		Log:     evsBudgetLogger{},
		Format2: evsservice.NewFormat2Gate(host),
	}
	if len(args) != 0 {
		usage, finish, err := evsEvacuateUsage(ctx, host)
		if err != nil {
			return err
		}
		defer finish()
		if usage != nil {
			deps.Usage = usage
		}
	}
	result, err := evsservice.RunEvacuation(ctx, host, deps, args)
	if result != nil && glog.V(1) {
		glog.Infof("[evsjobs]evacuation of %s: %d minutes read, %d deleted, %d waiting; %d kept, %d dead, %d fences; oldest batch %s\n",
			result.ClusterId, result.MinuteCount, result.MinuteDeletedCount, result.MinuteWaitingCount,
			result.KeptCount, result.DeadCount, result.FenceCount, result.OldestBatchAge)
	}
	return err
}

// A cluster's run's usage producer: a batcher under producer taskworker over host.Quota, and what sends its open
// window and ends it, which the run defers. nil, with nothing to finish, for a host with no Quota.
func evsEvacuateUsage(ctx context.Context, host *sidecar.ServiceHost) (*sidecar.QuotaUsageBatcher, func(), error) {
	if host.Quota == nil {
		return nil, func() {}, nil
	}
	usage, err := sidecar.NewQuotaUsageBatcher(ctx, host, sidecar.QuotaProducerTaskworker, nil)
	if err != nil {
		return nil, nil, err
	}
	finish := func() {
		// sent now, retried to the horizon, or dropped and counted: never a failed run, since the nightly count
		// corrects what a lost batch leaves
		if err := usage.Drain(ctx); err != nil {
			glog.Errorf("[evsjobs]evacuation's usage batches were not all answered: %v\n", err)
		}
		usage.Close()
	}
	return usage, finish, nil
}
