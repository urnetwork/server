// The request-time reader of the ARIN canary's published decision
// (arindbctl/ARINDB.md, "Publication and request-time consistency"). Readers
// evaluate stored facts for the published serving epoch S and choose each
// row's slot by its epoch, so rows written before and after a restart agree.
// Without a published decision (absent, expired or invalid) a reader uses each
// row's latest slot, as before the canary, and counts the event: refusing
// Quality instead would let a lost key empty it. A Redis error or timeout
// keeps the last decision the process read for a bounded time, so a blip
// cannot drop an active fallback either.
//
// Hard exclusions, probe admission and URL eligibility do not read the
// decision: risk always comes from the latest slot.
package model

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// How long a process reuses the decision it read. Readers switch within this
// of a new decision, while the export rewrites targets over minutes.
const arindbServingEpochCacheTtl = 5 * time.Second

// The bound of one shared read. It is detached from the request that happens
// to trigger it, so one canceled request cannot decide for every reader.
const arindbDecisionReadTimeout = 250 * time.Millisecond

// How long failed reads keep serving the epoch of the last read that found a
// decision. Publications write one every few minutes, so this outlasts a
// Redis blip but not an outage, after which readers serve each row's latest
// slot as without a decision.
const arindbServingEpochErrorHold = 15 * time.Minute

// The outcome of one read of the published decision.
const (
	ArindbDecisionReadOk      = "ok"
	ArindbDecisionReadAbsent  = "absent"
	ArindbDecisionReadInvalid = "invalid"
	ArindbDecisionReadError   = "error"
)

var arindbDecisionReadOutcomes = []string{
	ArindbDecisionReadOk,
	ArindbDecisionReadAbsent,
	ArindbDecisionReadInvalid,
	ArindbDecisionReadError,
}

var arindbDecisionReads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_arindb_decision_reads_total",
	Help: "Reads of the published ARIN canary decision by request-time readers, by outcome; absent and invalid serve each row's latest ARIN slot, error keeps the last decision read for up to 15 minutes",
}, []string{"outcome"})

func init() {
	for _, outcome := range arindbDecisionReadOutcomes {
		arindbDecisionReads.WithLabelValues(outcome)
	}
	prometheus.MustRegister(arindbDecisionReads)
}

// The process's serving epoch, read at most once per ttl. Concurrent readers
// share a refresh: while one reads, the others serve the previous value,
// or 0 until the first read completes. Only that reader publishes its result.
// Safe for concurrent use.
type arindbServingEpochCache struct {
	stateLock  sync.Mutex
	epoch      int64
	readAt     time.Time
	known      bool
	refreshing bool
	// the last read that found a decision, which failed reads keep serving
	// for arindbServingEpochErrorHold
	lastOkEpoch int64
	lastOkAt    time.Time
	hasLastOk   bool
}

var currentArindbServingEpoch = &arindbServingEpochCache{}

// The published serving epoch, 0 without a published decision.
func readArindbServingEpoch(ctx context.Context) int64 {
	return currentArindbServingEpoch.get(ctx, time.Now, readArindbServingEpochFromStore)
}

// The published serving epoch for readers outside the model, such as the
// provider-egress snapshot key; 0 without a readable decision.
func ArindbServingEpoch(ctx context.Context) int64 {
	return readArindbServingEpoch(ctx)
}

// Admits one refresh even before the first result exists.
func (self *arindbServingEpochCache) get(
	ctx context.Context,
	now func() time.Time,
	read func(context.Context) (int64, string),
) (servedEpoch int64) {
	refresh := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		servedEpoch = self.epoch
		if self.refreshing || (self.known && now().Sub(self.readAt) < arindbServingEpochCacheTtl) {
			return
		}
		self.refreshing = true
		refresh = true
	}()
	if !refresh {
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), arindbDecisionReadTimeout)
	defer cancel()
	// recorded even if the read panics, as an error, so a failed refresh can
	// never leave the cache refreshing forever
	readEpoch, outcome := int64(0), ArindbDecisionReadError
	defer func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			readAt := now()
			switch outcome {
			case ArindbDecisionReadOk:
				self.lastOkEpoch, self.lastOkAt, self.hasLastOk = readEpoch, readAt, true
			case ArindbDecisionReadError:
				// a failed read or timeout keeps the last decision read, for
				// a bounded time
				if self.hasLastOk && readAt.Sub(self.lastOkAt) < arindbServingEpochErrorHold {
					readEpoch = self.lastOkEpoch
				}
			default:
				// absent, expired or invalid: no decision is published
				self.hasLastOk = false
			}
			self.epoch = readEpoch
			self.readAt = readAt
			self.known = true
			self.refreshing = false
			servedEpoch = readEpoch
		}()
		arindbDecisionReads.WithLabelValues(outcome).Inc()
	}()
	readEpoch, outcome = read(readCtx)
	return
}

// One read of the published decision and its outcome.
func readArindbServingEpochFromStore(ctx context.Context) (int64, string) {
	data, err := arindbCanaryDecisions.get(ctx)
	if err != nil {
		return 0, ArindbDecisionReadError
	}
	if data == nil {
		return 0, ArindbDecisionReadAbsent
	}
	decision, err := parseArindbCanaryDecision(data)
	if err != nil {
		return 0, ArindbDecisionReadInvalid
	}
	return decision.ServingEpoch, ArindbDecisionReadOk
}

// newProviderCountFilterForClients with Quality facts selected by the
// published serving epoch, for the readers that report Quality: provider
// status, the provider-egress counts and inspection. Risk stays the latest
// slot's. The decision is read before any database connection is taken.
func newProviderQualityFilterForClients(ctx context.Context, clientIds []server.Id) providerCountFilter {
	servingEpoch := readArindbServingEpoch(ctx)
	f := newProviderCountFilterForClients(ctx, clientIds)
	if servingEpoch != 0 && len(clientIds) != 0 {
		applyArindbServingEpoch(&f, clientIds, loadArindbRollupSlots(ctx, clientIds), servingEpoch)
	}
	return f
}
