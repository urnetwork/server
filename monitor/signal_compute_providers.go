package monitor

// Signal compute-providers implements SIGNALS.md §32.2 (CSRV-M12-06; DESIGN-COMPUTE §14.7, §4, §5, §6.6, §6.7, §10):
// the providers' and the treasury's pages, each a minute from Mimir while monitor.yml's compute block enables the
// providers family (compute_signals.go), each provider its own target by its catalog id:
//
//   - compute-provider-failing: a provider failing every call for 5 minutes;
//   - compute-attestation-failures: attestation failures above 5% of a provider's attestations for 10 minutes;
//   - compute-provider-balance-low: a provider balance under a day of its forecast;
//   - compute-gamma-unknown-spend: compute_gamma_watch's alarm, a GammaSpent from our hotkeys no spend explains;
//   - compute-reconcile-gap: compute_reconcile's gap over 1%;
//   - compute-dispute-rate: disputes from at least 3 networks past 0.5% of a provider's requests in a day;
//   - compute-canary-gap: compute_canary's gap over 2%;
//   - compute-margin-low: a provider's realized margin under 5% for a week, business hours;
//   - compute-direct-worst-case-rise: a direct provider's costliest worst case above its value an hour before.
//
// Its liveness is the calls' counter, which the broker publishes while it runs.

import (
	"strconv"
)

// SIGNALS.md §32.2's signal.
func NewComputeProvidersSignal() Signal {
	return newComputeFamilySignal(computeProvidersFamily())
}

// A per-provider gauge's rule: fires where the gauge is past the line, below it when below is true.
func computeProviderGaugeRule(class string, metric string, line func(settings *ComputeMonitorSettings) float64, below bool, symptom string, action string) *computeRule {
	return &computeRule{
		class:   class,
		groupBy: "provider",
		expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
			if below {
				return map[string]string{"value": computeGaugeMin(environment, settings, metric, "provider")}
			}
			return map[string]string{"value": computeGauge(environment, settings, metric, "provider")}
		},
		judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
			value, ok := values["value"]
			observed := "value=" + strconv.FormatFloat(value, 'g', 6, 64) + " line=" + strconv.FormatFloat(line(settings), 'g', 6, 64)
			if below {
				return ok && value < line(settings), observed, ok
			}
			return ok && line(settings) < value, observed, ok
		},
		symptom: symptom,
		action:  action,
	}
}

