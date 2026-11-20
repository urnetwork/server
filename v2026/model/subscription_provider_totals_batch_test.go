// Native database controls count actual account writes and force replay boundaries.
package model

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// A trigger observes committed accounting writes, independently of Go helpers.
func providerTotalsBatchWriteCounter(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
            CREATE TABLE test_provider_total_write (network_id uuid NOT NULL);
            CREATE FUNCTION test_provider_total_write() RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN INSERT INTO test_provider_total_write VALUES (NEW.network_id); RETURN NEW; END $$;
            CREATE TRIGGER test_provider_total_write AFTER INSERT OR UPDATE ON account_balance
            FOR EACH ROW EXECUTE FUNCTION test_provider_total_write()`))
	})
}

// Keep the fixture's scheduler admission explicit instead of waiting for wall time.
func providerTotalsBatchDue(ctx context.Context, taskIds []server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=ANY($1)`, taskIds, time.Time{}))
	})
}

// This reproduces one account write per contract on the unbatched registered
// target. The candidate performs one write per already-claimed provider cohort.
func TestLegacyProviderTotalsClaimedBatchCoalescesWritesAndFinalizesOwners(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		worker := task.NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
		totalCount := 0
		expectedWrites := 0
		for _, count := range []int{4, 4, legacyProviderTotalsBatchLimit + 1} {
			taskIds := make([]server.Id, 0, count)
			for range count {
				taskIds = append(taskIds, providerTotalsTestTask(ctx, server.NewId(), networkId))
			}
			providerTotalsBatchDue(ctx, taskIds)
			// Admission cannot run two prepared writers for this same provider
			// at once. Preserve the complete65-owner oracle across64+1 claims.
			remaining := count
			for pass := 0; remaining > 0; pass++ {
				if pass >= (count+legacyProviderTotalsBatchLimit-1)/legacyProviderTotalsBatchLimit {
					t.Fatal("bounded provider admission did not finish its exact cohort")
				}
				finished, retried, posts, err := worker.EvalTasks(remaining)
				if err != nil || len(finished) != min(remaining, legacyProviderTotalsBatchLimit) || len(retried) != 0 || len(posts) != 0 {
					t.Fatalf("claimed provider cohort did not finalize exactly: finished=%d retry=%d posts=%d err=%v", len(finished), len(retried), len(posts), err)
				}
				remaining -= len(finished)
			}
			for _, id := range taskIds {
				completed := task.GetFinishedTasks(ctx, id)[id]
				if completed == nil {
					t.Fatal("missing independently finalized owner")
				}
				payload, err := decodeLegacyProviderTotals(completed.ArgsJson)
				if err != nil || !payload.Applied {
					t.Fatal("finalizer did not retain the committed marker", err)
				}
			}
			totalCount += count
			expectedWrites += (count + legacyProviderTotalsBatchLimit - 1) / legacyProviderTotalsBatchLimit
			server.Db(ctx, func(conn server.PgConn) {
				var writes, pending int
				var provided, revenue int64
				server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents,
                (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1),
                (SELECT count(*) FROM pending_task) FROM account_balance WHERE network_id=$1`, networkId).
					Scan(&provided, &revenue, &writes, &pending))
				if provided != int64(17*totalCount) || revenue != int64(29*totalCount) || writes != expectedWrites || pending != 0 {
					t.Fatalf("expected exact totals and %d writes with no backlog: bytes=%d revenue=%d writes=%d pending=%d", expectedWrites, provided, revenue, writes, pending)
				}
			})
		}
	})
}

// A malformed neighbor and a multi-provider owner do not join another network's
// batch. Both provider cohorts and the multi-provider allocation still complete.
func TestLegacyProviderTotalsClaimedBatchKeepsUnrelatedAllocationsIndependent(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		taskIds := []server.Id{}
		for _, networkId := range networkIds[:2] {
			for range 4 {
				taskIds = append(taskIds, providerTotalsTestTask(ctx, server.NewId(), networkId))
			}
		}
		multiTaskId := providerTotalsTestPublish(ctx, server.NewId(), map[server.Id]*contractPayout{
			networkIds[2]: {payoutByteCount: 31, payout: 43},
			networkIds[3]: {payoutByteCount: 47, payout: 59},
		})
		taskIds = append(taskIds, multiTaskId)
		badTaskId := providerTotalsTestTask(ctx, server.NewId(), networkIds[0])
		taskIds = append(taskIds, badTaskId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{version}','2'::jsonb)::text WHERE task_id=$1`, badTaskId))
		})
		providerTotalsBatchDue(ctx, taskIds)
		worker := task.NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
		finished, retried, posts, err := worker.EvalTasks(len(taskIds))
		if err != nil || len(finished) != 9 || len(retried) != 1 || retried[0] != badTaskId || len(posts) != 0 {
			t.Fatal("a refused allocation contaminated unrelated task results", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM test_provider_total_write)=4 AND
                (SELECT count(*) FROM account_balance WHERE network_id=ANY($1)
                    AND provided_byte_count=68 AND provided_net_revenue_nano_cents=116)=2 AND
                EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2
                    AND provided_byte_count=31 AND provided_net_revenue_nano_cents=43) AND
                EXISTS(SELECT 1 FROM account_balance WHERE network_id=$3
                    AND provided_byte_count=47 AND provided_net_revenue_nano_cents=59)`,
				networkIds[:2], networkIds[2], networkIds[3]).Scan(&exact))
			if !exact {
				t.Fatal("independent cohorts changed exact allocations or repeated account writes")
			}
		})
		requireProviderTotalsTestState(t, ctx, badTaskId, networkIds[0], false, 68, 116)
	})
}

// Observe a real batch-to-provider wait edge, then cancel that owner. No accounting
// or marker survives; a committed retry and stale old-reader replay apply once.
func TestLegacyProviderTotalsBatchHeldProviderRollbackAndLostReplyReplay(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id) VALUES($1)`, networkId))
		})
		providerTotalsBatchWriteCounter(t, ctx)
		taskIds := []server.Id{}
		for range 8 {
			taskIds = append(taskIds, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		staleTasks := task.GetTasks(ctx, taskIds...)
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
		defer release()
		var holderPid int
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, networkId))
		projectionCtx, cancelProjection := context.WithCancel(ctx)
		defer cancelProjection()
		projectionPid := make(chan int, 1)
		projectionDone := make(chan any, 1)
		go func() {
			projectionDone <- server.HandleError(func() {
				server.Tx(projectionCtx, func(tx server.PgTx) {
					// Only this native control lengthens the SQL bound to observe
					// the exact lock edge. Production retains its 250ms limit.
					server.RaisePgResult(tx.Exec(projectionCtx, `SET LOCAL lock_timeout='10s'; SET LOCAL statement_timeout='10s'`))
					var pid int
					server.Raise(tx.QueryRow(projectionCtx, `SELECT pg_backend_pid()`).Scan(&pid))
					projectionPid <- pid
					server.Raise(applyLegacyProviderTotalsBatchInTx(projectionCtx, tx, taskIds, networkId))
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
		case <-ctx.Done():
			t.Fatal("batch did not publish its backend")
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(
                    SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'
                    AND query LIKE '%INSERT INTO account_balance%')`, pid, holderPid).Scan(&blocked))
			})
			if blocked {
				break
			}
			select {
			case failure := <-projectionDone:
				joined = true
				t.Fatal("batch ended before its provider wait was observed", failure)
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("actual provider blocking edge was not observed")
			}
		}
		// Another provider must remain writable while this exact edge exists.
		otherNetworkId := server.NewId()
		otherIds := []server.Id{
			providerTotalsTestTask(ctx, server.NewId(), otherNetworkId),
			providerTotalsTestTask(ctx, server.NewId(), otherNetworkId),
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(applyLegacyProviderTotalsBatchInTx(ctx, tx, otherIds, otherNetworkId))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1`))
		for _, id := range otherIds {
			requireProviderTotalsTestState(t, ctx, id, otherNetworkId, true, 34, 58)
		}
		cancelProjection()
		failure := <-projectionDone
		joined = true
		if failure == nil {
			t.Fatal("canceled held batch committed")
		}
		for _, id := range taskIds {
			requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		}
		release()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(applyLegacyProviderTotalsBatchInTx(ctx, tx, taskIds, networkId))
		}, server.TxReadCommitted, server.OptNoRetry())
		// Discard the commit reply and re-run every stale single-task invocation.
		oldReader := task.NewTaskTarget(ApplyLegacyProviderTotals)
		for _, id := range taskIds {
			_, _, err := oldReader.RunSpecific(ctx, staleTasks[id])
			server.Raise(err)
			requireProviderTotalsTestState(t, ctx, id, networkId, true, 136, 232)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var writes int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_provider_total_write WHERE network_id=$1`, networkId).Scan(&writes))
			if writes != 1 {
				t.Fatalf("batch retry or old-reader replay repeated committed writes: %d", writes)
			}
		})
	})
}

// A marker failure occurs after the account update and rolls the entire batch back.
func TestLegacyProviderTotalsBatchMarkerFailureRollsBackAccounting(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		taskIds := []server.Id{
			providerTotalsTestTask(ctx, server.NewId(), networkId),
			providerTotalsTestTask(ctx, server.NewId(), networkId),
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
                CREATE FUNCTION test_provider_total_marker_failure() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN RAISE EXCEPTION 'synthetic marker failure' USING ERRCODE='P0001'; END $$;
                CREATE TRIGGER test_provider_total_marker_failure BEFORE UPDATE ON pending_task
                FOR EACH ROW EXECUTE FUNCTION test_provider_total_marker_failure()`))
		})
		failure := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(applyLegacyProviderTotalsBatchInTx(ctx, tx, taskIds, networkId))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if failure == nil {
			t.Fatal("marker failure was not exercised")
		}
		for _, id := range taskIds {
			requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		}
	})
}

