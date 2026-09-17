package model

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// updateClientScoresPhase is deliberately closed: each additional phase adds
// exactly one series to each collector in every taskworker process. Never put
// a target, caller, worker, host, or identifier in these labels.
type updateClientScoresPhase uint8

const (
	updateClientScoresPhaseSourceLoad updateClientScoresPhase = iota
	updateClientScoresPhaseTargetExport
	updateClientScoresPhaseTargetMap
	updateClientScoresPhaseGobEncode
	updateClientScoresPhaseCacheWrite
	updateClientScoresPhaseCount
)

var updateClientScoresPhaseNames = [...]string{
	"source_load",
	"target_export",
	"target_map",
	"gob_encode",
	"cache_write",
}

type updateClientScoresPhaseMetric struct {
	active   prometheus.Gauge
	duration prometheus.Counter
	exits    prometheus.Counter
	items    prometheus.Counter
	bytes    prometheus.Counter
}

// updateClientScoresPhaseMetricSet pre-binds every fixed label value. The hot
// 48-way export path therefore increments direct metric handles instead of
// performing a label-map lookup for each span.
type updateClientScoresPhaseMetricSet struct {
	active   *prometheus.GaugeVec
	duration *prometheus.CounterVec
	exits    *prometheus.CounterVec
	items    *prometheus.CounterVec
	bytes    *prometheus.CounterVec
	phase    [updateClientScoresPhaseCount]updateClientScoresPhaseMetric
	now      func() time.Time
}

func newUpdateClientScoresPhaseMetricSet(now func() time.Time) *updateClientScoresPhaseMetricSet {
	if now == nil {
		now = time.Now
	}
	metrics := &updateClientScoresPhaseMetricSet{
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "urnetwork_update_client_scores_phase_active",
			Help: "Current UpdateClientScores spans in each fixed phase; target_export is a parent of target_map, gob_encode, and cache_write",
		}, []string{"phase"}),
		duration: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_update_client_scores_phase_duration_seconds_total",
			Help: "Wall-clock seconds accumulated by completed UpdateClientScores phase spans",
		}, []string{"phase"}),
		exits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_update_client_scores_phase_exits_total",
			Help: "UpdateClientScores phase spans exited, including error and panic cleanup",
		}, []string{"phase"}),
		items: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_update_client_scores_phase_work_items_total",
			Help: "Deterministic UpdateClientScores work units: source rows, target passes, map/filter inputs inspected, gob values encoded, or Redis SET attempts",
		}, []string{"phase"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_update_client_scores_phase_work_bytes_total",
			Help: "Deterministic bytes produced by gob encoding or submitted in Redis SET attempts; zero for phases without a byte unit",
		}, []string{"phase"}),
		now: now,
	}
	for phase := updateClientScoresPhase(0); phase < updateClientScoresPhaseCount; phase++ {
		name := updateClientScoresPhaseNames[phase]
		metrics.phase[phase] = updateClientScoresPhaseMetric{
			active:   metrics.active.WithLabelValues(name),
			duration: metrics.duration.WithLabelValues(name),
			exits:    metrics.exits.WithLabelValues(name),
			items:    metrics.items.WithLabelValues(name),
			bytes:    metrics.bytes.WithLabelValues(name),
		}
	}
	return metrics
}

func (m *updateClientScoresPhaseMetricSet) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.active, m.duration, m.exits, m.items, m.bytes}
}

func (m *updateClientScoresPhaseMetricSet) metric(phase updateClientScoresPhase) *updateClientScoresPhaseMetric {
	if phase >= updateClientScoresPhaseCount {
		panic(fmt.Sprintf("invalid UpdateClientScores phase %d", phase))
	}
	return &m.phase[phase]
}

type updateClientScoresPhaseSpan struct {
	metrics *updateClientScoresPhaseMetricSet
	phase   updateClientScoresPhase
	start   time.Time
	done    bool
}

func (m *updateClientScoresPhaseMetricSet) start(phase updateClientScoresPhase) updateClientScoresPhaseSpan {
	metric := m.metric(phase)
	start := m.now()
	metric.active.Inc()
	return updateClientScoresPhaseSpan{metrics: m, phase: phase, start: start}
}

// finish is idempotent so callers can both defer it for panic/error cleanup
// and end a phase early before entering a nested phase.
func (s *updateClientScoresPhaseSpan) finish() {
	if s.done {
		return
	}
	s.done = true
	metric := s.metrics.metric(s.phase)
	duration := s.metrics.now().Sub(s.start)
	if duration < 0 {
		duration = 0
	}
	metric.duration.Add(duration.Seconds())
	metric.exits.Inc()
	metric.active.Dec()
}

func (m *updateClientScoresPhaseMetricSet) addWork(phase updateClientScoresPhase, items, workBytes int) {
	if items < 0 || workBytes < 0 {
		panic("UpdateClientScores phase work cannot be negative")
	}
	metric := m.metric(phase)
	metric.items.Add(float64(items))
	metric.bytes.Add(float64(workBytes))
}

// encodeClientScoreGobValue preserves the writer's existing ignored-encode-
// error behavior while measuring exact produced bytes. The byte count is
// deterministic for the value; it is not presented as a process allocation.
func encodeClientScoreGobValue(metrics *updateClientScoresPhaseMetricSet, value any) []byte {
	span := metrics.start(updateClientScoresPhaseGobEncode)
	defer span.finish()
	b := bytes.NewBuffer(nil)
	e := gob.NewEncoder(b)
	e.Encode(value)
	valueBytes := b.Bytes()
	metrics.addWork(updateClientScoresPhaseGobEncode, 1, len(valueBytes))
	return valueBytes
}

var updateClientScoresPhaseMetrics = newUpdateClientScoresPhaseMetricSet(time.Now)

func init() {
	prometheus.MustRegister(updateClientScoresPhaseMetrics.collectors()...)
}
