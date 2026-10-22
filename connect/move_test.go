// The mover's loop (move.go), tick by tick over fakes of the Redis move
// model, the residents, the inventory, the executor, the breaker and the
// database wait: balance moves within the share, dry run, the kill switch
// through the control, the database guard, emergency shedding after the
// required ticks, evacuation, arrival-time moves (off by default), the budget
// caps, the cooldown and the failed-send withdrawal. The control narrows
// through the real model function, so the kill switch is the one Redis
// carries.
package connect

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One reservation the fake budget answered.
type moveTestReservation struct {
	sourceHost string
	targetHost string
	refusedCap string
}

// A fake of the Redis move model. The budget counts within the test, with no
// windows, and refuses at the caps it is given like the script.
type moveTestStore struct {
	stateLock sync.Mutex

	controlFields map[string]string
	controlErr    error
	controlReads  int

	loads    []*model.ConnectMoveLoad
	nodeTime time.Time
	loadsErr error

	staleCleanupTimeouts []time.Duration

	fleetCount   int
	sourceCounts map[string]int
	targetCounts map[string]int
	budgetErr    error
	reservations []moveTestReservation

	// client id -> the move id of its cooldown
	cooldowns      map[server.Id]server.Id
	cooldownTtls   map[server.Id]time.Duration
	cooldownReads  [][]server.Id
	setCooldownErr error
}

// An empty store: no control, no table, an unspent budget, no cooldowns.
func newMoveTestStore() *moveTestStore {
	return &moveTestStore{
		controlFields: map[string]string{},
		sourceCounts:  map[string]int{},
		targetCounts:  map[string]int{},
		cooldowns:     map[server.Id]server.Id{},
		cooldownTtls:  map[server.Id]time.Duration{},
	}
}

// The scripted control, narrowed by the real model function.
func (self *moveTestStore) getControl(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.controlReads += 1
	if self.controlErr != nil {
		return nil, self.controlErr
	}
	return model.NarrowConnectMoveControl(self.controlFields, ceiling), nil
}

// The scripted table and node clock.
func (self *moveTestStore) getLoads(ctx context.Context) ([]*model.ConnectMoveLoad, time.Time, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.loadsErr != nil {
		return nil, time.Time{}, self.loadsErr
	}
	return self.loads, self.nodeTime, nil
}

// Records the cleanup timeout; removes nothing.
func (self *moveTestStore) removeStaleLoads(ctx context.Context, loads []*model.ConnectMoveLoad, nodeTime time.Time, cleanupTimeout time.Duration) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.staleCleanupTimeouts = append(self.staleCleanupTimeouts, cleanupTimeout)
	return 0, nil
}

// Refuses at the first spent cap, in the script's order, and records the
// answer.
func (self *moveTestStore) reserveBudget(ctx context.Context, sourceHost string, targetHost string, caps model.ConnectMoveBudgetCaps) (string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.budgetErr != nil {
		return "", self.budgetErr
	}
	refusedCap := ""
	switch {
	case caps.Fleet <= self.fleetCount:
		refusedCap = model.ConnectMoveBudgetCapFleet
	case caps.Source <= self.sourceCounts[sourceHost]:
		refusedCap = model.ConnectMoveBudgetCapSource
	case caps.Target <= self.targetCounts[targetHost]:
		refusedCap = model.ConnectMoveBudgetCapTarget
	default:
		self.fleetCount += 1
		self.sourceCounts[sourceHost] += 1
		self.targetCounts[targetHost] += 1
	}
	self.reservations = append(self.reservations, moveTestReservation{
		sourceHost: sourceHost,
		targetHost: targetHost,
		refusedCap: refusedCap,
	})
	return refusedCap, nil
}

// The cooling clients of clientIds. Records the read.
func (self *moveTestStore) getCooldowns(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cooldownReads = append(self.cooldownReads, slices.Clone(clientIds))
	cooling := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if _, ok := self.cooldowns[clientId]; ok {
			cooling[clientId] = true
		}
	}
	return cooling, nil
}

// Starts the client's cooldown, or fails as scripted.
func (self *moveTestStore) setCooldown(ctx context.Context, clientId server.Id, moveId server.Id, ttl time.Duration) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.setCooldownErr != nil {
		return self.setCooldownErr
	}
	self.cooldowns[clientId] = moveId
	self.cooldownTtls[clientId] = ttl
	return nil
}

// Ends the client's cooldown if moveId set it.
func (self *moveTestStore) removeCooldown(ctx context.Context, clientId server.Id, moveId server.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.cooldowns[clientId] == moveId {
		delete(self.cooldowns, clientId)
		delete(self.cooldownTtls, clientId)
	}
	return nil
}

// Replaces the control the next tick reads.
func (self *moveTestStore) setControl(fields map[string]string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.controlFields = fields
}

type moveTestResidents struct {
	stateLock  sync.Mutex
	candidates []*MoveCandidate
}

// A copy of the scripted residents.
func (self *moveTestResidents) MoveCandidates() []*MoveCandidate {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.candidates)
}

type moveTestLoadSource struct {
	stateLock sync.Mutex
	counts    MoveLoadCounts
}

// The scripted counts.
func (self *moveTestLoadSource) MoveLoadCounts() MoveLoadCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counts
}

