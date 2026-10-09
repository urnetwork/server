// Cooperative admission yields only at completed protocol boundaries. Real
// partial-write rollback is an accounting control, not healthy zero-retry work.
package model

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The parent deadline remains the hard limit, while an injected admission
// clock controls only the decision to start another financial exchange.
func TestLegacyFinancialCohortBudgetPreservesParentDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(t.Context(), deadline)
	defer cancelParent()
	child, cancelChild := context.WithTimeout(parent, 2*time.Minute)
	defer cancelChild()
	now := deadline.Add(-legacyFinancialCohortAdmissionTime - time.Nanosecond)
	ctx := withLegacyFinancialCohortBudget(context.WithValue(child, legacyFinancialCohortClockKey{}, func() time.Time { return now }))
	actual, ok := ctx.Deadline()
	budget, _ := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget)
	if !ok || !actual.Equal(deadline) || budget == nil || !budget.admitBefore.Equal(deadline.Add(-legacyFinancialCohortAdmissionTime)) {
		t.Fatal("cooperative budget changed the actual parent deadline", actual, budget)
	}
	if err := checkLegacyFinancialCohortBudget(ctx); err != nil {
		t.Fatal("admission ended before its boundary", err)
	}
	now = budget.admitBefore
	if err := checkLegacyFinancialCohortBudget(ctx); !errors.Is(err, errLegacyFinancialCohortBudget) || ctx.Err() != nil {
		t.Fatal("admission boundary canceled the hard context", err, ctx.Err())
	}
	cancelParent()
	if err := checkLegacyFinancialCohortBudget(ctx); !errors.Is(err, context.Canceled) || errors.Is(err, errLegacyFinancialCohortBudget) {
		t.Fatal("soft budget hid real parent cancellation", err)
	}
}

