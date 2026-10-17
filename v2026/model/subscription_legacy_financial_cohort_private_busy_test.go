// Private intent/contract owners leave no financial scope to admit. Their
// original per-contract busy results survive until those actual owners retire.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func legacyFinancialCohortRequireAllPrivateBusy(t *testing.T, holdIntents bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyFinancialCohortSeed(t, ctx, 8)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = held.Rollback(cleanup)
		}()
		query := `SELECT contract_id FROM transfer_contract WHERE contract_id=ANY($1) ORDER BY contract_id FOR UPDATE`
		gate := legacySettlementBusyContract
		if holdIntents {
			query = `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=ANY($1) ORDER BY contract_id FOR UPDATE`
			gate = legacySettlementBusyIntent
		}
		locked := map[server.Id]bool{}
		rows, err := held.Query(ctx, query, fixture.ids)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				locked[id] = true
			}
		})
		if len(locked) != len(fixture.ids) {
			t.Fatal("private owner did not lock the exact cohort")
		}
		for _, id := range fixture.ids {
			if !locked[id] {
				t.Fatal("private owner missed a selected contract")
			}
		}
		before := contractClosedCounter.Snapshot()
		attempts, attemptErr := flushLegacySettlementCohort(ctx, fixture.ids)
		afterBusy := contractClosedCounter.Snapshot()
		if !before.Stable || !afterBusy.Stable || before.Confirmed != afterBusy.Confirmed ||
			before.Uncertain != afterBusy.Uncertain || before.Untracked != afterBusy.Untracked {
			t.Fatal("private-row contention changed financial commit custody")
		}
		legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
		if legacyTargetMirrorQueueCount(ctx) != 0 {
			t.Fatal("private-row contention published a mirror owner")
		}
		// Retire the actual row owner before ordinary financial progress.
		// Both source arms must finish the same money/replay oracles before
		// the final busy-versus-fallback classification assertion.
		server.Raise(held.Rollback(ctx))
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		if err != nil || page.Completed != 8 || page.Failed != 0 || page.BusyOrGone != 0 {
			t.Fatal("retired private owner did not restore ordinary settlement", page, err)
		}
		completed := legacyFinancialCohortCompleted(fixture.ids)
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		drain, err := legacyFinancialDrainOwners(t, ctx, 9, nil)
		if err != nil || drain.Finished != 9 {
			t.Fatal("private-row recovery lost a real durable output owner", drain, err)
		}
		requireLegacyOwnedMetadataRedis(t, ctx, fixture.balances[0], 0)
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		if replay, err := FlushLegacySettlements(ctx, 1, nil, 8); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("private-row recovery replay repeated financial ownership", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, completed)
		final := contractClosedCounter.Snapshot()
		if !final.Stable || final.Confirmed-before.Confirmed != 8 || final.Uncertain != before.Uncertain || final.Untracked != before.Untracked {
			t.Fatal("private-row recovery or replay changed exact commit custody")
		}
		t.Logf("cohort_private_busy held_intents=%t attempts=%+v error=%v completed=%d owners=%d", holdIntents, attempts, attemptErr, page.Completed, drain.Finished)
		if attemptErr != nil || len(attempts) != len(fixture.ids) {
			t.Fatal("empty financial ownership scope replaced private busy rows with fallback", attempts, attemptErr)
		}
		for index, attempt := range attempts {
			if attempt.contractId != fixture.ids[index] || attempt.completed || !attempt.busy || attempt.busyGate != gate || attempt.fallback || attempt.deadlineFallback {
				t.Fatal("private owner lost its original per-contract busy classification", index, attempt)
			}
		}
	})
}

func TestLegacyFinancialCohortAllHeldIntentsPreserveBusy(t *testing.T) {
	legacyFinancialCohortRequireAllPrivateBusy(t, true)
}

func TestLegacyFinancialCohortAllHeldContractsPreserveBusy(t *testing.T) {
	legacyFinancialCohortRequireAllPrivateBusy(t, false)
}
