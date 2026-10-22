// Cooperative admission refuses unwritten work at completed protocol boundaries.
// Admitted writes finish within the hard deadline; genuine failures still roll back.
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

	"github.com/urnetwork/server/v2026"
)

// The parent deadline remains the hard limit, while an injected admission
// clock controls only the decision to enter financial writes.
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

// A written cohort is exempt only from the soft admission boundary. Real
// cancellation and an already-expired hard deadline still belong to its owner.
func TestLegacyFinancialCohortWrittenBudgetPreservesHardCancellation(t *testing.T) {
	parent, cancelParent := context.WithTimeout(t.Context(), time.Minute)
	defer cancelParent()
	now := time.Now().Add(time.Hour)
	clock := func() time.Time { return now }
	ctx := withLegacyFinancialCohortBudget(context.WithValue(parent, legacyFinancialCohortClockKey{}, clock))
	budget := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget)
	if err := checkLegacyFinancialCohortBudget(ctx); !errors.Is(err, errLegacyFinancialCohortBudget) {
		t.Fatal("unwritten work bypassed its exhausted admission budget", err)
	}
	budget.written = true
	if err := checkLegacyFinancialCohortBudget(ctx); err != nil {
		t.Fatal("admitted writes were refused by the soft boundary", err)
	}
	cancelParent()
	if err := checkLegacyFinancialCohortBudget(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("admitted writes ignored real parent cancellation", err)
	}
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancelExpired()
	expired = withLegacyFinancialCohortBudget(expired)
	expired.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget).written = true
	if err := checkLegacyFinancialCohortBudget(expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("admitted writes ignored the hard deadline", err)
	}
}

// Both late controls advance the same clock after the real outcome UPDATE8.
// Only an actual metadata error may roll back those acknowledged writes.
type legacyFinancialCohortBudgetControl struct {
	afterWrite   bool
	failMetadata bool
}

