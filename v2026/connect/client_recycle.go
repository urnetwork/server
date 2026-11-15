// Recycles a provider the api reported stuck (CPUOPT3, the rejection creep;
// controller/connect_control_stuck_provider.go). The api sets the client's
// `{<clientId>}c_recycle` flag (model/client_recycle_model.go). On the flag's
// keyspace event, every process that holds any of the client's transports or
// its resident does what a deploy does to the client there: it writes a drain
// excuse, so the reconnect counts as excused rather than against the
// provider's reliability, closes the client's transports on this process with
// the moved close (4006), and retires the resident if it lives here. A
// transport forwards to its instance's resident wherever that lives, so a
// transport on another process than the resident is closed by its own
// process, and the client redials every transport into a fresh resident. That
// is the only thing that ends the retries of an sdk that cannot retire a
// refused route. A removed client is left to the removal's close and to its
// resident's idle ttl.
//
// The switch: EnableStuckProviderRecycle is the code ceiling, off by default
// and on only in the connect service (run.go); alt's exchange keeps it off and
// subscribes no flags. The Redis move control's `stuckProviderRecycle`
// (model/connect_move_control.go) chooses connect blocks,
// StuckProviderRecycleDefault ("off") applies while it sets none, and the
// exchange reads it every StuckProviderRecycleControlRefreshInterval. A failed
// read keeps the last value until StuckProviderRecycleControlMaxFailedReads
// fail in a row, which turn the switch off. A flag while the switch is off
// for this block does nothing but count, where the client is held, as
// disabled.
//
// Pacing: the dispatch only queues a held client, on a bounded queue, and a
// fixed pool of workers recycles at most StuckProviderRecyclesPerSecond with a
// burst of StuckProviderRecycleBurst. A flag that finds the queue full is
// counted and dropped. The flag's cooldown bounds a client to one recycle per
// cooldown. A recycle reads the removed flag and the resident record, and
// writes only the drain excuse. Safe for concurrent use.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// One count per recycle flag event this process handled, by outcome:
// recycled (its resident here retired, and at least one of its transports
// here closed), resident_only (its resident here retired, with none of its
// transports here), replaced (a replaced resident here retired, and its
// transports here closed), transport_only (its transports here closed, with
// no live resident here), resident_done (its resident here had already ended,
// and no transport was here), not_held (nothing of the client here), disabled
// (held here, with the switch off for this block), removed (the client was
// removed), queue_full (held here, past the queue), error (the removed flag
// was unreadable, or the recycle failed).
var stuckProviderRecyclesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "stuck_provider_recycles_total",
		Help:      "Recycle flag events the api set for a stuck provider, handled by this process, by outcome: recycled (its resident here retired and at least one of its transports here closed with the moved close), resident_only (its resident here retired, with no transport here), replaced (a replaced resident here retired), transport_only (only its transports here closed), resident_done (its resident here had already ended), not_held (nothing of the client here), disabled (held here, with the switch off for this block), removed (a removed client, left alone), queue_full (past the recycle queue), error (the removed flag was unreadable, or the recycle failed)",
	},
	[]string{"outcome"},
)

var stuckProviderRecycleTransportClosesCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "stuck_provider_recycle_transport_closes_total",
		Help:      "Client transports a stuck provider recycle closed with the moved close",
	},
)

var stuckProviderRecycleExcusesCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "stuck_provider_recycle_excuses_total",
		Help:      "Drain excuse markers a stuck provider recycle wrote before closing a client's transports, so the reconnect counts as excused",
	},
)

var stuckProviderRecycleEnabledGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "stuck_provider_recycle_enabled",
		Help:      "Whether this process's exchange recycles stuck providers: EnableStuckProviderRecycle as the Redis control last chose for its block, 1 on, 0 off",
	},
)

const (
	stuckProviderRecycleRecycled      = "recycled"
	stuckProviderRecycleResidentOnly  = "resident_only"
	stuckProviderRecycleReplaced      = "replaced"
	stuckProviderRecycleTransportOnly = "transport_only"
	stuckProviderRecycleResidentDone  = "resident_done"
	stuckProviderRecycleNotHeld       = "not_held"
	stuckProviderRecycleDisabled      = "disabled"
	stuckProviderRecycleRemoved       = "removed"
	stuckProviderRecycleQueueFull     = "queue_full"
	stuckProviderRecycleError         = "error"
)

func init() {
	for _, outcome := range []string{
		stuckProviderRecycleRecycled,
		stuckProviderRecycleResidentOnly,
		stuckProviderRecycleReplaced,
		stuckProviderRecycleTransportOnly,
		stuckProviderRecycleResidentDone,
		stuckProviderRecycleNotHeld,
		stuckProviderRecycleDisabled,
		stuckProviderRecycleRemoved,
		stuckProviderRecycleQueueFull,
		stuckProviderRecycleError,
	} {
		stuckProviderRecyclesCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		stuckProviderRecyclesCounter,
		stuckProviderRecycleTransportClosesCounter,
		stuckProviderRecycleExcusesCounter,
		stuckProviderRecycleEnabledGauge,
	)
}

