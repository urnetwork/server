package work

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

var urlProbeFleetReadPhaseLabels = [server.DbReadPhaseCount]string{
	"starting", "acquire_begin", "acquire_done", "query_begin", "rows", "query_done", "error", "complete",
}

type providerUrlProbeFleetReadSnapshot struct {
	comparisonAt time.Time
	observation  *server.DbReadObservation
}

// Read phases describe only the latest attempted census, separately from the
// last successful census. One scrape loads a single immutable observation.
// All counters here reset at the next refresh and are therefore gauge values.
type providerUrlProbeFleetReadCollector struct {
	snapshot atomic.Pointer[providerUrlProbeFleetReadSnapshot]
	enabled  *prometheus.Desc
	state    *prometheus.Desc
	events   *prometheus.Desc
	phaseAt  *prometheus.Desc
}

func newProviderUrlProbeFleetReadCollector(registerer prometheus.Registerer) *providerUrlProbeFleetReadCollector {
	collector := &providerUrlProbeFleetReadCollector{
		enabled: prometheus.NewDesc("urnetwork_url_probe_fleet_read_observation_enabled",
			"Census read phase contract version 1; idle capability never fabricates an attempted or successful census", nil, nil),
		state: prometheus.NewDesc("urnetwork_url_probe_fleet_read",
			"Latest attempted census only: numeric source clocks, completed acquisition/query counts and client wall seconds, and decoded aggregate rows; no durable-credit or PostgreSQL execution-time claim", []string{"state"}, nil),
		events: prometheus.NewDesc("urnetwork_url_probe_fleet_read_phase_events",
			"Fixed phase event counts within the latest census attempt, including retries; reset on its next attempt", []string{"phase"}, nil),
		phaseAt: prometheus.NewDesc("urnetwork_url_probe_fleet_read_phase_timestamp_seconds",
			"Latest source UTC clock for each phase in the same attempted census; zero means the phase was not reached", []string{"phase"}, nil),
	}
	registerer.MustRegister(collector)
	return collector
}

var urlProbeFleetReadMetrics = newProviderUrlProbeFleetReadCollector(prometheus.DefaultRegisterer)

func (self *providerUrlProbeFleetReadCollector) begin(comparisonAt time.Time) *server.DbReadObservation {
	observation := server.NewDbReadObservation()
	if self != nil {
		self.snapshot.Store(&providerUrlProbeFleetReadSnapshot{comparisonAt: comparisonAt, observation: observation})
	}
	return observation
}

func (self *providerUrlProbeFleetReadCollector) Describe(metrics chan<- *prometheus.Desc) {
	metrics <- self.enabled
	metrics <- self.state
	metrics <- self.events
	metrics <- self.phaseAt
}

func (self *providerUrlProbeFleetReadCollector) Collect(metrics chan<- prometheus.Metric) {
	metrics <- prometheus.MustNewConstMetric(self.enabled, prometheus.GaugeValue, 1)
	snapshot := self.snapshot.Load()
	if snapshot == nil {
		return
	}
	read := snapshot.observation.Snapshot()
	seconds := func(at time.Time) float64 {
		if at.IsZero() {
			return 0
		}
		return float64(at.UnixNano()) / float64(time.Second)
	}
	finished := 0.0
	if read.Finished {
		finished = 1
	}
	for _, cell := range []struct {
		state string
		value float64
	}{
		{state: "comparison_timestamp_seconds", value: seconds(snapshot.comparisonAt)},
		{state: "started_timestamp_seconds", value: seconds(read.StartedAt)},
		{state: "updated_timestamp_seconds", value: seconds(read.UpdatedAt)},
		{state: "phase", value: float64(read.Phase)},
		{state: "finished", value: finished},
		{state: "acquire_succeeded", value: float64(read.AcquireSucceeded)},
		{state: "query_succeeded", value: float64(read.QuerySucceeded)},
		{state: "acquire_seconds", value: read.AcquireDuration.Seconds()},
		{state: "query_seconds", value: read.QueryDuration.Seconds()},
		{state: "rows", value: float64(read.Rows)},
	} {
		metrics <- prometheus.MustNewConstMetric(self.state, prometheus.GaugeValue, cell.value, cell.state)
	}
	for phase, label := range urlProbeFleetReadPhaseLabels {
		metrics <- prometheus.MustNewConstMetric(self.events, prometheus.GaugeValue, float64(read.PhaseCounts[phase]), label)
		metrics <- prometheus.MustNewConstMetric(self.phaseAt, prometheus.GaugeValue, seconds(read.PhaseAt[phase]), label)
	}
}
