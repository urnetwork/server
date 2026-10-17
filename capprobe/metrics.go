// The probe's metrics, per taskworker process. Every bounded label set is
// published from the start, zero included, so a probe that stops reads as
// counters that stop rising rather than as series that vanish. SIGNALS.md
// §2.19g says what each means and when it is a finding; the egress probes
// dashboard charts every one.
package capprobe

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Pass results, the labels of urnetwork_capability_probe_passes_total.
const (
	// the pass read its pages and measured what was due
	PassResultMeasured = "measured"
	// the sweep's next start had not come: nothing was read
	PassResultWaiting = "waiting"
	// the settings switch the probe off: the pass runs nothing and ends its
	// chain
	PassResultDisabled = "disabled"
	// the pass failed: its settings, prober shard, selection or a write
	PassResultFailed = "failed"
)

// Every pass result.
var passResults = []string{
	PassResultMeasured,
	PassResultWaiting,
	PassResultDisabled,
	PassResultFailed,
}

// Selection stages, the labels of urnetwork_capability_probe_selection_total.
const (
	selectionScanned  = "scanned"
	selectionDue      = "due"
	selectionEligible = "eligible"
	selectionStarted  = "started"
)

var selectionStages = []string{
	selectionScanned,
	selectionDue,
	selectionEligible,
	selectionStarted,
}

// The two capabilities of urnetwork_capability_probe_readings_total.
const (
	capabilityE2e    = "e2e"
	capabilityWindow = "window"
)

// What became of a downgrade of fresh modern evidence, the labels of
// urnetwork_capability_probe_downgrades_total.
const (
	// a negative reading was held back
	downgradeHeld = "held"
	// a later sweep's negative reading confirmed a held one and was recorded
	downgradeConfirmed = "confirmed"
)

var downgradeResults = []string{
	downgradeHeld,
	downgradeConfirmed,
}

var passesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "passes_total",
	Help:      "Capability probe passes by result: measured, waiting (every ten minutes between sweeps), disabled (the last of a chain once switched off), failed",
}, []string{"result"})

var selectionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "selection_total",
	Help:      "Providers the capability probe's sweep scanned, found due for evidence, found eligible for quality or speed, and started measuring",
}, []string{"stage"})

var providersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "providers_total",
	Help:      "Finished capability measurements by outcome: recorded with the gate's outcome, or why nothing was recorded",
}, []string{"outcome"})

var readingsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "readings_total",
	Help:      "Final readings of opened capability measurements, per capability (e2e, window) and reading; not_tested and pending are not evidence",
}, []string{"capability", "reading"})

var measureSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "measure_seconds",
	Help:      "One capability measurement, from opening its tunnel to reading its evidence, teardown excluded",
	Buckets:   []float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 75, 90, 120, 180, 300},
})

var inFlight = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "in_flight",
	Help:      "Capability measurements this taskworker has running, teardown included",
})

var closeErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "close_errors_total",
	Help:      "Capability tunnel teardowns that returned an error; the derived client's retirement is then for the prober reaper",
})

var downgradesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "downgrades_total",
	Help:      "Negative readings of providers with fresh modern evidence: held back for a later sweep to confirm, or confirmed by one and recorded",
}, []string{"result"})

var breakerTripsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "breaker_trips_total",
	Help:      "Capability probe passes that negotiated no end-to-end session with any provider and so recorded none of their negative readings",
})

// The funding readiness steps a pass's check fails at, the labels of
// urnetwork_capability_probe_readiness_failures_total.
var readinessSteps = []string{
	"funding_unknown",
	"funding_unavailable",
}

var readinessFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "readiness_failures_total",
	Help:      "Capability probe passes whose prober shard could not show it was funded, by step: funding_unknown (the balance read failed), funding_unavailable (no active balance); the pass fails",
}, []string{"step"})

var shardCleanupErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "shard_cleanup_errors_total",
	Help:      "Capability probe passes whose prober shard drain or reap failed afterwards; the prober reaper retries it, and a finished pass still stands",
})

var sweepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "sweeps_total",
	Help:      "Capability probe sweeps this taskworker completed by reaching the end of the connected providers",
})

var lastSweepSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "last_sweep_seconds",
	Help:      "How long the last sweep this taskworker completed took, from its first pass's start",
})

var chainRestartsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "chain_restarts_total",
	Help:      "Capability probe chains this taskworker, with the probe switched on, found without their task row and scheduled again: a disabled pass on a taskworker with an older config, or maintenance, had ended them",
})

var lastPassTimestampSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "capability_probe",
	Name:      "last_pass_timestamp_seconds",
	Help:      "Unix time this taskworker last finished a capability probe pass that measured or waited",
})

// Registers the metrics with every bounded label set seeded.
func init() {
	for _, result := range passResults {
		passesTotal.WithLabelValues(result)
	}
	for _, stage := range selectionStages {
		selectionTotal.WithLabelValues(stage)
	}
	for _, outcome := range outcomes {
		providersTotal.WithLabelValues(outcome)
	}
	for _, reading := range e2eReadings {
		readingsTotal.WithLabelValues(capabilityE2e, string(reading))
	}
	for _, reading := range windowReadings {
		readingsTotal.WithLabelValues(capabilityWindow, string(reading))
	}
	for _, step := range readinessSteps {
		readinessFailuresTotal.WithLabelValues(step)
	}
	for _, result := range downgradeResults {
		downgradesTotal.WithLabelValues(result)
	}
	prometheus.MustRegister(
		passesTotal,
		selectionTotal,
		providersTotal,
		readingsTotal,
		downgradesTotal,
		breakerTripsTotal,
		measureSeconds,
		inFlight,
		closeErrorsTotal,
		readinessFailuresTotal,
		shardCleanupErrorsTotal,
		sweepsTotal,
		lastSweepSeconds,
		chainRestartsTotal,
		lastPassTimestampSeconds,
	)
}

// Counts one pass the task finished without running it, or failed before
// it ran: PassResultDisabled or PassResultFailed.
func ObservePass(result string) {
	passesTotal.WithLabelValues(result).Inc()
}

// Counts one pass whose prober shard's drain or reap failed after it.
func ObserveShardCleanupFailure() {
	shardCleanupErrorsTotal.Inc()
}

// Counts one chain found without its task row and scheduled again.
func ObserveChainRestart() {
	chainRestartsTotal.Inc()
}

// Counts one pass whose funding readiness check failed at step.
func ObserveReadinessFailure(step string) {
	readinessFailuresTotal.WithLabelValues(step).Inc()
}

// Counts a selection's stages.
func observeSelection(selected selection, startedCount int) {
	selectionTotal.WithLabelValues(selectionScanned).Add(float64(selected.scanned))
	selectionTotal.WithLabelValues(selectionDue).Add(float64(selected.due))
	selectionTotal.WithLabelValues(selectionEligible).Add(float64(selected.eligible))
	selectionTotal.WithLabelValues(selectionStarted).Add(float64(startedCount))
}

// Counts one finished measurement under outcome.
func observeMeasurement(result measurement, outcome string) {
	providersTotal.WithLabelValues(outcome).Inc()
	measureSeconds.Observe(result.elapsed.Seconds())
	if result.opened {
		readingsTotal.WithLabelValues(capabilityE2e, string(result.reading.e2e)).Inc()
		readingsTotal.WithLabelValues(capabilityWindow, string(result.reading.window)).Inc()
	}
	if result.closeErr != nil {
		closeErrorsTotal.Inc()
	}
}
