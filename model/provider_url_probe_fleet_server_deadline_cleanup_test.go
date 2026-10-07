package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// A server timeout must retain SQLSTATE 57014, abort the owned statement, and
// restore the session setting on the exact same connection after rollback.
func TestProviderUrlProbeFleetServerTimeoutCauseAndReuse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			var original string
			server.Raise(conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&original))
			defer func() {
				server.RaisePgResult(conn.Exec(ctx, "SELECT set_config('statement_timeout',$1,false)", original))
			}()
			server.RaisePgResult(conn.Exec(ctx, "SET statement_timeout='200ms'"))
			pid := conn.Conn().PgConn().PID()
			readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			failure := server.HandleError(func() {
				providerUrlProbeFleetRead(readCtx, conn, func(tx server.PgTx) {
					rows, err := tx.Query(readCtx, "SELECT pg_sleep(30)")
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
						}
					})
				})
			})
			err, ok := failure.(error)
			var pgErr *pgconn.PgError
			if !ok || !errors.As(err, &pgErr) || pgErr.Code != "57014" || readCtx.Err() != nil {
				t.Fatalf("server statement timeout cause was not retained: %v", failure)
			}
			var restored string
			server.Raise(conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&restored))
			if restored != "200ms" || conn.Conn().PgConn().PID() != pid || conn.Conn().PgConn().TxStatus() != 'I' {
				t.Fatal("server timeout cleanup did not restore the same idle backend")
			}
			testingCheckUrlCensusJitRestored(ctx, conn, "on")
		})
	})
}

// Existing callers with no deadline keep the inherited limit. The production
// publisher always supplies 10s; this helper must not invent a wider budget.
func TestProviderUrlProbeFleetServerDeadlineNoDeadlineKeepsLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			var original string
			server.Raise(conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&original))
			defer func() {
				server.RaisePgResult(conn.Exec(ctx, "SELECT set_config('statement_timeout',$1,false)", original))
			}()
			server.RaisePgResult(conn.Exec(ctx, "SET statement_timeout='200ms'"))
			providerUrlProbeFleetRead(context.WithoutCancel(ctx), conn, func(tx server.PgTx) {
				var actual string
				server.Raise(tx.QueryRow(ctx, "SHOW statement_timeout").Scan(&actual))
				if actual != "200ms" {
					t.Fatal("unbounded caller widened or replaced the inherited statement timeout")
				}
			})
			testingCheckUrlCensusJitRestored(ctx, conn, "on")
		})
	})
}

func TestProviderUrlProbeFleetServerDeadlineCanceledBeforeBegin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testingUrlCensusJitConnection(t, func(ctx context.Context, conn server.PgConn) {
			readCtx, cancel := context.WithCancel(ctx)
			cancel()
			called := false
			failure := server.HandleError(func() {
				providerUrlProbeFleetRead(readCtx, conn, func(server.PgTx) { called = true })
			})
			err, ok := failure.(error)
			if called || !ok || !errors.Is(err, context.Canceled) {
				t.Fatal("canceled census admitted a transaction callback or lost cancellation cause")
			}
			if conn.Conn().IsClosed() || conn.Conn().PgConn().TxStatus() != 'I' {
				t.Fatal("canceled-before-begin census changed the idle connection")
			}
		})
	})
}