// The switch and pacing of stuck provider recycles. Embedded in
// ExchangeSettings.
type ExchangeStuckProviderRecycleSettings struct {
	// The code ceiling. False: the exchange subscribes and reads no flags, and
	// the control is not read. Off by default; the connect service turns it on
	// (run.go), and embedders such as alt keep it off.
	EnableStuckProviderRecycle bool
	// The blocks that recycle while the Redis control sets no
	// `stuckProviderRecycle`, in the control's syntax: "off", "all", or blocks
	// separated by commas or spaces. Not a ceiling: an operator widens it with
	// the control, up to EnableStuckProviderRecycle. "off" ships the code dark.
	StuckProviderRecycleDefault string
	// How often the exchange reads the control's `stuckProviderRecycle`.
	// Non-positive reads it once, at start.
	StuckProviderRecycleControlRefreshInterval time.Duration
	// Bounds one read of the control.
	StuckProviderRecycleControlReadTimeout time.Duration
	// This many failed reads in a row turn the switch off. Non-positive keeps
	// the last value through any number.
	StuckProviderRecycleControlMaxFailedReads int
	// The recycle rate of this process, and its burst. Non-positive recycles
	// none.
	StuckProviderRecyclesPerSecond float64
	StuckProviderRecycleBurst      int
	// Flags waiting for a worker. A flag past it is dropped.
	StuckProviderRecycleQueueSize int
	// The workers that recycle.
	StuckProviderRecycleWorkerCount int
	// Bounds one recycle's reads and writes of the client's removed flag and
	// drain excuse.
	StuckProviderRecycleTimeout time.Duration
}

func DefaultExchangeStuckProviderRecycleSettings() *ExchangeStuckProviderRecycleSettings {
	return &ExchangeStuckProviderRecycleSettings{
		EnableStuckProviderRecycle:                 false,
		StuckProviderRecycleDefault:                model.ConnectLocalTransportOff,
		StuckProviderRecycleControlRefreshInterval: 10 * time.Second,
		StuckProviderRecycleControlReadTimeout:     5 * time.Second,
		StuckProviderRecycleControlMaxFailedReads:  3,
		StuckProviderRecyclesPerSecond:             5,
		StuckProviderRecycleBurst:                  20,
		StuckProviderRecycleQueueSize:              1024,
		StuckProviderRecycleWorkerCount:            4,
		StuckProviderRecycleTimeout:                5 * time.Second,
	}
}

// The recycle switch and pacing of one exchange, as the control last chose it
// for this block, and the workers Exchange.WaitForIdle joins.
type exchangeStuckProviderRecycle struct {
	ctx      context.Context
	settings *ExchangeSettings
	block    string
	read     func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)
	// recycles one queued client; the exchange's recycleStuckProvider
	recycle func(clientId server.Id)

	queue  chan server.Id
	bucket *server.TokenBucket

	// whether this block recycles, as of the last good read of the control,
	// and before the first as the code default chooses
	allowed atomic.Bool

	// workerLock closes admission before Exchange.WaitForIdle joins the
	// control loop and the recycle workers
	workerLock    sync.Mutex
	workersClosed bool
	workers       sync.WaitGroup

	// owned by the control loop: failed reads in a row. The first failure is
	// logged once until a read succeeds.
	failedReads int
}

// The exchange's recycle switch, which reads the control and runs its workers
// until ctx ends. Nil while EnableStuckProviderRecycle is off: flags are
// ignored and nothing is read.
func newExchangeStuckProviderRecycle(ctx context.Context, settings *ExchangeSettings, block string, recycle func(server.Id)) *exchangeStuckProviderRecycle {
	if !settings.EnableStuckProviderRecycle {
		return nil
	}
	stuckProviderRecycle := newExchangeStuckProviderRecycleWithStore(ctx, settings, block, model.GetConnectMoveControl, recycle)
	stuckProviderRecycle.start()
	return stuckProviderRecycle
}

