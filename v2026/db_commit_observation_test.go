package server

// Real pgx wire barriers cover acknowledgement ownership and ambiguous replies.
// The final control also compares counters with committed PostgreSQL rows.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

func requireTxCommitSnapshot(t testing.TB, counter *TxCommitCounter, confirmed, uncertain, untracked uint64) {
	t.Helper()
	want := TxCommitCounterSnapshot{Confirmed: confirmed, Uncertain: uncertain, Untracked: untracked, Stable: true}
	if got := counter.Snapshot(); got != want {
		t.Fatalf("commit observation got=%+v want=%+v", got, want)
	}
}

// Hold the publication guard explicitly: the bounded snapshot must refuse,
// while the direct counter retains acknowledged work for a concurrent scrape.
func TestTxCommitCounterDirectReadSurvivesBusySnapshot(t *testing.T) {
	var absent *TxCommitCounter
	var counter TxCommitCounter
	if absent.ConfirmedCount() != 0 || counter.ConfirmedCount() != 0 {
		t.Fatal("empty confirmed count changed")
	}
	counter.add(&counter.confirmed, 7)
	counter.writers.Add(1)
	defer counter.writers.Add(-1)
	if snapshot := counter.Snapshot(); snapshot.Stable || snapshot.Confirmed != 0 {
		t.Fatal("fixture did not hold the multi-field publication guard", snapshot)
	}
	if got := counter.ConfirmedCount(); got != 7 {
		t.Fatal("busy snapshot erased acknowledged count", got)
	}
	counter.add(&counter.uncertain, 3)
	counter.add(&counter.untracked, 5)
	if got := counter.ConfirmedCount(); got != 7 {
		t.Fatal("coverage signals changed acknowledged count", got)
	}
	counter.add(&counter.confirmed, 11)
	if got := counter.ConfirmedCount(); got != 18 {
		t.Fatal("direct count missed subsequent acknowledged work", got)
	}
}

// The commit reply is held first, then an optional post is held independently.
// Cancellation and the held post cannot delay or erase a confirmed event count.
func TestTxCommitCounterConfirmedBeforeHeldPostAfterCancellation(t *testing.T) {
	commitEntered, releaseCommit := make(chan struct{}), make(chan struct{})
	postEntered, releasePost := make(chan struct{}), make(chan struct{})
	var releaseCommitOnce, releasePostOnce sync.Once
	_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if query == "commit" {
			close(commitEntered)
			<-releaseCommit
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	var counter TxCommitCounter
	finished := make(chan struct{})
	var failure any
	go func() {
		defer close(finished)
		failure = captureDbErrorPanic(func() {
			txWithPool(requestCtx, pool, func(tx PgTx) {
				RaisePgResult(tx.Exec(requestCtx, "INSERT INTO synthetic_effect VALUES (1)"))
				if !AddTxCommitCount(tx, &counter, 2) || !AddTxCommitCount(tx, &counter, 3) {
					panic(errors.New("owned event registration refused"))
				}
				if !AddTxPostCommit(tx, "synthetic-held-post", func() any {
					close(postEntered)
					<-releasePost
					return nil
				}) {
					panic(errors.New("owned post registration refused"))
				}
			}, OptNoRetry())
		})
	}()
	defer func() {
		releaseCommitOnce.Do(func() { close(releaseCommit) })
		releasePostOnce.Do(func() { close(releasePost) })
		cancelRequest()
		<-finished
	}()
	select {
	case <-commitEntered:
	case <-ctx.Done():
		t.Fatal("commit did not enter reply barrier")
	}
	requireTxCommitSnapshot(t, &counter, 0, 0, 0)
	cancelRequest()
	releaseCommitOnce.Do(func() { close(releaseCommit) })
	select {
	case <-postEntered:
	case <-ctx.Done():
		t.Fatal("confirmed transaction did not reach held post")
	}
	requireTxCommitSnapshot(t, &counter, 5, 0, 0)
	releasePostOnce.Do(func() { close(releasePost) })
	<-finished
	if failure != nil {
		t.Fatal("observation changed canceled-request commit result", failure)
	}
	requireTxCommitSnapshot(t, &counter, 5, 0, 0)
}

func TestTxCommitCounterBodyRollbackDiscardsEvents(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	var counter TxCommitCounter
	failure := errors.New("synthetic body rollback")
	recovered := captureDbErrorPanic(func() {
		txWithPool(t.Context(), pool, func(tx PgTx) {
			RaisePgResult(tx.Exec(t.Context(), "INSERT INTO synthetic_effect VALUES (1)"))
			AddTxCommitCount(tx, &counter, 7)
			panic(failure)
		}, OptNoRetry())
	})
	if recovered != failure {
		t.Fatal("counter changed owning rollback error", recovered)
	}
	requireTxCommitSnapshot(t, &counter, 0, 0, 0)
}

func TestTxCommitCounterBodyRetryPublishesOnlyFinalAttempt(t *testing.T) {
	failed := false
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
				if !failed && query == "SELECT 1" {
					failed = true
					return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic body rollback"}
				}
				return nil
			}
		})
	var counter TxCommitCounter
	attempts := 0
	retry := OptRetryDefault()
	retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
	txWithPool(t.Context(), pool, func(tx PgTx) {
		attempts++
		RaisePgResult(tx.Exec(t.Context(), "INSERT INTO synthetic_effect VALUES (1)"))
		AddTxCommitCount(tx, &counter, uint64(attempts))
		RaisePgResult(tx.Exec(t.Context(), "SELECT 1"))
	}, retry)
	if attempts != 2 {
		t.Fatal("body retry count changed", attempts)
	}
	requireTxCommitSnapshot(t, &counter, 2, 0, 0)
}

