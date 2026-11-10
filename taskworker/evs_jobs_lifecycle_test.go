package taskworker

import (
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// evs's registry binds the shred, which drops args naming no shred rather than retry them forever, and the daily
// reconciler, whose chain starts at each runtime's start and schedules its next day.
func TestEvsJobsBindTheShredAndTheReconciler(t *testing.T) {
	names := map[string]*evsJob{}
	for _, job := range evsJobs {
		names[job.name] = job
	}
	shred, reconcile := names[evsservice.EvsShredTaskName], names[evsservice.EvsReconcileTaskName]
	if shred == nil || shred.first != nil || reconcile == nil || reconcile.first == nil {
		t.Fatalf("jobs %v", names)
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	sidecartest.RegisterEvsTwins(fake.Redis)
	host := fake.ServiceHost()
	if err := shred.run(t.Context(), host, []byte("no shred")); err != nil {
		t.Fatalf("args naming no shred: %v", err)
	}
	if err := reconcile.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	scheduled := false
	for _, request := range fake.Tasks.AllRequests() {
		scheduled = scheduled || (request.Name == evsservice.EvsReconcileTaskName && request.RunAt.Equal(clock.Now().Add(evsReconcileInterval)))
	}
	if !scheduled {
		t.Fatal("the reconciler did not schedule its next day")
	}
	if request := reconcile.first(host); request.OnceKey != evsservice.EvsReconcileTaskName {
		t.Fatalf("the reconciler's chain starts under %q", request.OnceKey)
	}
}
