package controller

// Reports a provider stuck retrying a route its own sdk cannot retire (CPUOPT3,
// the rejection creep). Providers on connect builds before v2026.9.8 never act
// on a CreateContractResult error: one that holds return traffic for a removed
// window client asks for that route on its own schedule, about every 5 s (its
// send sequence's contract wait asks at 0, 1, 3, 7, 12, 17, 22 and 27 s of
// each 30 s wait, then the next return packet starts another), until its
// connection is replaced. Every connect deploy replaces them all, which is the
// creep's sawtooth. A replay answered entirely from terminal refusals (the
// destination row present and inactive, `model.ContractRouteRejection.Terminal`)
// for a pack none of whose frames carries a `SenderRole` (connect builds before
// v2026.9.4) is such a retry. This owner counts those per client and, once a
// client has kept retrying long enough, sets the client's recycle flag
// (model/client_recycle_model.go). The connect process hosting the client's
// resident then recycles it as a deploy would (connect/client_recycle.go).
// Nothing here changes an answer: the replay, the refusal caches and the
// client's limit run as before, and a report runs off the request.
//
// Thresholds. The load balancers spread one provider's retries over about 20
// api processes, so a process sees one stuck route about every 100 s
// (5 s × 20), and a provider stuck on k routes k times as often. Each process
// decides alone, from what it sees:
//   - MinRetryDuration, 10 min: the client's run of terminal answers on this
//     process has lasted this long. One contract wait lasts 30 s, so a route
//     the provider stops asking for by itself never spans it.
//   - MinRetryCount, 4: at least this many answers in the run. A stuck route
//     gives about 6 in 10 min here, and 4 or more with probability about 0.85.
//   - MaxRetryGap, 5 min: a longer silence on this process starts the run
//     over. For a stuck route a gap this long has probability e^-3, about
//     0.05, and a client that paused or stopped starts again from zero.
// So a single stuck route crosses on a given process within one 10 min window
// with probability about 0.6, and on at least one of the 20 within about 10
// to 15 min almost surely. A provider whose terminal answers come in brief
// bursts, one contract wait each, crosses only if this process sees a burst
// at least every 5 min for 10 min. More api processes slow each one down and
// add as many chances; fewer speed each one up.
//
// A crossing starts the run over, so a process reports a client at most once
// per MinRetryDuration. The flag is set only if absent, with a ttl of
// Cooldown, so a client is recycled at most once per Cooldown however many
// processes report it; the other reports count a cooldown.
//
// The switch: EnableStuckProviderSignal is the code ceiling, off by default
// and on only in the api service (api/run.go). The Redis move control's
// `stuckProviderSignal` (model/connect_move_control.go) chooses api blocks,
// StuckProviderSignalDefault ("off") applies while it sets none, and the owner
// reads it every ControlRefreshInterval, keeping its last good read. While the
// switch is off nothing is counted or written, and turning it off forgets
// every run.
//
// Bounded: two generations of at most TrackedClientCount clients each, as the
// terminal refusal registry (model/contract_route_rejection.go), and at most
// MaxConcurrentSignals reports in flight; a report past that is dropped and
// counted, and its client starts a new run. Safe for concurrent use.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One count per report this process decided to send, by result: set (the
// recycle flag was set), cooldown (it was already set), error (the write
// failed), dropped (no report worker was free). Their sum is the decisions.
var stuckProviderRecycleDecisionsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "connect",
	Name:      "stuck_provider_recycle_decisions_total",
	Help:      "Clients this api process found answered only from terminal refusals, for packs without a sender role, long enough to report for a recycle, by result: set (the recycle flag was set), cooldown (it was already set), error (the write failed), dropped (no report worker was free)",
}, []string{"result"})

var stuckProviderTrackedClientsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "connect",
	Name:      "stuck_provider_tracked_clients",
	Help:      "Clients with a run of terminal refusal answers tracked by this api process, in both generations",
})

var stuckProviderSignalEnabledGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "connect",
	Name:      "stuck_provider_signal_enabled",
	Help:      "Whether this api process reports stuck providers: EnableStuckProviderSignal as the Redis control last chose for its block, 1 on, 0 off",
})

const (
	stuckProviderRecycleSet      = "set"
	stuckProviderRecycleCooldown = "cooldown"
	stuckProviderRecycleError    = "error"
	stuckProviderRecycleDropped  = "dropped"
)

