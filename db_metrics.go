package server

// Read-only PostgreSQL client-pool saturation metrics. Collection never opens
// a pool; a service that has not used one emits no series for it.

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// pgPoolMetricSnapshot is one identity-free pgx pool generation snapshot.
type pgPoolMetricSnapshot struct {
	acquiredConnections int32
	idleConnections     int32
	totalConnections    int32
	maximumConnections  int32
	acquires            int64
	emptyAcquires       int64
	canceledAcquires    int64
	acquireDuration     time.Duration
	newConnections      int64
	lifetimeDestroyed   int64
	idleDestroyed       int64
}

// pgPoolMetricsSource supplies a snapshot without opening a pool.
type pgPoolMetricsSource interface {
	metricSnapshot() (pgPoolMetricSnapshot, bool)
}

// metricSnapshot returns false until this pool has actually been opened.
func (self *safePgPool) metricSnapshot() (pgPoolMetricSnapshot, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.pool == nil {
		return pgPoolMetricSnapshot{}, false
	}
	stats := self.pool.Stat()
	return pgPoolMetricSnapshot{
		acquiredConnections: stats.AcquiredConns(),
		idleConnections:     stats.IdleConns(),
		totalConnections:    stats.TotalConns(),
		maximumConnections:  stats.MaxConns(),
		acquires:            stats.AcquireCount(),
		emptyAcquires:       stats.EmptyAcquireCount(),
		canceledAcquires:    stats.CanceledAcquireCount(),
		acquireDuration:     stats.AcquireDuration(),
		newConnections:      stats.NewConnsCount(),
		lifetimeDestroyed:   stats.MaxLifetimeDestroyCount(),
		idleDestroyed:       stats.MaxIdleDestroyCount(),
	}, true
}

// pgPoolMetricsCollector publishes the two finite pool roles.
type pgPoolMetricsCollector struct {
	connectionsDesc     *prometheus.Desc
	acquiresDesc        *prometheus.Desc
	acquireDurationDesc *prometheus.Desc
	createdDesc         *prometheus.Desc
	destroyedDesc       *prometheus.Desc
	stateLock           sync.Mutex
	sources             map[string]pgPoolMetricsSource
}

// newPgPoolMetricsCollector constructs a collector over finite pool roles.
func newPgPoolMetricsCollector(sources map[string]pgPoolMetricsSource) *pgPoolMetricsCollector {
	return &pgPoolMetricsCollector{
		connectionsDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_connections",
			"PostgreSQL client-pool connections by finite pool role and state.",
			[]string{"pool", "state"}, nil,
		),
		acquiresDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_acquires_total",
			"Cumulative PostgreSQL client-pool acquire outcomes for this process generation.",
			[]string{"pool", "outcome"}, nil,
		),
		acquireDurationDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_acquire_duration_seconds_total",
			"Cumulative time spent acquiring PostgreSQL client-pool connections.",
			[]string{"pool"}, nil,
		),
		createdDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_connections_created_total",
			"Cumulative PostgreSQL client-pool connections created in this process generation.",
			[]string{"pool"}, nil,
		),
		destroyedDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_connections_destroyed_total",
			"Cumulative PostgreSQL client-pool connections destroyed by bounded reason.",
			[]string{"pool", "reason"}, nil,
		),
		sources: sources,
	}
}

var pgPoolMetrics = newPgPoolMetricsCollector(map[string]pgPoolMetricsSource{
	"default":     safePool,
	"maintenance": safeMaintenancePool,
})

func init() {
	prometheus.MustRegister(pgPoolMetrics)
}

// Describe implements prometheus.Collector.
func (self *pgPoolMetricsCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.connectionsDesc
	descriptions <- self.acquiresDesc
	descriptions <- self.acquireDurationDesc
	descriptions <- self.createdDesc
	descriptions <- self.destroyedDesc
}

// Collect implements prometheus.Collector without initializing unused pools.
func (self *pgPoolMetricsCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	sources := make(map[string]pgPoolMetricsSource, len(self.sources))
	for role, source := range self.sources {
		sources[role] = source
	}
	self.stateLock.Unlock()
	for role, source := range sources {
		snapshot, ok := source.metricSnapshot()
		if !ok {
			continue
		}
		for _, state := range []struct {
			name  string
			value int32
		}{
			{name: "acquired", value: snapshot.acquiredConnections},
			{name: "idle", value: snapshot.idleConnections},
			{name: "total", value: snapshot.totalConnections},
			{name: "maximum", value: snapshot.maximumConnections},
		} {
			metrics <- prometheus.MustNewConstMetric(self.connectionsDesc, prometheus.GaugeValue, float64(state.value), role, state.name)
		}
		for _, outcome := range []struct {
			name  string
			value int64
		}{
			{name: "acquired", value: snapshot.acquires},
			{name: "empty", value: snapshot.emptyAcquires},
			{name: "canceled", value: snapshot.canceledAcquires},
		} {
			metrics <- prometheus.MustNewConstMetric(self.acquiresDesc, prometheus.CounterValue, float64(outcome.value), role, outcome.name)
		}
		metrics <- prometheus.MustNewConstMetric(self.acquireDurationDesc, prometheus.CounterValue, snapshot.acquireDuration.Seconds(), role)
		metrics <- prometheus.MustNewConstMetric(self.createdDesc, prometheus.CounterValue, float64(snapshot.newConnections), role)
		metrics <- prometheus.MustNewConstMetric(self.destroyedDesc, prometheus.CounterValue, float64(snapshot.lifetimeDestroyed), role, "max_lifetime")
		metrics <- prometheus.MustNewConstMetric(self.destroyedDesc, prometheus.CounterValue, float64(snapshot.idleDestroyed), role, "max_idle")
	}
}