// Accumulating individually valid nonnegative payloads must not wrap Go integers.
func TestLegacyProviderTotalsBatchSumOverflowPreservesUnappliedOwners(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		for _, revenueOverflow := range []bool{false, true} {
			networkId := server.NewId()
			taskIds := []server.Id{}
			contractIds := []server.Id{server.NewId(), server.NewId()}
			keys := []server.PgOwnershipKey{
				task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractIds[0])),
				task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractIds[1])),
			}
			server.OwnedTx(ctx, keys, func(tx server.PgTx) {
				large := &contractPayout{payoutByteCount: math.MaxInt64, payout: 1}
				if revenueOverflow {
					large = &contractPayout{payoutByteCount: 1, payout: math.MaxInt64}
				}
				taskIds = append(taskIds, queueLegacyProviderTotalsInTx(ctx, tx, contractIds[0], map[server.Id]*contractPayout{networkId: large}))
				taskIds = append(taskIds, queueLegacyProviderTotalsInTx(ctx, tx, contractIds[1], map[server.Id]*contractPayout{networkId: {payoutByteCount: 1, payout: 1}}))
			}, server.TxReadCommitted)
			failure := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(applyLegacyProviderTotalsBatchInTx(ctx, tx, taskIds, networkId))
				}, server.TxReadCommitted, server.OptNoRetry())
			})
			if failure == nil {
				t.Fatal("overflowing allocation sum was accepted")
			}
			for _, id := range taskIds {
				requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
			}
		}
	})
}

