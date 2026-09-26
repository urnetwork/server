package work

// Process-local aggregate ownership, not task identities or durable verdicts.
// One mutex gives a coherent scrape without retaining an owner registry.

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
)

type providerEgressBlackholeProgressSnapshot struct {
	states      [4]int64
	events      [4]uint64
	submissions [3]uint64
}

type providerEgressBlackholeProgressMetrics struct {
	mu             sync.Mutex
	values         providerEgressBlackholeProgressSnapshot
	stateDesc      *prometheus.Desc
	eventDesc      *prometheus.Desc
	submissionDesc *prometheus.Desc
	enabledDesc    *prometheus.Desc
}

func newProviderEgressBlackholeProgressMetrics() *providerEgressBlackholeProgressMetrics {
	return &providerEgressBlackholeProgressMetrics{
		stateDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_inflight",
			"Aggregate currently owned blackhole batch states; completed_buffered includes unmeasured/guardable results, some safe results may be submitted early, and the gauge does not prove durability", []string{"state"}, nil),
		eventDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_worker_events_total",
			"Actual blackhole worker lifecycle events; completed means retained before guard and publication, canceled/discarded do not retain a result", []string{"event"}, nil),
		submissionDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_submission_outcomes_total",
			"Nonempty blackhole batch calls by returned outcome; acknowledged is not a measured-row count or proof of row replacement", []string{"outcome"}, nil),
		enabledDesc: prometheus.NewDesc("urnetwork_egress_probe_blackhole_progress_enabled",
			"Executable capability for fixed-cardinality actual-worker progress and post-call blackhole submission outcomes", nil, nil),
	}
}

var egressProbeBlackholeProgress = newProviderEgressBlackholeProgressMetrics()

func init() {
	prometheus.MustRegister(egressProbeBlackholeProgress)
}

func (self *providerEgressBlackholeProgressMetrics) snapshot() providerEgressBlackholeProgressSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.values
}

func (self *providerEgressBlackholeProgressMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- self.stateDesc
	ch <- self.eventDesc
	ch <- self.submissionDesc
	ch <- self.enabledDesc
}

func (self *providerEgressBlackholeProgressMetrics) Collect(ch chan<- prometheus.Metric) {
	values := self.snapshot()
	for index, state := range []string{"active_batches", "queued", "running", "completed_buffered"} {
		ch <- prometheus.MustNewConstMetric(self.stateDesc, prometheus.GaugeValue, float64(values.states[index]), state)
	}
	for index, event := range []string{"started", "completed", "canceled", "discarded"} {
		ch <- prometheus.MustNewConstMetric(self.eventDesc, prometheus.CounterValue, float64(values.events[index]), event)
	}
	for index, outcome := range []string{"acknowledged", "canceled", "error_or_unknown"} {
		ch <- prometheus.MustNewConstMetric(self.submissionDesc, prometheus.CounterValue, float64(values.submissions[index]), outcome)
	}
	ch <- prometheus.MustNewConstMetric(self.enabledDesc, prometheus.GaugeValue, 1)
}

// A batch owns only these aggregate contributions. Closing it cannot erase a
// sibling batch and is not an acknowledgement. Its workers must already join.
type providerEgressBlackholeProgressOwner struct {
	metrics                   *providerEgressBlackholeProgressMetrics
	queued, running, buffered int64
	closed                    bool
}

func (self *providerEgressBlackholeProgressMetrics) begin(selected int) *providerEgressBlackholeProgressOwner {
	owner := &providerEgressBlackholeProgressOwner{metrics: self, queued: int64(max(0, selected))}
	self.mu.Lock()
	self.values.states[0]++
	self.values.states[1] += owner.queued
	self.mu.Unlock()
	return owner
}

func (self *providerEgressBlackholeProgressOwner) observe(event fleetprobe.BlackholeProgress) {
	metrics := self.metrics
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if self.closed {
		return
	}
	switch event {
	case fleetprobe.BlackholeStarted:
		if self.queued <= 0 {
			return
		}
		self.queued--
		self.running++
		metrics.values.states[1]--
		metrics.values.states[2]++
		metrics.values.events[0]++
	case fleetprobe.BlackholeCompleted, fleetprobe.BlackholeCanceled, fleetprobe.BlackholeDiscarded:
		if self.running <= 0 {
			return
		}
		self.running--
		metrics.values.states[2]--
		switch event {
		case fleetprobe.BlackholeCompleted:
			self.buffered++
			metrics.values.states[3]++
			metrics.values.events[1]++
		case fleetprobe.BlackholeCanceled:
			metrics.values.events[2]++
		case fleetprobe.BlackholeDiscarded:
			metrics.values.events[3]++
		}
	}
}

func (self *providerEgressBlackholeProgressOwner) close() {
	metrics := self.metrics
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	metrics.values.states[0]--
	metrics.values.states[1] -= self.queued
	metrics.values.states[2] -= self.running
	metrics.values.states[3] -= self.buffered
}

func (self *providerEgressBlackholeProgressMetrics) submitted(err error) {
	index := 0
	if err != nil {
		index = 2
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			index = 1
		}
	}
	self.mu.Lock()
	self.values.submissions[index]++
	self.mu.Unlock()
}
