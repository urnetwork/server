package connect

// The provider liveness lease writes of connect (model/provider_liveness_lease.go):
// each providing connection's refreshes of its client's lease, written by a
// worker of the connection's own, and each process's stamp of its handler in
// the writer set, which tells the API readers that every lease this process's
// connections hold is being written.
//
// A refresh does not wait for the connection's sync rounds. A round can hold
// for minutes, on a database acquisition without a deadline or on a failed
// read's retries and backoff, and a lease lapses 150 s after its last refresh.
// The stamp vouches for the refreshes themselves: the process withholds it
// while any of its providing connections has gone
// model.ProviderLivenessWriterWithholdAge without a successful refresh, so a
// process whose lease writes stop, for one connection or for all, stops
// vouching before any of its leases lapses.

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var connectionLivenessLeaseWritesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "liveness_lease_writes_total",
		Help:      "Provider liveness lease writes by outcome: refreshed and refresh_failed, every 30 s from each connection whose client provides for discovery; released and release_failed, when a read finds the client no longer providing; stamped, stamp_failed and stamp_withheld, the process's handler stamp in the writer set every 5 s, withheld while a providing connection has gone 75 s without a successful refresh",
	},
	[]string{"outcome"},
)

const (
	livenessLeaseOutcomeRefreshed     = "refreshed"
	livenessLeaseOutcomeRefreshFailed = "refresh_failed"
	livenessLeaseOutcomeReleased      = "released"
	livenessLeaseOutcomeReleaseFailed = "release_failed"
	livenessLeaseOutcomeStamped       = "stamped"
	livenessLeaseOutcomeStampFailed   = "stamp_failed"
	livenessLeaseOutcomeStampWithheld = "stamp_withheld"
)

func init() {
	prometheus.MustRegister(connectionLivenessLeaseWritesCounter)
	for _, outcome := range []string{
		livenessLeaseOutcomeRefreshed,
		livenessLeaseOutcomeRefreshFailed,
		livenessLeaseOutcomeReleased,
		livenessLeaseOutcomeReleaseFailed,
		livenessLeaseOutcomeStamped,
		livenessLeaseOutcomeStampFailed,
		livenessLeaseOutcomeStampWithheld,
	} {
		connectionLivenessLeaseWritesCounter.WithLabelValues(outcome)
	}
}

// Whether the client is in the pool FindProviders2 draws from: Public for any
// caller, Network for callers in its own network.
func providesForDiscovery(provideModes map[model.ProvideMode]bool) bool {
	return provideModes[model.ProvideModePublic] || provideModes[model.ProvideModeNetwork]
}

// Each providing connection's last successful refresh, process wide. A
// connection enters at the read that first finds its client providing, moves
// with each refresh that succeeds, and leaves when a read finds its client no
// longer providing or the connection ends. Safe for concurrent use.
type providerLivenessRefreshes struct {
	stateLock              sync.Mutex
	connectionRefreshTimes map[*providerLivenessLeaseConnection]time.Time
}

func newProviderLivenessRefreshes() *providerLivenessRefreshes {
	return &providerLivenessRefreshes{
		connectionRefreshTimes: map[*providerLivenessLeaseConnection]time.Time{},
	}
}

var defaultProviderLivenessRefreshes = newProviderLivenessRefreshes()

func (self *providerLivenessRefreshes) set(connection *providerLivenessLeaseConnection, refreshTime time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectionRefreshTimes[connection] = refreshTime
}

func (self *providerLivenessRefreshes) remove(connection *providerLivenessLeaseConnection) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.connectionRefreshTimes, connection)
}

// The earliest last refresh of any providing connection, and whether there is
// one.
func (self *providerLivenessRefreshes) oldest() (oldestTime time.Time, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, refreshTime := range self.connectionRefreshTimes {
		if !ok || refreshTime.Before(oldestTime) {
			oldestTime, ok = refreshTime, true
		}
	}
	return
}

// One announcement's part in its client's liveness lease.
//
// The round goroutine reports each successful sync read (afterRead) and the
// connection's end (end). The read that first finds the client providing
// starts the connection's worker, the only writer of its lease, which
// refreshes at once and then every refresh interval while the client
// provides. The read that finds the client no longer providing has the worker
// release the lease once; a failed release is requested again only by a later
// read that still finds the client not providing, never from older state. The
// end writes nothing: the lease is the client's, another connection of the
// client or the one it reconnects with may hold it, and it lapses within the
// window. Safe for concurrent use by the round goroutine and the worker.
type providerLivenessLeaseConnection struct {
	clientId        server.Id
	refreshes       *providerLivenessRefreshes
	refreshInterval time.Duration
	now             func() time.Time
	startWorker     func(func()) bool
	refreshLease    func(context.Context, server.Id) error
	releaseLease    func(context.Context, server.Id) error
	// observes each write's outcome; nil in production
	writtenForTest func(outcome string)
	wake           chan struct{}

	stateLock sync.Mutex
	// whether the client provides for discovery, as the last successful read found
	providing bool
	// a read found the client no longer providing; the worker releases once
	releaseRequested bool
	// a requested release failed; the next read that finds the client not
	// providing requests it again
	releasePending bool
	workerStarted  bool
	ended          bool
}

