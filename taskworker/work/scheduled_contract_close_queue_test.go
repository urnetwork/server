// The startup pass queues closes only for legacy contracts and never rewrites a
// queued close; a deferral queues no successor for a contract that closed.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Recreates the queued close of a contract another closer already retired, as
// the queue retained them before closers removed them: with its deadline and
// a wake a minute earlier, so it leads the queue as those rows did on Main.
func requeueRetiredScheduledClose(t testing.TB, ctx context.Context, contractId server.Id, deadline time.Time) {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(model.ScheduledContractCloseRunOnce(contractId))}, func(tx server.PgTx) {
		task.ScheduleTaskInTx(tx, CloseScheduledContract, &CloseScheduledContractArgs{Private: true,
			ScheduledContractClose: ScheduledContractClose{ContractId: contractId, Deadline: deadline}}, owner,
			model.ScheduledContractCloseRunOnce(contractId), task.RunAt(deadline.Add(-time.Minute)),
			task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Open contracts minted a millisecond apart from mintedAt, each with the given
// stored deadline (nil for legacy rows), in id order.
func newStartupClosureMintedContracts(ctx context.Context, networkId, sourceId, destinationId server.Id,
	mintedAt time.Time, count int, deadline *time.Time) []server.Id {
	ids := make([]server.Id, count)
	for index := range ids {
		ids[index] = contractIdMintedAt(mintedAt.Add(time.Duration(index) * time.Millisecond))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
			SELECT contract_id,$2,$3,$2,$4,100,true,$5,$6 FROM unnest($1::uuid[]) AS candidate(contract_id)`,
			ids, networkId, sourceId, destinationId, mintedAt, deadline))
	})
	return ids
}

// One complete startup pass at startedAt with the given page size.
func runStartupContractClosurePass(t testing.TB, ctx context.Context, startedAt time.Time, pageSize int) {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	if _, err := ScheduleOpenContractClosures(&ScheduleOpenContractClosuresArgs{PageSize: pageSize, StartedAt: startedAt}, owner); err != nil {
		t.Fatal("startup pass failed", err)
	}
}

// The contracts with a queued close.
func readQueuedScheduledCloseContracts(t testing.TB, ctx context.Context) map[server.Id]expiryRecoveryQueued {
	t.Helper()
	queued := map[server.Id]expiryRecoveryQueued{}
	for _, row := range readExpiryRecoveryQueue(t, ctx) {
		if row.function != scheduledCloseFunctionName {
			continue
		}
		deadline, ok := scheduledCloseDeadline(row.function, row.args, row.runAt)
		if !ok {
			t.Fatal("queued close lost its arguments", row.args)
		}
		queued[deadline.ContractId] = row
	}
	return queued
}

// A contract issued with a stored deadline is closed at it by the deadline
// pass; a queued close for it only waited behind the legacy backlog. The pass
// queued one for every open contract, and once it reached the contracts issued
// with a stored deadline it would have queued one per new contract.
func TestStartupContractClosurePublishesOnlyLegacyContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-3 * time.Hour)
		past := mintedAt.Add(model.DefaultContractExpiration)
		future := server.NowUtc().Truncate(time.Microsecond).Add(time.Hour)
		legacy := newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt, 3, nil)
		stored := newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt.Add(time.Second), 3, &past)
		stored = append(stored, newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt.Add(2*time.Second), 3, &future)...)
		legacy = append(legacy, newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt.Add(3*time.Second), 3, nil)...)

		startedAt := server.NowUtc().Truncate(time.Microsecond)
		runStartupContractClosurePass(t, ctx, startedAt, 2)
		queued := readQueuedScheduledCloseContracts(t, ctx)
		for _, id := range legacy {
			row, found := queued[id]
			deadline, _ := scheduledCloseDeadline(row.function, row.args, row.runAt)
			if !found || !deadline.Deadline.Equal(startedAt.Add(model.DefaultContractExpiration)) {
				t.Fatal("legacy contract lost its queued close or its pass deadline", found, deadline.Deadline)
			}
		}
		for _, id := range stored {
			if _, found := queued[id]; found {
				t.Fatal("a contract with a stored deadline was queued a close")
			}
		}
		if len(queued) != len(legacy) {
			t.Fatal("the pass queued closes beyond the legacy contracts", len(queued), len(legacy))
		}
	})
}

// A later pass finds every legacy contract it queued before. It inserts the
// missing one and leaves each queued close as it was: the same tuple, the same
// wake generation and a deferral's later wake. The merge rewrote every row and
// advanced its generation, so a row its claim was closing woke a successor.
func TestStartupContractClosureLeavesQueuedClosesUntouched(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-3 * time.Hour)
		first := newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt, 4, nil)
		startedAt := server.NowUtc().Truncate(time.Microsecond).Add(-2 * time.Hour)
		runStartupContractClosurePass(t, ctx, startedAt, startupContractClosurePageSize)

		// A held owner deferred the last close past the backlog.
		deferredUntil := server.NowUtc().Truncate(time.Microsecond).Add(time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE run_once_key=$1`,
				model.ScheduledContractCloseRunOnce(first[3]).String(), deferredUntil))
		})
		type tupleState struct {
			xmin       string
			generation int64
			runAt      time.Time
		}
		readTuples := func(ids []server.Id) map[server.Id]tupleState {
			states := map[server.Id]tupleState{}
			server.Db(ctx, func(conn server.PgConn) {
				for _, id := range ids {
					var state tupleState
					server.Raise(conn.QueryRow(ctx, `SELECT xmin::text,run_once_generation,run_at FROM pending_task WHERE run_once_key=$1`,
						model.ScheduledContractCloseRunOnce(id).String()).Scan(&state.xmin, &state.generation, &state.runAt))
					states[id] = state
				}
			})
			return states
		}
		before := readTuples(first)
		added := newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt.Add(time.Second), 1, nil)
		runStartupContractClosurePass(t, ctx, startedAt.Add(time.Hour), startupContractClosurePageSize)
		after := readTuples(first)
		for _, id := range first {
			if after[id].xmin != before[id].xmin || after[id].generation != before[id].generation || !after[id].runAt.Equal(before[id].runAt) {
				t.Fatal("a later pass rewrote a queued close", before[id], after[id])
			}
		}
		if !after[first[3]].runAt.Equal(deferredUntil) {
			t.Fatal("a later pass moved a deferred close")
		}
		queued := readQueuedScheduledCloseContracts(t, ctx)
		if _, found := queued[added[0]]; !found || len(queued) != len(first)+1 {
			t.Fatal("a later pass did not queue the new legacy contract", len(queued))
		}
	})
}

