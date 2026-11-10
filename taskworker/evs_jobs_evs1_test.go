package taskworker

import (
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// evs's registry binds the lease sweep, whose chain starts at each runtime's start and schedules its next day, and
// the erasure reconciler, which drops args naming no repo rather than retry them forever.
func TestEvsJobsBindTheLeaseSweepAndTheErasureReconciler(t *testing.T) {
	names := map[string]*evsJob{}
	for _, job := range evsJobs {
		names[job.name] = job
	}
	sweep, reconciler := names[evsLeaseSweepTaskName], names[evsservice.EvsErasureFinishTaskName]
	if sweep == nil || sweep.first == nil || reconciler == nil || reconciler.first != nil {
		t.Fatalf("jobs %v", names)
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	sidecartest.RegisterEvsTwins(fake.Redis)
	host := fake.ServiceHost()
	if err := sweep.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	scheduled := false
	for _, request := range fake.Tasks.AllRequests() {
		scheduled = scheduled || (request.Name == evsLeaseSweepTaskName && request.RunAt.Equal(clock.Now().Add(evsLeaseSweepInterval)))
	}
	if !scheduled {
		t.Fatal("the sweep did not schedule its next day")
	}
	if err := reconciler.run(t.Context(), host, []byte("no repo")); err != nil {
		t.Fatalf("args naming no repo: %v", err)
	}
	if request := sweep.first(host); request.OnceKey != evsLeaseSweepTaskName {
		t.Fatalf("the sweep's chain starts under %q", request.OnceKey)
	}
}
