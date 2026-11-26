package monitor

// Signal compute-policy implements SIGNALS.md §32.3 (CSRV-M12-06; DESIGN-COMPUTE §14.7, §5.4): the compute policy's
// and its freshness tickets' pages, each a minute from Mimir while monitor.yml's compute block enables the policy
// family (compute_signals.go):
//
//   - compute-policy-expiring: the policy within 14 days of its not_after;
//   - compute-ticket-horizon: the newest signed ticket within 7 days of its horizon.
//
// Its liveness is the policy's expiry gauge, which the broker publishes from the policy it holds.

import (
	"strconv"
	"time"
)

// SIGNALS.md §32.3's signal.
func NewComputePolicySignal() Signal {
	return newComputeFamilySignal(computePolicyFamily())
}

// A gauge of seconds left, firing under its line.
func computeSecondsLeftRule(class string, metric string, line func(settings *ComputeMonitorSettings) time.Duration, symptom string, action string) *computeRule {
	return &computeRule{
		class: class,
		expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
			return map[string]string{"left": computeGaugeMin(environment, settings, metric, "")}
		},
		judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
			left, ok := values["left"]
			return ok && left < line(settings).Seconds(), "seconds_left=" + strconv.FormatFloat(left, 'f', 0, 64), ok
		},
		symptom: symptom,
		action:  action,
	}
}

// The policy's family.
func computePolicyFamily() *computeFamily {
	return &computeFamily{
		family: computeFamilyPolicy, number: "32.3", key: "compute-policy", name: "Compute policy",
		probeId: "compute/policy", liveness: computeMetricPolicyExpiry, unobservableClass: "compute-policy-unobservable",
		rules: []*computeRule{
			computeSecondsLeftRule("compute-policy-expiring", computeMetricPolicyExpiry,
				func(settings *ComputeMonitorSettings) time.Duration { return settings.PolicyExpiryWithin },
				"the compute policy is within 14 days of its not_after",
				"call a sitting of the compute-policy key's holders",
			),
			computeSecondsLeftRule("compute-ticket-horizon", computeMetricTicketHorizon,
				func(settings *ComputeMonitorSettings) time.Duration { return settings.TicketHorizonWithin },
				"the newest signed ticket is within 7 days of its horizon",
				"call the tickets' sitting; past the horizon remote calls, enclave creates and searches stop",
			),
		},
	}
}
