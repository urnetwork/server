// A connect process's live connection announcements batch the Redis and
// database I/O of their sync rounds and workers (LBALTOPT2.md R18).
//
// Every connection read its provide key changes and modes and wrote its
// reliability stats each sync round, about every 30 seconds, refreshed its
// verify egress lease every EgressRefreshInterval, and wrote its measurement
// snapshots, each with round trips of its own: 3–4% of connect's CPU on
// 2026-11-20 (§3.3). The timer sweep fires the rounds that come due in one
// tick together (§8.1), so they reach the batcher together.
//
// The batcher commits each kind of I/O in groups. A call submits one item and
// waits for that item's own outcome. A goroutine, running only while items
// wait, takes every waiting item, up to MaxBatchCount, whenever it is free and
// runs them as one batch: a lone item waits for no timer, and items that come
// together share a statement or a pipeline. Each item keeps its connection's
// semantics: its connection counts its own failure, in
// connection_sync_read_failures_total or connection_worker_failures_total, and
// retries on its own backoff, exactly as before, and the lease ttls are
// unchanged. A failure of one item's command fails that item alone; a failure
// of the whole batch, an outage, fails every item in it, as each item's own
// call would have failed.
//
// An item no batch has taken when its caller's context ends is withdrawn, and
// a taken item is waited for, so none of an item's I/O runs after its call
// returns: an announcement's final cleanup, which follows its workers' join,
// never races a refresh of the lease it clears.
package connect

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The kinds of announcement I/O the batcher commits, as its metrics label
// them.
const (
	announceBatchKindSyncRead          = "sync_read"
	announceBatchKindVerifyEgressLease = "verify_egress_lease"
	announceBatchKindReliabilityStats  = "reliability_stats"
	announceBatchKindMeasurementWrite  = "measurement_write"
)

var connectionAnnounceBatchesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "announce_batches_total",
		Help:      "Batches of live connection announcement I/O, by kind: sync_read, the sync rounds' provide key change and provide mode reads; verify_egress_lease, the verify egress lease refreshes; reliability_stats, the sync rounds' stats writes; measurement_write, the measurement publishers' writes",
	},
	[]string{"kind"},
)

var connectionAnnounceBatchItemsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "announce_batch_items_total",
		Help:      "Items of live connection announcement batches, one a connection's read or write, by kind; over urnetwork_connect_announce_batches_total, the mean batch size",
	},
	[]string{"kind"},
)

var connectionAnnounceBatchRaisesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "announce_batch_raises_total",
		Help:      "Live connection announcement batches that failed as a whole, by kind; each of their items failed, counted by its connection as its own read or write failure",
	},
	[]string{"kind"},
)

func init() {
	prometheus.MustRegister(connectionAnnounceBatchesCounter)
	prometheus.MustRegister(connectionAnnounceBatchItemsCounter)
	prometheus.MustRegister(connectionAnnounceBatchRaisesCounter)
	for _, kind := range []string{
		announceBatchKindSyncRead,
		announceBatchKindVerifyEgressLease,
		announceBatchKindReliabilityStats,
		announceBatchKindMeasurementWrite,
	} {
		connectionAnnounceBatchesCounter.WithLabelValues(kind)
		connectionAnnounceBatchItemsCounter.WithLabelValues(kind)
		connectionAnnounceBatchRaisesCounter.WithLabelValues(kind)
	}
}

func DefaultConnectionAnnounceBatchSettings() *ConnectionAnnounceBatchSettings {
	return &ConnectionAnnounceBatchSettings{
		// a statement's arrays and a pipeline of this many items stay small,
		// and a process's due rounds fit a few batches a sweep tick
		MaxBatchCount: 512,
		// one sync round: a batch that outlives it holds every round in it
		BatchTimeout: 30 * time.Second,
	}
}

type ConnectionAnnounceBatchSettings struct {
	// the most items one batch takes; the others wait for the next. A
	// non-positive count takes one item a batch.
	MaxBatchCount int
	// bounds one batch's I/O, its retries included. Its callers wait for it.
	// A non-positive timeout leaves a batch to its I/O's own timeouts.
	BatchTimeout time.Duration
}

// A process's batcher of announcement I/O, one queue a kind. Safe for
// concurrent use; it has no goroutine while nothing waits, so it needs no
// close.
type connectionAnnounceBatcher struct {
	syncReads         *announceBatchQueue[model.ProvideKeyChangesRead, model.ProvideKeyChanges]
	verifyFeeds       *announceBatchQueue[model.VerifyConnectionEgressFeed, struct{}]
	reliabilityStats  *announceBatchQueue[model.ClientReliabilityStatsRecord, struct{}]
	measurementWrites *announceBatchQueue[connectionMeasurements, struct{}]
}

