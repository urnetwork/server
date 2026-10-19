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
	acquiredConnections     int32
	constructingConnections int32
	idleConnections         int32
	totalConnections        int32
	maximumConnections      int32
	acquires                int64
	emptyAcquires           int64
	canceledAcquires        int64
	acquireDuration         time.Duration
	newConnections          int64
	lifetimeDestroyed       int64
	idleDestroyed           int64
	wrapper                 pgPoolWrapperSnapshot
	startup                 [pgPoolStartupPhaseCount]pgPoolStartupPhaseSnapshot
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
		acquiredConnections:     stats.AcquiredConns(),
		constructingConnections: stats.ConstructingConns(),
		idleConnections:         stats.IdleConns(),
		totalConnections:        stats.TotalConns(),
		maximumConnections:      stats.MaxConns(),
		acquires:                stats.AcquireCount(),
		emptyAcquires:           stats.EmptyAcquireCount(),
		canceledAcquires:        stats.CanceledAcquireCount(),
		acquireDuration:         stats.AcquireDuration(),
		newConnections:          stats.NewConnsCount(),
		lifetimeDestroyed:       stats.MaxLifetimeDestroyCount(),
		idleDestroyed:           stats.MaxIdleDestroyCount(),
		wrapper:                 self.wrapperSnapshot(self.pool),
		startup:                 self.startupMetrics.snapshot(),
	}, true
}

// pgPoolMetricsCollector publishes the two finite pool roles.
type pgPoolMetricsCollector struct {
	connectionsDesc     *prometheus.Desc
	acquiresDesc        *prometheus.Desc
	acquireDurationDesc *prometheus.Desc
	createdDesc         *prometheus.Desc
	destroyedDesc       *prometheus.Desc
	wrapperDesc         *prometheus.Desc
	trackingDroppedDesc *prometheus.Desc
	startupActiveDesc   *prometheus.Desc
	startupCompleteDesc *prometheus.Desc
	startupDurationDesc *prometheus.Desc
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
			"Cumulative PostgreSQL client-pool constructor starts in this process generation; includes failed construction.",
			[]string{"pool"}, nil,
		),
		destroyedDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_connections_destroyed_total",
			"Cumulative PostgreSQL client-pool connections destroyed by bounded reason.",
			[]string{"pool", "reason"}, nil,
		),
		wrapperDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_wrapper_connections",
			"Observed Db/Tx wrapper ownership, release, and pending disposal by fixed state; excludes raw leases and pool-internal disposal, and is sampled separately from pool state.",
			[]string{"pool", "state"}, nil,
		),
		trackingDroppedDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_wrapper_cleanup_tracking_dropped_total",
			"Cumulative wrapper cleanup observations omitted by this process and pool role because a generation's bounded registry was full; pending cleanup coverage may be incomplete.",
			[]string{"pool"}, nil,
		),
		startupActiveDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_startup_phases_active",
			"PostgreSQL constructors inside initial Ping or failed-startup cleanup; sampled separately from pool state.",
			[]string{"pool", "phase"}, nil,
		),
		startupCompleteDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_startup_phases_completed_total",
			"Completed PostgreSQL startup phases by fixed outcome for this pool generation; cleanup ok requires observed CleanupDone.",
			[]string{"pool", "phase", "outcome"}, nil,
		),
		startupDurationDesc: prometheus.NewDesc(
			"urnetwork_pg_pool_startup_phase_duration_seconds_total",
			"Cumulative completed PostgreSQL startup phase time for this pool generation; excludes still-active phases.",
			[]string{"pool", "phase"}, nil,
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
	descriptions <- self.wrapperDesc
	descriptions <- self.trackingDroppedDesc
	descriptions <- self.startupActiveDesc
	descriptions <- self.startupCompleteDesc
	descriptions <- self.startupDurationDesc
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
			{name: "constructing", value: snapshot.constructingConnections},
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
		for _, state := range []struct {
			name  string
			value int64
		}{
			{name: "owned", value: snapshot.wrapper.owned},
			{name: "releasing", value: snapshot.wrapper.releasing},
			{name: "cleanup_pending", value: snapshot.wrapper.cleanupPending},
		} {
			metrics <- prometheus.MustNewConstMetric(self.wrapperDesc, prometheus.GaugeValue, float64(state.value), role, state.name)
		}
		metrics <- prometheus.MustNewConstMetric(self.trackingDroppedDesc, prometheus.CounterValue, float64(snapshot.wrapper.trackingDropped), role)
		for phase, name := range [...]string{"initial_ping", "failed_startup_cleanup"} {
			observation := snapshot.startup[phase]
			metrics <- prometheus.MustNewConstMetric(self.startupActiveDesc, prometheus.GaugeValue, float64(observation.active), role, name)
			metrics <- prometheus.MustNewConstMetric(self.startupDurationDesc, prometheus.CounterValue, observation.durationSeconds, role, name)
			for outcome, result := range [...]string{"ok", "deadline", "canceled", "other"} {
				metrics <- prometheus.MustNewConstMetric(self.startupCompleteDesc, prometheus.CounterValue, float64(observation.completed[outcome]), role, name, result)
			}
		}
	}
}
