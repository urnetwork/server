package server

// Real pgx commit/retry replies exercise projection ownership at the connection
// boundary; no database fixture or elapsed-time race is needed.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The server withholds the commit reply beyond the old registration lifetime.
// Request cancellation cannot discard a confirmed commit's bounded publication.
func TestTxPostCommitTimestampFollowsHeldCommitAndCallerCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if query == "commit" {
			close(entered)
			<-release
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	stamped := make(chan time.Time, 1)
	finished := make(chan struct{})
	var failure any
	go func() {
		defer close(finished)
		failure = captureDbErrorPanic(func() {
			txWithPool(requestCtx, pool, func(tx PgTx) {
				AddTxPostCommitAt(tx, "held-commit", func(committedAt time.Time) any {
					stamped <- committedAt
					return nil
				})
			}, OptNoRetry())
		})
	}()
	defer func() { releaseOnce.Do(func() { close(release) }); cancelRequest(); <-finished }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("commit did not reach reply barrier")
	}
	cancelRequest()
	select {
	case <-time.After(TxPostCommitTimeout + 10*time.Millisecond):
	case <-ctx.Done():
		t.Fatal("held commit lost its test owner")
	}
	releasedAt := time.Now()
	releaseOnce.Do(func() { close(release) })
	<-finished
	if failure != nil {
		t.Fatal("confirmed commit failed after request cancellation", failure)
	}
	select {
	case committedAt := <-stamped:
		if committedAt.Before(releasedAt) || time.Since(committedAt) >= TxPostCommitTimeout {
			t.Fatal("publication was stamped before the confirmed commit", committedAt, releasedAt)
		}
	default:
		t.Fatal("confirmed commit lost its post")
	}
}

// A single-connection pool can be re-entered by a post only after release.
func TestTxPostCommitRunsAfterConnectionReleaseAndCoalesces(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(_ *pgPoolWireFixture, config *pgxpool.Config) { config.MaxConns = 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var called atomic.Int32
	txWithPool(ctx, pool, func(tx PgTx) {
		if !AddTxPostCommit(tx, "synthetic", func() any { called.Add(100); return nil }) {
			t.Fatal("server transaction rejected post")
		}
		AddTxPostCommit(tx, "synthetic", func() any {
			dbWithPool(ctx, pool, func(conn PgConn) { RaisePgResult(conn.Exec(ctx, "SELECT 1")) }, OptNoRetry())
			called.Add(1)
			return nil
		})
		if called.Load() != 0 {
			t.Fatal("projection ran before commit")
		}
	}, OptNoRetry())
	if called.Load() != 1 {
		t.Fatalf("projection calls = %d", called.Load())
	}
}

// A rolled-back attempt's closure cannot survive into the successful attempt.
func TestTxPostCommitDiscardsRetriedAttempt(t *testing.T) {
	failed := false
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
				if !failed && query == "SELECT 1" {
					failed = true
					return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic rollback"}
				}
				return nil
			}
		})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	retry := OptRetryDefault()
	retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
	var called atomic.Int32
	attempts := 0
	txWithPool(ctx, pool, func(tx PgTx) {
		attempts++
		attempt := attempts
		AddTxPostCommit(tx, "synthetic", func() any { called.Add(int32(attempt)); return nil })
		RaisePgResult(tx.Exec(ctx, "SELECT 1"))
	}, retry)
	if attempts != 2 || called.Load() != 2 {
		t.Fatalf("attempts=%d posts=%d", attempts, called.Load())
	}
}

// Body failures and ambiguous commits both leave recovery to the durable source.
func TestTxPostCommitDiscardsRollbackAndAmbiguousCommit(t *testing.T) {
	for _, lostCommit := range []bool{false, true} {
		func() {
			_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return !lostCommit || query != "commit" },
				func(context.Context, pgxpool.ShouldPingParams) bool { return false })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var called atomic.Int32
			failure := errors.New("synthetic rollback")
			recovered := captureDbErrorPanic(func() {
				txWithPool(ctx, pool, func(tx PgTx) {
					AddTxPostCommit(tx, "synthetic", func() any { called.Add(1); return nil })
					if !lostCommit {
						panic(failure)
					}
				}, OptNoRetry())
			})
			if recovered == nil || called.Load() != 0 {
				t.Fatalf("lost commit=%t recovered=%v posts=%d", lostCommit, recovered, called.Load())
			}
		}()
	}
}
