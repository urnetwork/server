package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Settlement publishes no provider totals since 2026-11-11 (BILLING-SPLIT.md).
// A provider totals row already queued under the contract's key, as binaries
// built before then queued one per settled contract, neither refuses
// settlement (a publication collision used to roll the whole financial
// transition back) nor is merged, replaced or applied by it. The per-task
// reader still applies the row, beside the settlement's own mirror owner.
func TestLegacyProviderTotalsQueuedRowDoesNotBlockSettlement(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestOpenFinals(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		queued := providerTotalsTestTask(ctx, id, f.destinationNetworkId)
		before := task.GetTasks(ctx, queued)[queued].ArgsJson
		if err := SettleEscrow(ctx, id, ContractOutcomeSettled); err != nil {
			t.Fatal("a queued provider totals row refused settlement", err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, 989, 0)
		if got := task.GetTasks(ctx, queued)[queued]; got == nil || got.ArgsJson != before {
			t.Fatal("settlement changed the queued provider allocation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT sum(payout_byte_count)=11 AND sum(payout_net_revenue_nano_cents)=11 FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2) AND
				NOT EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2) AND
				(SELECT count(*) FROM pending_task WHERE run_once_key=$3)=1`, id, f.destinationNetworkId, task.RunOnce("legacy_provider_totals", id).String()).Scan(&exact))
			if !exact {
				t.Fatal("settlement lost its sweep, credited provider totals or queued another allocation")
			}
		})
		mirrorOwner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE task_id=ANY($2)`, time.Time{}, []server.Id{queued, mirrorOwner.TaskId}))
		})
		mirrorOwner = task.GetTasks(ctx, mirrorOwner.TaskId)[mirrorOwner.TaskId]
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTarget(ApplyLegacyProviderTotals))
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != queued || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("the queued provider allocation did not finalize", err)
		}
		requireProviderTotalsTestMirrorUntouched(t, ctx, mirrorOwner)
		requireLegacyOwnedSettlementReplayInert(t, ctx, id)
		requireProviderTotalsTestMirrorUntouched(t, ctx, mirrorOwner)
		finalizeProviderTotalsTestMirror(t, ctx, mirrorOwner)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			// the account holds the queued allocation (17/29) and nothing of the
			// settlement's 11/11
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM pending_task)=0 AND
                (SELECT provided_byte_count=17 AND provided_net_revenue_nano_cents=29 FROM account_balance WHERE network_id=$1)`, f.destinationNetworkId).Scan(&exact))
			if !exact {
				t.Fatal("the queued allocation was lost or settlement added to the provider totals")
			}
		})
	})
}

// A row an older binary queued with its settlement keeps its exact payload
// after contract and sweep retention removes everything it was computed from,
// and the per-task reader applies it once.
func TestLegacyProviderTotalsSurvivesContractAndSweepRetention(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestOpenFinals(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		// A committed initial settlement loses its reply and starts no posts.
		server.Tx(ctx, func(tx server.PgTx) {
			_, closed, err := settleEscrowForegroundInTx(ctx, tx, id, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("retention control did not commit initial settlement")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		taskId := queueLegacyProviderTotalsFromSweepsForTest(t, ctx, id)
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
