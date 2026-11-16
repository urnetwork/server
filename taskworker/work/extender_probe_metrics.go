package work

// What the extender uptime probe decided, so the false-revocation rate is
// visible (connect/EXTENDER.md C3; factury research extender-health-2026-11-11
// §4 item 5).
//
// The revocations and the restores are counted where they happen, so
//
//	sum(increase(urnetwork_taskworker_extender_probe_restores_total{kind="extender"}[24h]))
//	  / sum(increase(urnetwork_taskworker_extender_probe_revocations_total[24h]))
//
// is the share of a day's revocations whose extender answered a probe again
// within the recovery window: revocations that were false, or an outage the
// extender came back from. The held results and the held passes are the
// failures the common-mode guard and the control target kept from counting
// (extender_probe_common_mode.go, extender_probe_control.go), and the time a
// restored extender spent revoked is how long a false revocation kept it out of
// the directories.
//
// Every label is a bounded enum, so the series count is fixed whatever the
// directory's size; no series names an extender. The counters are per process,
// and the probe chain moves between taskworkers, so a dashboard sums them.

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// The sets of addresses a pass probes.
const (
	// the active addresses, whose failures count toward deactivation
	extenderProbeSetActive = "active"
	// the addresses the probes deactivated, probed for their return
	extenderProbeSetRecovery = "recovery"
)

// What a probe of one address came to.
const (
	extenderProbeResultOk     = "ok"
	extenderProbeResultFailed = "failed"
	// a failure the pass's verdict kept from counting
	extenderProbeResultHeld = "held"
)

// What a restore brought back.
const (
	// an address of an extender that another address kept active
	extenderProbeRestoreAddress = "address"
	// a revoked extender
	extenderProbeRestoreExtender = "extender"
)

var extenderProbeResultsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_results_total",
	Help:      "Extender uptime probes by family, by set (active addresses, or deactivated ones probed for their return) and by result (ok, failed, or held: a failure the pass's verdict kept from counting)",
}, []string{"ip_version", "set", "result"})

var extenderProbePassesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_passes_total",
	Help:      "Extender uptime probe passes by family and verdict: counted, common_mode (the failures spanned the family's networks), or control_failed (the prober's own control target failed)",
}, []string{"ip_version", "verdict"})

var extenderProbeControlCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_control_total",
	Help:      "Checks of the extender uptime prober's own control target, the api host, by family and result (ok, failed, absent, unknown)",
}, []string{"ip_version", "result"})

var extenderProbeDeactivationsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_deactivations_total",
	Help:      "Extender addresses the uptime probe deactivated after consecutive failures, by family",
}, []string{"ip_version"})

var extenderProbeRevocationsCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_revocations_total",
	Help:      "Extenders the uptime probe revoked when their last active address was deactivated",
})

var extenderProbeRestoresCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_restores_total",
	Help:      "Deactivated extender addresses the uptime probe restored when they answered again, by family and by what came back: an address of an extender still active, or a revoked extender",
}, []string{"ip_version", "kind"})

var extenderProbeRestoreRevokedSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "extender_probe_restore_revoked_seconds",
	Help:      "How long an extender the uptime probe restored had been revoked",
	Buckets: []float64{
		5 * 60,
		15 * 60,
		30 * 60,
		60 * 60,
		2 * 60 * 60,
		4 * 60 * 60,
		8 * 60 * 60,
		12 * 60 * 60,
		24 * 60 * 60,
		48 * 60 * 60,
		72 * 60 * 60,
	},
})

func init() {
	prometheus.MustRegister(
		extenderProbeResultsCounter,
		extenderProbePassesCounter,
		extenderProbeControlCounter,
		extenderProbeDeactivationsCounter,
		extenderProbeRevocationsCounter,
		extenderProbeRestoresCounter,
		extenderProbeRestoreRevokedSeconds,
	)
}

// The ip_version label of a family.
func extenderProbeIpVersionLabel(ipVersion int) string {
	return strconv.Itoa(ipVersion)
}
