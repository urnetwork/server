// Attribute live JWT-state SQL attempts to fixed, server-selected operations.
// These counters retain no identities and grant no cached authorization.
package session

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Only a server call site selects a source. Invalid values and unannotated
// callers share an explicit unknown bucket; request headers cannot select it.
type StateQuerySource uint8

const (
	StateQueryUnknown StateQuerySource = iota
	StateQueryApiControl
	StateQueryApiMint
	StateQueryApiRetire
	StateQueryApiDiscovery
	StateQueryApiRefresh
	StateQueryApiOther
	StateQueryConnectH1
	StateQueryConnectH3
	StateQueryHostedBootstrap
	StateQueryHostedMint
	StateQueryHostedRetire
	StateQueryHostedControl
	StateQueryHostedDiscovery
	StateQueryHostedRefresh
	StateQueryHostedPublicKey
	StateQueryHostedRead
	StateQueryHostedOther
	StateQueryProberMint
	StateQueryProberRetire
	StateQueryProberControl
	stateQuerySourceCount
)

var stateQuerySourceLabels = [stateQuerySourceCount][2]string{
	{"unknown", "unknown"},
	{"api", "control"},
	{"api", "mint"},
	{"api", "retire"},
	{"api", "discovery"},
	{"api", "refresh"},
	{"api", "other"},
	{"connect_h1", "handshake"},
	{"connect_h3", "handshake"},
	{"hosted", "bootstrap"},
	{"hosted", "mint"},
	{"hosted", "retire"},
	{"hosted", "control"},
	{"hosted", "discovery"},
	{"hosted", "refresh"},
	{"hosted", "public_key"},
	{"hosted", "read"},
	{"hosted", "other"},
	{"prober", "mint"},
	{"prober", "retire"},
	{"prober", "control"},
}

type stateQuerySourceKey struct{}

// Copies only a closed-vocabulary source into the caller's existing lifetime.
func WithStateQuerySource(ctx context.Context, source StateQuerySource) context.Context {
	if stateQuerySourceCount <= source {
		source = StateQueryUnknown
	}
	return context.WithValue(ctx, stateQuerySourceKey{}, source)
}

type stateQueryOutcome uint8

const (
	stateQueryError stateQueryOutcome = iota
	stateQueryValid
	stateQueryNoActiveRow
	stateQueryCredentialRotated
	stateQueryCanceled
	stateQueryDeadline
	stateQueryOutcomeCount
)

var stateQueryOutcomeLabels = [stateQueryOutcomeCount]string{
	"query_error", "state_valid", "no_active_row", "credential_rotated", "canceled", "deadline",
}

var stateQueryCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "jwt", Name: "state_queries_total",
	Help: "JWT-state SQL query attempts recorded on return or unwind by trusted caller, operation, credential shape and Go state verdict; retries count separately, state_valid is not request success, unknown marks unannotated callers.",
}, []string{"caller", "operation", "credential", "outcome"})

// Resolve bounded child handles once, avoiding label-map work on each query.
var stateQueryCounters [stateQuerySourceCount][2][stateQueryOutcomeCount]prometheus.Counter

func init() {
	for source, labels := range stateQuerySourceLabels {
		for credential, shape := range [...]string{"account", "client"} {
			for outcome, label := range stateQueryOutcomeLabels {
				stateQueryCounters[source][credential][outcome] = stateQueryCounter.WithLabelValues(labels[0], labels[1], shape, label)
			}
		}
	}
	prometheus.MustRegister(stateQueryCounter)
}

// An observation belongs to one actual connection or transaction Query attempt, after acquisition.
// Pool failures before that boundary, signature rejection and missing claims
// create no observation. A database callback retry creates another one.
type stateQueryObservation struct {
	source     StateQuerySource
	credential int
	outcome    stateQueryOutcome
}

// Starts immediately before the chosen account/client statement is sent.
func beginStateQuery(ctx context.Context, client bool) stateQueryObservation {
	source, _ := ctx.Value(stateQuerySourceKey{}).(StateQuerySource)
	if stateQuerySourceCount <= source {
		source = StateQueryUnknown
	}
	credential := 0
	if client {
		credential = 1
	}
	return stateQueryObservation{source: source, credential: credential, outcome: stateQueryError}
}

// A returned row may still fail credential rotation. Mirror the unchanged
// caller's Go verdict only after the result has closed successfully.
func (self *stateQueryObservation) complete(found bool, createTime, changeTime time.Time) {
	switch {
	case !found:
		self.outcome = stateQueryNoActiveRow
	case createTime.Before(changeTime):
		self.outcome = stateQueryCredentialRotated
	default:
		self.outcome = stateQueryValid
	}
}

// Deferred completion observes errors and panics without recovering them or
// changing retry policy. Cancellation is distinct from a definitive rejection.
func (self *stateQueryObservation) finish(ctx context.Context) {
	if self.outcome == stateQueryError {
		switch ctx.Err() {
		case context.Canceled:
			self.outcome = stateQueryCanceled
		case context.DeadlineExceeded:
			self.outcome = stateQueryDeadline
		}
	}
	stateQueryCounters[self.source][self.credential][self.outcome].Inc()
}
