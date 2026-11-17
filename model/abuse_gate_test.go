package model

// §21.7's standing in memory (ABUSE-M5-07) with no store: the derivation every reader shares, the limits table, the
// hard deny snapshot over a test's source and NewAbuseGate's cache over a test's loader, each on a clock the test
// moves. Loads are waited on through OnLoad, never slept on. BenchmarkAbuseGate holds a warm read to < 1 us and no
// allocation, which TestAbuseGateAllocatesNothing checks on every run.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// A clock the test moves. Safe for concurrent use.
type testAbuseClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func newTestAbuseClock() *testAbuseClock {
	return &testAbuseClock{now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
}

func (self *testAbuseClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testAbuseClock) Advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// The held networks a test sets, or a failure. Safe for concurrent use.
type testAbuseHardDenySource struct {
	stateLock          sync.Mutex
	networkIdStandings map[server.Id]AbuseStanding
	err                error
}

func (self *testAbuseHardDenySource) set(networkIdStandings map[server.Id]AbuseStanding, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkIdStandings = networkIdStandings
	self.err = err
}

func (self *testAbuseHardDenySource) load(ctx context.Context) (map[server.Id]AbuseStanding, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return nil, self.err
	}
	networkIdStandings := map[server.Id]AbuseStanding{}
	for networkId, standing := range self.networkIdStandings {
		networkIdStandings[networkId] = standing
	}
	return networkIdStandings, nil
}

// A snapshot over source on clock that loads once at construction and then only when the test calls load, its first
// load waited on.
func testAbuseHardDenySnapshot(t testing.TB, source *testAbuseHardDenySource, clock *testAbuseClock) *AbuseHardDenySnapshot {
	t.Helper()
	loads := make(chan error, 1)
	settings := DefaultAbuseHardDenySettings()
	settings.Interval = 0
	settings.OnLoad = func(count int, err error) {
		select {
		case loads <- err:
		default:
		}
	}
	snapshot := newAbuseHardDenySnapshot(context.Background(), source.load, clock.Now, settings)
	t.Cleanup(snapshot.Close)
	<-loads
	settings.OnLoad = nil
	return snapshot
}

// The facts a test's loader answers, and every batch it was asked. Safe for concurrent use.
type testAbuseGateLoad struct {
	stateLock      sync.Mutex
	networkIdFacts map[server.Id]*networkAbuseFacts
	err            error
	batches        [][]server.Id
	// when set, each load waits for it to close before it answers
	hold chan struct{}
}

func newTestAbuseGateLoad() *testAbuseGateLoad {
	return &testAbuseGateLoad{networkIdFacts: map[server.Id]*networkAbuseFacts{}}
}

func (self *testAbuseGateLoad) put(networkId server.Id, facts *networkAbuseFacts) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkIdFacts[networkId] = facts
}

func (self *testAbuseGateLoad) remove(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.networkIdFacts, networkId)
}

func (self *testAbuseGateLoad) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

// Holds every load until the returned release.
func (self *testAbuseGateLoad) holdLoads() (release func()) {
	hold := make(chan struct{})
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hold = hold
	return func() {
		close(hold)
	}
}

