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
// early pass, no sooner than the backlog interval after the last one, and
// passes keep that interval while rows known to be stale remain: rows of
// failed final disconnects, and stale rows not yet retired. A row unheld for a
// moment during ordinary churn waits for the regular interval. Passes
// retire in bounded batches through the model. A retire takes the transport
// disconnect's fences with try-locks, as handler expiry does, so a busy
// endpoint defers its row to a later pass instead of queueing. Busy and failed
// retires are tried after rows with fewer retries, so neither can monopolize
// a full batch. After three consecutive failures the pass leaves the
// rest of its batch too. Rows of a process that ended remain handler expiry's.
//
// All methods are safe for concurrent use. A nil reconcile holds nothing and
// never retires, which is what an announcement without a handler owner uses.
package connect

import (
	"cmp"
	"context"
	"math"
	mathrand "math/rand"
	"runtime"
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

var connectionReconcileUnretiredCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "connection_reconcile_unretired_total",
		Help:      "Stale connection rows a reconcile pass did not retire, by reason: the retire raised, or a fence or the row was busy; the row stays for a later pass",
	},
	[]string{"reason"},
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
		connectionReconcileUnretiredCounter,
		connectionReconcileUnheldGauge,
	)
	for _, result := range []string{"ok", "error"} {
		connectionReconcilePassesCounter.WithLabelValues(result)
	}
	for _, reason := range []string{"failed", "busy"} {
		connectionReconcileUnretiredCounter.WithLabelValues(reason)
	}
}

// The least time between passes whatever the settings, so a pass that keeps
// raising, or keeps finding rows to confirm, cannot spin.
const connectionReconcileMinInterval = 100 * time.Millisecond

// A pass leaves the rest of its batch for the next pass after this many
// consecutive failed retires: the database is likely failing them all.
const connectionReconcileMaxConsecutiveRetireFailures = 3

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
	// between passes when no row known to be stale remains
	ReconcileInterval time.Duration
	// added at random to each interval, so blocks started together spread
	// their reads
	ReconcileIntervalJitter time.Duration
	// between passes while rows known to be stale remain (rows of failed final
	// disconnects, and stale rows not yet retired), and the earliest a failed
	// final disconnect moves a pass up to after the last pass
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
	// unheld rows whose retire raised or deferred, by how many times
	retireRetryCounts map[server.Id]int
	// rows whose final disconnect failed, by when their transport ended
	releaseTimes map[server.Id]time.Time
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
		retireRetryCounts:           map[server.Id]int{},
		releaseTimes:                map[server.Id]time.Time{},
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
// the reconcile's, retired with this release's time as its disconnect time,
// and wakes an early pass.
func (self *handlerConnectionReconcile) release(connectionId server.Id, disconnected bool) {
	if self == nil {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.heldConnectionIds, connectionId)
		if !disconnected {
			self.releaseTimes[connectionId] = server.NowUtc()
		}
	}()
	if !disconnected {
		select {
		case self.wake <- struct{}{}:
		default:
		}
	}
}

