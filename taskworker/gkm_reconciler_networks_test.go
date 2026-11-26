//go:build sidecar_pending

package taskworker

// §10.6's networks row at a taskworker runtime's start (owner decision 371; QD-893), on the private test stack:
// network_acl_head in Postgres and sidecar.yml's gkm gate choose the networks, §8.9's real gate cache loads each
// from the admission world's log on the Net's store, and the real reconciler reads it. No {acl_}v event and no log
// entry comes after a runtime starts, so what the reconciler reads it reads because the listing asked for it.
//
//	pages    the listing reads a page at once and the next at its interval, never sooner, from the cursor past the
//	         last head it read, and a network the gkm gate does not admit is never asked for or loaded
//	restart  a quiet network's holder expiry gets its pending hint at its time, and its ended forwarding
//	         Delegation's key object is deleted DelegationKeyRetention after the end, with no new entry
//	evicted  a network the bounded cache evicted is asked for again at the reconciler's next wake for it, and read
//	ended    the world's own end: a load its end cancels fails nothing, and its cleanup waits for a load held then
//
// The fake clock is the Net's; every wait on a goroutine is a monitor or the cache's OnLoad, with a real-time guard
// only a broken cache or reconciler reaches. Nothing here blocks the reconciler: what it reports is kept in lists.
// The test's end cancels the world's context, and so every load of a cache the world built, which fails nothing; each
// such cache's cleanup waits for its loaders (AclGateCache.CloseAndWait), so none reports on a test that has
// completed. A cleanup runs on the test itself, after every attempt TestEnv.Run makes.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

// How long a wait on a goroutine may take before the test fails: a guard, never the proof.
const gkmListingGuardTimeout = 60 * time.Second

// What a goroutine reported, in order, and a monitor of how much: a waiter scans from its own cursor.
type gkmListingLog[T any] struct {
	stateLock sync.Mutex
	items     []T
	count     *sidecar.MonitorValue[int]
}

func newGkmListingLog[T any]() *gkmListingLog[T] {
	return &gkmListingLog[T]{count: sidecar.NewMonitorValue(0)}
}

// Adds an item, never blocking.
func (self *gkmListingLog[T]) add(item T) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.items = append(self.items, item)
	}()
	self.count.Update(func(count int) int { return count + 1 })
}

func (self *gkmListingLog[T]) all() []T {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.items)
}

// Waits for an item at *cursor or past it that match takes, moving *cursor past it.
func (self *gkmListingLog[T]) await(t testing.TB, cursor *int, what string, match func(item T) bool) T {
	t.Helper()
	guard := time.After(gkmListingGuardTimeout)
	for {
		_, changed := self.count.Get()
		items := self.all()
		for *cursor < len(items) {
			item := items[*cursor]
			*cursor += 1
			if match(item) {
				return item
			}
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("no %s", what)
		}
	}
}

// One read the reconciler made of a network's state.
type gkmListingRead struct {
	networkId sidecar.Id
	loaded    bool
}

// The reconciler's state as the reconciler reads it, recording every read and every ask.
type gkmListingRecorder struct {
	inner gkmservice.GkmReconcilerState
	reads *gkmListingLog[gkmListingRead]
	asks  *gkmListingLog[sidecar.Id]
	// the next read awaitRead looks at
	readCursor int
}

func newGkmListingRecorder(inner gkmservice.GkmReconcilerState) *gkmListingRecorder {
	return &gkmListingRecorder{inner: inner, reads: newGkmListingLog[gkmListingRead](), asks: newGkmListingLog[sidecar.Id]()}
}

var _ gkmservice.GkmReconcilerStateLoader = (*gkmListingRecorder)(nil)

func (self *gkmListingRecorder) Watch(changed func(change *gkmservice.GkmStateChange)) (func(), error) {
	return self.inner.Watch(changed)
}

func (self *gkmListingRecorder) Networks() []sidecar.Id {
	return self.inner.Networks()
}

func (self *gkmListingRecorder) State(ctx context.Context, networkId sidecar.Id) (*gkmservice.GkmNetworkState, error) {
	state, err := self.inner.State(ctx, networkId)
	self.reads.add(gkmListingRead{networkId: networkId, loaded: err == nil})
	return state, err
}

func (self *gkmListingRecorder) SidecarStanding(ctx context.Context, sidecarId sidecar.Id) (sidecar.Id, bool, bool, error) {
	return self.inner.SidecarStanding(ctx, sidecarId)
}

