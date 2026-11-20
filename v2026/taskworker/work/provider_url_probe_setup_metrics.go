package work

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

var urlProbeSetupCallNames = [2]string{"credential_acquire", "registered_constructor"}
var urlProbeSetupOutcomeNames = [3]string{"ok", "canceled", "error"}
var urlProbeSetupCoverageNames = [providertunnel.SetupAdmissionCoverageCount]string{
	"matched", "missing", "ambiguous", "overflow", "invalid", "incomplete", "no_constructor",
}
var urlProbeSetupRouteNames = [4]string{"credential_acquire", "credential_to_constructor", "registered_constructor", "registration_to_admission"}

type providerUrlProbeSetupDescriptors struct {
	calls, seconds, pending, tunnels, route, evaluation, evaluationCount, turns, closeErrors, enabled *prometheus.Desc
}

func newProviderUrlProbeSetupDescriptors() *providerUrlProbeSetupDescriptors {
	return &providerUrlProbeSetupDescriptors{
		calls:           prometheus.NewDesc("urnetwork_url_probe_completed_setup_calls_total", "Returned setup calls in the same completed URL turn cohort; calls can overlap and include cleanup", []string{"stage", "outcome"}, nil),
		seconds:         prometheus.NewDesc("urnetwork_url_probe_completed_setup_call_seconds_total", "Sum of returned credential acquisition or processed-registration constructor residence; not a wall-time partition", []string{"stage", "outcome"}, nil),
		pending:         prometheus.NewDesc("urnetwork_url_probe_completed_setup_pending_calls_total", "Setup wrapper calls still pending when an owned tunnel froze after terminal cleanup", []string{"stage"}, nil),
		tunnels:         prometheus.NewDesc("urnetwork_url_probe_completed_setup_tunnels_total", "Closed owned tunnels by strict single-credential single-constructor admission timing coverage", []string{"coverage"}, nil),
		route:           prometheus.NewDesc("urnetwork_url_probe_completed_setup_route_stage_seconds_total", "Source-time stages from credential entry to first observed Added for matched unique constructors; all four stages share matched tunnel denominator", []string{"stage"}, nil),
		evaluation:      prometheus.NewDesc("urnetwork_url_probe_completed_setup_evaluation_seconds_total", "Source InEvaluation to Added for matched tunnels with both events; includes contract transport and send admission, not ping RTT", nil, nil),
		evaluationCount: prometheus.NewDesc("urnetwork_url_probe_completed_setup_evaluation_tunnels_total", "Matched admission tunnels with an observed or missing source InEvaluation event", []string{"coverage"}, nil),
		turns:           prometheus.NewDesc("urnetwork_url_probe_completed_setup_turns_total", "Returned URL batches with complete ProbeOne timing and at least one closed setup observation, or unobserved", []string{"coverage"}, nil),
		closeErrors:     prometheus.NewDesc("urnetwork_url_probe_completed_setup_close_errors_total", "Observed tunnels whose unchanged terminal Close returned an error", nil, nil),
		enabled:         prometheus.NewDesc("urnetwork_url_probe_completed_setup_timing_enabled", "Executable capability for bounded owned-tunnel setup timing in the completed URL turn cohort", nil, nil),
	}
}

func (self *providerUrlProbeSetupDescriptors) describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{self.calls, self.seconds, self.pending, self.tunnels, self.route, self.evaluation, self.evaluationCount, self.turns, self.closeErrors, self.enabled} {
		ch <- desc
	}
}

// The parent collector passes its already-copied snapshot. Never take a second
// generation while emitting stage sums, setup sums, and their denominators.
func (self *providerUrlProbeSetupDescriptors) collect(ch chan<- prometheus.Metric, values providerUrlProbeStageSnapshot) {
	setup := values.setup
	for phase, stage := range urlProbeSetupCallNames {
		for result, outcome := range urlProbeSetupOutcomeNames {
			call := setup.Calls[phase][result]
			ch <- prometheus.MustNewConstMetric(self.calls, prometheus.CounterValue, float64(call.Count), stage, outcome)
			ch <- prometheus.MustNewConstMetric(self.seconds, prometheus.CounterValue, call.Duration.Seconds(), stage, outcome)
		}
		ch <- prometheus.MustNewConstMetric(self.pending, prometheus.CounterValue, float64(setup.Pending[phase]), stage)
	}
	for i, coverage := range urlProbeSetupCoverageNames {
		ch <- prometheus.MustNewConstMetric(self.tunnels, prometheus.CounterValue, float64(setup.Tunnels[i]), coverage)
	}
	for i, stage := range urlProbeSetupRouteNames {
		ch <- prometheus.MustNewConstMetric(self.route, prometheus.CounterValue, setup.RouteStages[i].Seconds(), stage)
	}
	ch <- prometheus.MustNewConstMetric(self.evaluation, prometheus.CounterValue, setup.EvaluationToAdmission.Seconds())
	ch <- prometheus.MustNewConstMetric(self.evaluationCount, prometheus.CounterValue, float64(setup.EvaluationCount), "observed")
	ch <- prometheus.MustNewConstMetric(self.evaluationCount, prometheus.CounterValue, float64(setup.Tunnels[providertunnel.SetupAdmissionMatched]-setup.EvaluationCount), "missing")
	ch <- prometheus.MustNewConstMetric(self.turns, prometheus.CounterValue, float64(values.setupObserved), "observed")
	ch <- prometheus.MustNewConstMetric(self.turns, prometheus.CounterValue, float64(values.setupUnobserved), "unobserved")
	ch <- prometheus.MustNewConstMetric(self.closeErrors, prometheus.CounterValue, float64(setup.CloseErrors))
	ch <- prometheus.MustNewConstMetric(self.enabled, prometheus.GaugeValue, 1)
}

func (self *providerUrlProbeStageSnapshot) addSetup(setup providertunnel.SetupTiming, valid bool) {
	var tunnels uint64
	for _, count := range setup.Tunnels {
		tunnels += count
	}
	if !valid || tunnels == 0 {
		self.setupUnobserved++
		return
	}
	self.setupObserved++
	for phase := range setup.Calls {
		for result, call := range setup.Calls[phase] {
			self.setup.Calls[phase][result].Count += call.Count
			self.setup.Calls[phase][result].Duration += call.Duration
		}
		self.setup.Pending[phase] += setup.Pending[phase]
	}
	for i, count := range setup.Tunnels {
		self.setup.Tunnels[i] += count
	}
	for i, duration := range setup.RouteStages {
		self.setup.RouteStages[i] += duration
	}
	self.setup.CloseErrors += setup.CloseErrors
	self.setup.EvaluationCount += setup.EvaluationCount
	self.setup.EvaluationToAdmission += setup.EvaluationToAdmission
}
