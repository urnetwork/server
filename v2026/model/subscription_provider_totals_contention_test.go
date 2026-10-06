package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// The baseline waits on the held provider row before the first financial
// commit. The producer candidate drains the entire shared-grant cohort while
// that same provider row remains held, then projects every exact allocation.
func TestLegacyProviderTotalsHeldProviderDoesNotRetainGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		const count = 512
		f, firstId := legacySettlementTestIntent(t, ctx)
		ids := []server.Id{firstId}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,balance_byte_count=1000000,net_revenue_nano_cents=2000000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id) VALUES($1)`, f.destinationNetworkId))
		})
		for index := 1; index < count; index++ {
			e, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
			server.RunPosts(ctx, posts...)
			id := e.ContractId
			id[15] = byte(index % LegacySettlementShardCount)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, e.ContractId, id))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, e.ContractId, id))
			})
			server.Raise(CloseContract(ctx, id, f.sourceId, 11, false))
			server.Raise(CloseContract(ctx, id, f.destinationId, 11, false))
			ids = append(ids, id)
		}
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
		defer release()
		var holderPid int
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, f.destinationNetworkId))
		completed, busy, _, err := flushLegacySettlement(ctx, firstId)
		if err != nil || !completed || busy {
			t.Fatal("financial settlement waited on the held provider projection row")
		}
		var firstTask server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, task.RunOnce("legacy_provider_totals", firstId).String()).Scan(&firstTask))
		})
		projectionCtx, cancelProjection := context.WithCancel(ctx)
		defer cancelProjection()
		projectionPid := make(chan int, 1)
		entered := make(chan struct{})
		projectionDone := make(chan any, 1)
		go func() {
			projectionDone <- server.HandleError(func() {
				server.Tx(projectionCtx, func(tx server.PgTx) {
					// The longer synthetic timeout permits native edge observation;
					// production still keeps its unchanged 250ms lock timeout.
					server.RaisePgResult(tx.Exec(projectionCtx, `SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='3s'`))
					var pid int
					server.Raise(tx.QueryRow(projectionCtx, `SELECT pg_backend_pid()`).Scan(&pid))
					projectionPid <- pid
					server.Raise(applyLegacyProviderTotalsInTx(projectionCtx, &providerTotalContentionTx{PgTx: tx, beforeProvider: func() { close(entered) }}, firstTask))
				}, server.TxReadCommitted, server.OptNoRetry())
			})
		}()
		joined := false
		defer func() {
			cancelProjection()
			release()
			if !joined {
				<-projectionDone
			}
		}()
		var pid int
		select {
		case pid = <-projectionPid:
		case <-projectionDone:
			joined = true
			t.Fatal("projection ended before publishing its backend")
		case <-ctx.Done():
			t.Fatal("projection did not publish its backend")
		}
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("projection did not reach provider update")
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var actualEdge bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(SELECT 1 FROM pg_stat_activity
                    WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%INSERT INTO account_balance%')`, pid, holderPid).Scan(&actualEdge))
			})
			if actualEdge {
				break
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("exact projection blocking edge was not observed")
			}
		}
		completed, busy, _, err = flushLegacySettlement(ctx, ids[1])
		if err != nil || !completed || busy {
			t.Fatal("blocked projection retained the sibling financial grant")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var stillBlocked bool
			server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(SELECT 1 FROM pg_stat_activity
                WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%INSERT INTO account_balance%')`, pid, holderPid).Scan(&stillBlocked))
			if !stillBlocked {
				t.Fatal("projection stopped blocking before sibling completion was proved")
			}
		})
		cancelProjection()
		projectionFailure := <-projectionDone
		joined = true
		if projectionFailure == nil {
			t.Fatal("held projection unexpectedly committed")
		}
		requireProviderTotalsTestState(t, ctx, firstTask, f.destinationNetworkId, false, 0, 0)
		for shard := range LegacySettlementShardCount {
			page, err := FlushLegacySettlements(ctx, shard, nil, 64)
			if err != nil || page.Failed != 0 || page.BusyOrGone != 0 || page.More {
				t.Fatal("provider-held shared-grant cohort did not drain")
			}
		}
		// The held transaction must still exist throughout the complete drain;
		// an expired connection is not proof of independence from its row lock.
		server.RaisePgResult(held.Exec(ctx, `SELECT 1`))
		expectedContracts := map[server.Id]bool{}
		for _, id := range ids {
			expectedContracts[id] = true
		}
		projectionOwners := map[server.Id]bool{}
		seenContracts := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT task_id,args_json FROM pending_task WHERE function_name=$1`, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName())
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var taskId server.Id
					var raw []byte
					server.Raise(rows.Scan(&taskId, &raw))
					var payload legacyProviderTotalsPayload
					server.Raise(json.Unmarshal(raw, &payload))
					if !expectedContracts[payload.ContractId] || seenContracts[payload.ContractId] || !payload.Private || payload.Version != 1 || payload.Applied ||
						len(payload.Totals) != 1 || payload.Totals[0].NetworkId != f.destinationNetworkId || payload.Totals[0].Bytes != 11 || payload.Totals[0].Revenue != 11 {
						t.Fatal("financial outcome lost, duplicated, or changed its exact projection owner")
					}
					seenContracts[payload.ContractId] = true
					projectionOwners[taskId] = false
				}
			})
		})
		if len(seenContracts) != count || len(projectionOwners) != count {
			t.Fatal("financial cohort has incomplete projection ownership")
		}
		assertFinancial := func(projected bool) {
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
                    (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$4 AND
                    (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND settle_time IS NOT NULL AND payout_byte_count=11)=$4 AND
                    NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1)) AND
                    (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000000-11*$4 AND
                    (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=11*$4 AND
                    (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=11*$4 AND
                    (SELECT provided_byte_count FROM account_balance WHERE network_id=$3)=CASE WHEN $5 THEN 11*$4 ELSE 0 END AND
                    (SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3)=CASE WHEN $5 THEN 11*$4 ELSE 0 END`,
					ids, f.balanceId, f.destinationNetworkId, count, projected).Scan(&exact))
				if !exact {
					t.Fatal("financial or provider projection exact conservation failed")
				}
			})
		}
		assertFinancial(false)
		// This is the real payment selection, with totals still blocked. Its
		// obligations come from committed sweeps, independent of display totals.
		server.Tx(ctx, func(tx server.PgTx) {
			planner := &PaymentPlanner{ctx: ctx, tx: tx, networkPayments: map[server.Id]*AccountPayment{}}
			server.Raise(planner.planPayments())
			payment := planner.networkPayments[f.destinationNetworkId]
			if len(planner.networkPayments) != 1 || payment == nil || payment.PayoutByteCount != 11*count || payment.Payout != 11*count {
				t.Fatal("unprojected totals changed payment selection")
			}
		})
		release()
		worker := task.NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTarget(ApplyLegacyProviderTotals))
		finished := 0
		for attempts := 0; attempts < 16 && finished < count; attempts++ {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE function_name=$2`, time.Time{}, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()))
			})
			done, _, posts, err := worker.EvalTasks(64)
			if err != nil || len(posts) != 0 {
				t.Fatal("projection task evaluation failed or created a post")
			}
			for _, taskId := range done {
				if completed, known := projectionOwners[taskId]; !known || completed {
					t.Fatal("projection finalized a duplicate or unowned task")
				}
				projectionOwners[taskId] = true
			}
			finished += len(done)
		}
		if finished != count {
			t.Fatal("finite provider projection cohort did not finish")
		}
		ownerIds := make([]server.Id, 0, len(projectionOwners))
		for id := range projectionOwners {
			ownerIds = append(ownerIds, id)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM finished_task WHERE task_id=ANY($1) AND (args_json::jsonb->>'applied')::boolean)=$2 AND
                NOT EXISTS(SELECT 1 FROM pending_task WHERE task_id=ANY($1))`, ownerIds, count).Scan(&exact))
			if !exact {
				t.Fatal("projection finalization lost applied markers or left owners pending")
			}
		})
		assertFinancial(true)
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
		t.Log("exact provider blocking edge retained no financial grant; all 512 same-grant settlements completed while provider held; real task execution projected and finalized all allocations exactly")
	})
}

type providerTotalContentionTx struct {
	server.PgTx
	beforeProvider func()
}

func (self *providerTotalContentionTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if self.beforeProvider != nil && strings.Contains(sql, "INSERT INTO account_balance") {
		before := self.beforeProvider
		self.beforeProvider = nil
		before()
	}
	return self.PgTx.Exec(ctx, sql, args...)
}
