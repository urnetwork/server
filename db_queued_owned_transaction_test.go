// A queued admission waits in its key's lock queue ahead of later probes, for
// a bounded time, holding nothing else, and gives the key up after its bound.
package server

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// Whether the backend waits for an advisory lock. The queued session holds no
// other lock, so its one waiting advisory lock is the key.
func queuedOwnedTxTestWaiting(t testing.TB, ctx context.Context, pid uint32) (waiting bool) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, pid).Scan(&waiting))
	}, OptNoRetry())
	return
}

// The holder's release admits the queued session before a probe that follows
// the release at once, which a probe alone could not win. The queued business
// transaction runs once with the key held, and the session's lock bound ends
// with its admission.
func TestQueuedOwnedTxAdmitsAheadOfLaterProbes(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("synthetic-queued-owned", NewId())
		held, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		holder := startOwnedTransactionTest(func() {
			OwnedTx(ctx, []PgOwnershipKey{key}, func(PgTx) {
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted, OptNoRetry())
		})
		defer func() {
			resume()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			holder.join(t, cleanup)
		}()
		ownedTransactionTestAwait(t, ctx, held)

		var stateLock sync.Mutex
		var events []PgOwnershipEvent
		observed := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
			stateLock.Lock()
			defer stateLock.Unlock()
			events = append(events, event)
		})
		entered, finish := make(chan struct{}), make(chan struct{})
		var finishOnce sync.Once
		proceed := func() { finishOnce.Do(func() { close(finish) }) }
		defer proceed()
		var admitted bool
		queued := startOwnedTransactionTest(func() {
			admitted = QueuedOwnedTx(observed, key, 20*time.Second, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,17)`))
				close(entered)
				select {
				case <-finish:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted, OptNoRetry())
		})
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			stateLock.Lock()
			var pid uint32
			if 0 < len(events) && events[0].Kind == PgOwnershipWaiting {
				pid = events[0].BackendPid
			}
			stateLock.Unlock()
			if pid != 0 && queuedOwnedTxTestWaiting(t, ctx, pid) {
				break
			}
			select {
			case <-queued.done:
				t.Fatal("the queued admission returned while the key was held", queued.recovered, admitted)
			case <-ctx.Done():
				t.Fatal("the queued admission did not wait in the key's queue", ctx.Err())
			case <-ticker.C:
			}
		}
		if TryOwnedTx(ctx, []PgOwnershipKey{key}, func(PgTx) {}, TxReadCommitted, OptNoRetry()) {
			t.Fatal("a probe was admitted while the key was held")
		}
		resume()
		if recovered := holder.join(t, ctx); recovered != nil {
			t.Fatal("the holder did not finish", recovered)
		}
		ownedTransactionTestAwait(t, ctx, entered)
		if TryOwnedTx(ctx, []PgOwnershipKey{key}, func(PgTx) {}, TxReadCommitted, OptNoRetry()) {
			t.Fatal("a probe after the holder's release was admitted ahead of the queued session")
		}
		proceed()
		if recovered := queued.join(t, ctx); recovered != nil || !admitted {
			t.Fatal("the queued admission did not run its transaction", recovered, admitted)
		}
		stateLock.Lock()
		kinds := []PgOwnershipEventKind{}
		for _, event := range events {
			kinds = append(kinds, event.Kind)
		}
		stateLock.Unlock()
		if !slices.Equal(kinds, []PgOwnershipEventKind{PgOwnershipWaiting, PgOwnershipAdmitted, PgOwnershipReleased}) {
			t.Fatal("the queued admission did not wait, admit and release once", kinds)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND sum(amount)=17 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("the queued transaction did not commit its effect once")
			}
		}, OptNoRetry())
	})
}

// A key held past the bound: the wait ends with a refusal, no business work,
// no key and no lock bound left on the session, and the checkout returned.
func TestQueuedOwnedTxRefusesAfterItsBound(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		pop := Config.PushSimpleResource(MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		defer pop()
		safeMaintenancePool.reset()
		defer safeMaintenancePool.reset()
		key := NewPgOwnershipKey("synthetic-queued-owned", NewId())
		held, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		holder := startOwnedTransactionTest(func() {
			OwnedTx(ctx, []PgOwnershipKey{key}, func(PgTx) {
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted, OptNoRetry())
		})
		defer func() {
			resume()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			holder.join(t, cleanup)
		}()
		ownedTransactionTestAwait(t, ctx, held)
		var events []PgOwnershipEvent
		var refusedAcquired int32
		observed := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
			events = append(events, event)
			if event.Kind == PgOwnershipRefused {
				refusedAcquired = safeMaintenancePool.open().Stat().AcquiredConns()
			}
		})
		calls := 0
		if QueuedOwnedTx(observed, key, 10*time.Millisecond, func(PgTx) { calls++ }, TxReadCommitted, OptNoRetry()) ||
			calls != 0 || len(events) != 2 || events[0].Kind != PgOwnershipWaiting || events[1].Kind != PgOwnershipRefused || refusedAcquired != 1 {
			t.Fatal("a wait past its bound entered business or kept its checkout", calls, refusedAcquired, events)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			// The holder has the pool's other session, so this is the one the
			// refused wait returned.
			var pid uint32
			var remaining int
			var unbounded bool
			Raise(conn.QueryRow(ctx, `SELECT pg_backend_pid(),
				(SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'),
				(SELECT setting=reset_val FROM pg_settings WHERE name='lock_timeout')`).Scan(&pid, &remaining, &unbounded))
			if pid != events[0].BackendPid || remaining != 0 || !unbounded {
				t.Fatal("the refused wait left a key or its lock bound on its session", pid, events[0].BackendPid, remaining, unbounded)
			}
		}, OptNoRetry())
		resume()
		if recovered := holder.join(t, ctx); recovered != nil {
			t.Fatal("the holder did not finish", recovered)
		}
		if !QueuedOwnedTx(ctx, key, 10*time.Millisecond, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(2,23)`))
		}, TxReadCommitted, OptNoRetry()) {
			t.Fatal("a free key was not admitted by its probe")
		}
	})
}
