package taskworker

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/sidecarhost"
)

// Only the production registry claims the sidecar engines' jobs, evs's among them; a subnet operator runs none.
func TestSidecarTaskTargetIsProductionOnly(t *testing.T) {
	name := sidecarTasks.Target().TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("sidecar jobs registration profile=%q present=%t", profile, present)
		}
	}
	if name != sidecarhost.NewTasks(nil).Target().TargetFunctionName() {
		t.Fatal("the registry's target is not RunSidecarTask's")
	}
}

// An evs job runs only on the host a runtime holds: before it and after the runtime's end a run fails and waits
// on the backoff, and while it holds one the job's chain starts at the runtime's start and its run gets that host.
func TestEvsJobsRunOnTheHostTheRuntimeHolds(t *testing.T) {
	if evsJobHost.Load() != nil {
		t.Fatal("a host held before the test")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	tasks := sidecartest.NewFakeTasks(clock, sidecartest.NewFaultQueue(), nil)
	var stateLock sync.Mutex
	runHosts := []*sidecar.ServiceHost{}
	jobs := []*evsJob{
		{
			name: "evs.test_chain",
			run: func(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
				stateLock.Lock()
				defer stateLock.Unlock()
				runHosts = append(runHosts, host)
				return nil
			},
			first: func(host *sidecar.ServiceHost) *sidecar.TaskRequest {
				return &sidecar.TaskRequest{OnceKey: "chain", RunAt: host.ClockOrReal().Now()}
			},
		},
		{
			name: "evs.test_queued",
			run: func(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
				return errors.New("never asked")
			},
		},
	}
	registerEvsJobs(tasks, jobs, evsJobHost.Load)

	// queued before any runtime holds a host: the run fails, and nothing runs the job's function
	if err := tasks.Schedule(t.Context(), &sidecar.TaskRequest{Name: "evs.test_chain", OnceKey: "early"}); err != nil {
		t.Fatal(err)
	}
	if ran, err := tasks.RunDue(t.Context()); ran != 1 || err != nil {
		t.Fatalf("ran %d: %v", ran, err)
	}
	if len(runHosts) != 0 || len(tasks.Pending()) != 1 {
		t.Fatalf("with no host: %d runs reached the job, %d pending", len(runHosts), len(tasks.Pending()))
	}

	host := &sidecar.ServiceHost{Tasks: tasks, Clock: clock}
	ctx, cancel := context.WithCancel(t.Context())
	var lifetime sync.WaitGroup
	closed := 0
	startEvsJobsOnHost(ctx, &lifetime, host, jobs, func() { closed += 1 })
	if evsJobHost.Load() != host {
		t.Fatal("the runtime's host is not the jobs'")
	}
	pending := tasks.Pending()
	chains := 0
	for _, request := range pending {
		if request.Name == "evs.test_queued" {
			t.Error("a job with no chain was scheduled at the start")
		}
		if request.Name == "evs.test_chain" && request.OnceKey == "chain" {
			chains += 1
		}
	}
	if chains != 1 {
		t.Fatalf("the chain's first run scheduled %d times: %v", chains, pending)
	}
	if ran, err := tasks.RunDue(t.Context()); ran != 1 || err != nil {
		t.Fatalf("ran %d: %v", ran, err)
	}
	if len(runHosts) != 1 || runHosts[0] != host {
		t.Fatalf("runs got hosts %v", runHosts)
	}

	// the runtime's end: the host leaves, and what it held ends
	cancel()
	lifetime.Wait()
	if evsJobHost.Load() != nil || closed != 1 {
		t.Fatalf("after the end: host %v, closed %d", evsJobHost.Load(), closed)
	}
	if err := tasks.Schedule(t.Context(), &sidecar.TaskRequest{Name: "evs.test_chain", OnceKey: "late"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(sidecartest.DefaultFakeTasksSettings().RetryTimeout)
	if _, err := tasks.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(runHosts) != 1 {
		t.Fatalf("a run after the end reached the job: %d runs", len(runHosts))
	}
}

// A taskworker whose vault holds no file-clusters-taskworker.yml, today's production among them, builds no evs
// host, so evs's jobs wait rather than run without their clock check and budget.
func TestEvsJobsWaitWithoutFileClusters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	ctx, cancel := context.WithCancel(t.Context())
	var lifetime sync.WaitGroup
	startEvsJobsFromVault(ctx, &lifetime)
	if evsJobHost.Load() != nil {
		t.Fatal("an evs host with no file clusters")
	}
	cancel()
	lifetime.Wait()
}
