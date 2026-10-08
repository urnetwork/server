// Exercise payer service through the actual recurring task and its durable
// continuation. The dense fixture uses rolling-writer inserts, not backfill.
package work

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A healthy dense payer cannot consume both service opportunities of another.
func TestLegacyPayerFairnessTaskHealthyDensePrefix(t *testing.T) {
	testLegacyPayerFairnessTaskDensePrefix(t, false)
}

// Holding the dense payer's grant must leave an independent payer serviceable.
func TestLegacyPayerFairnessTaskHeldDensePrefix(t *testing.T) {
	testLegacyPayerFairnessTaskDensePrefix(t, true)
}

// Both cases retain ordinary financial owners, exact conservation and replay.
// No intent due tuple may be rewritten to make the target reachable.
func testLegacyPayerFairnessTaskDensePrefix(t *testing.T, holdPrefix bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		ctx = model.Testing_WithLegacyPayerSettlementCollectionWindow(ctx)
		type payerFixture struct {
			sourceNetworkId      server.Id
			sourceId             server.Id
			destinationNetworkId server.Id
			destinationId        server.Id
			balanceId            server.Id
		}
		newPayer := func() payerFixture {
			f := payerFixture{
				sourceNetworkId: server.NewId(), sourceId: server.NewId(),
				destinationNetworkId: server.NewId(), destinationId: server.NewId(),
			}
			for _, client := range []struct{ networkId, clientId server.Id }{
				{networkId: f.sourceNetworkId, clientId: f.sourceId},
				{networkId: f.destinationNetworkId, clientId: f.destinationId},
			} {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id)
						VALUES($1,$2,$3)`, client.networkId, "synthetic-payer-fairness-"+client.networkId.String(), server.NewId()))
				})
				model.Testing_CreateDevice(ctx, client.networkId, server.NewId(), client.clientId, "synthetic-payer-client", "synthetic")
			}
			model.AddBasicTransferBalance(ctx, f.sourceNetworkId, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
			balances := model.GetActiveTransferBalances(ctx, f.sourceNetworkId)
			if len(balances) != 1 {
				t.Fatal("synthetic payer does not own exactly one grant", len(balances))
			}
			f.balanceId = balances[0].BalanceId
			return f
		}
		prefixOwner, targetOwner := newPayer(), newPayer()
		const predecessors = 8193
		const boundedPages = 2
		idPrefix := server.NewId()
		identity := func(sequence uint32) server.Id {
			id := idPrefix
			binary.BigEndian.PutUint32(id[11:15], sequence)
			id[15] = 1
			return id
		}
		oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		prefixIds := make([]server.Id, predecessors)
		prefixTimes := make([]time.Time, predecessors)
		for index := range predecessors {
			prefixIds[index] = identity(uint32(index + 1))
			prefixTimes[index] = oldest.Add(time.Duration(index+1) * time.Millisecond)
		}
		target := identity(100000)
		targetDue := oldest.Add(time.Duration(predecessors+1) * time.Millisecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET
				start_balance_byte_count=20000,balance_byte_count=20000,net_revenue_nano_cents=40000
				WHERE balance_id=$1`, prefixOwner.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, targetOwner.balanceId))
			for _, cohort := range []struct {
				owner payerFixture
				ids   []server.Id
				due   []time.Time
			}{
				{owner: prefixOwner, ids: prefixIds, due: prefixTimes},
				{owner: targetOwner, ids: []server.Id{target}, due: []time.Time{targetDue}},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
					SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS row(id)`,
					cohort.ids, cohort.owner.sourceNetworkId, cohort.owner.sourceId, cohort.owner.destinationNetworkId, cohort.owner.destinationId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					SELECT id,$2,2 FROM unnest($1::uuid[]) AS row(id)`, cohort.ids, cohort.owner.balanceId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS row(id)
					CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, cohort.ids))
				// This is the old writer's column list. The insert trigger must
				// derive payer metadata without changing its due tuple.
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
					SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS row(id,due)`, cohort.ids, cohort.due))
				var keyed bool
				server.Raise(tx.QueryRow(ctx, `SELECT count(*)=$3 AND bool_and(payer_network_id IS NOT DISTINCT FROM $2)
					FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, cohort.ids, cohort.owner.sourceNetworkId, len(cohort.ids)).Scan(&keyed))
				if !keyed {
					t.Fatal("rolling-writer insert did not derive the exact payer")
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION legacy_fairness_test_preserve_due() RETURNS trigger
				LANGUAGE plpgsql AS $body$ BEGIN
				IF NEW.next_attempt_time IS DISTINCT FROM OLD.next_attempt_time THEN
				 RAISE EXCEPTION 'synthetic fairness fixture forbids due rewrite'; END IF;
				RETURN NEW; END $body$;
				CREATE TRIGGER legacy_fairness_test_preserve_due BEFORE UPDATE ON legacy_settlement_intent
				FOR EACH ROW EXECUTE FUNCTION legacy_fairness_test_preserve_due()`))
		})
		model.ReconcileNetEscrowForNetwork(ctx, prefixOwner.sourceNetworkId, true)
		model.ReconcileNetEscrowForNetwork(ctx, targetOwner.sourceNetworkId, true)
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			var due time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent
				WHERE shard=1 AND (next_attempt_time,contract_id)<($1,$2)`, targetDue, target).Scan(&count))
			server.Raise(conn.QueryRow(ctx, `SELECT next_attempt_time FROM legacy_settlement_intent WHERE contract_id=$1`, target).Scan(&due))
			if count != predecessors || !due.Equal(targetDue) {
				t.Fatal("dense predecessor interval differs from the original service regression", count)
			}
		})

		conn, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer conn.Release()
		server.RaisePgResult(conn.Exec(ctx, `SET default_transaction_read_only=off`))
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		if holdPrefix {
			// The returned lock acquisition is the barrier; no sleep chooses
			// whether the financial owner encounters the held grant.
			server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, prefixOwner.balanceId))
		}
		assertTarget := func(settled bool) []byte {
			t.Helper()
			want := 0
			if settled {
				want = 1
			}
			var proof []byte
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM transfer_contract WHERE contract_id=$1 AND outcome='settled' AND NOT open)=$3
					AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1 AND failure_code='none')=1-$3
					AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND settled)=$3
					AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000-$3
					AND (SELECT coalesce(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=$1)=$3
					AND (SELECT coalesce(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=$1)=$3`,
					target, targetOwner.balanceId, want).Scan(&exact))
				if !exact {
					t.Fatal("target outcome, grant debit, intent or payout violated conservation", settled)
				}
				server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, target).Scan(&proof))
			})
			if got := model.Testing_NetEscrowByteCount(ctx, targetOwner.balanceId); got != model.ByteCount(2*(1-want)) {
				t.Fatal("target mirror did not conserve its reservation", got, want)
			}
			if settled {
				var usage struct {
					Version   int   `json:"version"`
					ByteCount int64 `json:"byte_count"`
					Providers []struct {
						ClientId  server.Id `json:"client_id"`
						NetworkId server.Id `json:"network_id"`
						ByteCount int64     `json:"byte_count"`
					} `json:"providers"`
				}
				server.Raise(json.Unmarshal(proof, &usage))
				if usage.Version != 1 || usage.ByteCount != 1 || len(usage.Providers) != 1 ||
					usage.Providers[0].NetworkId != targetOwner.destinationNetworkId ||
					usage.Providers[0].ClientId != targetOwner.destinationId || usage.Providers[0].ByteCount != 1 {
					t.Fatal("ordinary task financial owner changed exact provider usage")
				}
			}
			return proof
		}
		assertTarget(false)
		owner := session.NewLocalClientSession(ctx, "192.0.2.1:0", nil)
		defer owner.Cancel()
		args := FlushLegacySettlementsArgs{Shard: 1}
		visited, completed, busy, payerCompleted, financialPages := 0, 0, 0, 0, 0
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(model.NewLegacyPayerSettlementTaskTarget())
		for range boundedPages {
			before, err := json.Marshal(args)
			server.Raise(err)
			result, err := FlushLegacySettlements(&args, owner)
			if err != nil || result == nil || result.Dispatch == nil || result.Dispatch.RegistrationFailed ||
				result.Dispatch.Probes > 16 || result.Completed != 0 || result.Visited != 0 {
				t.Fatalf("recurring shard did financial work or lost bounded dispatch: %+v err=%v", result, err)
			}
			after, err := json.Marshal(args)
			server.Raise(err)
			if !bytes.Equal(before, after) {
				t.Fatal("dispatcher mutated its input cursor")
			}
			withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key='["flush_legacy_settlements_1"]'`))
				server.Raise(FlushLegacySettlementsPost(&args, result, owner, tx))
			})
			var persisted []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json FROM pending_task
					WHERE run_once_key='["flush_legacy_settlements_1"]'`).Scan(&persisted))
			})
			args = FlushLegacySettlementsArgs{}
			server.Raise(json.Unmarshal(persisted, &args))
			expected, err := json.Marshal(FlushLegacySettlementsArgs{Shard: 1, Cursor: result.Dispatch.Cursor, PayerCursor: result.Dispatch.PayerCursor})
			server.Raise(err)
			restarted, err := json.Marshal(args)
			server.Raise(err)
			if !bytes.Equal(expected, restarted) {
				t.Fatal("dispatch handoff lost a durable cursor")
			}
			var availableBlock int64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT max(available_block) FROM pending_task WHERE function_name=$1`,
					model.NewLegacyPayerSettlementTaskTarget().TargetFunctionName()).Scan(&availableBlock))
			})
			select {
			case <-time.After(time.Until(time.Unix(availableBlock, 0))):
			case <-ctx.Done():
				t.Fatal("bounded fairness owners did not become claim eligible", ctx.Err())
			}
			finished, retried, postRetried, err := worker.EvalTasks(2)
			if err != nil || len(retried)+len(postRetried) != 0 {
				t.Fatal("actual payer task failed", err, len(retried), len(postRetried))
			}
			for _, done := range task.GetFinishedTasks(ctx, finished...) {
				var result model.LegacyPayerSettlementResult
				var scope model.LegacyPayerSettlementArgs
				server.Raise(json.Unmarshal([]byte(done.ResultJson), &result))
				server.Raise(json.Unmarshal([]byte(done.ArgsJson), &scope))
				if result.Pages < 1 || result.Visited > result.Pages*model.LegacySettlementPageLimit || result.Failed != 0 {
					t.Fatal("payer task exceeded its per-page financial bound", result)
				}
				visited += result.Visited
				financialPages += result.Pages
				completed += result.Completed
				busy += result.BusyOrGone
				if scope.PayerNetworkId == targetOwner.sourceNetworkId {
					payerCompleted += result.Completed
				}
			}
		}
		// The exact audit above intentionally left the durable admission
		// snapshot cold. The financial commit must queue its mirror owner;
		// only that separate task may perform the historical census. Execute
		// the real durable handlers twice before checking mirror conservation.
		mirror := task.NewTaskTarget(model.ApplyLegacyNetEscrowMirror)
		mirrorIds := []server.Id{}
		targetMirror := false
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT task_id,(args_json::jsonb->>'balance_id')::uuid FROM pending_task
			 WHERE function_name=$1 AND (args_json::jsonb->>'balance_id')::uuid=ANY($2)
			 ORDER BY task_id LIMIT 3`, mirror.TargetFunctionName(), []server.Id{prefixOwner.balanceId, targetOwner.balanceId})
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var taskId, balanceId server.Id
					server.Raise(rows.Scan(&taskId, &balanceId))
					mirrorIds = append(mirrorIds, taskId)
					targetMirror = targetMirror || balanceId == targetOwner.balanceId
				}
			})
		})
		if !targetMirror || len(mirrorIds) > 2 {
			t.Fatal("cold task settlement did not retain exact durable mirror ownership", len(mirrorIds))
		}
		mirrorOwners := task.GetTasks(ctx, mirrorIds...)
		for _, taskId := range mirrorIds {
			if mirrorOwners[taskId] == nil {
				t.Fatal("cold mirror owner disappeared")
			}
			for range 2 {
				if _, _, err := mirror.RunSpecific(ctx, mirrorOwners[taskId]); err != nil {
					t.Fatal("real cold mirror owner or its replay failed", err)
				}
			}
		}
		proof := assertTarget(true)
		if payerCompleted < 1 || visited > financialPages*model.LegacySettlementPageLimit || completed < 1 ||
			holdPrefix && (completed != 1 || busy == 0) {
			t.Fatal("independent payer was not served within two bounded dispatch pages", visited, completed, busy, payerCompleted)
		}
		replay, err := model.DrainLegacySettlements(ctx, model.LegacySettlementDrainRequest{
			ExpectedPayerNetworkId: targetOwner.sourceNetworkId, ContractIds: []server.Id{target}, Apply: true,
		})
		if err != nil || len(replay.Contracts) != 1 || replay.Contracts[0].FinancialCommitAcknowledged ||
			replay.Contracts[0].Status != "busy_intent_or_absent" || !bytes.Equal(proof, assertTarget(true)) {
			t.Fatal("completed target replay repeated accounting or changed immutable proof", err)
		}
		server.Raise(held.Rollback(ctx))

		projection := task.NewTaskTarget(model.ApplyLegacyProviderTotals)
		taskIds := []server.Id{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT task_id FROM pending_task
				WHERE function_name=$1 AND NOT COALESCE((args_json::jsonb->>'applied')::boolean,false)
				ORDER BY task_id`, projection.TargetFunctionName())
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var taskId server.Id
					server.Raise(rows.Scan(&taskId))
					taskIds = append(taskIds, taskId)
				}
			})
		})
		pending := task.GetTasks(ctx, taskIds...)
		for _, taskId := range taskIds {
			if pending[taskId] == nil {
				t.Fatal("provider projection lost its durable owner")
			}
			for range 2 {
				if _, _, err := projection.RunSpecific(ctx, pending[taskId]); err != nil {
					t.Fatal("ordinary provider projection or its replay failed", err)
				}
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled' AND NOT open)=$3
				AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1))=$4-$3
				AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled)=$3
				AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=20000-$3
				AND (SELECT coalesce(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$3
				AND (SELECT coalesce(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$3
				AND coalesce((SELECT provided_byte_count FROM account_balance WHERE network_id=$5),0)=$3
				AND coalesce((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$5),0)=$3
				AND (SELECT provided_byte_count FROM account_balance WHERE network_id=$6)=1
				AND (SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$6)=1`,
				prefixIds, prefixOwner.balanceId, completed-1, predecessors, prefixOwner.destinationNetworkId, targetOwner.destinationNetworkId).Scan(&exact))
			if !exact {
				t.Fatal("task completion counts, dense grant debit, provider allocation or replay did not conserve")
			}
		})
		if got := model.Testing_NetEscrowByteCount(ctx, prefixOwner.balanceId); got != model.ByteCount(2*(predecessors-completed+1)) {
			t.Fatal("dense payer mirror did not conserve remaining reservations", got)
		}
		assertTarget(true)
		t.Logf("actual task service: dense=%d pages=%d limit=%d held=%t visited=%d completed=%d busy=%d payer_completed=%d; new insert provenance only, no historical null-payer backfill claim",
			predecessors, boundedPages, model.LegacySettlementPageLimit, holdPrefix, visited, completed, busy, payerCompleted)
	})
}
