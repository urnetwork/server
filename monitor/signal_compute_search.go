package monitor

// Signal compute-search implements SIGNALS.md §32.4 (CSRV-M12-06; DESIGN-COMPUTE §14.7, §21.5, §21.8): web search's
// pages, the broker's and searchsigner's, each a minute from Mimir while monitor.yml's compute block enables the search
// family (compute_signals.go), none naming a network (compute-10, compute-11):
//
//   - compute-search-lapses: lapsed search authorizations above 5% for 10 minutes;
//   - compute-search-float-low: the payer float under one day of searches;
//   - compute-search-failure-rate: Parallel's failure rate above 5% in an hour of at least 100 searches;
//   - compute-search-payer-transfer: compute_search_watch's alarm, a USDC transfer from the payer that settles no
//     authorization the signer issued;
//   - compute-search-nonce-unknown: the signer's alarm, a used nonce it did not issue;
//   - compute-search-sanctions-record: a payee the screen refused, or an authorization settled outside the
//     facilitator;
//   - compute-search-screen-stale: the screen's lists unfetched for 24 hours;
//   - compute-search-base-disagree: Base's two sources disagreeing;
//   - compute-search-row-retained: compute_search_purge's alarm, a web search row past 72 hours;
//   - compute-search-unattributed: a day's unattributed search value above $10 or 1% of its settled value, or a
//     search day that does not balance;
//   - compute-search-unsettled-high: the signer's value not yet final past 80% of max_unsettled.
//
// Its liveness is the authorizations' counter, which the broker publishes while it runs.

import (
	"strconv"
)

// SIGNALS.md §32.4's signal.
func NewComputeSearchSignal() Signal {
	return newComputeFamilySignal(computeSearchFamily())
}

// The search family.
func computeSearchFamily() *computeFamily {
	return &computeFamily{
		family: computeFamilySearch, number: "32.4", key: "compute-search", name: "Compute web search",
		probeId: "compute/search", liveness: computeMetricSearchAuthorizations, unobservableClass: "compute-search-unobservable",
		rules: []*computeRule{
			{
				class: "compute-search-lapses",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"authorized": computeIncrease(environment, computeMetricSearchAuthorizations, settings.SearchLapseWindow, "", `outcome="authorized"`),
						"lapsed":     computeIncrease(environment, computeMetricSearchAuthorizations, settings.SearchLapseWindow, "", `outcome="lapsed"`),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					authorized := values["authorized"]
					lapsed := values["lapsed"]
					return 0 < authorized && settings.SearchLapseShare < lapsed/authorized, computeShareText("lapsed", lapsed, authorized), true
				},
				symptom: "lapsed search authorizations pass 5% for 10 minutes",
				action:  "the search signer's Base sources and the facilitator's settlements; holds lapse on their own through compute_search_tickets",
			},
			{
				class: "compute-search-float-low",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{"days": computeGaugeMin(environment, settings, computeMetricSearchFloatDays, "")}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					days, ok := values["days"]
					return ok && days < settings.SearchFloatMinDays, "float_days=" + strconv.FormatFloat(days, 'f', 2, 64), ok
				},
				symptom: "the payer float is under one day of searches",
				action:  "compute_search_float's last refill and Coinbase's withdrawal to the payer address",
			},
			{
				class: "compute-search-failure-rate",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"all": computeIncrease(environment, computeMetricSearchOutcomes, settings.SearchFailureWindow, ""),
						"ok":  computeIncrease(environment, computeMetricSearchOutcomes, settings.SearchFailureWindow, "", `class="ok"`),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					all := values["all"]
					failed := all - values["ok"]
					fires := settings.SearchFailureMinCount <= all && settings.SearchFailureShare < failed/all
					return fires, computeShareText("failed", failed, all), true
				},
				symptom: "Parallel's failure rate passes 5% in an hour of at least 100 searches",
				action:  "Parallel's status; nothing is retried elsewhere; the month's rates go to the owner",
			},
			computeAlarmRule("compute-search-payer-transfer", []string{"search_payer_transfer"},
				"a USDC transfer from the payer settles no authorization the signer issued",
				"compute_search_watch has stopped signing and refills: treat the payer key as leaked, the search payer's runbook",
			),
			computeAlarmRule("compute-search-nonce-unknown", []string{"search_nonce_unknown"},
				"a used nonce the signer did not issue",
				"compute_search_watch has stopped signing and refills: treat the payer key as leaked, the search payer's runbook",
			),
			computeAlarmRule("compute-search-sanctions-record", []string{"search_sanctions_record"},
				"a payee the screen refused, or an authorization settled outside the facilitator",
				"keep the sanctions record, tell counsel, and suspend the network's searches if its sidecar sent it",
			),
			{
				class: "compute-search-screen-stale",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{"age": computeGauge(environment, settings, computeMetricSearchScreenAge, "")}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					age, ok := values["age"]
					return ok && settings.SearchScreenStaleMax.Seconds() < age, "age_seconds=" + strconv.FormatFloat(age, 'f', 0, 64), ok
				},
				symptom: "the screen's lists are unfetched for 24 hours",
				action:  "compute_sanctions_list's fetch, each list's source; at 72 hours signing stops on its own",
			},
			computeAlarmRule("compute-search-base-disagree", []string{"search_base_disagree"},
				"Base's two sources disagree",
				"the signer holds what it reads twice; find the source that is behind or forked and let finality settle it",
			),
			computeAlarmRule("compute-search-row-retained", []string{"search_row_retained"},
				"a web search row is past 72 hours",
				"compute_search_purge's last run: no per-search row may outlive 72 hours",
			),
			{
				class: "compute-search-unattributed",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"unattributed": computeGauge(environment, settings, computeMetricSearchUnattributed, ""),
						"settled":      computeGauge(environment, settings, computeMetricSearchSettled, ""),
						"unbalanced":   computeGauge(environment, settings, computeMetricSearchDayUnbalanced, ""),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					unattributed, ok := values["unattributed"]
					settled := values["settled"]
					unbalanced := values["unbalanced"]
					fires := ok && (settings.SearchUnattributedMaxNanoUsd < unattributed ||
						(0 < settled && settings.SearchUnattributedShare < unattributed/settled)) || 0 < unbalanced
					return fires, computeShareText("unattributed_nano_usd", unattributed, settled) + " unbalanced=" + strconv.FormatFloat(unbalanced, 'f', 0, 64), ok
				},
				symptom: "a day's unattributed search value passes $10 or 1% of its settled value, or a search day does not balance",
				action:  "the signer's day totals against the broker's charges for the day; unattributed settlements are URnetwork's cost",
			},
			{
				class: "compute-search-unsettled-high",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"unsettled": computeGauge(environment, settings, computeMetricSignerUnsettled, ""),
						"max":       computeGauge(environment, settings, computeMetricSignerMaxUnsettled, ""),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					unsettled, hasUnsettled := values["unsettled"]
					maxUnsettled, hasMax := values["max"]
					ok := hasUnsettled && hasMax && 0 < maxUnsettled
					return ok && settings.SignerUnsettledShare*maxUnsettled < unsettled, computeShareText("unsettled_nano_usd", unsettled, maxUnsettled), ok
				},
				symptom: "the signer's value not yet final passes 80% of max_unsettled",
				action:  "the signer stops at max_unsettled on its own; Base's finalized head, the facilitator's backlog",
			},
		},
	}
}
