package controller

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The ping report's refusals (connect/GEOMAP.md §2.5): the claims
// `POST /network/ping-report` refused before storing, by why. Before this
// counter a refused claim was counted only in the `rejected` of the reporter's
// own result, which connect's reporter does not log (it logs a refused report,
// on the pinger's host): it was not stored, logged or metered, so how many
// claims the operator itself turned away, and why, could not be told from the
// operator's side -- for example the reports of an extender slot that shares
// its identity key with the slot that last activated it, which are refused as
// another client's.
//
// The counter is fixed cardinality: every label value is one of the constants
// here, never something a reporter sent, and every series is created at zero,
// so a rate is defined before the first refusal. A claim refused and later
// posted again is counted at each refusal.

// Why a claim was refused (urnetwork_extender_ping_report_refused_total).
const (
	// The report carried more claims than one report holds, and was refused
	// whole. The reporter logs a refusal of its report and does not post the
	// claims again.
	pingReportRefusalOversized = "oversized"
	// The reporting client was past its report budget, and the report was
	// refused whole.
	pingReportRefusalRateLimited = "rate_limited"
	// The claim does not decode: a field that does not decode or is the wrong
	// size, a pinger kind its identity contradicts or no known kind, an outcome
	// that is not one of the three, an unreadable co-signature, or a reason,
	// round trip or relay depth no column can hold.
	pingReportRefusalMalformed = "malformed"
	// The claim's own timestamp is missing or outside the window the report
	// accepts around the operator's clock.
	pingReportRefusalTimeWindow = "time_window"
	// The claim's pinger is not the reporting client: a provider's claim naming
	// another client id, or an extender's under a key another client activated
	// last.
	pingReportRefusalPingerNotReporter = "pinger_not_reporter"
	// The operator holds no key for the pinger: a provider with no registered
	// client key, or an extender key no extender activated under.
	pingReportRefusalPingerUnknown = "pinger_unknown"
	// No extender activated under the target's key.
	pingReportRefusalTargetUnknown = "target_unknown"
	// The target is the pinger itself, or another extender the reporting
	// client activated.
	pingReportRefusalTargetOwn = "target_own"
	// The pinger's signature does not verify under the key the operator holds
	// for it.
	pingReportRefusalBadSignature = "bad_signature"
	// A claim without a co-signature that verifies, past the report's cap on
	// them (ExtenderPingReportSettings.MaxUncosignedPerPost).
	pingReportRefusalUncosignedCap = "uncosigned_cap"
	// The claim is already stored, or repeats one earlier in the report.
	pingReportRefusalReplay = "replay"
)

// Every reason a claim can be refused for, in publication order.
var pingReportRefusalReasons = []string{
	pingReportRefusalOversized,
	pingReportRefusalRateLimited,
	pingReportRefusalMalformed,
	pingReportRefusalTimeWindow,
	pingReportRefusalPingerNotReporter,
	pingReportRefusalPingerUnknown,
	pingReportRefusalTargetUnknown,
	pingReportRefusalTargetOwn,
	pingReportRefusalBadSignature,
	pingReportRefusalUncosignedCap,
	pingReportRefusalReplay,
}

// The claims of one report refused before storing, by reason.
type pingReportRefusalCounts map[string]int

// Counts one refused claim.
func (self pingReportRefusalCounts) add(reason string) {
	self[reason] += 1
}

// The claims refused, every reason together: the report's `rejected`.
func (self pingReportRefusalCounts) total() int {
	total := 0
	for _, count := range self {
		total += count
	}
	return total
}

type extenderPingReportMetrics struct {
	refused *prometheus.CounterVec
}

func newExtenderPingReportMetrics(registerer prometheus.Registerer) *extenderPingReportMetrics {
	metrics := &extenderPingReportMetrics{
		refused: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_ping_report_refused_total",
			Help: "Ping claims POST /network/ping-report refused before storing them, by reason. " +
				"A whole report: oversized, more claims than one report holds; rate_limited, the reporting client was past its report budget. " +
				"A claim: malformed, it does not decode; time_window, its timestamp is outside the window the report accepts; " +
				"pinger_not_reporter, its pinger is not the reporting client (another client id, or an extender key another client activated last); " +
				"pinger_unknown, no key is held for its pinger; target_unknown, no extender activated under the target key; " +
				"target_own, the target is the pinger or another extender of the reporting client; bad_signature, the pinger's signature does not verify; " +
				"uncosigned_cap, a claim without a verifying co-signature past the report's cap; replay, already stored or repeated in the report.",
		}, []string{"reason"}),
	}
	for _, reason := range pingReportRefusalReasons {
		metrics.refused.WithLabelValues(reason)
	}
	registerer.MustRegister(metrics.refused)
	return metrics
}

var defaultExtenderPingReportMetrics = newExtenderPingReportMetrics(prometheus.DefaultRegisterer)

// Counts a report's refused claims. A reason that is not one of the constants
// above counts nothing, so the label cannot grow; nil counts nothing.
func (self *extenderPingReportMetrics) observe(refusals pingReportRefusalCounts) {
	if self == nil {
		return
	}
	for _, reason := range pingReportRefusalReasons {
		if count := refusals[reason]; 0 < count {
			self.refused.WithLabelValues(reason).Add(float64(count))
		}
	}
}
