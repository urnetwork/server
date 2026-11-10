package taskworker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// An authorization state holding no network: a sweep finds nothing to keep or delete on its account.
type gkmReconcilerTestState struct{}

func (gkmReconcilerTestState) Watch(changed func(change *gkmservice.GkmStateChange)) (func(), error) {
	return func() {}, nil
}

func (gkmReconcilerTestState) Networks() []sidecar.Id {
	return nil
}

func (gkmReconcilerTestState) State(ctx context.Context, networkId sidecar.Id) (*gkmservice.GkmNetworkState, error) {
	return nil, errors.New("no network loaded")
}

func (gkmReconcilerTestState) SidecarStanding(ctx context.Context, sidecarId sidecar.Id) (sidecar.Id, bool, bool, error) {
	return sidecar.Id{}, false, false, nil
}

// gkm's sweep runs only on the reconciler a runtime holds: before it and after the runtime's end a run fails and
// waits on the backoff; while one is current its chain starts at the runtime's start, under the sweep's once key, and
// a run sweeps a page and queues the next.
func TestGkmReconcilerSweepRunsOnTheReconcilerTheRuntimeHolds(t *testing.T) {
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler current before the test")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	tasks := fakeHost.Tasks
	registerGkmReconcilerJobs(tasks, gkmReconcilerCurrent.Load)

	// queued before any runtime holds a reconciler: the run fails
	if err := tasks.Schedule(t.Context(), &sidecar.TaskRequest{Name: gkmservice.GkmReconcilerSweepTaskName, OnceKey: "early"}); err != nil {
		t.Fatal(err)
	}
	if ran, err := tasks.RunDue(t.Context()); ran != 1 || err != nil {
		t.Fatalf("ran %d: %v", ran, err)
	}
	if pending := tasks.Pending(); len(pending) != 1 || pending[0].OnceKey != "early" {
		t.Fatalf("with no reconciler the run did not wait for its retry: %v", pending)
	}
	clock.Advance(sidecartest.DefaultFakeTasksSettings().RetryTimeout)

	host := fakeHost.ServiceHost()
	ctx, cancel := context.WithCancel(t.Context())
	var lifetime sync.WaitGroup
	reconciler, err := startGkmReconcilerOnHost(ctx, &lifetime, host, gkmReconcilerTestState{}, fakeHost.Push)
	if err != nil {
		t.Fatal(err)
	}
	if gkmReconcilerCurrent.Load() != reconciler {
		t.Fatal("the runtime's reconciler is not current")
	}
	chains := 0
	for _, request := range tasks.Pending() {
		if request.Name == gkmservice.GkmReconcilerSweepTaskName && request.OnceKey == gkmservice.GkmReconcilerSweepTaskName {
			chains += 1
		}
	}
	if chains != 1 {
		t.Fatalf("the sweep's chain started %d times: %v", chains, tasks.Pending())
	}
	before := tasks.RunCount(gkmservice.GkmReconcilerSweepTaskName)
	if _, err := tasks.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if tasks.RunCount(gkmservice.GkmReconcilerSweepTaskName) <= before {
		t.Fatal("no sweep ran on the current reconciler")
	}
	next := 0
	for _, request := range tasks.Pending() {
		if request.Name == gkmservice.GkmReconcilerSweepTaskName && request.OnceKey == gkmservice.GkmReconcilerSweepTaskName {
			next += 1
		}
	}
	if next != 1 {
		t.Fatalf("a finished page queued %d next runs", next)
	}

	// the runtime's end: the reconciler is no longer current
	cancel()
	lifetime.Wait()
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler current after the runtime's end")
	}
}

// A process with no evs host, or with one and no authorization state to give the reconciler, starts none and keeps
// the sweep's name bound, so a run queued anyway waits on its retry.
func TestGkmReconcilerWaitsWithoutItsHostOrState(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var lifetime sync.WaitGroup
	if evsJobHost.Load() != nil {
		t.Fatal("an evs host held before the test")
	}
	startGkmReconcilerFromHost(ctx, &lifetime)
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler with no host")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil).ServiceHost()
	evsJobHost.Store(host)
	defer evsJobHost.CompareAndSwap(host, nil)
	if gkmReconcilerStateSource != nil {
		t.Fatal("server has an authorization state for gkm's reconciler: this test's premise has moved")
	}
	startGkmReconcilerFromHost(ctx, &lifetime)
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler with no authorization state")
	}
	cancel()
	lifetime.Wait()
}
