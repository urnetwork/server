// Actual cleanup owners must finish or defer while a synthetic lock is held.
// Positive pg_locks edges make the old waiting behavior a causal failure;
// signed journals, fresh handler checks and replay verify what still commits.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Completion wins only after the actual method returns. A positive lock edge
// is an immediate failure, rather than inferring progress from a short sleep.
func awaitHandlerRetirementWithoutWait(t testing.TB, ctx context.Context, holder server.PgTx, result <-chan error, earlierClientIds ...server.Id) {
	t.Helper()
	for {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal("handler retirement failed", err)
			}
			return
		default:
		}
		var waiting bool
		server.Raise(holder.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
		 WHERE NOT granted AND pg_backend_pid()=ANY(pg_blocking_pids(pid)))`).Scan(&waiting))
		if waiting {
			for _, clientId := range earlierClientIds {
				var ownsEarlier bool
				server.Raise(holder.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks owned
				 WHERE owned.granted AND owned.locktype='advisory' AND owned.classid=776 AND owned.objsubid=2
				 AND owned.objid::bigint=(('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int::bigint & 4294967295)
				 AND pg_backend_pid()=ANY(pg_blocking_pids(owned.pid)))`, clientId).Scan(&ownsEarlier))
				t.Logf("waiting cleanup retains independent endpoint fence=%t", ownsEarlier)
			}
			t.Fatal("handler cleanup waited behind the held owner instead of deferring its page")
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatal("handler retirement failed", err)
			}
			return
		case <-ctx.Done():
			t.Fatal("handler retirement did not reach its terminal state", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Mint through the real owner before removing the ephemeral handler. The
// resulting orphan retains a signed admission and the normal journal trigger.
func handlerRetirementTestOrphan(t testing.TB, f *providerWorkSessionFixture, clientId, networkId server.Id) (server.Id, server.Id) {
	t.Helper()
	handlerId := CreateNetworkClientHandler(f.ctx)
	connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, clientId, "192.0.2.21:12000", handlerId, 4, networkId)
	server.Raise(err)
	server.Tx(f.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client_handler WHERE handler_id=$1`, handlerId))
	})
	return connectionId, handlerId
}

// Holding the later advisory hash must not retain an earlier endpoint fence.
// The independent endpoint admits a real connection before the holder ends.
func TestHandlerRetirementDoesNotConvoyAcrossEndpoints(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		f.ctx = ctx
		server.Db(ctx, func(conn server.PgConn) {
			var sourceFirst bool
			server.Raise(conn.QueryRow(ctx, `SELECT
			 ('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int <
			 ('x'||substr(md5($2::uuid::text),1,8))::bit(32)::int`, f.sourceId, f.destinationId).Scan(&sourceFirst))
			if !sourceFirst {
				f.sourceId, f.destinationId = f.destinationId, f.sourceId
				f.sourceNetworkId, f.destinationNetworkId = f.destinationNetworkId, f.sourceNetworkId
				f.sourceConnectionId, f.destinationConnectionId = f.destinationConnectionId, f.sourceConnectionId
			}
		})
		sourceOrphan, _ := handlerRetirementTestOrphan(t, f, f.sourceId, f.sourceNetworkId)
		destinationOrphan, _ := handlerRetirementTestOrphan(t, f, f.destinationId, f.destinationNetworkId)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		if !providerWorkLockSessionMutationInTx(ctx, holder, f.destinationId) {
			t.Fatal("synthetic last endpoint fence unavailable")
		}
		var reruns atomic.Int64
		workerCtx, stop := context.WithCancel(server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) }))
		result := make(chan error, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			var resultErr error
			if recovered := server.HandleError(func() { CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour)) }); recovered != nil {
				resultErr = fmt.Errorf("cleanup owner: %v", recovered)
			}
			result <- resultErr
		}()
		defer func() {
			stop()
			rollbackCloseReportTestTransaction(ctx, holder)
			select {
			case <-joined:
			case <-time.After(35 * time.Second):
				t.Error("cleanup owner did not join canceled teardown")
			}
		}()
		awaitHandlerRetirementWithoutWait(t, ctx, holder, result, f.sourceId)
		if GetNetworkClientConnectionStatus(ctx, sourceOrphan).Connected || !GetNetworkClientConnectionStatus(ctx, destinationOrphan).Connected {
			t.Fatal("independent orphan did not retire or busy orphan was lost")
		}
		connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.22:12001", f.handlerId, 4, f.sourceNetworkId)
		server.Raise(err)
		server.Raise(DisconnectNetworkClient(ctx, connectionId))
		server.Raise(holder.Commit(ctx))
		CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour))
		if GetNetworkClientConnectionStatus(ctx, destinationOrphan).Connected || reruns.Load() != 0 {
			t.Fatal("deferred orphan did not recover without transaction replay", reruns.Load())
		}
		requireProviderWorkConnectionJournal(t, f, 6, 1)
	})
}

// A row-first rolling owner must not deadlock with maintenance holding its
// endpoint. After release the next pass retires that same original exactly once.
func TestHandlerRetirementSkipsHeldConnectionRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		f.ctx = ctx
		orphanId, _ := handlerRetirementTestOrphan(t, f, f.sourceId, f.sourceNetworkId)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		server.RaisePgResult(holder.Exec(ctx, `SELECT connection_id FROM network_client_connection WHERE connection_id=$1 FOR UPDATE`, orphanId))
		var reruns atomic.Int64
		workerCtx, stop := context.WithCancel(server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) }))
		result := make(chan error, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			var resultErr error
			if recovered := server.HandleError(func() { CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour)) }); recovered != nil {
				resultErr = fmt.Errorf("cleanup owner: %v", recovered)
			}
			result <- resultErr
		}()
		defer func() {
			stop()
			rollbackCloseReportTestTransaction(ctx, holder)
			select {
			case <-joined:
			case <-time.After(35 * time.Second):
				t.Error("cleanup owner did not join canceled teardown")
			}
		}()
		awaitHandlerRetirementWithoutWait(t, ctx, holder, result)
		if !GetNetworkClientConnectionStatus(ctx, orphanId).Connected {
			t.Fatal("locked orphan changed before its owner ended")
		}
		var acquired bool
		server.Raise(holder.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(776,('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int)`, f.sourceId).Scan(&acquired))
		if !acquired {
			t.Fatal("deferred page retained its endpoint fence")
		}
		server.Raise(holder.Commit(ctx))
		CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour))
		CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour))
		if reruns.Load() != 0 {
			t.Fatal("row deferral replayed a transaction", reruns.Load())
		}
		requireProviderWorkConnectionJournal(t, f, 4, 1)
	})
}