func (self *gkmListingRecorder) Load(networkId sidecar.Id) {
	self.asks.add(networkId)
	if loader, ok := self.inner.(gkmservice.GkmReconcilerStateLoader); ok {
		loader.Load(networkId)
	}
}

// Waits for the reconciler's next read of the network, loaded or not as given, passing over every other read.
func (self *gkmListingRecorder) awaitRead(t testing.TB, networkId sidecar.Id, loaded bool) {
	t.Helper()
	self.reads.await(t, &self.readCursor, fmt.Sprintf("read of %s, loaded %t", networkId, loaded), func(read gkmListingRead) bool {
		return read.networkId == networkId && read.loaded == loaded
	})
}

// One pending hint the reconciler sent, at the clock's time.
type gkmListingHint struct {
	sidecarIds []sidecar.Id
	groupId    sidecar.Id
	at         time.Time
}

// gkm's hint writer as the reconciler calls it, keeping each pending hint.
type gkmListingPush struct {
	clock sidecar.Clock
	hints *gkmListingLog[*gkmListingHint]
}

func newGkmListingPush(clock sidecar.Clock) *gkmListingPush {
	return &gkmListingPush{clock: clock, hints: newGkmListingLog[*gkmListingHint]()}
}

func (self *gkmListingPush) Hint(ctx context.Context, clientIds []connect.Id, hint *sidecar.ClientHint) error {
	return nil
}

func (self *gkmListingPush) HintSidecars(ctx context.Context, sidecarIds []connect.Id, hint *sidecar.ClientHint) error {
	body := &protocol.GkmHint{}
	if err := proto.Unmarshal(hint.Body, body); err != nil {
		return err
	}
	if body.GetKind() != protocol.GkmHintKind_GKM_HINT_PENDING {
		return nil
	}
	recorded := &gkmListingHint{at: self.clock.Now()}
	for _, sidecarId := range sidecarIds {
		recorded.sidecarIds = append(recorded.sidecarIds, sidecar.Id(sidecarId))
	}
	if groupId, err := sidecar.IdFromBytes(body.GetGroupId()); err == nil {
		recorded.groupId = groupId
	}
	self.hints.add(recorded)
	return nil
}

// Moves the clock by step once the reconciler's loop waits, then waits for the pass the move starts: step is the gate's
// hint interval, which bounds how far the loop's timer is, so the clock never moves while a pass runs
// (GkmReconciler.Idle). A pass a change started meanwhile may set the loop's timer one step late, which the next step
// reaches.
func stepGkmReconciler(t testing.TB, clock *sidecartest.FakeClock, reconciler *gkmservice.GkmReconciler, step time.Duration) {
	t.Helper()
	guard := time.After(gkmListingGuardTimeout)
	for {
		idle, changed := reconciler.Idle()
		if idle {
			break
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatal("the reconciler never waited")
		}
	}
	count, changed := reconciler.Passes()
	clock.Advance(step)
	for {
		select {
		case <-changed:
		case <-guard:
			t.Fatal("no pass after the clock moved")
		}
		var passed int
		passed, changed = reconciler.Passes()
		if count < passed {
			return
		}
	}
}

// Steps the clock until done holds, calling each after every step, and fails once the clock reaches deadline first.
func stepGkmReconcilerUntil(t testing.TB, clock *sidecartest.FakeClock, reconciler *gkmservice.GkmReconciler, step time.Duration, deadline time.Time, what string, done func() bool, each func()) {
	t.Helper()
	for !done() {
		if !clock.Now().Before(deadline) {
			t.Fatalf("no %s by %v", what, deadline)
		}
		stepGkmReconciler(t, clock, reconciler, step)
		if each != nil {
			each()
		}
	}
}

// Whether the reconciler has read the network, loaded or not as given, at *cursor or past it; moves *cursor past what
// it looked at.
func (self *gkmListingRecorder) sawRead(cursor *int, networkId sidecar.Id, loaded bool) bool {
	reads := self.reads.all()
	for *cursor < len(reads) {
		read := reads[*cursor]
		*cursor += 1
		if read.networkId == networkId && read.loaded == loaded {
			return true
		}
	}
	return false
}

// Waits until the listing has read count pages.
func awaitGkmStartListing(t testing.TB, reconciler *gkmservice.GkmReconciler, count int) gkmservice.GkmNetworkListing {
	t.Helper()
	guard := time.After(gkmListingGuardTimeout)
	for {
		listing, changed := reconciler.NetworkListing()
		if count <= listing.PageCount {
			return listing
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("the listing never read %d pages: %+v", count, listing)
		}
	}
}

