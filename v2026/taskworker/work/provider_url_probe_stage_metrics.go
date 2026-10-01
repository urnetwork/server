package work

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

var urlProbeStageNames = [8]string{
	"readiness", "open", "check_and_buffer", "close_join", "attempt_report", "publication", "other", "total",
}

type providerUrlProbeStageSnapshot struct {
	seconds                        [8]float64
	timed, unobserved              uint64
	setup                          providertunnel.SetupTiming
	setupObserved, setupUnobserved uint64
}

// A single copy under one lock keeps all stage sums and their completed-turn
// denominator in the same scrape generation. These are wall-clock occupancy,
// not CPU, successful measurements, or a census of still-running probes.
type providerUrlProbeStageMetrics struct {
	mu                      sync.Mutex
	values                  providerUrlProbeStageSnapshot
	seconds, turns, enabled *prometheus.Desc
	setup                   *providerUrlProbeSetupDescriptors
}

func newProviderUrlProbeStageMetrics() *providerUrlProbeStageMetrics {
	return &providerUrlProbeStageMetrics{
		setup: newProviderUrlProbeSetupDescriptors(),
		seconds: prometheus.NewDesc("urnetwork_url_probe_completed_stage_seconds_total",
			"Wall-clock stages from the same returned one-provider URL batches; check_and_buffer includes asynchronous route setup, total excludes still-running and unobserved batches", []string{"stage"}, nil),
		turns: prometheus.NewDesc("urnetwork_url_probe_completed_stage_turns_total",
			"Returned one-provider URL batches by complete timing coverage; neither a success nor durable coverage counter", []string{"coverage"}, nil),
		enabled: prometheus.NewDesc("urnetwork_url_probe_completed_stage_timing_enabled",
			"Executable capability for coherent identity-free completed URL batch timing", nil, nil),
	}
}

func (self *providerUrlProbeStageMetrics) snapshot() providerUrlProbeStageSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.values
}

func (self *providerUrlProbeStageMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.seconds
	ch <- self.turns
	ch <- self.enabled
	self.setup.describe(ch)
}

func (self *providerUrlProbeStageMetrics) Collect(ch chan<- prometheus.Metric) {
	values := self.snapshot()
	for i, stage := range urlProbeStageNames {
		ch <- prometheus.MustNewConstMetric(self.seconds, prometheus.CounterValue, values.seconds[i], stage)
	}
	ch <- prometheus.MustNewConstMetric(self.turns, prometheus.CounterValue, float64(values.timed), "timed")
	ch <- prometheus.MustNewConstMetric(self.turns, prometheus.CounterValue, float64(values.unobserved), "unobserved")
	ch <- prometheus.MustNewConstMetric(self.enabled, prometheus.GaugeValue, 1)
	self.setup.collect(ch, values)
}

var urlProbeStageMetrics = newProviderUrlProbeStageMetrics()

func init() {
	prometheus.MustRegister(urlProbeStageMetrics)
}

// One URL batch owns one provider. Its worker callback only snapshots timing;
// publication happens after the joined scheduler and durable release return.
type providerUrlProbeStageOwner struct {
	metrics                *providerUrlProbeStageMetrics
	started                time.Time
	readiness, publication time.Duration
	mu                     sync.Mutex
	probeCount             int
	probe                  prober.ProbeTiming
	setup                  *providertunnel.SetupObservations
	finished               bool
}

func newProviderUrlProbeStageOwner(metrics *providerUrlProbeStageMetrics) *providerUrlProbeStageOwner {
	return &providerUrlProbeStageOwner{metrics: metrics, started: time.Now(), setup: &providertunnel.SetupObservations{}}
}

func (self *providerUrlProbeStageOwner) now() time.Time {
	if self == nil {
		return time.Time{}
	}
	return time.Now()
}

func (self *providerUrlProbeStageOwner) finishReadiness(start time.Time) {
	if self != nil {
		self.readiness = time.Since(start)
	}
}

func (self *providerUrlProbeStageOwner) finishPublication(start time.Time) {
	if self != nil {
		self.publication = time.Since(start)
	}
}

func (self *providerUrlProbeStageOwner) observe(probe prober.ProbeTiming) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.finished {
		return
	}
	self.probeCount = min(2, self.probeCount+1)
	self.probe = probe
}

func (self *providerUrlProbeStageOwner) finish() {
	total := time.Since(self.started)
	self.mu.Lock()
	if self.finished {
		self.mu.Unlock()
		return
	}
	self.finished = true
	probe, count := self.probe, self.probeCount
	self.mu.Unlock()
	stages := [8]time.Duration{self.readiness, probe.Open, probe.CheckAndBuffer,
		probe.CloseJoin, probe.AttemptReport, self.publication, 0, total}
	valid := count == 1 && probe.Total >= 0 && probe.Total <= total && probe.Other >= 0
	probeParts := probe.Open + probe.CheckAndBuffer + probe.CloseJoin + probe.AttemptReport + probe.Other
	valid = valid && probeParts == probe.Total
	var measured time.Duration
	for _, value := range stages[:6] {
		valid = valid && value >= 0 && value <= total
		measured += value
	}
	valid = valid && measured <= total
	stages[6] = total - measured
	setup := self.setup.Snapshot()
	self.metrics.mu.Lock()
	defer self.metrics.mu.Unlock()
	self.metrics.values.addSetup(setup, valid)
	if !valid {
		// No start, an unsupported injected runner, or ambiguous callback
		// coverage must not look like a returned zero-latency provider probe.
		self.metrics.values.unobserved++
		return
	}
	self.metrics.values.timed++
	for i, value := range stages {
		self.metrics.values.seconds[i] += value.Seconds()
	}
}