type moveTestInventory struct {
	stateLock sync.Mutex
	inventory map[string]MoveInventoryHost
	err       error
	loadCount int
}

// The scripted inventory, or error. Counts the loads.
func (self *moveTestInventory) LoadMoveInventory(ctx context.Context) (map[string]MoveInventoryHost, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loadCount += 1
	if self.err != nil {
		return nil, self.err
	}
	return self.inventory, nil
}

type moveTestExecutor struct {
	stateLock sync.Mutex
	requests  []*MoveRequest
	// clients whose sends fail
	failClientIds map[server.Id]bool
	// receives every request without blocking, when set
	sent chan *MoveRequest
}

// Records the request, or fails for a scripted client.
func (self *moveTestExecutor) SendMove(ctx context.Context, request *MoveRequest) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failClientIds[request.ClientId] {
		return errors.New("resident is no longer here")
	}
	self.requests = append(self.requests, request)
	if self.sent != nil {
		select {
		case self.sent <- request:
		default:
		}
	}
	return nil
}

// The requests sent so far.
func (self *moveTestExecutor) sentRequests() []*MoveRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

type moveTestBreaker struct {
	stateLock sync.Mutex
	open      map[netip.Addr]bool
	queries   []netip.Addr
}

// Whether the scripted breaker of the address is open. Records the query.
func (self *moveTestBreaker) TargetBreakerOpen(ctx context.Context, address netip.Addr) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.queries = append(self.queries, address)
	return self.open[address]
}

type moveTestDbWait struct {
	stateLock sync.Mutex
	wait      time.Duration
}

// The scripted wait.
func (self *moveTestDbWait) PoolAcquireWait() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.wait
}

// A mover of one process on edge-hot.example over fakes, and the fakes.
type moveTestFixture struct {
	settings   *ExchangeSettings
	identity   MoveProcessIdentity
	store      *moveTestStore
	residents  *moveTestResidents
	loadSource *moveTestLoadSource
	inventory  *moveTestInventory
	executor   *moveTestExecutor
	breaker    *moveTestBreaker
	dbWait     *moveTestDbWait
	now        time.Time
	mover      *Mover
}

const (
	moveTestOwnHost  = "edge-hot.example"
	moveTestCoolHost = "edge-cool.example"
)

// The fleet: this process alone on edge-hot with ownResidents, and coolHost
// with coolResidents, 10 cores each. The mover is on unless a test changes
// the control.
func newMoveTestFixture(t testing.TB, ownResidents int, coolResidents int) *moveTestFixture {
	t.Helper()
	settings := DefaultExchangeSettings()
	fixture := &moveTestFixture{
		settings: settings,
		identity: MoveProcessIdentity{
			Host:       moveTestOwnHost,
			Block:      "g1",
			ExchangeId: server.NewId(),
		},
		store:      newMoveTestStore(),
		residents:  &moveTestResidents{},
		loadSource: &moveTestLoadSource{},
		inventory: &moveTestInventory{inventory: map[string]MoveInventoryHost{
			moveTestOwnHost:  moveTestInventoryHost("192.0.2.10", "2001:db8::10"),
			moveTestCoolHost: moveTestInventoryHost("192.0.2.20", "2001:db8::20"),
		}},
		executor: &moveTestExecutor{failClientIds: map[server.Id]bool{}},
		breaker:  &moveTestBreaker{open: map[netip.Addr]bool{}},
		dbWait:   &moveTestDbWait{},
		now:      moveTestNodeTime,
	}
	fixture.store.nodeTime = moveTestNodeTime
	own := moveTestLoad(moveTestOwnHost, "g1", ownResidents, 10, 0.6)
	own.ExchangeId = fixture.identity.ExchangeId
	fixture.store.loads = []*model.ConnectMoveLoad{
		own,
		moveTestLoad(moveTestCoolHost, "g1", coolResidents, 10, 0.3),
	}
	fixture.store.controlFields = map[string]string{model.ConnectMoveControlFieldMode: "on"}
	fixture.mover = newMoverWithStore(
		context.Background(),
		fixture.identity,
		settings,
		&MoverDependencies{
			Residents:     fixture.residents,
			LoadSource:    fixture.loadSource,
			Inventory:     fixture.inventory,
			Executor:      fixture.executor,
			TargetBreaker: fixture.breaker,
			DbWait:        fixture.dbWait,
		},
		fixture.store,
		moveTestRandom(),
		func() time.Time { return fixture.now },
	)
	t.Cleanup(fixture.mover.Close)
	return fixture
}

// An idle, capable, dual-stack resident on a direct carrier.
func (self *moveTestFixture) idleCandidate() *MoveCandidate {
	return &MoveCandidate{
		ClientId:          server.NewId(),
		ResidentStartTime: self.now.Add(-time.Hour),
		LastPayloadTime:   self.now.Add(-30 * time.Minute),
		MoveVersion:       1,
		DirectCarrier:     true,
		Ipv4:              true,
		Ipv6:              true,
	}
}

// Adds count idle candidates and returns them.
func (self *moveTestFixture) addIdleCandidates(count int) []*MoveCandidate {
	candidates := []*MoveCandidate{}
	for i := 0; i < count; i += 1 {
		candidates = append(candidates, self.idleCandidate())
	}
	self.residents.stateLock.Lock()
	defer self.residents.stateLock.Unlock()
	self.residents.candidates = append(self.residents.candidates, candidates...)
	return candidates
}