// The Net and its admission worlds.
type gkmListingWorld struct {
	t   testing.TB
	ctx context.Context
	net *sidecartest.Net
	// each load of a cache built by cache, by network, after its eviction step
	loads *gkmListingLog[sidecar.Id]
}

func newGkmListingWorld(t testing.TB, ctx context.Context) *gkmListingWorld {
	net := sidecartest.NewNet(ctx, nil)
	t.Cleanup(net.Close)
	return &gkmListingWorld{t: t, ctx: ctx, net: net, loads: newGkmListingLog[sidecar.Id]()}
}

// A new network on the Net, bootstrapped.
func (self *gkmListingWorld) admission() *sidecartest.AdmissionNet {
	self.t.Helper()
	tier, err := self.net.AdmissionTier()
	if err != nil {
		self.t.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	admission := sidecartest.NewAdmissionNet(self.t, tier)
	self.t.Cleanup(admission.Close)
	return admission
}

// Raises the network's network_acl_head to its log's tip, as api's append path does.
func (self *gkmListingWorld) raiseHead(admission *sidecartest.AdmissionNet) {
	self.t.Helper()
	tip := admission.Tip()
	if _, err := model.AdvanceNetworkAclHead(self.ctx, server.Id(admission.NetworkId()), tip.GetVersion(), tip.GetChain(), false); err != nil {
		self.t.Fatal(err)
	}
}

// A gate cache over the Net's store, as gkmReconcilerStateFromHost builds one but on settings a test adjusts, hearing
// no event and polling never, so it loads only what it is asked for; over source instead when one is given. Each load
// is reported to loads, and a load error fails the test while the world's context lives.
func (self *gkmListingWorld) cache(adjust func(settings *sidecar.AclGateSettings), source sidecar.AclGateSource) *sidecar.AclGateCache {
	host := self.net.Host()
	settings := sidecar.DefaultAclGateSettings()
	settings.Log = sidecar.NewNoopLogger()
	settings.PollInterval = 0
	settings.OnLoad = func(networkId sidecar.Id, pin *bridge.AuthPin, err error) {
		// the test's end cancels the world's context, the cache's parent, and so a load still running, which fails
		// nothing; the cleanup below waits for every loader, so none reports once the test has completed
		if err != nil && self.ctx.Err() == nil {
			self.t.Errorf("the gate's load of %s: %v", networkId, err)
		}
		self.loads.add(networkId)
	}
	if adjust != nil {
		adjust(settings)
	}
	if source == nil {
		source = sidecar.NewAclGateDirectSource(host.Redis, host.Blobs, settings)
	}
	cache := sidecar.NewAclGateCache(self.ctx, source, host.Clock, settings)
	// after the admission worlds' and the Net's cleanups are registered, so it runs before them
	self.t.Cleanup(cache.CloseAndWait)
	return cache
}

// A gate source whose cold starts a test holds: Genesis reports on started and answers only once its context ends, so
// the load it serves ends as the cache's context does. It serves nothing else.
type gkmListingHeldSource struct {
	started chan struct{}
}

func newGkmListingHeldSource() *gkmListingHeldSource {
	return &gkmListingHeldSource{started: make(chan struct{}, 1)}
}

var _ sidecar.AclGateSource = (*gkmListingHeldSource)(nil)

func (self *gkmListingHeldSource) Genesis(ctx context.Context, networkId sidecar.Id) ([]byte, error) {
	select {
	case self.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (self *gkmListingHeldSource) Entries(ctx context.Context, networkId sidecar.Id, after *bridge.AuthPin) ([][]byte, error) {
	return nil, errors.New("the held source serves no entries")
}

func (self *gkmListingHeldSource) Rows(ctx context.Context, networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
	return nil, errors.New("the held source serves no rows")
}

// Waits until a cold start is held.
func (self *gkmListingHeldSource) awaitStarted(t testing.TB) {
	t.Helper()
	select {
	case <-self.started:
	case <-time.After(gkmListingGuardTimeout):
		t.Fatal("no cold start reached the held source")
	}
}

// sidecar.yml with the gkm gate on for networkIds alone.
func gkmListingConfig(networkIds ...sidecar.Id) string {
	listed := []string{}
	for _, networkId := range networkIds {
		listed = append(listed, networkId.String())
	}
	return fmt.Sprintf("sidecar:\n  gkm: {enabled: true, networks: [%s]}\n", strings.Join(listed, ", "))
}

// Networks sorted by id, each with its head raised.
func (self *gkmListingWorld) sortedNetworks(count int) []*sidecartest.AdmissionNet {
	self.t.Helper()
	admissions := []*sidecartest.AdmissionNet{}
	for range count {
		admission := self.admission()
		self.raiseHead(admission)
		admissions = append(admissions, admission)
	}
	slices.SortFunc(admissions, func(a *sidecartest.AdmissionNet, b *sidecartest.AdmissionNet) int {
		return a.NetworkId().Cmp(b.NetworkId())
	})
	return admissions
}

// The listing reads a page at once and the next only NetworkPageInterval later, from the cursor past the last head it
// read, and the reconciler asks for and reads each network the gkm gate admits; one the gate does not admit has a
// head too, and is never asked for, never loaded and never read. A gate that is off lists nothing, and one open by
// percent lists every head.
func TestGkmReconcilerStartListsTheGkmNetworksAPageAnInterval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newGkmListingWorld(t, ctx)
		networkIds := []sidecar.Id{}
		for _, admission := range world.sortedNetworks(3) {
			networkIds = append(networkIds, admission.NetworkId())
		}
		closed := networkIds[1]
		defer server.Testing_PushSidecarConfig(gkmListingConfig(networkIds[0], networkIds[2]))()

		cache := world.cache(nil, nil)
		recorder := newGkmListingRecorder(gkmservice.NewAclGkmReconcilerState(cache))
		settings := gkmservice.DefaultGkmReconcilerSettings()
		settings.NetworkPageCount = 1
		settings.NetworkPageInterval = 10 * time.Second
		var lifetime sync.WaitGroup
		defer lifetime.Wait()
		defer cancel()
		clock := world.net.Clock()
		reconciler, err := startGkmReconcilerOnHost(ctx, &lifetime, world.net.Host(), recorder, newGkmListingPush(clock), listGkmReconcilerNetworks, settings)
		if err != nil {
			t.Fatal(err)
		}

		// a head a page: the first page asks for the first network at once, and the reconciler reads its install
		if listing := awaitGkmStartListing(t, reconciler, 1); listing.Done {
			t.Fatalf("the first page: %+v", listing)
		}
		if asks := recorder.asks.all(); !slices.Equal(asks, networkIds[:1]) {
			t.Fatalf("asked for %v after the first page", asks)
		}
		recorder.awaitRead(t, networkIds[0], true)

		// the second page, the closed network's head, comes only at the interval's end, and asks for nothing
		clock.Advance(settings.NetworkPageInterval - time.Second)
		if listing, _ := reconciler.NetworkListing(); listing.PageCount != 1 {
			t.Fatalf("a page before the interval ended: %+v", listing)
		}
		clock.Advance(time.Second)
		awaitGkmStartListing(t, reconciler, 2)
		if asks := recorder.asks.all(); !slices.Equal(asks, networkIds[:1]) {
			t.Fatalf("asked for %v after the closed network's page", asks)
		}

		// the third page asks for the last network, and a fourth finds no head past it
		clock.Advance(settings.NetworkPageInterval)
		awaitGkmStartListing(t, reconciler, 3)
		recorder.awaitRead(t, networkIds[2], true)
		clock.Advance(settings.NetworkPageInterval)
		if listing := awaitGkmStartListing(t, reconciler, 4); !listing.Done || listing.FailureCount != 0 || listing.PageCount != 4 {
			t.Fatalf("the listing's end: %+v", listing)
		}
		admitted := []sidecar.Id{networkIds[0], networkIds[2]}
		if asks := recorder.asks.all(); !slices.Equal(asks, admitted) {
			t.Fatalf("asked for %v", asks)
		}
		if loaded := recorder.Networks(); !slices.Equal(loaded, admitted) {
			t.Errorf("the networks loaded: %v", loaded)
		}
		if loads := world.loads.all(); slices.Contains(loads, closed) {
			t.Errorf("the closed network loaded: %v", loads)
		}
		if _, status := cache.State(closed); status != sidecar.AclGateUnknown {
			t.Errorf("the closed network's status in the cache: %d", status)
		}
		for _, read := range recorder.reads.all() {
			if read.networkId == closed {
				t.Errorf("the closed network was read")
			}
		}

		// a gate that is off lists nothing; one open by percent lists every head, and a page past the last ends it
		func() {
			defer server.Testing_PushSidecarConfig("sidecar:\n  gkm: {enabled: false, networks: []}\n")()
			if pageIds, next, err := listGkmReconcilerNetworks(ctx, nil, 10); len(pageIds) != 0 || next != nil || err != nil {
				t.Errorf("a gate that is off listed %v, %v, %v", pageIds, next, err)
			}
		}()
		func() {
			defer server.Testing_PushSidecarConfig("sidecar:\n  gkm: {enabled: true, percent: 100}\n")()
			pageIds, next, err := listGkmReconcilerNetworks(ctx, nil, len(networkIds))
			if err != nil || !slices.Equal(pageIds, networkIds) || next == nil || *next != networkIds[2] {
				t.Errorf("every network by percent: %v, %v, %v", pageIds, next, err)
			}
			if pageIds, next, err := listGkmReconcilerNetworks(ctx, next, len(networkIds)); len(pageIds) != 0 || next != nil || err != nil {
				t.Errorf("past the last head: %v, %v, %v", pageIds, next, err)
			}
		}()
	})
}

