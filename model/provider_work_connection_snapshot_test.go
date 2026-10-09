// Real connection owners must read the session head after their endpoint wait.
// The held transaction and its visible lock edges establish each ordering.
package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Waits for the exact synthetic endpoint fence, without inspecting query text
// or inferring a blocked operation from a short absence-of-progress timeout.
func requireProviderWorkConnectionWaiters(t testing.TB, ctx context.Context, holder server.PgTx, clientId server.Id, want int) {
	t.Helper()
	for {
		var waiting int
		server.Raise(holder.QueryRow(ctx, `SELECT count(DISTINCT pid) FROM pg_locks
			WHERE locktype='advisory' AND NOT granted AND classid=776 AND objsubid=2
			AND objid::bigint=(('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int::bigint & 4294967295)
			AND pg_backend_pid()=ANY(pg_blocking_pids(pid))`, clientId).Scan(&waiting))
		if waiting == want {
			return
		}
		if waiting > want {
			t.Fatalf("synthetic endpoint has %d waiters, want %d", waiting, want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("endpoint wait barrier: got %d, want %d: %v", waiting, want, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Checks both the atomic journal and a fresh signed reservation after the
// owners join, so lower retry work cannot pass by losing a mutation or original.
func requireProviderWorkConnectionJournal(t testing.TB, f *providerWorkSessionFixture, wantSequence, wantConnected int) {
	t.Helper()
	server.Db(f.ctx, func(conn server.PgConn) {
		var sequence, events, originals, connected int
		server.Raise(conn.QueryRow(f.ctx, `SELECT h.sequence,
			(SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
			(SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1),
			(SELECT count(*) FROM network_client_connection WHERE client_id=$1 AND connected)
			FROM provider_work_session_head h WHERE h.client_id=$1`, f.sourceId).
			Scan(&sequence, &events, &originals, &connected))
		if sequence != wantSequence || events != wantSequence || originals != wantSequence || connected != wantConnected {
			t.Fatalf("journal sequence/events/originals/connected=%d/%d/%d/%d, want %d/%d/%d/%d",
				sequence, events, originals, connected, wantSequence, wantSequence, wantSequence, wantConnected)
		}
	})
	contractId := f.contract(t)
	receipts := providerWorkFixtureReceipts(t, f.ctx, contractId)
	reservation := providerWorkFixtureReservation(t, receipts, contractId)
	if !reservation.Reservation.Complete || reservation.Reservation.SourceHead.Sequence != uint64(wantSequence) {
		t.Fatal("current session mutations lost the complete reservation cut")
	}
	states := providerWorkFixtureEndpoint(t, f, receipts, f.sourceId)
	if len(states) != wantSequence || states[len(states)-1].ActiveConnections != uint32(wantConnected) {
		t.Fatal("signed session replay differs from the durable connection set")
	}
}

// Six same-client operations queue behind one actual uncommitted retirement.
// A default repeatable-read owner takes its snapshot before this wait, so the
// changed head forces a retry even though the endpoint fence already orders it.
func exerciseProviderWorkConnectionSnapshot(t testing.TB, retire, cancelWait bool) {
	t.Helper()
	f := newProviderWorkSessionFixture(t)
	ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancel()
	f.ctx = ctx
	const ownerCount = 6
	connectionIds := make([]server.Id, ownerCount)
	wantSequence, wantConnected := 2, 1
	if retire {
		for i := range connectionIds {
			var err error
			connectionIds[i], _, _, _, err = ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.12:10003", f.handlerId, 4, f.sourceNetworkId)
			server.Raise(err)
		}
		wantSequence += ownerCount
		wantConnected += ownerCount
	}

	holderConn := acquireContractLifecycleTestConnection(t, ctx)
	defer holderConn.Release()
	holder, err := holderConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	server.Raise(err)
	defer rollbackCloseReportTestTransaction(ctx, holder)
	providerWorkLockSessionMutationInTx(ctx, holder, f.sourceId)
	server.RaisePgResult(holder.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
	providerWorkRetainSessionEventsInTx(ctx, holder, f.sourceId)

	var reruns atomic.Int64
	workerCtx, stopWorkers := context.WithCancel(server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) }))
	type result struct {
		connectionId server.Id
		err          error
	}
	results := make(chan result, ownerCount)
	joined := make(chan struct{})
	var workers sync.WaitGroup
	for i := range ownerCount {
		workers.Go(func() {
			value := result{}
			server.HandleError(func() {
				if retire {
					value.connectionId = connectionIds[i]
					value.err = DisconnectNetworkClient(workerCtx, value.connectionId)
				} else {
					value.connectionId, _, _, _, value.err = ConnectNetworkClientWithIpFamily(workerCtx, f.sourceId, "192.0.2.13:10004", f.handlerId, 4, f.sourceNetworkId)
				}
			}, func(err error) { value.err = err })
			results <- value
		})
	}
	go func() { workers.Wait(); close(joined) }()
	defer func() {
		stopWorkers()
		rollbackCloseReportTestTransaction(ctx, holder)
		select {
		case <-joined:
		case <-time.After(35 * time.Second):
			t.Error("connection owners did not join canceled cleanup")
		}
	}()
	requireProviderWorkConnectionWaiters(t, ctx, holder, f.sourceId, ownerCount)
	if reruns.Load() != 0 {
		t.Fatal("an owner retried before the held endpoint was released")
	}
	// An unrelated endpoint still admits and retires while all six exact
	// source operations are waiting; no global bridge is being relaxed.
	independentId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, f.destinationId, "192.0.2.14:10005", f.handlerId, 4, f.destinationNetworkId)
	server.Raise(err)
	server.Raise(DisconnectNetworkClient(ctx, independentId))
	if cancelWait {
		stopWorkers()
	} else {
		server.Raise(holder.Commit(ctx))
		wantSequence++
		wantConnected--
	}
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("queued connection owners did not finish", ctx.Err())
	}
	seen := map[server.Id]bool{}
	for range ownerCount {
		value := <-results
		if cancelWait {
			if !errors.Is(value.err, context.Canceled) {
				t.Fatal("canceled endpoint waiter did not retain its cancellation", value.err)
			}
		} else {
			if value.err != nil || value.connectionId == (server.Id{}) || seen[value.connectionId] {
				t.Fatal("connection operation lost its distinct successful result", value.err)
			}
			seen[value.connectionId] = true
		}
	}
	if got := reruns.Load(); got != 0 {
		t.Fatalf("endpoint wait caused %d avoidable snapshot retries for %d connection owners", got, ownerCount)
	}
	if cancelWait {
		// The holder is still uncommitted throughout the canceled joins. Its
		// rollback must leave both its mutation and every queued operation absent.
		server.Raise(holder.Rollback(ctx))
	} else {
		wantSequence += ownerCount
		if retire {
			wantConnected -= ownerCount
		} else {
			wantConnected += ownerCount
		}
	}
	requireProviderWorkConnectionJournal(t, f, wantSequence, wantConnected)
}

// Admission keeps the exclusive fence but starts a fresh statement snapshot
// after that fence, preserving all six distinct signed connection births.
func TestProviderWorkConnectionAdmissionWaitUsesCurrentHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseProviderWorkConnectionSnapshot(t, false, false) })
}