func init() {
	for _, result := range []string{
		stuckProviderRecycleSet,
		stuckProviderRecycleCooldown,
		stuckProviderRecycleError,
		stuckProviderRecycleDropped,
	} {
		stuckProviderRecycleDecisionsCounter.WithLabelValues(result)
	}
	prometheus.MustRegister(
		stuckProviderRecycleDecisionsCounter,
		stuckProviderTrackedClientsGauge,
		stuckProviderSignalEnabledGauge,
	)
}

// The switch, thresholds and bounds of stuck provider reports. See the file
// header for how the thresholds follow from the retry cadence.
type StuckProviderSignalSettings struct {
	// The code ceiling. False: no owner is made, nothing is counted, and the
	// control is not read. The api service turns it on (api/run.go); alt keeps
	// it off.
	EnableStuckProviderSignal bool
	// The api blocks that report while the Redis control sets no
	// `stuckProviderSignal`, in the control's syntax. Not a ceiling. "off"
	// ships the code dark.
	StuckProviderSignalDefault string
	// How often the owner reads the control. Non-positive reads it once, at
	// start.
	ControlRefreshInterval time.Duration
	// Bounds one read of the control.
	ControlReadTimeout time.Duration

	// How long a client's run of terminal answers on this process lasts
	// before it is reported.
	MinRetryDuration time.Duration
	// The fewest answers in a run that is reported.
	MinRetryCount int
	// A longer silence between two answers to a client starts its run over.
	// Non-positive allows any gap.
	MaxRetryGap time.Duration
	// Each of the two generations holds at most this many clients.
	// Non-positive tracks none.
	TrackedClientCount int

	// The recycle flag's ttl: the fleet-wide cooldown between two recycles of
	// a client.
	Cooldown time.Duration
	// Bounds one report's write.
	SignalTimeout time.Duration
	// The most reports in flight. A report past it is dropped.
	MaxConcurrentSignals int
}

func DefaultStuckProviderSignalSettings() *StuckProviderSignalSettings {
	return &StuckProviderSignalSettings{
		EnableStuckProviderSignal:  false,
		StuckProviderSignalDefault: model.ConnectLocalTransportOff,
		ControlRefreshInterval:     10 * time.Second,
		ControlReadTimeout:         5 * time.Second,
		MinRetryDuration:           10 * time.Minute,
		MinRetryCount:              4,
		MaxRetryGap:                5 * time.Minute,
		TrackedClientCount:         16384,
		Cooldown:                   1 * time.Hour,
		SignalTimeout:              5 * time.Second,
		MaxConcurrentSignals:       16,
	}
}

// One client's current run of terminal answers on this process.
type stuckProviderRetry struct {
	firstTime time.Time
	lastTime  time.Time
	count     int
}

// The per-process owner of stuck provider reports. Nil while the ceiling is
// off; every method of a nil owner does nothing.
type StuckProviderSignals struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *StuckProviderSignalSettings
	block    string

	readControl func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)
	setRecycle  func(ctx context.Context, clientId server.Id, cooldown time.Duration) (bool, error)
	now         func() time.Time

	// whether this block reports, as of the last good read of the control,
	// and before the first as the code default chooses
	allowed atomic.Bool

	stateLock             sync.Mutex
	currentClientRetries  map[server.Id]stuckProviderRetry
	previousClientRetries map[server.Id]stuckProviderRetry

	// workerLock closes admission before Close joins the control loop and
	// every report in flight
	workerLock     sync.Mutex
	workersClosed  bool
	signalsRunning int
	workers        sync.WaitGroup

	// owned by the control loop: the last read failed. A failure is logged
	// once until a read succeeds.
	failing bool
}

// The owner for the api processes of block, which reads the control until
// ctx ends or Close. Nil while EnableStuckProviderSignal is off.
func NewStuckProviderSignals(ctx context.Context, settings *StuckProviderSignalSettings, block string) *StuckProviderSignals {
	if settings == nil || !settings.EnableStuckProviderSignal {
		return nil
	}
	signals := newStuckProviderSignalsWithStore(ctx, settings, block, model.GetConnectMoveControl, model.SetClientRecycle, time.Now)
	signals.start()
	return signals
}

