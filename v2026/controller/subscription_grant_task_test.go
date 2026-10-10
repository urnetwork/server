// Durable grant effects must survive a lost task-completion acknowledgement.
package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Run the actual serialized body, discard its completion, then let the worker
// replay that same pending row. A separate task is still a distinct grant.
func assertTransferGrantTaskReplay[A, R any](t testing.TB, owner *session.ClientSession,
	body func(A, *session.ClientSession) (R, error),
	post func(A, R, *session.ClientSession, server.PgTx) error,
	args A, kind string, expectedRows int, expectedBytes int64,
) {
	ctx := owner.Ctx
	target := task.NewTaskTargetWithPost(body, post)
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(target)
	id := task.ScheduleTask(body, args, owner, task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
	queued := task.GetTasks(ctx, id)[id]
	if _, _, err := target.RunSpecific(ctx, queued); err != nil {
		t.Fatal(kind, "first grant failed", err)
	}
	verify := func(multiplier int) {
		server.Db(ctx, func(conn server.PgConn) {
			var rows int
			var total int64
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(sum(start_balance_byte_count),0)
				FROM transfer_balance WHERE grant_kind=$1`, kind).Scan(&rows, &total))
			if rows != multiplier*expectedRows || total != int64(multiplier)*expectedBytes {
				t.Errorf("%s task replay duplicated or lost grants: rows=%d bytes=%d multiplier=%d", kind, rows, total, multiplier)
			}
		})
	}
	verify(1)
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
		t.Fatal(kind, "replay failed to complete", finished, retried, posts, err)
	}
	verify(1)
	server.Db(ctx, func(conn server.PgConn) {
		var completed, future bool
		var pending int
		server.Raise(conn.QueryRow(ctx, `SELECT post_completed FROM finished_task WHERE task_id=$1`, id).Scan(&completed))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*),bool_and(run_at>$2) FROM pending_task WHERE function_name=$1`,
			target.TargetFunctionName(), server.NowUtc()).Scan(&pending, &future))
		if !completed || pending != 1 || !future {
			t.Fatal(kind, "replay lost its future successor", completed, pending, future)
		}
	})
	newId := task.ScheduleTask(body, args, owner, task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
	if _, _, err := target.RunSpecific(ctx, task.GetTasks(ctx, newId)[newId]); err != nil {
		t.Fatal(kind, "distinct grant failed", err)
	}
	verify(2)
}

// All three grants used to mint again after a crash between body commit and
// task handback. Include paid/free classification and both referral sides.
func TestRecurringGrantTasksDoNotDuplicateCommittedBodyOnReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := model.Pro()
		previous := *config
		defer func() { *config = previous }()
		config.Free.Data, config.Free.DataPeriod, config.Pro.Data = 100, 24*time.Hour, 400
		config.ReferralBonus, config.ReferredBonus, config.MaxReferrals = 200, 300, 10
		freeId, proId, referrerId, refereeId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, id := range []server.Id{freeId, proId, referrerId, refereeId} {
			model.Testing_CreateNetwork(ctx, id, "synthetic-grant-"+id.String(), server.NewId())
		}
		now := server.NowUtc()
		server.Raise(model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId: proId, SubscriptionType: model.SubscriptionTypeSupporter,
			StartTime: now.Add(-24 * time.Hour), EndTime: now.Add(29 * 24 * time.Hour), NetRevenue: model.UsdToNanoCents(5),
		}))
		code := model.CreateNetworkReferralCode(ctx, referrerId)
		if model.CreateNetworkReferral(ctx, refereeId, code.ReferralCode) == nil {
			t.Fatal("synthetic referral was rejected")
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		assertTransferGrantTaskReplay(t, owner, RefreshFreeTransferBalances, RefreshFreeTransferBalancesPost,
			&RefreshFreeTransferBalancesArgs{}, model.GrantKindFree, 3, 300)
		assertTransferGrantTaskReplay(t, owner, RefreshProTransferBalances, RefreshProTransferBalancesPost,
			&RefreshProTransferBalancesArgs{}, model.GrantKindPro, 1, 400)
		assertTransferGrantTaskReplay(t, owner, RefreshReferralTransferBalances, RefreshReferralTransferBalancesPost,
			&RefreshReferralTransferBalancesArgs{}, model.GrantKindReferral, 2, 500)
	})
}

// A failed referral SELECT used to look like a successful empty run. A real
// database error must retain the pending task and leave no completion receipt.
func TestReferralGrantTaskRejectsDiscoveryFailureAndRecovers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		config := model.Pro()
		previous := *config
		defer func() { *config = previous }()
		config.ReferralBonus, config.ReferredBonus, config.MaxReferrals = 200, 300, 10
		referrerId, refereeId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, referrerId, "synthetic-referrer", server.NewId())
		model.Testing_CreateNetwork(ctx, refereeId, "synthetic-referee", server.NewId())
		code := model.CreateNetworkReferralCode(ctx, referrerId)
		if model.CreateNetworkReferral(ctx, refereeId, code.ReferralCode) == nil {
			t.Fatal("synthetic referral was rejected")
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := task.NewTaskTargetWithPost(RefreshReferralTransferBalances, RefreshReferralTransferBalancesPost)
		id := task.ScheduleTask(RefreshReferralTransferBalances, &RefreshReferralTransferBalancesArgs{}, owner)
		queued := task.GetTasks(ctx, id)[id]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_referral RENAME TO synthetic_unavailable_referrals`))
		})
		_, _, err := target.RunSpecific(ctx, queued)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE synthetic_unavailable_referrals RENAME TO network_referral`))
		})
		if err == nil {
			t.Fatal("referral discovery failure was acknowledged as an empty grant")
		}
		// Reject a balance insert after receipt admission, then retry the same
		// task. The receipt must roll back with the failed financial effects.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_refuse_grant() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'synthetic grant refusal'; END $$;
				CREATE TRIGGER synthetic_refuse_grant BEFORE INSERT ON transfer_balance
				FOR EACH ROW EXECUTE FUNCTION synthetic_refuse_grant()`))
		})
		_, _, err = target.RunSpecific(ctx, queued)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_refuse_grant ON transfer_balance`))
		})
		if err == nil {
			t.Fatal("failed grant was acknowledged")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var rows int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_balance_grant_run WHERE run_id=$1`, id).Scan(&rows))
			if rows != 0 {
				t.Fatal("failed grant retained a replay receipt")
			}
		})
		for range 2 {
			if _, _, err := target.RunSpecific(ctx, queued); err != nil {
				t.Fatal("restored grant could not recover", err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var rows int
			var total int64
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),sum(start_balance_byte_count) FROM transfer_balance WHERE grant_kind='referral'`).Scan(&rows, &total))
			if rows != 2 || total != 500 {
				t.Fatal("recovered referral grant was lost or doubled", rows, total)
			}
		})
	})
}
