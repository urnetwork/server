package task

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

const (
	taskMetricsRefreshInterval = 15 * time.Second
	taskMetricsQueryTimeout    = 5 * time.Second
	taskMetricsMaxInterval     = time.Minute
)

var taskExecutionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "executions_total",
	Help:      "Task function executions by finite registered task, caller attribution, and bounded terminal outcome.",
}, []string{"task", "attribution", "outcome"})

var taskExecutionSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "execution_duration_seconds",
	Help:      "Task function execution duration by finite registered task and caller attribution.",
	Buckets:   []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 5, 15, 30, 60, 120, 300, 900, 3600},
}, []string{"task", "attribution"})

var taskExecutionInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "execution_inflight",
	Help:      "Task functions currently executing by finite registered task and caller attribution.",
}, []string{"task", "attribution"})

var taskExecutionBytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "execution_bytes_total",
	Help:      "Serialized task argument and already-produced result bytes by finite registered task and direction.",
}, []string{"task", "direction"})

var taskPollsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "polls_total",
	Help:      "Taskworker EvalTasks polls by bounded result: claimed, empty, or error.",
}, []string{"outcome"})

var taskFinalizationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "finalizations_total",
	Help:      "Task finalization outcomes after function execution, including post-hook reschedules.",
}, []string{"outcome"})

var taskQueueSnapshotErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "queue_snapshot_errors_total",
	Help:      "Pending-task queue snapshots that failed or exceeded their bounded query context.",
})

// taskQueueMetricsSample keeps every queue value with the freshness timestamp
// that owns it, so a concurrent scrape cannot combine generations.
type taskQueueMetricsSample struct {
	snapshot   taskQueueMetricsSnapshot
	observedAt time.Time
}

// taskQueueMetricsCollector publishes a complete immutable queue snapshot.
// Before the first successful database query it emits no business series.
type taskQueueMetricsCollector struct {
	depthDesc         *prometheus.Desc
	oldestOverdueDesc *prometheus.Desc
	timestampDesc     *prometheus.Desc
	stateLock         sync.Mutex
	sample            *taskQueueMetricsSample
}

// newTaskQueueMetricsCollector constructs an empty coherent collector.
func newTaskQueueMetricsCollector() *taskQueueMetricsCollector {
	return &taskQueueMetricsCollector{
		depthDesc: prometheus.NewDesc(
			"urnetwork_taskworker_queue_depth",
			"Pending task rows by bounded overlapping state at the latest successful queue snapshot.",
			[]string{"state"}, nil,
		),
		oldestOverdueDesc: prometheus.NewDesc(
			"urnetwork_taskworker_queue_oldest_overdue_seconds",
			"Age past run_at of the oldest available pending task at the latest successful queue snapshot.",
			nil, nil,
		),
		timestampDesc: prometheus.NewDesc(
			"urnetwork_taskworker_queue_snapshot_timestamp_seconds",
			"Unix time of the latest successful bounded pending-task queue snapshot.",
			nil, nil,
		),
	}
}

var taskQueueMetrics = newTaskQueueMetricsCollector()

// taskExecutionMaxSample is one task/attribution maximum in a wall-clock
// minute and carries the exact observation time used to freshness-gate it.
type taskExecutionMaxSample struct {
	bucket     int64
	seconds    float64
	observedAt time.Time
}

// taskExecutionMaxCollector publishes coherent max/timestamp pairs without
// retaining arguments, results, task ids, or errors.
type taskExecutionMaxCollector struct {
	maximumDesc   *prometheus.Desc
	timestampDesc *prometheus.Desc
	stateLock     sync.Mutex
	samples       map[string]taskExecutionMaxSample
	now           func() time.Time
}

func newTaskExecutionMaxCollector() *taskExecutionMaxCollector {
	return &taskExecutionMaxCollector{
		maximumDesc: prometheus.NewDesc(
			"urnetwork_taskworker_execution_interval_max_seconds",
			"Maximum task execution duration in the latest one-minute interval that completed for this task and attribution.",
			[]string{"task", "attribution"}, nil,
		),
		timestampDesc: prometheus.NewDesc(
			"urnetwork_taskworker_execution_interval_max_timestamp_seconds",
			"Unix time of the task observation backing the latest one-minute execution maximum.",
			[]string{"task", "attribution"}, nil,
		),
		samples: map[string]taskExecutionMaxSample{},
		now:     time.Now,
	}
}

