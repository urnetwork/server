// Real backend ownership controls keep admission separate from business SQL.
package server

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func ownedTransactionTestEnv(t *testing.T, run func(testing.TB, context.Context)) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE owned_tx_effect(id integer PRIMARY KEY,amount integer NOT NULL)`))
		}, OptReadWrite(), OptNoRetry())
		run(t, ctx)
	})
}

type ownedTransactionTestRun struct {
	done      chan struct{}
	recovered any
}

func startOwnedTransactionTest(run func()) *ownedTransactionTestRun {
	result := &ownedTransactionTestRun{done: make(chan struct{})}
	go func() {
		defer close(result.done)
		result.recovered = captureDbErrorPanic(run)
	}()
	return result
}

func (self *ownedTransactionTestRun) join(t testing.TB, ctx context.Context) any {
	t.Helper()
	select {
	case <-self.done:
		return self.recovered
	case <-ctx.Done():
		t.Fatal("owned transaction did not join", ctx.Err())
		return nil
	}
}

func ownedTransactionTestAwait(t testing.TB, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("owned transaction barrier did not arrive", ctx.Err())
	}
}

func requirePgOwnershipHeld(t testing.TB, ctx context.Context, pid uint32, key PgOwnershipKey) {
	t.Helper()
	Db(ctx, func(conn PgConn) {
		var held bool
		Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
            AND pid=$1 AND classid=$2::oid AND objid=$3::oid AND objsubid=2 AND granted)`,
			pid, int64(uint32(key.first)), int64(uint32(key.second))).Scan(&held))
		if !held {
			t.Fatal("actual transaction backend does not hold the required ownership key")
		}
	}, OptNoRetry())
}

func releaseOwnedTransactionTestKeys(ctx context.Context, conn PgConn) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	RaisePgResult(conn.Exec(cleanup, `SELECT pg_advisory_unlock_all()`))
}

// Two direct pool slots suffice: a positively refused peer releases its slot
// before waiting, allowing unrelated work to commit while the owner is held.
func TestOwnedTxBusyAdmissionReleasesPoolBeforeIndependentBusiness(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		pop := Config.PushSimpleResource(MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		defer pop()
		safeMaintenancePool.reset()
		defer safeMaintenancePool.reset()
		key := NewPgOwnershipKey("owned-tx-test", NewId())
		entered, release := make(chan struct{}), make(chan struct{})
		waiting, releaseWaiter := make(chan struct{}), make(chan struct{})
		var releaseOnce, waiterOnce sync.Once
		releaseAll := func() { releaseOnce.Do(func() { close(release) }); waiterOnce.Do(func() { close(releaseWaiter) }) }
		var ownerPid, waitingPid uint32
		var peerCalls atomic.Int32
		owner := startOwnedTransactionTest(func() {
			OwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
				Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted)
		})
		defer func() { releaseAll(); owner.join(t, ctx) }()
		ownedTransactionTestAwait(t, ctx, entered)
		requirePgOwnershipHeld(t, ctx, ownerPid, key)
		peerCtx := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
			if event.Kind == PgOwnershipWaiting {
				waitingPid = event.BackendPid
				close(waiting)
				select {
				case <-releaseWaiter:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}
		})
		peer := startOwnedTransactionTest(func() {
			OwnedTx(peerCtx, []PgOwnershipKey{key}, func(tx PgTx) {
				peerCalls.Add(1)
				RaisePgResult(tx.Exec(peerCtx, `INSERT INTO owned_tx_effect VALUES(2,23)`))
			}, TxReadCommitted)
		})
		defer func() { releaseAll(); peer.join(t, ctx) }()
		ownedTransactionTestAwait(t, ctx, waiting)
		Db(ctx, func(conn PgConn) {
			var idle bool
			Raise(conn.QueryRow(ctx, `SELECT backend_xid IS NULL AND xact_start IS NULL FROM pg_stat_activity WHERE pid=$1`, waitingPid).Scan(&idle))
			if !idle || peerCalls.Load() != 0 {
				t.Fatal("busy admission entered a business transaction")
			}
		}, OptNoRetry())
		OwnedTx(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())}, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(3,37)`))
		}, TxReadCommitted)
		requirePgOwnershipHeld(t, ctx, ownerPid, key)
		releaseOnce.Do(func() { close(release) })
		if err := owner.join(t, ctx); err != nil {
			t.Fatal("held owner failed", err)
		}
		waiterOnce.Do(func() { close(releaseWaiter) })
		if err := peer.join(t, ctx); err != nil || peerCalls.Load() != 1 {
			t.Fatal("queued peer did not execute once", err)
		}
		Db(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=3 AND sum(amount)=71 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("independent/queued business effects differ")
			}
		}, OptNoRetry())
	})
}

// A complete allocation larger than one SQL chunk cannot bypass its last key.
// A refused attempt releases earlier keys, and cancellation executes no body.
func TestOwnedTxAllKeysAcrossChunksReleaseOnCanceledAdmission(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+7)
		for index := range keys {
			keys[index] = NewPgOwnershipKey("owned-tx-test", NewId())
		}
		keys = normalizePgOwnershipKeys(keys)
		held := keys[len(keys)-1]
		conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer conn.Release()
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1,$2)`, held.first, held.second))
		defer releaseOwnedTransactionTestKeys(ctx, conn)
		requestCtx, cancelRequest := context.WithCancel(ctx)
		var attempts atomic.Int32
		waiting := make(chan struct{})
		var once sync.Once
		requestCtx = Testing_WithPgOwnershipObservation(requestCtx, func(event PgOwnershipEvent) {
			if event.Kind == PgOwnershipWaiting {
				once.Do(func() { close(waiting) })
				cancelRequest()
			}
		})
		peer := startOwnedTransactionTest(func() {
			OwnedTx(requestCtx, append(slices.Clone(keys), keys[0]), func(PgTx) { attempts.Add(1) })
		})
		defer func() { cancelRequest(); peer.join(t, ctx) }()
		ownedTransactionTestAwait(t, ctx, waiting)
		err, ok := peer.join(t, ctx).(error)
		if !ok || !errors.Is(err, context.Canceled) || attempts.Load() != 0 {
			t.Fatal("canceled multi-key admission entered business", err)
		}
		probe := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer probe.Release()
		acquired, err := tryPgOwnershipKeys(ctx, probe, keys[:len(keys)-1], false, probe.Conn().PgConn().PID())
		if err != nil || !acquired {
			t.Fatal("partial refusal retained an unrelated key", err)
		}
		RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock_all()`))
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`))
		OwnedTx(ctx, keys, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,71)`)) })
	})
}

// A canceled caller still receives detached commit/rollback cleanup. Confirmed
// observations and optional posts keep the ordinary engine's exact semantics.
func TestOwnedTxConfirmedCommitReleasesBeforeCanceledCallerPost(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("owned-tx-test", NewId())
		requestCtx, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		var counter TxCommitCounter
		var posts atomic.Int32
		OwnedTx(requestCtx, []PgOwnershipKey{key}, func(tx PgTx) {
			RaisePgResult(tx.Exec(requestCtx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
			if !AddTxCommitCount(tx, &counter, 1) {
				panic(errors.New("owned commit counter rejected"))
			}
			if !AddTxPostCommit(tx, "owned-reentrant-post", func() any {
				OwnedTx(ctx, []PgOwnershipKey{key}, func(next PgTx) { RaisePgResult(next.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(2,23)`)) })
				posts.Add(1)
				return nil
			}) {
				panic(errors.New("owned post rejected"))
			}
			cancelRequest()
		})
		requireTxCommitSnapshot(t, &counter, 1, 0, 0)
		if posts.Load() != 1 {
			t.Fatal("confirmed owned transaction lost its exact post")
		}
	})
}

