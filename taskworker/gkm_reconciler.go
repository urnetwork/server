package taskworker

// gkm's reconciler in taskworker (KEYS-M2-17; §10.6, §19.4's taskworker row, D-OPS-2, R-KEYS-14): its pending hints,
// retention, staged GC and §2.8's TTLs (gkmservice's reconciler.go and retention.go). Its sweep is a sidecar job, a
// name in the process's sidecar task registry (sidecarTasks) bound once a process to whichever reconciler a runtime
// holds now, so a run any process queues under that name runs here, once-keyed by server/task's RunOnce, and a page
// schedules the next under the same key before it returns.
//
// The reconciler runs on the process's evs host (evs_jobs.go), which a production runtime builds at its start from
// vault's file-clusters-taskworker.yml, so gkm's sweeps spend the same process write budget as evs's jobs (§11.13: one
// budget a process), with Push, pushservice's hint writer, added for gkm. A process with no such file holds no host
// and runs no reconciler, today's production among them.
//
// Not yet here, and failing closed:
//
//   - gkm's authorization state, GkmReconcilerState over §8.9's gate cache and {acl_<networkId>}v: server has no
//     implementation of it (AclGateCache keeps no list of networks, no per-sidecar index and no install callback for
//     it to read), so gkmReconcilerStateSource is nil and a production runtime starts no reconciler; the sweep's name
//     stays bound, so a run queued anyway fails and waits on server/task's backoff rather than find no function;
//   - the hint writer's sidecar_id resolver, which plan-KEYS-hint-addressing gives taskworker over
//     sidecar_identity_binding: without it every pending hint is counted unresolved and lost, which the clients'
//     hourly resync covers (§6.8).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/pushservice"
)

// gkm's authorization state for a host: nil until server builds GkmReconcilerState over §8.9's gate cache (the file
// header), and a test's.
var gkmReconcilerStateSource func(ctx context.Context, host *sidecar.ServiceHost) (gkmservice.GkmReconcilerState, error)

// The reconciler the newest runtime started, nil while none runs.
var gkmReconcilerCurrent atomic.Pointer[gkmservice.GkmReconciler]

// A sweep run in a process that runs no reconciler: retried on server/task's backoff, never run without one.
var errNoGkmReconciler = errors.New("this taskworker runs no gkm reconciler, so gkm's sweep waits")

// Binds the sweep's name once a process; each run reads the reconciler current at its start.
var registerGkmReconcilerJobsOnce sync.Once

// What startGkmReconciler runs: a test replaces it.
var startGkmReconciler = startGkmReconcilerFromHost

// Binds gkm's sweep in tasks, each run on the reconciler current gives at its start, or failing with
// errNoGkmReconciler when there is none.
func registerGkmReconcilerJobs(tasks sidecar.Tasks, current func() *gkmservice.GkmReconciler) {
	tasks.Register(gkmservice.GkmReconcilerSweepTaskName, func(ctx context.Context, args []byte) error {
		reconciler := current()
		if reconciler == nil {
			return errNoGkmReconciler
		}
		return reconciler.RunSweepTask(ctx, args)
	})
}

// Binds the sweep's name in the process's registry, then starts the reconciler on the process's evs host, which
// startEvsJobs built just before, once its authorization state exists. Returns once it is started; a process with no
// host or no state starts none and says why.
func startGkmReconcilerFromHost(ctx context.Context, lifetime *sync.WaitGroup) {
	registerGkmReconcilerJobsOnce.Do(func() {
		registerGkmReconcilerJobs(sidecarTasks, gkmReconcilerCurrent.Load)
	})
	host := evsJobHost.Load()
	if host == nil {
		glog.Infof("[gkmreconciler]no file clusters, so gkm's reconciler waits\n")
		return
	}
	if gkmReconcilerStateSource == nil {
		glog.Infof("[gkmreconciler]gkm's reconciler waits for its authorization state, GkmReconcilerState over the gate cache\n")
		return
	}
	state, err := gkmReconcilerStateSource(ctx, host)
	if err != nil {
		glog.Errorf("[gkmreconciler]no authorization state, so gkm's reconciler waits: %v\n", err)
		return
	}
	if _, err := startGkmReconcilerOnHost(ctx, lifetime, host, state, nil); err != nil {
		glog.Errorf("[gkmreconciler]gkm's reconciler did not start: %v\n", err)
	}
}

// Starts a reconciler on a copy of host carrying gkm's hint writer, with push when given and else pushservice's with
// no resolver, makes it current until ctx ends, then closes it, and starts its sweep's chain through host.Tasks. The
// copy shares host's clusters, budget, Redis and registry. A chain that does not start is said, and the next runtime's
// start tries again.
func startGkmReconcilerOnHost(ctx context.Context, lifetime *sync.WaitGroup, host *sidecar.ServiceHost, state gkmservice.GkmReconcilerState, push sidecar.Push) (*gkmservice.GkmReconciler, error) {
	gkmHost := *host
	if push == nil {
		// no resolver: a hint by sidecar id is lost and counted until plan-KEYS-hint-addressing's resolver lands
		writer, err := pushservice.NewHintWriter(host.Redis, nil, nil, host.Metrics)
		if err != nil {
			return nil, err
		}
		push = writer
	}
	gkmHost.Push = push
	reconciler, err := gkmservice.NewGkmReconciler(ctx, &gkmHost, nil, &gkmservice.GkmReconcilerDeps{
		State: state,
		// glog, as the budget loops' own adapter writes it
		Log: evsBudgetLogger{},
	})
	if err != nil {
		return nil, err
	}
	gkmReconcilerCurrent.Store(reconciler)
	lifetime.Go(func() {
		<-ctx.Done()
		gkmReconcilerCurrent.CompareAndSwap(reconciler, nil)
		reconciler.Close()
	})
	if err := reconciler.StartSweep(ctx); err != nil {
		glog.Errorf("[gkmreconciler]the sweep's chain did not start: %v\n", err)
	}
	return reconciler, nil
}