func newConnectionAnnounceBatcher(settings *ConnectionAnnounceBatchSettings) *connectionAnnounceBatcher {
	return &connectionAnnounceBatcher{
		syncReads: newAnnounceBatchQueue(
			announceBatchKindSyncRead,
			settings,
			func(ctx context.Context, reads []model.ProvideKeyChangesRead) []announceBatchOutcome[model.ProvideKeyChanges] {
				outcomes := make([]announceBatchOutcome[model.ProvideKeyChanges], len(reads))
				for i, changes := range model.GetProvideKeyChangesBatch(ctx, reads) {
					outcomes[i] = announceBatchOutcome[model.ProvideKeyChanges]{result: changes}
					if changes.Err != nil {
						outcomes[i].raised = changes.Err
					}
				}
				return outcomes
			},
		),
		verifyFeeds: newAnnounceBatchQueue(
			announceBatchKindVerifyEgressLease,
			settings,
			func(ctx context.Context, feeds []model.VerifyConnectionEgressFeed) []announceBatchOutcome[struct{}] {
				return announceBatchErrOutcomes(model.FeedVerifyConnectionEgressBatch(ctx, feeds))
			},
		),
		reliabilityStats: newAnnounceBatchQueue(
			announceBatchKindReliabilityStats,
			settings,
			func(ctx context.Context, records []model.ClientReliabilityStatsRecord) []announceBatchOutcome[struct{}] {
				// best-effort, as each record's write is: it never raises
				model.RecordClientReliabilityStatsRangeBatch(ctx, records)
				return make([]announceBatchOutcome[struct{}], len(records))
			},
		),
		measurementWrites: newAnnounceBatchQueue(
			announceBatchKindMeasurementWrite,
			settings,
			func(ctx context.Context, items []connectionMeasurements) []announceBatchOutcome[struct{}] {
				return announceBatchErrOutcomes(writeConnectionMeasurementsBatch(ctx, items))
			},
		),
	}
}

// The batcher of announcements built without one, as tests build them.
var defaultConnectionAnnounceBatcher = newConnectionAnnounceBatcher(DefaultConnectionAnnounceBatchSettings())

// Outcomes that raise each non-nil error.
func announceBatchErrOutcomes(errs []error) []announceBatchOutcome[struct{}] {
	outcomes := make([]announceBatchOutcome[struct{}], len(errs))
	for i, err := range errs {
		if err != nil {
			outcomes[i].raised = err
		}
	}
	return outcomes
}

// GetProvideKeyChanges for one sync round, in a batch. Raises what the read
// raised.
func (self *connectionAnnounceBatcher) getProvideKeyChanges(
	ctx context.Context,
	clientId server.Id,
	minTime time.Time,
) (changedCount int, provideModes map[model.ProvideMode]bool) {
	changes := self.syncReads.submit(ctx, model.ProvideKeyChangesRead{
		ClientId: clientId,
		MinTime:  minTime,
	})
	return changes.ChangedCount, changes.ProvideModes
}

// FeedVerifyConnectionEgress, or FeedVerifyProxiedConnectionEgress, for one
// connection, in a batch. Raises what the feed raised.
func (self *connectionAnnounceBatcher) feedVerifyEgress(ctx context.Context, feed model.VerifyConnectionEgressFeed) {
	self.verifyFeeds.submit(ctx, feed)
}

// RecordClientReliabilityStatsRange for one sync round, in a batch.
// Best-effort and never raises, as the record's own write: a record withdrawn
// at its connection's end is dropped, as a write the end interrupted was.
func (self *connectionAnnounceBatcher) recordReliabilityStats(ctx context.Context, record model.ClientReliabilityStatsRecord) {
	self.reliabilityStats.submitWithoutRaise(ctx, record)
}

// One connection's measurement snapshot write, in a batch. Raises what the
// write raised.
func (self *connectionAnnounceBatcher) writeMeasurements(ctx context.Context, measurements connectionMeasurements) {
	self.measurementWrites.submit(ctx, measurements)
}

// One item's result, and what the item raised: nil when it succeeded.
type announceBatchOutcome[Result any] struct {
	result Result
	raised any
}

// One submitted item. taken is guarded by its queue's stateLock; outcome is
// written by the batch before done closes.
type announceBatchCall[Item any, Result any] struct {
	item    Item
	done    chan struct{}
	taken   bool
	outcome announceBatchOutcome[Result]
}

