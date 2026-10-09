// A separate healthy profile applies finite protocol jitter to both database
// routes. It retains the mixed fixture's real dispatcher and task targets.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// All 2,048 persisted closes are pending before 32 typed owners run. No grant
// is deliberately held; modeled network delay is not a correctness clock.
func TestContractCloseOwnerHealthy2048JitterHasNoObservedContention(t *testing.T) {
	runContractCloseOwnerMixed2048(t, true)
}

// Only fixed counters cross concurrent task/transaction callbacks. Main-test
// result fields are written after the one joined EvalTasks call has returned.
type contractCloseOwnerJitterObservation struct {
	protocol                    *legacyFinancialRunProtocol
	databaseWait                *legacyCloseDBWaitObserver
	reruns                      parallelCloseRetryObservation
	ownership                   [5]atomic.Int64
	unknownEvents               atomic.Int64
	beforeCounters              map[string]float64
	beforeClosed                server.TxCommitCounterSnapshot
	started                     time.Time
	pendingContracts            int
	workerCalls                 int
	finished                    int
	taskRetries                 int
	postRetries                 int
	taskCallFailed              bool
	accountingAndReplayVerified bool
	results                     []LegacyPayerSettlementResult
	stateLock                   sync.Mutex
	cohortTimings               []legacyFinancialCohortObservation
	financialDiagnostics        []legacyFinancialDiagnosticObservation
	droppedFinancialDiagnostics int
	nativeDiagnosticSetup       bool
}

// Preserve both original resource configurations and pool limits. The existing
// dual-route binder checks the same disposable database and joins its proxies.
func newContractCloseOwnerJitterObservation(t testing.TB, ctx context.Context) (context.Context, *contractCloseOwnerJitterObservation, func()) {
	t.Helper()
	observation := &contractCloseOwnerJitterObservation{}
	ctx = context.WithValue(ctx, legacyFinancialCohortObservationKey{}, func(value legacyFinancialCohortObservation) {
		observation.stateLock.Lock()
		observation.cohortTimings = append(observation.cohortTimings, value)
		observation.stateLock.Unlock()
	})
	ctx = context.WithValue(ctx, legacyFinancialDiagnosticKey{}, func(value legacyFinancialDiagnosticObservation) {
		observation.stateLock.Lock()
		if len(observation.financialDiagnostics) < 4096 {
			observation.financialDiagnostics = append(observation.financialDiagnostics, value)
		} else {
			observation.droppedFinancialDiagnostics++
		}
		observation.stateLock.Unlock()
	})
	ctx = observation.reruns.context(ctx)
	ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
		switch event.Kind {
		case server.PgOwnershipWaiting, server.PgOwnershipAdmitted, server.PgOwnershipReleased,
			server.PgOwnershipRefused, server.PgOwnershipUncertain:
			observation.ownership[event.Kind].Add(1)
		default:
			observation.unknownEvents.Add(1)
		}
	})
	observation.nativeDiagnosticSetup = awaitLegacyOwnerDiagnosticNativeSetup(t, ctx)
	observation.databaseWait = newLegacyCloseDBWaitObserver(t, ctx)
	ready := false
	var cleanupProtocol func()
	cleanup := func() {
		observation.databaseWait.close()
		if cleanupProtocol != nil {
			cleanupProtocol()
		}
	}
	defer func() {
		if !ready {
			cleanup()
		}
	}()
	protocol, releaseProtocol := legacyFinancialRunProtocolBind(t, ctx)
	cleanupProtocol = releaseProtocol
	for _, proxy := range protocol.proxies {
		proxy.enableDiagnosticCapture()
	}
	if observation.nativeDiagnosticSetup {
		requireLegacyOwnerDiagnosticLogging(t, ctx)
	}
	observation.protocol = protocol
	observation.beforeCounters = parallelCloseCounterSnapshot(t)
	observation.beforeClosed = contractClosedCounter.Snapshot()
	observation.databaseWait.start(t)
	for _, proxy := range protocol.proxies {
		proxy.jitter.Store(true)
	}
	observation.started = time.Now()
	protocol.enabled(true)
	ready = true
	return ctx, observation, cleanup
}

