package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const (
	taskMetricsRefreshInterval = 15 * time.Second
	taskMetricsQueryTimeout    = 5 * time.Second
	// Ends each refresh statement on the server inside the owner's client
	// budget, so a refresh never outlives the interval that admitted it.
	taskMetricsStatementTimeout = 3 * time.Second
	// Rows counted exactly on each side of the availability boundary.
	taskMetricsBoundRows   = 10000
	taskMetricsMaxInterval = time.Minute
	// Names the transaction-scoped advisory key a refresh holds until the
	// server has finished its statements.
	taskMetricsAdvisoryLockName = "urnetwork:task_queue_metrics"
)

var taskExecutionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "executions_total",
	Help:      "Task function executions by finite registered task, caller attribution, and bounded terminal outcome.",
}, []string{"task", "attribution", "outcome"})

var taskExecutionSeconds = newTaskExecutionSeconds()

// newTaskExecutionSeconds retains exact sum/count observations without a
// bucket multiplier for every registered task and attribution pair.
func newTaskExecutionSeconds() *prometheus.SummaryVec {
	return prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace:  "urnetwork",
		Subsystem:  "taskworker",
		Name:       "execution_duration_seconds",
		Help:       "Task function execution duration by finite registered task and caller attribution.",
		Objectives: nil,
	}, []string{"task", "attribution"})
}

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
	Help:      "Acknowledged task finalization outcomes: succeeded, rescheduled, or post_rescheduled. Excludes failed or unacknowledged finalization attempts and external post errors.",
}, []string{"outcome"})

var taskQueueSnapshotErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "taskworker",
	Name:      "queue_snapshot_errors_total",
	Help:      "Pending-task queue snapshot reads, queries, or publications that failed their bounded refresh.",
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
	estimatedDesc     *prometheus.Desc
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
			fmt.Sprintf("Pending task rows by overlapping state at the latest successful queue snapshot. Exact up to %d rows on each side of the availability boundary; past that, a planner estimate (see queue_depth_estimated).", taskMetricsBoundRows),
			[]string{"state"}, nil,
		),
		estimatedDesc: prometheus.NewDesc(
			"urnetwork_taskworker_queue_depth_estimated",
			fmt.Sprintf("1 when the queue_depth state at the latest successful snapshot is a planner estimate, or for claimed a lower bound, because its side of the availability boundary held more than %d rows; 0 when exact.", taskMetricsBoundRows),
			[]string{"state"}, nil,
		),
		oldestOverdueDesc: prometheus.NewDesc(
			"urnetwork_taskworker_queue_oldest_overdue_seconds",
			fmt.Sprintf("Age past run_at of the oldest available pending task at the latest successful queue snapshot, read from the first %d available rows in poll order.", taskMetricsBoundRows),
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
		taskExecutionErrorsTotal,
		taskExecutionSeconds,
		taskExecutionInflight,
		taskExecutionBytesTotal,
		taskPollsTotal,
		taskFinalizationsTotal,
		taskFinalizationErrorsTotal,
		taskFinalizationPhaseErrorsTotal,
		taskFinalizationAdmissionErrorsTotal,
		taskFinalizationFailureSeconds,
		taskQueueMetrics,
		taskQueueSnapshotErrorsTotal,
		taskExecutionMaximum,
	)
}

// Describe implements prometheus.Collector.
func (self *taskQueueMetricsCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.depthDesc
	descriptions <- self.estimatedDesc
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
		name      string
		value     int64
		estimated bool
	}{
		{name: "total", value: sample.snapshot.Total, estimated: sample.snapshot.TotalEstimated},
		{name: "available", value: sample.snapshot.Available, estimated: sample.snapshot.AvailableEstimated},
		{name: "claimed", value: sample.snapshot.Claimed, estimated: sample.snapshot.ClaimedEstimated},
		{name: "reschedule_error", value: sample.snapshot.RescheduleError, estimated: sample.snapshot.RescheduleErrorEstimated},
	} {
		metrics <- prometheus.MustNewConstMetric(self.depthDesc, prometheus.GaugeValue, float64(state.value), state.name)
		estimated := 0.0
		if state.estimated {
			estimated = 1
		}
		metrics <- prometheus.MustNewConstMetric(self.estimatedDesc, prometheus.GaugeValue, estimated, state.name)
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
	causes := inspectTaskRetryCauses(err)
	if causes.complete && causes.drained {
		return "drained"
	}
	if causes.complete && causes.targetMissing {
		return "target_not_found"
	}
	return "failed"
}

// recordTaskExecution records one already-completed execution.
func recordTaskExecution(taskName string, attribution string, argsBytes int, resultBytes int, duration time.Duration, err error) {
	outcome := taskMetricOutcome(err)
	taskExecutionsTotal.WithLabelValues(taskName, attribution, outcome).Inc()
	if err != nil {
		taskExecutionErrorsTotal.WithLabelValues(taskName, attribution, taskExecutionErrorCause(err)).Inc()
	}
	taskExecutionSeconds.WithLabelValues(taskName, attribution).Observe(duration.Seconds())
	taskExecutionBytesTotal.WithLabelValues(taskName, "args").Add(float64(argsBytes))
	if 0 < resultBytes {
		taskExecutionBytesTotal.WithLabelValues(taskName, "result").Add(float64(resultBytes))
	}
	taskExecutionMaximum.observe(taskName, attribution, duration)
}

