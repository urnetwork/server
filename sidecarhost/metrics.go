package sidecarhost

// ServiceHost.Metrics on prometheus (host_metrics.go; §19.8, D-OPS-8): the process's default registry,
// so server.StartStatsPusher carries an engine's metrics with every other.
//
// A declaration is held to §19.8 when it is made, so a metric that would carry an identity never exists:
//
//   - a name outside urnetwork_<svc>_* panics;
//   - a label named for a network, client, repo, host, alias, address, user or domain panics (ops-1,
//     I-13): a label's values come from a closed set the engine names, never from a request;
//   - a name declared again with another kind or label set panics, as prometheus's MustRegister does,
//     and one declared again alike returns the same metric, so two engines in one process, or a
//     restarted one in a test, share it. Help and buckets are the first declaration's.
//
// What a label's value holds is not checked here, since a panic on a value would end a serving
// process: T2's sidecartest.FakeMetrics flags an id or an address in one. Safe for concurrent use.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar"
)

var _ sidecar.Metrics = (*Metrics)(nil)

// urnetwork_<svc>_*: lowercase words joined by _, the service first.
var metricsNamePattern = regexp.MustCompile(`^urnetwork_[a-z0-9]+_[a-z0-9_]+$`)

// A label name's words that name an identity (D-OPS-8, ops-1): a network, client, repo, host, alias,
// address, user or domain, and the spellings of each.
var metricsIdentityLabelWords = map[string]bool{
	"network":    true,
	"networks":   true,
	"client":     true,
	"clients":    true,
	"repo":       true,
	"repos":      true,
	"repository": true,
	"host":       true,
	"hosts":      true,
	"hostname":   true,
	"alias":      true,
	"aliases":    true,
	"address":    true,
	"addresses":  true,
	"addr":       true,
	"ip":         true,
	"user":       true,
	"users":      true,
	"username":   true,
	"account":    true,
	"domain":     true,
	"domains":    true,
	"email":      true,
}

// Why a label name may not be declared, nil when it may.
func checkMetricsLabelName(metricName string, labelName string) error {
	for _, word := range strings.FieldsFunc(strings.ToLower(labelName), func(r rune) bool {
		return r == '_' || r == '-'
	}) {
		if metricsIdentityLabelWords[word] {
			return fmt.Errorf("metric %q: label %q names an identity (D-OPS-8, I-13)", metricName, labelName)
		}
	}
	return nil
}

type metricsKind string

const (
	metricsCounter   metricsKind = "counter"
	metricsGauge     metricsKind = "gauge"
	metricsHistogram metricsKind = "histogram"
)

// One declared metric.
type metricsDeclared struct {
	kind       metricsKind
	labelNames []string
	counter    *prometheus.CounterVec
	gauge      *prometheus.GaugeVec
	histogram  *prometheus.HistogramVec
}

// Metrics on one registry. NewMetrics gives the process's, on the default registry.
type Metrics struct {
	registerer prometheus.Registerer

	stateLock sync.Mutex
	declared  map[string]*metricsDeclared
}

// The process's metrics on prometheus's default registry, one for every host in the process, so a
// name two engines declare alike is one metric.
func NewMetrics() *Metrics {
	return processMetrics()
}

var processMetrics = sync.OnceValue(func() *Metrics {
	return newMetrics(prometheus.DefaultRegisterer)
})

func newMetrics(registerer prometheus.Registerer) *Metrics {
	return &Metrics{
		registerer: registerer,
		declared:   map[string]*metricsDeclared{},
	}
}

// Holds a declaration to §19.8 and to what name already declared, returning the earlier one when it
// matches, or nil for a new name. Panics as host_metrics.go says.
func (self *Metrics) declaredWithLock(name string, kind metricsKind, labelNames []string) *metricsDeclared {
	if !metricsNamePattern.MatchString(name) {
		panic(fmt.Sprintf("metric %q is not urnetwork_<svc>_* (§19.8)", name))
	}
	for _, labelName := range labelNames {
		if err := checkMetricsLabelName(name, labelName); err != nil {
			panic(err.Error())
		}
	}
	if declared, ok := self.declared[name]; ok {
		if declared.kind != kind || !slices.Equal(declared.labelNames, labelNames) {
			panic(fmt.Sprintf("metric %q redeclared: a %s of %v, then a %s of %v", name, declared.kind, declared.labelNames, kind, labelNames))
		}
		return declared
	}
	return nil
}

// Registers collector, or panics as MustRegister does.
func (self *Metrics) mustRegister(collector prometheus.Collector) {
	self.registerer.MustRegister(collector)
}

// See sidecar.Metrics.
func (self *Metrics) Counter(name string, help string, labelNames ...string) sidecar.MetricsCounter {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if declared := self.declaredWithLock(name, metricsCounter, labelNames); declared != nil {
		return &metricsCounterVec{vec: declared.counter}
	}
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labelNames)
	self.mustRegister(vec)
	self.declared[name] = &metricsDeclared{kind: metricsCounter, labelNames: slices.Clone(labelNames), counter: vec}
	return &metricsCounterVec{vec: vec}
}

// See sidecar.Metrics.
func (self *Metrics) Gauge(name string, help string, labelNames ...string) sidecar.MetricsGauge {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if declared := self.declaredWithLock(name, metricsGauge, labelNames); declared != nil {
		return &metricsGaugeVec{vec: declared.gauge}
	}
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labelNames)
	self.mustRegister(vec)
	self.declared[name] = &metricsDeclared{kind: metricsGauge, labelNames: slices.Clone(labelNames), gauge: vec}
	return &metricsGaugeVec{vec: vec}
}

// See sidecar.Metrics: nil bounds take prometheus's default buckets.
func (self *Metrics) Histogram(name string, help string, bucketUpperBounds []float64, labelNames ...string) sidecar.MetricsHistogram {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if declared := self.declaredWithLock(name, metricsHistogram, labelNames); declared != nil {
		return &metricsHistogramVec{vec: declared.histogram}
	}
	if bucketUpperBounds == nil {
		bucketUpperBounds = prometheus.DefBuckets
	}
	vec := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: slices.Clone(bucketUpperBounds)}, labelNames)
	self.mustRegister(vec)
	self.declared[name] = &metricsDeclared{kind: metricsHistogram, labelNames: slices.Clone(labelNames), histogram: vec}
	return &metricsHistogramVec{vec: vec}
}

// A declared counter. A wrong number of label values or a negative value panics, as prometheus does.
type metricsCounterVec struct {
	vec *prometheus.CounterVec
}

// See sidecar.MetricsCounter.
func (self *metricsCounterVec) Add(value float64, labelValues ...string) {
	self.vec.WithLabelValues(labelValues...).Add(value)
}

// A declared gauge.
type metricsGaugeVec struct {
	vec *prometheus.GaugeVec
}

// See sidecar.MetricsGauge.
func (self *metricsGaugeVec) Set(value float64, labelValues ...string) {
	self.vec.WithLabelValues(labelValues...).Set(value)
}

// A declared histogram.
type metricsHistogramVec struct {
	vec *prometheus.HistogramVec
}

// See sidecar.MetricsHistogram.
func (self *metricsHistogramVec) Observe(value float64, labelValues ...string) {
	self.vec.WithLabelValues(labelValues...).Observe(value)
}
