package controller

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// The drift test's counters (lag.dev/DRIFTTEST.md P7, W8), beside lag.dev's
// (lag_metrics.go). During market discovery nothing is billed or allowanced
// (Q9), so usage is visible here: runs and verdicts per network per day, as
// increase over a day. The network label names an Embed customer's network,
// of which there are few, and never a visitor, an address or an extender
// (amendment 9). Demo starts are counted by outcome, the only record of them
// (W8).

var lagDriftAllocationsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_allocations_total",
		Help:      "Drift tests asked for (P2), by outcome: created, refused_<code> (embed, disabled, client_token, limit, unavailable), rejected or error",
	},
	[]string{"outcome"},
)

var lagDriftStartsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_starts_total",
		Help:      "Drift test starts (P5, W8), by outcome: started, demo, refused_<code>, rejected or error",
	},
	[]string{"outcome"},
)

var lagDriftRunsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_runs_total",
		Help:      "Drift test runs started (P7), by network",
	},
	[]string{"network_id"},
)

var lagDriftRunOutcomesCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_run_outcomes_total",
		Help:      "Drift test runs completed (E5), by outcome: counted, or why the run stays out of the window; and by the browser's stop (V10), or none without a finish",
	},
	[]string{"outcome", "stop"},
)

var lagDriftVerdictReasonsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_verdict_reasons_total",
		Help:      "Drift test verdicts computed (P4, V6), by verdict and reason, without the network",
	},
	[]string{"verdict", "reason"},
)

// How long the visitor waits for a verdict once the frame has finished its
// run: its reports, at once from a current extender (E4), and the processing.
var lagDriftProcessingHistogram = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_processing_seconds",
		Help:      "From a drift run's finish to its completion (E5), for runs the frame finished",
		Buckets:   []float64{0.5, 1, 2, 5, 10, 15, 20, 25, 35, 60, 120},
	},
)

// Set by the drift sweep at the end of each pass, so an alert can read only
// the newest pass and see a sweep that stopped.
var lagDriftSweepTimestampGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_sweep_timestamp_seconds",
		Help:      "When the drift sweep last finished a pass, unix seconds (E5)",
	},
)

var lagDriftVerdictsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_verdicts_total",
		Help:      "Drift test verdicts computed (P7, V6), by network and verdict: agree, disagree or inconclusive",
	},
	[]string{"network_id", "verdict"},
)

var lagDriftDemoReportsCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_demo_reports_total",
		Help:      "Reports of demo sessions dropped unstored (W8, E4): sent by extenders that predate NoReport",
	},
)

// Set by the drift sweep: runs open past every deadline, which only a sweep
// that can't complete them leaves, such as a taskworker without the borders.
var lagDriftStuckRunsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_stuck_runs",
		Help:      "Drift test runs still started or processing a minute past their report wait (E5)",
	},
)

var lagDriftFailuresCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_failures_total",
		Help:      "Drift tests failed by an internal error (P4)",
	},
)

func init() {
	prometheus.MustRegister(
		lagDriftAllocationsCounter,
		lagDriftStartsCounter,
		lagDriftRunsCounter,
		lagDriftRunOutcomesCounter,
		lagDriftVerdictReasonsCounter,
		lagDriftProcessingHistogram,
		lagDriftVerdictsCounter,
		lagDriftDemoReportsCounter,
		lagDriftStuckRunsGauge,
		lagDriftSweepTimestampGauge,
		lagDriftFailuresCounter,
	)
}

// Counts one answer to `POST /lag/drift-test`.
func countLagDriftAllocate(result *LagDriftTestResult, err error) {
	lagDriftAllocationsCounter.WithLabelValues(lagDriftAnswerOutcome(result != nil, nil, err, "created")).Inc()
}

// Counts one answer to `POST /lag/drift-test/start`.
func countLagDriftStart(result *LagDriftStartResult, err error) {
	var lagError *LagError
	success := "started"
	if result != nil {
		lagError = result.Error
		if result.Demo {
			success = "demo"
		}
	}
	lagDriftStartsCounter.WithLabelValues(lagDriftAnswerOutcome(result != nil, lagError, err, success)).Inc()
}

// The outcome of an answer: an error is rejected or error, as for the lag
// routes (lagErrorOutcome); a refusal is refused_<code>, its code one of the
// drift routes' few.
func lagDriftAnswerOutcome(answered bool, lagError *LagError, err error, success string) string {
	var statusError *lagDriftStatusError
	if errors.As(err, &statusError) {
		// a refusal answered with its status (P5): an invalid request is
		// rejected, as for the lag routes, and the rest are refusals
		if statusError.lagError.Code == LagDriftErrorCodeInvalid {
			return lagOutcomeRejected
		}
		lagError = statusError.lagError
		err = nil
	}
	switch {
	case err != nil:
		return lagErrorOutcome(err)
	case !answered && lagError == nil:
		return lagOutcomeError
	case lagError != nil:
		switch lagError.Code {
		case LagErrorCodeLimit, LagErrorCodeUnavailable, LagDriftErrorCodeEmbed, LagDriftErrorCodeExpired,
			LagDriftErrorCodeClosed, LagDriftErrorCodeBusy, LagDriftErrorCodeOrigin, LagDriftErrorCodeDisabled,
			LagDriftErrorCodeClientToken:
			return "refused_" + lagError.Code
		}
		return lagOutcomeError
	}
	return success
}

// Counts a run started for a network's drift test.
func countLagDriftRun(networkId server.Id) {
	lagDriftRunsCounter.WithLabelValues(networkId.String()).Inc()
}

// Counts a completed run by its outcome and the browser's stop, both bounded
// sets, and for a run the frame finished, how long it took from its finish
// to complete.
func countLagDriftRunCompleted(outcome string, stop string, finishToComplete *time.Duration) {
	if stop == "" {
		stop = "none"
	}
	lagDriftRunOutcomesCounter.WithLabelValues(outcome, stop).Inc()
	if finishToComplete != nil {
		lagDriftProcessingHistogram.Observe(max(finishToComplete.Seconds(), 0))
	}
}

// Counts a verdict computed for a network's drift test, by the network and
// verdict, and by the verdict and reason without the network.
func countLagDriftVerdict(networkId server.Id, verdict string, reason string) {
	lagDriftVerdictsCounter.WithLabelValues(networkId.String(), verdict).Inc()
	lagDriftVerdictReasonsCounter.WithLabelValues(verdict, reason).Inc()
}

func countLagDriftFailure() {
	lagDriftFailuresCounter.Inc()
}

func countLagDriftDemoReport() {
	lagDriftDemoReportsCounter.Inc()
}

// SetLagDriftStuckRuns sets the gauge of runs open past every deadline.
func SetLagDriftStuckRuns(count int) {
	lagDriftStuckRunsGauge.Set(float64(count))
}

// SetLagDriftSweepTimestamp records that a drift sweep pass finished at `at`.
func SetLagDriftSweepTimestamp(at time.Time) {
	lagDriftSweepTimestampGauge.Set(float64(at.Unix()))
}