// Waiting records actual failed session admission, rather than entering the
// API. Refused records a failed transaction-scoped probe; both must stay zero.
func (self *contractCloseOwnerJitterObservation) requireHealthy(t testing.TB) {
	t.Helper()
	self.databaseWait.close()
	if err := self.databaseWait.snapshot().zeroWaitError(); err != nil {
		t.Fatal("healthy jitter profile failed direct PostgreSQL lock-wait observation", err)
	}
	if self.pendingContracts != 2048 || self.workerCalls != 1 || self.finished != 32 || len(self.results) != 32 ||
		self.taskRetries != 0 || self.postRetries != 0 || self.taskCallFailed || !self.accountingAndReplayVerified {
		t.Fatal("healthy jitter profile did not finish its exact close-task batch", self.finished, len(self.results), self.taskRetries, self.postRetries, self.taskCallFailed)
	}
	if self.reruns.callbacks.Load() != 0 || self.unknownEvents.Load() != 0 ||
		self.ownership[server.PgOwnershipAdmitted].Load() == 0 ||
		self.ownership[server.PgOwnershipWaiting].Load() != 0 ||
		self.ownership[server.PgOwnershipRefused].Load() != 0 ||
		self.ownership[server.PgOwnershipUncertain].Load() != 0 {
		t.Fatal("healthy jitter profile observed a database retry or ownership failure", self.ownershipCounts(), self.reruns.callbacks.Load())
	}
	afterClosed := contractClosedCounter.Snapshot()
	if !self.beforeClosed.Stable || !afterClosed.Stable || self.beforeClosed.Overflow || afterClosed.Overflow ||
		afterClosed.Confirmed-self.beforeClosed.Confirmed != 2048 ||
		afterClosed.Uncertain != self.beforeClosed.Uncertain || afterClosed.Untracked != self.beforeClosed.Untracked {
		t.Fatal("healthy jitter profile lost acknowledged close ownership", self.beforeClosed, afterClosed)
	}
	for name, value := range parallelCloseCounterDelta(t, self.beforeCounters, parallelCloseCounterSnapshot(t)) {
		if value != 0 && (strings.HasPrefix(name, "urnetwork_db_rerun_decisions_total{") || strings.HasPrefix(name, "urnetwork_db_aborted_transactions_total{")) {
			t.Fatal("healthy jitter profile recorded a retry decision or aborted callback", name, value)
		}
	}
	protocol := self.protocol.snapshot()
	for _, route := range []string{server.DefaultPgVaultResourceName, server.MaintenancePgVaultResourceName} {
		counters := protocol[route]
		if counters["ready_replies_observed"] == 0 || counters["ready_replies_charged_delay"] != counters["ready_replies_observed"] {
			t.Fatal("healthy jitter profile did not observe and delay its database route", route, counters)
		}
		for _, bucket := range []string{"1ms", "2ms", "3ms", "4ms", "5ms"} {
			charged, applied := counters["ready_jitter_charged_"+bucket], counters["ready_jitter_applied_"+bucket]
			if applied == 0 || charged != applied {
				t.Fatal("healthy jitter bucket was absent or canceled", route, bucket, charged, applied)
			}
		}
		for name, value := range counters {
			if strings.HasPrefix(name, "error_response_") && value != 0 {
				t.Fatal("healthy jitter profile observed a PostgreSQL error response", route, name, value)
			}
		}
		if counters["held_financial_set_replies"] != 0 || counters["connections_closed_after_begin_without_end_command"] != 0 {
			t.Fatal("healthy jitter profile held a reply or lost an active transaction", route, counters)
		}
		if counters["transactions_with_writes_committed"] == 0 {
			t.Fatal("healthy jitter profile did not observe completed write transactions", route, counters)
		}
		if counters["transactions_with_writes_rolled_back"] != 0 || counters["connections_closed_with_active_writes"] != 0 {
			t.Fatal("healthy jitter profile repeated or lost a transaction that executed writes", route, counters)
		}
	}
}

// Event names remain stable evidence fields without exposing opaque keys.
func (self *contractCloseOwnerJitterObservation) ownershipCounts() map[string]int64 {
	return map[string]int64{
		"waiting":        self.ownership[server.PgOwnershipWaiting].Load(),
		"admitted":       self.ownership[server.PgOwnershipAdmitted].Load(),
		"released":       self.ownership[server.PgOwnershipReleased].Load(),
		"refused":        self.ownership[server.PgOwnershipRefused].Load(),
		"uncertain":      self.ownership[server.PgOwnershipUncertain].Load(),
		"unknown_events": self.unknownEvents.Load(),
	}
}

