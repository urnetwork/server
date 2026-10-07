package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

func TestLegacyProviderTotalsQueueCollisionRollsBackFinancialPrefix(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		collision := providerTotalsTestTask(ctx, id, f.destinationNetworkId)
		before := task.GetTasks(ctx, collision)[collision].ArgsJson
		_, _, _, err := flushLegacySettlement(ctx, id)
		if err == nil {
			t.Fatal("existing task payload was silently merged into financial settlement")
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		if got := task.GetTasks(ctx, collision)[collision]; got == nil || got.ArgsJson != before {
			t.Fatal("collision replaced the existing task allocation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1) AND
                NOT EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2) AND
                (SELECT count(*) FROM pending_task WHERE run_once_key=$3)=1`, id, f.destinationNetworkId, task.RunOnce("legacy_provider_totals", id).String()).Scan(&untouched))
			if !untouched {
				t.Fatal("queue collision committed part of the financial transition")
			}
		})
		// Remove only this deliberately invalid synthetic owner; production
		// must preserve all unapplied projection tasks instead of canceling them.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, collision))
		})
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy {
			t.Fatal("clean outcome did not enqueue its allocation")
		}
		var projectionId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, task.RunOnce("legacy_provider_totals", id).String()).Scan(&projectionId))
		})
		mirrorOwner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE task_id=ANY($2)`, time.Time{}, []server.Id{projectionId, mirrorOwner.TaskId}))
		})
		mirrorOwner = task.GetTasks(ctx, mirrorOwner.TaskId)[mirrorOwner.TaskId]
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTarget(ApplyLegacyProviderTotals))
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != projectionId || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("exact provider projection did not finalize", err)
		}
		requireProviderTotalsTestMirrorUntouched(t, ctx, mirrorOwner)
		// RunOnce has been released by finalization. The terminal outcome,
		// rather than the vanished queue key, still prevents another enqueue.
		completed, _, _, err = flushLegacySettlement(ctx, id)
		if err != nil || completed {
			t.Fatal("terminal outcome replay claimed a second settlement")
		}
		requireProviderTotalsTestMirrorUntouched(t, ctx, mirrorOwner)
		finalizeProviderTotalsTestMirror(t, ctx, mirrorOwner)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM pending_task)=0 AND
                (SELECT provided_byte_count=11 AND provided_net_revenue_nano_cents=11 FROM account_balance WHERE network_id=$1)`, f.destinationNetworkId).Scan(&exact))
			if !exact {
				t.Fatal("finalized-task deletion lost the lifetime outcome replay fence")
			}
		})
	})
}

func TestLegacyProviderTotalsSurvivesContractAndSweepRetention(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		// A committed initial settlement loses its reply and starts no posts.
		server.Tx(ctx, func(tx server.PgTx) {
			_, completed, busy, _, err := flushLegacySettlementInTx(ctx, tx, id)
			server.Raise(err)
			if !completed || busy {
				t.Fatal("retention control did not commit initial settlement")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		var taskId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, task.RunOnce("legacy_provider_totals", id).String()).Scan(&taskId))
		})
		stale := task.GetTasks(ctx, taskId)[taskId]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow_sweep WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
		})
		task.RemoveFinishedTasks(ctx, server.NowUtc().Add(time.Hour), server.NowUtc().Add(time.Hour))
		requireProviderTotalsTestState(t, ctx, taskId, f.destinationNetworkId, false, 0, 0)
		target := task.NewTaskTarget(ApplyLegacyProviderTotals)
		for range 2 {
			_, _, err := target.RunSpecific(ctx, stale)
			server.Raise(err)
		}
		requireProviderTotalsTestState(t, ctx, taskId, f.destinationNetworkId, true, 11, 11)
	})
}
