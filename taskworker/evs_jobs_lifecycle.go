package taskworker

// EVS1-M2-19's shred and reconciler among evs's jobs (evs_jobs.go), each a sidecar job on the process's evs host:
//
//   - evs.shred: queued by evs's DELETE and by the reconciler's walk, its args a repo, the deletion it shreds and the
//     pass (evsservice.RunShred). Its gkm step is gkmservice.ShredGroup, which evs cannot import, so it is passed in
//     here.
//   - evs.reconcile: daily, a walk of every repo/ that applies the projected RepoDeletes and RepoRestores a repo
//     lacks and queues each deleted repo's start and each shredding repo's shred again (evsservice.RunReconcile).
//
// The projection both read is §8.9's state in the process's one gate cache (C-09), which gkm's reconciler start
// builds on the runtime's host (gkm_reconciler.go): evsservice.AclEvsAuthState over it answers each repo's newest
// RepoDelete from the network's AclRepo rows alone, dated by the row's deleted_time_ms, signed by its signer and
// restored by its restored_at, so a start 7 d 10 min after the RepoDelete's entry decides from rows the log may no
// longer hold (QD-743), and the walk applies a RepoDelete whose entry the cache's log no longer keeps, or never held,
// writing repo/'s deleted{deletion_id, time, signer} from the row (QD-798). A network the cache has not loaded
// answers an error, which starts its load and leaves the repo to the job's retry or the next walk. Until a runtime
// holds that cache, the source is nil: the walk applies nothing and only queues, and a deleted repo's 7-day start
// waits, asked again a ShredHoldRecheckInterval on. A --now shred, which needs no projection, runs to its tombstone.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
)

// The shred's and the reconciler's tunables in taskworker (QD-702).
type evsLifecycleJobSettings struct {
	// The reconciler's interval: daily (§11.12's log first row). Positive.
	ReconcileInterval time.Duration
}

// §11.12's values. A fresh value each call.
func defaultEvsLifecycleJobSettings() *evsLifecycleJobSettings {
	return &evsLifecycleJobSettings{
		ReconcileInterval: 24 * time.Hour,
	}
}

// The projection the shred's start and the reconciler read, over the gate cache the newest runtime built; nil until
// one is installed (see the file header).
var evsDeletionSourceCurrent atomic.Pointer[evsservice.AclEvsAuthState]

// The projection current now, a nil interface while none is installed, so a run reads no typed nil.
func evsDeletionSource() evsservice.EvsRepoDeletionSource {
	if source := evsDeletionSourceCurrent.Load(); source != nil {
		return source
	}
	return nil
}

// Makes §8.9's state in gate the projection evs's lifecycle jobs read, and returns what removes it at its runtime's end,
// which leaves a newer runtime's in place.
func installEvsDeletionSource(gate evsservice.EvsAclGate) func() {
	// the deletion facts read no rev/ object, so the state takes no revocation reader
	source := evsservice.NewAclEvsAuthState(gate, nil)
	evsDeletionSourceCurrent.Store(source)
	return func() {
		evsDeletionSourceCurrent.CompareAndSwap(source, nil)
	}
}

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
		Deletions: evsDeletionSource(),
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
		Name: evsservice.EvsReconcileTaskName, OnceKey: evsservice.EvsReconcileTaskName, RunAt: now.Add(defaultEvsLifecycleJobSettings().ReconcileInterval),
	}); err != nil {
		return err
	}
	counts, err := evsservice.RunReconcile(ctx, host, &evsservice.EvsReconcileDeps{Deletions: evsDeletionSource()})
	if err != nil {
		return err
	}
	glog.V(1).Infof("[evsjobs]reconcile read %d repos, applied %d, queued %d, failed %d\n", counts.Repos, counts.Applied, counts.Queued, counts.Failed)
	return nil
}
