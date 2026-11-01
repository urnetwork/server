// Recycles a provider the api reported stuck (CPUOPT3, the rejection creep;
// controller/connect_control_stuck_provider.go). The api sets the client's
// `{<clientId>}c_recycle` flag (model/client_recycle_model.go). On the flag's
// keyspace event, the process that hosts the client's resident does what a
// compulsory move's deadline does (enforceResidentMoveDeadline): it closes the
// client's transports on this process with the moved close (4006), so none of
// them nominates here again, and then retires the resident, so the client
// redials at once and its transports nominate a fresh one. That is what a
// connect deploy does to each client it serves, and the only thing that ends
// the retries of an sdk that cannot retire a refused route. A resident that
// another resident replaced only idles here, and only retires.
//
// The switch: EnableStuckProviderRecycle is the code ceiling, off by default
// and on only in the connect service (run.go); alt's exchange keeps it off and
// ignores the flags. The Redis move control's `stuckProviderRecycle`
// (model/connect_move_control.go) chooses connect blocks,
// StuckProviderRecycleDefault ("off") applies while it sets none, and the
// exchange reads it every StuckProviderRecycleControlRefreshInterval, keeping
// its last good read. A flag while the switch is off for this block does
// nothing but count, where the resident lives, as disabled.
//
// Every process receives every flag's event and only the resident's host acts.
// The flag's cooldown bounds a client to one recycle per cooldown, and a
// recycle writes nothing to Redis or PostgreSQL: its one read is the resident
// record that decides whether the local resident was replaced. Safe for
// concurrent use.
package connect

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

// One count per recycle flag event this process handled, by outcome:
// recycled (its transports here got the moved close and its resident
// retired), replaced (another resident replaced the local one, which only
// retired), not_resident (no resident of the client here), resident_done (the
// local resident had already ended), disabled (a resident here, with the
// switch off for this block: a recycle it would have made).
var stuckProviderRecyclesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "stuck_provider_recycles_total",
		Help:      "Recycle flag events the api set for a stuck provider, handled by this process, by outcome: recycled (its transports here closed with the moved close and its resident retired), replaced (only its replaced local resident retired), not_resident (no resident here), resident_done (the resident had already ended), disabled (a resident here, with the switch off for this block)",
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

var stuckProviderRecycleEnabledGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "stuck_provider_recycle_enabled",
		Help:      "Whether this process's exchange recycles stuck providers: EnableStuckProviderRecycle as the Redis control last chose for its block, 1 on, 0 off",
	},
)

const (
	stuckProviderRecycleRecycled     = "recycled"
	stuckProviderRecycleReplaced     = "replaced"
	stuckProviderRecycleNotResident  = "not_resident"
	stuckProviderRecycleResidentDone = "resident_done"
	stuckProviderRecycleDisabled     = "disabled"
)

func init() {
	for _, outcome := range []string{
		stuckProviderRecycleRecycled,
		stuckProviderRecycleReplaced,
		stuckProviderRecycleNotResident,
		stuckProviderRecycleResidentDone,
		stuckProviderRecycleDisabled,
	} {
		stuckProviderRecyclesCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		stuckProviderRecyclesCounter,
		stuckProviderRecycleTransportClosesCounter,
		stuckProviderRecycleEnabledGauge,
	)
}

// The switch of stuck provider recycles. Embedded in ExchangeSettings.
type ExchangeStuckProviderRecycleSettings struct {
	// The code ceiling. False: the exchange ignores recycle flags, and the
	// control is not read. Off by default; the connect service turns it on
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
}

func DefaultExchangeStuckProviderRecycleSettings() *ExchangeStuckProviderRecycleSettings {
	return &ExchangeStuckProviderRecycleSettings{
		EnableStuckProviderRecycle:                 false,
		StuckProviderRecycleDefault:                model.ConnectLocalTransportOff,
		StuckProviderRecycleControlRefreshInterval: 10 * time.Second,
		StuckProviderRecycleControlReadTimeout:     5 * time.Second,
	}
}

