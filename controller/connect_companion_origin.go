package controller

// Companion creation waits briefly for the opposite-direction origin without
// retaining a database transaction between attempts. Each request owns its wait.

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/model"
)

// Keep the first cold-start retry fast while bounding a genuinely absent origin.
const CompanionOriginWaitTimeout = 3 * time.Second
const CompanionOriginWaitPollTimeout = 100 * time.Millisecond
const companionOriginFallbackTimeout = 500 * time.Millisecond

var companionOriginLookupCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_connect_companion_origin_lookups_total",
	Help: "Authoritative origin lookup attempts by a fixed wake source; each lookup may execute primary and chained-origin reads",
}, []string{"source"})
var companionOriginWakeCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_connect_companion_origin_wait_wakes_total",
	Help: "Companion-origin waits woken by an event, timed fallback, deadline, or caller cancellation",
}, []string{"source"})
var companionOriginLookupHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "urnetwork_connect_companion_origin_lookups_per_request",
	Help:    "Authoritative origin lookup attempts in a completed companion request, including immediate hits and canceled waits",
	Buckets: []float64{0, 1, 2, 4, 8, 16, 32},
})

func init() {
	for _, source := range []string{"initial", "event", "fallback", "deadline"} {
		companionOriginLookupCounter.WithLabelValues(source)
	}
	for _, source := range []string{"event", "fallback", "deadline", "cancel"} {
		companionOriginWakeCounter.WithLabelValues(source)
	}
	prometheus.MustRegister(companionOriginLookupCounter, companionOriginWakeCounter, companionOriginLookupHistogram)
}

// A watch shares only missing reads; the controller still owns each creation
// and forces an independent authoritative read at its own final deadline.
type companionOriginLookup interface {
	Lookup(context.Context, bool, func() (*model.TransferEscrow, error)) (*model.TransferEscrow, error)
}

// Only an absent origin is retryable. A successful creation is returned exactly
// once, even if the caller's context expires during its committed transaction.
func waitForCompanionOrigin(
	ctx context.Context,
	shared companionOriginLookup,
	createEscrow func() (*model.TransferEscrow, error),
	updates ...func() <-chan struct{},
) (*model.TransferEscrow, error) {
	deadline := time.Now().Add(CompanionOriginWaitTimeout)
	attempts := 0
	lookups := 0
	defer func() { companionOriginLookupHistogram.Observe(float64(lookups)) }()
	var lastLookup time.Time
	source := "initial"
	for {
		if ctx.Err() != nil {
			companionOriginWakeCounter.WithLabelValues("cancel").Inc()
			return nil, ctx.Err()
		}
		// Coalesced hints must not turn a reconnect or duplicate-message burst
		// into a tighter query loop than the original first-retry interval.
		if !lastLookup.IsZero() {
			if delay := min(time.Until(deadline), time.Until(lastLookup.Add(CompanionOriginWaitPollTimeout))); delay > 0 {
				select {
				case <-ctx.Done():
					companionOriginWakeCounter.WithLabelValues("cancel").Inc()
					return nil, ctx.Err()
				case <-time.After(delay):
				}
			}
		}
		if ctx.Err() != nil {
			continue
		}
		var update <-chan struct{}
		if len(updates) > 0 && updates[0] != nil {
			update = updates[0]()
		}
		// Subscribe immediately before the authoritative read. A commit or
		// subscription acknowledgement during that read remains observable.
		lastLookup = time.Now()
		attempts++
		queried := false
		create := func() (*model.TransferEscrow, error) {
			queried = true
			lookups++
			companionOriginLookupCounter.WithLabelValues(source).Inc()
			return createEscrow()
		}
		var escrow *model.TransferEscrow
		var err error
		if shared == nil {
			escrow, err = create()
		} else {
			escrow, err = shared.Lookup(ctx, !lastLookup.Before(deadline), create)
		}
		// Another request's read may finish across our deadline. Its absence
		// cannot replace our final snapshot; committed successes stay one-shot.
		if !queried && errors.Is(err, model.ErrMissingCompanionOrigin) && !time.Now().Before(deadline) {
			source = "deadline"
			escrow, err = shared.Lookup(ctx, true, create)
		}
		if !errors.Is(err, model.ErrMissingCompanionOrigin) {
			return escrow, err
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		fallback := companionOriginFallbackTimeout
		if attempts == 1 {
			fallback = CompanionOriginWaitPollTimeout
		}
		wait := min(fallback, time.Until(deadline))
		select {
		case <-ctx.Done():
			companionOriginWakeCounter.WithLabelValues("cancel").Inc()
			return nil, ctx.Err()
		case <-update:
			source = "event"
		case <-time.After(wait):
			source = "fallback"
			if !time.Now().Before(deadline) {
				source = "deadline"
			}
		}
		companionOriginWakeCounter.WithLabelValues(source).Inc()
	}
}