func (self *testAbuseGateLoad) load(ctx context.Context, networkIds []server.Id, now time.Time) (map[server.Id]*networkAbuseFacts, error) {
	self.stateLock.Lock()
	hold := self.hold
	self.stateLock.Unlock()
	if hold != nil {
		<-hold
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.batches = append(self.batches, slices.Clone(networkIds))
	if self.err != nil {
		return nil, self.err
	}
	networkIdFacts := map[server.Id]*networkAbuseFacts{}
	for _, networkId := range networkIds {
		if facts, found := self.networkIdFacts[networkId]; found {
			networkIdFacts[networkId] = facts
		}
	}
	return networkIdFacts, nil
}

// How many times the loader was asked for networkId.
func (self *testAbuseGateLoad) loadCount(networkId server.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, batch := range self.batches {
		if slices.Contains(batch, networkId) {
			count += 1
		}
	}
	return count
}

// One load the gate finished: the networks it read and why it failed.
type testAbuseGateLoaded struct {
	networkIds []server.Id
	err        error
}

// A gate over snapshot and load on clock, every finished load sent on the returned channel.
func testAbuseGate(
	t testing.TB,
	snapshot *AbuseHardDenySnapshot,
	load *testAbuseGateLoad,
	clock *testAbuseClock,
	change func(settings *AbuseGateSettings),
) (*NetworkAbuseGate, chan testAbuseGateLoaded) {
	t.Helper()
	loads := make(chan testAbuseGateLoaded, 1024)
	settings := DefaultAbuseGateSettings()
	settings.LoadRetryInterval = time.Millisecond
	settings.OnLoad = func(networkIds []server.Id, err error) {
		loads <- testAbuseGateLoaded{networkIds: slices.Clone(networkIds), err: err}
	}
	if change != nil {
		change(settings)
	}
	gate := newAbuseGate(context.Background(), snapshot, load.load, clock.Now, settings)
	t.Cleanup(gate.Close)
	return gate, loads
}

// Waits until the gate's loads have read every one of networkIds, however it batched them, and answers the last
// load's error.
func testAwaitAbuseGateLoad(t testing.TB, loads chan testAbuseGateLoaded, networkIds ...server.Id) error {
	t.Helper()
	waiting := map[server.Id]bool{}
	for _, networkId := range networkIds {
		waiting[networkId] = true
	}
	for {
		select {
		case loaded := <-loads:
			for _, networkId := range loaded.networkIds {
				delete(waiting, networkId)
			}
			if len(waiting) == 0 {
				return loaded.err
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no load")
			return nil
		}
	}
}

// A network's facts as the store would read them at now: created age ago, with row as its network_abuse_state.
func testAbuseFacts(now time.Time, age time.Duration, row *networkAbuseRow, verified bool) *networkAbuseFacts {
	return &networkAbuseFacts{
		createTime: now.Add(-age),
		row:        row,
		verified:   verified,
		version:    now.UnixMicro(),
	}
}

const testAbuseDay = 24 * time.Hour

// QD-194's order: suspended, probation, restricted, trusted, standard, each holding exactly while its entry does.
func TestDeriveNetworkAbuseStanding(t *testing.T) {
	settings := DefaultAbuseSettings()
	now := newTestAbuseClock().Now()
	networkId := server.NewId()
	caseId := server.NewId()
	ahead := now.Add(3 * testAbuseDay)
	behind := now.Add(-time.Second)
	restricted := func(untilTime *time.Time) *networkAbuseRow {
		return &networkAbuseRow{state: networkAbuseStateRestricted, caseId: caseId, untilTime: untilTime}
	}
	for _, c := range []struct {
		name       string
		age        time.Duration
		row        *networkAbuseRow
		standing   AbuseStanding
		withCase   bool
		changeTime *time.Time
	}{
		{name: "6 days old", age: 6 * testAbuseDay, standing: AbuseStandingProbation, changeTime: ptrTime(now.Add(testAbuseDay))},
		{name: "8 days old", age: 8 * testAbuseDay, standing: AbuseStandingStandard},
		{name: "exactly 7 days old", age: 7 * testAbuseDay, standing: AbuseStandingStandard},
		{name: "restricted ahead", age: 8 * testAbuseDay, row: restricted(&ahead), standing: AbuseStandingRestricted, withCase: true, changeTime: &ahead},
		{name: "restricted behind", age: 8 * testAbuseDay, row: restricted(&behind), standing: AbuseStandingStandard},
		{name: "restricted with no end", age: 8 * testAbuseDay, row: restricted(nil), standing: AbuseStandingRestricted, withCase: true},
		// probation's mail rule is the stricter, so it goes first
		{name: "restricted on probation", age: 2 * testAbuseDay, row: restricted(&ahead), standing: AbuseStandingProbation, changeTime: ptrTime(now.Add(5 * testAbuseDay))},
		{name: "suspended on probation", age: 2 * testAbuseDay, row: &networkAbuseRow{state: networkAbuseStateSuspended, caseId: caseId}, standing: AbuseStandingSuspended, withCase: true},
		{name: "suspended", age: 400 * testAbuseDay, row: &networkAbuseRow{state: networkAbuseStateSuspended, caseId: caseId}, standing: AbuseStandingSuspended, withCase: true},
		{name: "a state this build does not know", age: 400 * testAbuseDay, row: &networkAbuseRow{state: "banished", caseId: caseId}, standing: AbuseStanding("banished"), withCase: true},
	} {
		standing := deriveNetworkAbuseStanding(networkId, now.Add(-c.age), c.row, false, now, settings)
		if standing.Standing != c.standing || standing.NetworkId != networkId {
			t.Errorf("%s: %+v", c.name, standing)
			continue
		}
		if (standing.CaseId != nil) != c.withCase || (c.withCase && *standing.CaseId != caseId) {
			t.Errorf("%s: case %v", c.name, standing.CaseId)
		}
		if (standing.ChangeTime == nil) != (c.changeTime == nil) || (c.changeTime != nil && !standing.ChangeTime.Equal(*c.changeTime)) {
			t.Errorf("%s: change time %v, want %v", c.name, standing.ChangeTime, c.changeTime)
		}
	}
	if standing := deriveNetworkAbuseStanding(networkId, now.Add(-banishedAge()), &networkAbuseRow{state: "banished", caseId: caseId}, false, now, settings); standing.Standing.Known() {
		t.Error("a state this build does not know reads as one a gate passes")
	}

	// a non-positive ProbationAge puts no network on probation
	settings.ProbationAge = 0
	if standing := deriveNetworkAbuseStanding(networkId, now, nil, false, now, settings); standing.Standing != AbuseStandingStandard {
		t.Errorf("no probation: %+v", standing)
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func banishedAge() time.Duration {
	return 400 * testAbuseDay
}

// Only kills and network halts restrict, and never a no-strike one or one review lifted as wrong
// (plan-ABUSE-small-readings #4).
func TestAbuseActionRestricts(t *testing.T) {
	for _, c := range []struct {
		kind        AbuseActionKind
		category    AbuseCategory
		reviewState AbuseReviewState
		restricts   bool
	}{
		{kind: AbuseActionKindHostKill, category: AbuseCategoryPhishing, reviewState: AbuseReviewStatePending, restricts: true},
		{kind: AbuseActionKindHostSuspend, category: AbuseCategoryMalware, reviewState: AbuseReviewStateUpheld, restricts: true},
		{kind: AbuseActionKindLabelKill, category: AbuseCategoryCommandAndControl, reviewState: AbuseReviewStatePending, restricts: true},
		{kind: AbuseActionKindZoneSuspend, category: AbuseCategoryPhishing, reviewState: AbuseReviewStatePending, restricts: true},
		{kind: AbuseActionKindNetworkSuspend, category: AbuseCategoryCsam, reviewState: AbuseReviewStatePending, restricts: true},
		{kind: AbuseActionKindMailHalt, category: AbuseCategorySpam, reviewState: AbuseReviewStatePending, restricts: true},
		// lifted, not as wrong: still a strike
		{kind: AbuseActionKindHostKill, category: AbuseCategoryCopyright, reviewState: AbuseReviewStateLifted, restricts: true},
		{kind: AbuseActionKindHostKill, category: AbuseCategoryPhishing, reviewState: AbuseReviewStateLiftedWrong, restricts: false},
		{kind: AbuseActionKindHostKill, category: AbuseCategoryExposedPanel, reviewState: AbuseReviewStatePending, restricts: false},
		{kind: AbuseActionKindMailHalt, category: AbuseCategoryCompromise, reviewState: AbuseReviewStatePending, restricts: false},
		{kind: AbuseActionKindMailLabelHalt, category: AbuseCategorySpam, reviewState: AbuseReviewStatePending, restricts: false},
		{kind: AbuseActionKindCertRevoke, category: AbuseCategoryPhishing, reviewState: AbuseReviewStatePending, restricts: false},
		{kind: AbuseActionKindNoticeForward, category: AbuseCategoryCopyright, reviewState: AbuseReviewStatePending, restricts: false},
		{kind: AbuseActionKindLift, category: AbuseCategoryPhishing, reviewState: AbuseReviewStateUpheld, restricts: false},
	} {
		if restricts := AbuseActionRestricts(c.kind, c.category, c.reviewState); restricts != c.restricts {
			t.Errorf("%s %s %s: %t", c.kind, c.category, c.reviewState, restricts)
		}
	}
}

// §21.7's Effect column for each standing; a standing this build does not know takes suspended's zeros.
func TestAbuseStandingLimits(t *testing.T) {
	settings := DefaultAbuseSettings()
	probationDnsEntries := Pro().SidecarAbuse.Limit(DnsProbationEntriesLimitKey)

	probation := AbuseStandingLimits(AbuseStandingProbation, settings)
	if !probation.Reach || probation.TlsEndpoints != 1 || probation.PublishMaxDuration != 24*time.Hour ||
		probation.DnsEntries != probationDnsEntries || probation.DnsAddressRecords || probation.HeldNames ||
		probation.Aliases || probation.OutboundShares || !probation.SameNetworkMailOnly || probation.CommunityMailCaps || probation.BrandUse {
		t.Errorf("probation: %+v", probation)
	}
	restricted := AbuseStandingLimits(AbuseStandingRestricted, settings)
	if !restricted.Reach || restricted.TlsEndpoints != 1 || restricted.PublishMaxDuration != 24*time.Hour ||
		restricted.DnsEntries != probationDnsEntries || restricted.DnsAddressRecords || restricted.HeldNames ||
		restricted.Aliases || restricted.OutboundShares || restricted.SameNetworkMailOnly || !restricted.CommunityMailCaps {
		t.Errorf("restricted: %+v", restricted)
	}
	standard := AbuseStandingLimits(AbuseStandingStandard, settings)
	if !standard.Reach || standard.TlsEndpoints != -1 || standard.PublishMaxDuration != 0 || standard.DnsEntries != -1 ||
		!standard.DnsAddressRecords || !standard.HeldNames || !standard.Aliases || !standard.OutboundShares ||
		standard.SameNetworkMailOnly || standard.CommunityMailCaps || standard.BrandUse {
		t.Errorf("standard: %+v", standard)
	}
	trusted := AbuseStandingLimits(AbuseStandingTrusted, settings)
	if !trusted.Reach || trusted.TlsEndpoints != -1 || !trusted.OutboundShares || !trusted.BrandUse {
		t.Errorf("trusted: %+v", trusted)
	}
	for _, standing := range []AbuseStanding{AbuseStandingSuspended, AbuseStanding("banished"), AbuseStanding("")} {
		limits := AbuseStandingLimits(standing, settings)
		if limits.Reach || limits.TlsEndpoints != 0 || limits.DnsEntries != 0 || limits.DnsAddressRecords ||
			limits.HeldNames || limits.Aliases || limits.OutboundShares || limits.BrandUse {
			t.Errorf("%q: %+v", standing, limits)
		}
	}
	// the endpoint cap is the settings', whatever the tier
	settings.ProbationTlsEndpoints = 3
	if limits := AbuseStandingLimits(AbuseStandingProbation, settings); limits.TlsEndpoints != 3 {
		t.Errorf("a set cap: %+v", limits)
	}
}

// Not loaded until a load reads cleanly; a failed load keeps the last set; a set past MaxAge answers not loaded.
func TestAbuseHardDenySnapshot(t *testing.T) {
	clock := newTestAbuseClock()
	held, other := server.NewId(), server.NewId()
	source := &testAbuseHardDenySource{err: errors.New("no store")}
	snapshot := testAbuseHardDenySnapshot(t, source, clock)
	if _, _, ok := snapshot.Denied(held); ok || snapshot.Ready() {
		t.Fatal("a snapshot that never loaded answered")
	}

	source.set(map[server.Id]AbuseStanding{held: AbuseStandingSuspended}, nil)
	snapshot.load()
	if standing, denied, ok := snapshot.Denied(held); !ok || !denied || standing != AbuseStandingSuspended {
		t.Fatalf("the held network: %q %t %t", standing, denied, ok)
	}
	if _, denied, ok := snapshot.Denied(other); !ok || denied {
		t.Fatal("a network with no row is held")
	}

	// a failed load keeps the last set, and a lift reaches the set at the next clean load
	source.set(nil, errors.New("the store went away"))
	clock.Advance(5 * time.Second)
	snapshot.load()
	if _, denied, ok := snapshot.Denied(held); !ok || !denied {
		t.Fatal("a failed load dropped the last set")
	}
	source.set(map[server.Id]AbuseStanding{}, nil)
	snapshot.load()
	if _, denied, ok := snapshot.Denied(held); !ok || denied {
		t.Fatal("a lift did not reach the set")
	}

	// a set past MaxAge, every load since having failed, answers not loaded
	source.set(nil, errors.New("the store went away"))
	clock.Advance(DefaultAbuseHardDenySettings().MaxAge)
	snapshot.load()
	if _, _, ok := snapshot.Denied(other); ok || snapshot.Ready() {
		t.Fatal("a set past MaxAge answered")
	}
}

// An unloaded network answers ok false, never a pass, and asks for one load however often it is read; once loaded,
// its standing and network_verified read back.
func TestAbuseGateUnloadedRefusesThenLoads(t *testing.T) {
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(t, &testAbuseHardDenySource{}, clock)
	load := newTestAbuseGateLoad()
	networkId := server.NewId()
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, true))
	// hold the loader inside its first load, so every read below finds the network asked and not loaded
	release := load.holdLoads()
	gate, loads := testAbuseGate(t, snapshot, load, clock, nil)

	for range 10 {
		if standing, ok := gate.Standing(connect.Id(networkId)); ok {
			t.Fatalf("an unloaded network passed as %q", standing)
		}
		if verified, ok := gate.Verified(connect.Id(networkId)); ok || verified {
			t.Fatal("an unloaded network passed as verified")
		}
	}
	release()
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	if count := load.loadCount(networkId); count != 1 {
		t.Fatalf("%d loads for one network's misses", count)
	}
	if standing, ok := gate.Standing(connect.Id(networkId)); !ok || standing != AbuseStandingStandard {
		t.Fatalf("loaded: %q %t", standing, ok)
	}
	if verified, ok := gate.Verified(connect.Id(networkId)); !ok || !verified {
		t.Fatalf("loaded verification: %t %t", verified, ok)
	}
	if cachedCount, loadedCount := gate.Stats(); cachedCount != 1 || loadedCount != 1 {
		t.Fatalf("stats: %d cached, %d loaded", cachedCount, loadedCount)
	}
}

// Probation's end and a restriction's end take effect at the moment asked, with no load.
func TestAbuseGateDerivesAtTheMomentAsked(t *testing.T) {
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(t, &testAbuseHardDenySource{}, clock)
	// the set stays in force however far the clock moves, so a change below is the derivation's
	snapshot.settings.MaxAge = 0
	load := newTestAbuseGateLoad()
	young, restricted := server.NewId(), server.NewId()
	now := clock.Now()
	load.put(young, testAbuseFacts(now, 6*testAbuseDay, nil, false))
	until := now.Add(2 * testAbuseDay)
	load.put(restricted, testAbuseFacts(now, 60*testAbuseDay, &networkAbuseRow{state: networkAbuseStateRestricted, caseId: server.NewId(), untilTime: &until}, true))
	gate, loads := testAbuseGate(t, snapshot, load, clock, func(settings *AbuseGateSettings) {
		settings.RefreshAfter = 10 * testAbuseDay
		settings.MaxAge = 0
	})
	gate.Standing(connect.Id(young))
	gate.Standing(connect.Id(restricted))
	if err := testAwaitAbuseGateLoad(t, loads, young, restricted); err != nil {
		t.Fatal(err)
	}
	if standing, ok := gate.Standing(connect.Id(young)); !ok || standing != AbuseStandingProbation {
		t.Fatalf("6 days old: %q %t", standing, ok)
	}
	if standing, ok := gate.Standing(connect.Id(restricted)); !ok || standing != AbuseStandingRestricted {
		t.Fatalf("restricted: %q %t", standing, ok)
	}
	clock.Advance(2*testAbuseDay + time.Second)
	if standing, ok := gate.Standing(connect.Id(young)); !ok || standing != AbuseStandingStandard {
		t.Fatalf("8 days old: %q %t", standing, ok)
	}
	if standing, ok := gate.Standing(connect.Id(restricted)); !ok || standing != AbuseStandingStandard {
		t.Fatalf("past the restriction: %q %t", standing, ok)
	}
	if load.loadCount(young) != 1 || load.loadCount(restricted) != 1 {
		t.Fatal("a derivation asked for a load")
	}
}

// The set holds a suspension within its interval, with no entry and over a cached one; a state this build does not
// know is held too.
func TestAbuseGateHeldBySet(t *testing.T) {
	clock := newTestAbuseClock()
	held, unknown, free := server.NewId(), server.NewId(), server.NewId()
	source := &testAbuseHardDenySource{networkIdStandings: map[server.Id]AbuseStanding{}}
	snapshot := testAbuseHardDenySnapshot(t, source, clock)
	load := newTestAbuseGateLoad()
	load.put(held, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, true))
	load.put(free, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, true))
	gate, loads := testAbuseGate(t, snapshot, load, clock, nil)
	gate.Standing(connect.Id(held))
	gate.Standing(connect.Id(free))
	if err := testAwaitAbuseGateLoad(t, loads, held, free); err != nil {
		t.Fatal(err)
	}
	if standing, ok := gate.Standing(connect.Id(held)); !ok || standing != AbuseStandingStandard {
		t.Fatalf("before the suspension: %q %t", standing, ok)
	}

	source.set(map[server.Id]AbuseStanding{held: AbuseStandingSuspended, unknown: AbuseStanding("banished")}, nil)
	clock.Advance(5 * time.Second)
	snapshot.load()
	if standing, ok := gate.Standing(connect.Id(held)); !ok || standing != AbuseStandingSuspended {
		t.Fatalf("suspended over a cached entry: %q %t", standing, ok)
	}
	if standing, ok := gate.Standing(connect.Id(unknown)); !ok || standing.Known() {
		t.Fatalf("an unknown state: %q %t", standing, ok)
	}
	if standing, ok := gate.Standing(connect.Id(free)); !ok || standing != AbuseStandingStandard {
		t.Fatalf("another network: %q %t", standing, ok)
	}
	// verification still needs the entry, which a held network keeps
	if verified, ok := gate.Verified(connect.Id(held)); !ok || !verified {
		t.Fatalf("a held network's verification: %t %t", verified, ok)
	}
}

