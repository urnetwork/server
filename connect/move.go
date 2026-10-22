// The mover (connect/MOVE.md §5.5, §5.6, D6-D9, D14): every connect process
// moves some of its own residents, slowly and within a fleet-wide Redis
// budget, from a host above the band around the fleet mean to hosts below it.
//
// Each tick, every MoveTickInterval while the process is not draining:
//  1. Read the Redis move control and the load table, and compute the host
//     loads and the fleet mean (move_select.go). The load gauges are exported
//     in every mode, so the imbalance is visible before anything moves.
//  2. Decide the tick's trigger: evacuation, emergency or balance, in that
//     order (below).
//  3. Skip the tick while this process's Postgres pool acquire wait exceeds
//     MoveDbWaitPauseThreshold: every arrival writes to the database.
//  4. For each candidate, pick a target host with probability proportional to
//     its deficit and one address per family, reserve the budget, then start
//     the client's cooldown, then send the move through the executor, which
//     writes the intent and the excuse markers (the move primitives).
//
// The triggers:
//   - evacuation: the own host is in the control's `evacuateHosts`. Every
//     resident gets a compulsory move, the longest payload-idle first, paced
//     by the budget (MOVE.md §5.6).
//   - emergency: the own host's CPU has been at or above MoveEmergencyCpu for
//     MoveEmergencyTicks consecutive ticks. Balance candidates get compulsory
//     moves, up to MoveMaxPerTick a tick.
//   - balance: the own host is above the band. This process's share of the
//     host's excess goes as advisory moves of balance candidates (D7),
//     residents that just arrived first when arrival moves are on (D9).
//
// Dry run does all of that except the cooldown and the send, and counts the
// moves it would send. Off computes and exports only.
//
// A tick runs under a deadline of MoveTickInterval, so ticks never overlap and
// every Redis call and send is bounded. Any Redis error fails the tick closed:
// nothing more is sent until the next tick. A tick examines at most
// MoveMaxPerTick candidates, so its work is bounded however many residents
// are in cooldown.
//
// Emergency moves go only to capable clients: a legacy client re-resolves the
// same name, which MOVE.md §1 shows lands mostly on the hot hosts again, so
// moving one costs a reconnect without moving load. Evacuation moves every
// resident, legacy and DNS-carried ones included; those migrate by name.
//
// Safe for concurrent use only through OnResidentArrival and Close: one loop
// owns the tick state.
package connect