// A runtime's own start, startGkmReconcilerFromHost on the process's evs host, lists with the real page function on
// the default settings: the network the gkm gate admits is asked for and read with no {acl_}v event, and the one it
// does not admit is not, both heads in the one page.
func TestGkmReconcilerRuntimeStartListsTheGkmNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if gkmReconcilerCurrent.Load() != nil || evsJobHost.Load() != nil {
			t.Fatal("a reconciler or an evs host held before the test")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newGkmListingWorld(t, ctx)
		admissions := world.sortedNetworks(2)
		listedId, closedId := admissions[0].NetworkId(), admissions[1].NetworkId()
		defer server.Testing_PushSidecarConfig(gkmListingConfig(listedId))()
		host := world.net.Host()
		evsJobHost.Store(host)
		defer evsJobHost.CompareAndSwap(host, nil)
		var recorder *gkmListingRecorder
		source := gkmReconcilerStateSource
		defer func() {
			gkmReconcilerStateSource = source
		}()
		gkmReconcilerStateSource = func(ctx context.Context, host *sidecar.ServiceHost) (gkmservice.GkmReconcilerState, error) {
			state, err := gkmReconcilerStateFromHost(ctx, host)
			if err != nil {
				return nil, err
			}
			recorder = newGkmListingRecorder(state)
			return recorder, nil
		}
		var lifetime sync.WaitGroup
		defer lifetime.Wait()
		defer cancel()
		startGkmReconcilerFromHost(ctx, &lifetime)
		reconciler := gkmReconcilerCurrent.Load()
		if reconciler == nil || recorder == nil {
			t.Fatal("a runtime with a host and its gate cache started no reconciler")
		}
		if listing := awaitGkmStartListing(t, reconciler, 1); !listing.Done || listing.FailureCount != 0 {
			t.Fatalf("the runtime's listing: %+v", listing)
		}
		if asks := recorder.asks.all(); !slices.Equal(asks, []sidecar.Id{listedId}) {
			t.Fatalf("asked for %v", asks)
		}
		recorder.awaitRead(t, listedId, true)
		for _, read := range recorder.reads.all() {
			if read.networkId == closedId {
				t.Error("the network the gate does not admit was read")
			}
		}
	})
}

