// Scheduled children make retained intents selectable in their owned handoff.
package work

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func TestScheduledContractClosureRegistersAndSettlesRetainedPaidIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		payer, source, provider, destination := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, payer, "synthetic-exact-close-payer", server.NewId())
		model.Testing_CreateDevice(ctx, payer, server.NewId(), source, "synthetic-source", "synthetic")
		model.Testing_CreateNetwork(ctx, provider, "synthetic-exact-close-provider", server.NewId())
		model.Testing_CreateDevice(ctx, provider, server.NewId(), destination, "synthetic-provider", "synthetic")
		server.Raise(model.AddBasicTransferBalance(ctx, payer, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		balances := model.GetActiveTransferBalances(ctx, payer)
		if len(balances) != 1 {
			t.Fatal("public exact-close fixture did not create one actual grant")
		}
		balance := balances[0].BalanceId
		id, _, err := model.CreateContract(ctx, payer, source, provider, destination, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, source, 17, true))
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
				VALUES($1,$2,'settled',$3)`, id, int(id[15])%model.LegacySettlementShardCount, deadline))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
				SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, id))
			var exact bool
			server.Raise(tx.QueryRow(ctx, `SELECT c.outcome IS NULL AND c.expiration_time IS NULL
				AND i.payer_network_id IS NULL AND i.source_client_id IS NULL AND i.next_attempt_time=$2
				AND e.redis_reserved AND NOT e.settled AND e.balance_byte_count=100
				FROM transfer_contract c JOIN legacy_settlement_intent i USING(contract_id)
				JOIN transfer_escrow e USING(contract_id) WHERE c.contract_id=$1`, id, deadline).Scan(&exact))
			if !exact {
				t.Fatal("retained paid fixture did not preserve due missing hints and real reservation")
			}
		})
		read := func() (raw string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(c.outcome,c.usage_unverified,c.provider_usage,
					(SELECT to_jsonb(i)-'payer_network_id'-'source_client_id' FROM legacy_settlement_intent i WHERE contract_id=$1),
					(SELECT jsonb_agg(to_jsonb(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1),
					(SELECT jsonb_agg(to_jsonb(e) ORDER BY balance_id) FROM transfer_escrow e WHERE contract_id=$1),
					(SELECT to_jsonb(b) FROM transfer_balance b WHERE balance_id=$2))::text
					FROM transfer_contract c WHERE contract_id=$1`, id, balance).Scan(&raw))
			})
			return
		}
		before := read()
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		key := task.RunOnce("close_scheduled_contract", id)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			scheduleContractClose(client, tx, &CloseScheduledContractArgs{Private: true,
				ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}})
		}, server.TxReadCommitted, server.OptNoRetry())
		child := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer child.Close()
		evalStartupClosureTask(t, ctx, child, readExpiryRecoveryQueue(t, ctx)[key.String()].id)
		server.Db(ctx, func(conn server.PgConn) {
			var registered bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NOT DISTINCT FROM $2::uuid
				AND source_client_id IS NOT DISTINCT FROM $3::uuid
				FROM legacy_settlement_intent WHERE contract_id=$1`, id, payer, source).Scan(&registered))
			if !registered {
				t.Fatal("completed close child published an owner whose retained intent remained invisible")
			}
		})
		queue := readExpiryRecoveryQueue(t, ctx)
		payerTask, found := queue[task.RunOnce("flush_legacy_payer_settlements", payer).String()]
		if !found || read() != before || model.Testing_NetEscrowByteCount(ctx, balance) != 100 {
			t.Fatal("registration changed accepted authority, financial state or lost its actual payer task")
		}
		// No compatibility dispatcher executes. Run the published payer and its
		// ordinary debit/provider/mirror owners through their real finalizers.
		shard := int(balance[15]) % model.TransferDebitShardCount
		debitKey := task.RunOnce(fmt.Sprintf("flush_transfer_debits_%d", shard))
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(debitKey)}, func(tx server.PgTx) {
			task.ScheduleTaskInTx(tx, FlushTransferDebits, &FlushTransferDebitsArgs{Shard: shard}, client,
				debitKey, task.RunAt(deadline), task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
		}, server.TxReadCommitted, server.OptNoRetry())
		finance := startupClosureWorker(ctx, model.NewLegacyPayerSettlementTaskTarget(), NewTransferDebitTaskTarget(),
			model.NewLegacyProviderTotalsTaskTarget(), model.NewLegacyNetEscrowMirrorTaskTarget())
		defer finance.Close()
		names := []string{model.NewLegacyPayerSettlementTaskTarget().TargetFunctionName(), NewTransferDebitTaskTarget().TargetFunctionName(),
			model.NewLegacyProviderTotalsTaskTarget().TargetFunctionName(), model.NewLegacyNetEscrowMirrorTaskTarget().TargetFunctionName()}
		completed, payerFinished := false, false
		for turn := 0; turn < 12 && !completed; turn++ {
			// Advance only this finite fixture's pending owner deadlines. Every
			// task body, financial transaction and completion path remains real.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2
					WHERE function_name=ANY($1)`, names, deadline))
			})
			finished, retried, posts, err := finance.EvalTasks(4)
			if err != nil || len(retried)+len(posts) != 0 {
				t.Fatal("registered paid intent required a failed financial task", err)
			}
			for _, finishedId := range finished {
				payerFinished = payerFinished || finishedId == payerTask.id
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT c.outcome='settled'
					AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
					AND (SELECT count(*)=1 AND bool_and(settled AND payout_byte_count=17) FROM transfer_escrow WHERE contract_id=$1)
					AND COALESCE((SELECT balance_byte_count=983 FROM transfer_balance WHERE balance_id=$2),false)
					AND (SELECT COALESCE(sum(payout_byte_count),0)=17 FROM transfer_escrow_sweep WHERE contract_id=$1)
					AND COALESCE((SELECT provided_byte_count=17 FROM account_balance WHERE network_id=$3),false)
					AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
					AND NOT EXISTS(SELECT 1 FROM pending_task WHERE function_name=ANY($4))
					FROM transfer_contract c WHERE contract_id=$1`, id, balance, provider,
					names[2:]).Scan(&completed))
			})
		}
		if !completed || !payerFinished || model.Testing_NetEscrowByteCount(ctx, balance) != 0 {
			t.Fatal("exact child handoff did not finish real paid accounting and reservation release", completed, payerFinished)
		}
	})
}

// A real child body caps the deadline while an independent owner holds only I.
// Its Post must commit a new child, never wait on I or lose the sole wake.
func TestScheduledContractClosureBusyIntentKeepsOrdinarySuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 30*time.Second)
		defer cancel()
		network, source, destination := newStartupClosureFreeClients(ctx)
		id, err := model.CreateContractNoEscrow(ctx, network, source, network, destination, 100)
		server.Raise(err)
		server.Raise(model.CloseContract(ctx, id, source, 17, true))
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(-time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
				VALUES($1,$2,'settled',$3)`, id, int(id[15])%model.LegacySettlementShardCount, deadline))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, id))
		})
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		key := task.RunOnce("close_scheduled_contract", id)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			scheduleContractClose(client, tx, &CloseScheduledContractArgs{Private: true,
				ScheduledContractClose: ScheduledContractClose{ContractId: id, Deadline: deadline}})
		}, server.TxReadCommitted, server.OptNoRetry())
		initial := readExpiryRecoveryQueue(t, ctx)[key.String()]
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer conn.Release()
		server.RaisePgResult(conn.Exec(ctx, `SET default_transaction_read_only=off`))
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		var locked server.Id
		server.Raise(held.QueryRow(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, id).Scan(&locked))
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		start := server.NowUtc()
		evalStartupClosureTask(t, ctx, worker, initial.id)
		end := server.NowUtc()
		queue := readExpiryRecoveryQueue(t, ctx)
		next, found := queue[key.String()]
		if !found || next.id == initial.id || next.args != initial.args || next.runAt.Before(start.Add(2*time.Second)) || next.runAt.After(end.Add(2*time.Second)) {
			t.Fatal("busy intent lost the ordinary successor or changed its accepted deadline")
		}
		if _, found := queue[task.RunOnce("flush_legacy_source_settlements", source).String()]; found {
			t.Fatal("busy intent published a source owner before registration")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id IS NULL
				FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&untouched))
			if !untouched {
				t.Fatal("busy intent routing was edited outside its owner")
			}
		})
		server.Raise(held.Rollback(ctx))
		// Cross the successor's eligibility without changing the original
		// retirement deadline; the next normal child commits exact registration.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, next.id, deadline))
		})
		evalStartupClosureTask(t, ctx, worker, next.id)
		queue = readExpiryRecoveryQueue(t, ctx)
		if _, found := queue[key.String()]; found {
			t.Fatal("released intent retained an unnecessary child successor")
		}
		if _, found := queue[task.RunOnce("flush_legacy_source_settlements", source).String()]; !found {
			t.Fatal("released intent did not receive its actual source wake")
		}
	})
}
