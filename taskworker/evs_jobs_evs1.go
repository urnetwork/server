package taskworker

// EVS1-M2-15's lease sweep and EVS1-M2-20's erasure reconciler among evs's jobs (evs_jobs.go): each a sidecar job on
// the process's evs host.
//
//   - evs.lease_sweep: daily, every repo's expired leases dropped from {evsl_<repo_id>} (§11.10's renew row), its
//     next run scheduled under its own key first;
//   - evs.erasure_finish: queued by evs when an ERASE's slot rewrites did not finish before its answer, a minute on,
//     its args the repo id; it finishes every committed ERASE in the repo's erasure ref's window (§11.12's push row).
//
// Both are idempotent: a sweep drops only what has lapsed, and a rewrite drops only keys a slot still holds.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

// The sweep's name and interval.
const (
	evsLeaseSweepTaskName = "evs.lease_sweep"
	evsLeaseSweepInterval = 24 * time.Hour
)

func init() {
	evsJobs = append(evsJobs,
		&evsJob{name: evsLeaseSweepTaskName, run: runEvsLeaseSweep, first: firstEvsLeaseSweep},
		&evsJob{name: evsservice.EvsErasureFinishTaskName, run: runEvsErasureFinish},
	)
}

// The sweep's chain, from the runtime's start.
func firstEvsLeaseSweep(host *sidecar.ServiceHost) *sidecar.TaskRequest {
	return &sidecar.TaskRequest{OnceKey: evsLeaseSweepTaskName, RunAt: host.ClockOrReal().Now()}
}

// One sweep, its next run scheduled first.
func runEvsLeaseSweep(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	now := host.ClockOrReal().Now()
	if err := host.Tasks.Schedule(ctx, &sidecar.TaskRequest{
		Name: evsLeaseSweepTaskName, OnceKey: evsLeaseSweepTaskName, RunAt: now.Add(evsLeaseSweepInterval),
	}); err != nil {
		return err
	}
	removed, err := evsservice.SweepAllLeases(ctx, host, now)
	if err != nil {
		return err
	}
	glog.V(1).Infof("[evsjobs]lease sweep dropped %d leases\n", removed)
	return nil
}

// One repo's ERASEs finished. Args that name no repo are dropped, said once: a retry could never succeed.
func runEvsErasureFinish(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	repoId, err := sidecar.IdFromBytes(args)
	if err != nil {
		glog.Errorf("[evsjobs]%s with args that name no repo: dropped\n", evsservice.EvsErasureFinishTaskName)
		return nil
	}
	_, err = evsservice.FinishErasures(ctx, host, repoId)
	return err
}