var taskExecutionMaximum = newTaskExecutionMaxCollector()

func init() {
	prometheus.MustRegister(
		taskExecutionsTotal,
		taskExecutionSeconds,
		taskExecutionInflight,
		taskExecutionBytesTotal,
		taskPollsTotal,
		taskFinalizationsTotal,
		taskQueueMetrics,
		taskQueueSnapshotErrorsTotal,
		taskExecutionMaximum,
	)
}

// Describe implements prometheus.Collector.
func (self *taskQueueMetricsCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.depthDesc
	descriptions <- self.oldestOverdueDesc
	descriptions <- self.timestampDesc
}

// Collect implements prometheus.Collector. Every emitted value and timestamp
// comes from one sample copied under stateLock.
func (self *taskQueueMetricsCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	var sample *taskQueueMetricsSample
	if self.sample != nil {
		copySample := *self.sample
		sample = &copySample
	}
	self.stateLock.Unlock()
	if sample == nil {
		return
	}
	for _, state := range []struct {
		name  string
		value int64
	}{
		{name: "total", value: sample.snapshot.Total},
		{name: "available", value: sample.snapshot.Available},
		{name: "claimed", value: sample.snapshot.Claimed},
		{name: "reschedule_error", value: sample.snapshot.RescheduleError},
	} {
		metrics <- prometheus.MustNewConstMetric(self.depthDesc, prometheus.GaugeValue, float64(state.value), state.name)
	}
	metrics <- prometheus.MustNewConstMetric(
		self.oldestOverdueDesc,
		prometheus.GaugeValue,
		sample.snapshot.OldestOverdueSecond,
	)
	metrics <- prometheus.MustNewConstMetric(
		self.timestampDesc,
		prometheus.GaugeValue,
		float64(sample.observedAt.UnixNano())/float64(time.Second),
	)
}

// publish replaces one complete queue generation.
func (self *taskQueueMetricsCollector) publish(snapshot taskQueueMetricsSnapshot, observedAt time.Time) {
	self.stateLock.Lock()
	self.sample = &taskQueueMetricsSample{snapshot: snapshot, observedAt: observedAt}
	self.stateLock.Unlock()
}

// Describe implements prometheus.Collector.
func (self *taskExecutionMaxCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.maximumDesc
	descriptions <- self.timestampDesc
}

// Collect implements prometheus.Collector and copies complete sample pairs.
func (self *taskExecutionMaxCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	samples := make(map[string]taskExecutionMaxSample, len(self.samples))
	for key, sample := range self.samples {
		samples[key] = sample
	}
	self.stateLock.Unlock()
	for key, sample := range samples {
		parts := strings.SplitN(key, "\x00", 2)
		metrics <- prometheus.MustNewConstMetric(self.maximumDesc, prometheus.GaugeValue, sample.seconds, parts[0], parts[1])
		metrics <- prometheus.MustNewConstMetric(
			self.timestampDesc,
			prometheus.GaugeValue,
			float64(sample.observedAt.UnixNano())/float64(time.Second),
			parts[0],
			parts[1],
		)
	}
}

// observe records one duration into the current wall-clock minute.
func (self *taskExecutionMaxCollector) observe(taskName string, attribution string, duration time.Duration) {
	now := self.now()
	bucket := now.UnixNano() / taskMetricsMaxInterval.Nanoseconds()
	key := taskName + "\x00" + attribution
	self.stateLock.Lock()
	current, ok := self.samples[key]
	if !ok || current.bucket != bucket || current.seconds < duration.Seconds() {
		self.samples[key] = taskExecutionMaxSample{bucket: bucket, seconds: duration.Seconds(), observedAt: now}
	}
	self.stateLock.Unlock()
}

// taskMetricName turns a registered Go target into a stable, finite dashboard
// label. It retains the package plus function and strips only module ancestry.
func taskMetricName(functionName string) string {
	name := path.Base(functionName)
	if name == "." || name == "/" || name == "" {
		return "unregistered"
	}
	return name
}

// taskMetricAttribution classifies whether durable task metadata names an
// authenticated caller without exporting or parsing that identity.
func taskMetricAttribution(task *Task) string {
	if task != nil && task.ClientByJwtJson != "" {
		return "client"
	}
	return "system"
}