// After a restart, a network whose log is quiet is read because the start's listing asked for it: its reader's grant
// expiry closes the repo's gate at its time and the writers get their pending hint, never before, and its ended
// forwarding Delegation's key object stands until DelegationKeyRetention after the end and goes then, with no {acl_}v
// event and no entry since the runtime started. The state is gkmReconcilerStateFromHost's, the one a runtime builds,
// and the reconciler runs on its default settings. The expiry and the deletion fall on the loop's minute grid from the
// runtime's start, which the clock steps through.
func TestGkmReconcilerAfterARestartReadsAQuietNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newGkmListingWorld(t, ctx)
		admission := world.admission()
		networkId := admission.NetworkId()
		owner := admission.Owner()
		clock := world.net.Clock()
		host := world.net.Host()
		settings := gkmservice.DefaultGkmReconcilerSettings()
		step := min(settings.PendingHintInterval, settings.DroppedLeafHintInterval)

		// a forwarding Delegation of a member device, ended by its revocation
		_, phone := admission.ConsentedMember("restart phone")
		forwardingSeed := sha256.Sum256([]byte("restart forwarding key"))
		forwarding := admission.Sign(phone, bridge.SignedType_SIGNED_DELEGATION, &protocol.Delegation{
			Header:               admission.Header(),
			Pubkey:               ed25519.NewKeyFromSeed(forwardingSeed[:]).Public().(ed25519.PublicKey),
			ReaderXwingPublicKey: bytes.Repeat([]byte{0x5a}, 1216),
			Forward:              &bridge.ForwardScope{ForwardSidecarId: phone.SidecarId.Bytes()},
		})
		admission.MustLand(forwarding)
		delegationId := sidecartest.AdmissionStatementId(forwarding)
		revoked := admission.MustLand(admission.Sign(phone, bridge.SignedType_SIGNED_DELEGATION_REVOKE, admission.DelegationRevoke(delegationId, 0)))
		revokedEntry := &bridge.AuthEntry{}
		if err := proto.Unmarshal(admission.Entry(revoked.Pin.GetVersion()), revokedEntry); err != nil {
			t.Fatal(err)
		}
		endedAt := time.UnixMilli(revokedEntry.GetTimeMs())

		// a repo the owner writes and an ephemeral device reads until an hour after that end, by a grant
		repoId, groupId := admission.Id("restart repo"), admission.Id("restart group")
		admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_REPO_CREATE, &protocol.RepoCreate{
			Header: admission.Header(), RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL,
		}))
		burner := admission.Device("restart burner", owner.MemberId)
		admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_SIDECAR_ADMIT,
			admission.Admission(burner, nil, true, clock.Now().Add(48*time.Hour).UnixMilli(), 0)))
		expires := endedAt.Add(time.Hour)
		admission.MustLand(admission.Sign(owner, bridge.SignedType_SIGNED_REPO_GRANT, &protocol.RepoGrant{
			Header: admission.Header(), RepoId: repoId.Bytes(), Subject: burner.SidecarId.Bytes(),
			Role: protocol.RepoGrantRole_REPO_GRANT_ROLE_READER, Expires: expires.UnixMilli(),
		}))
		if expiry, found, err := sidecar.GkmHolderExpiry(admission.State(), repoId, clock.Now(), expires.Add(time.Hour)); err != nil || !found || !expiry.Equal(expires) {
			t.Fatalf("the fixture's holder expiry: %v, %t, %v", expiry, found, err)
		}

		// gkm's objects as KF1 and the forwarding device left them: the repo placed, its group formed with its first
		// generation at the tip, and the Delegation's reader key object
		tip := admission.Tip()
		first, err := host.Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		put := func(key string, objectBytes []byte) {
			t.Helper()
			if _, err := first.PutIfNoneMatch(ctx, key, objectBytes, nil); err != nil {
				t.Fatal(err)
			}
		}
		signed := func(signedType bridge.SignedType, body proto.Message) []byte {
			t.Helper()
			bodyBytes, err := proto.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			signedBytes, err := proto.Marshal(&bridge.Signed{
				Body: bodyBytes, SignerId: owner.SidecarId.Bytes(), Signature: make([]byte, ed25519.SignatureSize), Type: uint32(signedType),
			})
			if err != nil {
				t.Fatal(err)
			}
			return signedBytes
		}
		place, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: owner.SidecarId}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		put(sidecar.EvsPlaceKey(repoId), place)
		put(gkmservice.GkmFormKey(repoId, 1), signed(bridge.SignedType_SIGNED_GROUP_FORM, &protocol.GroupForm{
			RepoId: repoId.Bytes(), GroupId: groupId.Bytes(), Generation: 1,
		}))
		commitment := sha256.Sum256([]byte("restart commitment"))
		put(gkmservice.GkmGenerationKey(groupId, 1), signed(bridge.SignedType_SIGNED_GEN_ROTATE, &protocol.GenRotate{
			GroupId: groupId.Bytes(), RepoId: repoId.Bytes(), Generation: 1, Commitment: commitment[:], Cause: 2,
			Auth: tip, TimeMs: clock.Now().UnixMilli(),
		}))
		delegationKey, err := gkmservice.GkmDelegationKeyKey(networkId, delegationId)
		if err != nil {
			t.Fatal(err)
		}
		put(delegationKey, []byte("a forwarding reader key"))
		world.raiseHead(admission)
		defer server.Testing_PushSidecarConfig(gkmListingConfig(networkId))()

		// the restart, ten minutes after the end: a runtime whose cache has heard nothing, as every process's does at
		// its start, the expiry and the deletion whole minutes on
		clock.AdvanceTo(endedAt.Add(10 * time.Minute))
		state, err := gkmReconcilerStateFromHost(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		if networkIds := state.Networks(); len(networkIds) != 0 {
			t.Fatalf("networks before the listing: %v", networkIds)
		}
		recorder := newGkmListingRecorder(state)
		push := newGkmListingPush(clock)
		var lifetime sync.WaitGroup
		defer lifetime.Wait()
		defer cancel()
		reconciler, err := startGkmReconcilerOnHost(ctx, &lifetime, host, recorder, push, listGkmReconcilerNetworks, nil)
		if err != nil {
			t.Fatal(err)
		}
		recorder.awaitRead(t, networkId, true)
		if asks := recorder.asks.all(); !slices.Equal(asks, []sidecar.Id{networkId}) {
			t.Fatalf("asked for %v", asks)
		}
		networkState, err := state.State(ctx, networkId)
		if err != nil || !proto.Equal(networkState.View.Pin(), tip) {
			t.Fatalf("the network's state after the listing: %v", err)
		}
		if at, known := networkState.PositionTime(revoked.Pin.GetVersion()); !known || !at.Equal(endedAt) {
			t.Fatalf("the Delegation's end at %v, %t", at, known)
		}

		// the grant's expiry: the gate closes at its time and the writers are hinted, never before
		hintCursor := 0
		hinted := func() bool {
			hints := push.hints.all()
			for hintCursor < len(hints) {
				hint := hints[hintCursor]
				hintCursor += 1
				if hint.groupId == groupId && slices.Contains(hint.sidecarIds, owner.SidecarId) {
					return true
				}
			}
			return false
		}
		stepGkmReconcilerUntil(t, clock, reconciler, step, expires.Add(3*step), "pending hint at the expiry", hinted, nil)
		for _, hint := range push.hints.all() {
			if hint.groupId == groupId && hint.at.Before(expires) {
				t.Errorf("a pending hint before the expiry, at %v to %v", hint.at, hint.sidecarIds)
			}
		}

		// the key object stands until DelegationKeyRetention after the end, and goes then: the pass at that time queues
		// it for the deleter, whose own goroutine deletes it
		deleteAt := endedAt.Add(settings.DelegationKeyRetention)
		due := func() bool {
			return !clock.Now().Before(deleteAt)
		}
		standsBefore := func() {
			if count, _ := reconciler.DelegationKeyDeletes(); count != 0 && clock.Now().Before(deleteAt) {
				t.Fatalf("a key object deletion at %v, before %v", clock.Now(), deleteAt)
			}
			if _, err := first.Head(ctx, delegationKey); err != nil && clock.Now().Before(deleteAt) {
				t.Fatalf("the key object at %v, before its retention ended: %v", clock.Now(), err)
			}
		}
		stepGkmReconcilerUntil(t, clock, reconciler, step, deleteAt.Add(step), "the deletion's time", due, standsBefore)
		guard := time.After(gkmListingGuardTimeout)
		for {
			count, changed := reconciler.DelegationKeyDeletes()
			if 1 <= count {
				break
			}
			select {
			case <-changed:
			case <-guard:
				t.Fatal("the key object was never deleted")
			}
		}
		if _, err := first.Head(ctx, delegationKey); !errors.Is(err, sidecar.ErrBlobNotFound) {
			t.Errorf("the key object after its retention: %v", err)
		}
		if !proto.Equal(admission.Tip(), tip) {
			t.Errorf("the log moved: %d, was %d", admission.Tip().GetVersion(), tip.GetVersion())
		}
	})
}