func TestTxCommitCounterKnownCommitRollbackRetainsRetry(t *testing.T) {
	commits := 0
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
				if query == "commit" {
					commits++
					if commits == 1 {
						return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic commit rollback"}
					}
				}
				return nil
			}
		})
	var counter TxCommitCounter
	attempts := 0
	retry := OptRetryDefault()
	retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
	txWithPool(t.Context(), pool, func(tx PgTx) {
		attempts++
		AddTxCommitCount(tx, &counter, uint64(attempts))
	}, retry)
	if attempts != 2 || commits != 2 {
		t.Fatal("confirmed rollback changed retry policy", attempts, commits)
	}
	requireTxCommitSnapshot(t, &counter, 2, 0, 0)
}

func TestTxCommitCounterExplicitRollbackReplyIsNotUncertain(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.commitRollbacks.Store(1) })
	var counter TxCommitCounter
	recovered := captureDbErrorPanic(func() {
		txWithPool(t.Context(), pool, func(tx PgTx) { AddTxCommitCount(tx, &counter, 5) }, OptNoRetry())
	})
	err, ok := recovered.(error)
	if !ok || !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatal("explicit rollback error changed", recovered)
	}
	requireTxCommitSnapshot(t, &counter, 0, 0, 0)
}

func TestTxCommitCounterLostReplyIsUncertainWithoutReplay(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return query != "commit" },
		func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	var counter TxCommitCounter
	attempts := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(t.Context(), pool, func(tx PgTx) {
			attempts++
			AddTxCommitCount(tx, &counter, 9)
		})
	})
	fixture.stateLock.Lock()
	dials := fixture.dialCount
	fixture.stateLock.Unlock()
	if recovered == nil || attempts != 1 || dials != 1 {
		t.Fatal("unknown reply changed error or replayed callback", recovered, attempts, dials)
	}
	requireTxCommitSnapshot(t, &counter, 0, 9, 0)
}

