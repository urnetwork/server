// The queue head is one payer's run of queued closes, so every claim at the
// head refuses on that payer's held grant. Those claims must still retire
// other owners' due closes, or the fleet closes one owner at a time.
package work

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Holds one payer's grant owner, as the head owner batch does, until release.
func holdScheduledCloseOwner(t testing.TB, ctx context.Context, payer *scheduledCloseOwnerPayer) (release func()) {
	t.Helper()
	admitted, released, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		server.OwnedTx(ctx, []server.PgOwnershipKey{server.NewPgOwnershipKey("transfer_balance", payer.balanceId)}, func(tx server.PgTx) {
			close(admitted)
			<-released
		}, server.TxReadCommitted, server.OptNoRetry())
	}()
	<-admitted
	return func() {
		close(released)
		<-done
	}
}

// The stored result fields of each finished queued close, by contract.
func readScheduledCloseResultFields(t testing.TB, ctx context.Context) map[server.Id]map[string]any {
	t.Helper()
	fields := map[server.Id]map[string]any{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT args_json,result_json FROM finished_task WHERE function_name=$1`, scheduledCloseFunctionName)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var argsJson, resultJson string
				server.Raise(rows.Scan(&argsJson, &resultJson))
				var args CloseScheduledContractArgs
				server.Raise(json.Unmarshal([]byte(argsJson), &args))
				result := map[string]any{}
				server.Raise(json.Unmarshal([]byte(resultJson), &result))
				fields[args.ContractId] = result
			}
		})
	})
	return fields
}

// Every queued close of the payer is still pending with its own deadline.
func requireScheduledClosesQueued(t testing.TB, ctx context.Context, payer *scheduledCloseOwnerPayer, deadline time.Time) {
	t.Helper()
	queued := 0
	for _, row := range readExpiryRecoveryQueue(t, ctx) {
		if row.function != scheduledCloseFunctionName {
			continue
		}
		var args CloseScheduledContractArgs
		server.Raise(json.Unmarshal([]byte(row.args), &args))
		if !slices.Contains(payer.contractIds, args.ContractId) {
			continue
		}
		if !args.Deadline.Equal(deadline) {
			t.Fatal("queued close lost its deadline", args.ContractId, args.Deadline, deadline)
		}
		queued++
	}
	if queued != len(payer.contractIds) {
		t.Fatal("payer queued closes were lost", queued, len(payer.contractIds))
	}
}

// Production shape: the queue head is a held payer's run, two other payers
// with a full owner batch each are queued behind it, and a fourth payer's
// contracts carry a stored deadline that has not passed. Three claims at the head all refuse on the held
// grant. Each refusal now closes one other owner's due batch, so the three
// claims retire both owners' contracts with exact accounting, two owner turns
// each, while the held payer and the not-yet-due payer stay untouched and
// every refused close keeps its deadline. On the unchanged worker the same
// three claims retire nothing.
func TestScheduledContractCloseHeadRefusalsCloseOtherOwners(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const headCount = 3
		f := newScheduledCloseOwnerFixture(t.Context())
		head := f.newPayer(t, t.Context(), "synthetic-steal-head-payer")
		f.addLegacyContracts(t.Context(), head, headCount)
		behind := []*scheduledCloseOwnerPayer{
			f.newPayer(t, t.Context(), "synthetic-steal-behind-payer-0"),
			f.newPayer(t, t.Context(), "synthetic-steal-behind-payer-1"),
		}
		for _, payer := range behind {
			f.addLegacyContracts(t.Context(), payer, model.ContractDeadlineOwnerBatchLimit)
		}
		future := f.newPayer(t, t.Context(), "synthetic-steal-future-payer")
		f.addLegacyContracts(t.Context(), future, 8)
		futureDeadline := server.NowUtc().Truncate(time.Microsecond).Add(time.Hour)
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1)`,
				future.contractIds, futureDeadline))
		})
		events := newScheduledCloseOwnerEvents(append([]*scheduledCloseOwnerPayer{head, future}, behind...)...)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)

		release := holdScheduledCloseOwner(t, t.Context(), head)
		defer release()
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		for range headCount {
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || len(retried) != 0 || len(posts) != 0 {
				t.Fatal("head claim failed instead of deferring", finished, retried, posts, err)
			}
		}

		results := readScheduledCloseResultFields(t, ctx)
		for _, contractId := range head.contractIds {
			if _, deferred := results[contractId]["deferred_until"]; !deferred {
				t.Fatal("head claim did not refuse on the held grant", contractId, results[contractId])
			}
		}
		if len(results) != headCount {
			t.Fatal("the claims were not the queue head", len(results))
		}
		closed := 0
		for _, payer := range behind {
			server.Db(ctx, func(conn server.PgConn) {
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NOT NULL`,
					payer.contractIds).Scan(&count))
				closed += count
			})
		}
		if want := len(behind) * model.ContractDeadlineOwnerBatchLimit; closed != want {
			t.Fatalf("%d refused head claims closed %d contracts of other owners; want %d", headCount, closed, want)
		}
		for _, payer := range behind {
			requireScheduledCloseOwnerSettled(t, ctx, payer, model.ContractDeadlineOwnerBatchLimit)
			admitted, waiting, refused := events.counts(payer.balanceId)
			if admitted != 2 || waiting != 0 || refused != 0 {
				t.Fatal("other owner did not close through one owner batch", admitted, waiting, refused)
			}
		}
		for _, payer := range []*scheduledCloseOwnerPayer{head, future} {
			requireScheduledCloseOwnerSettled(t, ctx, payer, 0)
		}
		if admitted, waiting, refused := events.counts(head.balanceId); admitted != 0 || waiting != 0 || refused != headCount {
			t.Fatal("held head owner was admitted or retried", admitted, waiting, refused)
		}
		if admitted, waiting, refused := events.counts(future.balanceId); admitted != 0 || waiting != 0 || refused != 0 {
			t.Fatal("an owner whose closes are not due was admitted", admitted, waiting, refused)
		}
		requireScheduledClosesQueued(t, ctx, head, deadline)
		// A stored deadline closes through the deadline pass, not a queued close.
		for contractId := range readQueuedScheduledCloseContracts(t, ctx) {
			if slices.Contains(future.contractIds, contractId) {
				t.Fatal("a contract with a stored deadline was queued a close")
			}
		}
	})
}

// A retained queued close whose contract another closer already settled
// finishes from one unlocked read. That claim now also closes another owner's
// due batch; on the unchanged worker it retires nothing.
func TestScheduledContractCloseTerminalClaimClosesAnotherOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		settled := f.newPayer(t, t.Context(), "synthetic-steal-settled-payer")
		f.addLegacyContracts(t.Context(), settled, 1)
		behind := f.newPayer(t, t.Context(), "synthetic-steal-terminal-behind-payer")
		f.addLegacyContracts(t.Context(), behind, model.ContractDeadlineOwnerBatchLimit)
		events := newScheduledCloseOwnerEvents(settled, behind)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)
		if closed, err := model.ReconcileContractAtDeadline(t.Context(), settled.contractIds[0], deadline); err != nil || closed.AlreadyClosed {
			t.Fatal("fixture did not settle the head contract before its queued close", err)
		}
		requeueRetiredScheduledClose(t, ctx, settled.contractIds[0], deadline)

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("terminal head claim failed", finished, retried, posts, err)
		}
		results := readScheduledCloseResultFields(t, ctx)
		reconciliation, _ := results[settled.contractIds[0]]["reconciliation"].(map[string]any)
		if len(results) != 1 || reconciliation["already_closed"] != true {
			t.Fatal("the claim was not the terminal queue head", results)
		}
		requireScheduledCloseOwnerSettled(t, ctx, settled, 1)
		requireScheduledCloseOwnerSettled(t, ctx, behind, model.ContractDeadlineOwnerBatchLimit)
		if admitted, waiting, refused := events.counts(behind.balanceId); admitted != 2 || waiting != 0 || refused != 0 {
			t.Fatal("other owner did not close through one owner batch", admitted, waiting, refused)
		}
	})
}