// Sets the own host's CPU in its record.
func (self *moveTestFixture) setOwnCpu(cpuBusy float64) {
	self.store.stateLock.Lock()
	defer self.store.stateLock.Unlock()
	for _, load := range self.store.loads {
		if load.ExchangeId == self.identity.ExchangeId {
			load.HostCpuBusy = cpuBusy
		}
	}
}

// The counter value now, for deltas across a test.
func moveTestSent(kind string, trigger string, targetHost string) float64 {
	return testutil.ToFloat64(movesSentCounter.WithLabelValues(kind, trigger, moveTestOwnHost, targetHost))
}

// The would-send counter value now, for deltas across a test.
func moveTestWouldSend(kind string, trigger string, targetHost string) float64 {
	return testutil.ToFloat64(movesWouldSendCounter.WithLabelValues(kind, trigger, moveTestOwnHost, targetHost))
}

// Above the band, the process sheds its share of the host's excess, capped by
// MoveMaxPerTick, as advisory moves of balance candidates to the target below
// the mean, one address per family, migrate times jittered over the tick.
// Each move reserves the budget, then starts the client's cooldown with the
// move's id, then sends.
func TestMoverBalanceSendsAdvisoryMoves(t *testing.T) {
	// mean 60, band edge 66: edge-hot at 100 sheds 340, capped at 10
	fixture := newMoveTestFixture(t, 1000, 200)
	eligible := fixture.addIdleCandidates(20)
	young := fixture.idleCandidate()
	young.ResidentStartTime = fixture.now.Add(-time.Minute)
	legacy := fixture.idleCandidate()
	legacy.MoveVersion = 0
	streaming := fixture.idleCandidate()
	streaming.OpenStreamCount = 2
	overDns := fixture.idleCandidate()
	overDns.DirectCarrier = false
	arrivedByMove := fixture.idleCandidate()
	arrivedByMove.ArrivedByMove = true
	outstanding := fixture.idleCandidate()
	outstanding.MoveOutstanding = true
	fixture.residents.candidates = append(fixture.residents.candidates, young, legacy, streaming, overDns, arrivedByMove, outstanding)

	sentBefore := moveTestSent(moveKindAdvisory, moveTriggerBalance, moveTestCoolHost)
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	requests := fixture.executor.sentRequests()
	if len(requests) != fixture.settings.MoveMaxPerTick {
		t.Fatalf("sent %d moves, want MoveMaxPerTick %d", len(requests), fixture.settings.MoveMaxPerTick)
	}
	if delta := moveTestSent(moveKindAdvisory, moveTriggerBalance, moveTestCoolHost) - sentBefore; delta != 10 {
		t.Errorf("moves sent counter rose %v", delta)
	}
	seen := map[server.Id]bool{}
	for _, request := range requests {
		if !slices.ContainsFunc(eligible, func(candidate *MoveCandidate) bool { return candidate.ClientId == request.ClientId }) {
			t.Errorf("moved an ineligible client %s", request.ClientId)
		}
		if seen[request.ClientId] {
			t.Errorf("moved %s twice", request.ClientId)
		}
		seen[request.ClientId] = true
		if request.Kind != protocol.ResidentMoveKind_ResidentMoveKindAdvisory || request.TargetHost != moveTestCoolHost {
			t.Errorf("request %+v", request)
		}
		if !slices.Equal(request.Targets, []netip.Addr{netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr("2001:db8::20")}) {
			t.Errorf("targets = %v", request.Targets)
		}
		if request.MigrateTime.Before(fixture.now) || !request.MigrateTime.Before(fixture.now.Add(fixture.settings.MoveTickInterval)) {
			t.Errorf("migrate time %s is not within the tick", request.MigrateTime)
		}
		if fixture.store.cooldowns[request.ClientId] != request.MoveId ||
			fixture.store.cooldownTtls[request.ClientId] != fixture.settings.MoveClientCooldown {
			t.Errorf("client %s cooldown is not its move's", request.ClientId)
		}
	}
	if len(fixture.store.reservations) != 10 || fixture.store.reservations[0].sourceHost != moveTestOwnHost ||
		fixture.store.reservations[0].targetHost != moveTestCoolHost {
		t.Errorf("reservations = %+v", fixture.store.reservations)
	}
	// one bounded cooldown read of at most MoveMaxPerTick candidates
	if len(fixture.store.cooldownReads) != 1 || len(fixture.store.cooldownReads[0]) != 10 {
		t.Errorf("cooldown reads = %d", len(fixture.store.cooldownReads))
	}
	// the stale cleanup ran with its timeout
	if !slices.Equal(fixture.store.staleCleanupTimeouts, []time.Duration{10 * time.Minute}) {
		t.Errorf("stale cleanup timeouts = %v", fixture.store.staleCleanupTimeouts)
	}
	// the breaker was asked once per target address
	if len(fixture.breaker.queries) != 2 {
		t.Errorf("breaker queries = %v", fixture.breaker.queries)
	}
}

