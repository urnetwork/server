// Orders overlapping public provide calls at their actual SQL boundary.
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
	"github.com/urnetwork/server"
)

// A context-local barrier leaves unrelated clients and pools independent.
type provideSerializationTraceKey struct{}

type provideSerializationTrace struct {
	firstStatement chan string
	deleteReached  chan struct{}
	resume         chan struct{}
	statementOnce  sync.Once
	deleteOnce     sync.Once
}

// Observe the real lock/delete query, optionally pausing before deletion.
type provideSerializationTracer struct{}

func (provideSerializationTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace, _ := ctx.Value(provideSerializationTraceKey{}).(*provideSerializationTrace)
	if trace == nil {
		return ctx
	}
	statement := ""
	if strings.Contains(data.SQL, "pg_advisory_xact_lock") && strings.Contains(data.SQL, "provide_key/") {
		statement = "lock"
	} else if strings.Contains(data.SQL, "DELETE FROM provide_key") {
		statement = "delete"
	}
	if statement != "" {
		trace.statementOnce.Do(func() { trace.firstStatement <- statement })
	}
	if statement == "delete" && trace.deleteReached != nil {
		trace.deleteOnce.Do(func() {
			close(trace.deleteReached)
			select {
			case <-trace.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (provideSerializationTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Start an owner call without hiding a raised database failure from the test.
func runProvideSerializationCall(ctx context.Context, workers *sync.WaitGroup, trace *provideSerializationTrace, clientId server.Id, keys map[ProvideMode][]byte) <-chan error {
	done := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		var callErr error
		server.HandleError(func() {
			if trace != nil {
				ctx = context.WithValue(ctx, provideSerializationTraceKey{}, trace)
			}
			SetProvide(ctx, clientId, keys)
		}, func(err error) { callErr = err })
		done <- callErr
	}()
	return done
}

// A second replacement must wait before its delete, while another client can
// complete. Without the lock the second call deterministically reaches delete.
func TestSetProvideSerializesReplacementBeforeDelete(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		clientId, siblingId := server.NewId(), server.NewId()
		for _, id := range []server.Id{clientId, siblingId} {
			Testing_CreateDevice(ctx, server.NewId(), server.NewId(), id, "synthetic", "synthetic")
		}
		scope, err := server.NewTestPgQueryScope(ctx, provideSerializationTracer{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		firstKeys := map[ProvideMode][]byte{ProvideModeNetwork: bytes.Repeat([]byte{0x31}, 32), ProvideModePublic: bytes.Repeat([]byte{0x32}, 32)}
		secondKeys := map[ProvideMode][]byte{ProvideModeNetwork: bytes.Repeat([]byte{0x41}, 32), ProvideModePublic: bytes.Repeat([]byte{0x42}, 32)}
		firstTrace := &provideSerializationTrace{firstStatement: make(chan string, 1), deleteReached: make(chan struct{}), resume: make(chan struct{})}
		var workers sync.WaitGroup
		defer func() { cancel(); workers.Wait() }()
		firstDone := runProvideSerializationCall(ctx, &workers, firstTrace, clientId, firstKeys)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(firstTrace.resume) }) }
		defer release()
		select {
		case <-firstTrace.deleteReached:
		case err := <-firstDone:
			t.Fatalf("first replacement returned before barrier: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		secondTrace := &provideSerializationTrace{firstStatement: make(chan string, 1)}
		secondDone := runProvideSerializationCall(ctx, &workers, secondTrace, clientId, secondKeys)
		select {
		case statement := <-secondTrace.firstStatement:
			if statement != "lock" {
				t.Error("overlapping replacement reached delete before acquiring its client lock")
				release()
			}
		case err := <-secondDone:
			t.Fatalf("second replacement returned before SQL: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if !t.Failed() {
			if err := waitForPaymentLockWaiter(ctx, "provide_key/"); err != nil {
				t.Fatal(err)
			}
			siblingDone := runProvideSerializationCall(ctx, &workers, nil, siblingId, firstKeys)
			select {
			case err := <-siblingDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("unrelated client blocked behind replacement")
			}
		}
		release()
		for _, done := range []<-chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if !t.Failed() && !maps.EqualFunc(loadProvideSecretKeys(ctx, clientId), secondKeys, bytes.Equal) {
			t.Fatal("last committed replacement did not retain its complete key set")
		}
	})
}