import (
	"context"
	mathrand "math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A local resident as the mover sees it. Times are on this process's clock.
type MoveCandidate struct {
	ClientId server.Id
	// When this resident started here (its nomination). Zero is unknown, and
	// the resident is then never a balance candidate.
	ResidentStartTime time.Time
	// The latest payload on any of the resident's forwards. Zero when there
	// has been none since the resident started. Pings, and an open contract
	// without payload, are not payload (D7).
	LastPayloadTime time.Time
	// The highest `resident_move_version` any of the client's transports here
	// declared (D2). Zero for a legacy client.
	MoveVersion int
	// Every transport of the client here reaches the platform directly (H1, or
	// H3 on UDP), none over DNS (H3Dns, H3DnsPump).
	DirectCarrier bool
	// Streams open on the resident.
	OpenStreamCount int
	// The IP families of the client's transports here. Neither set means
	// unknown, and the mover offers every family the target has.
	Ipv4 bool
	Ipv6 bool
}

// This process's residents. Implemented by the exchange.
type MoveResidents interface {
	// A snapshot of the local residents, leaving out server probes (D15:
	// capprobe, qualityprobe, egress probe). An in-memory read, called once a
	// tick from the mover's goroutine: it must not do I/O or hold a lock that
	// packet paths need while it builds the snapshot.
	MoveCandidates() []*MoveCandidate
}

// One move for the executor to send.
type MoveRequest struct {
	// Minted by the mover: the intent's and the frame's `move_id`, and the
	// value of the client's cooldown.
	MoveId   server.Id
	ClientId server.Id
	// Advisory or compulsory. The mover never sends drain moves.
	Kind protocol.ResidentMoveKind
	// The host the intent names (`targetHost`).
	TargetHost string
	// At most one IPv4 and one IPv6 address of TargetHost. A compulsory move
	// may carry an address of a family the client does not use; the client
	// then migrates by name.
	Targets []netip.Addr
	// The frame's `migrate_time`, jittered over the tick. A compulsory move's
	// `close_time` is this plus MoveCompulsoryTimeout.
	MigrateTime time.Time
}

// Sends moves. Implemented by the move primitives.
type MoveExecutor interface {
	// Writes the intent `move_intent_<clientId>` = {moveId, kind,
	// fromResidentId, fromExchangeId, targetHost} with TTL MoveArrivalTimeout
	// and the drain excuse markers, then sends `ResidentMigrate{kind, targets,
	// move_id, migrate_time, close_time, prefer_duration}` from the resident's
	// control. A compulsory move also holds the close at its deadline, and
	// goes to a legacy client as the legacy migrate. Returns an error when it
	// sent nothing (the resident is no longer here, the send was refused, a
	// write failed); the mover then withdraws the client's cooldown. Returns
	// within ctx's deadline and calls nothing back on the mover.
	SendMove(ctx context.Context, request *MoveRequest) error
}

// The target breaker (MOVE.md §5.5). Implemented by the move primitives.
type MoveTargetBreaker interface {
	// Whether moves to this target interface address are held off. Called at
	// most once per address per tick, within ctx's deadline. An
	// implementation that cannot tell answers true, so moves avoid targets
	// whose state is unknown.
	TargetBreakerOpen(ctx context.Context, address netip.Addr) bool
}

// This process's Postgres pool.
type MoveDbWait interface {
	// The recent acquire wait of this process's Postgres pool, for example the
	// mean wait over the last tick. An in-memory read.
	PoolAcquireWait() time.Duration
}

// What the mover needs from the process and the move primitives. All are
// required.
type MoverDependencies struct {
	Residents MoveResidents
	// Draining stops the ticks.
	LoadSource    MoveLoadSource
	Inventory     MoveInventoryLoader
	Executor      MoveExecutor
	TargetBreaker MoveTargetBreaker
	DbWait        MoveDbWait
}

// The trigger of a move, as the move metrics label it.
const (
	moveTriggerBalance    = "balance"
	moveTriggerArrival    = "arrival"
	moveTriggerEvacuation = "evacuation"
	moveTriggerEmergency  = "emergency"
)

// The kind of a move, as the move metrics label it.
const (
	moveKindAdvisory   = "advisory"
	moveKindCompulsory = "compulsory"
)

func moveKindLabel(kind protocol.ResidentMoveKind) string {
	if kind == protocol.ResidentMoveKind_ResidentMoveKindCompulsory {
		return moveKindCompulsory
	}
	return moveKindAdvisory
}

// The outcome of one tick, as the ticks metric labels it.
const (
	moverTickOff         = "off"
	moverTickDraining    = "draining"
	moverTickStoreError  = "store_error"
	moverTickNoLoad      = "no_load"
	moverTickInBand      = "in_band"
	moverTickDbPaused    = "db_paused"
	moverTickNoTarget    = "no_target"
	moverTickNoCandidate = "no_candidate"
	moverTickRefused     = "budget_refused"
	moverTickSendFailed  = "send_failed"
	moverTickMoved       = "moved"
)

// The Redis state the mover reads and writes (model/connect_move_*.go).
// Tests replace it.
type moveStore interface {
	getControl(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)
	getLoads(ctx context.Context) ([]*model.ConnectMoveLoad, time.Time, error)
	removeStaleLoads(ctx context.Context, loads []*model.ConnectMoveLoad, nodeTime time.Time, cleanupTimeout time.Duration) (int, error)
	reserveBudget(ctx context.Context, sourceHost string, targetHost string, caps model.ConnectMoveBudgetCaps) (string, error)
	getCooldowns(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error)
	setCooldown(ctx context.Context, clientId server.Id, moveId server.Id, ttl time.Duration) error
	removeCooldown(ctx context.Context, clientId server.Id, moveId server.Id) error
}

// The move model in Redis.
type moveModelStore struct{}

func (moveModelStore) getControl(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	return model.GetConnectMoveControl(ctx, ceiling)
}

func (moveModelStore) getLoads(ctx context.Context) ([]*model.ConnectMoveLoad, time.Time, error) {
	return model.GetConnectMoveLoads(ctx)
}

func (moveModelStore) removeStaleLoads(ctx context.Context, loads []*model.ConnectMoveLoad, nodeTime time.Time, cleanupTimeout time.Duration) (int, error) {
	return model.RemoveStaleConnectMoveLoads(ctx, loads, nodeTime, cleanupTimeout)
}

func (moveModelStore) reserveBudget(ctx context.Context, sourceHost string, targetHost string, caps model.ConnectMoveBudgetCaps) (string, error) {
	return model.ReserveConnectMoveBudget(ctx, sourceHost, targetHost, caps)
}

func (moveModelStore) getCooldowns(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	return model.GetConnectMoveCooldowns(ctx, clientIds)
}

func (moveModelStore) setCooldown(ctx context.Context, clientId server.Id, moveId server.Id, ttl time.Duration) error {
	return model.SetConnectMoveCooldown(ctx, clientId, moveId, ttl)
}

func (moveModelStore) removeCooldown(ctx context.Context, clientId server.Id, moveId server.Id) error {
	return model.RemoveConnectMoveCooldown(ctx, clientId, moveId)
}

// The code settings the Redis control narrows (D14). The drain moves read
// the same control with the same ceiling.
func moveControlCeiling(settings *ExchangeSettings) model.ConnectMoveControlCeiling {
	return model.ConnectMoveControlCeiling{
		EnableMove:       settings.EnableMove,
		EnableDrainMoves: settings.EnableDrainMoves,
		DefaultMode:      settings.MoveDefaultMode,
		MaxPerMinute:     settings.MoveMaxPerMinute,
		Tolerance:        settings.MoveBalanceTolerance,
	}
}

// Consecutive ticks at the emergency CPU threshold (MOVE.md §5.6):
// MoveEmergencyCpu for MoveEmergencyTicks.
type moveEmergency struct {
	settings *ExchangeMoverSettings
	count    int
}

// Counts a tick whose own host CPU busy fraction is hostCpuBusy, and answers
// whether the host sheds this tick: its CPU has been at or above the
// threshold for the required consecutive ticks. An unknown CPU (negative) is
// below the threshold. A non-positive threshold or tick count never sheds.
func (self *moveEmergency) ObserveHostCpu(hostCpuBusy float64) bool {
	cpu := self.settings.MoveEmergencyCpu
	ticks := self.settings.MoveEmergencyTicks
	if !(0 < cpu) || ticks <= 0 || hostCpuBusy < cpu {
		self.count = 0
		return false
	}
	self.count += 1
	return ticks <= self.count
}

// One tick's plan: who to move, how and how many.
type movePlan struct {
	trigger string
	kind    protocol.ResidentMoveKind
	// moves to send at most
	share int
	// in the order to try, at most MoveMaxPerTick
	candidates []*MoveCandidate
	// clients in candidates that arrived here recently (D9)
	arrivalClientIds map[server.Id]bool
	// whether a cooldown keeps a candidate from moving; evacuation moves
	// every resident whatever its cooldown
	checkCooldown bool
}

// The mover of one connect process. See the file header.
type Mover struct {
	ctx    context.Context
	cancel context.CancelFunc

	identity     MoveProcessIdentity
	settings     *ExchangeSettings
	dependencies MoverDependencies
	store        moveStore
	random       moveRandom
	now          func() time.Time

	inventory *moveInventoryCache
	emergency *moveEmergency

	stateLock sync.Mutex
	// clients that arrived here recently and not by intent, oldest first, at
	// most MoveMaxPerTick
	arrivalClientIds []server.Id
	// whether the last tick found this host a balance source with the mover
	// not off, which arrival moves need
	balanceSource bool
}

// Starts the mover of the process identity in the exchange's settings. It
// ticks until ctx ends or Close. With EnableMove off, or a non-positive
// MoveTickInterval, it never ticks.
func NewMover(
	ctx context.Context,
	identity MoveProcessIdentity,
	settings *ExchangeSettings,
	dependencies *MoverDependencies,
) *Mover {
	return startMover(newMoverWithStore(
		ctx,
		identity,
		settings,
		dependencies,
		moveModelStore{},
		mathrand.New(mathrand.NewPCG(mathrand.Uint64(), mathrand.Uint64())),
		time.Now,
	))
}

// Starts the mover's loop.
func startMover(mover *Mover) *Mover {
	go server.HandleError(mover.run)
	return mover
}

// The mover over an explicit store, randomness and clock, without its loop.
// Tests drive its ticks directly.
func newMoverWithStore(
	ctx context.Context,
	identity MoveProcessIdentity,
	settings *ExchangeSettings,
	dependencies *MoverDependencies,
	store moveStore,
	random moveRandom,
	now func() time.Time,
) *Mover {
	if dependencies == nil || dependencies.Residents == nil || dependencies.LoadSource == nil ||
		dependencies.Inventory == nil || dependencies.Executor == nil ||
		dependencies.TargetBreaker == nil || dependencies.DbWait == nil {
		panic("the mover needs every dependency")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	return &Mover{
		ctx:          cancelCtx,
		cancel:       cancel,
		identity:     identity,
		settings:     settings,
		dependencies: *dependencies,
		store:        store,
		random:       random,
		now:          now,
		inventory: &moveInventoryCache{
			loader:   dependencies.Inventory,
			settings: &settings.ExchangeMoverSettings,
		},
		emergency: &moveEmergency{
			settings: &settings.ExchangeMoverSettings,
		},
	}
}

// One tick every MoveTickInterval until the mover closes. Ends, with the mode
// gauge off, when EnableMove is off or the interval is not positive.
func (self *Mover) run() {
	defer self.cancel()
	for {
		interval := self.settings.MoveTickInterval
		if !self.settings.EnableMove || interval <= 0 {
			moverModeGauge.Set(float64(model.ConnectMoveModeOff))
			return
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(interval):
		}
		server.HandleError(func() {
			self.tick()
		})
	}
}

// Stops the mover. A tick in progress ends at its next Redis call or send.
func (self *Mover) Close() {
	self.cancel()
}

// Notes a resident that just arrived here, for an arrival-time move at the
// next tick (D9). byIntent: the client arrived by a move intent, and is never
// moved again. Cheap and non-blocking, for the nomination path. Keeps the
// MoveMaxPerTick most recent arrivals. Does nothing unless EnableArrivalMoves,
// or while the last tick did not find this host a balance source.
func (self *Mover) OnResidentArrival(clientId server.Id, byIntent bool) {
	maxPerTick := self.settings.MoveMaxPerTick
	if byIntent || !self.settings.EnableArrivalMoves || maxPerTick <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.balanceSource {
		return
	}
	if maxPerTick <= len(self.arrivalClientIds) {
		self.arrivalClientIds = append(self.arrivalClientIds[:0], self.arrivalClientIds[len(self.arrivalClientIds)-maxPerTick+1:]...)
	}
	self.arrivalClientIds = append(self.arrivalClientIds, clientId)
}

// Records whether this host is a balance source and takes the arrivals noted
// since the last tick.
func (self *Mover) takeArrivals(balanceSource bool) []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.balanceSource = balanceSource
	arrivalClientIds := self.arrivalClientIds
	self.arrivalClientIds = nil
	return arrivalClientIds
}

// One tick under its deadline, counted by outcome.
func (self *Mover) tick() string {
	tickCtx, tickCancel := context.WithTimeout(self.ctx, self.settings.MoveTickInterval)
	defer tickCancel()
	outcome := self.tickWithContext(tickCtx)
	moverTicksCounter.WithLabelValues(outcome).Inc()
	return outcome
}

func (self *Mover) tickWithContext(ctx context.Context) string {
	settings := self.settings
	now := self.now()

	if self.dependencies.LoadSource.MoveLoadCounts().Draining {
		self.takeArrivals(false)
		return moverTickDraining
	}

	control, err := self.store.getControl(ctx, moveControlCeiling(settings))
	if err != nil {
		// an unreadable control is off
		moverModeGauge.Set(float64(model.ConnectMoveModeOff))
		self.takeArrivals(false)
		if glog.V(1) {
			glog.Infof("[move]control read failed; off this tick (%s)\n", err)
		}
		return moverTickStoreError
	}
	moverModeGauge.Set(float64(control.Mode))

	loads, nodeTime, err := self.store.getLoads(ctx)
	if err != nil {
		resetMoveLoadGauges()
		self.takeArrivals(false)
		if glog.V(1) {
			glog.Infof("[move]load read failed (%s)\n", err)
		}
		return moverTickStoreError
	}
	if removedCount, err := self.store.removeStaleLoads(ctx, loads, nodeTime, settings.MoveLoadCleanupTimeout); err != nil {
		if glog.V(1) {
			glog.Infof("[move]stale load cleanup failed (%s)\n", err)
		}
	} else if 0 < removedCount && glog.V(1) {
		glog.Infof("[move]removed %d stale load records\n", removedCount)
	}

	inventory := self.inventory.hosts(ctx, now)
	fleet := computeMoveFleetLoad(loads, nodeTime, settings.MoveLoadStaleTimeout, inventory, control.EvacuateHosts)
	candidates := self.dependencies.Residents.MoveCandidates()
	exportMoveLoadGauges(fleet, self.identity.Host, control.Tolerance, candidates)

	ownHostLoad := fleet.hosts[self.identity.Host]
	ownHostCpu := model.ConnectMoveLoadCpuUnknown
	if ownHostLoad != nil {
		ownHostCpu = ownHostLoad.cpuBusy
	}
	emergency := self.emergency.ObserveHostCpu(ownHostCpu)
	balanceSource := fleet.isBalanceSource(self.identity.Host, control.Tolerance, settings.MoveSourceMinCpu)
	arrivalClientIds := self.takeArrivals(balanceSource && control.Mode != model.ConnectMoveModeOff)

	if control.Mode == model.ConnectMoveModeOff {
		return moverTickOff
	}
	processResidents, ok := fleet.processResidentCount(self.identity.Host, self.identity.Block, self.identity.ExchangeId)
	if !ok || ownHostLoad == nil || !fleet.hasMean() {
		return moverTickNoLoad
	}

	plan := self.plan(fleet, control, emergency, balanceSource, processResidents, candidates, arrivalClientIds, now)
	if plan == nil || plan.share <= 0 {
		return moverTickInBand
	}
	if len(plan.candidates) == 0 {
		return moverTickNoCandidate
	}

	if 0 < settings.MoveDbWaitPauseThreshold && settings.MoveDbWaitPauseThreshold < self.dependencies.DbWait.PoolAcquireWait() {
		return moverTickDbPaused
	}

	breakerStates := map[netip.Addr]bool{}
	breakerOpen := func(address netip.Addr) bool {
		if open, ok := breakerStates[address]; ok {
			return open
		}
		open := self.dependencies.TargetBreaker.TargetBreakerOpen(ctx, address)
		breakerStates[address] = open
		return open
	}
	targets := moveTargets(fleet, self.identity.Host, settings.MoveTargetMaxCpu, inventory, breakerOpen)
	if len(targets) == 0 {
		return moverTickNoTarget
	}

	return self.commit(ctx, plan, control, targets, now)
}

// The tick's plan, or nil when this host has nothing to shed.
func (self *Mover) plan(
	fleet *moveFleetLoad,
	control *model.ConnectMoveControl,
	emergency bool,
	balanceSource bool,
	processResidents int,
	candidates []*MoveCandidate,
	arrivalClientIds []server.Id,
	now time.Time,
) *movePlan {
	settings := self.settings
	maxPerTick := max(0, settings.MoveMaxPerTick)
	ownHost := self.identity.Host
	ownHostLoad := fleet.hosts[ownHost]

	balanceCandidates := func() []*MoveCandidate {
		eligible := []*MoveCandidate{}
		for _, candidate := range candidates {
			if balanceMoveIneligibility(candidate, now, &settings.ExchangeMoverSettings) == moveEligible {
				eligible = append(eligible, candidate)
			}
		}
		return eligible
	}

	switch {
	case fleet.isEvacuating(ownHost):
		ordered := evacuationMoveOrder(candidates)
		return &movePlan{
			trigger:       moveTriggerEvacuation,
			kind:          protocol.ResidentMoveKind_ResidentMoveKindCompulsory,
			share:         maxPerTick,
			candidates:    ordered[:min(maxPerTick, len(ordered))],
			checkCooldown: false,
		}

	case emergency:
		return &movePlan{
			trigger:       moveTriggerEmergency,
			kind:          protocol.ResidentMoveKind_ResidentMoveKindCompulsory,
			share:         maxPerTick,
			candidates:    sampleMoveCandidates(balanceCandidates(), maxPerTick, self.random),
			checkCooldown: true,
		}

	case balanceSource:
		share := moveProcessShare(
			fleet.hostExcess(ownHost, control.Tolerance),
			processResidents,
			ownHostLoad.residents,
			maxPerTick,
			self.random,
		)
		plan := &movePlan{
			trigger:          moveTriggerBalance,
			kind:             protocol.ResidentMoveKind_ResidentMoveKindAdvisory,
			share:            share,
			arrivalClientIds: map[server.Id]bool{},
			checkCooldown:    true,
		}
		if share <= 0 {
			return plan
		}
		// arrivals first: they are the cheapest to move (D9)
		if settings.EnableArrivalMoves && 0 < len(arrivalClientIds) {
			arrived := map[server.Id]bool{}
			for _, clientId := range arrivalClientIds {
				arrived[clientId] = true
			}
			for _, candidate := range candidates {
				if maxPerTick <= len(plan.candidates) {
					break
				}
				if arrived[candidate.ClientId] && !plan.arrivalClientIds[candidate.ClientId] &&
					arrivalMoveIneligibility(candidate) == moveEligible {
					plan.candidates = append(plan.candidates, candidate)
					plan.arrivalClientIds[candidate.ClientId] = true
				}
			}
		}
		idle := []*MoveCandidate{}
		for _, candidate := range balanceCandidates() {
			if !plan.arrivalClientIds[candidate.ClientId] {
				idle = append(idle, candidate)
			}
		}
		plan.candidates = append(plan.candidates, sampleMoveCandidates(idle, maxPerTick-len(plan.candidates), self.random)...)
		return plan

	default:
		return nil
	}
}

// Sends the plan's moves: for each candidate in order, until the plan's share
// is sent, a target and its addresses, the budget, the cooldown and the send.
func (self *Mover) commit(
	ctx context.Context,
	plan *movePlan,
	control *model.ConnectMoveControl,
	targets []*moveTarget,
	now time.Time,
) string {
	settings := self.settings
	ownHost := self.identity.Host
	kindLabel := moveKindLabel(plan.kind)
	compulsory := plan.kind == protocol.ResidentMoveKind_ResidentMoveKindCompulsory
	caps := model.ConnectMoveBudgetCaps{
		Fleet:  control.MaxPerMinute,
		Source: settings.MoveMaxPerMinutePerSourceHost,
		Target: settings.MoveMaxPerMinutePerTargetHost,
	}

	cooling := map[server.Id]bool{}
	if plan.checkCooldown {
		clientIds := make([]server.Id, len(plan.candidates))
		for i, candidate := range plan.candidates {
			clientIds[i] = candidate.ClientId
		}
		var err error
		cooling, err = self.store.getCooldowns(ctx, clientIds)
		if err != nil {
			if glog.V(1) {
				glog.Infof("[move]cooldown read failed (%s)\n", err)
			}
			return moverTickStoreError
		}
	}

	// A target for the candidate and its addresses. A compulsory move whose
	// client uses no family a target offers goes to any target, and the
	// client migrates by name.
	chooseTarget := func(candidate *MoveCandidate) (*moveTarget, []netip.Addr) {
		families := candidateMoveFamilies(candidate)
		target := chooseMoveTarget(targets, families, self.random)
		if target == nil && compulsory {
			families = moveFamilies{ipv4: true, ipv6: true}
			target = chooseMoveTarget(targets, families, self.random)
		}
		if target == nil {
			return nil, nil
		}
		return target, chooseMoveTargetAddresses(target, families, self.random)
	}

	sentCount := 0
	refusedCount := 0
	sendErrorCount := 0
	for _, candidate := range plan.candidates {
		if plan.share <= sentCount {
			break
		}
		if cooling[candidate.ClientId] {
			continue
		}
		trigger := plan.trigger
		if plan.arrivalClientIds[candidate.ClientId] {
			trigger = moveTriggerArrival
		}

		var target *moveTarget
		var addresses []netip.Addr
		for {
			target, addresses = chooseTarget(candidate)
			if target == nil {
				break
			}
			refusedCap, err := self.store.reserveBudget(ctx, ownHost, target.host, caps)
			if err != nil {
				if glog.V(1) {
					glog.Infof("[move]budget reservation failed (%s)\n", err)
				}
				return moverTickStoreError
			}
			if refusedCap == "" {
				break
			}
			moveBudgetRefusedCounter.WithLabelValues(refusedCap).Inc()
			refusedCount += 1
			if refusedCap != model.ConnectMoveBudgetCapTarget {
				// the fleet or this host is spent for the window
				return self.commitOutcome(sentCount, refusedCount, sendErrorCount)
			}
			// that target is spent for the window; try the others
			target.deficit = 0
		}
		if target == nil {
			continue
		}

		if control.Mode == model.ConnectMoveModeDryRun {
			movesWouldSendCounter.WithLabelValues(kindLabel, trigger, ownHost, target.host).Inc()
			target.deficit -= 1
			sentCount += 1
			continue
		}

		moveId := server.NewId()
		if err := self.store.setCooldown(ctx, candidate.ClientId, moveId, settings.MoveClientCooldown); err != nil {
			if glog.V(1) {
				glog.Infof("[move]cooldown write failed (%s)\n", err)
			}
			return moverTickStoreError
		}
		request := &MoveRequest{
			MoveId:      moveId,
			ClientId:    candidate.ClientId,
			Kind:        plan.kind,
			TargetHost:  target.host,
			Targets:     addresses,
			MigrateTime: now.Add(time.Duration(self.random.Float64() * float64(settings.MoveTickInterval))),
		}
		if err := self.dependencies.Executor.SendMove(ctx, request); err != nil {
			moveSendErrorsCounter.WithLabelValues(kindLabel, trigger).Inc()
			sendErrorCount += 1
			if err := self.store.removeCooldown(ctx, candidate.ClientId, moveId); err != nil && glog.V(1) {
				glog.Infof("[move]cooldown withdraw failed (%s)\n", err)
			}
			if glog.V(1) {
				glog.Infof("[move]%s send to %s failed (%s)\n", moveId, target.host, err)
			}
			continue
		}
		movesSentCounter.WithLabelValues(kindLabel, trigger, ownHost, target.host).Inc()
		target.deficit -= 1
		sentCount += 1
		if glog.V(2) {
			glog.Infof("[move]%s %s %s %s -> %s %v\n", moveId, kindLabel, trigger, candidate.ClientId, target.host, addresses)
		}
	}
	return self.commitOutcome(sentCount, refusedCount, sendErrorCount)
}

func (self *Mover) commitOutcome(sentCount int, refusedCount int, sendErrorCount int) string {
	switch {
	case 0 < sentCount:
		return moverTickMoved
	case 0 < refusedCount:
		return moverTickRefused
	case 0 < sendErrorCount:
		return moverTickSendFailed
	default:
		return moverTickNoCandidate
	}
}
