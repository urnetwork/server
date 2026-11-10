package taskworker

// EVS1-M2-19's shred and reconciler among evs's jobs (evs_jobs.go), each a sidecar job on the process's evs host:
//
//   - evs.shred: queued by evs's DELETE and by the reconciler's walk, its args a repo, the deletion it shreds and the
//     pass (evsservice.RunShred). Its gkm step is gkmservice.ShredGroup, which evs cannot import, so it is passed in
//     here.
//   - evs.reconcile: daily, a walk of every repo/ that applies the projected RepoDeletes and RepoRestores a repo
//     lacks and queues each deleted repo's start and each shredding repo's shred again (evsservice.RunReconcile).
//
// No projection source reaches taskworker yet (ACL's projection reader for §8.8's AclRepo rows), so evsDeletionSource
// is nil: the walk applies nothing and only queues, and a deleted repo's 7-day start waits, asked again daily. A
// --now shred, which needs no projection, runs to its tombstone.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
)

// The reconciler's interval: daily (§11.12's log first row).
const evsReconcileInterval = 24 * time.Hour

// The projection the shred's start and the reconciler read; nil until one reaches taskworker (see the file header).
var evsDeletionSource evsservice.EvsRepoDeletionSource

func init() {
	evsJobs = append(evsJobs,
		&evsJob{name: evsservice.EvsShredTaskName, run: runEvsShred},
		&evsJob{name: evsservice.EvsReconcileTaskName, run: runEvsReconcile, first: firstEvsReconcile},
	)
}

// One shred pass. Args that name no shred are dropped, said once: a retry could never succeed.
func runEvsShred(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	if _, _, _, err := evsservice.ParseEvsShredArgs(args); err != nil {
		glog.Errorf("[evsjobs]%s with args that name no shred: dropped\n", evsservice.EvsShredTaskName)
		return nil
	}
	outcome, err := evsservice.RunShred(ctx, host, &evsservice.EvsShredDeps{
		ShredGroup: func(ctx context.Context, cluster sidecar.BlobCluster, repoId sidecar.Id, final bool) error {
			return gkmservice.ShredGroup(ctx, host, cluster, repoId, final, nil)
		},
		Deletions: evsDeletionSource,
	}, args)
	if err != nil {
		return err
	}
	glog.V(1).Infof("[evsjobs]shred pass ended %d\n", outcome)
	return nil
}

// The reconciler's chain, from the runtime's start.
func firstEvsReconcile(host *sidecar.ServiceHost) *sidecar.TaskRequest {
	return &sidecar.TaskRequest{OnceKey: evsservice.EvsReconcileTaskName, RunAt: host.ClockOrReal().Now()}
}

// One walk, its next run scheduled first.
func runEvsReconcile(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	now := host.ClockOrReal().Now()
	if err := host.Tasks.Schedule(ctx, &sidecar.TaskRequest{
		Name: evsservice.EvsReconcileTaskName, OnceKey: evsservice.EvsReconcileTaskName, RunAt: now.Add(evsReconcileInterval),
	}); err != nil {
		return err
	}
	counts, err := evsservice.RunReconcile(ctx, host, &evsservice.EvsReconcileDeps{Deletions: evsDeletionSource})
	if err != nil {
		return err
	}
	glog.V(1).Infof("[evsjobs]reconcile read %d repos, applied %d, queued %d, failed %d\n", counts.Repos, counts.Applied, counts.Queued, counts.Failed)
	return nil
}