// The owner over an explicit store and clock, without its control loop.
// Tests drive refresh directly, or start the loop.
func newStuckProviderSignalsWithStore(
	ctx context.Context,
	settings *StuckProviderSignalSettings,
	block string,
	readControl func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error),
	setRecycle func(ctx context.Context, clientId server.Id, cooldown time.Duration) (bool, error),
	now func() time.Time,
) *StuckProviderSignals {
	cancelCtx, cancel := context.WithCancel(ctx)
	signals := &StuckProviderSignals{
		ctx:                   cancelCtx,
		cancel:                cancel,
		settings:              settings,
		block:                 block,
		readControl:           readControl,
		setRecycle:            setRecycle,
		now:                   now,
		currentClientRetries:  map[server.Id]stuckProviderRetry{},
		previousClientRetries: map[server.Id]stuckProviderRetry{},
	}
	// until the first read, the code default chooses
	allowed := model.NarrowConnectMoveControl(nil, signals.ceiling()).StuckProviderSignalAllows(block)
	signals.allowed.Store(allowed)
	stuckProviderSignalEnabledGauge.Set(stuckProviderGaugeValue(allowed))
	return signals
}

func stuckProviderGaugeValue(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

// The code settings the control narrows.
func (self *StuckProviderSignals) ceiling() model.ConnectMoveControlCeiling {
	return model.ConnectMoveControlCeiling{
		EnableStuckProviderSignal:  self.settings.EnableStuckProviderSignal,
		StuckProviderSignalDefault: self.settings.StuckProviderSignalDefault,
	}
}

// Whether this block reports, as of the last good read.
func (self *StuckProviderSignals) allows() bool {
	return self != nil && self.allowed.Load()
}

// Admits one worker for Close unless the owner has closed.
func (self *StuckProviderSignals) beginWorker() bool {
	self.workerLock.Lock()
	defer self.workerLock.Unlock()
	if self.workersClosed {
		return false
	}
	self.workers.Add(1)
	return true
}

// Admits one report worker unless the owner has closed or MaxConcurrentSignals
// are in flight.
func (self *StuckProviderSignals) beginSignal() bool {
	self.workerLock.Lock()
	defer self.workerLock.Unlock()
	if self.workersClosed || self.settings.MaxConcurrentSignals <= self.signalsRunning {
		return false
	}
	self.signalsRunning += 1
	self.workers.Add(1)
	return true
}

func (self *StuckProviderSignals) endSignal() {
	func() {
		self.workerLock.Lock()
		defer self.workerLock.Unlock()
		self.signalsRunning -= 1
	}()
	self.workers.Done()
}

// Starts the control loop.
func (self *StuckProviderSignals) start() {
	if !self.beginWorker() {
		return
	}
	go server.HandleError(func() {
		defer self.workers.Done()
		self.run()
	})
}

// Reads the control now, then every ControlRefreshInterval until the owner
// ends, on one reused timer. A non-positive interval reads once.
func (self *StuckProviderSignals) run() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		self.refresh()
		if self.settings.ControlRefreshInterval <= 0 {
			return
		}
		timer.Reset(self.settings.ControlRefreshInterval)
		select {
		case <-self.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// Reads the control once. A read that fails or raises keeps the last value.
// Called only by the loop; tests call it in the loop's place.
func (self *StuckProviderSignals) refresh() {
	var control *model.ConnectMoveControl
	var err error
	server.HandleError(func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.ControlReadTimeout)
		defer readCancel()
		control, err = self.readControl(readCtx, self.ceiling())
	}, func(readErr error) {
		err = readErr
	})
	if err != nil {
		if self.ctx.Err() != nil {
			// the owner is closing
			return
		}
		if !self.failing {
			self.failing = true
			glog.Infof("[stuck]stuck provider control unavailable, keeping report=%t (%s)\n", self.allows(), err)
		} else if glog.V(1) {
			glog.Infof("[stuck]stuck provider control unavailable (%s)\n", err)
		}
		return
	}
	self.failing = false
	self.setAllowed(control.StuckProviderSignalAllows(self.block))
}

// Publishes the switch. Turning it off forgets every run, so turning it on
// again starts each client from zero.
func (self *StuckProviderSignals) setAllowed(allowed bool) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.allowed.Load() == allowed {
			return false
		}
		self.allowed.Store(allowed)
		if !allowed {
			clear(self.currentClientRetries)
			clear(self.previousClientRetries)
			stuckProviderTrackedClientsGauge.Set(0)
		}
		return true
	}()
	if !changed {
		return
	}
	stuckProviderSignalEnabledGauge.Set(stuckProviderGaugeValue(allowed))
	glog.Infof("[stuck]stuck provider control: block %s report=%t\n", self.block, allowed)
}

