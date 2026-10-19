// Qualification-only phase correlation separates explicit Ready jitter from
// other client wall time. It retains no SQL, parameters or financial identities.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type legacyOwnerPhaseWireSummary struct {
	Stage            string `json:"stage"`
	Guard            string `json:"guard"`
	WallNs           int64  `json:"wall_ns"`
	Ready            int    `json:"ready"`
	RequestedDelayNs int64  `json:"requested_delay_ns"`
	ActualTimerNs    int64  `json:"actual_timer_ns"`
	ReadyForwardNs   int64  `json:"ready_forward_ns"`
	Frames           int64  `json:"frames"`
	Bytes            int64  `json:"bytes"`
}

type legacyOwnerTransactionDiagnostic struct {
	Kind             string                        `json:"kind"`
	WallNs           int64                         `json:"wall_ns"`
	Matched          bool                          `json:"matched"`
	Ambiguous        bool                          `json:"ambiguous"`
	Incomplete       bool                          `json:"incomplete"`
	Ready            int                           `json:"ready"`
	RequestedDelayNs int64                         `json:"requested_delay_ns"`
	ActualTimerNs    int64                         `json:"actual_timer_ns"`
	ReadyForwardNs   int64                         `json:"ready_forward_ns"`
	BeginCount       int64                         `json:"begin_count"`
	CommitCount      int64                         `json:"commit_count"`
	RollbackCount    int64                         `json:"rollback_count"`
	DroppedPhases    int                           `json:"dropped_phases"`
	Phases           []legacyOwnerPhaseWireSummary `json:"phases,omitempty"`
}

// A zero-delay Ready has no timer interval. Incomplete forwarding is never
// turned into a duration from the zero time or a confirmed reply.
func legacyOwnerReadyDurations(ready legacyCohortReadyDiagnostic) (timer, forward int64) {
	forwardedAfter := ready.ReadyReceived
	if !ready.TimerStart.IsZero() && !ready.TimerDone.IsZero() {
		timer = ready.TimerDone.Sub(ready.TimerStart).Nanoseconds()
		forwardedAfter = ready.TimerDone
	}
	if ready.Forwarded && !ready.ForwardDone.IsZero() && !forwardedAfter.IsZero() {
		forward = ready.ForwardDone.Sub(forwardedAfter).Nanoseconds()
	}
	return
}

// One BEGIN-bearing Ready observed before the callback's bind timestamp pins
// the transaction. A pooled connection's later transaction cannot replace it.
func correlateLegacyOwnerTransaction(observation legacyFinancialDiagnosticObservation, connections []legacyCohortConnectionDiagnosticSnapshot) legacyOwnerTransactionDiagnostic {
	result := legacyOwnerTransactionDiagnostic{Kind: observation.Kind, WallNs: observation.WallNs, DroppedPhases: observation.DroppedPhases}
	var connection *legacyCohortConnectionDiagnosticSnapshot
	var ordinal uint64
	for index := range connections {
		candidate := &connections[index]
		if candidate.BackendPid != observation.backendPID {
			continue
		}
		for _, ready := range candidate.Ready {
			if ready.BeginCount == 0 || ready.ReadyReceived.Before(observation.started) || ready.ReadyReceived.After(observation.bound) {
				continue
			}
			if connection != nil {
				result.Ambiguous = true
			}
			connection, ordinal = candidate, ready.TransactionOrdinal
		}
	}
	if connection == nil || result.Ambiguous {
		return result
	}
	result.Matched = true
	for _, ready := range connection.Ready {
		if ready.TransactionOrdinal != ordinal {
			continue
		}
		if ready.MixedTransactions {
			result.Ambiguous = true
			result.Matched = false
		}
		if !ready.Forwarded {
			result.Incomplete = true
		}
		result.Ready++
		result.RequestedDelayNs += ready.RequestedDelayNs
		timer, forward := legacyOwnerReadyDurations(ready)
		result.ActualTimerNs += timer
		result.ReadyForwardNs += forward
		result.BeginCount += int64(ready.BeginCount)
		result.CommitCount += int64(ready.CommitCount)
		result.RollbackCount += int64(ready.RollbackCount)
	}
	if result.BeginCount != 1 || result.CommitCount+result.RollbackCount != 1 {
		result.Incomplete = true
	}
	if observation.Kind != "cohort" {
		return result
	}
	previous := observation.started
	previousStage, previousGuard := "acquire_begin", "ready"
	for _, phase := range observation.Phases {
		summary := legacyOwnerPhaseWireSummary{Stage: previousStage, Guard: previousGuard, WallNs: phase.at.Sub(previous).Nanoseconds()}
		for _, ready := range connection.Ready {
			if ready.TransactionOrdinal != ordinal || ready.ReadyReceived.Before(previous) || !ready.ReadyReceived.Before(phase.at) {
				continue
			}
			summary.Ready++
			summary.RequestedDelayNs += ready.RequestedDelayNs
			timer, forward := legacyOwnerReadyDurations(ready)
			summary.ActualTimerNs += timer
			summary.ReadyForwardNs += forward
			summary.Frames += int64(ready.Frames)
			summary.Bytes += int64(ready.Bytes)
		}
		result.Phases = append(result.Phases, summary)
		previous, previousStage, previousGuard = phase.at, phase.Stage, phase.Guard
	}
	return result
}