// taskMetricOutcome maps execution errors to a finite operational taxonomy.
func taskMetricOutcome(err error) string {
	if err == nil {
		return "succeeded"
	}
	if errors.Is(err, ErrDrained) {
		return "drained"
	}
	if errors.Is(err, ErrTargetNotFound) {
		return "target_not_found"
	}
	return "failed"
}

// recordTaskExecution records one already-completed execution.
func recordTaskExecution(taskName string, attribution string, argsBytes int, resultBytes int, duration time.Duration, err error) {
	outcome := taskMetricOutcome(err)
	taskExecutionsTotal.WithLabelValues(taskName, attribution, outcome).Inc()
	taskExecutionSeconds.WithLabelValues(taskName, attribution).Observe(duration.Seconds())
	taskExecutionBytesTotal.WithLabelValues(taskName, "args").Add(float64(argsBytes))
	if 0 < resultBytes {
		taskExecutionBytesTotal.WithLabelValues(taskName, "result").Add(float64(resultBytes))
	}
	taskExecutionMaximum.observe(taskName, attribution, duration)
}

// taskQueueMetricsSnapshot is one identity-free database aggregate.
type taskQueueMetricsSnapshot struct {
	Total               int64
	Available           int64
	Claimed             int64
	RescheduleError     int64
	OldestOverdueSecond float64
}

// loadTaskQueueMetricsSnapshot executes one bounded aggregate over pending
// tasks. States intentionally overlap and therefore must not be summed.
func loadTaskQueueMetricsSnapshot(ctx context.Context, now time.Time) (taskQueueMetricsSnapshot, error) {
	snapshot := taskQueueMetricsSnapshot{}
	queryCtx, cancel := context.WithTimeout(ctx, taskMetricsQueryTimeout)
	defer cancel()
	returnErr := error(nil)
	if recovered := server.HandleError(func() {
		server.ReplicaDb(queryCtx, func(conn server.PgConn) {
			returnErr = conn.QueryRow(
				queryCtx,
				`/* taskworker-queue-metrics */
				 SELECT count(*)::bigint,
				        count(*) FILTER (WHERE available_block <= $2)::bigint,
				        count(*) FILTER (WHERE $1 < release_time)::bigint,
				        count(*) FILTER (WHERE has_reschedule_error)::bigint,
				        COALESCE(EXTRACT(EPOCH FROM ($1 - min(run_at) FILTER (WHERE available_block <= $2))), 0)::double precision
				 FROM pending_task`,
				now,
				now.Unix()/BlockSizeSeconds,
			).Scan(
				&snapshot.Total,
				&snapshot.Available,
				&snapshot.Claimed,
				&snapshot.RescheduleError,
				&snapshot.OldestOverdueSecond,
			)
		})
	}); recovered != nil {
		if err, ok := recovered.(error); ok {
			return snapshot, err
		}
		return snapshot, errors.New("task queue snapshot failed")
	}
	if returnErr != nil {
		return snapshot, returnErr
	}
	if snapshot.OldestOverdueSecond < 0 {
		snapshot.OldestOverdueSecond = 0
	}
	return snapshot, nil
}

// publishTaskQueueMetricsSnapshot atomically advances freshness only after
// every aggregate value was obtained successfully.
func publishTaskQueueMetricsSnapshot(snapshot taskQueueMetricsSnapshot, now time.Time) {
	taskQueueMetrics.publish(snapshot, now)
}

// StartQueueMetrics refreshes queue pressure until the taskworker stops. A
// failed query preserves the old values and timestamp so dashboards turn
// stale/no-data rather than displaying a fresh false zero.
func StartQueueMetrics(ctx context.Context) {
	refresh := func() {
		now := server.NowUtc()
		snapshot, err := loadTaskQueueMetricsSnapshot(ctx, now)
		if err != nil {
			taskQueueSnapshotErrorsTotal.Inc()
			glog.Infof("[taskworker]queue metrics refresh failed: %v\n", err)
			return
		}
		publishTaskQueueMetricsSnapshot(snapshot, now)
	}
	refresh()
	go server.HandleError(func() {
		ticker := time.NewTicker(taskMetricsRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	})
}
