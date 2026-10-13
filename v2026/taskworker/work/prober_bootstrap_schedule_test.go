package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestProberBootstrapInitialTaskRunsImmediately(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProberBootstrap(clientSession, tx)
		})

		runAt := proberBootstrapRunAt(t, ctx)
		if runAt.Before(before.Add(-time.Second)) || before.Add(5*time.Second).Before(runAt) {
			t.Fatalf("initial bootstrap run_at = %s, want immediate after %s", runAt, before)
		}
	})
}

func TestProberBootstrapStartupAdvancesOldSixHourSchedule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleProberBootstrapAt(clientSession, tx, before.Add(6*time.Hour))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProberBootstrap(clientSession, tx)
		})
		runAt := proberBootstrapRunAt(t, ctx)
		if runAt.Before(before.Add(-time.Second)) || before.Add(5*time.Second).Before(runAt) {
			t.Fatalf("startup did not advance the old six-hour task: run_at=%s", runAt)
		}
		var rows int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key = '["prober_bootstrap"]'`).Scan(&rows))
		})
		if rows != 1 {
			t.Fatalf("startup created %d bootstrap rows, want one", rows)
		}
	})
}

func TestProberBootstrapPostKeepsTheRecurringCadence(t *testing.T) {
	if ProberBootstrapTimeout > 5*time.Minute {
		t.Fatalf("crashed probe accounts may wait %s before the next cleanup", ProberBootstrapTimeout)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProberBootstrapPost(&ProberBootstrapArgs{}, &ProberBootstrapResult{}, clientSession, tx); err != nil {
				t.Fatalf("ProberBootstrapPost: %v", err)
			}
		})

		runAt := proberBootstrapRunAt(t, ctx)
		want := before.Add(ProberBootstrapTimeout)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("recurring bootstrap run_at = %s, want about %s", runAt, want)
		}
	})
}

func TestProberBootstrapReapsShardsWithoutRecreatingSharedCredit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		s := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer s.Cancel()
		if _, err := ProberBootstrap(&ProberBootstrapArgs{}, s); err != nil {
			t.Fatal(err)
		}
		if model.GetProberIdentity(ctx) != nil {
			t.Fatal("fresh deployment created a shared probe account")
		}
		// Simulate a pre-upgrade identity whose old shared grants have been
		// retired. The scheduled cleanup task must never recreate that credit.
		if _, err := model.BootstrapProberIdentity(s); err != nil {
			t.Fatal(err)
		}
		legacy := model.GetProberIdentity(ctx)
		key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: 1}
		if _, err := model.BeginProberShard(ctx, key, 4096, time.Hour); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE network_id=$1`, *legacy.NetworkId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run SET deadline=now()-interval '1 second',next_cleanup_time=now()-interval '1 second' WHERE task_id=$1 AND epoch=$2`, key.TaskId, key.Epoch))
		})
		if _, err := ProberBootstrap(&ProberBootstrapArgs{}, s); err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var grants int
			var closed bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_balance WHERE network_id=$1),
				(SELECT state='closed' FROM prober_shard_run WHERE task_id=$2 AND epoch=$3)`, *legacy.NetworkId, key.TaskId, key.Epoch).Scan(&grants, &closed))
			if grants != 0 || !closed {
				t.Fatal("recurring cleanup replenished shared credit or failed crash recovery")
			}
		})
	})
}

func proberBootstrapRunAt(t testing.TB, ctx context.Context) time.Time {
	t.Helper()
	var runAt time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT run_at FROM pending_task WHERE run_once_key = '["prober_bootstrap"]'`)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatal("no prober bootstrap task was scheduled")
			}
			server.Raise(result.Scan(&runAt))
		})
	})
	return runAt
}
