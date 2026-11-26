package taskworker

// §14.5's compute jobs in taskworker (DESIGN-COMPUTE §14.5, §14.8; CSRV-M12-05), started by one call in run.go beside
// evs's: sidecar/work's design table (work.ComputeJobDesign), each job's logic as its area registered it there
// (work.RegisterJob), run on the process's global Env. For each job whose migration group this binary links:
//
//   - a job of a minute or more is a once-keyed chain compute.<job> in the process's sidecar task registry
//     (sidecarTasks), whose run queues the next an interval on, or the next day's at its UTC time for a daily job,
//     before it works, so a crash loses no link; a run continues where it stopped partway (its args, once-keyed
//     compute.<job>.continue), at once, or after ComputeJobSettings.ContinueRetryAfter when it failed there;
//   - compute_settle is a loop a shard on goroutines of its own, every 2 s, never a chain through pending_task, which
//     at that cadence would write Postgres tens of thousands of times a day a shard for nothing; two workers' loops on
//     one shard split its rows, which the booking claims FOR UPDATE SKIP LOCKED and books under each network's fence
//     (BAL-M12-04), so no row is booked twice and no loop holds a transaction across another's;
//   - compute_gamma_watch is one loop polling the chain's finalized head at its settings' interval;
//   - compute_model_mirror is queued, once a file, by `sidecarctl compute models mirror`, and starts nothing here.
//
// A job no area has registered fails each run with errComputeJobNotBuilt, which waits on server/task's backoff (a
// chain) or the loop's interval, so its schedule stands and it begins working the moment its logic lands. A binary
// that does not link a job's group schedules nothing for it, and a run of it queued elsewhere fails and waits. Every
// job's name is bound in the registry either way, so a queued run never finds no function.
//
// What the jobs run on that sidecar/work cannot reach is installed for them (work.InstallComputeJobDeps): the
// settings, the model mirror's store over the process's blob store, where vault configures one, and a log.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/work"

	"github.com/urnetwork/server"
)

// A run of a job whose area has not registered its logic: retried on the backoff, never dropped.
var errComputeJobNotBuilt = errors.New("the compute job's logic has not landed, so its runs wait")

// A run of a job whose group this binary does not link.
var errComputeJobGroupUnlinked = errors.New("this binary does not link the compute job's tables, so its runs wait")

// What startComputeJobs runs: a test replaces it.
var startComputeJobs = startComputeJobsFromProcess

// Whether this binary links a job's group, "" being none.
func computeJobGroupLinked(group string) bool {
	switch group {
	case "":
		return true
	case work.ComputeJobGroupLedger:
		return server.SidecarMgC1Linked()
	case work.ComputeJobGroupProviders:
		return server.SidecarMgC2Linked()
	case work.ComputeJobGroupEnclaves:
		return server.SidecarMgC3Linked()
	}
	return false
}

// What the compute jobs are scheduled and run with.
type computeJobRuntime struct {
	settings *work.ComputeJobSettings
	// the process's sidecar task registry, which the production worker claims from
	tasks  sidecar.Tasks
	clock  sidecar.Clock
	linked func(group string) bool
	// the job's registered logic, nil for none
	job func(name string) *work.Job
}

// The runtime over the process's registry, real time, the binary's groups and sidecar/work's registry.
func newComputeJobRuntime(settings *work.ComputeJobSettings) *computeJobRuntime {
	if settings == nil {
		settings = work.DefaultComputeJobSettings()
	}
	return &computeJobRuntime{
		settings: settings,
		tasks:    sidecarTasks,
		clock:    sidecar.RealClock{},
		linked:   computeJobGroupLinked,
		job:      work.RegisteredJob,
	}
}

// Binds each compute job's name once a process.
var registerComputeJobsOnce sync.Once

// Installs what the jobs run on until ctx ends, binds their names, and starts their schedules.
func startComputeJobsFromProcess(ctx context.Context, lifetime *sync.WaitGroup) {
	runtime := newComputeJobRuntime(nil)
	deps := &work.ComputeJobDeps{Settings: runtime.settings, Log: evsBudgetLogger{}}
	if store, ok := server.LoadBlobStore(); ok && store != nil {
		deps.ModelMirrorStore = server.NewComputeModelBlobStore(nil)
	}
	removeDeps := work.InstallComputeJobDeps(deps)
	lifetime.Go(func() {
		<-ctx.Done()
		removeDeps()
	})
	registerComputeJobsOnce.Do(runtime.register)
	runtime.start(ctx, lifetime)
}

// Binds every design job's name in the registry.
func (self *computeJobRuntime) register() {
	for _, job := range work.ComputeJobDesign(self.settings) {
		self.tasks.Register(work.ComputeJobTaskName(job.Name), self.taskFunction(job))
	}
}

