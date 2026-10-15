// The handler connection reconcile's retire must defer a row whose endpoint
// is busy, as handler expiry does, and never queue behind its fence: a queued
// retire holds a pooled connection and the reconcile's pass for as long as
// the owner keeps the fence. A positive pg_locks wait edge is the failure.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Waits for the retire to return, failing at once if it waits on a lock the
// holder keeps.
func awaitRetireWithoutWait(t testing.TB, ctx context.Context, holder server.PgTx, result <-chan error) {
	t.Helper()
	for {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal("retire failed:", err)
			}
			return
		default:
		}
		var waiting bool
		server.Raise(holder.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
		 WHERE NOT granted AND pg_backend_pid()=ANY(pg_blocking_pids(pid)))`).Scan(&waiting))
		if waiting {
			t.Fatal("the reconcile's retire waited behind the held endpoint fence instead of deferring the row")
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatal("retire failed:", err)
			}
			return
		case <-ctx.Done():
			t.Fatal("the retire did not return", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A session owner holds the endpoint fence of a live handler's row. The
// retire defers the row without waiting and leaves it connected; once the
// owner ends, the retire disconnects it with the given time.
func TestRetireNetworkClientHandlerConnectionDefersBehindAHeldEndpoint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		f.ctx = ctx
		connectionId := f.sourceConnectionId

		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		var acquired bool
		server.Raise(holder.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(776,('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int)`, f.sourceId).Scan(&acquired))
		if !acquired {
			t.Fatal("fixture: the holder must take the endpoint fence")
		}

		var retired, deferred bool
		result := make(chan error, 1)
		go func() {
			var resultErr error
			if recovered := server.HandleError(func() {
				retired, deferred = RetireNetworkClientHandlerConnection(ctx, f.handlerId, connectionId, server.NowUtc())
			}); recovered != nil {
				resultErr = fmt.Errorf("%v", recovered)
			}
			result <- resultErr
		}()
		awaitRetireWithoutWait(t, ctx, holder, result)
		if retired || !deferred {
			t.Fatalf("a retire behind a held endpoint returned retired %t deferred %t, want a deferral", retired, deferred)
		}
		if !GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatal("the deferred row changed while its endpoint was held")
		}

		server.Raise(holder.Rollback(ctx))
		disconnectTime := server.NowUtc().Add(-time.Minute).Truncate(time.Millisecond)
		retired, deferred = RetireNetworkClientHandlerConnection(ctx, f.handlerId, connectionId, disconnectTime)
		if !retired || deferred {
			t.Fatalf("the retire after the owner ended returned retired %t deferred %t, want retired", retired, deferred)
		}
		if GetNetworkClientConnectionStatus(ctx, connectionId).Connected {
			t.Fatal("the retired row is still connected")
		}
		var recorded time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT disconnect_time FROM network_client_connection WHERE connection_id = $1`, connectionId).Scan(&recorded))
		})
		if !recorded.Equal(disconnectTime) {
			t.Fatalf("the retire recorded disconnect time %s, want the given %s", recorded, disconnectTime)
		}
	})
}
