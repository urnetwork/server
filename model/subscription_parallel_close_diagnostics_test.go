// Failure diagnostics observe the existing burst. They never release an owner,
// change a deadline, decide progress, or replace the full accounting verdict.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

type parallelPublicCloseQueryDiagnostic struct {
	Calls                  int64 `json:"calls"`
	Ended                  int64 `json:"ended_including_failure_unwind"`
	TotalNs                int64 `json:"total_ns"`
	MaxNs                  int64 `json:"max_ns"`
	LastStartedUtcUnixNs   int64 `json:"last_started_utc_unix_ns"`
	LastCompletedUtcUnixNs int64 `json:"last_completed_utc_unix_ns"`
	MaxStartedUtcUnixNs    int64 `json:"max_started_utc_unix_ns"`
	MaxCompletedUtcUnixNs  int64 `json:"max_completed_utc_unix_ns"`
}

// Snapshot never reads the invocation/return slices written by public callers.
// Logging copies protected state before calling testing.TB, including when a
// backend terminal callback runs concurrently with the test's observer query.
type parallelPublicCloseDiagnostics struct {
	t                    testing.TB
	started              time.Time
	stateLock            sync.Mutex
	eventKVs             map[string]map[string]int64
	queryKVs             map[string]parallelPublicCloseQueryDiagnostic
	invocations          atomic.Int64
	returns              atomic.Int64
	accepted             atomic.Int64
	hotNativeAccepted    atomic.Int64
	backendEndObserved   atomic.Bool
	hotOwnerAdmitted     atomic.Bool
	independentObserved  atomic.Bool
	taskSnapshotCaptured bool // Only the test goroutine uses the reserved observer.
}

func newParallelPublicCloseDiagnostics(t testing.TB) *parallelPublicCloseDiagnostics {
	return &parallelPublicCloseDiagnostics{t: t, started: time.Now(),
		eventKVs: map[string]map[string]int64{}, queryKVs: map[string]parallelPublicCloseQueryDiagnostic{}}
}

func (self *parallelPublicCloseDiagnostics) snapshot() map[string]any {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return map[string]any{"events": maps.Clone(self.eventKVs), "observer_queries": maps.Clone(self.queryKVs),
		"invocations_entered": self.invocations.Load(), "public_calls_returned": self.returns.Load(),
		"accepted_acknowledgements": self.accepted.Load(), "accepted_hot_native_acknowledgements": self.hotNativeAccepted.Load(),
		"backend_end_callback_observed": self.backendEndObserved.Load()}
}

func (self *parallelPublicCloseDiagnostics) log(name string, extra map[string]any) {
	record := self.snapshot()
	record["name"] = name
	record["observed_utc_unix_ns"] = time.Now().UTC().UnixNano()
	record["diagnostic_elapsed_ns"] = time.Since(self.started).Nanoseconds()
	for key, value := range extra {
		record[key] = value
	}
	raw, err := json.Marshal(record)
	if err != nil {
		// Diagnostics must not replace the actual financial/witness failure.
		self.t.Logf("parallel_public_close_diagnostic_encode_error=%v", err)
		return
	}
	self.t.Logf("parallel_public_close_diagnostic=%s", raw)
}

func (self *parallelPublicCloseDiagnostics) event(name string, pipelineElapsedNs int64) {
	observed := time.Now()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.eventKVs[name] = map[string]int64{"utc_unix_ns": observed.UTC().UnixNano(),
			"diagnostic_elapsed_ns": observed.Sub(self.started).Nanoseconds(), "pipeline_elapsed_ns": pipelineElapsedNs}
	}()
	self.log(name, nil)
}

func (self *parallelPublicCloseDiagnostics) backendEnd() {
	self.backendEndObserved.Store(true)
	self.event("native_backend_terminal_or_loss_callback", -1)
}