// Both arms enter real transaction ownership. One advances the admission clock
// after its reply; the other waits for the actual eight-row outcome update.
func legacyFinancialCohortRequireBudgetHandoff(t *testing.T, afterWrite bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, legacyFinancialCohortLimit)
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		var advanced atomic.Bool
		var outcomeUpdates atomic.Int64
		var invalidAdmission atomic.Bool
		var events [5]atomic.Int64
		base := time.Now()
		clock := func() time.Time {
			if advanced.Load() {
				return base.Add(time.Hour)
			}
			return base
		}
		observed := context.WithValue(ctx, legacyFinancialCohortClockKey{}, clock)
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			events[event.Kind].Add(1)
			if event.Kind == server.PgOwnershipAdmitted {
				if !event.TransactionScoped || event.BackendPid == 0 || !slices.Contains(event.Keys, server.NewPgOwnershipKey("transfer_balance", fixture.payer.balanceId)) {
					invalidAdmission.Store(true)
				}
				if !afterWrite {
					advanced.Store(true)
				}
			}
		})
		retries := &parallelCloseRetryObservation{}
		observed = retries.context(observed)
		ordinary := protocol.proxies[server.DefaultPgVaultResourceName]
		ordinary.commandCompleteObservation.Store(&legacyCohortCommandCompleteObservation{observe: func(tag []byte) {
			if events[server.PgOwnershipAdmitted].Load() > 0 && bytes.Equal(tag, []byte("UPDATE 8\x00")) {
				outcomeUpdates.Add(1)
				if afterWrite {
					advanced.Store(true)
				}
			}
		}})
		before := contractClosedCounter.Snapshot()
		protocol.enabled(true)
		attempts, err := flushLegacySettlementCohort(observed, fixture.ids)
		protocol.enabled(false)
		ordinary.commandCompleteObservation.Store(nil)
		wire := protocol.snapshot()
		after := contractClosedCounter.Snapshot()
		if err != nil || ctx.Err() != nil || len(attempts) != 1 {
			t.Fatal("cooperative rollback did not return a live-parent fallback", attempts, err, ctx.Err())
		}
		attempt := attempts[0]
		if attempt.contractId != fixture.ids[0] || !attempt.fallback || !attempt.deadlineFallback || attempt.completed || attempt.busy || attempt.financialWriteRollback != afterWrite {
			t.Fatal("cooperative rollback lost exact prefix or write custody", attempt, afterWrite)
		}
		if !advanced.Load() || invalidAdmission.Load() || events[server.PgOwnershipAdmitted].Load() != 1 || events[server.PgOwnershipReleased].Load() != 1 ||
			events[server.PgOwnershipWaiting].Load() != 0 || events[server.PgOwnershipRefused].Load() != 0 || events[server.PgOwnershipUncertain].Load() != 0 || retries.callbacks.Load() != 0 {
			t.Fatal("cooperative boundary did not confirm actual ownership cleanup", advanced.Load(), invalidAdmission.Load(),
				events[server.PgOwnershipAdmitted].Load(), events[server.PgOwnershipReleased].Load(), events[server.PgOwnershipWaiting].Load(),
				events[server.PgOwnershipRefused].Load(), events[server.PgOwnershipUncertain].Load(), retries.callbacks.Load())
		}
		writes := int64(0)
		if afterWrite {
			writes = 1
		}
		if outcomeUpdates.Load() != writes || wire[server.DefaultPgVaultResourceName]["begin_commands_observed"] != 1 ||
			wire[server.DefaultPgVaultResourceName]["rollback_commands_observed"] != 1 || wire[server.DefaultPgVaultResourceName]["commit_commands_observed"] != 0 ||
			wire[server.DefaultPgVaultResourceName]["transactions_with_writes_rolled_back"] != writes {
			t.Fatal("fallback lacks its exact completed-write and rollback replies", outcomeUpdates.Load(), wire)
		}
		for route, counters := range wire {
			for name, count := range counters {
				if (strings.HasPrefix(name, "error_response_") || name == "connections_closed_after_begin_without_end_command" ||
					name == "connections_closed_with_active_writes" || name == "transactions_with_writes_committed") && count != 0 {
					t.Fatal("cooperative fallback canceled protocol work or committed writes", route, name, count)
				}
			}
		}
		if !before.Stable || !after.Stable || before.Confirmed != after.Confirmed || before.Uncertain != after.Uncertain || before.Untracked != after.Untracked {
			t.Fatal("rolled-back cohort escaped committed outcome custody", before, after)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
		if legacyTargetMirrorQueueCount(ctx) != 0 {
			t.Fatal("rolled-back cohort retained a mirror producer")
		}
		for _, id := range fixture.ids {
			completed, busy, _, err := flushLegacySettlement(ctx, id)
			if err != nil || !completed || busy {
				t.Fatal("ordinary singleton did not recover the retained outcome", id, completed, busy, err)
			}
		}
		completed := legacyFinancialCohortCompleted(fixture.ids)
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		if replay, err := FlushLegacyPayerSettlements(ctx, fixture.payer.sourceNetworkId, nil, legacyFinancialCohortLimit); err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.More {
			t.Fatal("singleton recovery replay repeated financial work", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		final := contractClosedCounter.Snapshot()
		if !final.Stable || final.Confirmed-before.Confirmed != uint64(len(fixture.ids)) || final.Uncertain != before.Uncertain || final.Untracked != before.Untracked {
			t.Fatal("singleton recovery lost exact per-contract commit custody", before, final)
		}
		t.Logf("cohort_budget_handoff after_outcome_write=%t actual_outcome_updates=%d financial_write_rollback=%t protocol=%v; intentional fallback control, not healthy zero-retry evidence",
			afterWrite, outcomeUpdates.Load(), attempt.financialWriteRollback, wire)
	})
}

// A real admitted key set is released by acknowledged rollback before any
// financial mutation, with all original intents available to the singleton.
func TestLegacyFinancialCohortBudgetHandoffAfterAdmission(t *testing.T) {
	legacyFinancialCohortRequireBudgetHandoff(t, false)
}

// An actual outcome update is rolled back, then exact accounting is recovered.
// This intentional repeated financial work must fail the healthy retry gate.
func TestLegacyFinancialCohortBudgetHandoffAfterOutcomeWrite(t *testing.T) {
	legacyFinancialCohortRequireBudgetHandoff(t, true)
}