// taskQueueMetricsSnapshot is one identity-free database aggregate. States
// overlap and must not be summed. An estimated state is a planner estimate,
// or for claimed a lower bound (see taskQueueMetricsBound.snapshot).
type taskQueueMetricsSnapshot struct {
	Total               int64
	Available           int64
	Claimed             int64
	RescheduleError     int64
	OldestOverdueSecond float64

	TotalEstimated           bool
	AvailableEstimated       bool
	ClaimedEstimated         bool
	RescheduleErrorEstimated bool
}

// Another refresh still holds the database key, so this one did no work.
var errTaskQueueMetricsBusy = errors.New("task queue metrics snapshot already running")

// Planner predicates for a saturated side. $1 is the availability block.
const (
	taskQueueMetricsDuePredicate     = `available_block <= $1`
	taskQueueMetricsWaitingPredicate = `$1 < available_block`
	taskQueueMetricsErrorPredicate   = ` AND has_reschedule_error`
)

// Production refresh bounds. The caller's context bounds the client wait.
func loadTaskQueueMetricsSnapshot(ctx context.Context, now time.Time) (taskQueueMetricsSnapshot, error) {
	return taskQueueMetricsQuery{
		statementTimeout: taskMetricsStatementTimeout,
		boundRows:        taskMetricsBoundRows,
	}.load(ctx, now)
}

// One refresh reads at most boundRows+1 rows on each side of the availability
// boundary, in poll order through pending_task_poll_order, instead of the
// whole table. Due rows (available_block <= now) give available, their
// reschedule errors and the oldest overdue run_at. Waiting rows give the rest
// of the total and of the reschedule errors, and every claim: a claim's
// release_time puts its available_block past now.
//
// The work is bounded by the server itself. A client that stops waiting does
// not stop PostgreSQL, since its cancel request does not reliably cross the
// transaction pooler, so every statement runs under a transaction-scoped
// statement_timeout, without parallel workers or JIT. The transaction-scoped
// advisory key stays held until the server finishes, even after its client
// has gone, so a later owner skips instead of starting a second scan beside
// it. Transaction-scoped settings and keys are safe through the pooler.
type taskQueueMetricsQuery struct {
	statementTimeout time.Duration
	boundRows        int64
}