func TestTxCommitCounterUnknownCompletionSqlstateIsNotRollback(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
				if query == "commit" {
					return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40003", Message: "synthetic completion unknown"}
				}
				return nil
			}
		})
	var counter TxCommitCounter
	attempts := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(t.Context(), pool, func(tx PgTx) {
			attempts++
			AddTxCommitCount(tx, &counter, 4)
		})
	})
	err, ok := recovered.(error)
	var pgErr *pgconn.PgError
	if !ok || !errors.As(err, &pgErr) || pgErr.Code != "40003" || attempts != 1 {
		t.Fatal("unknown SQL completion changed error or replayed", recovered, attempts)
	}
	requireTxCommitSnapshot(t, &counter, 0, 4, 0)
}

type txCommitObservationPanicTx struct {
	PgTx
	failure error
}

func (self txCommitObservationPanicTx) Commit(context.Context) error { panic(self.failure) }

func TestTxCommitCounterCommitPanicPreservesIdentityAndUncertainty(t *testing.T) {
	failure := errors.New("synthetic commit panic")
	owner := &postCommitPgTx{PgTx: txCommitObservationPanicTx{failure: failure}}
	var counter TxCommitCounter
	AddTxCommitCount(owner, &counter, 3)
	recovered := captureDbErrorPanic(func() { _ = commitObservedTx(t.Context(), owner) })
	if recovered != failure {
		t.Fatal("commit observation replaced panic", recovered)
	}
	requireTxCommitSnapshot(t, &counter, 0, 3, 0)
	owner.commitObservations.finish(true, false)
	requireTxCommitSnapshot(t, &counter, 0, 3, 0)
}

func TestTxCommitCounterCapacityCoalescesAndExposesUntracked(t *testing.T) {
	owner := &postCommitPgTx{}
	counters := make([]TxCommitCounter, txCommitCounterLimit+1)
	for i := range txCommitCounterLimit {
		if !AddTxCommitCount(owner, &counters[i], 1) {
			t.Fatal("bounded counter registration refused", i)
		}
	}
	if !AddTxCommitCount(owner, &counters[0], 2) || AddTxCommitCount(owner, &counters[txCommitCounterLimit], 5) {
		t.Fatal("full observer did not coalesce existing counter or enforce capacity")
	}
	owner.commitObservations.finish(true, false)
	for i := range txCommitCounterLimit {
		want := uint64(1)
		if i == 0 {
			want = 3
		}
		requireTxCommitSnapshot(t, &counters[i], want, 0, 0)
	}
	requireTxCommitSnapshot(t, &counters[txCommitCounterLimit], 0, 0, 5)
	if AddTxCommitCount(owner, &counters[0], 7) {
		t.Fatal("completed attempt accepted a late registration")
	}
	requireTxCommitSnapshot(t, &counters[0], 3, 0, 7)
}

func TestTxCommitCounterOverflowAndBusySnapshotInvalidateEvidence(t *testing.T) {
	var counter TxCommitCounter
	counter.confirmed.Store(^uint64(0) - 1)
	owner := &postCommitPgTx{}
	AddTxCommitCount(owner, &counter, 2)
	owner.commitObservations.finish(true, false)
	if snapshot := counter.Snapshot(); !snapshot.Stable || !snapshot.Overflow || snapshot.Confirmed != 0 {
		t.Fatal("counter overflow did not invalidate wrapped value", snapshot)
	}
	next := &postCommitPgTx{}
	AddTxCommitCount(next, &counter, 1)
	next.commitObservations.finish(true, false)
	if snapshot := counter.Snapshot(); !snapshot.Stable || !snapshot.Overflow || snapshot.Confirmed != 1 {
		t.Fatal("a later update cleared sticky overflow", snapshot)
	}
	var pending TxCommitCounter
	attempt := &postCommitPgTx{}
	if !AddTxCommitCount(attempt, &pending, ^uint64(0)) || AddTxCommitCount(attempt, &pending, 1) {
		t.Fatal("attempt sum overflow was silently accepted")
	}
	attempt.commitObservations.finish(true, false)
	requireTxCommitSnapshot(t, &pending, ^uint64(0), 0, 1)
	pending.writers.Add(1)
	if pending.Snapshot().Stable {
		t.Fatal("in-progress observation produced a qualified snapshot")
	}
	pending.writers.Add(-1)
	requireTxCommitSnapshot(t, &pending, ^uint64(0), 0, 1)
}

