// A handler's connection reconcile retires the connection rows its process no
// longer holds.
//
// Each announcement writes its connection row once and disconnects it once,
// with a bounded final write. Until this reconcile nothing retried that write:
// when it failed (a database outage, a pooler stall), handler expiry never
// reached the row either, because this process kept its handler alive. The
// row then stayed connected until the process restarted, and every reader of
// connected rows counted a provider that was gone. On Main, the 2026-10-13
// primary restarts and pooler livelock left about 29k provider rows connected
// on the blocks that kept running.
//
// The reconcile keeps, in memory, the connections this process holds: rows
// whose registration committed and whose announcement has not ended. A pass
// reads the rows the database holds connected under the handler and retires
// any row the process does not hold. A registration commits its row before it
// can hold it, so each registration records the generation it began in, and
// the generation advances after every pass's read: a registration that began
// later cannot own a row that read saw. A row is retired on its second
// sighting, once no registration that began before its first sighting's read
// returned is still in flight. Waiting for the second sighting keeps steady
// connection churn from ever blocking a retire, and it covers a row whose
// registration raised after its commit. A failed final disconnect wakes an
// early pass, no sooner than the backlog interval after the last one. Passes
// retire in bounded batches through the model, with the same endpoint fence as
// the transport's own disconnect. Rows of a process that ended remain handler
// expiry's.
//
// All methods are safe for concurrent use. A nil reconcile holds nothing and
// never retires, which is what an announcement without a handler owner uses.
package connect

import (
	"context"
	"math"
	mathrand "math/rand"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var connectionReconcileRetiredCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "connection_reconcile_retired_total",
		Help:      "Connection rows the handler's reconcile disconnected because this process no longer held them",
	},
)

var connectionReconcilePassesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "connection_reconcile_passes_total",
		Help:      "Connection reconcile passes, by whether the pass completed or raised",
	},
	[]string{"result"},
)

var connectionReconcileUnheldGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "connection_reconcile_unheld",
		Help:      "Connection rows the last reconcile pass found connected under this handler but not held by this process",
	},
)

// Registers the reconcile metrics with the default registry.
func init() {
	prometheus.MustRegister(
		connectionReconcileRetiredCounter,
		connectionReconcilePassesCounter,
		connectionReconcileUnheldGauge,
	)
	for _, result := range []string{"ok", "error"} {
		connectionReconcilePassesCounter.WithLabelValues(result)
	}
}

// The least time between passes whatever the settings, so a pass that keeps
// raising, or keeps finding rows to confirm, cannot spin.
const connectionReconcileMinInterval = 100 * time.Millisecond

// A five minute pass costs one indexed read of the handler's open rows (about
// 26 ms for the 24k rows of the largest Main block on 2026-10-14). A backlog
// drains at most 256 rows every 15 seconds, each with the transport
// disconnect's own fence.
func DefaultConnectionReconcileSettings() *ConnectionReconcileSettings {
	return &ConnectionReconcileSettings{
		ReconcileInterval:       5 * time.Minute,
		ReconcileIntervalJitter: 30 * time.Second,
		BacklogInterval:         15 * time.Second,
		RetireBatchCount:        256,
		OperationTimeout:        30 * time.Second,
	}
}

// The pace and bounds of a handler's connection reconcile.
type ConnectionReconcileSettings struct {
	// between passes when the last pass left nothing to confirm or retire
	ReconcileInterval time.Duration
	// added at random to each interval, so blocks started together spread
	// their reads
	ReconcileIntervalJitter time.Duration
	// between passes while rows remain to confirm or retire, and the earliest
	// a failed final disconnect moves a pass up to after the last pass
	BacklogInterval time.Duration
	// the most rows one pass retires; zero observes and counts unheld rows
	// without retiring any
	RetireBatchCount int
	// bounds the pass's read and each retire
	OperationTimeout time.Duration
}