// An ownership reply or the first eight-row update advances the admission
// clock. No query, reply, transaction acknowledgement or hard deadline is faked.
func legacyFinancialCohortRequireBudgetControl(t *testing.T, control legacyFinancialCohortBudgetControl) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, legacyFinancialCohortLimit)
		if control.failMetadata {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION cohort_budget_refuse_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.settled THEN RAISE EXCEPTION 'synthetic cohort budget metadata refusal'; END IF; RETURN NEW; END $$`))
				server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER cohort_budget_refuse_metadata BEFORE UPDATE ON transfer_escrow
 FOR EACH ROW EXECUTE FUNCTION cohort_budget_refuse_metadata()`))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		protocol, cleanup := legacyFinancialRunProtocolBind(t, ctx)
		defer cleanup()
		var advanced atomic.Bool
		var eightRowUpdates atomic.Int64
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
				if !control.afterWrite {
					advanced.Store(true)
				}
			}
		})
		retries := &parallelCloseRetryObservation{}
		observed = retries.context(observed)
		observed, finishPosts := withLegacySettlementPostBatch(observed)
		defer finishPosts()
		ordinary := protocol.proxies[server.DefaultPgVaultResourceName]
		ordinary.commandCompleteObservation.Store(&legacyCohortCommandCompleteObservation{observe: func(tag []byte) {
			if events[server.PgOwnershipAdmitted].Load() > 0 && bytes.Equal(tag, []byte("UPDATE 8\x00")) {
				eightRowUpdates.Add(1)
				if control.afterWrite {
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
		committed := control.afterWrite && !control.failMetadata
		if err != nil || ctx.Err() != nil {
			t.Fatal("budget control lost its live parent", attempts, err, ctx.Err())
		}
		if committed {
			if len(attempts) != len(fixture.ids) {
				t.Fatal("admitted financial work lost its packed cohort", attempts)
			}
			for index, attempt := range attempts {
				if attempt.contractId != fixture.ids[index] || !attempt.completed || attempt.fallback || attempt.deadlineFallback || attempt.busy || attempt.financialWriteRollback {
					t.Fatal("soft boundary repeated admitted financial work", index, attempt)
				}
			}
		} else {
			if len(attempts) != 1 {
				t.Fatal("rolled-back cohort lost its fallback prefix", attempts)
			}
			attempt := attempts[0]
			if attempt.contractId != fixture.ids[0] || !attempt.fallback || attempt.deadlineFallback != !control.failMetadata || attempt.completed || attempt.busy || attempt.financialWriteRollback != control.failMetadata {
				t.Fatal("rollback lost exact failure and write custody", attempt, control)
			}
		}
		if !advanced.Load() || invalidAdmission.Load() || events[server.PgOwnershipAdmitted].Load() != 1 || events[server.PgOwnershipReleased].Load() != 1 ||
			events[server.PgOwnershipWaiting].Load() != 0 || events[server.PgOwnershipRefused].Load() != 0 || events[server.PgOwnershipUncertain].Load() != 0 || retries.callbacks.Load() != 0 {
			t.Fatal("budget control did not confirm actual ownership cleanup", advanced.Load(), invalidAdmission.Load(),
				events[server.PgOwnershipAdmitted].Load(), events[server.PgOwnershipReleased].Load(), events[server.PgOwnershipWaiting].Load(),
				events[server.PgOwnershipRefused].Load(), events[server.PgOwnershipUncertain].Load(), retries.callbacks.Load())
		}
		var updates, commits, writeRollbacks, committedOutcomes int64
		rollbacks := int64(1)
		if committed {
			updates, commits, rollbacks, committedOutcomes = 2, 1, 0, int64(len(fixture.ids))
		} else if control.failMetadata {
			updates, writeRollbacks = 1, 1
		}
		if eightRowUpdates.Load() != updates || wire[server.DefaultPgVaultResourceName]["begin_commands_observed"] != 1 ||
			wire[server.DefaultPgVaultResourceName]["rollback_commands_observed"] != rollbacks || wire[server.DefaultPgVaultResourceName]["commit_commands_observed"] != commits ||
			wire[server.DefaultPgVaultResourceName]["transactions_with_writes_rolled_back"] != writeRollbacks ||
			wire[server.DefaultPgVaultResourceName]["transactions_with_writes_committed"] != commits {
			t.Fatal("budget control lacks its exact write and transaction replies", eightRowUpdates.Load(), control, wire)
		}
		for route, counters := range wire {
			for name, count := range counters {
				if strings.HasPrefix(name, "error_response_") {
					expected := int64(0)
					if control.failMetadata && route == server.DefaultPgVaultResourceName && name == "error_response_other" {
						expected = 1
					}
					if count != expected {
						t.Fatal("budget control saw an unexpected PostgreSQL error", route, name, count, expected)
					}
				}
				if (name == "connections_closed_after_begin_without_end_command" || name == "connections_closed_with_active_writes") && count != 0 {
					t.Fatal("budget control lost transaction cleanup", route, name, count)
				}
			}
		}
		if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(committedOutcomes) || before.Uncertain != after.Uncertain || before.Untracked != after.Untracked {
			t.Fatal("budget control escaped committed outcome custody", before, after, committedOutcomes)
		}
		finishPosts()
		if !committed {
			legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
			if legacyTargetMirrorQueueCount(ctx) != 0 {
				t.Fatal("rolled-back cohort retained a mirror producer")
			}
			if control.failMetadata {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER cohort_budget_refuse_metadata ON transfer_escrow`))
				}, server.TxReadCommitted, server.OptNoRetry())
			}
			for _, id := range fixture.ids {
				completed, busy, _, err := flushLegacySettlement(ctx, id)
				if err != nil || !completed || busy {
					t.Fatal("ordinary singleton did not recover the retained outcome", id, completed, busy, err)
				}
			}
		}
		completed := legacyFinancialCohortCompleted(fixture.ids)
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		if replay, err := FlushLegacyPayerSettlements(ctx, fixture.payer.sourceNetworkId, nil, legacyFinancialCohortLimit); err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.BusyOrGone != 0 || replay.Failed != 0 || replay.More {
			t.Fatal("budget control replay repeated financial work", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		final := contractClosedCounter.Snapshot()
		if !final.Stable || final.Confirmed-before.Confirmed != uint64(len(fixture.ids)) || final.Uncertain != before.Uncertain || final.Untracked != before.Untracked {
			t.Fatal("budget control lost exact per-contract commit custody", before, final)
		}
		t.Logf("cohort_budget_control after_outcome_write=%t metadata_failure=%t packed_committed=%t eight_row_updates=%d financial_write_rollbacks=%d protocol=%v; intentional refusal/failure arms are accounting controls",
			control.afterWrite, control.failMetadata, committed, eightRowUpdates.Load(), writeRollbacks, wire)
	})
}

// A real admitted key set is released by acknowledged rollback before any
// financial mutation, with all original intents available to the singleton.
func TestLegacyFinancialCohortBudgetHandoffAfterAdmission(t *testing.T) {
	legacyFinancialCohortRequireBudgetControl(t, legacyFinancialCohortBudgetControl{})
}

// Acknowledged outcome writes spend the reserved half of the same hard budget.
// The soft boundary cannot replace one packed commit with singleton retries.
func TestLegacyFinancialCohortBudgetAfterOutcomeWriteCommitsPackedEight(t *testing.T) {
	legacyFinancialCohortRequireBudgetControl(t, legacyFinancialCohortBudgetControl{afterWrite: true})
}

// A real statement failure after acknowledged outcomes still rolls back all
// eight owners, and ordinary singletons recover their exact original accounting.
func TestLegacyFinancialCohortBudgetMetadataFailureRetainsEveryOwner(t *testing.T) {
	legacyFinancialCohortRequireBudgetControl(t, legacyFinancialCohortBudgetControl{afterWrite: true, failMetadata: true})
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