// The deferred report also runs after an assertion failure. It names the
// observed boundaries instead of converting protocol timing into lock timing.
func (self *contractCloseOwnerJitterObservation) report(t testing.TB) {
	t.Helper()
	self.databaseWait.close()
	self.protocol.enabled(false)
	self.stateLock.Lock()
	cohortTimings := append([]legacyFinancialCohortObservation(nil), self.cohortTimings...)
	self.stateLock.Unlock()
	completed, visited, busy, failed := 0, 0, 0, 0
	cohortAttempts, cohortSelected, cohortCompleted, cohortFallbacks, cohortWriteRollbacks := 0, 0, 0, 0, 0
	for _, result := range self.results {
		completed += result.Completed
		visited += result.Visited
		busy += result.BusyOrGone
		failed += result.Failed
		cohortAttempts += result.FinancialCohortAttempts
		cohortSelected += result.FinancialCohortSelected
		cohortCompleted += result.FinancialCohortCompleted
		cohortFallbacks += result.FinancialCohortFallbacks
		cohortWriteRollbacks += result.FinancialCohortWriteRollbacks
	}
	report := map[string]any{
		"profile":     "healthy_mixed2048_cyclic_ready_jitter",
		"test_failed": t.Failed(), "pending_contracts_before_worker": self.pendingContracts,
		"paid_contracts": 1024, "free_contracts": 1024,
		"payer_networks": 16, "source_clients": 16, "typed_owners": 32,
		"equal_uuid_owner_pairs": 16, "contracts_per_owner": 64,
		"worker_slots": 32, "worker_eval_calls": self.workerCalls, "held_grants": 0,
		"context_budget_ns": int64(5 * time.Minute), "production_budgets_changed": true,
		"cohort_hard_timeout_ns":      int64(legacyFinancialCohortTimeout),
		"cohort_admission_reserve_ns": int64(legacyFinancialCohortAdmissionTime),
		"pool_limits_changed":         false, "fixture_seed_in_observation_window": false,
		"ready_jitter_sequence_ns": []int64{1000000, 2000000, 3000000, 4000000, 5000000},
		"observation_wall_ns":      time.Since(self.started).Nanoseconds(),
		"completed":                completed, "visited": visited, "busy_or_gone": busy, "failed": failed,
		"finished_close_tasks": self.finished, "task_retries": self.taskRetries,
		"post_retries": self.postRetries, "task_call_failed": self.taskCallFailed,
		"accounting_and_replay_verified": self.accountingAndReplayVerified,
		"transaction_callback_reruns":    self.reruns.callbacks.Load(), "ownership": self.ownershipCounts(),
		"protocol_by_route": self.protocol.snapshot(), "close_results": self.results,
		"financial_cohort_attempts": cohortAttempts, "financial_cohort_selected": cohortSelected,
		"financial_cohort_completed": cohortCompleted, "financial_cohort_fallbacks": cohortFallbacks,
		"financial_cohort_write_rollbacks": cohortWriteRollbacks,
		"cohort_database_timings":          cohortTimings,
		"financial_phase_diagnostic":       self.financialPhaseDiagnostic(),
		"database_lock_wait_observation":   self.databaseWait.snapshot(),
		"counter_delta":                    parallelCloseCounterDelta(t, self.beforeCounters, parallelCloseCounterSnapshot(t)),
		"close_counter_before":             self.beforeClosed, "close_counter_after": contractClosedCounter.Snapshot(),
		"qualifiers": []string{
			"The finite window includes mixed dispatch, task publication, 32 actual close targets, joined posts, accounting assertions and empty replay; fixture creation precedes observation.",
			"The direct PostgreSQL sampler covers both database routes with measured5ms cadence and a50ms maximum accepted blind interval. It adds one observer connection and reports its query wall cost; matched baseline and candidate must retain this same observation overhead. No absolute zero sub-gap wait claim follows.",
			"Both ordinary and maintenance PostgreSQL resources use the same disposable database through joined proxies; original pool limits and production task/page budgets remain unchanged.",
			"Each route repeats the deterministic 1,2,3,4,5ms sequence over observed Ready replies. Scheduler interleaving assigns replies to connections; timer overshoot and wall duration are not correctness thresholds.",
			"Zero contention acceptance covers model BusyOrGone, failed advisory admissions, uncertain ownership, transaction reruns, task/post retries and protocol error responses. It does not measure every PostgreSQL row-lock wait or pool-queue duration.",
			"Every healthy payer must complete exactly eight cohorts of eight without fallback. Real write-bearing rollbacks and disconnects also fail acceptance, independently of callback retry counters. This candidate uses a1s acquire/body cap and500ms prewrite reserve; parent deadlines, statement500ms, lock250ms and page15s remain unchanged.",
			"Provider totals are checked as exact account plus unapplied durable payload custody. Provider-total and mirror output-target executions and their retry counts are unmeasured in this close-owner profile.",
			"The separate held-grant test intentionally produces BusyOrGone and is not included in this healthy acceptance profile. No sustained throughput or fleet-capacity claim is made.",
		},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Error("healthy jitter evidence encoding failed", err)
		return
	}
	t.Logf("contract_close_owner_healthy_jitter=%s", raw)
}