func (self *parallelPublicCloseDiagnostics) acknowledge(hotNative bool) {
	accepted := self.accepted.Add(1)
	if accepted == 1 || accepted == parallelPublicCloseCount/2 || accepted == parallelPublicCloseCount {
		self.event(fmt.Sprintf("accepted_acknowledgements_%d", accepted), -1)
	}
	if hotNative && self.hotNativeAccepted.Add(1) == 1 {
		self.event("first_hot_native_acknowledgement", -1)
	}
}

// A diagnostic gather never calls Fatal or changes the primary failure. Keep
// counter deltas separate from gauges and disclose a missing/reset lifetime.
func (self *parallelPublicCloseDiagnostics) counters(before map[string]float64) map[string]any {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return map[string]any{"gather_error_type": fmt.Sprintf("%T", err)}
	}
	after, gauges := map[string]float64{}, map[string]float64{}
	for _, family := range families {
		name := family.GetName()
		if name != "urnetwork_db_rerun_decisions_total" && name != "urnetwork_db_aborted_transactions_total" &&
			!strings.HasPrefix(name, "urnetwork_taskworker_") && name != "urnetwork_transfer_debit_flush_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := make([]string, 0, len(metric.Label))
			for _, label := range metric.Label {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			slices.Sort(labels)
			key := name + "{" + strings.Join(labels, ",") + "}"
			if metric.Counter != nil {
				after[key] = metric.Counter.GetValue()
			} else if metric.Gauge != nil {
				gauges[key] = metric.Gauge.GetValue()
			}
		}
	}
	lifetimeValid := true
	delta, additional := map[string]float64{}, map[string]float64{}
	for key, value := range before {
		current, present := after[key]
		lifetimeValid = lifetimeValid && present && current >= value
	}
	for key, value := range after {
		name, _, _ := strings.Cut(key, "{")
		switch name {
		case "urnetwork_db_rerun_decisions_total", "urnetwork_db_aborted_transactions_total",
			"urnetwork_taskworker_polls_total", "urnetwork_taskworker_finalizations_total",
			"urnetwork_taskworker_executions_total", "urnetwork_transfer_debit_flush_total":
			delta[key] = value - before[key]
		default:
			additional[key] = value
		}
	}
	return map[string]any{"counter_deltas_from_fixture_baseline": delta, "current_gauges": gauges,
		"additional_counter_totals_without_baseline": additional,
		"baseline_counter_lifetime_valid":            lifetimeValid,
		"scope":                                      "new labels in baseline families start at zero; other counters are absolute; initial claimed poll is counted after runTaskSlots returns; gauges/function inflight do not prove slot retirement"}
}

// Every original query is timed in place. Only the first start/completion of
// each shape is logged eagerly; later calls contribute exact totals/maxima.
func (self *parallelPublicCloseDiagnostics) query(name string) func() {
	started := time.Now()
	first := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		value := self.queryKVs[name]
		value.Calls++
		value.LastStartedUtcUnixNs = started.UTC().UnixNano()
		first = value.Calls == 1
		self.queryKVs[name] = value
	}()
	if first {
		self.event(name+"_first_started", -1)
	}
	return func() {
		ended := time.Now()
		elapsed := ended.Sub(started).Nanoseconds()
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			value := self.queryKVs[name]
			value.Ended++
			value.TotalNs += elapsed
			value.LastCompletedUtcUnixNs = ended.UTC().UnixNano()
			if value.MaxNs < elapsed {
				value.MaxNs = elapsed
				value.MaxStartedUtcUnixNs = started.UTC().UnixNano()
				value.MaxCompletedUtcUnixNs = ended.UTC().UnixNano()
			}
			self.queryKVs[name] = value
		}()
		if first {
			self.event(name+"_first_completed", -1)
		}
	}
}

