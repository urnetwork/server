// Forced ownership transitions qualify observer completion without timing races.
package work

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Empty or one malformed row isolate sampler ownership from a database fixture.
type privateObserverTestRows struct {
	server.PgResult
	err     error
	scanErr error
	hasRow  bool
	onClose func()
}

// A Scan-error control returns exactly one malformed row; other controls are empty.
func (self *privateObserverTestRows) Next() bool {
	if self.hasRow {
		self.hasRow = false
		return true
	}
	return false
}

// A malformed synthetic row exercises the real observer's Scan failure branch.
func (self *privateObserverTestRows) Scan(...any) error { return self.scanErr }

// The stream's terminal error is independent of whether it returned any rows.
func (self *privateObserverTestRows) Err() error { return self.err }

// The synthetic row stream owns no resources.
func (self *privateObserverTestRows) Close() {
	if self.onClose != nil {
		self.onClose()
	}
}

// Queries expose a barrier so stop follows a completed acquisition.
type privateObserverTestQuery struct {
	called      chan struct{}
	once        sync.Once
	onQuery     func(context.Context)
	err         error
	rowErr      error
	scanErr     error
	onRowsClose func()
}

// Preserve the real interface used by the observer loop.
func (self *privateObserverTestQuery) Query(ctx context.Context, _ string, _ ...any) (server.PgResult, error) {
	self.once.Do(func() { close(self.called) })
	if self.onQuery != nil {
		self.onQuery(ctx)
	}
	return &privateObserverTestRows{err: self.rowErr, scanErr: self.scanErr, hasRow: self.scanErr != nil, onClose: self.onRowsClose}, self.err
}

// Every control retains a separate release acknowledgement even when the reversed owner closes done early.
func privateLoadTestObserver(t *testing.T, ctx context.Context, query server.PgCanQuery, release func() error) *privateLoadObserver {
	released := make(chan struct{})
	observer := newPrivateLoadObserver(ctx, query, func() error { defer close(released); return release() })
	t.Cleanup(func() {
		observer.cancel()
		func() { defer func() { _ = recover() }(); observer.Close() }()
		privateLoadAwait(t, released)
		privateLoadAwait(t, observer.done)
	})
	return observer
}

// Completion cannot be visible while the actual release owner remains blocked.
func TestPrivateLoadObserverCompletionWaitsForRelease(t *testing.T) {
	query := &privateObserverTestQuery{called: make(chan struct{})}
	releaseEntered, releaseAllowed, releaseExited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	allowRelease := sync.OnceFunc(func() { close(releaseAllowed) })
	observer := privateLoadTestObserver(t, t.Context(), query, func() error {
		defer close(releaseExited)
		close(releaseEntered)
		<-releaseAllowed
		return nil
	})
	returned := make(chan struct{})
	defer func() {
		allowRelease()
		observer.cancel()
		privateLoadAwait(t, releaseExited)
		privateLoadAwait(t, returned)
	}()
	<-query.called
	go func() { defer close(returned); observer.Close() }()
	<-releaseEntered
	select {
	case <-observer.done:
		t.Error("observer reported completion before connection release")
	default:
	}
	allowRelease()
	<-returned
	<-releaseExited
	<-observer.done
}

// Deferred and explicit cleanup share one cancel and one completed release.
func TestPrivateLoadObserverCloseIsRepeatable(t *testing.T) {
	query := &privateObserverTestQuery{called: make(chan struct{})}
	var releases atomic.Int32
	observer := privateLoadTestObserver(t, t.Context(), query, func() error { releases.Add(1); return nil })
	<-query.called
	first := observer.Close()
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		_ = observer.Close()
	}()
	if panicValue != nil {
		t.Fatal("repeated observer cleanup panicked")
	}
	if releases.Load() != 1 {
		t.Fatalf("observer released %d times", releases.Load())
	}
	first["caller_mutation"] = 1
	if observer.Close()["caller_mutation"] != 0 {
		t.Fatal("caller mutation changed retained observer result")
	}
}

// Concurrent cleanup callers all wait for the same release owner.
func TestPrivateLoadObserverCloseIsConcurrent(t *testing.T) {
	query := &privateObserverTestQuery{called: make(chan struct{})}
	var releases atomic.Int32
	observer := privateLoadTestObserver(t, t.Context(), query, func() error { releases.Add(1); return nil })
	<-query.called
	results := make(chan any, 8)
	start := make(chan struct{})
	for range 8 {
		go func() {
			var panicValue any
			defer func() {
				if p := recover(); p != nil {
					panicValue = p
				}
				results <- panicValue
			}()
			<-start
			observer.Close()
		}()
	}
	close(start)
	for range 8 {
		if <-results != nil {
			t.Error("concurrent observer cleanup panicked")
		}
	}
	if releases.Load() != 1 {
		t.Fatalf("observer released %d times", releases.Load())
	}
}

// Query failures cannot become an empty healthy activity result.
func TestPrivateLoadObserverRetainsQueryFailure(t *testing.T) {
	query := &privateObserverTestQuery{called: make(chan struct{}), err: errors.New("synthetic query failure")}
	observer := privateLoadTestObserver(t, t.Context(), query, func() error { return nil })
	<-observer.done
	if observer.Close()["sampling_error"] != 1 {
		t.Fatal("observer lost query failure")
	}
}

