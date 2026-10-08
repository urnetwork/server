// Actual financial pages distinguish one admitted turn from task rescheduling.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Only setup is bulk synthetic data. The real registered payer owner produces
// every outcome, debit, sweep and immutable provider task from original reports.
func seedLegacyPayerTurnIntents(t testing.TB, ctx context.Context, count int) (netEscrowOrderingTestFixture, []server.Id) {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	ids := make([]server.Id, count)
	due := make([]time.Time, count)
	prefix := server.NewId()
	for index := range ids {
		ids[index] = legacyPayerTestContractId(prefix, uint32(index+1), index%LegacySettlementShardCount)
		due[index] = time.Date(2010, time.January, 1, 0, 0, index, 0, time.UTC)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
 SET start_balance_byte_count=4096,balance_byte_count=4096,net_revenue_nano_cents=8192 WHERE balance_id=$1`, f.balanceId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS requested(id)`,
			ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
 SELECT id,$2,2 FROM unnest($1::uuid[]) AS requested(id)`, ids, f.balanceId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
 SELECT id,party,1,timestamp '2010-01-01',false FROM unnest($1::uuid[]) AS requested(id)
 CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, ids))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,get_byte(uuid_send(id),15)%16,'settled',due FROM unnest($1::uuid[],$2::timestamp[]) AS requested(id,due)`, ids, due))
	}, server.OptNoRetry())
	refreshNetEscrow(ctx, []server.Id{f.balanceId})
	return f, ids
}

// The unchanged generated block is the eligibility authority. Tests wait for
// that real boundary instead of rewriting scheduling timestamps to run early.
func waitLegacyPayerTurnEligible(t testing.TB, ctx context.Context, functionName string) {
	t.Helper()
	var block int64
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT max(available_block) FROM pending_task WHERE function_name=$1`, functionName).Scan(&block))
	})
	select {
	case <-time.After(time.Until(time.Unix(block, 0))):
	case <-ctx.Done():
		t.Fatal("real payer owner never became eligible", ctx.Err())
	}
}

// Exactly one registered task invocation drains four full pages and its EOF
// probe. The initial five-second collection wait remains in the measured span.
func TestLegacyPayerTurnDrains1024WithoutTaskHandoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(Testing_WithLegacyPayerSettlementCollectionWindow(t.Context()), time.Minute)
		defer cancel()
		f, ids := seedLegacyPayerTurnIntents(t, ctx, 1024)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		began := server.NowUtc()
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			QueueLegacyPayerSettlementsInTx(owner, tx, f.sourceNetworkId)
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyPayerSettlementTaskTarget())
		waitLegacyPayerTurnEligible(t, ctx, NewLegacyPayerSettlementTaskTarget().TargetFunctionName())
		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			t.Fatal("single payer invocation did not finish", finished, retried, postRetried, err)
		}
		done := task.GetFinishedTasks(ctx, finished...)[finished[0]]
		var result LegacyPayerSettlementResult
		server.Raise(json.Unmarshal([]byte(done.ResultJson), &result))
		if result.Completed != len(ids) || result.Visited != len(ids) || result.Pages != 5 || result.More ||
			result.Cursor != nil || result.Failed != 0 || result.BusyOrGone != 0 || result.PassEndTime.IsZero() ||
			done.RunAt.Before(began.Add(5*time.Second)) || done.RunStartTime.Before(done.RunAt) {
			t.Fatal("healthy pages crossed a task handoff or lost their collection deadline", result, done.RunAt, done.RunStartTime)
		}
		assertMoney := func() {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
 SELECT allocation FROM pending_task CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
 WHERE function_name=$4 AND NOT COALESCE((args_json::jsonb->>'applied')::boolean,false)
 AND (allocation->>'network_id')::uuid=$3
 ) SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=1024
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
 AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=1)=1024
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=3072
 AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1024
 AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1024
 AND (SELECT count(*) FROM contract_close WHERE contract_id=ANY($1) AND used_transfer_byte_count=1
      AND close_time=timestamp '2010-01-01' AND NOT checkpoint)=2048
 AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)
     +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=1024
 AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)
     +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=1024
 AND NOT EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$5)`, ids, f.balanceId, f.destinationNetworkId,
					task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(),
					task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&exact))
				if !exact {
					t.Fatal("multi-page turn lost exact financial or durable provider custody")
				}
			})
		}
		assertMoney()
		replay, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: f.sourceNetworkId}, owner)
		if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.More {
			t.Fatal("multi-page replay repeated financial work", replay, err)
		}
		assertMoney()
		t.Logf("1024 financial contracts: one actual invocation, pages=%d, collection-inclusive elapsed=%s, function=%s; durable output completion is a separate benchmark denominator",
			result.Pages, server.NowUtc().Sub(began), done.RunEndTime.Sub(done.RunStartTime))
	})
}

