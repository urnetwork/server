// Startup classification preserves the complete physical read failure while
// retaining independent migration, local-file and cancellation refusals.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Already stopped startup never reaches configuration or a database pool and
// keeps the original cause in both the pure check and the status-latching API.
func TestStartupReadinessPreservesStoppedContextCause(t *testing.T) {
	t.Cleanup(SetWarpStatusReady)
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		want := context.Canceled
		if deadline {
			ctx, cancel = context.WithDeadline(t.Context(), time.Unix(1, 0))
			defer cancel()
			want = context.DeadlineExceeded
		}
		for _, check := range []func(context.Context) error{CheckStartupReadiness, StartupReadiness} {
			if err := check(ctx); !errors.Is(err, want) {
				t.Fatalf("stopped startup lost its original cause: deadline=%t err=%v", deadline, err)
			}
		}
	}
}

// Physical read errors survive the database/Redis panic boundary. A retry
// requires every leaf to be unavailable, not merely one reachable timeout.
func TestStartupReadinessPreservesDependencyReadCauses(t *testing.T) {
	transport := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	for _, component := range []string{"pg", "redis"} {
		original := errors.Join(transport, context.DeadlineExceeded)
		result := startupReadinessReadError(component, original)
		if !errors.Is(result, original) || !errors.Is(result, transport) || !errors.Is(result, context.DeadlineExceeded) || !RetryableStartupReadinessError(result) {
			t.Fatal("startup dependency error lost original read causes", component, result)
		}
		for _, hard := range []error{context.Canceled, errors.New("synthetic migration contradiction"), &pgconn.PgError{Code: "42P01"}, &os.PathError{Op: "read", Path: "synthetic-private-config", Err: context.DeadlineExceeded}} {
			if RetryableStartupReadinessError(errors.Join(result, hard)) {
				t.Fatal("unavailable startup read concealed an independent hard cause", component, hard)
			}
		}
	}
	for _, err := range []error{io.EOF, &pgconn.PgError{Code: "57P03"}, &pgconn.PgError{Code: "53300"}, &pgconn.PgError{Code: "40001"},
		pgconn.ErrConnClosed, redis.ErrPoolTimeout, redis.ErrPoolExhausted,
		&net.DNSError{Err: "synthetic temporary resolver loss", Name: "cache.startup.example", IsTemporary: true},
		startupReadinessRedisError("LOADING synthetic restart"), startupReadinessRedisError("CLUSTERDOWN synthetic election")} {
		if !RetryableStartupReadinessError(fmt.Errorf("synthetic dependency: %w", err)) {
			t.Fatal("typed temporary dependency failure cannot retry", err)
		}
	}
	if !RetryableStartupReadinessError(errors.Join(server.DbContextDoneError, context.DeadlineExceeded)) ||
		RetryableStartupReadinessError(server.DbContextDoneError) ||
		RetryableStartupReadinessError(errors.Join(server.DbContextDoneError, context.Canceled)) {
		t.Fatal("legacy database stop marker replaced the actual context classification")
	}
	if RetryableStartupReadinessError(startupReadinessReadError("pg", "synthetic timeout text")) {
		t.Fatal("untyped startup panic acquired retry authority")
	}
}

// A Redis server response carries protocol provenance rather than free text.
type startupReadinessRedisError string

// Preserve the original protocol response while exercising its exact code.
func (self startupReadinessRedisError) Error() string { return string(self) }

// This marker is part of the go-redis public error interface.
func (self startupReadinessRedisError) RedisError() {}

// A malformed wrapper cannot create an unbounded startup classifier loop.
type startupReadinessCycleError struct{}

// A diagnostic does not establish dependency provenance.
func (self *startupReadinessCycleError) Error() string { return "synthetic error cycle" }

// Deliberately returns the same node to exercise the traversal bound.
func (self *startupReadinessCycleError) Unwrap() error { return self }

// A wrapper implementation can also fail while being inspected.
type startupReadinessPanicError struct{}

// A diagnostic does not establish dependency provenance.
func (self *startupReadinessPanicError) Error() string { return "synthetic invalid wrapper" }

// Deliberately refuses traversal instead of supplying a complete cause tree.
func (self *startupReadinessPanicError) Unwrap() error { panic("synthetic unwrap failure") }

// Unknown or malformed trees remain hard without crashing the startup owner.
func TestStartupReadinessRejectsIncompleteCauseTrees(t *testing.T) {
	for _, err := range []error{&startupReadinessCycleError{}, &startupReadinessPanicError{}, errors.New("connection reset by peer"),
		errors.New("LOADING synthetic untyped text"), startupReadinessRedisError("NOAUTH synthetic missing credential"),
		startupReadinessRedisError("WRONGTYPE synthetic schema refusal"), nil} {
		if RetryableStartupReadinessError(err) {
			t.Fatal("incomplete startup cause acquired retry authority", err)
		}
	}
}