// The providers' family.
func computeProvidersFamily() *computeFamily {
	return &computeFamily{
		family: computeFamilyProviders, number: "32.2", key: "compute-providers", name: "Compute providers",
		probeId: "compute/providers", liveness: computeMetricProviderCalls, unobservableClass: "compute-providers-unobservable",
		rules: []*computeRule{
			{
				class:   "compute-provider-failing",
				groupBy: "provider",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"calls": computeIncrease(environment, computeMetricProviderCalls, settings.ProviderFailingWindow, "provider"),
						"ok":    computeIncrease(environment, computeMetricProviderCalls, settings.ProviderFailingWindow, "provider", `outcome="ok"`),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					calls, ok := values["calls"]
					succeeded := values["ok"]
					return ok && 0 < calls && succeeded <= 0, computeShareText("ok", succeeded, calls), ok
				},
				symptom: "a provider is failing every call for 5 minutes",
				action:  "read the provider's status and compute_canary's last run; calls fail with a named error, never another model; a suspended account is the provider adapters' runbook",
			},
			{
				class:   "compute-attestation-failures",
				groupBy: "provider",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"all":    computeIncrease(environment, computeMetricAttestations, settings.AttestationFailureWindow, "provider"),
						"failed": computeIncrease(environment, computeMetricAttestations, settings.AttestationFailureWindow, "provider", `result="failed"`),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					all, ok := values["all"]
					failed := values["failed"]
					return ok && 0 < all && settings.AttestationFailureShare < failed/all, computeShareText("failed", failed, all), ok
				},
				symptom: "attestation failures pass 5% of a provider's attestations for 10 minutes",
				action:  "find the failing check in the metric's reason; a provider's new image not yet in the policy is a sitting the same day; never widen a profile to pass it",
			},
			computeProviderGaugeRule("compute-provider-balance-low", computeMetricProviderBalanceDays,
				func(settings *ComputeMonitorSettings) float64 { return settings.ProviderBalanceMinDays }, true,
				"a provider's balance is under a day of its forecast",
				"sidecarctl compute treasury status; compute_gamma_mint's and compute_gamma_spend's last runs; the week's transfer early if the float is short",
			),
			computeAlarmRule(
				"compute-gamma-unknown-spend",
				[]string{"gamma_unknown_spend"},
				"a GammaSpent from our hotkeys that no spend the job submitted explains",
				"compute_gamma_watch has set the deposit's cap to 0: pause the provider in the treasury, rotate K_P and reconcile the provider's VM list",
			),
			computeProviderGaugeRule("compute-reconcile-gap", computeMetricReconcileGap,
				func(settings *ComputeMonitorSettings) float64 { return settings.ReconcileGapShare }, false,
				"a reconciliation gap passes 1%",
				"compute_reconcile's report: the provider's bill against our charges and spends; a billing fault is raised with the provider before the next week's transfer",
			),
			{
				class:   "compute-dispute-rate",
				groupBy: "provider",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					return map[string]string{
						"calls":    computeIncrease(environment, computeMetricProviderCalls, settings.DisputeWindow, "provider"),
						"disputes": computeIncrease(environment, computeMetricDisputes, settings.DisputeWindow, "provider"),
						"networks": computeGauge(environment, settings, computeMetricDisputeNetworks, "provider"),
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					calls, ok := values["calls"]
					disputes := values["disputes"]
					networks := values["networks"]
					fires := ok && 0 < calls && settings.DisputeShare < disputes/calls && settings.DisputeMinNetworks <= networks
					return fires, computeShareText("disputes", disputes, calls) + " networks=" + strconv.FormatFloat(networks, 'f', 0, 64), ok
				},
				symptom: "disputes from at least 3 networks pass 0.5% of a provider's requests in a day",
				action:  "compare the provider's usage with the devices'; a provider over-reporting leaves the catalog by the policy's next line, its disclosure re-read",
			},
			computeProviderGaugeRule("compute-canary-gap", computeMetricCanaryGap,
				func(settings *ComputeMonitorSettings) float64 { return settings.CanaryGapShare }, false,
				"the canary's gap passes 2%",
				"compute_canary's run against the provider's usage and its bill; as the disputes page",
			),
			computeProviderGaugeRule("compute-margin-low", computeMetricMargin,
				func(settings *ComputeMonitorSettings) float64 { return settings.MarginMinShare }, true,
				"a provider's realized margin is under 5% for a week",
				"compute_reconcile's weekly margins; a price change is the owner's, through the catalog's next prices",
			),
			{
				class:   "compute-direct-worst-case-rise",
				groupBy: "provider",
				expressions: func(environment string, settings *ComputeMonitorSettings) map[string]string {
					selector := sidecarStorageSelector(environment, computeMetricDirectWorstCase)
					return map[string]string{
						"now":    computeGauge(environment, settings, computeMetricDirectWorstCase, "provider"),
						"before": "max by (provider) (" + selector + " offset " + strconv.FormatInt(int64(settings.DirectWorstCaseWindow.Seconds()), 10) + "s)",
					}
				},
				judge: func(settings *ComputeMonitorSettings, values map[string]float64) (bool, string, bool) {
					now, ok := values["now"]
					before, hasBefore := values["before"]
					// a provider with no value an hour ago has risen from none, which its first price read is not
					return ok && hasBefore && before < now, "now=" + strconv.FormatFloat(now, 'f', 0, 64) + " before=" + strconv.FormatFloat(before, 'f', 0, 64), ok
				},
				symptom: "a direct provider's costliest worst case is rising",
				action:  "compute_provider_prices' exposure rule: hold the provider out of direct at the next sitting if the rise outruns the holds",
			},
		},
	}
}