// Counts one replay answered only from terminal refusals for a pack without a
// sender role, and reports the client once its run crosses the thresholds.
// Never blocks on I/O.
func (self *StuckProviderSignals) observeTerminalRetry(clientId server.Id) {
	if !self.allows() {
		return
	}
	if !self.noteRetry(clientId, self.now()) {
		return
	}
	self.signal(clientId)
}

// Adds one answer at now to the client's run. Returns whether the run crossed
// the thresholds, in which case it starts over.
func (self *StuckProviderSignals) noteRetry(clientId server.Id, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.settings.TrackedClientCount <= 0 || !self.allowed.Load() {
		return false
	}
	retry, ok := self.currentClientRetries[clientId]
	if !ok {
		if retry, ok = self.previousClientRetries[clientId]; ok {
			delete(self.previousClientRetries, clientId)
		}
	}
	if ok && 0 < self.settings.MaxRetryGap && self.settings.MaxRetryGap < now.Sub(retry.lastTime) {
		ok = false
	}
	if ok {
		if retry.lastTime.Before(now) {
			retry.lastTime = now
		}
		retry.count += 1
	} else {
		retry = stuckProviderRetry{
			firstTime: now,
			lastTime:  now,
			count:     1,
		}
	}
	crossed := self.settings.MinRetryCount <= retry.count && self.settings.MinRetryDuration <= now.Sub(retry.firstTime)
	if crossed {
		delete(self.currentClientRetries, clientId)
	} else {
		self.putWithLock(clientId, retry)
	}
	stuckProviderTrackedClientsGauge.Set(float64(len(self.currentClientRetries) + len(self.previousClientRetries)))
	return crossed
}

// A full current generation becomes the previous one and the old previous one
// is dropped whole, so at most twice TrackedClientCount runs are held.
func (self *StuckProviderSignals) putWithLock(clientId server.Id, retry stuckProviderRetry) {
	if _, ok := self.currentClientRetries[clientId]; !ok && self.settings.TrackedClientCount <= len(self.currentClientRetries) {
		self.previousClientRetries = self.currentClientRetries
		self.currentClientRetries = map[server.Id]stuckProviderRetry{}
	}
	self.currentClientRetries[clientId] = retry
}

// Reports the client off the request: sets its recycle flag if absent.
func (self *StuckProviderSignals) signal(clientId server.Id) {
	if !self.beginSignal() {
		stuckProviderRecycleDecisionsCounter.WithLabelValues(stuckProviderRecycleDropped).Inc()
		return
	}
	go server.HandleError(func() {
		defer self.endSignal()
		result := stuckProviderRecycleError
		var err error
		server.HandleError(func() {
			signalCtx, signalCancel := context.WithTimeout(self.ctx, self.settings.SignalTimeout)
			defer signalCancel()
			var set bool
			set, err = self.setRecycle(signalCtx, clientId, self.settings.Cooldown)
			switch {
			case err != nil:
			case set:
				result = stuckProviderRecycleSet
			default:
				result = stuckProviderRecycleCooldown
			}
		}, func(signalErr error) {
			err = signalErr
		})
		stuckProviderRecycleDecisionsCounter.WithLabelValues(result).Inc()
		if glog.V(1) {
			glog.Infof("[stuck]%s recycle report=%s err=%v\n", clientId, result, err)
		}
	})
}

// Ends the control loop, closes admission and joins every report in flight.
func (self *StuckProviderSignals) Close() {
	if self == nil {
		return
	}
	self.cancel()
	func() {
		self.workerLock.Lock()
		defer self.workerLock.Unlock()
		self.workersClosed = true
	}()
	self.workers.Wait()
}

type stuckProviderSignalsKey struct{}

// Binds the router's owner to a control request.
func WithStuckProviderSignals(ctx context.Context, signals *StuckProviderSignals) context.Context {
	return context.WithValue(ctx, stuckProviderSignalsKey{}, signals)
}

func stuckProviderSignalsFromContext(ctx context.Context) *StuckProviderSignals {
	signals, _ := ctx.Value(stuckProviderSignalsKey{}).(*StuckProviderSignals)
	return signals
}