// Neither a retryable statement error nor caller-supplied retry options may
// replay monetary SQL after ownership admission.
func TestOwnedTxForcesNoRetryAndPreservesStatementCause(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("owned-tx-test", NewId())
		var calls, posts atomic.Int32
		var counter TxCommitCounter
		err, ok := captureDbErrorPanic(func() {
			OwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
				calls.Add(1)
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
				AddTxCommitCount(tx, &counter, 1)
				AddTxPostCommit(tx, "must-not-publish", func() any { posts.Add(1); return nil })
				RaisePgResult(tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION 'synthetic owned refusal' USING ERRCODE='40001'; END $$`))
			}, OptRetryDefault())
		}).(error)
		var pgErr *pgconn.PgError
		if !ok || !errors.As(err, &pgErr) || pgErr.Code != "40001" || calls.Load() != 1 || posts.Load() != 0 {
			t.Fatal("owned failure reran or lost its exact cause", err)
		}
		requireTxCommitSnapshot(t, &counter, 0, 0, 0)
		OwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,37)`)) })
	})
}

// Killing the owning physical backend also kills its uncommitted business SQL;
// a new owner cannot overlap an old business transaction on a different route.
func TestOwnedTxBackendLossFencesOldBusinessAndPermitsExactRecovery(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("owned-tx-test", NewId())
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var pid uint32
		var calls atomic.Int32
		old := startOwnedTransactionTest(func() {
			OwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) {
				calls.Add(1)
				Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,11)`))
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
				RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(2,23)`))
			})
		})
		defer func() { once.Do(func() { close(release) }); old.join(t, ctx) }()
		ownedTransactionTestAwait(t, ctx, entered)
		requirePgOwnershipHeld(t, ctx, pid, key)
		Db(ctx, func(conn PgConn) {
			var terminated bool
			Raise(conn.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
			if !terminated {
				t.Fatal("fixture failed to terminate its actual ownership backend")
			}
		}, OptNoRetry())
		OwnedTx(ctx, []PgOwnershipKey{key}, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,37)`)) })
		once.Do(func() { close(release) })
		if err := old.join(t, ctx); err == nil || calls.Load() != 1 {
			t.Fatal("lost owner repeated or acknowledged old business", err)
		}
		Db(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND min(id)=1 AND sum(amount)=37 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("lost backend overlapped recovery or leaked its old effect")
			}
		}, OptNoRetry())
	})
}

// Session and transaction owners share one namespace. A caller-owned attempt
// may retain private rows, but refusal precedes every shared business write.
func TestTryTxOwnershipRefusalAndAcknowledgedEndUseActualTransaction(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		key := NewPgOwnershipKey("owned-tx-test", NewId())
		conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer conn.Release()
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1,$2)`, key.first, key.second))
		defer releaseOwnedTransactionTestKeys(ctx, conn)
		var events []PgOwnershipEventKind
		observed := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
			if !event.TransactionScoped {
				panic(errors.New("transaction observer used session scope"))
			}
			events = append(events, event.Kind)
		})
		Tx(observed, func(tx PgTx) {
			ok, err := TryTxOwnership(observed, tx, []PgOwnershipKey{key})
			Raise(err)
			if ok {
				panic(errors.New("transaction bypassed the session owner"))
			}
		}, TxReadCommitted, OptNoRetry())
		if !slices.Equal(events, []PgOwnershipEventKind{PgOwnershipRefused}) {
			t.Fatal("refused transaction invented an admitted interval", events)
		}
		RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`))
		events = nil
		Tx(observed, func(tx PgTx) {
			ok, err := TryTxOwnership(observed, tx, []PgOwnershipKey{key})
			Raise(err)
			if !ok {
				panic(errors.New("fresh transaction was not admitted"))
			}
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,37)`))
		}, TxReadCommitted, OptNoRetry())
		if !slices.Equal(events, []PgOwnershipEventKind{PgOwnershipAdmitted, PgOwnershipReleased}) {
			t.Fatal("acknowledged transaction ownership lifetime changed", events)
		}
		events = nil
		failure := errors.New("synthetic caller rollback")
		got := captureDbErrorPanic(func() {
			Tx(observed, func(tx PgTx) {
				ok, err := TryTxOwnership(observed, tx, []PgOwnershipKey{key})
				Raise(err)
				if !ok {
					panic(errors.New("rollback owner was not admitted"))
				}
				panic(failure)
			}, TxReadCommitted, OptNoRetry())
		})
		if got != failure || !slices.Equal(events, []PgOwnershipEventKind{PgOwnershipAdmitted, PgOwnershipReleased}) {
			t.Fatal("rollback lost actual ownership cleanup", got, events)
		}
	})
}

