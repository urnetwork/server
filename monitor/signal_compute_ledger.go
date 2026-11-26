package monitor

// Signal compute-ledger implements SIGNALS.md §32.1 (CSRV-M12-06; DESIGN-COMPUTE §14.7, §9.7, §9.13): the compute
// ledger's pages from compute_settle's, compute_op_recover's, compute_rollup_day's and compute_direct_settle's
// metrics, each a minute from Mimir while monitor.yml's compute block enables the ledger (compute_signals.go):
//
//   - compute-settlement-lag: the oldest settled operation row unbooked past 60 s;
//   - compute-op-stale: an operation row open past 24 hours, or settled and unbooked past 60 s;
//   - compute-rollup-mismatch: compute_rollup_day's alarm, a day's rollup differing from its charges or a network's
//     totals from its day rows and pruned sums;
//   - compute-direct-absorbed-high: absorbed direct-call charges above $50 in a day across networks.
//
// Its liveness is the settlement lag's gauge, which the broker publishes while it runs.

import (
	"strconv"
)

// SIGNALS.md §32.1's signal.
func NewComputeLedgerSignal() Signal {
	return newComputeFamilySignal(computeLedgerFamily())
}

// The ledger's family.
func computeLedgerFamily() *computeFamily {
	return &computeFamily{
		family: computeFamilyLedger, number: "32.1", key: "compute-ledger", name: "Compute ledger",
		probeId: "compute/ledger", liveness: computeMetricSettlementLag, unobservableClass: "compute-ledger-unobservable",
		rules: []*computeRule{
			{
				class: "compute-settlement-lag",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{"lag": computeGauge(environment, settings, computeMetricSettlementLag, "")}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					lag, ok := values["lag"]
					return ok && settings.SettlementLagMax.Seconds() < lag, "lag_seconds=" + strconv.FormatFloat(lag, 'f', 0, 64), ok
				},
				symptom: "settlement lags past 60 s",
				action:  "compute_settle on taskworker: its last run, Postgres's health, a network fence held; restart the job's chain, never edit a hold in Redis",
			},
			{
				class: "compute-op-stale",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"open":    computeGauge(environment, settings, computeMetricOpOldestSeconds, "", `state="open"`),
						"settled": computeGauge(environment, settings, computeMetricOpOldestSeconds, "", `state="settled"`),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					open, hasOpen := values["open"]
					settled, hasSettled := values["settled"]
					observed := "open_seconds=" + strconv.FormatFloat(open, 'f', 0, 64) + " settled_seconds=" + strconv.FormatFloat(settled, 'f', 0, 64)
					return settings.OpOpenMax.Seconds() < open || settings.OpSettledUnbookedMax.Seconds() < settled, observed, hasOpen && hasSettled
				},
				symptom: "an operation row is open past 24 hours, or settled and unbooked past 60 s",
				action:  "compute_op_recover's and compute_settle's last runs; a row open 24 hours is released at no charge, its cost URnetwork's: let the job do it",
			},
			computeAlarmRule(
				"compute-rollup-mismatch",
				[]string{"rollup_day", "rollup_total"},
				"a day's spend rollup differs from its charges, or a network's totals from its day rows and pruned sums",
				"compute_rollup_day's alarm names the day and network; take no partition by hand, and file the difference with the rows that show it",
			),
			{
				class: "compute-direct-absorbed-high",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"absorbed": computeIncrease(environment, computeMetricDirectAbsorbed, settings.DirectAbsorbedWindow, ""),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					// no series is nothing absorbed
					absorbed := values["absorbed"]
					return settings.DirectAbsorbedMaxNanoUsd < absorbed, "absorbed_nano_usd=" + strconv.FormatFloat(absorbed, 'f', 0, 64) + " over " + settings.DirectAbsorbedWindow.String(), true
				},
				symptom: "absorbed direct-call charges pass $50 in a day across networks",
				action:  "compute_direct_settle: a provider whose lookups fail keeps its holds pending and gets no new call; raise its usage lookup with it",
			},
		},
	}
}
