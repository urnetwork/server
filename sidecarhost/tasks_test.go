package sidecarhost

// ServiceHost.Tasks on server/task. Scheduling and runs need Postgres, so these run on the private test
// store through .stack/server-test.sh.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A worker that claims only sidecar jobs, as taskworker's would beside its own.
func newSidecarTaskWorker(ctx context.Context, tasks *Tasks) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(tasks.Target())
	return worker
}

// The sidecar jobs pending under one job name.
func pendingSidecarJobs(ctx context.Context, name string) []*task.Task {
	pending := []*task.Task{}
	for _, claimed := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if strings.Contains(claimed.RunOnceKey, `"`+name+`"`) {
			pending = append(pending, claimed)
		}
	}
	return pending
}

// Two schedules of one key run once, with the first request's args, at the sooner time.
func TestTasksTwoSchedulesOfOneKeyRunOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		tasks := NewTasks(nil)
		var stateLock sync.Mutex
		runArgs := []string{}
		tasks.Register("sidecarhost-test-job", func(ctx context.Context, args []byte) error {
			stateLock.Lock()
			defer stateLock.Unlock()
			runArgs = append(runArgs, string(args))
			return nil
		})
		worker := newSidecarTaskWorker(ctx, tasks)
		defer worker.Close()

		// the claim admits a run a whole second after its run_at, so both ask two seconds back
		runAt := server.NowUtc().Add(-2 * time.Second)
		for _, args := range []string{"first", "second"} {
			if err := tasks.Schedule(ctx, &sidecar.TaskRequest{
				Name:    "sidecarhost-test-job",
				OnceKey: "repo-1",
				Args:    []byte(args),
				RunAt:   runAt,
			}); err != nil {
				tb.Fatal(err)
			}
		}
		if pending := pendingSidecarJobs(ctx, "sidecarhost-test-job"); len(pending) != 1 {
			tb.Fatalf("pending %d, want one", len(pending))
		}
		finished, retried, postRetried, err := worker.EvalTasks(4)
		if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			tb.Fatalf("ran %v/%v/%v, %v", finished, retried, postRetried, err)
		}
		if finished, _, _, err := worker.EvalTasks(4); err != nil || len(finished) != 0 {
			tb.Fatalf("a second run: %v, %v", finished, err)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if len(runArgs) != 1 || runArgs[0] != "first" {
			tb.Fatalf("runs %q", runArgs)
		}
	})
}

// One OnceKey in two jobs is two requests, and each job runs its own function.
func TestTasksOnceKeysAreAJobsOwn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		tasks := NewTasks(nil)
		var stateLock sync.Mutex
		ran := map[string]int{}
		for _, name := range []string{"sidecarhost-test-a", "sidecarhost-test-b"} {
			tasks.Register(name, func(ctx context.Context, args []byte) error {
				stateLock.Lock()
				defer stateLock.Unlock()
				ran[name] += 1
				return nil
			})
			if err := tasks.Schedule(ctx, &sidecar.TaskRequest{Name: name, OnceKey: "shared", RunAt: server.NowUtc().Add(-2 * time.Second)}); err != nil {
				tb.Fatal(err)
			}
		}
		worker := newSidecarTaskWorker(ctx, tasks)
		defer worker.Close()
		finished, _, _, err := worker.EvalTasks(4)
		if err != nil || len(finished) != 2 {
			tb.Fatalf("ran %v, %v", finished, err)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if ran["sidecarhost-test-a"] != 1 || ran["sidecarhost-test-b"] != 1 {
			tb.Fatalf("ran %v", ran)
		}
	})
}

// A run's error reruns it on server/task's backoff, and a job no function is bound to in the claiming
// process is rescheduled, never dropped.
func TestTasksFailedRunsAreRescheduled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		tasks := NewTasks(nil)
		tasks.Register("sidecarhost-test-failing", func(ctx context.Context, args []byte) error {
			return errors.New("not yet")
		})
		worker := newSidecarTaskWorker(ctx, tasks)
		defer worker.Close()
		for _, name := range []string{"sidecarhost-test-failing", "sidecarhost-test-unbound"} {
			if err := tasks.Schedule(ctx, &sidecar.TaskRequest{Name: name, OnceKey: "k", RunAt: server.NowUtc().Add(-2 * time.Second)}); err != nil {
				tb.Fatal(err)
			}
		}
		finished, retried, _, err := worker.EvalTasks(4)
		if err != nil || len(finished) != 0 || len(retried) != 2 {
			tb.Fatalf("finished %v, retried %v, %v", finished, retried, err)
		}
		for _, name := range []string{"sidecarhost-test-failing", "sidecarhost-test-unbound"} {
			if pending := pendingSidecarJobs(ctx, name); len(pending) != 1 {
				tb.Fatalf("%s: pending %d after a failed run", name, len(pending))
			}
		}
	})
}

// A request needs a job name and a once key, and a name binds one function.
func TestTasksRefuseCallerBugs(t *testing.T) {
	tasks := NewTasks(nil)
	ctx := t.Context()
	for _, request := range []*sidecar.TaskRequest{
		nil,
		{OnceKey: "k"},
		{Name: "job"},
		{Name: "job", OnceKey: "k", MaxTime: -time.Second},
	} {
		if err := tasks.Schedule(ctx, request); err == nil {
			t.Errorf("%+v was scheduled", request)
		}
	}
	tasks.Register("job", func(ctx context.Context, args []byte) error { return nil })
	if message := metricsPanic(func() {
		tasks.Register("job", func(ctx context.Context, args []byte) error { return nil })
	}); message == "" {
		t.Error("a second function for one job was taken")
	}
	if message := metricsPanic(func() { tasks.Register("", func(ctx context.Context, args []byte) error { return nil }) }); message == "" {
		t.Error("a job with no name was taken")
	}
	if message := metricsPanic(func() { tasks.Register("nothing", nil) }); message == "" {
		t.Error("a job with no function was taken")
	}
}

// Every sidecar job is one function to server/task, named for RunSidecarTask.
func TestTasksTargetIsRunSidecarTask(t *testing.T) {
	name := NewTasks(nil).Target().TargetFunctionName()
	if !strings.HasSuffix(name, "/sidecarhost.RunSidecarTask") {
		t.Fatalf("target %q", name)
	}
}