// A busy payer returns after one bounded page while its actual grant owner is
// still held. Another registered payer settles without releasing that owner.
func TestLegacyPayerTurnBusyOwnerYieldsIndependentPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		hot, hotIds := seedLegacyPayerTurnIntents(t, ctx, 257)
		other, otherId := legacySettlementTestIntent(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, hot.balanceId))
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		withLegacyPayerQueueTestTx(ctx, []server.Id{hot.sourceNetworkId, other.sourceNetworkId}, func(tx server.PgTx) {
			for _, payer := range []server.Id{hot.sourceNetworkId, other.sourceNetworkId} {
				ScheduleLegacyPayerSettlementsInTx(owner, tx, payer, nil, time.Unix(1, 0))
			}
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyPayerSettlementTaskTarget())
		finished, retried, posts, err := worker.EvalTasks(2)
		if err != nil || len(finished) != 2 || len(retried)+len(posts) != 0 {
			t.Fatal("busy payer suppressed an independent actual task", finished, retried, posts, err)
		}
		for _, done := range task.GetFinishedTasks(ctx, finished...) {
			var args LegacyPayerSettlementArgs
			var result LegacyPayerSettlementResult
			server.Raise(json.Unmarshal([]byte(done.ArgsJson), &args))
			server.Raise(json.Unmarshal([]byte(done.ResultJson), &result))
			if args.PayerNetworkId == hot.sourceNetworkId &&
				(result.Pages != 1 || result.Visited != LegacySettlementPageLimit || result.Completed != 0 || result.BusyOrGone != LegacySettlementPageLimit || !result.More) {
				t.Fatal("busy owner looped into a second page", result)
			}
		}
		requireLegacySettlementTestState(t, ctx, other, otherId, false, true, 989, 0)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1))=257
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=4096
 AND NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NOT NULL)
 AND EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$3)`, hotIds, hot.balanceId,
				task.RunOnce("flush_legacy_payer_settlements", hot.sourceNetworkId).String()).Scan(&exact))
			if !exact {
				t.Fatal("busy turn lost durable work or touched the held payer")
			}
		})
		server.Raise(held.Rollback(ctx))
		resumed, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: hot.sourceNetworkId}, owner)
		if err != nil || resumed.Completed != len(hotIds) || resumed.More || resumed.Failed != 0 {
			t.Fatal("released payer did not resume all durable work", resumed, err)
		}
	})
}

// The same shared budget reaches every page. A cancellation between pages
// yields the prior committed cursor; parent and unrelated errors remain errors.
func TestLegacyPayerTurnBudgetPreservesPrefixWithoutReset(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		foreign := &pgconn.PgError{Code: "40001", Message: "synthetic foreign transaction failure"}
		for _, scenario := range []struct {
			name         string
			betweenPages bool
			cancelParent bool
			failure      error
			yield        bool
		}{
			{name: "between_pages", betweenPages: true, yield: true},
			{name: "next_page_selection", failure: context.Canceled, yield: true},
			{name: "foreign_error", failure: errors.Join(context.Canceled, foreign)},
			{name: "parent_cancel", cancelParent: true, failure: context.Canceled},
		} {
			f, ids := seedLegacyPayerTurnIntents(t, ctx, 2)
			parent, cancelParent := context.WithCancel(ctx)
			deadline, cancelDeadline := context.WithTimeout(parent, 15*time.Second)
			bounded, expire := context.WithCancelCause(deadline)
			fixedDeadline, _ := bounded.Deadline()
			calls := 0
			result, err := flushLegacyPayerSettlementPages(parent, bounded, f.sourceNetworkId, nil, 1, true,
				func(callParent, page context.Context, shard int, cursor *LegacySettlementCursor, limit int) (LegacySettlementFlushResult, error) {
					calls++
					if got, ok := page.Deadline(); !ok || !got.Equal(fixedDeadline) {
						t.Fatal("payer continuation reset its fixed budget", scenario.name)
					}
					if calls == 1 {
						part, err := flushLegacySettlementsPage(callParent, page, shard, cursor, limit,
							flushLegacySettlementWithGrantWait, flushLegacySettlementCohort)
						if err != nil || part.Completed != 1 {
							t.Fatal("first real page did not commit", scenario.name, part, err)
						}
						if scenario.betweenPages {
							expire(errLegacySettlementPageBudget)
						}
						return part, err
					}
					if calls != 2 {
						t.Fatal("interrupted turn started another page", scenario.name)
					}
					expire(errLegacySettlementPageBudget)
					if scenario.cancelParent {
						cancelParent()
					}
					return LegacySettlementFlushResult{}, scenario.failure
				})
			expire(nil)
			cancelDeadline()
			cancelParent()
			if scenario.yield && err != nil || !scenario.yield && err != scenario.failure ||
				result.Completed != 1 || result.Visited != 1 || result.Cursor == nil || result.Cursor.ContractId != ids[0] ||
				result.More != scenario.yield || scenario.betweenPages && calls != 1 {
				t.Fatal("turn lost committed prefix or original failure", scenario.name, result, err)
			}
			other, otherId := legacySettlementTestIntent(t, ctx)
			owner := session.NewLocalClientSession(ctx, "", nil)
			progress, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: other.sourceNetworkId}, owner)
			if err != nil || progress.Completed != 1 {
				t.Fatal("yielded budget retained ownership against another payer", progress, err)
			}
			requireLegacySettlementTestState(t, ctx, other, otherId, false, true, 989, 0)
			resumed, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: f.sourceNetworkId, Cursor: result.Cursor}, owner)
			owner.Cancel()
			if err != nil || resumed.Completed != 1 || resumed.More || resumed.Failed != 0 {
				t.Fatal("yielded real prefix failed exact continuation", resumed, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1)=4094
 AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($2) AND outcome='settled')=2
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($2))`, f.balanceId, ids).Scan(&exact))
				if !exact {
					t.Fatal("budget continuation changed accounting", scenario.name)
				}
			})
		}
	})
}