func (self *contractCloseOwnerJitterObservation) financialPhaseDiagnostic() map[string]any {
	self.stateLock.Lock()
	observations := append([]legacyFinancialDiagnosticObservation(nil), self.financialDiagnostics...)
	dropped := self.droppedFinancialDiagnostics
	self.stateLock.Unlock()
	ordinary := self.protocol.proxies[server.DefaultPgVaultResourceName].diagnosticSnapshot()
	transactions := make([]legacyOwnerTransactionDiagnostic, 0, len(observations))
	groups := map[string][]int{}
	var missing, ambiguous, incomplete int
	privatePgCorrelation := make([]map[string]any, 0)
	for _, observation := range observations {
		if observation.Kind == "cohort" {
			privatePgCorrelation = append(privatePgCorrelation, map[string]any{
				"cohort_ordinal": len(privatePgCorrelation) + 1, "fixture_pg_backend_pid": observation.backendPID,
				"started_at": observation.started.UTC(), "bound_at": observation.bound.UTC(), "finished_at": observation.finished.UTC(),
			})
		}
		result := correlateLegacyOwnerTransaction(observation, ordinary.Connections)
		if !result.Matched {
			missing++
		}
		if result.Ambiguous {
			ambiguous++
		}
		if result.Incomplete {
			incomplete++
		}
		transactions = append(transactions, result)
		if result.Matched && !result.Incomplete {
			groups[result.Kind] = append(groups[result.Kind], result.Ready)
		}
	}
	summaries := map[string]any{}
	for kind, counts := range groups {
		slices.Sort(counts)
		total := 0
		for _, count := range counts {
			total += count
		}
		summaries[kind] = map[string]any{"transactions": len(counts), "ready_total": total,
			"ready_min": counts[0], "ready_max": counts[len(counts)-1], "ready_median": counts[len(counts)/2]}
	}
	cohorts := make([]legacyOwnerTransactionDiagnostic, 0)
	for _, transaction := range transactions {
		if transaction.Kind == "cohort" {
			cohorts = append(cohorts, transaction)
		}
	}
	return map[string]any{
		"diagnostic_only":               true,
		"native_setup_barrier":          self.nativeDiagnosticSetup,
		"user_acceptance":               []string{"at least5x throughput", "2048 overlapping contracts", "zero observed contention/retries/healthy write-bearing rollbacks"},
		"candidate_design_budgets_ns":   map[string]int64{"cohort_hard": int64(legacyFinancialCohortTimeout), "write_entry_reserve": int64(legacyFinancialCohortAdmissionTime)},
		"budgets_are_user_requirements": false,
		"matched_transaction_groups":    summaries, "cohorts": cohorts,
		"private_fixture_pg_correlation": privatePgCorrelation,
		"unmatched_transactions":         missing, "ambiguous_transactions": ambiguous, "incomplete_transactions": incomplete,
		"dropped_model_observations": dropped,
		"wire_ready_total":           ordinary.ReadyTotal, "wire_ready_recorded": ordinary.ReadyRecorded,
		"wire_dropped_ready": ordinary.DroppedReady, "wire_dropped_connections": ordinary.DroppedConnections,
		"wire_overflow":      ordinary.Overflow,
		"wire_pending_ready": ordinary.PendingReady,
		"qualifiers": []string{
			"Ready boundaries count actual complete server replies for each pinned transaction, including prepare and rollback; paid/source singleton groups provide the individual comparison.",
			"Requested timer delay differs from elapsed timer wait; timer overshoot includes process scheduling. Forward duration covers only the Ready frame writes, not SQL execution or every preceding data frame.",
			"Phase wall minus observed timer/forward intervals remains unattributed client/server work; private PostgreSQL duration logs and cgroup deltas are separate diagnostic evidence.",
			"No numeric budget, lock order, query, fixed2048 packing or zero gate is changed. This diagnostic run is never a throughput or release GO.",
		},
	}
}

