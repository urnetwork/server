package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
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
		scheduled = scheduled || (request.Name == evsservice.EvsReconcileTaskName && request.RunAt.Equal(clock.Now().Add(defaultEvsLifecycleJobSettings().ReconcileInterval)))
	}
	if !scheduled {
		t.Fatal("the reconciler did not schedule its next day")
	}
	if request := reconcile.first(host); request.OnceKey != evsservice.EvsReconcileTaskName {
		t.Fatalf("the reconciler's chain starts under %q", request.OnceKey)
	}
}

// The shred's start and the reconciler read the projection a runtime installs over its gate cache, from rows alone
// (QD-743): none before a runtime installs it; then the repo's newest RepoDelete dated by its row, with no log behind
// it, and a network the gate has not loaded an error that leaves the repo to a retry; a newer runtime's source
// outlives the older runtime's end, and none is left once the newest ends.
func TestEvsDeletionSourceIsTheRuntimesGateCache(t *testing.T) {
	// an earlier test's runtime removes its own source when its context's AfterFunc runs, which may come later
	evsDeletionSourceCurrent.Store(nil)
	if evsDeletionSource() != nil {
		t.Fatal("no source installed, and a projection all the same")
	}
	networkId, repoId, deletionId := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	deletedTime := time.UnixMilli(sidecartest.DefaultClockStart().UnixMilli())
	states := sidecartest.NewFakeAclContractStates()
	rows := &protocol.AclRows{HasDocument: true, Repos: []*protocol.AclRepo{{
		RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL,
		Deletion: &protocol.AclRepoDeletionRow{DeletionId: deletionId.Bytes(), DeletedAt: 7, DeletedTimeMs: deletedTime.UnixMilli()},
	}}}
	if err := states.Install(networkId, rows, sidecartest.NewFakeAclView()); err != nil {
		t.Fatal(err)
	}
	removeOlder := installEvsDeletionSource(states)
	source := evsDeletionSource()
	if source == nil {
		t.Fatal("no projection once a runtime installed one")
	}
	deletion, err := source.RepoDeletion(t.Context(), networkId, repoId)
	if err != nil || deletion == nil || deletion.DeletionId != deletionId || !deletion.Time.Equal(deletedTime) || deletion.Restored || deletion.Now {
		t.Fatalf("the deletion %+v: %v", deletion, err)
	}
	if _, err := source.RepoDeletion(t.Context(), sidecar.NewId(), repoId); err == nil {
		t.Error("a network the gate has not loaded answered")
	}

	removeNewer := installEvsDeletionSource(states)
	newer := evsDeletionSource()
	removeOlder()
	if evsDeletionSource() != newer {
		t.Fatal("the older runtime's end removed the newer runtime's projection")
	}
	removeNewer()
	if evsDeletionSource() != nil {
		t.Fatal("a projection after the newest runtime's end")
	}
}