// Retirement must not replay a completed lock wait merely because another
// retirement advanced the same endpoint head while this owner was queued.
func TestProviderWorkConnectionRetirementWaitUsesCurrentHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) { exerciseProviderWorkConnectionSnapshot(t, true, false) })
}

// Cancellation joins the actual admission and retirement transaction owners
// while the endpoint remains held, with no connection or journal mutation.
func TestProviderWorkConnectionCanceledWaitLeavesNoJournal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, retire := range []bool{false, true} {
			exerciseProviderWorkConnectionSnapshot(t, retire, true)
		}
	})
}

// Maintenance defers an owned endpoint, then reads its committed current head
// on the next pass while retaining the live-handler eligibility check.
func TestProviderWorkHandlerRetirementDefersBusyHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		f.ctx = ctx
		orphanHandlerId := CreateNetworkClientHandler(ctx)
		orphanId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.15:10006", orphanHandlerId, 4, f.sourceNetworkId)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE handler_id=$1`, orphanHandlerId))
		})
		holderConn := acquireContractLifecycleTestConnection(t, ctx)
		defer holderConn.Release()
		holder, err := holderConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer rollbackCloseReportTestTransaction(ctx, holder)
		providerWorkLockSessionMutationInTx(ctx, holder, f.sourceId)
		server.RaisePgResult(holder.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
		providerWorkRetainSessionEventsInTx(ctx, holder, f.sourceId)
		var reruns atomic.Int64
		workerCtx, stopWorker := context.WithCancel(server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) }))
		result := make(chan error, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			var resultErr error
			if recovered := server.HandleError(func() { CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour)) }); recovered != nil {
				resultErr = fmt.Errorf("handler retirement: %v", recovered)
			}
			result <- resultErr
		}()
		defer func() {
			stopWorker()
			rollbackCloseReportTestTransaction(ctx, holder)
			select {
			case <-joined:
			case <-time.After(35 * time.Second):
				t.Error("handler retirement did not join canceled cleanup")
			}
		}()
		awaitHandlerRetirementWithoutWait(t, ctx, holder, result)
		if !GetNetworkClientConnectionStatus(ctx, orphanId).Connected {
			t.Fatal("busy endpoint was retired without its fence")
		}
		server.Raise(holder.Commit(ctx))
		select {
		case <-joined:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		CloseExpiredNetworkClientHandlers(workerCtx, server.NowUtc().Add(-time.Hour))
		if got := reruns.Load(); got != 0 {
			t.Fatalf("handler endpoint wait caused %d avoidable snapshot retries", got)
		}
		if GetNetworkClientConnectionStatus(ctx, orphanId).Connected || !GetNetworkClientConnectionStatus(ctx, f.destinationConnectionId).Connected {
			t.Fatal("handler cleanup lost orphan retirement or changed a live handler")
		}
		requireProviderWorkConnectionJournal(t, f, 5, 0)
	})
}