// One read-only statement, at most once, on the already-reserved observer after
// loss/failure. It cannot consume the healthy held interval. The one-second
// diagnostic context neither extends a production owner nor changes cleanup.
// Stored eligibility/lease times are observations, not proof of live ownership
// or a reconstructed poll history. No task/contract/payer identities are logged.
func (self *parallelPublicCloseDiagnostics) taskSnapshot(ctx context.Context, observer server.PgConn, roles, functions []string) {
	if self.taskSnapshotCaptured {
		return
	}
	self.taskSnapshotCaptured = true
	observed := server.NowUtc()
	cutoff := observed.Unix() / task.BlockSizeSeconds
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	started := time.Now()
	var rows []byte
	var queryErr error
	server.HandleError(func() {
		queryErr = observer.QueryRow(bounded, `WITH names AS (
 SELECT * FROM unnest($1::text[],$2::text[]) AS input(role,function_name))
SELECT jsonb_agg(jsonb_build_object('role',names.role,
 'pending',pending.observation,'finished',finished.observation,
 'first_pending_by_live_claim_order',pending_rows.observation,
 'recent_finished_intervals',finished_rows.observation) ORDER BY names.role)
FROM names
CROSS JOIN LATERAL (SELECT jsonb_build_object(
 'count',count(*),'available_at_observed_cutoff',count(*) FILTER(WHERE available_block<=$3),
 'future_available',count(*) FILTER(WHERE available_block>$3),
 'future_release_time',count(*) FILTER(WHERE release_time>$4),
 'reschedule_error_rows',count(*) FILTER(WHERE reschedule_error IS NOT NULL),
 'min_available_block',min(available_block),'max_available_block',max(available_block),
 'min_run_at',min(run_at),'max_run_at',max(run_at),
 'min_claim_time',min(claim_time),'max_claim_time',max(claim_time),
 'min_release_time',min(release_time),'max_release_time',max(release_time)) AS observation
 FROM pending_task WHERE function_name=names.function_name) AS pending
CROSS JOIN LATERAL (SELECT jsonb_build_object(
 'count',count(*),'post_completed',count(*) FILTER(WHERE post_completed),
 'post_error_rows',count(*) FILTER(WHERE post_error IS NOT NULL),
 'min_run_at',min(run_at),'max_run_at',max(run_at),
 'min_run_start_time',min(run_start_time),'max_run_start_time',max(run_start_time),
 'min_run_end_time',min(run_end_time),'max_run_end_time',max(run_end_time)) AS observation
 FROM finished_task WHERE function_name=names.function_name) AS finished
CROSS JOIN LATERAL (SELECT COALESCE(jsonb_agg(to_jsonb(sample)),'[]'::jsonb) AS observation
 FROM (SELECT available_block,run_at,claim_time,release_time,run_priority,run_max_time_seconds,
    reschedule_error IS NOT NULL AS has_reschedule_error
  FROM pending_task WHERE function_name=names.function_name
  ORDER BY available_block,run_priority DESC,run_max_time_seconds DESC,task_id LIMIT 16) AS sample) AS pending_rows
CROSS JOIN LATERAL (SELECT COALESCE(jsonb_agg(to_jsonb(sample)),'[]'::jsonb) AS observation
 FROM (SELECT run_at,run_start_time,run_end_time,post_completed,
    post_error IS NOT NULL AS has_post_error,run_priority,run_max_time_seconds
  FROM finished_task WHERE function_name=names.function_name
  ORDER BY run_end_time DESC,task_id LIMIT 16) AS sample) AS finished_rows`,
			roles, functions, cutoff, observed).Scan(&rows)
	}, func(err error) { queryErr = err })
	extra := map[string]any{"snapshot_started_utc_unix_ns": started.UTC().UnixNano(),
		"snapshot_elapsed_ns": time.Since(started).Nanoseconds(), "snapshot_cutoff_block": cutoff,
		"snapshot_clock_utc_unix_ns": observed.UnixNano(), "row_samples_per_function_limit": 16,
		"query_error_present": queryErr != nil, "query_context_done": bounded.Err() != nil,
		"scope": "single failure/loss snapshot; cutoff is observation time, lease timestamps are not a live-owner proof; no exact poll-count inference"}
	if queryErr != nil {
		extra["query_error_type"] = fmt.Sprintf("%T", queryErr)
	} else {
		extra["task_functions"] = json.RawMessage(rows)
	}
	self.log("bounded_task_failure_snapshot", extra)
}
