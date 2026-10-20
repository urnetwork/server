// A probe tunnel's derived client keeps acknowledging its provider's late
// returns after the tunnel closes, until no return can still be on its way,
// and only then is it revoked.
//
// A provider on SDK v2026.8.31 or later keeps every return it read from an
// origin until the derived client acknowledges it, and retransmits it on a
// resend timer that backs off to 8s. Providers before the September 2026
// sdks never give up: once the derived client is revoked, every such return
// is retried against the platform every few seconds for as long as the
// provider stays connected, as a contract refused for an inactive
// destination. On 2026-10-19 about 2% of URL probes left one, and they
// accumulated to about 70k routes and 15k refused contracts a second between
// fleet-wide restarts.
//
// The teardown drain (providerReturnDrain.wait) ends the provider's flows
// and waits for one quiet second. A return the receive path refused, or the
// network lost, comes back only when its resend timer fires, so acknowledge
// waits for a quiet interval longer than the longest resend wait, counted
// only while the provider is in the multi-client to return through. A pass's
// Retirements holds the closed tunnel for that wait so its probe slot is not
// held, bounded by count, and joins every held tunnel before the pass ends.
package providertunnel

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// How a tunnel came to be closed, as its retirement metrics label it.
const (
	// its owner closed it with its provider present and its caller live
	retirementPathClosed = "closed"
	// its caller's context ended first: a pass canceled by a deploy, a
	// shutdown or its deadline
	retirementPathCanceled = "canceled"
	// its provider left the multi-client first (Lost), as a probe that
	// re-created the tunnel retires the old one
	retirementPathLost = "lost"
)

var retirementPaths = []string{retirementPathClosed, retirementPathCanceled, retirementPathLost}

// How the acknowledgment of late provider returns ended.
const (
	// a whole acknowledge interval passed with no return while the provider
	// was present: nothing it holds can still be on its way
	retirementOutcomeQuiet = "quiet"
	// the acknowledge timeout ended it with the provider present and still
	// returning, or never quiet for a whole interval
	retirementOutcomeTimeBound = "time_bound"
	// the acknowledge timeout ended it with no provider in the multi-client
	// to return through
	retirementOutcomeNoPath = "no_path"
	// the tunnel sent its provider no packet, or the provider never joined
	// the multi-client, so it holds no return for the derived client
	retirementOutcomeUnused = "unused"
	// the wait was canceled
	retirementOutcomeCut = "cut"
)

var retirementOutcomes = []string{
	retirementOutcomeQuiet,
	retirementOutcomeTimeBound,
	retirementOutcomeNoPath,
	retirementOutcomeUnused,
	retirementOutcomeCut,
}

var tunnelRetirementCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_tunnel_retirements_total",
	Help: "Probe tunnels whose derived client was revoked, by how the tunnel was closed (path: closed; canceled, its caller's context ended first; lost, its provider left first) and how acknowledging late provider returns ended (outcome: quiet, a whole acknowledge interval with no return and the provider present; time_bound, the timeout ended it with the provider present; no_path, the timeout ended it with no provider to return through; unused, the tunnel sent its provider nothing; cut, the wait was canceled)",
}, []string{"path", "outcome"})

var tunnelRetirementLateReturnCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_tunnel_retirement_late_returns_total",
	Help: "Provider returns acknowledged after the teardown drain went quiet, by path. The drain alone revoked the derived client before each of them, and a provider sdk before September 2026 retransmits such a return to the revoked client indefinitely",
}, []string{"path"})

var tunnelRetirementTrailingReturnCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_tunnel_retirement_trailing_returns_total",
	Help: "Provider returns in the last interval of an acknowledgment that ended without a quiet one, by path and outcome: still arriving as the derived client was revoked",
}, []string{"path", "outcome"})

var tunnelRetirementHeldGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_provider_tunnel_retirements_held",
	Help: "Closed probe tunnels that passes' Retirements hold while their derived clients acknowledge late provider returns",
})

var tunnelRetirementBackpressureCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_provider_tunnel_retirement_backpressure_total",
	Help: "Probe tunnel closes that acknowledged late returns in the caller because their pass's Retirements had no room",
})

var tunnelRetirementErrorCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_provider_tunnel_retirement_errors_total",
	Help: "Held probe tunnel retirements whose teardown reported an error; a tunnel retired in its own Close returns the error instead",
})