// Admission before a savepoint survives rollback to that savepoint. Nested
// helpers may validate the same subset, but cannot add an unowned resource.
func TestTxOwnershipSubsetKeepsOuterAuthorityAcrossSavepoint(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		keys := []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId()), NewPgOwnershipKey("owned-tx-test", NewId())}
		additional := NewPgOwnershipKey("owned-tx-test", NewId())
		var events []PgOwnershipEventKind
		observed := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) { events = append(events, event.Kind) })
		var retired PgTx
		Tx(observed, func(tx PgTx) {
			retired = tx
			admitted, err := TryTxOwnership(observed, tx, keys)
			Raise(err)
			if !admitted || !TxOwnsKeys(tx, keys) {
				panic(errors.New("complete outer admission missing"))
			}
			RaisePgResult(tx.Exec(ctx, `SAVEPOINT owned_scope`))
			admitted, err = TryTxOwnership(observed, tx, keys[:1])
			Raise(err)
			if !admitted || len(events) != 1 {
				panic(errors.New("subset manufactured a second ownership interval"))
			}
			RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT owned_scope`))
			var pid uint32
			Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			for _, key := range keys {
				requirePgOwnershipHeld(t, ctx, pid, key)
			}
			if admitted, err := TryTxOwnership(observed, tx, []PgOwnershipKey{additional}); admitted || err == nil {
				panic(errors.New("nested helper expanded its outer ownership set"))
			}
			savepoint := RaisePgResult(tx.Begin(ctx))
			if admitted, err := TryTxOwnership(observed, savepoint, keys); admitted || err == nil {
				panic(errors.New("raw savepoint borrowed outer transaction ownership"))
			}
			Raise(savepoint.Rollback(ctx))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,37)`))
		}, TxReadCommitted, OptNoRetry())
		if TxOwnsKeys(retired, keys) || !slices.Equal(events, []PgOwnershipEventKind{PgOwnershipAdmitted, PgOwnershipReleased}) {
			t.Fatal("transaction handback retained authority or duplicated observations", events)
		}
		OwnedTx(observed, keys, func(tx PgTx) {
			admitted, err := TryTxOwnership(observed, tx, keys[:1])
			Raise(err)
			if !admitted || !TxOwnsKeys(tx, keys) {
				panic(errors.New("session-owned transaction lost subset validation"))
			}
		})
	})
}
