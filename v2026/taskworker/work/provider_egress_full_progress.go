// Fixed-cardinality full-lane progress distinguishes active probes from idle
// lanes without exposing provider identities or treating a finish as coverage.
package work

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type providerEgressFullProgressSnapshot struct {
	states     [4]int64
	events     [4]uint64
	selections [3]uint64
}

// One lock makes each scrape coherent across overlapping owners. This is
// aggregate telemetry, not a shared admission budget or an identity registry.
type providerEgressFullProgressMetrics struct {
	stateLock     sync.Mutex
	values        providerEgressFullProgressSnapshot
	stateDesc     *prometheus.Desc
	eventDesc     *prometheus.Desc
	selectionDesc *prometheus.Desc
	enabledDesc   *prometheus.Desc
}

// Predeclares all series, including zero activity, for capability-aware reads.
func newProviderEgressFullProgressMetrics() *providerEgressFullProgressMetrics {
	return &providerEgressFullProgressMetrics{
		stateDesc: prometheus.NewDesc("urnetwork_egress_probe_full_inflight",
			"Currently owned full batches and provider states; queued includes selected not started, finished_waiting includes failed/unmeasured work before batch guard and release, not durable coverage", []string{"state"}, nil),
		eventDesc: prometheus.NewDesc("urnetwork_egress_probe_full_worker_events_total",
			"Actual full batch and provider lifecycle events; provider_finished is ProbeOne return including teardown, not a successful or durable measurement", []string{"event"}, nil),
		selectionDesc: prometheus.NewDesc("urnetwork_egress_probe_full_selection_total",
			"Bounded full successor lookahead outcomes; prefix_exhausted means no unseen row in that bounded response, not an empty fleet backlog", []string{"outcome"}, nil),
		enabledDesc: prometheus.NewDesc("urnetwork_egress_probe_full_progress_enabled",
			"Executable capability for identity-free actual full-worker progress and bounded-prefix selection telemetry", nil, nil),
	}
}

var egressProbeFullProgress = newProviderEgressFullProgressMetrics()

func init() {
	prometheus.MustRegister(egressProbeFullProgress)
}

// Copies a coherent aggregate; no external callback runs under its lock.
func (self *providerEgressFullProgressMetrics) snapshot() providerEgressFullProgressSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.values
}

func (self *providerEgressFullProgressMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.stateDesc
	ch <- self.eventDesc
	ch <- self.selectionDesc
	ch <- self.enabledDesc
}

func (self *providerEgressFullProgressMetrics) Collect(ch chan<- prometheus.Metric) {
	values := self.snapshot()
	for index, state := range []string{"active_batches", "queued", "running", "finished_waiting"} {
		ch <- prometheus.MustNewConstMetric(self.stateDesc, prometheus.GaugeValue, float64(values.states[index]), state)
	}
	for index, event := range []string{"batch_started", "batch_finished", "provider_started", "provider_finished"} {
		ch <- prometheus.MustNewConstMetric(self.eventDesc, prometheus.CounterValue, float64(values.events[index]), event)
	}
	for index, outcome := range []string{"prefix_lookup", "prefix_advanced", "prefix_exhausted"} {
		ch <- prometheus.MustNewConstMetric(self.selectionDesc, prometheus.CounterValue, float64(values.selections[index]), outcome)
	}
	ch <- prometheus.MustNewConstMetric(self.enabledDesc, prometheus.GaugeValue, 1)
}

// Each batch owns only its contributions. The caller joins workers and
// releases guarded evidence before close; a sibling's state is never erased.
type providerEgressFullProgressOwner struct {
	metrics                   *providerEgressFullProgressMetrics
	queued, running, finished int64
	closed                    bool
}

func (self *providerEgressFullProgressMetrics) begin(selected int) *providerEgressFullProgressOwner {
	owner := &providerEgressFullProgressOwner{metrics: self, queued: int64(max(0, selected))}
	self.stateLock.Lock()
	self.values.states[0]++
	self.values.states[1] += owner.queued
	self.values.events[0]++
	self.stateLock.Unlock()
	return owner
}

// Only actual admitted ProbeOne calls move queued to running; a return may
// still mean no measurement. Owners ignore late notifications after close.
func (self *providerEgressFullProgressOwner) observe(event prober.Progress) {
	metrics := self.metrics
	metrics.stateLock.Lock()
	defer metrics.stateLock.Unlock()
	if self.closed {
		return
	}
	switch event {
	case prober.ProbeStarted:
		if self.queued <= 0 {
			return
		}
		self.queued--
		self.running++
		metrics.values.states[1]--
		metrics.values.states[2]++
		metrics.values.events[2]++
	case prober.ProbeFinished:
		if self.running <= 0 {
			return
		}
		self.running--
		self.finished++
		metrics.values.states[2]--
		metrics.values.states[3]++
		metrics.values.events[3]++
	}
}

func (self *providerEgressFullProgressOwner) close() {
	metrics := self.metrics
	metrics.stateLock.Lock()
	defer metrics.stateLock.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	metrics.values.states[0]--
	metrics.values.states[1] -= self.queued
	metrics.values.states[2] -= self.running
	metrics.values.states[3] -= self.finished
	metrics.values.events[1]++
}

func (self *providerEgressFullProgressMetrics) selection(index int) {
	self.stateLock.Lock()
	self.values.selections[index]++
	self.stateLock.Unlock()
}
