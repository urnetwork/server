// Connection lifecycles already own the endpoint fence and a current snapshot.
package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// KEY SHARE permits the journal's non-key sequence update. A preliminary
// FOR UPDATE unnecessarily makes actual reconnect/retire owners wait on it.
func TestProviderWorkConnectionOverlappingLifecyclesAvoidExtraHeadLock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		f.ctx = ctx
		const ownerCount = 6
		retireIds := make([]server.Id, ownerCount/2)
		for i := range retireIds {
			var err error
			retireIds[i], _, _, _, err = ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.17:10007", f.handlerId, 4, f.sourceNetworkId)
			server.Raise(err)
		}
		holderConn := acquireContractLifecycleTestConnection(t, ctx)
		defer holderConn.Release()
		holder, err := holderConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		var head server.Id
		server.Raise(holder.QueryRow(ctx, `SELECT client_id FROM provider_work_session_head WHERE client_id=$1 FOR KEY SHARE`, f.sourceId).Scan(&head))
		if head != f.sourceId {
			t.Fatal("synthetic head holder changed identity")
		}
		var reruns atomic.Int64
		workerCtx, stopWorkers := context.WithCancel(server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) }))
		errorsByOwner := make(chan error, ownerCount)
		start := make(chan struct{})
		joined := make(chan struct{})
		var workers sync.WaitGroup
		for i := range ownerCount {
			workers.Go(func() {
				<-start
				var operationErr error
				server.HandleError(func() {
					if i < len(retireIds) {
						operationErr = DisconnectNetworkClient(workerCtx, retireIds[i])
					} else {
						_, _, _, _, operationErr = ConnectNetworkClientWithIpFamily(workerCtx, f.sourceId, "192.0.2.18:10008", f.handlerId, 4, f.sourceNetworkId)
					}
				}, func(err error) { operationErr = err })
				errorsByOwner <- operationErr
			})
		}
		go func() { workers.Wait(); close(joined) }()
		defer func() {
			stopWorkers()
			rollbackCloseReportTestTransaction(ctx, holder)
			select {
			case <-joined:
			case <-time.After(35 * time.Second):
				t.Error("connection lifecycle owners did not join cleanup")
			}
		}()
		close(start)
		for {
			select {
			case <-joined:
				goto complete
			default:
			}
			var blocked int
			server.Raise(holder.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname=current_database() AND pg_backend_pid()=ANY(pg_blocking_pids(pid))`).Scan(&blocked))
			if blocked != 0 {
				t.Fatal("read-committed lifecycle took an unnecessary head lock behind KEY SHARE")
			}
			select {
			case <-joined:
				goto complete
			case <-ctx.Done():
				t.Fatal("connection lifecycle did not finish under compatible head reader", ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	complete:
		for range ownerCount {
			if err := <-errorsByOwner; err != nil {
				t.Fatal("overlapping connection lifecycle failed", err)
			}
		}
		if reruns.Load() != 0 {
			t.Fatal("compatible head reader caused a transaction rerun")
		}
		// The holder remains active through this exact durable and signed cut.
		requireProviderWorkConnectionJournal(t, f, 2+len(retireIds)+ownerCount, 1+len(retireIds))
	})
}

// Generic repeatable-read callers still need the old head-row guard: an
// advisory lock alone cannot advance a snapshot taken before a new admission.
func TestProviderWorkSessionMutationRetainsRepeatableReadHeadGuard(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		old, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, old)
		var sequence int
		server.Raise(old.QueryRow(ctx, `SELECT sequence FROM provider_work_session_head WHERE client_id=$1`, f.sourceId).Scan(&sequence))
		if sequence != 2 {
			t.Fatal("old snapshot did not start at the fixture head")
		}
		_, _, _, _, err = ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.19:10009", f.handlerId, 4, f.sourceNetworkId)
		server.Raise(err)
		var observedErr error
		server.HandleError(func() {
			providerWorkLockSessionMutationInTx(ctx, old, f.sourceId)
		}, func(err error) { observedErr = err })
		var pgErr *pgconn.PgError
		if !errors.As(observedErr, &pgErr) || pgErr.Code != "40001" {
			t.Fatal("old repeatable-read mutation lost its stale-head refusal", observedErr)
		}
	})
}