// The client-count bound alone did not bound its connection mutations. A page
// now commits at most 64 retirements; the ordinary next pass drains the suffix.
func TestHandlerRetirementBoundsPerEndpointJournal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 60*time.Second)
		defer cancel()
		f.ctx = ctx
		handlerId := CreateNetworkClientHandler(ctx)
		const count = 65
		for range count {
			_, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.23:12002", handlerId, 4, f.sourceNetworkId)
			server.Raise(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id=$1`, handlerId))
		})
		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-time.Hour))
		requireProviderWorkConnectionJournal(t, f, 2+count+64, 2)
		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-time.Hour))
		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-time.Hour))
		requireProviderWorkConnectionJournal(t, f, 2+2*count, 1)
	})
}

// A discovery result is not mutation authority: a fresh handler, a changed
// client, a removed row and a replay must all retain their actual current state.
func TestHandlerRetirementRechecksDiscoveredRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		orphanId, handlerId := handlerRetirementTestOrphan(t, f, f.sourceId, f.sourceNetworkId)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_client_handler(handler_id,heartbeat_time,handler_host) VALUES($1,$2,'restored.example')`, handlerId, server.NowUtc()))
		})
		at := server.NowUtc()
		retireNetworkClientHandlerPage(f.ctx, f.sourceId, []server.Id{orphanId}, at)
		if !GetNetworkClientConnectionStatus(f.ctx, orphanId).Connected {
			t.Fatal("restored handler was ignored by the fresh eligibility check")
		}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client_handler WHERE handler_id=$1`, handlerId))
		})
		retireNetworkClientHandlerPage(f.ctx, f.destinationId, []server.Id{orphanId, server.NewId()}, at)
		if !GetNetworkClientConnectionStatus(f.ctx, orphanId).Connected {
			t.Fatal("discovery from another client authorized retirement")
		}
		retireNetworkClientHandlerPage(f.ctx, f.sourceId, []server.Id{orphanId}, at)
		retireNetworkClientHandlerPage(f.ctx, f.sourceId, []server.Id{orphanId}, at.Add(time.Hour))
		server.Db(f.ctx, func(conn server.PgConn) {
			var actual time.Time
			server.Raise(conn.QueryRow(f.ctx, `SELECT disconnect_time FROM network_client_connection WHERE connection_id=$1`, orphanId).Scan(&actual))
			if !actual.Equal(at.Truncate(time.Microsecond)) {
				t.Fatal("replayed page changed the committed retirement time")
			}
		})
		requireProviderWorkConnectionJournal(t, f, 4, 1)
	})
}

// A refused page rolls its connection and original event back together. It
// does not replay inside the lock owner; a later ordinary pass owns recovery.
func TestHandlerRetirementFailureAndCancellationLeaveNoPartialJournal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		orphanId, _ := handlerRetirementTestOrphan(t, f, f.sourceId, f.sourceNetworkId)
		conn := acquireContractLifecycleTestConnection(t, f.ctx)
		defer conn.Release()
		server.RaisePgResult(conn.Exec(f.ctx, `CREATE FUNCTION pg_temp.refuse_handler_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'synthetic retirement refusal' USING ERRCODE='40001'; END; $$`))
		server.RaisePgResult(conn.Exec(f.ctx, fmt.Sprintf(`CREATE TRIGGER zz_test_handler_retirement_refusal
		 AFTER UPDATE ON network_client_connection FOR EACH ROW
		 WHEN (OLD.connection_id='%s'::uuid) EXECUTE FUNCTION pg_temp.refuse_handler_retirement()`, orphanId)))
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			server.RaisePgResult(conn.Exec(cleanup, `DROP TRIGGER IF EXISTS zz_test_handler_retirement_refusal ON network_client_connection; DROP FUNCTION IF EXISTS pg_temp.refuse_handler_retirement()`))
		}()
		var reruns atomic.Int64
		ctx, cancel := context.WithTimeout(server.Testing_WithTxRerunHook(f.ctx, func() { reruns.Add(1) }), 20*time.Second)
		defer cancel()
		var failure error
		server.HandleError(func() { retireNetworkClientHandlerPage(ctx, f.sourceId, []server.Id{orphanId}, server.NowUtc()) }, func(err error) { failure = err })
		var pgErr *pgconn.PgError
		if !errors.As(failure, &pgErr) || pgErr.Code != "40001" || reruns.Load() != 0 {
			t.Fatal("page refusal lost its cause or replayed the owner", failure, reruns.Load())
		}
		requireProviderWorkConnectionJournal(t, f, 3, 2)
		server.RaisePgResult(conn.Exec(f.ctx, `DROP TRIGGER zz_test_handler_retirement_refusal ON network_client_connection`))
		stoppedCtx, stop := context.WithCancel(ctx)
		stop()
		failure = nil
		server.HandleError(func() { retireNetworkClientHandlerPage(stoppedCtx, f.sourceId, []server.Id{orphanId}, server.NowUtc()) }, func(err error) { failure = err })
		if !errors.Is(failure, context.Canceled) {
			t.Fatal("page cancellation lost its cause", failure)
		}
		requireProviderWorkConnectionJournal(t, f, 3, 2)
		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-time.Hour))
		requireProviderWorkConnectionJournal(t, f, 4, 1)
	})
}

// The real savepoint sees a native non-schema SQL error at the head stage.
// Its rollback must not be mistaken for permission to retire without a fence.
type handlerRetirementFaultTx struct {
	server.PgTx
}

// Preserve the wrapper when the production owner opens its optional savepoint.
func (self *handlerRetirementFaultTx) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := self.PgTx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &handlerRetirementFaultTx{PgTx: tx}, nil
}

// All other SQL is real and unchanged; division by zero aborts this savepoint.
func (self *handlerRetirementFaultTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "WITH owned AS (") {
		return self.PgTx.QueryRow(ctx, `SELECT 1/0`)
	}
	return self.PgTx.QueryRow(ctx, query, args...)
}

func TestHandlerRetirementRejectsUnexpectedFenceError(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		orphanId, _ := handlerRetirementTestOrphan(t, f, f.sourceId, f.sourceNetworkId)
		var failure error
		var reruns atomic.Int64
		ctx := server.Testing_WithTxRerunHook(f.ctx, func() { reruns.Add(1) })
		server.HandleError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				retireNetworkClientHandlerPageInTx(ctx, &handlerRetirementFaultTx{PgTx: tx}, f.sourceId, []server.Id{orphanId}, server.NowUtc())
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { failure = err })
		var pgErr *pgconn.PgError
		if !errors.As(failure, &pgErr) || pgErr.Code != "22012" || reruns.Load() != 0 {
			t.Fatal("non-schema fence error was swallowed or replayed", failure, reruns.Load())
		}
		requireProviderWorkConnectionJournal(t, f, 3, 2)
		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc().Add(-time.Hour))
		requireProviderWorkConnectionJournal(t, f, 4, 1)
	})
}