// Real parent cancellation after an ownership reply remains an error. It must
// not be relabeled as an eligible soft-budget fallback or uncertain release.
func TestLegacyFinancialCohortBudgetParentCancellationDoesNotFallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, legacyFinancialCohortLimit)
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		caller, cancelCaller := context.WithCancel(ctx)
		defer cancelCaller()
		base := time.Now()
		caller = context.WithValue(caller, legacyFinancialCohortClockKey{}, func() time.Time { return base })
		var events [5]atomic.Int64
		caller = server.Testing_WithPgOwnershipObservation(caller, func(event server.PgOwnershipEvent) {
			events[event.Kind].Add(1)
			if event.Kind == server.PgOwnershipAdmitted {
				cancelCaller()
			}
		})
		protocol.enabled(true)
		attempts, err := flushLegacySettlementCohort(caller, fixture.ids)
		protocol.enabled(false)
		wire := protocol.snapshot()
		if !errors.Is(err, context.Canceled) || len(attempts) != 0 || ctx.Err() != nil {
			t.Fatal("real parent cancellation became a successful fallback", attempts, err, ctx.Err())
		}
		if events[server.PgOwnershipAdmitted].Load() != 1 || events[server.PgOwnershipReleased].Load() != 1 || events[server.PgOwnershipUncertain].Load() != 0 ||
			events[server.PgOwnershipWaiting].Load() != 0 || events[server.PgOwnershipRefused].Load() != 0 ||
			wire[server.DefaultPgVaultResourceName]["rollback_commands_observed"] != 1 || wire[server.DefaultPgVaultResourceName]["transactions_with_writes_rolled_back"] != 0 {
			t.Fatal("completed-boundary cancellation lost acknowledged rollback", wire, events[server.PgOwnershipReleased].Load(), events[server.PgOwnershipUncertain].Load())
		}
		legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
	})
}

// Holding a real in-flight reply forces pgx to close its canceled connection.
// The observer must preserve uncertainty when no rollback reply was received.
func TestLegacyFinancialCohortInFlightCancellationRetainsUncertainty(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, 2)
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		proxy := protocol.proxies[server.DefaultPgVaultResourceName]
		var events [5]atomic.Int64
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) { events[event.Kind].Add(1) })
		var connectionCleanup <-chan struct{}
		var transactionErr error
		proxy.enabled.Store(true)
		server.HandleError(func() {
			server.Tx(observed, func(tx server.PgTx) {
				admitted, err := server.TryTxOwnership(observed, tx, []server.PgOwnershipKey{server.NewPgOwnershipKey("transfer_balance", fixture.payer.balanceId)})
				server.Raise(err)
				if !admitted {
					server.Raise(errors.New("synthetic cancellation fixture did not admit its real grant"))
				}
				connectionCleanup = tx.Conn().PgConn().CleanupDone()
				queryCtx, cancelQuery := context.WithTimeout(observed, legacyFinancialCohortTimeout)
				defer cancelQuery()
				proxy.holdSet.Store(true)
				server.RaisePgResult(tx.Exec(queryCtx, `SET LOCAL lock_timeout='250ms'`))
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { transactionErr = err })
		if !errors.Is(transactionErr, context.DeadlineExceeded) || ctx.Err() != nil || connectionCleanup == nil || proxy.held.Load() != 1 {
			t.Fatal("real reply barrier did not force an in-flight child cancellation", transactionErr, ctx.Err(), proxy.snapshot())
		}
		select {
		case <-connectionCleanup:
		case <-ctx.Done():
			t.Fatal("canceled connection cleanup did not finish", ctx.Err())
		}
		if events[server.PgOwnershipAdmitted].Load() != 1 || events[server.PgOwnershipUncertain].Load() != 1 || events[server.PgOwnershipReleased].Load() != 0 ||
			events[server.PgOwnershipWaiting].Load() != 0 || events[server.PgOwnershipRefused].Load() != 0 {
			t.Fatal("canceled transport manufactured confirmed ownership release", events[server.PgOwnershipAdmitted].Load(),
				events[server.PgOwnershipUncertain].Load(), events[server.PgOwnershipReleased].Load())
		}
		proxy.close()
		wire := proxy.snapshot()
		if wire["begin_commands_observed"] != 1 || wire["commit_commands_observed"] != 0 || wire["rollback_commands_observed"] != 0 ||
			wire["connections_closed_after_begin_without_end_command"] != 1 || wire["connections_closed_with_active_writes"] != 0 {
			t.Fatal("in-flight cancellation lost its actual incomplete transaction", wire)
		}
		cleanup()
		legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
		t.Logf("cohort_inflight_cancel_control ownership_uncertain=%d protocol=%v; deliberate transport failure, not healthy acceptance evidence",
			events[server.PgOwnershipUncertain].Load(), wire)
	})
}