// A reused connection must match the first complete BEGIN inside the bounded
// observation, not a later transaction or a different backend. All times are
// fixed data: wall-speed assertions cannot make this control flaky.
func TestLegacyFinancialDiagnosticCorrelatesExactTransactionAndPhases(t *testing.T) {
	base := time.Unix(1, 0)
	at := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	observation := legacyFinancialDiagnosticObservation{Kind: "cohort", backendPID: 7, started: at(0), bound: at(3), WallNs: int64(10 * time.Millisecond),
		Phases: []legacyFinancialDiagnosticPhase{{Stage: "bound", Guard: "ready", at: at(3)}, {Stage: "intents", Guard: "ready", at: at(5)}, {Stage: "headers", Guard: "soft_refusal", at: at(8)}, {Stage: "joined", Guard: "finished", at: at(10)}},
	}
	ready := func(ordinal uint64, read, done int) legacyCohortReadyDiagnostic {
		return legacyCohortReadyDiagnostic{Forwarded: true, TransactionOrdinal: ordinal, ReadyReceived: at(read), TimerStart: at(read), TimerDone: at(done), ForwardDone: at(done), RequestedDelayNs: int64(time.Duration(done-read) * time.Millisecond)}
	}
	first, query, end, later := ready(1, 1, 2), ready(1, 5, 7), ready(1, 8, 9), ready(2, 11, 12)
	first.BeginCount = 1
	end.RollbackCount = 1
	later.BeginCount = 1
	connections := []legacyCohortConnectionDiagnosticSnapshot{{BackendPid: 7, Ready: []legacyCohortReadyDiagnostic{first, query, end, later}}}
	result := correlateLegacyOwnerTransaction(observation, connections)
	if !result.Matched || result.Ambiguous || result.Incomplete || result.Ready != 3 || result.BeginCount != 1 || result.RollbackCount != 1 || result.CommitCount != 0 || result.RequestedDelayNs != int64(4*time.Millisecond) || len(result.Phases) != 4 || result.Phases[2].Ready != 1 || result.Phases[3].Ready != 1 {
		t.Fatal("phase diagnostic lost exact transaction/Ready custody", result)
	}
	zeroDelay := legacyCohortReadyDiagnostic{Forwarded: true, ReadyReceived: at(1), ForwardDone: at(2)}
	if timer, forward := legacyOwnerReadyDurations(zeroDelay); timer != 0 || forward != int64(time.Millisecond) {
		t.Fatal("zero delay fabricated timer or forwarding wall", timer, forward)
	}
	zeroDelay.Forwarded = false
	if timer, forward := legacyOwnerReadyDurations(zeroDelay); timer != 0 || forward != 0 {
		t.Fatal("incomplete reply fabricated forwarding", timer, forward)
	}
	connections[0].BackendPid = 8
	if wrong := correlateLegacyOwnerTransaction(observation, connections); wrong.Matched {
		t.Fatal("another backend supplied this transaction", wrong)
	}
	connections[0].BackendPid = 7
	connections[0].Ready = append(connections[0].Ready, first)
	if duplicate := correlateLegacyOwnerTransaction(observation, connections); duplicate.Matched || !duplicate.Ambiguous {
		t.Fatal("ambiguous begin authorized diagnostic attribution", duplicate)
	}
}

func TestLegacyFinancialDiagnosticIsOptionalAndBounded(t *testing.T) {
	if observation := newLegacyFinancialDiagnostic(context.Background(), "cohort"); observation != nil {
		t.Fatal("ordinary context allocated a diagnostic")
	}
	var result legacyFinancialDiagnosticObservation
	calls := 0
	ctx := context.WithValue(context.Background(), legacyFinancialDiagnosticKey{}, func(value legacyFinancialDiagnosticObservation) { result = value; calls++ })
	observation := newLegacyFinancialDiagnostic(ctx, "cohort")
	for index := 0; index < legacyFinancialDiagnosticPhaseLimit+1; index++ {
		observation.phase("intents", "ready")
	}
	observation.finish()
	observation.finish()
	if calls != 1 {
		t.Fatal("joined and deferred finish emitted duplicate custody", calls)
	}
	if len(result.Phases) != legacyFinancialDiagnosticPhaseLimit || result.DroppedPhases != 2 {
		t.Fatal("phase overflow lost finite custody", len(result.Phases), result.DroppedPhases)
	}
	result.backendPID = 987654321
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["backendPID"]; ok || strings.Contains(string(encoded), "987654321") {
		t.Fatal("private correlation key was emitted", fields)
	}
}