func TestTxCommitCounterConcurrentAttemptsRemainAdditive(t *testing.T) {
	var counter TxCommitCounter
	var workers sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for range 64 {
				owner := &postCommitPgTx{}
				AddTxCommitCount(owner, &counter, 1)
				owner.commitObservations.finish(true, false)
			}
		}()
	}
	close(start)
	workers.Wait()
	requireTxCommitSnapshot(t, &counter, 1024, 0, 0)
}

// Raw/savepoint owners are visible coverage gaps; rolling them back cannot turn
// their attempted registrations into confirmed counts. No schema is migrated.
func TestTxCommitCounterMatchesActualPostgresCommitAndRollback(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE synthetic_commit_counter_rows(id integer PRIMARY KEY)`))
		}, OptReadWrite(), OptNoRetry())
		var counter TxCommitCounter
		requestCtx, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		Tx(requestCtx, func(tx PgTx) {
			tag := RaisePgResult(tx.Exec(requestCtx, `INSERT INTO synthetic_commit_counter_rows VALUES(1),(2)`))
			if !AddTxCommitCount(tx, &counter, uint64(tag.RowsAffected())) {
				panic(errors.New("real owned transaction refused event"))
			}
			cancelRequest()
		}, OptNoRetry())
		cancelRequest()
		requireTxCommitSnapshot(t, &counter, 2, 0, 0)
		failure := errors.New("synthetic row rollback")
		recovered := captureDbErrorPanic(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_commit_counter_rows VALUES(3)`))
				AddTxCommitCount(tx, &counter, 1)
				panic(failure)
			}, OptNoRetry())
		})
		if recovered != failure {
			t.Fatal("native rollback failure changed", recovered)
		}
		attempts := 0
		retry := OptRetryDefault()
		retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
		Tx(ctx, func(tx PgTx) {
			attempts++
			RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_commit_counter_rows VALUES(4)`))
			AddTxCommitCount(tx, &counter, 1)
			if attempts == 1 {
				RaisePgResult(tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION USING ERRCODE='40001',MESSAGE='synthetic rollback'; END $$`))
			}
		}, retry)
		if attempts != 2 {
			t.Fatal("native transaction retry changed", attempts)
		}
		Db(ctx, func(conn PgConn) {
			raw := RaisePgResult(conn.Begin(ctx))
			defer rollbackTx(ctx, raw)
			RaisePgResult(raw.Exec(ctx, `INSERT INTO synthetic_commit_counter_rows VALUES(5)`))
			if AddTxCommitCount(raw, &counter, 1) {
				panic(errors.New("raw owner was falsely counted as server-owned"))
			}
		}, OptReadWrite(), OptNoRetry())
		Tx(ctx, func(tx PgTx) {
			savepoint := RaisePgResult(tx.Begin(ctx))
			RaisePgResult(savepoint.Exec(ctx, `INSERT INTO synthetic_commit_counter_rows VALUES(6)`))
			if AddTxCommitCount(savepoint, &counter, 1) {
				panic(errors.New("savepoint borrowed outer commit authority"))
			}
			Raise(savepoint.Rollback(ctx))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_commit_counter_rows VALUES(7)`))
			AddTxCommitCount(tx, &counter, 1)
		}, OptNoRetry())
		var rowCount int64
		var ids []int32
		Db(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT count(*),array_agg(id ORDER BY id) FROM synthetic_commit_counter_rows`).Scan(&rowCount, &ids))
		}, OptReadOnly(), OptNoRetry())
		if rowCount != 4 || len(ids) != 4 || ids[0] != 1 || ids[1] != 2 || ids[2] != 4 || ids[3] != 7 {
			t.Fatal("native committed row set differs from owned events", rowCount, ids)
		}
		requireTxCommitSnapshot(t, &counter, uint64(rowCount), 0, 2)
	})
}