// With a cache that holds one network at a time, loading the second evicts the first; the reconciler's next wake for
// the first finds no state, asks for it again, and reads it once it installs, evicting the second, which is asked
// for nothing meanwhile. The pages and the wakes fall on the loop's minute grid, which the clock steps through.
func TestGkmReconcilerAsksForAnEvictedNetworkAtItsNextWake(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newGkmListingWorld(t, ctx)
		admissions := world.sortedNetworks(2)
		firstId, secondId := admissions[0].NetworkId(), admissions[1].NetworkId()
		defer server.Testing_PushSidecarConfig(gkmListingConfig(firstId, secondId))()

		cache := world.cache(func(settings *sidecar.AclGateSettings) {
			// below every network's bytes: a load evicts every other loaded network
			settings.ByteBudget = 1
		}, nil)
		recorder := newGkmListingRecorder(gkmservice.NewAclGkmReconcilerState(cache))
		settings := gkmservice.DefaultGkmReconcilerSettings()
		step := min(settings.PendingHintInterval, settings.DroppedLeafHintInterval)
		settings.NetworkPageCount = 1
		settings.NetworkPageInterval = 2 * step
		// a network with no expiry is woken this long after its read
		settings.ExpiryHorizon = 5 * step
		var lifetime sync.WaitGroup
		defer lifetime.Wait()
		defer cancel()
		clock := world.net.Clock()
		startedAt := clock.Now()
		reconciler, err := startGkmReconcilerOnHost(ctx, &lifetime, world.net.Host(), recorder, newGkmListingPush(clock), listGkmReconcilerNetworks, settings)
		if err != nil {
			t.Fatal(err)
		}
		loadCursor := 0
		isNetwork := func(networkId sidecar.Id) func(loaded sidecar.Id) bool {
			return func(loaded sidecar.Id) bool { return loaded == networkId }
		}
		awaitGkmStartListing(t, reconciler, 1)
		world.loads.await(t, &loadCursor, "load of the first network", isNetwork(firstId))
		recorder.awaitRead(t, firstId, true)

		// the second page at its interval: the second network loads, which evicts the first
		stepGkmReconciler(t, clock, reconciler, step)
		stepGkmReconciler(t, clock, reconciler, step)
		awaitGkmStartListing(t, reconciler, 2)
		// OnLoad runs after the load's eviction step
		world.loads.await(t, &loadCursor, "load of the second network", isNetwork(secondId))
		recorder.awaitRead(t, secondId, true)
		if _, status := cache.State(firstId); status != sidecar.AclGateNotLoaded {
			t.Fatalf("the first network's status once the second loaded: %d", status)
		}
		if stats := cache.Stats(); stats.EvictionCount != 1 {
			t.Fatalf("evictions: %d", stats.EvictionCount)
		}

		// the first network's wake, ExpiryHorizon after its read: no state, an ask, and its install read
		readCursor := recorder.readCursor
		wokenAt := startedAt.Add(settings.ExpiryHorizon)
		stepGkmReconcilerUntil(t, clock, reconciler, step, wokenAt.Add(3*step), "read of the evicted network at its wake", func() bool {
			return recorder.sawRead(&readCursor, firstId, false)
		}, nil)
		if clock.Now().Before(wokenAt) {
			t.Fatalf("the evicted network was read at %v, before its wake at %v", clock.Now(), wokenAt)
		}
		world.loads.await(t, &loadCursor, "the first network's reload", isNetwork(firstId))
		recorder.readCursor = readCursor
		recorder.awaitRead(t, firstId, true)
		if asks := recorder.asks.all(); !slices.Equal(asks, []sidecar.Id{firstId, secondId, firstId}) {
			t.Errorf("asked for %v", asks)
		}
		if _, status := cache.State(firstId); status != sidecar.AclGateLoaded {
			t.Errorf("the first network's status after its wake: %d", status)
		}
		if stats := cache.Stats(); stats.EvictionCount != 2 {
			t.Errorf("evictions: %d", stats.EvictionCount)
		}
	})
}

