package work

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// These stages belong to the synchronous shard scheduler, not its concurrent
// URL workers. In particular, waiting for a worker and draining a stopped pass
// are distinct from work performed by that worker.
const (
	urlSchedulerPrepare = iota
	urlSchedulerDispatch
	urlSchedulerDue
	urlSchedulerWait
	urlSchedulerDrain
	urlSchedulerUnstarted
	urlSchedulerFinalize
)

const (
	urlSchedulerReserve = iota
	urlSchedulerLimit
	urlSchedulerEmpty
	urlSchedulerTurnError
	urlSchedulerDueError
	urlSchedulerInvalidDue
	urlSchedulerCanceled
	urlSchedulerOther
)

const (
	urlDueFull = iota
	urlDuePartial
	urlDueEmpty
	urlDueMaintenance
	urlDueError
	urlDueInvalid
)

const (
	urlClaimAdmitted = iota
	urlClaimUnstarted
	urlClaimCompletionAcknowledged
	urlClaimCompletionFailed
)

var urlSchedulerPhaseNames = [...]string{"prepare", "dispatch", "due", "wait", "drain", "unstarted_completion", "finalize"}
var urlSchedulerStopNames = [...]string{"reserve", "selected_limit", "empty", "turn_error", "due_error", "invalid_due", "canceled", "other"}
var urlSchedulerDueNames = [...]string{"full", "partial", "empty", "maintenance", "error", "invalid"}
var urlSchedulerClaimNames = [...]string{"admitted", "unstarted", "completion_acknowledged", "completion_failed"}

type providerUrlProbeSchedulerSnapshot struct {
	active     [7]int64
	seconds    [7]float64
	stops      [8]uint64
	dueCalls   [6]uint64
	dueSeconds [6]float64
	claims     [4]uint64
}

// The sole label values are the finite enums above: 39 series per process.
// Active ownership is local and removed only when that invocation returns.
// No provider, URL, task identity or shard identity enters the collector.
type providerUrlProbeSchedulerMetrics struct {
	mu                                                            sync.Mutex
	values                                                        providerUrlProbeSchedulerSnapshot
	owners                                                        map[*providerUrlProbeSchedulerOwner]struct{}
	active, seconds, stops, dueCalls, dueSeconds, claims, enabled *prometheus.Desc
}

func newProviderUrlProbeSchedulerMetrics() *providerUrlProbeSchedulerMetrics {
	return &providerUrlProbeSchedulerMetrics{
		owners:     map[*providerUrlProbeSchedulerOwner]struct{}{},
		active:     prometheus.NewDesc("urnetwork_url_probe_scheduler_inflight", "Actual synchronous URL scheduler owners by phase; independent from concurrent worker state", []string{"phase"}, nil),
		seconds:    prometheus.NewDesc("urnetwork_url_probe_scheduler_seconds_total", "URL scheduler wall-clock residence by phase, including currently owned phase time; not worker residence or CPU", []string{"phase"}, nil),
		stops:      prometheus.NewDesc("urnetwork_url_probe_scheduler_stops_total", "First reason a URL pass stopped admitting work, observed only after the invocation returns", []string{"reason"}, nil),
		dueCalls:   prometheus.NewDesc("urnetwork_url_probe_due_calls_total", "Completed synchronous URL due requests by source result; successful responses may still arrive too late for admission", []string{"outcome"}, nil),
		dueSeconds: prometheus.NewDesc("urnetwork_url_probe_due_seconds_total", "Completed synchronous URL due request residence by source result", []string{"outcome"}, nil),
		claims:     prometheus.NewDesc("urnetwork_url_probe_claim_dispositions_total", "URL claims admitted or left unstarted after the admission boundary, and unstarted completion acknowledgments; neither measurements nor quota credit", []string{"disposition"}, nil),
		enabled:    prometheus.NewDesc("urnetwork_url_probe_scheduler_observation_enabled", "Executable capability for bounded identity-free URL scheduler phases and claim disposition", nil, nil),
	}
}