// The recycle switch of one exchange, as the control last chose it for this
// block, and the workers Exchange.WaitForIdle joins.
type exchangeStuckProviderRecycle struct {
	ctx      context.Context
	settings *ExchangeSettings
	block    string
	read     func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error)

	// whether this block recycles, as of the last good read of the control,
	// and before the first as the code default chooses
	allowed atomic.Bool

	// workerLock closes admission before Exchange.WaitForIdle joins the
	// control loop and every recycle in flight
	workerLock    sync.Mutex
	workersClosed bool
	workers       sync.WaitGroup

	// owned by the control loop: the last read failed. A failure is logged
	// once until a read succeeds.
	failing bool
}

// The exchange's recycle switch, which reads the control until ctx ends. Nil
// while EnableStuckProviderRecycle is off: flags are ignored and nothing is
// read.
func newExchangeStuckProviderRecycle(ctx context.Context, settings *ExchangeSettings, block string) *exchangeStuckProviderRecycle {
	if !settings.EnableStuckProviderRecycle {
		return nil
	}
	recycle := newExchangeStuckProviderRecycleWithStore(ctx, settings, block, model.GetConnectMoveControl)
	recycle.start()
	return recycle
}

// The switch over an explicit store, without its control loop. Tests drive
// refresh directly, or start the loop.
func newExchangeStuckProviderRecycleWithStore(
	ctx context.Context,
	settings *ExchangeSettings,
	block string,
	read func(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error),
) *exchangeStuckProviderRecycle {
	recycle := &exchangeStuckProviderRecycle{
		ctx:      ctx,
		settings: settings,
		block:    block,
		read:     read,
	}
	// until the first read, the code default chooses
	allowed := model.NarrowConnectMoveControl(nil, recycle.ceiling()).StuckProviderRecycleAllows(block)
	recycle.allowed.Store(allowed)
	setStuckProviderRecycleEnabledGauge(allowed)
	return recycle
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

// Starts the control loop.
func (self *exchangeStuckProviderRecycle) start() {
	if !self.beginWorker() {
		return
	}
	go server.HandleError(func() {
		defer self.endWorker()
		self.run()
	})
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

// Reads the control once. A read that fails or raises keeps the last value.
// Called only by the loop; tests call it in the loop's place.
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
		if !self.failing {
			self.failing = true
			glog.Infof("[exchange]stuck provider recycle control unavailable, keeping recycle=%t (%s)\n", self.allows(), err)
		} else if glog.V(1) {
			glog.Infof("[exchange]stuck provider recycle control unavailable (%s)\n", err)
		}
		return
	}
	self.failing = false
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

// The key-event dispatch of a client's recycle flag. It must not block: only
// the resident map and the switch are read here, and the recycle runs on its
// own worker. With the switch off a resident here counts as disabled, so a
// block left off reports the recycles it would have made.
func (self *Exchange) handleClientRecycle(clientId server.Id) {
	recycle := self.stuckProviderRecycle
	if recycle == nil {
		return
	}
	resident := self.localResident(clientId)
	if resident == nil {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleNotResident).Inc()
		return
	}
	if !recycle.allows() {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleDisabled).Inc()
		return
	}
	if !recycle.beginWorker() {
		// the exchange is closing, and its residents end with it
		return
	}
	go server.HandleError(func() {
		defer recycle.endWorker()
		self.recycleStuckProvider(resident)
	})
}

// Ends a reported provider's resident here the way a compulsory move's deadline
// does (enforceResidentMoveDeadline), without a move: replaced, the resident
// only retires; still current, the client's transports on this process close
// with the moved close first, then the resident retires.
func (self *Exchange) recycleStuckProvider(resident *Resident) {
	if resident.IsDone() {
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleResidentDone).Inc()
		return
	}
	if self.residentReplaced(resident) {
		self.retireMovedResident(resident)
		stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleReplaced).Inc()
		return
	}
	closed := self.closeClientTransportsMoved(resident.clientId)
	self.retireMovedResident(resident)
	stuckProviderRecycleTransportClosesCounter.Add(float64(closed))
	stuckProviderRecyclesCounter.WithLabelValues(stuckProviderRecycleRecycled).Inc()
	if glog.V(1) {
		glog.Infof("[exchange]stuck provider recycle %s/%s closed %d transports\n", resident.clientId, resident.residentId, closed)
	}
}
