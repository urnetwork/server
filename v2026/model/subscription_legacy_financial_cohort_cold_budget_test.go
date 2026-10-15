// Causal admission controls use actual financial-read replies and exact money
// oracles. The private admission clock models elapsed entry work, not SQL time.
package model

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestLegacyFinancialCohortWarmEntryCommitsPackedEight(t *testing.T) {
	legacyFinancialCohortRequireEntryBudget(t, 200*time.Millisecond, time.Minute, true)
}

// The v11 finite observation entered writes after 286.58-292.82 ms. A300ms
// completed-read boundary isolates that admission decision without adding
// sleeps, changing database resources or substituting financial results.
func TestLegacyFinancialCohortColdEntryCommitsPackedEight(t *testing.T) {
	legacyFinancialCohortRequireEntryBudget(t, 300*time.Millisecond, time.Minute, true)
}

// A600ms parent leaves only100ms of prewrite entry time with the500ms reserve.
// The same cold entry must refuse before any writes and retain its real parent.
func TestLegacyFinancialCohortColdEntryPreservesShortParent(t *testing.T) {
	legacyFinancialCohortRequireEntryBudget(t, 300*time.Millisecond, 600*time.Millisecond, false)
}

func legacyFinancialCohortRequireEntryBudget(t *testing.T, elapsed, parentBudget time.Duration, wantPacked bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, 8)
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		var advanced atomic.Bool
		var readSequence atomic.Int64
		var invalidAdmission atomic.Bool
		var events [5]atomic.Int64
		var diagnostic legacyFinancialDiagnosticObservation
		var diagnostics int
		base := time.Now()
		clock := func() time.Time {
			if advanced.Load() {
				return base.Add(elapsed)
			}
			return base
		}
		observed := context.WithValue(ctx, legacyFinancialCohortClockKey{}, clock)
		observed = context.WithValue(observed, legacyFinancialDiagnosticKey{}, func(value legacyFinancialDiagnosticObservation) {
			diagnostic = value
			diagnostics++
		})
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			events[event.Kind].Add(1)
			if event.Kind == server.PgOwnershipAdmitted && (!event.TransactionScoped || event.BackendPid == 0 ||
				!slices.Contains(event.Keys, server.NewPgOwnershipKey("transfer_balance", fixture.payer.balanceId))) {
				invalidAdmission.Store(true)
			}
		})
		reruns := &parallelCloseRetryObservation{}
		observed = reruns.context(observed)
		observed, finishPosts := withLegacySettlementPostBatch(observed)
		defer finishPosts()
		parent, cancelParent := context.WithDeadline(observed, base.Add(parentBudget))
		defer cancelParent()
		ordinary := protocol.proxies[server.DefaultPgVaultResourceName]
		// Exactly eight seeded rows yield16 reports,8 escrows, two empty
		// membership results and one admission snapshot. Match the whole
		// completed statement sequence; never retain SQL, parameters or rows.
		pattern := [][]byte{[]byte("SELECT 16\x00"), []byte("SELECT 8\x00"), []byte("SELECT 0\x00"), []byte("SELECT 0\x00"), []byte("SELECT 1\x00")}
		ordinary.commandCompleteObservation.Store(&legacyCohortCommandCompleteObservation{observe: func(tag []byte) {
			if advanced.Load() {
				return
			}
			index := int(readSequence.Load())
			if bytes.Equal(tag, pattern[index]) {
				index++
			} else if bytes.Equal(tag, pattern[0]) {
				index = 1
			} else {
				index = 0
			}
			readSequence.Store(int64(index))
			if index == len(pattern) {
				advanced.Store(true)
			}
		}})
		before := contractClosedCounter.Snapshot()
		protocol.enabled(true)
		attempts, err := flushLegacySettlementCohort(parent, fixture.ids)
		protocol.enabled(false)
		ordinary.commandCompleteObservation.Store(nil)
		wire := protocol.snapshot()
		after := contractClosedCounter.Snapshot()
		if err != nil || parent.Err() != nil || ctx.Err() != nil {
			t.Fatal("entry control lost its real parent or returned an I/O error")
		}
		if !advanced.Load() || readSequence.Load() != int64(len(pattern)) || diagnostics != 1 || diagnostic.Kind != "cohort" || diagnostic.DroppedPhases != 0 {
			t.Fatal("entry control did not observe the exact completed financial-read sequence")
		}
		packed := len(attempts) == len(fixture.ids)
		if packed {
			for index, attempt := range attempts {
				if attempt.contractId != fixture.ids[index] || !attempt.completed || attempt.fallback || attempt.deadlineFallback || attempt.busy || attempt.financialWriteRollback {
					t.Fatal("entry control changed an admitted contract's packed completion", index)
				}
			}
		} else if len(attempts) != 1 || attempts[0].contractId != fixture.ids[0] || !attempts[0].fallback || !attempts[0].deadlineFallback ||
			attempts[0].completed || attempts[0].busy || attempts[0].financialWriteRollback {
			t.Fatal("entry control lost the read-only soft-refusal prefix")
		}
		var outcomeGuards, softRefusals int
		for _, phase := range diagnostic.Phases {
			if phase.Guard == "context_error" || phase.Guard == "soft_refusal" && phase.Stage != "outcomes" {
				t.Fatal("entry control stopped before its intended completed-read boundary")
			}
			if phase.Guard == "soft_refusal" {
				softRefusals++
			}
			if phase.Stage == "outcomes" {
				outcomeGuards++
				wantGuard := "soft_refusal"
				if packed {
					wantGuard = "ready"
				}
				if phase.Guard != wantGuard {
					t.Fatal("entry control changed the exact prewrite guard")
				}
			}
		}
		wantSoftRefusals := 1
		var commits, completed int64
		rollbacks := int64(1)
		if packed {
			wantSoftRefusals, commits, rollbacks, completed = 0, 1, 0, 8
		}
		if outcomeGuards != 1 || softRefusals != wantSoftRefusals || invalidAdmission.Load() ||
			events[server.PgOwnershipAdmitted].Load() != 1 || events[server.PgOwnershipReleased].Load() != 1 ||
			events[server.PgOwnershipWaiting].Load() != 0 || events[server.PgOwnershipRefused].Load() != 0 ||
			events[server.PgOwnershipUncertain].Load() != 0 || reruns.callbacks.Load() != 0 {
			t.Fatal("entry control lost exact ownership, retry or prewrite custody")
		}
		if wire[server.DefaultPgVaultResourceName]["begin_commands_observed"] != 1 ||
			wire[server.DefaultPgVaultResourceName]["commit_commands_observed"] != commits ||
			wire[server.DefaultPgVaultResourceName]["rollback_commands_observed"] != rollbacks ||
			wire[server.DefaultPgVaultResourceName]["transactions_with_writes_committed"] != commits ||
			wire[server.DefaultPgVaultResourceName]["transactions_with_writes_rolled_back"] != 0 {
			t.Fatal("entry control did not confirm exactly one financial transaction end")
		}
		for _, counters := range wire {
			for name, count := range counters {
				if (strings.HasPrefix(name, "error_response_") || name == "connections_closed_after_begin_without_end_command" ||
					name == "connections_closed_with_active_writes") && count != 0 {
					t.Fatal("entry control observed an error, lost transaction or lost write acknowledgement", name)
				}
			}
		}
		if !before.Stable || !after.Stable || before.Overflow || after.Overflow || after.Confirmed-before.Confirmed != uint64(completed) ||
			before.Uncertain != after.Uncertain || before.Untracked != after.Untracked {
			t.Fatal("entry control lost committed financial outcome custody")
		}
		finishPosts()
		if !packed {
			legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
			if legacyTargetMirrorQueueCount(ctx) != 0 {
				t.Fatal("read-only entry refusal retained a financial output producer")
			}
			for _, id := range fixture.ids {
				completed, busy, _, err := flushLegacySettlement(ctx, id)
				if err != nil || !completed || busy {
					t.Fatal("ordinary singleton recovery failed after read-only refusal")
				}
			}
		}
		expected := legacyFinancialCohortCompleted(fixture.ids)
		legacyFinancialCohortRequire(t, ctx, fixture, expected)
		drain := legacyCohortLatencyDrain(t, ctx, 9)
		if drain.Finished != 9 || drain.Remaining != 0 {
			t.Fatal("entry control failed exact durable output drain")
		}
		for _, balanceId := range fixture.balances {
			if Testing_NetEscrowByteCount(ctx, balanceId) != 0 {
				t.Fatal("entry control retained released reservation capacity")
			}
		}
		legacyFinancialCohortRequire(t, ctx, fixture, expected)
		if replay, err := FlushLegacyPayerSettlements(ctx, fixture.payer.sourceNetworkId, nil, 8); err != nil ||
			replay.Visited != 0 || replay.Completed != 0 || replay.BusyOrGone != 0 || replay.Failed != 0 || replay.More {
			t.Fatal("entry control replay repeated financial work")
		}
		legacyFinancialCohortRequire(t, ctx, fixture, expected)
		final := contractClosedCounter.Snapshot()
		if !final.Stable || final.Overflow || final.Confirmed-before.Confirmed != 8 || final.Uncertain != before.Uncertain || final.Untracked != before.Untracked {
			t.Fatal("entry control lost exact final financial outcome custody")
		}
		t.Logf("cohort_entry_budget modeled_entry_ns=%d parent_budget_ns=%d body_cap_ns=%d prewrite_reserve_ns=%d packed=%t expected_packed=%t financial_conservation_and_replay=true durable_outputs_drained=9; modeled clock is not elapsed throughput or a database-resource result",
			elapsed.Nanoseconds(), parentBudget.Nanoseconds(), int64(legacyFinancialCohortTimeout), int64(legacyFinancialCohortAdmissionTime), packed, wantPacked)
		// Keep every conservation and replay assertion above the causal red.
		// On the baseline only the300ms packed-eight expectation should fail.
		if packed != wantPacked {
			t.Fatalf("completed financial-read entry packed=%t, want%t", packed, wantPacked)
		}
	})
}
