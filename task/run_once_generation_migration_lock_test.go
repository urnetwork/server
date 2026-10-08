// A held relation proves bounded DDL refusal leaves the published head intact.
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

func TestRunOnceMigrationRefusesHeldRelationAndRecovers(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		server.ApplyDbMigrationsUpTo(ctx, 795)
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer holder.Release()
		heldTx, err := holder.Begin(ctx)
		server.Raise(err)
		released := false
		defer func() {
			if !released {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := heldTx.Rollback(cleanupCtx); err != nil {
					t.Error("join synthetic relation holder", err)
				}
			}
		}()
		server.RaisePgResult(heldTx.Exec(ctx, `LOCK TABLE pending_task IN ACCESS SHARE MODE`))
		var actualHolder bool
		server.Raise(heldTx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
		 WHERE pid=pg_backend_pid() AND locktype='relation'
		 AND relation='pending_task'::regclass AND mode='AccessShareLock' AND granted)`).Scan(&actualHolder))
		if !actualHolder {
			t.Fatal("migration control did not hold its actual conflicting relation lock")
		}

		var migrationErr error
		server.HandleError(func() { server.ApplyDbMigrationsUpTo(ctx, 796) }, func(err error) { migrationErr = err })
		var pgErr *pgconn.PgError
		if !errors.As(migrationErr, &pgErr) || pgErr.Code != "55P03" || ctx.Err() != nil {
			t.Fatal("held relation did not reach the migration's native lock refusal", migrationErr, ctx.Err())
		}
		if version := server.DbVersion(ctx); version != 795 {
			t.Fatal("refused DDL advanced the published migration head", version)
		}
		columns := func() int {
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_attribute
				 WHERE attrelid='pending_task'::regclass AND NOT attisdropped
				 AND attname IN ('run_once_generation','run_once_wake_at','claim_generation')`).Scan(&count))
			})
			return count
		}
		if count := columns(); count != 0 {
			t.Fatal("refused DDL partially installed generation columns", count)
		}
		server.Raise(heldTx.Rollback(ctx))
		released = true
		server.ApplyDbMigrationsUpTo(ctx, 796)
		if version := server.DbVersion(ctx); version != 796 || columns() != 3 {
			t.Fatal("explicit retry after holder release did not publish the whole schema", version)
		}
	})
}