// Registers the retirement metrics, every label combination at zero.
func init() {
	prometheus.MustRegister(
		tunnelRetirementCounter,
		tunnelRetirementLateReturnCounter,
		tunnelRetirementTrailingReturnCounter,
		tunnelRetirementHeldGauge,
		tunnelRetirementBackpressureCounter,
		tunnelRetirementErrorCounter,
	)
	for _, path := range retirementPaths {
		tunnelRetirementLateReturnCounter.WithLabelValues(path)
		for _, outcome := range retirementOutcomes {
			tunnelRetirementCounter.WithLabelValues(path, outcome)
			tunnelRetirementTrailingReturnCounter.WithLabelValues(path, outcome)
		}
	}
}

// The verdict of acknowledge.
type returnAcknowledgment struct {
	outcome string
	// returns that arrived during the wait: each one the drain alone left for
	// a revoked client
	lateReturnCount uint64
	// returns in the last interval when the wait ended without a quiet one
	trailingReturnCount uint64
}

// Counts one revoked derived client by path and verdict.
func recordTunnelRetirement(path string, acknowledgment returnAcknowledgment) {
	tunnelRetirementCounter.WithLabelValues(path, acknowledgment.outcome).Inc()
	tunnelRetirementLateReturnCounter.WithLabelValues(path).Add(float64(acknowledgment.lateReturnCount))
	tunnelRetirementTrailingReturnCounter.WithLabelValues(path, acknowledgment.outcome).Add(float64(acknowledgment.trailingReturnCount))
}

// Keeps the derived client acknowledging after the drain until a whole
// acknowledge interval passes with no provider return while providerUp says
// the provider is in the multi-client, so a return on its way can still
// arrive. A quiet interval with the provider gone does not count: its
// returns wait for the path. Bounded by the acknowledge timeout and by ctx.
func (self *providerReturnDrain) acknowledge(ctx context.Context, providerUp func() bool) returnAcknowledgment {
	timeout := self.after(self.acknowledgeTimeout)
	startCount := self.returnCount.Load()
	lastCount := startCount
	ended := func(outcome string) returnAcknowledgment {
		count := self.returnCount.Load()
		return returnAcknowledgment{
			outcome:             outcome,
			lateReturnCount:     count - startCount,
			trailingReturnCount: count - lastCount,
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ended(retirementOutcomeCut)
		case <-timeout:
			if providerUp() {
				return ended(retirementOutcomeTimeBound)
			}
			return ended(retirementOutcomeNoPath)
		case <-self.after(self.acknowledgeQuietInterval):
		}
		count := self.returnCount.Load()
		if count == lastCount && providerUp() {
			return returnAcknowledgment{
				outcome:         retirementOutcomeQuiet,
				lateReturnCount: count - startCount,
			}
		}
		lastCount = count
	}
}

// Holds closed tunnels while their derived clients keep acknowledging late
// provider returns (tunnelTeardown.retire), so that wait does not hold the
// caller's probe slot. One pass owns one, passes it in every tunnel's Config,
// and closes it after its last tunnel closed. Close joins every held tunnel,
// each bounded by its acknowledge timeout and its CloseTimeout, so nothing a
// pass held outlives the pass or its prober shard. Bounded by count: a tunnel
// that finds no room is retired in its own Close, which slows the pass
// instead of growing its memory. A held tunnel costs what an open one does.
//
// Safe for concurrent use.
type Retirements struct {
	slots chan struct{}

	stateLock sync.Mutex
	closed    bool
	held      sync.WaitGroup
}

// An owner that holds at most maxHeldCount tunnels at once. Zero or negative
// holds none.
func NewRetirements(maxHeldCount int) *Retirements {
	return &Retirements{slots: make(chan struct{}, max(0, maxHeldCount))}
}

// Runs retire on a goroutine of its own when there is room, and returns
// whether it did. A nil, closed or full owner runs nothing.
func (self *Retirements) hold(retire func()) bool {
	if self == nil {
		return false
	}
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		select {
		case self.slots <- struct{}{}:
			self.held.Add(1)
			return true
		default:
			return false
		}
	}()
	if !admitted {
		return false
	}
	tunnelRetirementHeldGauge.Inc()
	go func() {
		defer self.held.Done()
		defer func() {
			<-self.slots
			tunnelRetirementHeldGauge.Dec()
		}()
		retire()
	}()
	return true
}

// Holds retire as a closed tunnel's retirement would be held, for a test
// outside this package to see whether the owner still takes tunnels.
func (self *Retirements) Testing_Hold(retire func()) bool {
	return self.hold(retire)
}

// Holds no more tunnels and waits for every held one to retire. Safe to call
// more than once.
func (self *Retirements) Close() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
	}()
	self.held.Wait()
}
