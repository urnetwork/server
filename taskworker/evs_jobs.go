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
// The storage format the jobs run as is the block's config's (evs_block_config.go; QD-854), read before the host is
// built: 2 with no file, 1 in a rollback's, and a config that does not read builds no host. Each run takes the
// engine settings read for its host, bound to its context, and a job format 2 added does not run at format 1.
//
// The jobs land with their own tasks, each adding its evsJob to evsJobs: EVSB-M2-11's hourly evacuation,
// EVSB-M2-12's nightly pass with EVSB-M2-13's orphan GC and EVSB-M2-14's marker collection, EVSB-M2-26's closing
// job of the nightly count, EVS1-M2-15's lease sweep, EVS1-M2-17's path-epoch copies, EVS1-M2-19's shreds and
// reconciler, EVS1-M2-20's erasure finishing, and v1's drop jobs. Each binds its name as it lands.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
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
	// Whether format 2 added the job, so a format-1 binary holds none of it: in a taskworker at instance format 1
	// its runs fail and wait (evs_block_config.go).
	format2 bool
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
// errNoEvsJobHost when there is none, with the engine settings read for that host bound to its context, or failing
// with errNoEvsJobSettings when none were. A job format 2 added fails with errEvsJobNeedsFormat2 at instance format 1.
func registerEvsJobs(tasks sidecar.Tasks, jobs []*evsJob, current func() *sidecar.ServiceHost) {
	for _, job := range jobs {
		tasks.Register(job.name, func(ctx context.Context, args []byte) error {
			host := current()
			if host == nil {
				return errNoEvsJobHost
			}
			evsSettings, err := evsJobSettingsOfHost(host)
			if err != nil {
				return err
			}
			if job.format2 && evsSettings.InstanceFormat < sidecar.EvsRepoFormat2 {
				return errEvsJobNeedsFormat2
			}
			return job.run(withEvsJobSettings(ctx, evsSettings), host, args)
		})
	}
}

// Binds evs's jobs in the process's registry, reads the block's config, builds the process's evs host from the vault
// file, and starts each repeating job's chain. Returns once they are started; a config that does not read or a vault
// without the file starts nothing, and the jobs' names stay bound so a queued run fails and retries rather than find
// no function.
func startEvsJobsFromVault(ctx context.Context, lifetime *sync.WaitGroup) {
	registerEvsJobsOnce.Do(func() {
		registerEvsJobs(sidecarTasks, evsJobs, evsJobHost.Load)
	})
	// before any store is touched: the format this taskworker was deployed as (evs_block_config.go)
	evsSettings, err := LoadEvsJobSettings()
	if err != nil {
		glog.Errorf("[evsjobs]no instance format, so evs's jobs wait: %v\n", err)
		return
	}
	if evsSettings.InstanceFormat < sidecar.EvsRepoFormat2 {
		// a rollback's taskworker, said at every start so nobody reads what it leaves as a fault
		glog.Warningf("[evsjobs]%s sets instance format %d, a rollback's: evs's jobs keep format 1's rules over format-2 data, and the drop job and the sweeps wait\n", evshost.BlockConfigFileName, evsSettings.InstanceFormat)
	}
	clusters, err := server.LoadFileClusters(ctx, evsBudgetFileClustersService, nil)
	if err != nil {
		glog.Infof("[evsjobs]no file clusters, so evs's jobs wait: %v\n", err)
		return
	}
	// one to a process, ending with the runtime's lifetime (server.ChronyClockCheck)
	clockCheck := server.NewChronyClockCheck(ctx, nil)
	// §20.7's Quota over model, this process holding Postgres (QUOTA-M2-11)
	quota, err := quotahost.NewLocalQuota(ctx, nil, nil, nil)
	if err != nil {
		glog.Errorf("[evsjobs]no quota, so evs's jobs wait: %v\n", err)
		clusters.Close()
		return
	}
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Postgres:   true,
		Tasks:      sidecarTasks,
		Blobs:      clusters,
		ClockCheck: clockCheck,
		Quota:      quota,
	})
	budget, err := evsservice.NewWriteBudget(ctx, host, nil, nil)
	if err != nil {
		glog.Errorf("[evsjobs]no write budget, so evs's jobs wait: %v\n", err)
		quota.Drain(ctx)
		clusters.Close()
		return
	}
	host.Budget = budget
	closeWriters := func() {}
	if firstCluster, err := clusters.FirstCluster(); err == nil {
		closeWriters = installEvsJobWriters(host, firstCluster, evsSettings)
	}
	startEvsJobsOnHost(ctx, lifetime, host, evsSettings, evsJobs, func() {
		closeWriters()
		budget.Close()
		quota.Drain(context.Background())
		clusters.Close()
	})
}

