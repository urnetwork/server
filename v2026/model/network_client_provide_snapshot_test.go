// Serializing a provide must also refresh its snapshot after a lock wait.
package model

import (
	"bytes"
	"context"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

type provideSnapshotTraceKey struct{}
type provideSnapshotLockQueryKey struct{}

// Each public call owns its barriers and retains its first SQL failure, even
// when the transaction wrapper retries and ultimately returns successfully.
type provideSnapshotTrace struct {
	lockAcquired chan struct{}
	resume       chan struct{}
	firstError   chan error
	lockOnce     sync.Once
}

type provideSnapshotTracer struct{}

// Mark the lock query so its completion can hold the first writer in its
// actual transaction, after snapshot acquisition and before any key changes.
func (provideSnapshotTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_advisory_xact_lock") && strings.Contains(data.SQL, "provide_key/") {
		return context.WithValue(ctx, provideSnapshotLockQueryKey{}, true)
	}
	return ctx
}

// The first writer owns the lock until the test releases it. Later writers
// have no barrier and must see that writer's commit without a failed attempt.
func (provideSnapshotTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	trace, _ := ctx.Value(provideSnapshotTraceKey{}).(*provideSnapshotTrace)
	if trace == nil {
		return
	}
	trace.recordError(data.Err)
	isLock, _ := ctx.Value(provideSnapshotLockQueryKey{}).(bool)
	if data.Err == nil && isLock && trace.lockAcquired != nil {
		trace.lockOnce.Do(func() {
			close(trace.lockAcquired)
			select {
			case <-trace.resume:
			case <-ctx.Done():
			}
		})
	}
}

// Inserts use pgx batches, so query tracing alone would miss the collision.
func (provideSnapshotTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (provideSnapshotTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if trace, _ := ctx.Value(provideSnapshotTraceKey{}).(*provideSnapshotTrace); trace != nil {
		trace.recordError(data.Err)
	}
}

func (provideSnapshotTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	if trace, _ := ctx.Value(provideSnapshotTraceKey{}).(*provideSnapshotTrace); trace != nil {
		trace.recordError(data.Err)
	}
}

// Never block a query callback or grow an unbounded retry-error history.
func (self *provideSnapshotTrace) recordError(err error) {
	if err != nil {
		select {
		case self.firstError <- err:
		default:
		}
	}
}

// Force a second public writer's snapshot to precede the first writer's commit.
// Advisory locking with repeatable read still produces a uniqueness/serialization
// error here; retrying that failure is not proof that the lock prevented it.
func TestSetProvideLockWaitUsesCommittedPredecessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		clientId, siblingId := server.NewId(), server.NewId()
		for _, id := range []server.Id{clientId, siblingId} {
			Testing_CreateDevice(ctx, server.NewId(), server.NewId(), id, "synthetic", "synthetic")
		}
		scope, err := server.NewTestPgQueryScope(ctx, provideSnapshotTracer{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		first := &provideSnapshotTrace{lockAcquired: make(chan struct{}), resume: make(chan struct{}), firstError: make(chan error, 1)}
		second := &provideSnapshotTrace{firstError: make(chan error, 1)}
		firstKeys := map[ProvideMode][]byte{ProvideModeNetwork: bytes.Repeat([]byte{0x31}, 32), ProvideModePublic: bytes.Repeat([]byte{0x32}, 32)}
		secondKeys := map[ProvideMode][]byte{ProvideModeNetwork: bytes.Repeat([]byte{0x41}, 32), ProvideModePublic: bytes.Repeat([]byte{0x42}, 32)}
		var workers sync.WaitGroup
		defer func() { cancel(); workers.Wait() }()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(first.resume) }) }
		defer release()
		start := func(trace *provideSnapshotTrace, id server.Id, keys map[ProvideMode][]byte) <-chan error {
			done := make(chan error, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				var callErr error
				server.HandleError(func() {
					SetProvide(context.WithValue(ctx, provideSnapshotTraceKey{}, trace), id, keys)
				}, func(err error) { callErr = err })
				done <- callErr
			}()
			return done
		}
		await := func(done <-chan error) {
			t.Helper()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		firstDone := start(first, clientId, firstKeys)
		select {
		case <-first.lockAcquired:
		case err := <-firstDone:
			t.Fatalf("first writer returned without its lock barrier: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		secondDone := start(second, clientId, secondKeys)
		if err := waitForPaymentLockWaiter(ctx, "provide_key/"); err != nil {
			t.Fatal(err)
		}
		await(start(&provideSnapshotTrace{firstError: make(chan error, 1)}, siblingId, firstKeys))
		release()
		await(firstDone)
		await(secondDone)
		for _, trace := range []*provideSnapshotTrace{first, second} {
			select {
			case err := <-trace.firstError:
				t.Errorf("serialized writer needed a failed SQL attempt: %v", err)
			default:
			}
		}
		if !maps.EqualFunc(loadProvideSecretKeys(ctx, clientId), secondKeys, bytes.Equal) {
			t.Fatal("last committed writer did not retain its complete key set")
		}
	})
}