// An entry cached while its network was suspended cannot tell what followed the lift, so once a newer set no longer
// holds the network it answers ok false until it reloads; held after the set, it answers held.
func TestAbuseGateHeldEntryAfterTheLift(t *testing.T) {
	clock := newTestAbuseClock()
	networkId := server.NewId()
	source := &testAbuseHardDenySource{networkIdStandings: map[server.Id]AbuseStanding{}}
	snapshot := testAbuseHardDenySnapshot(t, source, clock)
	load := newTestAbuseGateLoad()
	suspendedRow := &networkAbuseRow{state: networkAbuseStateSuspended, caseId: server.NewId()}
	clock.Advance(time.Second)
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, suspendedRow, true))
	gate, loads := testAbuseGate(t, snapshot, load, clock, nil)
	gate.Standing(connect.Id(networkId))
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	// suspended after the set loaded: refused as held
	if standing, ok := gate.Standing(connect.Id(networkId)); !ok || standing != AbuseStandingSuspended {
		t.Fatalf("held after the set: %q %t", standing, ok)
	}

	// the lift: a newer set without the network, and a row restricted until later
	until := clock.Now().Add(20 * testAbuseDay)
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, &networkAbuseRow{state: networkAbuseStateRestricted, caseId: suspendedRow.caseId, untilTime: &until}, true))
	clock.Advance(time.Second)
	snapshot.load()
	if standing, ok := gate.Standing(connect.Id(networkId)); ok {
		t.Fatalf("a stale held entry answered %q", standing)
	}
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	if standing, ok := gate.Standing(connect.Id(networkId)); !ok || standing != AbuseStandingRestricted {
		t.Fatalf("after the reload: %q %t", standing, ok)
	}
}

