package connect

// The provider liveness lease writes of connect (model/provider_liveness_lease.go):
// each announcement's refreshes of its client's lease, and each process's
// stamp of its handler in the writer set, which tells the API readers that
// every lease this process's connections hold is being written.

import (
	"context"
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
		Help:      "Provider liveness lease writes by outcome: refreshed and refresh_failed, once per sync round of a connection whose client provides for discovery; released and release_failed, when the client stops providing; stamped and stamp_failed, the process's handler stamp in the writer set",
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
	} {
		connectionLivenessLeaseWritesCounter.WithLabelValues(outcome)
	}
}

// Whether the client is in the pool FindProviders2 draws from: Public for any
// caller, Network for callers in its own network.
func providesForDiscovery(provideModes map[model.ProvideMode]bool) bool {
	return provideModes[model.ProvideModePublic] || provideModes[model.ProvideModeNetwork]
}

// One announcement's part in its client's liveness lease. Only the announce
// run goroutine touches it.
//
// While the client provides for discovery, as the last successful sync read
// found, each round refreshes the lease before its read, so a round whose read
// fails still refreshes it. The read that first finds the client providing
// refreshes it at once, not a round later; the read that finds it no longer
// providing releases it, and a failed release is retried by each later round
// and at the end. The end of a providing connection writes nothing: the lease
// is the client's, another connection of the client or the one it reconnects
// with may hold it, and it lapses within the window.
type providerLivenessLeaseConnection struct {
	clientId     server.Id
	refreshLease func(context.Context, server.Id) error
	releaseLease func(context.Context, server.Id) error
	// observes each write's outcome; nil in production
	writtenForTest func(outcome string)

	// whether the client provides for discovery, as the last successful read found
	providing bool
	// the client stopped providing and its release has not been written
	releasePending bool
}

// A connection of clientId that writes the lease through the model.
func newProviderLivenessLeaseConnection(clientId server.Id, writtenForTest func(string)) *providerLivenessLeaseConnection {
	return &providerLivenessLeaseConnection{
		clientId:       clientId,
		refreshLease:   model.RefreshProviderLivenessLease,
		releaseLease:   model.ReleaseProviderLivenessLease,
		writtenForTest: writtenForTest,
	}
}

// Refreshes the lease while the client provides, before the round's read.
func (self *providerLivenessLeaseConnection) beforeRead(ctx context.Context) {
	if self.providing {
		self.refresh(ctx)
	}
}

// Takes the provide modes of the round's successful read.
func (self *providerLivenessLeaseConnection) afterRead(ctx context.Context, provideModes map[model.ProvideMode]bool) {
	providing := providesForDiscovery(provideModes)
	if providing {
		self.releasePending = false
		if !self.providing {
			self.refresh(ctx)
		}
	} else if self.providing || self.releasePending {
		self.releasePending = true
		self.release(ctx)
	}
	self.providing = providing
}

// Retries a release that has not been written, with a cleanup context: the
// transport's context is canceled by now. Leaves a providing client's lease
// to lapse.
func (self *providerLivenessLeaseConnection) end(ctx context.Context) {
	if !self.releasePending {
		return
	}
	cleanupCtx, cleanupCancel := connectionCleanupContext(ctx)
	defer cleanupCancel()
	self.release(cleanupCtx)
}

func (self *providerLivenessLeaseConnection) refresh(ctx context.Context) {
	if err := self.refreshLease(ctx, self.clientId); err != nil {
		self.written(ctx, livenessLeaseOutcomeRefreshFailed, err)
		return
	}
	self.written(ctx, livenessLeaseOutcomeRefreshed, nil)
}

func (self *providerLivenessLeaseConnection) release(ctx context.Context) {
	if err := self.releaseLease(ctx, self.clientId); err != nil {
		self.written(ctx, livenessLeaseOutcomeReleaseFailed, err)
		return
	}
	self.releasePending = false
	self.written(ctx, livenessLeaseOutcomeReleased, nil)
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
// ends. A failed stamp is counted and written again at the next interval;
// readers fail open once the stamp is older than its window.
func runProviderLivenessWriter(
	ctx context.Context,
	handlerId server.Id,
	interval time.Duration,
	stamp func(context.Context, server.Id) error,
) {
	for {
		if err := stamp(ctx, handlerId); err != nil {
			if ctx.Err() != nil {
				return
			}
			connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeStampFailed).Inc()
			if glog.V(1) {
				glog.Infof("[t]liveness writer stamp failed: %s\n", err)
			}
		} else {
			connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeStamped).Inc()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
