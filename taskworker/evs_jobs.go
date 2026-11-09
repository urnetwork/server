package taskworker

// evs's jobs in taskworker (EVS1-M2-21; §11.20's services row, §19.4's taskworker row, host_tasks.go): GC, hourly
// evacuation and packing, the nightly pass, path-epoch copies, shreds and reconciliation, and from v1 §11.13's drop
// jobs. Each is a sidecar job: a name in the process's sidecar task registry (sidecarTasks), whose one target the
// production worker adds, so a run any process queues under that name runs here. A job that repeats starts its
// chain at each start, once-keyed so starts coalesce, and schedules its next run under its own key before it
// returns.
//
// Every job runs on the process's evs host, which a production runtime builds at its start from vault's
// file-clusters-taskworker.yml: the clusters, chronyd's clock check, under which no fence is raised and nothing
// deletes while it is unsure (§19.6), the process's write budget, through which every P2 write spends (§11.13),
// Redis, Metrics, the gates, and the registry as ServiceHost.Tasks. A process with no such file builds none, today's
// production among them, and a job run there fails and waits on server/task's backoff: no job runs without its
// clock check and its budget. The host ends with the runtime's lifetime, after the drain has cancelled the runs.
//
// The jobs land with their own tasks, each adding one evsJob to evsJobs: EVSB-M2-11's hourly evacuation,
// EVSB-M2-12's nightly pass with EVSB-M2-13's orphan GC and EVSB-M2-14's marker collection, EVS1-M2-15's lease
// sweep, EVS1-M2-17's path-epoch copies, EVS1-M2-19's shreds and reconciler, EVS1-M2-20's erasure finishing, and
// v1's drop jobs. None of them has landed, so the registry binds no evs name yet.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// The sidecar engines' jobs, by name (host_tasks.go): one registry a process, whose target the production worker
// adds. Each engine's part of taskworker binds its names once, before the worker claims; a run of a name nothing
// bound fails and retries (sidecarhost.RunSidecarTask).
var sidecarTasks = sidecarhost.NewTasks(nil)

// One of evs's jobs.
type evsJob struct {
	// Its name in the registry, evs.<job>, which every request for it names.
	name string
	// One run on the process's evs host. It may run again after a crash or an error, so it is idempotent and
	// resumes from durable state, never from args alone (host_tasks.go).
	run func(ctx context.Context, host *sidecar.ServiceHost, args []byte) error
	// For a job that repeats, the request that starts its chain at each start, given the host; its Name is the
	// job's. nil for a job another job or a route queues.
	first func(host *sidecar.ServiceHost) *sidecar.TaskRequest
}

// evs's jobs, each added by its own task (see the file header).
var evsJobs = []*evsJob{}

// The host evs's jobs run on while a runtime holds one.
var evsJobHost atomic.Pointer[sidecar.ServiceHost]

// A run in a process that holds no evs host: retried on server/task's backoff, never run without one.
var errNoEvsJobHost = errors.New("this taskworker holds no file clusters, so evs's jobs wait")

// Binds each evs job's name once a process; each run reads the host the newest runtime built.
var registerEvsJobsOnce sync.Once

// What startEvsJobs runs: a test replaces it.
var startEvsJobs = startEvsJobsFromVault

// Binds every job of jobs to its name in tasks, each run on the host current gives at its start, or failing with
// errNoEvsJobHost when there is none.
func registerEvsJobs(tasks sidecar.Tasks, jobs []*evsJob, current func() *sidecar.ServiceHost) {
	for _, job := range jobs {
		tasks.Register(job.name, func(ctx context.Context, args []byte) error {
			host := current()
			if host == nil {
				return errNoEvsJobHost
			}
			return job.run(ctx, host, args)
		})
	}
}

// Binds evs's jobs in the process's registry, builds the process's evs host from the vault file, and starts each
// repeating job's chain. Returns once they are started; a vault without the file starts nothing, and the jobs'
// names stay bound so a queued run fails and retries rather than find no function.
func startEvsJobsFromVault(ctx context.Context, lifetime *sync.WaitGroup) {
	registerEvsJobsOnce.Do(func() {
		registerEvsJobs(sidecarTasks, evsJobs, evsJobHost.Load)
	})
	clusters, err := server.LoadFileClusters(ctx, evsBudgetFileClustersService, nil)
	if err != nil {
		glog.Infof("[evsjobs]no file clusters, so evs's jobs wait: %v\n", err)
		return
	}
	// one to a process, ending with the runtime's lifetime (server.ChronyClockCheck)
	clockCheck := server.NewChronyClockCheck(ctx, nil)
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Postgres:   true,
		Tasks:      sidecarTasks,
		Blobs:      clusters,
		ClockCheck: clockCheck,
	})
	budget, err := evsservice.NewWriteBudget(ctx, host, nil, nil)
	if err != nil {
		glog.Errorf("[evsjobs]no write budget, so evs's jobs wait: %v\n", err)
		clusters.Close()
		return
	}
	host.Budget = budget
	startEvsJobsOnHost(ctx, lifetime, host, evsJobs, func() {
		budget.Close()
		clusters.Close()
	})
}

// Installs host for evs's jobs until ctx ends, then removes it and ends what closeHost ends, and starts each of
// jobs' chains through host.Tasks. A chain that does not start is said, and its next start tries again.
func startEvsJobsOnHost(ctx context.Context, lifetime *sync.WaitGroup, host *sidecar.ServiceHost, jobs []*evsJob, closeHost func()) {
	evsJobHost.Store(host)
	lifetime.Go(func() {
		<-ctx.Done()
		evsJobHost.CompareAndSwap(host, nil)
		closeHost()
	})
	for _, job := range jobs {
		if job.first == nil {
			continue
		}
		request := job.first(host)
		request.Name = job.name
		if err := host.Tasks.Schedule(ctx, request); err != nil {
			glog.Errorf("[evsjobs]%s's chain did not start: %v\n", job.name, err)
		}
	}
}
