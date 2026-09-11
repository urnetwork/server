package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
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

func TestProberBootstrapPostKeepsTheRecurringCadence(t *testing.T) {
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