// The switch over an explicit store, without its control loop or workers.
// Tests drive refresh directly, or start them.
func newExchangeStuckProviderRecycleWithStore(
	ctx context.Context,
	settings *ExchangeSettings,
	block string,
	read func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error),
	recycle func(server.Id),
) *exchangeStuckProviderRecycle {
	stuckProviderRecycle := &exchangeStuckProviderRecycle{
		ctx:      ctx,
		settings: settings,
		block:    block,
		read:     read,
		recycle:  recycle,
		queue:    make(chan server.Id, max(0, settings.StuckProviderRecycleQueueSize)),
		bucket:   server.NewTokenBucket(settings.StuckProviderRecyclesPerSecond, settings.StuckProviderRecycleBurst),
	}
	// until the first read, the code default chooses
	allowed := model.NarrowConnectMoveControl(nil, stuckProviderRecycle.ceiling()).StuckProviderRecycleAllows(block)
	stuckProviderRecycle.allowed.Store(allowed)
	setStuckProviderRecycleEnabledGauge(allowed)
	return stuckProviderRecycle
}

func setStuckProviderRecycleEnabledGauge(allowed bool) {
	if allowed {
		stuckProviderRecycleEnabledGauge.Set(1)
	} else {
		stuckProviderRecycleEnabledGauge.Set(0)
	}
}

// The code settings the control narrows.
func (self *exchangeStuckProviderRecycle) ceiling() model.ConnectMoveControlCeiling {
	return model.ConnectMoveControlCeiling{
		EnableStuckProviderRecycle:  self.settings.EnableStuckProviderRecycle,
		StuckProviderRecycleDefault: self.settings.StuckProviderRecycleDefault,
	}
}

// Whether this block recycles, as of the last good read.
func (self *exchangeStuckProviderRecycle) allows() bool {
	return self.allowed.Load()
}

// Admits one worker for Exchange.WaitForIdle unless the exchange has closed.
func (self *exchangeStuckProviderRecycle) beginWorker() bool {
	self.workerLock.Lock()
	defer self.workerLock.Unlock()
	if self.workersClosed {
		return false
	}
	self.workers.Add(1)
	return true
}

// Releases one worker admitted by beginWorker.
func (self *exchangeStuckProviderRecycle) endWorker() {
	self.workers.Done()
}

// Closes worker admission, so Exchange.WaitForIdle's join is final.
func (self *exchangeStuckProviderRecycle) closeWorkers() {
	self.workerLock.Lock()
	defer self.workerLock.Unlock()
	self.workersClosed = true
}

// Starts the control loop and the recycle workers.
func (self *exchangeStuckProviderRecycle) start() {
	if self.beginWorker() {
		go server.HandleError(func() {
			defer self.endWorker()
			self.run()
		})
	}
	for range max(0, self.settings.StuckProviderRecycleWorkerCount) {
		if !self.beginWorker() {
			return
		}
		go server.HandleError(func() {
			defer self.endWorker()
			self.runRecycles()
		})
	}
}

// Reads the control now, then every StuckProviderRecycleControlRefreshInterval
// until ctx ends, on one reused timer. A non-positive interval reads once.
func (self *exchangeStuckProviderRecycle) run() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		self.refresh()
		if self.settings.StuckProviderRecycleControlRefreshInterval <= 0 {
			return
		}
		timer.Reset(self.settings.StuckProviderRecycleControlRefreshInterval)
		select {
		case <-self.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// Reads the control once. A read that fails or raises keeps the last value
// until StuckProviderRecycleControlMaxFailedReads fail in a row, which turn
// the switch off. Called only by the loop; tests call it in the loop's place.
func (self *exchangeStuckProviderRecycle) refresh() {
	var control *model.ConnectMoveControl
	var err error
	server.HandleError(func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.StuckProviderRecycleControlReadTimeout)
		defer readCancel()
		control, err = self.read(readCtx, self.ceiling())
	}, func(readErr error) {
		err = readErr
	})
	if err != nil {
		if self.ctx.Err() != nil {
			// the exchange is closing
			return
		}
		self.failedReads += 1
		if self.failedReads == 1 {
			glog.Infof("[exchange]stuck provider recycle control unavailable, keeping recycle=%t (%s)\n", self.allows(), err)
		} else if glog.V(1) {
			glog.Infof("[exchange]stuck provider recycle control unavailable (%s)\n", err)
		}
		if 0 < self.settings.StuckProviderRecycleControlMaxFailedReads &&
			self.settings.StuckProviderRecycleControlMaxFailedReads <= self.failedReads {
			self.setAllowed(false)
		}
		return
	}
	self.failedReads = 0
	self.setAllowed(control.StuckProviderRecycleAllows(self.block))
}

// Publishes the switch for the next flag.
func (self *exchangeStuckProviderRecycle) setAllowed(allowed bool) {
	if self.allowed.Swap(allowed) == allowed {
		return
	}
	setStuckProviderRecycleEnabledGauge(allowed)
	glog.Infof("[exchange]stuck provider recycle control: block %s recycle=%t\n", self.block, allowed)
}