// Installs the first metadata cluster's writers this process runs beside evs's jobs, on firstCluster with host's
// budget: §21.9's hold/ writer for the abuse hold pass (ABUSE-M5-14) and its soft-state source, rev/'s expiry sweep,
// and, while the binary links MG2, the authorization log's sequencer for retention and effects. Every write each
// makes, the hold writer's creates, rewrites and DELETEs and the sweep's and retention's DELETEs among them, is P0
// (QD-084), spent from the budget at its own one of §11.13's cost rows once the store shows it took effect, by the
// table the budget, built with evsservice's default settings, prices with (SetWriteBudget's nil prices). Answers what
// removes them; the hold writer's removal leaves api's in place where all's process installed it since
// (model.InstallAbuseHoldStore; QD-1399).
func installEvsJobWriters(host *sidecar.ServiceHost, firstCluster sidecar.BlobCluster, evsSettings *evsservice.EvsSettings) func() {
	holdStore := model.NewAbuseHoldStore(firstCluster)
	holdStore.SetWriteBudget(host.Budget, nil)
	removeHoldStore := model.InstallAbuseHoldStore(holdStore)
	model.SetAbuseHoldSoftStateSource(model.NewAbuseHoldSoftStateSource(host, evsSettings))
	// The worker's scoped credentials sweep expired revocations. Signing
	// and all revocation writes belong to the issuer.
	revStore := model.NewSidecarRevStore(firstCluster, nil, nil)
	revStore.SetWriteBudget(host.Budget, nil)
	model.SetSidecarRevStore(revStore)
	if server.SidecarMg2Linked() {
		settings := model.DefaultConfigLogSettings()
		settings.QueueTicksAtIssuer = true
		model.SetConfigLogSequencer(controller.NewHostedConfigLogSequencer(host, firstCluster, settings))
	}
	return func() {
		model.SetAbuseHoldSoftStateSource(nil)
		removeHoldStore()
		model.SetSidecarRevStore(nil)
		if server.SidecarMg2Linked() {
			model.SetConfigLogSequencer(nil)
		}
	}
}

// Installs host for evs's jobs until ctx ends, with evsSettings the engine settings its runs take, this code's
// defaults when nil, then removes both and ends what closeHost ends, and starts each of jobs' chains through
// host.Tasks. A chain that does not start is said, and its next start tries again.
func startEvsJobsOnHost(ctx context.Context, lifetime *sync.WaitGroup, host *sidecar.ServiceHost, evsSettings *evsservice.EvsSettings, jobs []*evsJob, closeHost func()) {
	if evsSettings == nil {
		evsSettings = evsservice.DefaultEvsSettings()
	}
	// the settings first and removed last, so no run finds the host without them
	hostSettings := &evsJobHostSettings{host: host, evsSettings: evsSettings}
	evsJobHostSettingsCurrent.Store(hostSettings)
	evsJobHost.Store(host)
	lifetime.Go(func() {
		<-ctx.Done()
		evsJobHost.CompareAndSwap(host, nil)
		evsJobHostSettingsCurrent.CompareAndSwap(hostSettings, nil)
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