// Contracts minted after the stored deadline rollout all carry one, so a run of
// full pages without a legacy contract ends the pass instead of reading every
// open contract minted since. Fewer such pages keep it walking.
func TestStartupContractClosureEndsAfterLegacyFreePages(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		const pageSize = 2
		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-3 * time.Hour)
		deadline := mintedAt.Add(model.DefaultContractExpiration)
		mint := func(offset time.Duration, count int, stored bool) []server.Id {
			var storedDeadline *time.Time
			if stored {
				storedDeadline = &deadline
			}
			return newStartupClosureMintedContracts(ctx, networkId, sourceId, destinationId, mintedAt.Add(offset), count, storedDeadline)
		}
		// Pages: [L0 S], one page short of the limit without a legacy contract,
		// [L1 S], then the limit's pages without one, and L2 after them.
		first := mint(0, 1, false)
		mint(time.Second, 1+pageSize*(startupContractClosureLegacyFreePageLimit-1), true)
		second := mint(2*time.Second, 1, false)
		mint(3*time.Second, 1+pageSize*startupContractClosureLegacyFreePageLimit, true)
		straggler := mint(4*time.Second, 1, false)

		runStartupContractClosurePass(t, ctx, server.NowUtc().Truncate(time.Microsecond), pageSize)
		queued := readQueuedScheduledCloseContracts(t, ctx)
		if _, found := queued[first[0]]; !found {
			t.Fatal("the first legacy contract lost its queued close")
		}
		if _, found := queued[second[0]]; !found {
			t.Fatal("fewer legacy-free pages than the limit ended the pass")
		}
		if _, found := queued[straggler[0]]; found {
			t.Fatal("the pass read past the legacy-free pages")
		}
		if len(queued) != 2 {
			t.Fatal("the pass queued closes for contracts with a stored deadline", len(queued))
		}
	})
}

// A close deferred on a held owner, or claimed before its deadline, wakes a
// successor only while its contract is open. The held owner's own batch
// usually closed it, and its successor only finished as a terminal no-op.
func TestScheduledContractCloseDeferralOfARetiredContractQueuesNoSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-retired-deferral-payer")
		ids := f.addLegacyContracts(t.Context(), payer, 4)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(-time.Hour)
		for _, id := range []server.Id{ids[0], ids[2]} {
			if closed, err := model.ReconcileContractAtDeadline(ctx, id, deadline); err != nil || closed.AlreadyClosed {
				t.Fatal("fixture did not close the contract", err)
			}
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		deferredUntil := server.NowUtc().Truncate(time.Microsecond).Add(scheduledCloseOwnerBusyDeferral)
		later := server.NowUtc().Truncate(time.Microsecond).Add(time.Hour)
		results := []*CloseScheduledContractResult{
			{RetryAt: &deadline, DeferredUntil: &deferredUntil},
			{RetryAt: &deadline, DeferredUntil: &deferredUntil},
			{RetryAt: &later},
			{RetryAt: &later},
		}
		for index, id := range ids {
			args := &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}}
			if index >= 2 {
				args.Deadline = later
			}
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(model.ScheduledContractCloseRunOnce(id))}, func(tx server.PgTx) {
				server.Raise(CloseScheduledContractPost(args, results[index], owner, tx))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		queued := readQueuedScheduledCloseContracts(t, ctx)
		if _, found := queued[ids[0]]; found {
			t.Fatal("a deferral queued a successor for a contract that closed")
		}
		if _, found := queued[ids[2]]; found {
			t.Fatal("an early wake queued a successor for a contract that closed")
		}
		if row, found := queued[ids[1]]; !found || !row.runAt.Equal(deferredUntil) {
			t.Fatal("an open contract's deferral lost its successor", found)
		}
		if row, found := queued[ids[3]]; !found || !row.runAt.Equal(later) {
			t.Fatal("an open contract's early wake lost its successor", found)
		}
	})
}