func (self *providerUrlProbeSchedulerMetrics) snapshot() providerUrlProbeSchedulerSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()
	value := self.values
	now := time.Now()
	for owner := range self.owners {
		value.active[owner.phase]++
		value.seconds[owner.phase] += now.Sub(owner.entered).Seconds()
	}
	return value
}

func (self *providerUrlProbeSchedulerMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{self.active, self.seconds, self.stops, self.dueCalls, self.dueSeconds, self.claims, self.enabled} {
		ch <- d
	}
}

func (self *providerUrlProbeSchedulerMetrics) Collect(ch chan<- prometheus.Metric) {
	value := self.snapshot()
	for i, name := range urlSchedulerPhaseNames {
		ch <- prometheus.MustNewConstMetric(self.active, prometheus.GaugeValue, float64(value.active[i]), name)
		ch <- prometheus.MustNewConstMetric(self.seconds, prometheus.CounterValue, value.seconds[i], name)
	}
	for i, name := range urlSchedulerStopNames {
		ch <- prometheus.MustNewConstMetric(self.stops, prometheus.CounterValue, float64(value.stops[i]), name)
	}
	for i, name := range urlSchedulerDueNames {
		ch <- prometheus.MustNewConstMetric(self.dueCalls, prometheus.CounterValue, float64(value.dueCalls[i]), name)
		ch <- prometheus.MustNewConstMetric(self.dueSeconds, prometheus.CounterValue, value.dueSeconds[i], name)
	}
	for i, name := range urlSchedulerClaimNames {
		ch <- prometheus.MustNewConstMetric(self.claims, prometheus.CounterValue, float64(value.claims[i]), name)
	}
	ch <- prometheus.MustNewConstMetric(self.enabled, prometheus.GaugeValue, 1)
}

var urlProbeSchedulerMetrics = newProviderUrlProbeSchedulerMetrics()

func init() { prometheus.MustRegister(urlProbeSchedulerMetrics) }

type providerUrlProbeSchedulerOwner struct {
	metrics     *providerUrlProbeSchedulerMetrics
	phase, stop int
	entered     time.Time
	closed      bool
}

func (self *providerUrlProbeSchedulerMetrics) begin() *providerUrlProbeSchedulerOwner {
	self.mu.Lock()
	defer self.mu.Unlock()
	owner := &providerUrlProbeSchedulerOwner{metrics: self, phase: urlSchedulerPrepare, stop: -1, entered: time.Now()}
	self.owners[owner] = struct{}{}
	return owner
}

func (self *providerUrlProbeSchedulerOwner) enter(phase int) {
	self.metrics.mu.Lock()
	defer self.metrics.mu.Unlock()
	if self.closed {
		return
	}
	now := time.Now()
	self.metrics.values.seconds[self.phase] += now.Sub(self.entered).Seconds()
	self.phase, self.entered = phase, now
}

func (self *providerUrlProbeSchedulerOwner) stopAdmission(reason int) {
	self.metrics.mu.Lock()
	defer self.metrics.mu.Unlock()
	if !self.closed && self.stop < 0 {
		self.stop = reason
	}
}

func (self *providerUrlProbeSchedulerOwner) due(result int, elapsed time.Duration) {
	self.metrics.mu.Lock()
	defer self.metrics.mu.Unlock()
	if self.closed {
		return
	}
	self.metrics.values.dueCalls[result]++
	self.metrics.values.dueSeconds[result] += elapsed.Seconds()
}

func (self *providerUrlProbeSchedulerOwner) claim(disposition int, count int) {
	self.metrics.mu.Lock()
	defer self.metrics.mu.Unlock()
	if !self.closed {
		self.metrics.values.claims[disposition] += uint64(count)
	}
}

func (self *providerUrlProbeSchedulerOwner) close() {
	self.metrics.mu.Lock()
	defer self.metrics.mu.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	self.metrics.values.seconds[self.phase] += time.Since(self.entered).Seconds()
	if self.stop < 0 {
		self.stop = urlSchedulerOther
	}
	self.metrics.values.stops[self.stop]++
	delete(self.metrics.owners, self)
}