// A load the test's end cancels fails nothing: a load held in its source as the world's context ends reaches OnLoad
// with the cancel's error, and the world reports no failure for it. Before, OnLoad failed the test for any load error,
// the cancel of the test's own end among them ("the gate's load of ...: context canceled"), here at every attempt.
func TestGkmListingWorldFailsNothingForALoadItsEndCancels(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newGkmListingWorld(t, ctx)
		networkId := world.admission().NetworkId()
		held := newGkmListingHeldSource()
		cache := world.cache(nil, held)
		cache.Changed(networkId)
		held.awaitStarted(t)

		// the test's end, before the test returns: the held load fails with the cancel, and its OnLoad runs
		cancel()
		loadCursor := 0
		world.loads.await(t, &loadCursor, "the cancelled load's OnLoad", func(loaded sidecar.Id) bool {
			return loaded == networkId
		})
		if _, status := cache.State(networkId); status == sidecar.AclGateLoaded {
			t.Error("the cancelled load installed")
		}
	})
}

// A test that ends with its world's load held in the source: the end cancels the load, which fails nothing, and the
// cache's cleanup waits for its loader, so the load's OnLoad has run by the cleanups registered ahead of the cache.
// Before, the cleanup closed the cache without waiting, and the cancelled load's OnLoad failed the attempt from a
// loader nothing waited for, or reported on a test that had completed.
func TestGkmListingWorldEndsWithALoadHeld(t *testing.T) {
	test := t
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newGkmListingWorld(t, ctx)
		networkId := world.admission().NetworkId()
		// after the cache's cleanup, on the test itself, whose failures an attempt's cleanups would not carry; an attempt
		// that failed before its load was held checks nothing
		held := false
		test.Cleanup(func() {
			if held && !slices.Contains(world.loads.all(), networkId) {
				test.Error("the held load's OnLoad had not run when the cache's cleanup returned")
			}
		})
		source := newGkmListingHeldSource()
		world.cache(nil, source).Changed(networkId)
		source.awaitStarted(t)
		held = true
	})
}