// A connection of clientId that writes through the model and the process's
// refreshes, refreshing every refreshInterval (0 for the default) from a
// worker that startWorker runs.
func newProviderLivenessLeaseConnection(
	clientId server.Id,
	refreshInterval time.Duration,
	startWorker func(func()) bool,
	writtenForTest func(string),
) *providerLivenessLeaseConnection {
	if refreshInterval <= 0 {
		refreshInterval = model.ProviderLivenessLeaseRefreshInterval
	}
	return &providerLivenessLeaseConnection{
		clientId:        clientId,
		refreshes:       defaultProviderLivenessRefreshes,
		refreshInterval: refreshInterval,
		now:             time.Now,
		startWorker:     startWorker,
		refreshLease:    model.RefreshProviderLivenessLease,
		releaseLease:    model.ReleaseProviderLivenessLease,
		writtenForTest:  writtenForTest,
		wake:            make(chan struct{}, 1),
	}
}

// Takes the provide modes of a round's successful read. ctx is the
// connection's, and the worker's.
func (self *providerLivenessLeaseConnection) afterRead(ctx context.Context, provideModes map[model.ProvideMode]bool) {
	providing := providesForDiscovery(provideModes)
	start := false
	wake := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ended {
			return
		}
		if providing {
			self.releaseRequested = false
			self.releasePending = false
			if !self.providing {
				// the process vouches for this connection from now
				self.refreshes.set(self, self.now())
				if self.workerStarted {
					wake = true
				} else {
					self.workerStarted = true
					start = true
				}
			}
		} else {
			if self.providing {
				self.refreshes.remove(self)
			}
			if self.providing || self.releasePending {
				self.releaseRequested = true
				self.releasePending = false
				wake = true
			}
		}
		self.providing = providing
	}()
	if start {
		self.startWorker(func() {
			self.run(ctx)
		})
	} else if wake {
		select {
		case self.wake <- struct{}{}:
		default:
		}
	}
}

// Ends the connection's part: the process no longer vouches for it, and
// nothing more is written, a pending release included.
func (self *providerLivenessLeaseConnection) end() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ended = true
	self.refreshes.remove(self)
}

// Writes what the state asks for at once, then whenever a read wakes it and
// every refresh interval, until ctx ends.
func (self *providerLivenessLeaseConnection) run(ctx context.Context) {
	for {
		self.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-self.wake:
		case <-time.After(self.refreshInterval):
		}
	}
}

// One write: a refresh while the client provides, else a requested release.
func (self *providerLivenessLeaseConnection) step(ctx context.Context) {
	refresh := false
	release := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ended {
			return
		}
		if self.providing {
			refresh = true
		} else if self.releaseRequested {
			release = true
			self.releaseRequested = false
		}
	}()
	if refresh {
		if err := self.refreshLease(ctx, self.clientId); err != nil {
			self.written(ctx, livenessLeaseOutcomeRefreshFailed, err)
			return
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.providing && !self.ended {
				self.refreshes.set(self, self.now())
			}
		}()
		self.written(ctx, livenessLeaseOutcomeRefreshed, nil)
	} else if release {
		if err := self.releaseLease(ctx, self.clientId); err != nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if !self.providing {
					self.releasePending = true
				}
			}()
			self.written(ctx, livenessLeaseOutcomeReleaseFailed, err)
			return
		}
		self.written(ctx, livenessLeaseOutcomeReleased, nil)
	}
}

// Counts one write. A write that failed because its context ended is the
// connection's end, not a failure, and is not counted.
func (self *providerLivenessLeaseConnection) written(ctx context.Context, outcome string, err error) {
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if glog.V(1) {
			glog.Infof("[t]liveness lease %s: %s\n", outcome, err)
		}
	}
	connectionLivenessLeaseWritesCounter.WithLabelValues(outcome).Inc()
	if self.writtenForTest != nil {
		self.writtenForTest(outcome)
	}
}

// Stamps handlerId in the writer set at once and then every interval until ctx
// ends, unless refreshes says a providing connection of this process has gone
// the withhold age without a successful refresh (stampProviderLivenessWriter).
func runProviderLivenessWriter(
	ctx context.Context,
	handlerId server.Id,
	interval time.Duration,
	refreshes *providerLivenessRefreshes,
	stamp func(context.Context, server.Id) error,
) {
	for {
		if outcome := stampProviderLivenessWriter(ctx, handlerId, refreshes, time.Now(), stamp); outcome != "" {
			connectionLivenessLeaseWritesCounter.WithLabelValues(outcome).Inc()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// One stamp at now, or none: withheld while any providing connection's last
// successful refresh is more than model.ProviderLivenessWriterWithholdAge
// before now. Returns the outcome, "" when ctx ended the stamp.
func stampProviderLivenessWriter(
	ctx context.Context,
	handlerId server.Id,
	refreshes *providerLivenessRefreshes,
	now time.Time,
	stamp func(context.Context, server.Id) error,
) string {
	if oldestTime, ok := refreshes.oldest(); ok && model.ProviderLivenessWriterWithholdAge < now.Sub(oldestTime) {
		return livenessLeaseOutcomeStampWithheld
	}
	if err := stamp(ctx, handlerId); err != nil {
		if ctx.Err() != nil {
			return ""
		}
		if glog.V(1) {
			glog.Infof("[t]liveness writer stamp failed: %s\n", err)
		}
		return livenessLeaseOutcomeStampFailed
	}
	return livenessLeaseOutcomeStamped
}