// A share below MoveMaxPerTick is all a process sends, and a host in the band
// sends nothing.
func TestMoverSendsOnlyItsShare(t *testing.T) {
	// mean 50.5, band edge 55.55: edge-hot at 56 holds 4.5 above it
	fixture := newMoveTestFixture(t, 560, 450)
	fixture.addIdleCandidates(20)
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	if count := len(fixture.executor.sentRequests()); count != 4 && count != 5 {
		t.Fatalf("sent %d moves for a share of 4.5", count)
	}

	inBand := newMoveTestFixture(t, 540, 460)
	inBand.addIdleCandidates(20)
	if outcome := inBand.mover.tick(); outcome != moverTickInBand {
		t.Fatalf("in-band outcome = %s", outcome)
	}
	if len(inBand.executor.sentRequests()) != 0 || len(inBand.store.reservations) != 0 {
		t.Fatal("a host in the band moved clients")
	}
}

// Dry run does everything but the cooldown and the send: it reserves the
// budget and counts the moves it would send by source and target.
func TestMoverDryRunCountsWithoutSending(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.addIdleCandidates(20)
	fixture.store.setControl(map[string]string{model.ConnectMoveControlFieldMode: "dry_run"})

	wouldSendBefore := moveTestWouldSend(moveKindAdvisory, moveTriggerBalance, moveTestCoolHost)
	sentBefore := moveTestSent(moveKindAdvisory, moveTriggerBalance, moveTestCoolHost)
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != 0 {
		t.Fatal("dry run sent moves")
	}
	if len(fixture.store.cooldowns) != 0 {
		t.Fatal("dry run started cooldowns")
	}
	if len(fixture.store.reservations) != 10 {
		t.Fatalf("dry run reserved %d", len(fixture.store.reservations))
	}
	if delta := moveTestWouldSend(moveKindAdvisory, moveTriggerBalance, moveTestCoolHost) - wouldSendBefore; delta != 10 {
		t.Errorf("would-send counter rose %v", delta)
	}
	if delta := moveTestSent(moveKindAdvisory, moveTriggerBalance, moveTestCoolHost) - sentBefore; delta != 0 {
		t.Errorf("sent counter rose %v in dry run", delta)
	}
	if mode := testutil.ToFloat64(moverModeGauge); mode != float64(model.ConnectMoveModeDryRun) {
		t.Errorf("mode gauge = %v", mode)
	}
}

