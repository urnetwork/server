// Real pool and protocol boundaries preserve stopped-operation causes without
// replaying the original callback, statement or transaction.
package server

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// An already stopped real pgx acquire retains cancellation versus deadline;
// neither outcome is permission to enter a callback.
func TestDbStoppedAcquireRetainsContextCause(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		func() {
			_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(1, 0))
				defer cancel()
				want = context.DeadlineExceeded
			}
			calls := 0
			recovered := captureDbErrorPanic(func() {
				dbWithPool(ctx, pool, func(PgConn) { calls++ })
			})
			err, _ := recovered.(error)
			if !errors.Is(err, DbContextDoneError) || !errors.Is(err, want) || calls != 0 {
				t.Fatalf("stopped acquire lost its cause or ran work: deadline=%t err=%v calls=%d", deadline, err, calls)
			}
		}()
	}
}

// Force the transient, connection retry and canceled socket-timeout paths only
// after the actual pool acquired a connection. Every original error survives.
func TestDbStoppedCallbackRetainsPhysicalCause(t *testing.T) {
	for _, original := range []error{
		&pgconn.PgError{Code: "40001", Message: "synthetic serialization failure"},
		&pgconn.PgError{Code: "08006", Message: "synthetic connection failure"},
		&net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded},
	} {
		func() {
			_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			recovered := captureDbErrorPanic(func() {
				dbWithPool(ctx, pool, func(PgConn) {
					calls++
					cancel()
					panic(original)
				})
			})
			err, _ := recovered.(error)
			if !errors.Is(err, DbContextDoneError) || !errors.Is(err, context.Canceled) || !errors.Is(err, original) || calls != 1 {
				t.Fatal("stopped callback lost its physical cause or replayed work", original, err, calls)
			}
		}()
	}
}

// Both a rolled-back body and a rejected real commit reply reach their own
// cancellation boundary. The physical database verdict must remain visible.
func TestTxStoppedRetryRetainsDatabaseCause(t *testing.T) {
	for _, commit := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
				fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
					if commit && query == "commit" {
						cancel()
						return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic commit refusal"}
					}
					return nil
				}
			})
			calls := 0
			original := &pgconn.PgError{Code: "40001", Message: "synthetic body refusal"}
			recovered := captureDbErrorPanic(func() {
				txWithPool(ctx, pool, func(PgTx) {
					calls++
					if !commit {
						cancel()
						panic(original)
					}
				})
			})
			err, _ := recovered.(error)
			var database *pgconn.PgError
			if !errors.Is(err, DbContextDoneError) || !errors.Is(err, context.Canceled) || !errors.As(err, &database) || database.Code != "40001" || calls != 1 {
				t.Fatalf("stopped transaction lost verdict or replayed work: commit=%t err=%v calls=%d", commit, err, calls)
			}
			if (!commit && !errors.Is(err, original)) || (commit && database.Message != "synthetic commit refusal") {
				t.Fatal("stopped transaction changed its original database cause", commit, err)
			}
		}()
	}
}

// A stopped real Redis PING never admits the callback and retains the exact
// context cause instead of returning only the shared historical done marker.
func TestRedisStoppedPingRetainsContextCause(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		func() {
			peer := newDeadlineRedisPeer(t, "")
			client := redis.NewClient(deadlineTestRedisOptions(peer))
			defer client.Close()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(1, 0))
				defer cancel()
				want = context.DeadlineExceeded
			}
			calls := 0
			recovered := captureDbErrorPanic(func() {
				redisWithClient(ctx, &safeRedisClient{client: client}, func(RedisClient) { calls++ })
			})
			err, _ := recovered.(error)
			if !errors.Is(err, DbContextDoneError) || !errors.Is(err, want) || calls != 0 {
				t.Fatalf("stopped Redis admission lost cause or ran work: deadline=%t err=%v calls=%d", deadline, err, calls)
			}
		}()
	}
}

// The original physical failure is retained after a real successful Redis
// PING, and stopping at that boundary never replays the admitted callback.
func TestRedisStoppedCallbackRetainsPhysicalCause(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	client := redis.NewClient(deadlineTestRedisOptions(peer))
	defer client.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	calls := 0
	recovered := captureDbErrorPanic(func() {
		redisWithClient(ctx, &safeRedisClient{client: client}, func(RedisClient) {
			calls++
			cancel()
			panic(original)
		})
	})
	err, _ := recovered.(error)
	if !errors.Is(err, DbContextDoneError) || !errors.Is(err, context.Canceled) || !errors.Is(err, original) || calls != 1 || peer.count("ping") != 1 {
		t.Fatal("stopped Redis callback lost physical cause or replayed work", err, calls, peer.count("ping"))
	}
}