// Queues a held client for a worker without waiting. False when the queue is
// full.
func (self *exchangeStuckProviderRecycle) enqueue(clientId server.Id) bool {
	select {
	case self.queue <- clientId:
		return true
	default:
		return false
	}
}

// Recycles queued clients until ctx ends, each after taking a token. A
// recycle that raises counts as an error and the worker goes on.
func (self *exchangeStuckProviderRecycle) runRecycles() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var clientId server.Id
		select {
		case <-self.ctx.Done():
			return
		case clientId = <-self.queue:
		}
		for !self.bucket.Take(time.Now()) {
			wait := time.Second
			if 0 < self.settings.StuckProviderRecyclesPerSecond {
				wait = time.Duration(float64(time.Second) / self.settings.StuckProviderRecyclesPerSecond)
			}
			timer.Reset(wait)
			select {
			case <-self.ctx.Done():
				return
			case <-timer.C:
			}
		}
		server.HandleError(func() {
			self.recycle(clientId)
		}, func(err error) {
			stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleError).Inc()
			if glog.V(1) {
				glog.Infof("[exchange]stuck provider recycle %s failed (%s)\n", clientId, err)
			}
		})
	}
}

// Whether this process holds any of the client's transports or its resident.
func (self *Exchange) holdsClient(clientId server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.residents[clientId] != nil || 0 < len(self.connections[clientId])
}

// How many of the client's transports this process holds.
func (self *Exchange) clientConnectionCount(clientId server.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.connections[clientId])
}

// The key-event dispatch of a client's recycle flag. It must not block: only
// the client maps and the switch are read here, and a held client is queued
// for the workers. With the switch off a held client counts as disabled, so a
// block left off reports the recycles it would have made.
func (self *Exchange) handleClientRecycle(clientId server.Id) {
	recycle := self.stuckProviderRecycle
	if recycle == nil {
		return
	}
	if !self.holdsClient(clientId) {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleNotHeld).Inc()
		return
	}
	if !recycle.allows() {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleDisabled).Inc()
		return
	}
	if !recycle.enqueue(clientId) {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleQueueFull).Inc()
	}
}

// Does what a deploy does to the client on this process: excuses its
// reconnect, closes its transports here with the moved close, and retires its
// resident if it lives here, a replaced one included. A removed client is left
// alone: its transports already got the removal's close, and its resident
// ends at its idle ttl so peers' forwards do not spin on its lookup.
func (self *Exchange) recycleStuckProvider(clientId server.Id) {
	recycleCtx, recycleCancel := context.WithTimeout(self.ctx, self.settings.StuckProviderRecycleTimeout)
	defer recycleCancel()
	removed := false
	removedErr := server.HandleError(func() {
		removed = model.IsClientRemoved(recycleCtx, clientId)
	})
	if removedErr != nil {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleError).Inc()
		return
	}
	if removed {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleRemoved).Inc()
		return
	}
	resident := self.localResident(clientId)
	if resident != nil && resident.IsDone() {
		// counts only: an ended resident leaves on its own
		resident = nil
	}
	connectionCount := self.clientConnectionCount(clientId)
	if resident == nil && connectionCount == 0 {
		outcome := stuckProviderRecycleNotHeld
		if self.localResident(clientId) != nil {
			outcome = stuckProviderRecycleResidentDone
		}
		stuckProviderRecyclesCounter.WithLabelValues(outcome).Inc()
		return
	}
	// read before the close, which starts the client's reconnects
	replaced := resident != nil && self.residentReplaced(resident)
	if 0 < connectionCount && self.settings.EnableDrainExcuse {
		// as a drain, before the close: a zero id records no resident here
		residentId := server.Id{}
		if resident != nil {
			residentId = resident.residentId
		}
		model.SetDrainExcuse(recycleCtx, clientId, residentId, self.settings.DrainExcuseTtl)
		stuckProviderRecycleExcusesCounter.Inc()
	}
	// as a move's deadline: the transports here first, so none of them
	// nominates here again, then the resident
	closed := self.closeClientTransportsMoved(clientId)
	stuckProviderRecycleTransportClosesCounter.Add(float64(closed))
	if resident != nil {
		self.retireMovedResident(resident)
	}
	outcome := stuckProviderRecycleNotHeld
	switch {
	case replaced:
		outcome = stuckProviderRecycleReplaced
	case resident != nil && 0 < closed:
		outcome = stuckProviderRecycleRecycled
	case resident != nil:
		outcome = stuckProviderRecycleResidentOnly
	case 0 < closed:
		outcome = stuckProviderRecycleTransportOnly
	}
	stuckProviderRecyclesCounter.WithLabelValues(outcome).Inc()
	if glog.V(1) {
		glog.Infof("[exchange]stuck provider recycle %s outcome=%s closed %d transports\n", clientId, outcome, closed)
	}
}