// Past RefreshAfter an entry still answers and asks for a refresh; past MaxAge it answers ok false.
func TestAbuseGateRefreshAndMaxAge(t *testing.T) {
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(t, &testAbuseHardDenySource{}, clock)
	snapshot.settings.MaxAge = 0
	load := newTestAbuseGateLoad()
	networkId := server.NewId()
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, false))
	gate, loads := testAbuseGate(t, snapshot, load, clock, nil)
	gate.Verified(connect.Id(networkId))
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}

	// paid since: past RefreshAfter the old answer serves once more, and the refresh brings the new one
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, true))
	clock.Advance(gate.settings.RefreshAfter)
	if verified, ok := gate.Verified(connect.Id(networkId)); !ok || verified {
		t.Fatalf("past RefreshAfter: %t %t", verified, ok)
	}
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	if verified, ok := gate.Verified(connect.Id(networkId)); !ok || !verified {
		t.Fatalf("refreshed: %t %t", verified, ok)
	}

	// a loader that keeps failing: past MaxAge the entry stops answering
	load.fail(errors.New("the store went away"))
	clock.Advance(gate.settings.MaxAge)
	if _, ok := gate.Verified(connect.Id(networkId)); ok {
		t.Fatal("an entry past MaxAge answered")
	}
	if _, ok := gate.Standing(connect.Id(networkId)); ok {
		t.Fatal("an entry past MaxAge answered its standing")
	}
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err == nil {
		t.Fatal("the failing load read cleanly")
	}
}