// Starts each linked job's schedule: its chain's first run, or its loops on lifetime until ctx ends.
func (self *computeJobRuntime) start(ctx context.Context, lifetime *sync.WaitGroup) {
	for _, job := range work.ComputeJobDesign(self.settings) {
		if !self.linked(job.Group) {
			if glog.V(1) {
				glog.Infof("[computejobs]%s waits: this binary does not link %s\n", job.Name, job.Group)
			}
			continue
		}
		switch job.Cadence {
		case work.ComputeJobCadenceEvery, work.ComputeJobCadenceDaily:
			runAt := self.clock.Now()
			if job.Cadence == work.ComputeJobCadenceDaily {
				runAt = computeJobNextDaily(runAt, job.TimeOfDay)
			}
			if err := self.tasks.Schedule(ctx, self.chainRequest(job, runAt)); err != nil {
				glog.Errorf("[computejobs]%s's chain did not start: %v\n", job.Name, err)
			}
		case work.ComputeJobCadenceShardLoop, work.ComputeJobCadenceLoop:
			if job.Interval <= 0 {
				glog.Errorf("[computejobs]%s's loop has no interval, so it does not start\n", job.Name)
				continue
			}
		}
		switch job.Cadence {
		case work.ComputeJobCadenceShardLoop:
			for shard := range job.ShardCount {
				args := work.ComputeSettleJobArgs(int16(shard))
				lifetime.Go(func() {
					self.loop(ctx, job, args)
				})
			}
		case work.ComputeJobCadenceLoop:
			lifetime.Go(func() {
				self.loop(ctx, job, nil)
			})
		}
	}
}

// The chain's run of job at runAt, once-keyed by its task name.
func (self *computeJobRuntime) chainRequest(job *work.ComputeJob, runAt time.Time) *sidecar.TaskRequest {
	taskName := work.ComputeJobTaskName(job.Name)
	return &sidecar.TaskRequest{Name: taskName, OnceKey: taskName, RunAt: runAt, MaxTime: job.MaxTime}
}

// The first time after now that falls timeOfDay past a UTC midnight.
func computeJobNextDaily(now time.Time, timeOfDay time.Duration) time.Time {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(timeOfDay)
	for !next.After(utc) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// One run of job in the task registry: its group linked, the chain's next link queued first where this is a chain's
// run (no args), then the job's logic, or errComputeJobNotBuilt; a run that stops partway queues its continuation.
func (self *computeJobRuntime) taskFunction(job *work.ComputeJob) sidecar.TaskFunction {
	taskName := work.ComputeJobTaskName(job.Name)
	return func(ctx context.Context, args []byte) error {
		if !self.linked(job.Group) {
			return errComputeJobGroupUnlinked
		}
		now := self.clock.Now()
		if len(args) == 0 {
			switch job.Cadence {
			case work.ComputeJobCadenceEvery:
				if err := self.tasks.Schedule(ctx, self.chainRequest(job, now.Add(job.Interval))); err != nil {
					return err
				}
			case work.ComputeJobCadenceDaily:
				if err := self.tasks.Schedule(ctx, self.chainRequest(job, computeJobNextDaily(now, job.TimeOfDay))); err != nil {
					return err
				}
			}
		}
		registered := self.job(job.Name)
		if registered == nil {
			return errComputeJobNotBuilt
		}
		continueArgs, err := registered.Run(ctx, args)
		if continueArgs == nil {
			return err
		}
		runAt := self.clock.Now()
		if err != nil {
			glog.Warningf("[computejobs]%s stopped partway, continuing in %v: %v\n", job.Name, self.settings.ContinueRetryAfter, err)
			runAt = runAt.Add(self.settings.ContinueRetryAfter)
		}
		return self.tasks.Schedule(ctx, &sidecar.TaskRequest{
			Name:    taskName,
			OnceKey: taskName + ".continue",
			Args:    continueArgs,
			RunAt:   runAt,
			MaxTime: job.MaxTime,
		})
	}
}

// job's loop with args until ctx ends: a run, then its interval, a failure said when it begins and when it ends rather
// than at every run.
func (self *computeJobRuntime) loop(ctx context.Context, job *work.ComputeJob, args []byte) {
	failing := false
	for {
		err := func() (returnErr error) {
			server.HandleError(func() {
				registered := self.job(job.Name)
				if registered == nil {
					returnErr = errComputeJobNotBuilt
					return
				}
				_, returnErr = registered.Run(ctx, args)
			}, func(err error) {
				returnErr = err
			})
			return
		}()
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			glog.Warningf("[computejobs]%s %s fails: %v\n", job.Name, args, err)
			failing = true
		case err == nil && failing:
			glog.Infof("[computejobs]%s %s runs again\n", job.Name, args)
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-self.clock.After(job.Interval):
		}
	}
}