// The launcher may configure only the disposable database after seeding. The
// fresh pool rebind follows the acknowledgement, outside all observed work.
func awaitLegacyOwnerDiagnosticNativeSetup(t testing.TB, ctx context.Context) bool {
	t.Helper()
	directory := os.Getenv("URN_OWNER_DIAGNOSTIC_BARRIER_DIR")
	if directory == "" {
		return false
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("diagnostic barrier directory is not absolute")
	}
	readyPath, ackPath := filepath.Join(directory, "ready.json"), filepath.Join(directory, "continue-v1.txt")
	if _, err := os.Stat(ackPath); !os.IsNotExist(err) {
		t.Fatal("diagnostic acknowledgement already exists or cannot be checked", err)
	}
	ready, err := os.OpenFile(readyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("diagnostic ready marker cannot be created", err)
	}
	_, writeErr := ready.WriteString("{\"version\":1,\"stage\":\"fixture_seeded_before_pool_rebind\"}\n")
	closeErr := ready.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("diagnostic ready marker failed", writeErr, closeErr)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Minute)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("diagnostic native setup lost parent", ctx.Err())
		case <-timeout.C:
			t.Fatal("diagnostic native setup was not acknowledged")
		case <-ticker.C:
			info, err := os.Stat(ackPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || info.Size() > 64 {
				t.Fatal("diagnostic acknowledgement is invalid", err)
			}
			value, err := os.ReadFile(ackPath)
			if err != nil || string(value) != "continue-v1\n" {
				t.Fatal("diagnostic acknowledgement token is invalid", err)
			}
			return true
		}
	}
}

func requireLegacyOwnerDiagnosticLogging(t testing.TB, ctx context.Context) {
	t.Helper()
	for _, read := range []func(context.Context, func(server.PgConn), ...any){server.Db, server.MaintenanceDb} {
		read(ctx, func(conn server.PgConn) {
			var threshold string
			server.Raise(conn.QueryRow(ctx, `SHOW log_min_duration_statement`).Scan(&threshold))
			if threshold != "1ms" {
				t.Fatal("fresh diagnostic connection has wrong statement threshold", threshold)
			}
		})
	}
}

// The observer records real guard decisions without becoming a budget, retry
// decision or ownership authority. The clock change is deterministic.
func TestLegacyFinancialDiagnosticPreservesBudgetGuardDecisions(t *testing.T) {
	base := time.Now()
	now := base
	parent, cancel := context.WithTimeout(t.Context(), legacyFinancialCohortTimeout)
	defer cancel()
	var result legacyFinancialDiagnosticObservation
	parent = context.WithValue(parent, legacyFinancialDiagnosticKey{}, func(value legacyFinancialDiagnosticObservation) { result = value })
	parent = context.WithValue(parent, legacyFinancialCohortClockKey{}, func() time.Time { return now })
	ctx := withLegacyFinancialCohortBudget(parent)
	budget := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget)
	budget.diagnostic = newLegacyFinancialDiagnostic(ctx, "cohort")
	if err := checkLegacyFinancialCohortBudget(ctx, "intents"); err != nil {
		t.Fatal(err)
	}
	now = base.Add(time.Second)
	if err := checkLegacyFinancialCohortBudget(ctx, "headers"); !errors.Is(err, errLegacyFinancialCohortBudget) {
		t.Fatal("diagnostic changed unwritten admission refusal", err)
	}
	budget.written = true
	if err := checkLegacyFinancialCohortBudget(ctx, "post_outcomes"); err != nil {
		t.Fatal("diagnostic changed admitted hard-budget completion", err)
	}
	cancel()
	if err := checkLegacyFinancialCohortBudget(ctx, "provenance"); !errors.Is(err, context.Canceled) {
		t.Fatal("diagnostic changed real context cancellation", err)
	}
	budget.diagnostic.finish()
	expected := [][2]string{{"intents", "ready"}, {"headers", "soft_refusal"}, {"post_outcomes", "ready"}, {"provenance", "context_error"}, {"joined", "finished"}}
	if len(result.Phases) != len(expected) {
		t.Fatal("diagnostic lost real completed guard decisions", result)
	}
	for index, want := range expected {
		got := result.Phases[index]
		if got.Stage != want[0] || got.Guard != want[1] || (index > 0 && got.ElapsedNs < result.Phases[index-1].ElapsedNs) {
			t.Fatal("diagnostic reordered or reclassified a guard", index, got, want)
		}
	}
}