// A stream can fail after Query succeeds, even when Next returns no rows.
func TestPrivateLoadObserverRetainsRowsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	query := &privateObserverTestQuery{called: make(chan struct{}), rowErr: errors.New("synthetic rows failure"), onRowsClose: cancel}
	observer := privateLoadTestObserver(t, ctx, query, func() error { return nil })
	<-observer.done
	result := observer.Close()
	if result["sampling_error"] != 1 {
		t.Fatal("observer lost row-stream failure")
	}
}

// Stop interrupts the actual in-flight Query before publishing its stop edge.
func TestPrivateLoadObserverCloseCancelsOwner(t *testing.T) {
	queryEntered := make(chan context.Context, 1)
	queryExited, releaseExited := make(chan struct{}), make(chan struct{})
	query := &privateObserverTestQuery{called: make(chan struct{}), onQuery: func(ctx context.Context) {
		queryEntered <- ctx
		<-ctx.Done()
		close(queryExited)
	}}
	observer := privateLoadTestObserver(t, t.Context(), query, func() error { close(releaseExited); return nil })
	returned := make(chan struct{})
	defer func() {
		observer.cancel()
		privateLoadAwait(t, queryExited)
		privateLoadAwait(t, releaseExited)
		privateLoadAwait(t, returned)
	}()
	queryCtx := <-queryEntered
	go func() { defer close(returned); observer.Close() }()
	<-observer.stop
	if queryCtx.Err() == nil {
		t.Error("observer stop did not cancel in-flight query")
	}
	observer.cancel()
	privateLoadAwait(t, returned)
}

// Cleanup failure remains visible after the connection owner finishes.
func TestPrivateLoadObserverRetainsReleaseFailure(t *testing.T) {
	query := &privateObserverTestQuery{called: make(chan struct{})}
	observer := privateLoadTestObserver(t, t.Context(), query, func() error { return errors.New("synthetic release failure") })
	<-query.called
	if observer.Close()["cleanup_error"] != 1 {
		t.Fatal("observer lost release failure")
	}
}

// A canceled context does not erase an independent query failure.
func TestPrivateLoadObserverRetainsMixedQueryFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	query := &privateObserverTestQuery{called: make(chan struct{}), err: errors.Join(context.Canceled, errors.New("synthetic independent failure")), onQuery: func(context.Context) { cancel() }}
	observer := privateLoadTestObserver(t, ctx, query, func() error { return nil })
	defer observer.Close()
	privateLoadAwait(t, observer.done)
	if observer.Close()["sampling_error"] != 1 {
		t.Fatal("cancellation masked independent query failure")
	}
}

// A canceled context does not erase an independent stream failure.
func TestPrivateLoadObserverRetainsMixedRowsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	query := &privateObserverTestQuery{called: make(chan struct{}), rowErr: errors.Join(context.Canceled, errors.New("synthetic independent failure")), onRowsClose: cancel}
	observer := privateLoadTestObserver(t, ctx, query, func() error { return nil })
	defer observer.Close()
	privateLoadAwait(t, observer.done)
	if observer.Close()["sampling_error"] != 1 {
		t.Fatal("cancellation masked independent row failure")
	}
}

// Pure wrapped cancellation is expected shutdown, not a sampling failure.
func TestPrivateLoadObserverAllowsWrappedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	query := &privateObserverTestQuery{called: make(chan struct{}), err: fmt.Errorf("synthetic wrapper: %w", context.Canceled), onQuery: func(context.Context) { cancel() }}
	observer := privateLoadTestObserver(t, ctx, query, func() error { return nil })
	defer observer.Close()
	privateLoadAwait(t, observer.done)
	if observer.Close()["sampling_error"] != 0 {
		t.Fatal("pure owner cancellation became sampling failure")
	}
}

// An interrupted row stream cannot acknowledge a complete requested sample.
func TestPrivateLoadObserverCanceledRowsDoNotAcknowledgeSample(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sampled := make(chan struct{})
	requests := make(chan chan struct{}, 1)
	requests <- sampled
	query := &privateObserverTestQuery{called: make(chan struct{}), rowErr: context.Canceled, onRowsClose: cancel}
	observer := newPrivateLoadObserver(ctx, query, func() error { return nil }, requests)
	defer observer.Close()
	privateLoadAwait(t, observer.done)
	values := observer.Close()
	if values["samples"] != 0 || values["sampling_error"] != 0 {
		t.Error("canceled stream was counted as a complete sample or independent failure")
	}
	select {
	case <-sampled:
		t.Error("canceled stream acknowledged requested sample")
	default:
	}
}

// pgx retains a fatal Scan error in Err; count that one failure once and never acknowledge the sample.
func TestPrivateLoadObserverScanFailureCountedOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sampled := make(chan struct{})
	requests := make(chan chan struct{}, 1)
	requests <- sampled
	scanErr := errors.New("synthetic row scan failure")
	query := &privateObserverTestQuery{called: make(chan struct{}), scanErr: scanErr, rowErr: scanErr, onRowsClose: cancel}
	observer := newPrivateLoadObserver(ctx, query, func() error { return nil }, requests)
	defer observer.Close()
	privateLoadAwait(t, observer.done)
	values := observer.Close()
	if values["samples"] != 0 {
		t.Error("failed Scan was counted as a complete sample")
	}
	if values["sampling_error"] != 1 {
		t.Errorf("one fatal Scan error was counted %g times", values["sampling_error"])
	}
	select {
	case <-sampled:
		t.Error("malformed row acknowledged requested sample")
	default:
	}
}