// The connections of one handler's process and the loop that reconciles them
// with the handler's connected rows. See the file header.
type handlerConnectionReconcile struct {
	ctx       context.Context
	handlerId server.Id
	settings  *ConnectionReconcileSettings

	// the model by default; tests read and retire against a fake store
	readConnectionIds func(ctx context.Context, handlerId server.Id) []server.Id
	retireConnection  func(ctx context.Context, handlerId server.Id, connectionId server.Id, disconnectTime time.Time) (retired bool, deferred bool)

	wake chan struct{}

	stateLock sync.Mutex
	// registered connections whose announcement has not ended
	heldConnectionIds map[server.Id]bool
	// registrations in flight, by the generation each began in
	connectingGenerationCounts map[uint64]int
	// the number of pass reads that have returned
	generation uint64
	// rows seen connected and unheld, by the generation their first sighting's
	// read returned in
	unheldConnectionGenerations map[server.Id]uint64
}

// Starts the reconcile loop for handlerId, which ends with ctx.
func newHandlerConnectionReconcile(
	ctx context.Context,
	handlerId server.Id,
	settings *ConnectionReconcileSettings,
) *handlerConnectionReconcile {
	reconcile := newHandlerConnectionReconcileWithStore(
		ctx,
		handlerId,
		settings,
		model.GetConnectedNetworkClientHandlerConnectionIds,
		model.RetireNetworkClientHandlerConnection,
	)
	go server.HandleError(reconcile.run)
	return reconcile
}

// The reconcile state over an explicit store, without its loop. Tests drive
// its passes directly.
func newHandlerConnectionReconcileWithStore(
	ctx context.Context,
	handlerId server.Id,
	settings *ConnectionReconcileSettings,
	readConnectionIds func(ctx context.Context, handlerId server.Id) []server.Id,
	retireConnection func(ctx context.Context, handlerId server.Id, connectionId server.Id, disconnectTime time.Time) (retired bool, deferred bool),
) *handlerConnectionReconcile {
	if settings == nil || settings.ReconcileInterval <= 0 || settings.BacklogInterval <= 0 || settings.OperationTimeout <= 0 {
		// unset settings would spin the loop, or expire every read at once
		settings = DefaultConnectionReconcileSettings()
	}
	return &handlerConnectionReconcile{
		ctx:                         ctx,
		handlerId:                   handlerId,
		settings:                    settings,
		readConnectionIds:           readConnectionIds,
		retireConnection:            retireConnection,
		wake:                        make(chan struct{}, 1),
		heldConnectionIds:           map[server.Id]bool{},
		connectingGenerationCounts:  map[uint64]int{},
		unheldConnectionGenerations: map[server.Id]uint64{},
	}
}

// Admits one registration. The returned function ends it exactly once,
// holding connectionId when the registration committed one and nil when it
// returned an error or raised. Until it ends, no row it may have committed is
// retired.
func (self *handlerConnectionReconcile) beginConnect() func(connectionId *server.Id) {
	if self == nil {
		return func(*server.Id) {}
	}
	var generation uint64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		generation = self.generation
		self.connectingGenerationCounts[generation] += 1
	}()
	var once sync.Once
	return func(connectionId *server.Id) {
		once.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if count := self.connectingGenerationCounts[generation] - 1; count <= 0 {
				delete(self.connectingGenerationCounts, generation)
			} else {
				self.connectingGenerationCounts[generation] = count
			}
			if connectionId != nil {
				self.heldConnectionIds[*connectionId] = true
			}
		})
	}
}

// Ends the hold of a connection whose announcement ended. disconnected
// reports whether its final disconnect was written; a row it left connected is
// the reconcile's, and wakes an early pass.
func (self *handlerConnectionReconcile) release(connectionId server.Id, disconnected bool) {
	if self == nil {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.heldConnectionIds, connectionId)
	}()
	if !disconnected {
		select {
		case self.wake <- struct{}{}:
		default:
		}
	}
}

// Runs a pass every interval, or a backlog interval after the last pass while
// rows remain to confirm or retire or a failed final disconnect asked for one.
// A raised pass is retried a backlog interval later.
func (self *handlerConnectionReconcile) run() {
	lastPassTime := time.Now()
	nextPassTime := lastPassTime.Add(self.interval())
	for {
		if wait := time.Until(nextPassTime); 0 < wait {
			select {
			case <-self.ctx.Done():
				return
			case <-self.wake:
				if wakePassTime := lastPassTime.Add(self.backlogInterval()); wakePassTime.Before(nextPassTime) {
					nextPassTime = wakePassTime
				}
				continue
			case <-time.After(wait):
			}
		}
		if self.ctx.Err() != nil {
			return
		}

		backlog := true
		passErr := server.HandleError(func() {
			backlog = self.reconcile()
		})
		if passErr != nil {
			connectionReconcilePassesCounter.WithLabelValues("error").Inc()
		} else {
			connectionReconcilePassesCounter.WithLabelValues("ok").Inc()
		}
		lastPassTime = time.Now()
		if backlog {
			nextPassTime = lastPassTime.Add(self.backlogInterval())
		} else {
			nextPassTime = lastPassTime.Add(self.interval())
		}
	}
}