// The caller's context bounds only the client wait. The server ends every
// statement at the statement timeout whether or not the client still waits.
func (self taskQueueMetricsQuery) load(ctx context.Context, now time.Time) (snapshot taskQueueMetricsSnapshot, returnErr error) {
	availableBlock := now.Unix() / BlockSizeSeconds
	if recovered := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`/* taskworker-queue-metrics */
				 SELECT set_config('statement_timeout', $1, true),
				        set_config('max_parallel_workers_per_gather', '0', true),
				        set_config('jit', 'off', true)`,
				strconv.FormatInt(self.statementTimeout.Milliseconds(), 10),
			))
			admitted := false
			server.Raise(tx.QueryRow(
				ctx,
				`/* taskworker-queue-metrics */
				 SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
				taskMetricsAdvisoryLockName,
			).Scan(&admitted))
			if !admitted {
				returnErr = errTaskQueueMetricsBusy
				return
			}
			bound := taskQueueMetricsBound{}
			server.Raise(tx.QueryRow(ctx, self.boundedSql(), now, availableBlock).Scan(
				&bound.dueRows,
				&bound.dueErrors,
				&bound.oldestOverdueSecond,
				&bound.waitingRows,
				&bound.waitingClaimed,
				&bound.waitingErrors,
			))
			snapshot = bound.snapshot(self.boundRows, func(predicate string) int64 {
				var planJson []byte
				server.Raise(tx.QueryRow(
					ctx,
					`/* taskworker-queue-metrics */
					 EXPLAIN (FORMAT JSON) SELECT 1 FROM pending_task WHERE `+predicate,
					availableBlock,
				).Scan(&planJson))
				plans := []struct {
					Plan struct {
						Rows *float64 `json:"Plan Rows"`
					} `json:"Plan"`
				}{}
				if err := json.Unmarshal(planJson, &plans); err != nil || len(plans) != 1 || plans[0].Plan.Rows == nil ||
					!(0 <= *plans[0].Plan.Rows && *plans[0].Plan.Rows < 1e15) {
					server.Raise(errors.New("task queue estimate has no plan row count"))
				}
				return int64(math.Round(*plans[0].Plan.Rows))
			})
		}, server.TxReadCommitted, pgx.ReadOnly, server.OptNoRetry())
	}); recovered != nil {
		if err, ok := recovered.(error); ok {
			return taskQueueMetricsSnapshot{}, err
		}
		return taskQueueMetricsSnapshot{}, errors.New("task queue snapshot failed")
	}
	if returnErr != nil {
		return taskQueueMetricsSnapshot{}, returnErr
	}
	return snapshot, nil
}

// Each side stops at boundRows+1 rows, which marks it saturated. The limit is
// a literal so a cached generic plan still knows how few rows it needs.
func (self taskQueueMetricsQuery) boundedSql() string {
	limit := strconv.FormatInt(self.boundRows+1, 10)
	return `/* taskworker-queue-metrics */
		SELECT due.row_count, due.error_count, due.oldest_overdue_seconds,
		       waiting.row_count, waiting.claimed_count, waiting.error_count
		FROM (
			SELECT count(*)::bigint AS row_count,
			       count(*) FILTER (WHERE has_reschedule_error)::bigint AS error_count,
			       COALESCE(EXTRACT(EPOCH FROM ($1 - min(run_at))), 0)::double precision AS oldest_overdue_seconds
			FROM (
				SELECT run_at, has_reschedule_error
				FROM pending_task
				WHERE available_block <= $2
				ORDER BY available_block
				LIMIT ` + limit + `
			) AS head
		) AS due, (
			SELECT count(*)::bigint AS row_count,
			       count(*) FILTER (WHERE $1 < release_time)::bigint AS claimed_count,
			       count(*) FILTER (WHERE has_reschedule_error)::bigint AS error_count
			FROM (
				SELECT release_time, has_reschedule_error
				FROM pending_task
				WHERE $2 < available_block
				ORDER BY available_block
				LIMIT ` + limit + `
			) AS head
		) AS waiting`
}

// Rows read by one bounded refresh, before any estimate.
type taskQueueMetricsBound struct {
	dueRows             int64
	dueErrors           int64
	oldestOverdueSecond float64
	waitingRows         int64
	waitingClaimed      int64
	waitingErrors       int64
}

// A side holding more than boundRows rows reports planner estimates, which are
// requested only then and never lower what the bounded read proved. Every
// claim is a waiting row, so claimed is exact while the waiting side fits the
// bound and a lower bound otherwise. The oldest overdue age always comes from
// the earliest-available rows read, which cover every due row unless the due
// side is saturated.
func (self taskQueueMetricsBound) snapshot(boundRows int64, estimate func(predicate string) int64) taskQueueMetricsSnapshot {
	dueRows, dueErrors := self.dueRows, self.dueErrors
	dueEstimated := boundRows < dueRows
	if dueEstimated {
		dueRows = max(dueRows, estimate(taskQueueMetricsDuePredicate))
		dueErrors = min(dueRows, max(dueErrors, estimate(taskQueueMetricsDuePredicate+taskQueueMetricsErrorPredicate)))
	}
	waitingRows, waitingErrors := self.waitingRows, self.waitingErrors
	waitingEstimated := boundRows < waitingRows
	if waitingEstimated {
		waitingRows = max(waitingRows, estimate(taskQueueMetricsWaitingPredicate))
		waitingErrors = min(waitingRows, max(waitingErrors, estimate(taskQueueMetricsWaitingPredicate+taskQueueMetricsErrorPredicate)))
	}
	return taskQueueMetricsSnapshot{
		Total:                    dueRows + waitingRows,
		Available:                dueRows,
		Claimed:                  self.waitingClaimed,
		RescheduleError:          dueErrors + waitingErrors,
		OldestOverdueSecond:      max(0, self.oldestOverdueSecond),
		TotalEstimated:           dueEstimated || waitingEstimated,
		AvailableEstimated:       dueEstimated,
		ClaimedEstimated:         waitingEstimated,
		RescheduleErrorEstimated: dueEstimated || waitingEstimated,
	}
}

// Shares one database observation per interval across taskworkers. The initial
// read is asynchronous so this optional observer cannot gate worker readiness.
// Failed refreshes retain the previous values and their original source time.
func StartQueueMetrics(ctx context.Context) {
	cache := taskQueueMetricsCache{
		store:    redisTaskQueueMetricsStore{},
		now:      server.NowUtc,
		newToken: func() string { return server.NewId().String() },
		load:     loadTaskQueueMetricsSnapshot,
	}
	startTaskQueueMetrics(ctx, cache.get, taskQueueMetrics, func(err error) {
		taskQueueSnapshotErrorsTotal.Inc()
		glog.Infof("[taskworker]queue metrics refresh failed: %v\n", err)
	})
}

// Keeps storage reads outside the caller's startup path and serializes refreshes.
// Returning completion lets lifecycle tests join a canceled observer.
func startTaskQueueMetrics(
	ctx context.Context,
	load func(context.Context) (*taskQueueMetricsSample, error),
	collector *taskQueueMetricsCollector,
	onError func(error),
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			sample, err := load(ctx)
			if err != nil {
				onError(err)
			}
			if sample != nil {
				collector.publish(sample.snapshot, sample.observedAt)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(taskMetricsRefreshInterval):
			}
		}
	}()
	return done
}
