// A sweep page runs at most its owner batch limit at once, and a zero limit
// turns owner batches off so every due row is visited alone.
package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The context lowers the compiled 4 or turns batching off; any other value
// keeps the compiled limit.
func TestForceCloseOwnerBatchParallelOnlyLowersOrTurnsOff(t *testing.T) {
	if ForceCloseOwnerBatchParallel != 4 {
		t.Fatalf("compiled owner batch limit = %d, want 4", ForceCloseOwnerBatchParallel)
	}
	ctx := context.Background()
	cases := []struct {
		ctx  context.Context
		want int
	}{
		{ctx: ctx, want: ForceCloseOwnerBatchParallel},
		{ctx: WithForceCloseOwnerBatchParallel(ctx, 0), want: 0},
		{ctx: WithForceCloseOwnerBatchParallel(ctx, 1), want: 1},
		{ctx: WithForceCloseOwnerBatchParallel(ctx, 3), want: 3},
		{ctx: WithForceCloseOwnerBatchParallel(ctx, 4), want: 4},
		{ctx: WithForceCloseOwnerBatchParallel(ctx, -1), want: ForceCloseOwnerBatchParallel},
		{ctx: WithForceCloseOwnerBatchParallel(ctx, 5), want: ForceCloseOwnerBatchParallel},
	}
	for index, c := range cases {
		if parallel := forceCloseOwnerBatchParallel(c.ctx); parallel != c.want {
			t.Errorf("case %d: owner batch limit = %d, want %d", index, parallel, c.want)
		}
	}
}

// With owner batches off, one balance's due rows close one owned turn each,
// as before owner batches.
func TestForceClosePageClosesRowsAloneWithOwnerBatchesOff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, 3, now)
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		closed, cursor, _, err := forceCloseOpenContractIdsPage(WithForceCloseOwnerBatchParallel(observed, 0), now, 8, 4, 0, 0, nil)
		if err != nil || closed != int64(len(contractIds)) || cursor != nil {
			t.Fatal("the page did not close every row with owner batches off", closed, cursor, err)
		}
		admitted, refused := turns.snapshot()
		if len(admitted) != len(contractIds) || len(refused) != 0 {
			t.Fatalf("the page took %d owned turns and %d refusals, want one turn per row", len(admitted), len(refused))
		}
		for _, members := range admitted {
			if len(members) != 1 {
				t.Fatal("an owned turn covered several rows with owner batches off", len(members))
			}
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
	})
}

// Two payers' balances each hold one batch, and the limit is 1. The first
// admitted batch holds its turn until the other batch waits for a slot, so a
// second batch admitted alongside it is seen; at most one batch is ever
// admitted at once, and both close.
func TestForceClosePageRunsAtMostItsOwnerBatchLimitAtOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		now := server.NowUtc()
		fixtures := []netEscrowOrderingTestFixture{newNetEscrowOrderingTestFixture(t, ctx), newNetEscrowOrderingTestFixture(t, ctx)}
		contractIds := []server.Id{}
		for _, f := range fixtures {
			contractIds = append(contractIds, newForceCloseOwnerBatchRows(t, ctx, f, 2, now)...)
		}
		keyContractIds := map[server.PgOwnershipKey]server.Id{}
		for _, contractId := range contractIds {
			keyContractIds[task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))] = contractId
		}
		batchCovered := func(event server.PgOwnershipEvent) int {
			covered := 0
			for _, key := range event.Keys {
				if _, ok := keyContractIds[key]; ok {
					covered += 1
				}
			}
			return covered
		}

		slotWaited, secondAdmitted := make(chan struct{}), make(chan struct{})
		var slotWaitedOnce, secondAdmittedOnce sync.Once
		var stateLock sync.Mutex
		admittedCount, activeCount, maxActiveCount := 0, 0, 0
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if batchCovered(event) < 2 {
				return
			}
			first := false
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				switch event.Kind {
				case server.PgOwnershipAdmitted:
					admittedCount += 1
					activeCount += 1
					maxActiveCount = max(maxActiveCount, activeCount)
					first = admittedCount == 1
				case server.PgOwnershipReleased, server.PgOwnershipUncertain:
					activeCount -= 1
				}
			}()
			if event.Kind != server.PgOwnershipAdmitted {
				return
			}
			if !first {
				secondAdmittedOnce.Do(func() { close(secondAdmitted) })
				return
			}
			// Hold the first batch until the other waits for its slot, or
			// is admitted beside it.
			select {
			case <-slotWaited:
			case <-secondAdmitted:
			case <-ctx.Done():
			}
		})
		callCtx := context.WithValue(WithForceCloseOwnerBatchParallel(observed, 1), forceCloseOwnerBatchSlotWaitKey{}, func() {
			slotWaitedOnce.Do(func() { close(slotWaited) })
		})

		closed, cursor, _, err := forceCloseOpenContractIdsPage(callCtx, now, 8, 2, 0, 0, nil)
		if err != nil || closed != int64(len(contractIds)) || cursor != nil {
			t.Fatal("the page did not close both batches", closed, cursor, err)
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			if maxActiveCount != 1 || admittedCount != 2 {
				t.Fatalf("%d batches were admitted at once, %d in all; want at most 1 at once and 2 in all", maxActiveCount, admittedCount)
			}
		}()
		select {
		case <-slotWaited:
		default:
			t.Fatal("the second batch never waited for a slot")
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true, true})
	})
}