// One kind of announcement I/O, committed in groups. run serves one batch,
// with each item's outcome in the order of the items; a raise from run fails
// every item of the batch with it. Safe for concurrent use.
type announceBatchQueue[Item any, Result any] struct {
	settings *ConnectionAnnounceBatchSettings
	run      func(ctx context.Context, items []Item) []announceBatchOutcome[Result]

	batchesCounter prometheus.Counter
	itemsCounter   prometheus.Counter
	raisesCounter  prometheus.Counter

	// Tests only, nil in production. Runs on the driver before each take,
	// outside the lock, so a test can gather the items one batch takes.
	beforeTakeForTest func()
	// Tests only, nil in production. Runs with each batch's item count
	// before the batch runs.
	batchRanForTest func(itemCount int)

	stateLock sync.Mutex
	// in submission order
	waiting []*announceBatchCall[Item, Result]
	// a driver runs: it takes the waiting items until none wait
	driving bool
}

func newAnnounceBatchQueue[Item any, Result any](
	kind string,
	settings *ConnectionAnnounceBatchSettings,
	run func(ctx context.Context, items []Item) []announceBatchOutcome[Result],
) *announceBatchQueue[Item, Result] {
	return &announceBatchQueue[Item, Result]{
		settings:       settings,
		run:            run,
		batchesCounter: connectionAnnounceBatchesCounter.WithLabelValues(kind),
		itemsCounter:   connectionAnnounceBatchItemsCounter.WithLabelValues(kind),
		raisesCounter:  connectionAnnounceBatchRaisesCounter.WithLabelValues(kind),
	}
}

// Submits item and waits for its outcome; raises what the item raised. When
// ctx ends first, an item no batch has taken is withdrawn and ctx's error is
// raised, and a taken item is waited for, so nothing of the item's I/O runs
// after submit returns.
func (self *announceBatchQueue[Item, Result]) submit(ctx context.Context, item Item) Result {
	outcome, withdrawn := self.wait(ctx, item)
	if withdrawn {
		panic(ctx.Err())
	}
	if outcome.raised != nil {
		panic(outcome.raised)
	}
	return outcome.result
}

// Submits item and waits for it, as submit, but raises nothing: an item that
// raised or was withdrawn returns as if it succeeded.
func (self *announceBatchQueue[Item, Result]) submitWithoutRaise(ctx context.Context, item Item) {
	self.wait(ctx, item)
}

// Submits item and waits for its outcome, or for ctx to end while no batch
// has taken it, which withdraws it.
func (self *announceBatchQueue[Item, Result]) wait(ctx context.Context, item Item) (outcome announceBatchOutcome[Result], withdrawn bool) {
	call := &announceBatchCall[Item, Result]{
		item: item,
		done: make(chan struct{}),
	}
	startDriver := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.waiting = append(self.waiting, call)
		if self.driving {
			return false
		}
		self.driving = true
		return true
	}()
	if startDriver {
		go self.drive()
	}
	select {
	case <-call.done:
		return call.outcome, false
	case <-ctx.Done():
	}
	withdrawn = func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if call.taken {
			return false
		}
		self.waiting = slices.DeleteFunc(self.waiting, func(waitingCall *announceBatchCall[Item, Result]) bool {
			return waitingCall == call
		})
		return true
	}()
	if withdrawn {
		return announceBatchOutcome[Result]{}, true
	}
	// a taken item finishes within its batch's timeout
	<-call.done
	return call.outcome, false
}

// Takes and runs batches until no item waits.
func (self *announceBatchQueue[Item, Result]) drive() {
	for {
		if self.beforeTakeForTest != nil {
			self.beforeTakeForTest()
		}
		calls := func() []*announceBatchCall[Item, Result] {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if len(self.waiting) == 0 {
				self.driving = false
				return nil
			}
			n := min(len(self.waiting), max(1, self.settings.MaxBatchCount))
			calls := slices.Clone(self.waiting[:n])
			for _, call := range calls {
				call.taken = true
			}
			// the calls left keep their order, in a slice without the taken
			// calls' references
			self.waiting = slices.Clone(self.waiting[n:])
			return calls
		}()
		if calls == nil {
			return
		}
		self.runBatch(calls)
	}
}

// Runs one batch within the batch timeout and gives each call its outcome.
func (self *announceBatchQueue[Item, Result]) runBatch(calls []*announceBatchCall[Item, Result]) {
	defer func() {
		for _, call := range calls {
			close(call.done)
		}
	}()
	items := make([]Item, len(calls))
	for i, call := range calls {
		items[i] = call.item
	}
	self.batchesCounter.Inc()
	self.itemsCounter.Add(float64(len(items)))
	if self.batchRanForTest != nil {
		self.batchRanForTest(len(items))
	}
	ctx, cancel := context.WithCancel(context.Background())
	if 0 < self.settings.BatchTimeout {
		ctx, cancel = context.WithTimeout(context.Background(), self.settings.BatchTimeout)
	}
	defer cancel()
	var outcomes []announceBatchOutcome[Result]
	raised := func() (raised any) {
		defer func() {
			raised = recover()
		}()
		outcomes = self.run(ctx, items)
		return nil
	}()
	if raised == nil && len(outcomes) != len(calls) {
		raised = fmt.Errorf("announce batch returned %d outcomes for %d items", len(outcomes), len(calls))
	}
	if raised != nil {
		self.raisesCounter.Inc()
		for _, call := range calls {
			call.outcome = announceBatchOutcome[Result]{raised: raised}
		}
		return
	}
	for i, call := range calls {
		call.outcome = outcomes[i]
	}
}

