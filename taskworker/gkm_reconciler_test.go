package taskworker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
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
// the sweep's name bound, so a run queued anyway waits on its retry: no state source, one that fails, and a host
// whose Redis or clusters the real one cannot build on.
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
	source := gkmReconcilerStateSource
	defer func() {
		gkmReconcilerStateSource = source
	}()

	gkmReconcilerStateSource = nil
	startGkmReconcilerFromHost(ctx, &lifetime)
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler with no state source")
	}
	gkmReconcilerStateSource = func(ctx context.Context, host *sidecar.ServiceHost) (gkmservice.GkmReconcilerState, error) {
		return nil, errors.New("no authorization state")
	}
	startGkmReconcilerFromHost(ctx, &lifetime)
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler with no authorization state")
	}

	// the real source builds nothing on a host with no Redis or no clusters
	gkmReconcilerStateSource = source
	for _, strip := range []func(host *sidecar.ServiceHost){
		func(host *sidecar.ServiceHost) { host.Redis = nil },
		func(host *sidecar.ServiceHost) { host.Blobs = nil },
	} {
		stripped := *host
		strip(&stripped)
		if state, err := gkmReconcilerStateFromHost(ctx, &stripped); err == nil || state != nil {
			t.Errorf("a state on a host missing a store: %v", err)
		}
		evsJobHost.Store(&stripped)
		startGkmReconcilerFromHost(ctx, &lifetime)
		evsJobHost.Store(host)
		if gkmReconcilerCurrent.Load() != nil {
			t.Fatal("a reconciler on a host missing a store")
		}
	}
	cancel()
	lifetime.Wait()
}

// A runtime with an evs host starts its reconciler on the state server builds for it (QD-718): §8.9's gate cache
// over the host's Redis and first metadata cluster. That state hears {acl_<networkId>}v's INCR on the host's own
// subscriber, tells a change once the network's state is installed, and then lists the network, gives its state at
// the log's tip and knows its sidecars' standing; a revocation reaches it the same way.
func TestGkmReconcilerStartsOnTheHostsGateCache(t *testing.T) {
	if gkmReconcilerCurrent.Load() != nil || evsJobHost.Load() != nil {
		t.Fatal("a reconciler or an evs host held before the test")
	}
	net := sidecartest.NewNet(t.Context(), nil)
	defer net.Close()
	tier, err := net.AdmissionTier()
	if err != nil {
		t.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	admission := sidecartest.NewAdmissionNet(t, tier)
	defer admission.Close()
	host := net.Host()
	networkId := admission.NetworkId()
	owner := admission.Owner()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var lifetime sync.WaitGroup
	evsJobHost.Store(host)
	defer evsJobHost.CompareAndSwap(host, nil)
	startGkmReconcilerFromHost(ctx, &lifetime)
	reconciler := gkmReconcilerCurrent.Load()
	if reconciler == nil {
		t.Fatal("a runtime with a host and its gate cache started no reconciler")
	}

	state, err := gkmReconcilerStateFromHost(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	changes := make(chan *gkmservice.GkmStateChange, 64)
	stopWatch, err := state.Watch(func(change *gkmservice.GkmStateChange) {
		changes <- change
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stopWatch()
	// a guard, never the proof: only a broken cache reaches it
	await := func(what string) *gkmservice.GkmStateChange {
		t.Helper()
		select {
		case change := <-changes:
			return change
		case <-time.After(30 * time.Second):
			t.Fatalf("no %s", what)
		}
		return nil
	}
	hint := func() {
		t.Helper()
		if _, err := host.Redis.Increment(ctx, sidecar.AclVersionKey(networkId), 48*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	// until the tip is installed: a hint's load may land one entry behind a log that moved twice
	awaitTip := func() *gkmservice.GkmNetworkState {
		t.Helper()
		for {
			change := await("change")
			if change == nil || change.NetworkId != networkId {
				t.Fatalf("a change of another network: %+v", change)
			}
			networkState, err := state.State(ctx, networkId)
			if err != nil {
				t.Fatal(err)
			}
			if proto.Equal(networkState.View.Pin(), admission.Tip()) {
				return networkState
			}
		}
	}
	if change := await("first change"); change != nil {
		t.Fatalf("the watch's first change: %+v", change)
	}
	if networkIds := state.Networks(); len(networkIds) != 0 {
		t.Fatalf("networks before any event: %v", networkIds)
	}

	hint()
	networkState := awaitTip()
	if networkIds := state.Networks(); !slices.Equal(networkIds, []sidecar.Id{networkId}) {
		t.Fatalf("the networks: %v", networkIds)
	}
	if !slices.Contains(networkState.RepoIds, admission.ConfigRepoId()) {
		t.Errorf("the network's repos: %v", networkState.RepoIds)
	}
	if standingNetworkId, revoked, known, err := state.SidecarStanding(ctx, owner.SidecarId); err != nil || !known || revoked || standingNetworkId != networkId {
		t.Fatalf("the owner's standing: %s, %t, %t, %v", standingNetworkId, revoked, known, err)
	}

	_, phone := admission.ConsentedMember("taskworker reconciler")
	admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_SIDECAR_REVOKE, admission.Revocation(phone, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_LOST, 0)))
	if _, _, known, _ := state.SidecarStanding(ctx, phone.SidecarId); known {
		t.Fatal("a sidecar known before any event told of its admission")
	}
	hint()
	awaitTip()
	if _, revoked, known, err := state.SidecarStanding(ctx, phone.SidecarId); err != nil || !known || !revoked {
		t.Fatalf("the revoked device's standing: %t, %t, %v", revoked, known, err)
	}

	// evs's shred start and reconciler read the same cache (C-09): a RepoDelete it loaded comes dated by its row
	// (QD-743), with its signer while the cache's log keeps the entry
	repoId, deletionId := admission.Id("taskworker deleted repo"), admission.Id("taskworker deletion")
	admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_REPO_CREATE, &protocol.RepoCreate{
		Header: admission.Header(), RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL,
	}))
	deleted := admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_REPO_DELETE, &protocol.RepoDelete{
		Header: admission.Header(), RepoId: repoId.Bytes(), DeletionId: deletionId.Bytes(),
	}))
	hint()
	awaitTip()
	deletedEntry := &bridge.AuthEntry{}
	if err := proto.Unmarshal(admission.Entry(deleted.Pin.GetVersion()), deletedEntry); err != nil {
		t.Fatal(err)
	}
	source := evsDeletionSource()
	if source == nil {
		t.Fatal("the runtime gave evs's lifecycle jobs no projection")
	}
	deletion, err := source.RepoDeletion(ctx, networkId, repoId)
	if err != nil || deletion == nil || deletion.DeletionId != deletionId || !deletion.Time.Equal(time.UnixMilli(deletedEntry.GetTimeMs())) ||
		deletion.SignerSidecarId != owner.SidecarId || deletion.Now || deletion.Restored {
		t.Fatalf("the RepoDelete evs's jobs read: %+v, %v", deletion, err)
	}

	// the runtime's end: the reconciler is no longer current
	cancel()
	lifetime.Wait()
	if gkmReconcilerCurrent.Load() != nil {
		t.Fatal("a reconciler current after the runtime's end")
	}
}