// A network the store does not have answers ok false, and is asked again after AbsentRefreshAfter.
func TestAbuseGateAbsentNetwork(t *testing.T) {
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(t, &testAbuseHardDenySource{}, clock)
	snapshot.settings.MaxAge = 0
	load := newTestAbuseGateLoad()
	networkId := server.NewId()
	gate, loads := testAbuseGate(t, snapshot, load, clock, nil)
	gate.Standing(connect.Id(networkId))
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	if _, ok := gate.Standing(connect.Id(networkId)); ok {
		t.Fatal("an absent network answered")
	}
	if _, ok := gate.Verified(connect.Id(networkId)); ok {
		t.Fatal("an absent network answered its verification")
	}
	if load.loadCount(networkId) != 1 {
		t.Fatal("an absent network inside AbsentRefreshAfter was asked again")
	}

	// created since
	load.put(networkId, testAbuseFacts(clock.Now(), time.Minute, nil, false))
	clock.Advance(gate.settings.AbsentRefreshAfter)
	gate.Standing(connect.Id(networkId))
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	if standing, ok := gate.Standing(connect.Id(networkId)); !ok || standing != AbuseStandingProbation {
		t.Fatalf("created: %q %t", standing, ok)
	}
}

// With no set in force nothing says a network is not held, so a cached network answers ok false.
func TestAbuseGateRefusesWithNoSet(t *testing.T) {
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(t, &testAbuseHardDenySource{err: errors.New("no store")}, clock)
	load := newTestAbuseGateLoad()
	networkId := server.NewId()
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, true))
	gate, loads := testAbuseGate(t, snapshot, load, clock, nil)
	gate.Standing(connect.Id(networkId))
	if err := testAwaitAbuseGateLoad(t, loads, networkId); err != nil {
		t.Fatal(err)
	}
	if standing, ok := gate.Standing(connect.Id(networkId)); ok {
		t.Fatalf("no set, and the gate answered %q", standing)
	}
	// verification does not read the set
	if verified, ok := gate.Verified(connect.Id(networkId)); !ok || !verified {
		t.Fatalf("verification: %t %t", verified, ok)
	}
}

