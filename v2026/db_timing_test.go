package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real pgx wire replies prove that observing retries neither hides failed
// attempts nor permits replay of an ambiguous commit or a callback panic.
func TestDbTimingRetainsAttemptsAndFailureSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		counts  [DbTimingPhaseCount]uint64
		queries []string
	}{
		{"success", [DbTimingPhaseCount]uint64{1, 1, 1, 0, 0}, []string{"begin", "SELECT 1", "commit"}},
		{"body_retry", [DbTimingPhaseCount]uint64{2, 2, 1, 1, 1}, []string{"begin", "SELECT 1", "rollback", "begin", "SELECT 1", "commit"}},
		{"commit_retry", [DbTimingPhaseCount]uint64{2, 2, 2, 0, 1}, []string{"begin", "SELECT 1", "commit", "begin", "SELECT 1", "commit"}},
		{"body_panic", [DbTimingPhaseCount]uint64{1, 1, 0, 1, 0}, []string{"begin", "SELECT 1", "rollback"}},
		{"lost_commit", [DbTimingPhaseCount]uint64{1, 1, 1, 0, 0}, []string{"begin", "SELECT 1", "commit"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			failed := false
			fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
				return test.name != "lost_commit" || query != "commit"
			}, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
				fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
					if !failed && ((test.name == "body_retry" && query == "SELECT 1") || (test.name == "commit_retry" && query == "commit")) {
						failed = true
						return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic rollback"}
					}
					return nil
				}
			})
			retry := OptRetryDefault()
			retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var observation DbTiming
			original := errors.New("synthetic callback panic")
			callbacks := 0
			recovered := captureDbErrorPanic(func() {
				txWithPool(ctx, pool, func(tx PgTx) {
					callbacks++
					RaisePgResult(tx.Exec(ctx, "SELECT 1"))
					if test.name == "body_panic" {
						panic(original)
					}
				}, retry, &observation)
			})
			if test.name == "body_panic" {
				if recovered != original {
					t.Fatalf("observation replaced panic: %v", recovered)
				}
			} else if test.name == "lost_commit" {
				if recovered == nil || callbacks != 1 {
					t.Fatalf("ambiguous commit was hidden/replayed: panic=%v callbacks=%d", recovered, callbacks)
				}
			} else if recovered != nil {
				t.Fatalf("unexpected failure: %v", recovered)
			}
			for phase, want := range test.counts {
				sample := observation.Phases[phase]
				if sample.Count != want || (want > 0 && sample.Duration <= 0) || (want == 0 && sample.Duration != 0) {
					t.Fatalf("phase %d: %+v, want count %d", phase, sample, want)
				}
			}
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			var queries []string
			for _, query := range fixture.queries {
				if query == "-- ping" {
					continue
				}
				if strings.HasPrefix(query, "begin") {
					query = "begin"
				}
				queries = append(queries, query)
			}
			if !reflect.DeepEqual(queries, test.queries) {
				t.Fatalf("wire order changed: %v, want %v", queries, test.queries)
			}
		})
	}
}

func TestDbTimingCanceledAcquireDoesNotInventTransaction(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var observation DbTiming
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(PgTx) { t.Fatal("canceled acquire ran callback") }, OptNoRetry(), &observation)
	})
	if recovered == nil || observation.Phases[DbTimingAcquire].Count != 1 || observation.Phases[DbTimingAcquire].Duration <= 0 {
		t.Fatalf("missing failed acquire: panic=%v observation=%+v", recovered, observation)
	}
	for _, sample := range observation.Phases[1:] {
		if sample != (DbTimingSample{}) {
			t.Fatal("failed acquire fabricated a transaction phase")
		}
	}
}

func TestDbTimingCanceledRetryWaitRetainsRollback(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var observation DbTiming
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(PgTx) {
			cancel()
			panic(&pgconn.PgError{Code: "40001", Message: "synthetic serialization failure"})
		}, &observation)
	})
	recoveredErr, _ := recovered.(error)
	if !errors.Is(recoveredErr, DbContextDoneError) || !errors.Is(recoveredErr, context.Canceled) || observation.Phases[DbTimingRollback].Count != 1 || observation.Phases[DbTimingRetryWait].Count != 1 || observation.Phases[DbTimingCommit].Count != 0 {
		t.Fatalf("cancellation changed panic cleanup: %v %+v", recovered, observation)
	}
}