// The regular interval with its jitter, never below the floor.
func (self *handlerConnectionReconcile) interval() time.Duration {
	interval := self.settings.ReconcileInterval
	if 0 < self.settings.ReconcileIntervalJitter {
		interval += time.Duration(mathrand.Int63n(int64(self.settings.ReconcileIntervalJitter)))
	}
	return max(interval, connectionReconcileMinInterval)
}

// The backlog interval, never below the floor.
func (self *handlerConnectionReconcile) backlogInterval() time.Duration {
	return max(self.settings.BacklogInterval, connectionReconcileMinInterval)
}

// One pass: reads the handler's connected rows, notes the unheld ones, and
// retires at most a batch of those seen unheld before whose registrations have
// all ended. Returns whether rows remain to confirm or retire. The read and
// every retire are bounded by the operation timeout; a raise ends the pass and
// leaves its rows to the next.
func (self *handlerConnectionReconcile) reconcile() (backlog bool) {
	// every row read here committed before the read returned, so only a
	// registration that began before it returned can still come to hold one
	var connectionIds []server.Id
	func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.OperationTimeout)
		defer readCancel()
		connectionIds = self.readConnectionIds(readCtx, self.handlerId)
	}()

	var staleConnectionIds []server.Id
	unheldCount := 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// registrations that begin from here on are in this generation or a
		// later one, and none of them can own a row this read saw
		self.generation += 1
		generation := self.generation
		oldestConnectingGeneration := uint64(math.MaxUint64)
		for connectingGeneration := range self.connectingGenerationCounts {
			oldestConnectingGeneration = min(oldestConnectingGeneration, connectingGeneration)
		}
		unheldConnectionIds := map[server.Id]bool{}
		for _, connectionId := range connectionIds {
			if self.heldConnectionIds[connectionId] {
				continue
			}
			unheldConnectionIds[connectionId] = true
			firstGeneration, seen := self.unheldConnectionGenerations[connectionId]
			if !seen {
				self.unheldConnectionGenerations[connectionId] = generation
				continue
			}
			// a registration that began before the first sighting's read
			// returned, and is still in flight, may yet hold this row
			if firstGeneration <= oldestConnectingGeneration {
				staleConnectionIds = append(staleConnectionIds, connectionId)
			}
		}
		// a row disconnected or held since its sighting needs nothing more
		for connectionId := range self.unheldConnectionGenerations {
			if !unheldConnectionIds[connectionId] {
				delete(self.unheldConnectionGenerations, connectionId)
			}
		}
		unheldCount = len(self.unheldConnectionGenerations)
	}()
	connectionReconcileUnheldGauge.Set(float64(unheldCount))

	// oldest first, so a backlog drains in the order it formed
	slices.SortFunc(staleConnectionIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	batchCount := min(len(staleConnectionIds), max(self.settings.RetireBatchCount, 0))
	retiredCount := 0
	for _, connectionId := range staleConnectionIds[:batchCount] {
		if self.ctx.Err() != nil {
			return true
		}
		retired := func() bool {
			retireCtx, retireCancel := context.WithTimeout(self.ctx, self.settings.OperationTimeout)
			defer retireCancel()
			retired, _ := self.retireConnection(retireCtx, self.handlerId, connectionId, server.NowUtc())
			return retired
		}()
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// retired, or already disconnected by another owner
			delete(self.unheldConnectionGenerations, connectionId)
		}()
		if retired {
			retiredCount += 1
			connectionReconcileRetiredCounter.Inc()
		}
	}
	if 0 < retiredCount {
		glog.Infof("[t]connection reconcile retired %d of %d stale connections\n", retiredCount, len(staleConnectionIds))
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// a zero batch only observes, at the regular interval
		backlog = 0 < len(self.unheldConnectionGenerations) && 0 < self.settings.RetireBatchCount
	}()
	return
}
