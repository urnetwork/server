package sidecarhost

// ServiceHost.Tasks on server/task (host_tasks.go; §19.4): an engine's jobs by name, queued in
// pending_task and run by taskworker.
//
// server/task names a job by its Go function, so every sidecar job is one function, RunSidecarTask,
// whose args carry the job's name and its opaque args, and a Tasks dispatches by that name to what
// Register bound. Schedule calls server/task's once-keyed form, so requests of one name and OnceKey
// coalesce while pending, at the sooner RunAt, and a request during a run owns one successor once the
// run ends, its args left as they were (task.RunOnce). A run's error reruns it after server/task's
// backoff.
//
// Scheduling writes Postgres, so a host builds Tasks only for a process that holds a Postgres pool, and
// leaves it nil in evs and gkm, whose jobs run in taskworker and reschedule themselves
// (plan-OPS-small-readings #8). Only a process whose task worker adds Target claims these jobs.
//
// Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

var _ sidecar.Tasks = (*Tasks)(nil)

// What a sidecar job's pending_task row holds: the job's name and its args, which host_tasks.go keeps to
// ids and positions, never a secret or content.
type SidecarTaskArgs struct {
	Name string `json:"name"`
	Args []byte `json:"args,omitempty"`
}

// A sidecar job returns nothing to the queue.
type SidecarTaskResult struct{}

// The tunables.
type TasksSettings struct {
	// The longest Schedule may take when its caller's ctx sets no deadline: 10 s, a write to the
	// queue's table and nothing else.
	ScheduleTimeout time.Duration
}

// The defaults.
func DefaultTasksSettings() *TasksSettings {
	return &TasksSettings{
		ScheduleTimeout: 10 * time.Second,
	}
}

// Jobs by name. NewTasks makes one, and the process's task worker adds its Target.
type Tasks struct {
	settings *TasksSettings
	target   *sidecarTaskTarget

	stateLock sync.Mutex
	functions map[string]sidecar.TaskFunction
}

// A registry with settings, nil being the defaults.
func NewTasks(settings *TasksSettings) *Tasks {
	if settings == nil {
		settings = DefaultTasksSettings()
	}
	tasks := &Tasks{
		settings:  settings,
		functions: map[string]sidecar.TaskFunction{},
	}
	tasks.target = &sidecarTaskTarget{
		tasks: tasks,
		inner: task.NewTaskTarget(RunSidecarTask),
	}
	return tasks
}

// See sidecar.Tasks: a second function for one name, an empty name or a nil function panics.
func (self *Tasks) Register(name string, function sidecar.TaskFunction) {
	if name == "" {
		panic("a sidecar job with no name")
	}
	if function == nil {
		panic(fmt.Sprintf("sidecar job %q with no function", name))
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.functions[name]; ok {
		panic(fmt.Sprintf("a second function for sidecar job %q", name))
	}
	self.functions[name] = function
}

// The function bound to name, nil when none is.
func (self *Tasks) function(name string) sidecar.TaskFunction {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.functions[name]
}

// See sidecar.Tasks: queued durably under the once key (sidecar, name, OnceKey), so one OnceKey in two
// jobs names two requests. A MaxTime under server/task's two minutes runs for two minutes.
func (self *Tasks) Schedule(ctx context.Context, request *sidecar.TaskRequest) (returnErr error) {
	if request == nil {
		return errors.New("sidecarhost tasks: no request")
	}
	if request.Name == "" {
		return errors.New("sidecarhost tasks: a request with no job name")
	}
	if request.OnceKey == "" {
		return fmt.Errorf("sidecarhost tasks: job %q requested with no once key", request.Name)
	}
	if request.MaxTime < 0 {
		return fmt.Errorf("sidecarhost tasks: job %q with a max time of %v", request.Name, request.MaxTime)
	}
	if _, ok := ctx.Deadline(); !ok && 0 < self.settings.ScheduleTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.settings.ScheduleTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	options := []any{task.RunOnce("sidecar", request.Name, request.OnceKey)}
	if !request.RunAt.IsZero() {
		options = append(options, task.RunAt(request.RunAt))
	}
	if 0 < request.MaxTime {
		options = append(options, task.MaxTime(request.MaxTime))
	}
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = recoveredError(recovered)
		}
	}()
	task.ScheduleTask(RunSidecarTask, &SidecarTaskArgs{Name: request.Name, Args: request.Args}, clientSession, options...)
	return nil
}

// What a task worker adds to claim and run this registry's jobs.
func (self *Tasks) Target() task.Target {
	return self.target
}

// The registry a run dispatches to, carried in its context.
type sidecarTasksKey struct{}

// Every sidecar job's one function: it runs the function the job's name is bound to in the registry
// whose Target claimed it, under the job's MaxTime. A name nothing bound fails the run, so a process
// that claims a job it has no function for retries it on server/task's backoff rather than drop it.
func RunSidecarTask(args *SidecarTaskArgs, clientSession *session.ClientSession) (*SidecarTaskResult, error) {
	tasks, _ := clientSession.Ctx.Value(sidecarTasksKey{}).(*Tasks)
	if tasks == nil {
		return nil, errors.New("sidecarhost tasks: run outside a sidecar task target")
	}
	if args == nil || args.Name == "" {
		return nil, errors.New("sidecarhost tasks: a job with no name")
	}
	function := tasks.function(args.Name)
	if function == nil {
		return nil, fmt.Errorf("sidecarhost tasks: no function for job %q in this process", args.Name)
	}
	if err := function(clientSession.Ctx, args.Args); err != nil {
		return nil, err
	}
	return &SidecarTaskResult{}, nil
}

// server/task's target for RunSidecarTask, which puts its registry in each run's context.
type sidecarTaskTarget struct {
	tasks *Tasks
	inner *task.TaskTarget[*SidecarTaskArgs, *SidecarTaskResult]
}

// See task.Target.
func (self *sidecarTaskTarget) TargetFunctionName() string {
	return self.inner.TargetFunctionName()
}

// See task.Target.
func (self *sidecarTaskTarget) AlternateFunctionNames() []string {
	return self.inner.AlternateFunctionNames()
}

// See task.Target.
func (self *sidecarTaskTarget) Run(ctx context.Context, claimed *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	return self.inner.Run(context.WithValue(ctx, sidecarTasksKey{}, self.tasks), claimed)
}

// See task.Target.
func (self *sidecarTaskTarget) RunPost(ctx context.Context, finished *task.FinishedTask, tx server.PgTx) ([]server.PostFunction, error) {
	return self.inner.RunPost(ctx, finished, tx)
}