// The cache holds at most MaxNetworkCount networks, dropping those unread within MaxAge first.
func TestAbuseGateBoundsItsCache(t *testing.T) {
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(t, &testAbuseHardDenySource{}, clock)
	snapshot.settings.MaxAge = 0
	load := newTestAbuseGateLoad()
	networkIds := []server.Id{}
	for range 6 {
		networkId := server.NewId()
		networkIds = append(networkIds, networkId)
		load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, nil, true))
	}
	gate, loads := testAbuseGate(t, snapshot, load, clock, func(settings *AbuseGateSettings) {
		settings.MaxNetworkCount = 4
		settings.RefreshAfter = 10 * testAbuseDay
		settings.MaxAge = time.Hour
	})
	for _, networkId := range networkIds[:4] {
		gate.Standing(connect.Id(networkId))
	}
	if err := testAwaitAbuseGateLoad(t, loads, networkIds[:4]...); err != nil {
		t.Fatal(err)
	}
	// the first two read again half an hour on, the other two left idle past MaxAge
	clock.Advance(30 * time.Minute)
	for _, networkId := range networkIds[:2] {
		if _, ok := gate.Verified(connect.Id(networkId)); !ok {
			t.Fatal("a cached network did not answer")
		}
	}
	clock.Advance(31 * time.Minute)
	for _, networkId := range networkIds[4:] {
		gate.Verified(connect.Id(networkId))
	}
	if err := testAwaitAbuseGateLoad(t, loads, networkIds[4:]...); err != nil {
		t.Fatal(err)
	}
	cached := func(networkId server.Id) bool {
		gate.stateLock.RLock()
		defer gate.stateLock.RUnlock()
		_, found := gate.networkIdEntries[networkId]
		return found
	}
	if cachedCount, _ := gate.Stats(); cachedCount != 4 {
		t.Fatalf("%d cached past the bound", cachedCount)
	}
	for index, networkId := range networkIds {
		if want := index < 2 || 4 <= index; cached(networkId) != want {
			t.Errorf("network %d cached %t", index, cached(networkId))
		}
	}
}