// Runs a pass every interval, or a backlog interval after the last pass while
// rows known to be stale remain or a failed final disconnect asked for one.
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
// all ended. Returns whether rows known to be stale remain: rows of failed
// final disconnects, and stale rows the pass did not retire. The read and
// every retire are bounded by the operation timeout. A raised read ends the
// pass; a raised retire only fails its row (see the file header).
func (self *handlerConnectionReconcile) reconcile() (backlog bool) {
	// every row read here committed before the read returned, so only a
	// registration that began before it returned can still come to hold one
	readStartTime := server.NowUtc()
	var connectionIds []server.Id
	func() {
		readCtx, readCancel := context.WithTimeout(self.ctx, self.settings.OperationTimeout)
		defer readCancel()
		connectionIds = self.readConnectionIds(readCtx, self.handlerId)
	}()

	var staleConnectionIds []server.Id
	retryCounts := map[server.Id]int{}
	releaseTimes := map[server.Id]time.Time{}
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
				delete(self.retireRetryCounts, connectionId)
			}
		}
		// a row released before this read began, and not read connected, was
		// disconnected; a row released since may have committed after the read
		for connectionId, releaseTime := range self.releaseTimes {
			if !unheldConnectionIds[connectionId] && releaseTime.Before(readStartTime) {
				delete(self.releaseTimes, connectionId)
			}
		}
		unheldCount = len(self.unheldConnectionGenerations)
		for _, connectionId := range staleConnectionIds {
			retryCounts[connectionId] = self.retireRetryCounts[connectionId]
			if releaseTime, released := self.releaseTimes[connectionId]; released {
				releaseTimes[connectionId] = releaseTime
			}
		}
	}()
	connectionReconcileUnheldGauge.Set(float64(unheldCount))

	// Repeated failures and busy fences yield to rows with fewer attempts.
	// Otherwise oldest first, so untouched backlogs drain in their usual order.
	slices.SortFunc(staleConnectionIds, func(a server.Id, b server.Id) int {
		if c := cmp.Compare(retryCounts[a], retryCounts[b]); c != 0 {
			return c
		}
		return a.Cmp(b)
	})
	batchCount := min(len(staleConnectionIds), max(self.settings.RetireBatchCount, 0))
	retiredCount := 0
	busyCount := 0
	failedCount := 0
	consecutiveFailureCount := 0
	var lastFailure error
	for _, connectionId := range staleConnectionIds[:batchCount] {
		if self.ctx.Err() != nil {
			return true
		}
		// the transport's end when its release recorded it, else now
		disconnectTime, released := releaseTimes[connectionId]
		if !released {
			disconnectTime = server.NowUtc()
		}
		retired, deferred, retireErr := self.retireOne(connectionId, disconnectTime)
		if retireErr != nil {
			if self.ctx.Err() != nil {
				return true
			}
			// the row stays for a later pass, after the rows that have not failed
			failedCount += 1
			consecutiveFailureCount += 1
			lastFailure = retireErr
			connectionReconcileUnretiredCounter.WithLabelValues("failed").Inc()
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.retireRetryCounts[connectionId] += 1
			}()
			if connectionReconcileMaxConsecutiveRetireFailures <= consecutiveFailureCount {
				break
			}
			continue
		}
		consecutiveFailureCount = 0
		if deferred {
			// A busy prefix must yield its next batch to later free rows.
			busyCount += 1
			connectionReconcileUnretiredCounter.WithLabelValues("busy").Inc()
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.retireRetryCounts[connectionId] += 1
			}()
			continue
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// retired, or already disconnected by another owner
			delete(self.unheldConnectionGenerations, connectionId)
			delete(self.retireRetryCounts, connectionId)
			delete(self.releaseTimes, connectionId)
		}()
		if retired {
			retiredCount += 1
			connectionReconcileRetiredCounter.Inc()
		}
	}
	if 0 < failedCount {
		glog.Infof("[t]connection reconcile retired %d, deferred %d and failed %d of %d stale connections; last failure: %s\n", retiredCount, busyCount, failedCount, len(staleConnectionIds), lastFailure)
	} else if 0 < retiredCount {
		glog.Infof("[t]connection reconcile retired %d and deferred %d of %d stale connections\n", retiredCount, busyCount, len(staleConnectionIds))
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// a zero batch only observes, at the regular interval
		if self.settings.RetireBatchCount <= 0 {
			return
		}
		// Only rows known to be stale keep the backlog cadence: rows of failed
		// final disconnects, and stale rows this pass did not retire. A row
		// unheld for a moment during ordinary churn, such as a registration in
		// flight or a disconnect committing, waits for the regular pass.
		for _, connectionId := range staleConnectionIds {
			if _, unheld := self.unheldConnectionGenerations[connectionId]; unheld {
				backlog = true
				return
			}
		}
		for connectionId := range self.releaseTimes {
			if _, unheld := self.unheldConnectionGenerations[connectionId]; unheld {
				backlog = true
				return
			}
		}
	}()
	return
}

// One retire, bounded by the operation timeout. A raise is that retire's
// failure, and the caller keeps the row for a later pass; a programming fault
// (a runtime error, or a raise that is not an error) still raises.
func (self *handlerConnectionReconcile) retireOne(connectionId server.Id, disconnectTime time.Time) (retired bool, deferred bool, retireErr error) {
	retireCtx, retireCancel := context.WithTimeout(self.ctx, self.settings.OperationTimeout)
	defer retireCancel()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if _, fault := r.(runtime.Error); fault {
			panic(r)
		}
		err, ok := r.(error)
		if !ok {
			panic(r)
		}
		retireErr = err
	}()
	retired, deferred = self.retireConnection(retireCtx, self.handlerId, connectionId, disconnectTime)
	return
}
