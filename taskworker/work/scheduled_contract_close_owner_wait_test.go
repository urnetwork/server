// A queued close refused by a held owner waits in its claim and closes its
// contract, instead of deferring behind every due row of the queue.
package work

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The queue position of a contract's queued close, if one is pending.
func readScheduledCloseAvailableBlock(t testing.TB, ctx context.Context, contractId server.Id) (block int64, queued bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT available_block FROM pending_task WHERE run_once_key=$1`,
			model.ScheduledContractCloseRunOnce(contractId).String())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				server.Raise(rows.Scan(&block))
				queued = true
			}
		})
	})
	return
}

// Production shape on Main on 2026-10-13: one payer grant holds 64 due
// contracts that carry their own deadline and, minted after them, one legacy
// contract whose queued close is its only deadline authority. That close is
// claimed at the queue head, ahead of a due no-op close, while another closer
// holds the grant for the oldest contract. The holder's batch is its primary
// and the 63 oldest due siblings, so the legacy contract is not in it, and no
// later owner batch or steal reaches it. The refused close used to defer to now
// plus the deferral, so its successor woke behind every due row of the queue,
// the no-op close here and on Main about 44k of them, while its contract
// stayed open. It now tries the owner again within its claim and closes the
// contract at its deadline with exact accounting once the holder's batch
// commits, leaving no queued close behind.
func TestScheduledContractCloseRefusedByAHeldOwnerClosesWithinItsClaim(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-held-batch-bound-payer")
		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-4 * time.Hour)
		storedDeadline := mintedAt.Add(model.DefaultContractExpiration)
		storedIds := f.addDeadlineContracts(t.Context(), payer, mintedAt, model.ContractDeadlineOwnerBatchLimit, &storedDeadline)
		legacyId := f.addLegacyContracts(t.Context(), payer, 1)[0]

		// The first admission of the grant is the holder's. It keeps the grant
		// until the legacy close is first refused, and that close tries again
		// only after the holder's batch committed, as it does after a batch of
		// up to a few seconds on Main.
		balanceKey := server.NewPgOwnershipKey("transfer_balance", payer.balanceId)
		holderAdmitted, releaseHolder, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseHolder) }) }
		var stateLock sync.Mutex
		admitted, waiting, refused := 0, 0, 0
		counts := func() (int, int, int) {
			stateLock.Lock()
			defer stateLock.Unlock()
			return admitted, waiting, refused
		}
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), func(event server.PgOwnershipEvent) {
			if event.TransactionScoped || !slices.Contains(event.Keys, balanceKey) {
				return
			}
			first := false
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				switch event.Kind {
				case server.PgOwnershipAdmitted:
					admitted++
					first = admitted == 1
				case server.PgOwnershipWaiting:
					waiting++
				case server.PgOwnershipRefused:
					refused++
					first = refused == 1
				}
			}()
			switch {
			case first && event.Kind == server.PgOwnershipAdmitted:
				close(holderAdmitted)
				<-releaseHolder
			case first && event.Kind == server.PgOwnershipRefused:
				release()
				<-holderDone
			}
		})
		deadline := publishScheduledCloses(t, ctx)
		if queued := readQueuedScheduledCloseContracts(t, ctx); len(queued) != 1 {
			t.Fatal("the startup pass did not queue a close for the legacy contract alone", len(queued))
		}
		// A due close of a contract another closer retired, queued behind the
		// legacy close as the no-op backlog was on Main.
		noopId := server.NewId()
		requeueRetiredScheduledClose(t, ctx, noopId, deadline.Add(31*time.Minute))

		var holder *model.ContractOwnerDeadlineResult
		var holderErr, openErr error
		var openAfterHolder []server.Id
		go func() {
			defer close(holderDone)
			holder, holderErr = model.ReconcileContractOwnerAtDeadline(ctx, []server.Id{payer.balanceId},
				model.ContractDeadline{ContractId: storedIds[0], Deadline: storedDeadline}, dueContractDeadlines(server.NowUtc()))
			openAfterHolder, openErr = model.ReadOpenContractIds(ctx, []server.Id{legacyId})
		}()
		<-holderAdmitted
		defer func() {
			release()
			<-holderDone
		}()

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		release()
		<-holderDone
		if err != nil || len(finished) != 1 || len(retried)+len(posts) != 0 {
			t.Fatal("the legacy contract's queued close failed instead of finishing", finished, retried, posts, err)
		}
		if holderErr != nil || openErr != nil || holder == nil || holder.Busy || holder.Reconciliation == nil ||
			holder.Reconciliation.ContractId != storedIds[0] || holder.Reconciliation.AlreadyClosed ||
			holder.SiblingsClosed != model.ContractDeadlineOwnerBatchLimit-1 {
			t.Fatalf("the holder's batch was not its primary and its %d oldest due siblings: %+v %v %v",
				model.ContractDeadlineOwnerBatchLimit-1, holder, holderErr, openErr)
		}
		if !slices.Contains(openAfterHolder, legacyId) {
			t.Fatal("the holder's batch closed the legacy contract")
		}
		requireContractsSettledAt(t, ctx, storedIds, storedDeadline)
		results := readScheduledCloseResults(t, ctx)
		if len(results) != 1 {
			t.Fatal("the claim was not the legacy contract's close at the queue head", len(results))
		}
		result := results[0]
		if _, _, refusals := counts(); refusals == 0 {
			t.Fatal("the held grant never refused the legacy contract's close")
		}
		if result.DeferredUntil != nil {
			successor, queued := readScheduledCloseAvailableBlock(t, ctx, legacyId)
			noop, _ := readScheduledCloseAvailableBlock(t, ctx, noopId)
			openIds, err := model.ReadOpenContractIds(ctx, []server.Id{legacyId})
			server.Raise(err)
			t.Fatalf("the refused close deferred and its contract stayed open=%v: its successor (queued=%v) wakes at available block %d, %d s behind the due no-op close at %d",
				slices.Contains(openIds, legacyId), queued, successor, successor-noop, noop)
		}
		if closed := result.Reconciliation; closed == nil || closed.ContractId != legacyId || closed.AlreadyClosed || closed.Missing ||
			closed.Outcome != model.ContractOutcomeSettled || result.OwnerBatchClosed != 0 || result.StolenClosed != 0 {
			t.Fatalf("the refused close did not close its own contract: %+v", result)
		}
		if _, queued := readScheduledCloseAvailableBlock(t, ctx, legacyId); queued {
			t.Fatal("the closed legacy contract kept a queued close")
		}
		if _, queued := readScheduledCloseAvailableBlock(t, ctx, noopId); !queued {
			t.Fatal("the no-op close behind the legacy close was claimed")
		}
		requireContractsSettledAt(t, ctx, []server.Id{legacyId}, deadline)
		requireScheduledCloseOwnerSettled(t, ctx, payer, len(payer.contractIds))
		// The holder's two owner turns, then the legacy close's one: it found
		// no due sibling left.
		if admissions, waits, refusals := counts(); admissions != 3 || waits != 0 {
			t.Fatal("the grant was not admitted for the holder's batch and then the legacy close alone", admissions, waits, refusals)
		}
	})
}