// The latency rows of a batch: the latest snapshot of each connection.
const connectionLatencyBatchSql = `
	INSERT INTO network_client_latency (connection_id, latency_ms, sample_count)
	SELECT * FROM unnest($1::uuid[], $2::integer[], $3::bigint[])
	ON CONFLICT (connection_id) DO UPDATE
	SET latency_ms = EXCLUDED.latency_ms, sample_count = EXCLUDED.sample_count
	WHERE network_client_latency.sample_count < EXCLUDED.sample_count
`

// The speed rows of a batch: the latest snapshot of each connection.
const connectionSpeedBatchSql = `
	INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count)
	SELECT * FROM unnest($1::uuid[], $2::bigint[], $3::bigint[])
	ON CONFLICT (connection_id) DO UPDATE
	SET bytes_per_second = EXCLUDED.bytes_per_second, sample_count = EXCLUDED.sample_count
	WHERE network_client_speed.sample_count < EXCLUDED.sample_count
`

// Writes many connections' snapshots in one transaction, a statement for the
// latency rows and one for the speed rows, each with writeMeasurements' count
// fence. Of the snapshots of one connection only the latest of each metric
// is written, which the fence makes the same as writing each in turn. A
// latency outside the column's range cannot share a statement: its snapshot
// writes alone, and fails alone, as before. The errors are in the order of
// items; a raise fails all of them.
func writeConnectionMeasurementsBatch(ctx context.Context, items []connectionMeasurements) []error {
	errs := make([]error, len(items))
	latencies := map[server.Id]connectionMeasurements{}
	speeds := map[server.Id]connectionMeasurements{}
	// in the order of items
	latencyConnectionIds := []server.Id{}
	speedConnectionIds := []server.Id{}
	for i, measurements := range items {
		if measurements.latency && math.MaxInt32 < measurements.latencyMillis {
			func() {
				defer func() {
					if r := recover(); r != nil {
						if raisedErr, ok := r.(error); ok {
							errs[i] = raisedErr
						} else {
							errs[i] = fmt.Errorf("%v", r)
						}
					}
				}()
				writeConnectionMeasurements(ctx, measurements, nil)
			}()
			continue
		}
		if measurements.latency {
			latest, ok := latencies[measurements.connectionId]
			if !ok {
				latencyConnectionIds = append(latencyConnectionIds, measurements.connectionId)
			}
			if !ok || latest.latencySampleCount < measurements.latencySampleCount {
				latencies[measurements.connectionId] = measurements
			}
		}
		if measurements.speed {
			latest, ok := speeds[measurements.connectionId]
			if !ok {
				speedConnectionIds = append(speedConnectionIds, measurements.connectionId)
			}
			if !ok || latest.speedSampleCount < measurements.speedSampleCount {
				speeds[measurements.connectionId] = measurements
			}
		}
	}
	if len(latencyConnectionIds) == 0 && len(speedConnectionIds) == 0 {
		return errs
	}
	latencyMillis := make([]int64, len(latencyConnectionIds))
	latencySampleCounts := make([]int64, len(latencyConnectionIds))
	for i, connectionId := range latencyConnectionIds {
		measurements := latencies[connectionId]
		latencyMillis[i] = int64(measurements.latencyMillis)
		// addMeasurementsWithLock caps counts at the bigint limit
		latencySampleCounts[i] = int64(measurements.latencySampleCount)
	}
	bytesPerSeconds := make([]int64, len(speedConnectionIds))
	speedSampleCounts := make([]int64, len(speedConnectionIds))
	for i, connectionId := range speedConnectionIds {
		measurements := speeds[connectionId]
		bytesPerSeconds[i] = int64(measurements.bytesPerSecond)
		speedSampleCounts[i] = int64(measurements.speedSampleCount)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if 0 < len(latencyConnectionIds) {
			server.RaisePgResult(tx.Exec(ctx, connectionLatencyBatchSql, latencyConnectionIds, latencyMillis, latencySampleCounts))
		}
		if 0 < len(speedConnectionIds) {
			server.RaisePgResult(tx.Exec(ctx, connectionSpeedBatchSql, speedConnectionIds, bytesPerSeconds, speedSampleCounts))
		}
	})
	return errs
}