// A warm gate, its network loaded and its set in force: what the benchmark and the allocation check read.
func testWarmAbuseGate(tb testing.TB) (*NetworkAbuseGate, connect.Id) {
	tb.Helper()
	clock := newTestAbuseClock()
	snapshot := testAbuseHardDenySnapshot(tb, &testAbuseHardDenySource{networkIdStandings: map[server.Id]AbuseStanding{server.NewId(): AbuseStandingSuspended}}, clock)
	load := newTestAbuseGateLoad()
	networkId := server.NewId()
	until := clock.Now().Add(10 * testAbuseDay)
	load.put(networkId, testAbuseFacts(clock.Now(), 30*testAbuseDay, &networkAbuseRow{state: networkAbuseStateRestricted, caseId: server.NewId(), untilTime: &until}, true))
	gate, loads := testAbuseGate(tb, snapshot, load, clock, nil)
	gate.Standing(connect.Id(networkId))
	if err := testAwaitAbuseGateLoad(tb, loads, networkId); err != nil {
		tb.Fatal(err)
	}
	if standing, ok := gate.Standing(connect.Id(networkId)); !ok || standing != AbuseStandingRestricted {
		tb.Fatalf("warm: %q %t", standing, ok)
	}
	return gate, connect.Id(networkId)
}

// The inline read allocates nothing (§22.5's gate, R-TEST-2).
func TestAbuseGateAllocatesNothing(t *testing.T) {
	gate, networkId := testWarmAbuseGate(t)
	var abuse sidecar.AbuseGate = gate
	allocs := testing.AllocsPerRun(1000, func() {
		abuse.Standing(networkId)
		abuse.Verified(networkId)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations a read", allocs)
	}
}

// §22.5's gate: < 1 us and 0 allocations for a warm Standing and Verified.
func BenchmarkAbuseGate(b *testing.B) {
	gate, networkId := testWarmAbuseGate(b)
	var abuse sidecar.AbuseGate = gate
	b.ReportAllocs()
	for b.Loop() {
		abuse.Standing(networkId)
		abuse.Verified(networkId)
	}
}