// The Redis control stops the mover at its next tick without a deploy, an
// unreadable control is off, and EnableMove off holds it off whatever the
// control says (D14).
func TestMoverKillSwitchViaControl(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.addIdleCandidates(40)
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	sentCount := len(fixture.executor.sentRequests())
	reservationCount := len(fixture.store.reservations)

	fixture.store.setControl(map[string]string{model.ConnectMoveControlFieldMode: "off"})
	if outcome := fixture.mover.tick(); outcome != moverTickOff {
		t.Fatalf("killed outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != sentCount || len(fixture.store.reservations) != reservationCount {
		t.Fatal("the mover moved clients after the kill switch")
	}
	if mode := testutil.ToFloat64(moverModeGauge); mode != float64(model.ConnectMoveModeOff) {
		t.Errorf("mode gauge = %v", mode)
	}

	// an absent control is the code default, off
	fixture.store.setControl(map[string]string{})
	if outcome := fixture.mover.tick(); outcome != moverTickOff {
		t.Fatalf("absent control outcome = %s", outcome)
	}

	// an unreadable control fails closed
	fixture.store.setControl(map[string]string{model.ConnectMoveControlFieldMode: "on"})
	fixture.store.controlErr = errors.New("redis: connection refused")
	if outcome := fixture.mover.tick(); outcome != moverTickStoreError {
		t.Fatalf("unreadable control outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != sentCount {
		t.Fatal("the mover moved clients without its control")
	}
	if mode := testutil.ToFloat64(moverModeGauge); mode != float64(model.ConnectMoveModeOff) {
		t.Errorf("mode gauge = %v with an unreadable control", mode)
	}

	// the code ceiling holds whatever the control says
	fixture.store.controlErr = nil
	fixture.settings.EnableMove = false
	if outcome := fixture.mover.tick(); outcome != moverTickOff {
		t.Fatalf("EnableMove off outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != sentCount {
		t.Fatal("the mover moved clients past EnableMove")
	}
}

// A process whose Postgres pool acquire wait is above the threshold skips the
// tick before it reserves anything; a non-positive threshold disables the
// guard.
func TestMoverDbGuardPausesTheTick(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.addIdleCandidates(40)

	fixture.dbWait.wait = 80 * time.Millisecond
	pausedBefore := testutil.ToFloat64(moverTicksCounter.WithLabelValues(moverTickDbPaused))
	if outcome := fixture.mover.tick(); outcome != moverTickDbPaused {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != 0 || len(fixture.store.reservations) != 0 || len(fixture.store.cooldownReads) != 0 {
		t.Fatal("a paused tick did work")
	}
	if delta := testutil.ToFloat64(moverTicksCounter.WithLabelValues(moverTickDbPaused)) - pausedBefore; delta != 1 {
		t.Errorf("paused ticks rose %v", delta)
	}

	// the wait at the threshold is not above it
	fixture.dbWait.wait = 50 * time.Millisecond
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome at the threshold = %s", outcome)
	}

	fixture.dbWait.wait = time.Second
	fixture.settings.MoveDbWaitPauseThreshold = 0
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome without the guard = %s", outcome)
	}
}

// A host whose CPU stays at or above MoveEmergencyCpu sheds idle clients with
// compulsory moves from the MoveEmergencyTicks-th consecutive tick, even
// inside the band; a tick below the threshold starts the count again.
func TestMoverEmergencyShedsAfterConsecutiveTicks(t *testing.T) {
	// mean 50, band edge 55: edge-hot at 52 is in the band
	fixture := newMoveTestFixture(t, 520, 480)
	fixture.addIdleCandidates(30)
	fixture.setOwnCpu(0.96)

	for tick := 1; tick < fixture.settings.MoveEmergencyTicks; tick += 1 {
		if outcome := fixture.mover.tick(); outcome != moverTickInBand {
			t.Fatalf("tick %d outcome = %s", tick, outcome)
		}
	}
	if len(fixture.executor.sentRequests()) != 0 {
		t.Fatal("shed before the required ticks")
	}
	sentBefore := moveTestSent(moveKindCompulsory, moveTriggerEmergency, moveTestCoolHost)
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("tick %d outcome = %s", fixture.settings.MoveEmergencyTicks, outcome)
	}
	requests := fixture.executor.sentRequests()
	if len(requests) != fixture.settings.MoveMaxPerTick {
		t.Fatalf("shed %d", len(requests))
	}
	for _, request := range requests {
		if request.Kind != protocol.ResidentMoveKind_ResidentMoveKindCompulsory || request.TargetHost != moveTestCoolHost {
			t.Errorf("request %+v", request)
		}
	}
	if delta := moveTestSent(moveKindCompulsory, moveTriggerEmergency, moveTestCoolHost) - sentBefore; delta != 10 {
		t.Errorf("emergency counter rose %v", delta)
	}

	// a tick below the threshold resets the count
	fixture.setOwnCpu(0.5)
	if outcome := fixture.mover.tick(); outcome != moverTickInBand {
		t.Fatalf("outcome below the threshold = %s", outcome)
	}
	fixture.setOwnCpu(0.96)
	if outcome := fixture.mover.tick(); outcome != moverTickInBand {
		t.Fatalf("the count did not start again: %s", outcome)
	}
	// an unknown CPU is below the threshold
	fixture.setOwnCpu(model.ConnectMoveLoadCpuUnknown)
	if outcome := fixture.mover.tick(); outcome != moverTickInBand {
		t.Fatalf("outcome with unknown CPU = %s", outcome)
	}
}

// The emergency count itself, including the disabled settings.
func TestMoveEmergencyCount(t *testing.T) {
	emergency := &moveEmergency{settings: &ExchangeMoverSettings{MoveEmergencyCpu: 0.95, MoveEmergencyTicks: 3}}
	answers := []bool{}
	for _, cpu := range []float64{0.95, 0.99, 0.97, 0.96, 0.5, 0.96, 0.96, 0.96} {
		answers = append(answers, emergency.ObserveHostCpu(cpu))
	}
	if !slices.Equal(answers, []bool{false, false, true, true, false, false, false, true}) {
		t.Errorf("answers = %v", answers)
	}
	for _, disabled := range []*ExchangeMoverSettings{
		{MoveEmergencyCpu: 0, MoveEmergencyTicks: 3},
		{MoveEmergencyCpu: -1, MoveEmergencyTicks: 3},
		{MoveEmergencyCpu: 0.95, MoveEmergencyTicks: 0},
	} {
		emergency := &moveEmergency{settings: disabled}
		for i := 0; i < 5; i += 1 {
			if emergency.ObserveHostCpu(1) {
				t.Errorf("disabled %+v shed", disabled)
			}
		}
	}
}

// A host listed in evacuateHosts moves every resident with a compulsory move,
// legacy, active, DNS-carried ones and arrivals by intent too, the longest
// payload-idle first, whatever their cooldown, MoveMaxPerTick a tick; only a
// resident with a move outstanding waits. A client with no family the target
// offers still gets the move and migrates by name.
func TestMoverEvacuationMovesEveryResident(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.settings.MoveMaxPerTick = 11
	fixture.inventory.inventory[moveTestCoolHost] = moveTestInventoryHost("192.0.2.20", "")
	candidates := fixture.addIdleCandidates(5)
	legacy := fixture.idleCandidate()
	legacy.MoveVersion = 0
	legacy.LastPayloadTime = fixture.now.Add(-50 * time.Minute)
	active := fixture.idleCandidate()
	active.LastPayloadTime = fixture.now.Add(-time.Second)
	overDns := fixture.idleCandidate()
	overDns.DirectCarrier = false
	cooling := fixture.idleCandidate()
	cooling.LastPayloadTime = fixture.now.Add(-40 * time.Minute)
	ipv6Only := fixture.idleCandidate()
	ipv6Only.Ipv4 = false
	ipv6Only.LastPayloadTime = fixture.now.Add(-45 * time.Minute)
	young := fixture.idleCandidate()
	young.ResidentStartTime = fixture.now.Add(-time.Minute)
	young.LastPayloadTime = time.Time{}
	arrivedByMove := fixture.idleCandidate()
	arrivedByMove.ArrivedByMove = true
	arrivedByMove.LastPayloadTime = fixture.now.Add(-55 * time.Minute)
	outstanding := fixture.idleCandidate()
	outstanding.MoveOutstanding = true
	outstanding.LastPayloadTime = fixture.now.Add(-2 * time.Hour)
	candidates = append(candidates, legacy, active, overDns, cooling, ipv6Only, young, arrivedByMove)
	fixture.residents.candidates = append(slices.Clone(candidates), outstanding)
	fixture.store.cooldowns[cooling.ClientId] = server.NewId()
	fixture.store.setControl(map[string]string{
		model.ConnectMoveControlFieldMode:          "on",
		model.ConnectMoveControlFieldEvacuateHosts: moveTestOwnHost,
	})

	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	requests := fixture.executor.sentRequests()
	if len(requests) != fixture.settings.MoveMaxPerTick {
		t.Fatalf("moved %d", len(requests))
	}
	want := evacuationMoveOrder(candidates)[:fixture.settings.MoveMaxPerTick]
	for i, request := range requests {
		if request.ClientId != want[i].ClientId {
			t.Errorf("move %d is not the next longest idle resident", i)
		}
		if request.Kind != protocol.ResidentMoveKind_ResidentMoveKindCompulsory || request.TargetHost != moveTestCoolHost ||
			!slices.Equal(request.Targets, []netip.Addr{netip.MustParseAddr("192.0.2.20")}) {
			t.Errorf("request %+v", request)
		}
	}
	movedIds := map[server.Id]bool{}
	for _, request := range requests {
		movedIds[request.ClientId] = true
	}
	for _, candidate := range []*MoveCandidate{legacy, overDns, cooling, ipv6Only, young, arrivedByMove} {
		if !movedIds[candidate.ClientId] {
			t.Errorf("evacuation skipped a resident %+v", candidate)
		}
	}
	if movedIds[active.ClientId] {
		t.Error("evacuation reached the active client before idle ones")
	}
	if movedIds[outstanding.ClientId] {
		t.Error("evacuation sent a second move to a resident with one outstanding")
	}
	if len(fixture.store.cooldownReads) != 0 {
		t.Error("evacuation read cooldowns")
	}
}

// Arrival-time moves are off by default (D9): a client that just arrived is
// not moved. On, an arrival that did not come by intent moves first, with the
// arrival trigger and without the age and idle tests; an arrival by intent is
// never moved again.
func TestMoverArrivalMovesAreOffByDefault(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	arrival := fixture.idleCandidate()
	arrival.ResidentStartTime = fixture.now.Add(-5 * time.Second)
	arrival.LastPayloadTime = fixture.now.Add(-time.Second)
	byIntent := fixture.idleCandidate()
	byIntent.ResidentStartTime = fixture.now.Add(-5 * time.Second)
	fixture.residents.candidates = []*MoveCandidate{arrival, byIntent}

	if fixture.settings.EnableArrivalMoves {
		t.Fatal("arrival moves are on by default")
	}
	if outcome := fixture.mover.tick(); outcome != moverTickNoCandidate {
		t.Fatalf("outcome = %s", outcome)
	}
	fixture.mover.OnResidentArrival(arrival.ClientId, false)
	if outcome := fixture.mover.tick(); outcome != moverTickNoCandidate {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != 0 {
		t.Fatal("an arrival moved with arrival moves off")
	}

	fixture.settings.EnableArrivalMoves = true
	// the last tick found the host a source, so arrivals are noted now
	fixture.mover.OnResidentArrival(arrival.ClientId, false)
	fixture.mover.OnResidentArrival(byIntent.ClientId, true)
	sentBefore := moveTestSent(moveKindAdvisory, moveTriggerArrival, moveTestCoolHost)
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	requests := fixture.executor.sentRequests()
	if len(requests) != 1 || requests[0].ClientId != arrival.ClientId ||
		requests[0].Kind != protocol.ResidentMoveKind_ResidentMoveKindAdvisory {
		t.Fatalf("requests = %+v", requests)
	}
	if delta := moveTestSent(moveKindAdvisory, moveTriggerArrival, moveTestCoolHost) - sentBefore; delta != 1 {
		t.Errorf("arrival counter rose %v", delta)
	}
	// an arrival is moved once: the next tick has none noted
	if outcome := fixture.mover.tick(); outcome != moverTickNoCandidate {
		t.Fatalf("outcome = %s", outcome)
	}
}

// Arrivals are noted only while the last tick found the host a source, at
// most MoveMaxPerTick, the most recent kept; a legacy or streaming arrival is
// not moved.
func TestMoverArrivalNotes(t *testing.T) {
	fixture := newMoveTestFixture(t, 100, 900)
	fixture.settings.EnableArrivalMoves = true
	clientId := server.NewId()
	fixture.mover.OnResidentArrival(clientId, false)
	if len(fixture.mover.arrivalClientIds) != 0 {
		t.Fatal("noted an arrival before any tick found a source")
	}

	source := newMoveTestFixture(t, 1000, 200)
	source.settings.EnableArrivalMoves = true
	source.settings.MoveMaxPerTick = 3
	source.mover.tick()
	clientIds := []server.Id{}
	for i := 0; i < 5; i += 1 {
		clientIds = append(clientIds, server.NewId())
		source.mover.OnResidentArrival(clientIds[i], false)
	}
	if !slices.Equal(source.mover.arrivalClientIds, clientIds[2:]) {
		t.Fatalf("noted %v, want the 3 most recent", source.mover.arrivalClientIds)
	}

	legacy := source.idleCandidate()
	legacy.MoveVersion = 0
	legacy.ResidentStartTime = source.now
	streaming := source.idleCandidate()
	streaming.OpenStreamCount = 1
	streaming.ResidentStartTime = source.now
	source.residents.candidates = []*MoveCandidate{legacy, streaming}
	source.mover.takeArrivals(true)
	source.mover.OnResidentArrival(legacy.ClientId, false)
	source.mover.OnResidentArrival(streaming.ClientId, false)
	if outcome := source.mover.tick(); outcome != moverTickNoCandidate {
		t.Fatalf("outcome = %s", outcome)
	}
}

// The fleet cap stops the tick, though another target remains; a target cap
// sends the rest to the other targets until each is spent. Each refusal is
// counted by its cap.
func TestMoverBudgetRefusals(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.store.loads = append(fixture.store.loads, moveTestLoad("edge-cool2.example", "g1", 200, 10, 0.3))
	fixture.inventory.inventory["edge-cool2.example"] = moveTestInventoryHost("192.0.2.30", "2001:db8::30")
	fixture.addIdleCandidates(20)
	fixture.store.setControl(map[string]string{
		model.ConnectMoveControlFieldMode:         "on",
		model.ConnectMoveControlFieldMaxPerMinute: "3",
	})
	fleetRefusedBefore := testutil.ToFloat64(moveBudgetRefusedCounter.WithLabelValues(model.ConnectMoveBudgetCapFleet))
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != 3 || len(fixture.store.reservations) != 4 {
		t.Fatalf("sent %d after %d reservations, want 3 after 4", len(fixture.executor.sentRequests()), len(fixture.store.reservations))
	}
	if delta := testutil.ToFloat64(moveBudgetRefusedCounter.WithLabelValues(model.ConnectMoveBudgetCapFleet)) - fleetRefusedBefore; delta != 1 {
		t.Errorf("fleet refusals rose %v", delta)
	}
	if outcome := fixture.mover.tick(); outcome != moverTickRefused {
		t.Fatalf("spent outcome = %s", outcome)
	}

	// two targets of a target cap of 2
	targets := newMoveTestFixture(t, 1000, 200)
	targets.settings.MoveMaxPerMinutePerTargetHost = 2
	targets.store.loads = append(targets.store.loads, moveTestLoad("edge-cool2.example", "g1", 200, 10, 0.3))
	targets.inventory.inventory["edge-cool2.example"] = moveTestInventoryHost("192.0.2.30", "2001:db8::30")
	targets.addIdleCandidates(20)
	targetRefusedBefore := testutil.ToFloat64(moveBudgetRefusedCounter.WithLabelValues(model.ConnectMoveBudgetCapTarget))
	if outcome := targets.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	targetCounts := map[string]int{}
	for _, request := range targets.executor.sentRequests() {
		targetCounts[request.TargetHost] += 1
	}
	if targetCounts[moveTestCoolHost] != 2 || targetCounts["edge-cool2.example"] != 2 || len(targetCounts) != 2 {
		t.Fatalf("moves by target = %v, want 2 each", targetCounts)
	}
	if delta := testutil.ToFloat64(moveBudgetRefusedCounter.WithLabelValues(model.ConnectMoveBudgetCapTarget)) - targetRefusedBefore; delta != 2 {
		t.Errorf("target refusals rose %v, want one per target", delta)
	}

	// a budget error fails the tick closed
	failed := newMoveTestFixture(t, 1000, 200)
	failed.addIdleCandidates(20)
	failed.store.budgetErr = errors.New("redis: i/o timeout")
	if outcome := failed.mover.tick(); outcome != moverTickStoreError {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(failed.executor.sentRequests()) != 0 || len(failed.store.cooldowns) != 0 {
		t.Fatal("a failed reservation moved a client")
	}
}

// A candidate in cooldown is not moved; the others are.
func TestMoverCooldownSkipsCandidates(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	candidates := fixture.addIdleCandidates(10)
	for _, candidate := range candidates[:3] {
		fixture.store.cooldowns[candidate.ClientId] = server.NewId()
	}
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	requests := fixture.executor.sentRequests()
	if len(requests) != 7 {
		t.Fatalf("moved %d, want the 7 not cooling", len(requests))
	}
	for _, request := range requests {
		for _, candidate := range candidates[:3] {
			if request.ClientId == candidate.ClientId {
				t.Errorf("moved a cooling client")
			}
		}
	}
}

// A send that fails withdraws the cooldown its move started and is counted;
// the other moves go ahead.
func TestMoverFailedSendWithdrawsTheCooldown(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	candidates := fixture.addIdleCandidates(10)
	fixture.executor.failClientIds[candidates[4].ClientId] = true
	errorsBefore := testutil.ToFloat64(moveSendErrorsCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance))
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(fixture.executor.sentRequests()) != 9 {
		t.Fatalf("sent %d", len(fixture.executor.sentRequests()))
	}
	if _, ok := fixture.store.cooldowns[candidates[4].ClientId]; ok {
		t.Error("the failed move's cooldown stayed")
	}
	if len(fixture.store.cooldowns) != 9 {
		t.Errorf("cooldowns = %d", len(fixture.store.cooldowns))
	}
	if delta := testutil.ToFloat64(moveSendErrorsCounter.WithLabelValues(moveKindAdvisory, moveTriggerBalance)) - errorsBefore; delta != 1 {
		t.Errorf("send errors rose %v", delta)
	}

	// a cooldown that cannot be written fails the tick closed
	unwritable := newMoveTestFixture(t, 1000, 200)
	unwritable.addIdleCandidates(10)
	unwritable.store.setCooldownErr = errors.New("redis: i/o timeout")
	if outcome := unwritable.mover.tick(); outcome != moverTickStoreError {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(unwritable.executor.sentRequests()) != 0 {
		t.Fatal("moved a client without its cooldown")
	}
}

// A breaker-open address is never a target: the move carries the address of
// the other family. A target host whose every address is open is no target.
func TestMoverSkipsBreakerOpenTargets(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.addIdleCandidates(10)
	fixture.breaker.open[netip.MustParseAddr("192.0.2.20")] = true
	if outcome := fixture.mover.tick(); outcome != moverTickMoved {
		t.Fatalf("outcome = %s", outcome)
	}
	for _, request := range fixture.executor.sentRequests() {
		if !slices.Equal(request.Targets, []netip.Addr{netip.MustParseAddr("2001:db8::20")}) {
			t.Fatalf("targets = %v", request.Targets)
		}
	}

	closed := newMoveTestFixture(t, 1000, 200)
	closed.addIdleCandidates(10)
	closed.breaker.open[netip.MustParseAddr("192.0.2.20")] = true
	closed.breaker.open[netip.MustParseAddr("2001:db8::20")] = true
	if outcome := closed.mover.tick(); outcome != moverTickNoTarget {
		t.Fatalf("outcome = %s", outcome)
	}
}

// A draining process does not tick; a process with no fresh record of its own
// moves nothing; an unreadable table is an error.
func TestMoverIdleStates(t *testing.T) {
	draining := newMoveTestFixture(t, 1000, 200)
	draining.addIdleCandidates(10)
	draining.loadSource.counts.Draining = true
	if outcome := draining.mover.tick(); outcome != moverTickDraining {
		t.Fatalf("outcome = %s", outcome)
	}
	if draining.store.controlReads != 0 || len(draining.executor.sentRequests()) != 0 {
		t.Fatal("a draining process ticked")
	}

	unpublished := newMoveTestFixture(t, 1000, 200)
	unpublished.addIdleCandidates(10)
	unpublished.store.loads[0].Time = moveTestNodeTime.Add(-2 * time.Minute)
	if outcome := unpublished.mover.tick(); outcome != moverTickNoLoad {
		t.Fatalf("outcome = %s", outcome)
	}

	unreadable := newMoveTestFixture(t, 1000, 200)
	unreadable.store.loadsErr = errors.New("redis: i/o timeout")
	if outcome := unreadable.mover.tick(); outcome != moverTickStoreError {
		t.Fatalf("outcome = %s", outcome)
	}
}

// Off still computes and exports the load: the imbalance is visible before
// anything moves (MOVE.md §7 phase 1).
func TestMoverExportsLoadInEveryMode(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.store.setControl(map[string]string{})
	candidates := fixture.addIdleCandidates(4)
	candidates[0].MoveVersion = 0
	if outcome := fixture.mover.tick(); outcome != moverTickOff {
		t.Fatalf("outcome = %s", outcome)
	}
	if load := testutil.ToFloat64(moveHostNormalizedLoadGauge.WithLabelValues()); load != 100 {
		t.Errorf("host load = %v", load)
	}
	if mean := testutil.ToFloat64(moveFleetMeanLoadGauge.WithLabelValues()); mean != 60 {
		t.Errorf("fleet mean = %v", mean)
	}
	if excess := testutil.ToFloat64(moveHostExcessGauge.WithLabelValues()); !moveTestNear(excess, 1000-660) {
		t.Errorf("host excess = %v", excess)
	}
	if capable := testutil.ToFloat64(moveCapableResidentsGauge); capable != 3 {
		t.Errorf("capable residents = %v", capable)
	}
	if mode := testutil.ToFloat64(moverModeGauge); mode != float64(model.ConnectMoveModeOff) {
		t.Errorf("mode = %v", mode)
	}

	// no table: no load series
	fixture.store.loadsErr = errors.New("redis: i/o timeout")
	fixture.mover.tick()
	if count := testutil.CollectAndCount(moveHostNormalizedLoadGauge); count != 0 {
		t.Errorf("host load series = %d without a table", count)
	}
}

// The constructor starts the loop, which ticks every MoveTickInterval until
// the mover closes. With EnableMove off the loop ends at once and never
// reads the control.
func TestMoverLoopRunsUntilClosed(t *testing.T) {
	fixture := newMoveTestFixture(t, 1000, 200)
	fixture.addIdleCandidates(10)
	fixture.settings.MoveTickInterval = 5 * time.Millisecond
	fixture.executor.sent = make(chan *MoveRequest, 1)
	startMover(fixture.mover)
	select {
	case <-fixture.executor.sent:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop never ticked")
	}
	fixture.mover.Close()

	disabled := newMoveTestFixture(t, 1000, 200)
	disabled.settings.EnableMove = false
	done := make(chan struct{})
	go func() {
		defer close(done)
		disabled.mover.run()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a disabled mover kept running")
	}
	if disabled.store.controlReads != 0 {
		t.Error("a disabled mover read its control")
	}
}