// Preparation uses amounts only as hints: accounting re-reads locked durable rows.
func TestLegacyProviderTotalsBatchUsesDurableAmountsAndSkipsAppliedOwners(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		taskIds := []server.Id{
			providerTotalsTestTask(ctx, server.NewId(), networkId),
			providerTotalsTestTask(ctx, server.NewId(), networkId),
		}
		staleTasks := task.GetTasks(ctx, taskIds...)
		queued := []*task.Task{staleTasks[taskIds[0]], staleTasks[taskIds[1]]}
		for _, item := range queued {
			payload, err := decodeLegacyProviderTotals(item.ArgsJson)
			server.Raise(err)
			payload.Totals[0].Bytes = 999
			payload.Totals[0].Revenue = 999
			data, err := json.Marshal(payload)
			server.Raise(err)
			item.ArgsJson = string(data)
		}
		prepared := NewLegacyProviderTotalsTaskTarget().(task.TaskBatchPreparer).PrepareTaskBatch(queued)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(applyLegacyProviderTotalsInTx(ctx, tx, taskIds[0]))
		}, server.TxReadCommitted, server.OptNoRetry())
		for _, item := range queued {
			_, _, err := prepared.Run(ctx, item)
			server.Raise(err)
		}
		for _, id := range taskIds {
			requireProviderTotalsTestState(t, ctx, id, networkId, true, 34, 58)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var writes int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_provider_total_write WHERE network_id=$1`, networkId).Scan(&writes))
			if writes != 2 {
				t.Fatal("applied prefix or second batch invocation repeated accounting")
			}
		})
	})
}

// Two independent claim cohorts still serialize at their shared provider row;
// the second observes the first committed value and preserves both exact sums.
func TestLegacyProviderTotalsConcurrentBatchesAddOnce(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		taskIds := []server.Id{}
		for range 4 {
			taskIds = append(taskIds, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		first, err := conn.Begin(ctx)
		server.Raise(err)
		defer first.Rollback(context.Background())
		var firstPid int
		server.Raise(first.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPid))
		server.Raise(applyLegacyProviderTotalsBatchInTx(ctx, first, []server.Id{taskIds[1], taskIds[0]}, networkId))
		secondCtx, cancelSecond := context.WithCancel(ctx)
		defer cancelSecond()
		secondPid := make(chan int, 1)
		secondDone := make(chan any, 1)
		go func() {
			secondDone <- server.HandleError(func() {
				server.Tx(secondCtx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(secondCtx, `SET LOCAL lock_timeout='10s'; SET LOCAL statement_timeout='10s'`))
					var pid int
					server.Raise(tx.QueryRow(secondCtx, `SELECT pg_backend_pid()`).Scan(&pid))
					secondPid <- pid
					server.Raise(applyLegacyProviderTotalsBatchInTx(secondCtx, tx, taskIds[2:], networkId))
				}, server.TxReadCommitted, server.OptNoRetry())
			})
		}()
		joined := false
		defer func() {
			cancelSecond()
			_ = first.Rollback(context.Background())
			if !joined {
				<-secondDone
			}
		}()
		var pid int
		select {
		case pid = <-secondPid:
		case <-ctx.Done():
			t.Fatal("second batch did not publish its backend")
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(
                    SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'
                    AND query LIKE '%INSERT INTO account_balance%')`, pid, firstPid).Scan(&blocked))
			})
			if blocked {
				break
			}
			select {
			case failure := <-secondDone:
				joined = true
				t.Fatal("second batch ended before the shared-row edge was observed", failure)
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("concurrent batches did not expose their shared-row edge")
			}
		}
		for _, id := range taskIds {
			requireProviderTotalsTestState(t, ctx, id, networkId, false, 0, 0)
		}
		server.Raise(first.Commit(ctx))
		failure := <-secondDone
		joined = true
		if failure != nil {
			t.Fatal("second provider batch failed after the first committed", failure)
		}
		for _, id := range taskIds {
			requireProviderTotalsTestState(t, ctx, id, networkId, true, 68, 116)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var writes int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_provider_total_write WHERE network_id=$1`, networkId).Scan(&writes))
			if writes != 2 {
				t.Fatal("concurrent claim cohorts repeated or lost an additive update")
			}
		})
	})
}

// A payload changed after preparation is revalidated under row ownership. A new
// evaluation can retry the whole cohort after the synthetic fault is repaired.
func TestLegacyProviderTotalsBatchInvalidDurableMemberRollsBackAndRetries(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		taskIds := []server.Id{
			providerTotalsTestTask(ctx, server.NewId(), networkId),
			providerTotalsTestTask(ctx, server.NewId(), networkId),
		}
		original := task.GetTasks(ctx, taskIds...)
		queued := []*task.Task{original[taskIds[0]], original[taskIds[1]]}
		target := NewLegacyProviderTotalsTaskTarget().(task.TaskBatchPreparer)
		prepared := target.PrepareTaskBatch(queued)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{version}','2'::jsonb)::text WHERE task_id=$1`, taskIds[1]))
		})
		for _, item := range queued {
			if _, _, err := prepared.Run(ctx, item); err == nil {
				t.Fatal("stale preparation accepted a malformed durable member")
			}
			requireProviderTotalsTestState(t, ctx, item.TaskId, networkId, false, 0, 0)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE task_id=$1`, taskIds[1], original[taskIds[1]].ArgsJson))
		})
		prepared = target.PrepareTaskBatch(queued)
		for _, item := range queued {
			_, _, err := prepared.Run(ctx, item)
			server.Raise(err)
			requireProviderTotalsTestState(t, ctx, item.TaskId, networkId, true, 34, 58)
		}
	})
}
