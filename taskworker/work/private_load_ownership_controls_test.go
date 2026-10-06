// Forced Goexit and panic controls exercise the exact owners used by diagnostic fixtures.
package work

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// The fake keeps only acquisition state; transaction behavior follows the actual interface.
type privateBarrierTestConn struct {
	tx      *privateBarrierTestTx
	begin   func()
	release func()
}

// Begin can force an abnormal exit immediately after the lease has been acquired.
func (self *privateBarrierTestConn) Begin(context.Context) (server.PgTx, error) {
	if self.begin != nil {
		self.begin()
	}
	return self.tx, nil
}

// A captured release is the exact pool-reset boundary observed by the control.
func (self *privateBarrierTestConn) Release() { self.release() }

// Embedded methods outside the owned scope intentionally have no implementation.
type privateBarrierTestTx struct {
	server.PgTx
	exec     func()
	rollback func(context.Context) error
}

// The single synthetic row preserves the held-row helper's affected-row guard.
func (self *privateBarrierTestTx) Exec(context.Context, string, ...any) (server.PgTag, error) {
	if self.exec != nil {
		self.exec()
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

// Rollback exposes its fresh finite cleanup context to the control.
func (self *privateBarrierTestTx) Rollback(ctx context.Context) error { return self.rollback(ctx) }

// A fresh bound is only a rescue guard; causal assertions use explicit lifecycle edges.
func privateLoadAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned control did not join")
	}
}

// Run each stage in a scope whose deferred reset observes all resources already released.
func privateLoadCheckBarrierUnwind(t *testing.T, stage string, fail func()) {
	t.Helper()
	var stateLock sync.Mutex
	released, rolledBack := false, false
	finished := make(chan struct{})
	tx := &privateBarrierTestTx{rollback: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Error("rollback inherited canceled work context")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("rollback lacks finite fresh bound")
		}
		stateLock.Lock()
		rolledBack = true
		stateLock.Unlock()
		return nil
	}}
	conn := &privateBarrierTestConn{tx: tx, release: func() { stateLock.Lock(); released = true; stateLock.Unlock() }}
	if stage == "begin" {
		conn.begin = fail
	}
	if stage == "lock" {
		tx.exec = fail
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	go func() {
		defer close(finished)
		defer func() {
			_ = recover()
			stateLock.Lock()
			if !released {
				t.Error("pool reset reached before maintenance release")
			}
			if stage != "begin" && !rolledBack {
				t.Error("pool reset reached before transaction rollback")
			}
			stateLock.Unlock()
		}()
		_ = privateLoadWithBarrier(ctx, func(context.Context) (privateLoadBarrierConn, error) { return conn, nil }, "synthetic held row", server.Id{}, func() error { fail(); return nil })
	}()
	privateLoadAwait(t, finished)
}

// Goexit is the testing Fatal path and must still return every acquired pool slot.
func TestPrivateLoadBarrierGoexitUnwinds(t *testing.T) {
	for _, stage := range []string{"begin", "lock", "body"} {
		privateLoadCheckBarrierUnwind(t, stage, runtime.Goexit)
	}
}

// Panics have the same in-frame ownership requirement as testing Fatal.
func TestPrivateLoadBarrierPanicUnwinds(t *testing.T) {
	for _, stage := range []string{"begin", "lock", "body"} {
		privateLoadCheckBarrierUnwind(t, stage, func() { panic("synthetic stage failure") })
	}
}

// Returned encoder and transaction failures retain their original cause after bounded cleanup.
func TestPrivateLoadBarrierReturnedFailureUnwinds(t *testing.T) {
	sentinel := errors.New("synthetic encode failure")
	released, rolledBack := false, false
	tx := &privateBarrierTestTx{rollback: func(context.Context) error { rolledBack = true; return nil }}
	conn := &privateBarrierTestConn{tx: tx, release: func() { released = true }}
	err := privateLoadWithBarrier(t.Context(), func(context.Context) (privateLoadBarrierConn, error) { return conn, nil }, "synthetic held row", server.Id{}, func() error { return sentinel })
	if !errors.Is(err, sentinel) || !released || !rolledBack {
		t.Fatal("returned failure lost ownership or cause")
	}
}

// Constructor failure closes its notification owner before control returns.
func TestPrivateLoadFixtureConstructorFailureUnwinds(t *testing.T) {
	sentinel := errors.New("synthetic constructor failure")
	closed := 0
	_, closeOwner, err := privateLoadFinishControl(func() (*providerEgressControl, error) { return nil, sentinel }, func() { closed++ })
	if !errors.Is(err, sentinel) || closed != 1 {
		t.Fatal("failed fixture retained its notification owner")
	}
	closeOwner()
	if closed != 1 {
		t.Fatal("fixture notification cleanup repeated")
	}
}

// Goexit during construction has no returned owner for the caller to clean up.
func TestPrivateLoadFixtureConstructorGoexitUnwinds(t *testing.T) {
	closed, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		privateLoadFinishControl(func() (*providerEgressControl, error) { runtime.Goexit(); return nil, nil }, func() { close(closed) })
	}()
	privateLoadAwait(t, finished)
	select {
	case <-closed:
	default:
		t.Fatal("fixture Goexit leaked notifications")
	}
}

// A failed parent before readiness still releases and joins its gated wave.
func TestPrivateLoadWaveFailureBeforeReadyJoins(t *testing.T) {
	exited := make(chan struct{})
	owner := privateLoadStartWave(t.Context(), func(ctx context.Context, ready chan<- struct{}, release <-chan struct{}) privateLoadProcessReport {
		defer close(exited)
		ready <- struct{}{}
		<-release
		if ctx.Err() == nil {
			t.Error("failure cleanup did not cancel requests")
		}
		return privateLoadProcessReport{Failed: 1}
	})
	defer func() { owner.cancel(); owner.Start(); owner.Wait() }()
	owner.Close()
	select {
	case <-exited:
	default:
		t.Error("failure cleanup returned before joining the gated wave")
	}
	owner.Start()
	privateLoadAwait(t, exited)
	if owner.Wait().Failed != 1 {
		t.Fatal("joined wave result lost")
	}
}

// An early sibling error does not permit teardown before the remaining owner exits.
func TestPrivateLoadResultsJoinAfterFirstFailure(t *testing.T) {
	sentinel := errors.New("synthetic sibling error")
	results := make(chan error)
	joined := make(chan struct{})
	var got error
	go func() { defer close(joined); got = privateLoadJoinResults(2, results) }()
	results <- sentinel
	// An early return closes joined; the corrected owner must receive its second result.
	select {
	case <-joined:
		t.Error("sibling failure returned before joining the remaining owner")
	case results <- nil:
	}
	privateLoadAwait(t, joined)
	if !errors.Is(got, sentinel) {
		t.Fatal("joining siblings discarded original error")
	}
}

// Setup Goexit publishes completion but never readiness; callers must not wait on a dead producer.
func TestPrivateLoadWaveGoexitBeforeReady(t *testing.T) {
	owner := privateLoadStartWave(t.Context(), func(context.Context, chan<- struct{}, <-chan struct{}) privateLoadProcessReport {
		runtime.Goexit()
		return privateLoadProcessReport{}
	})
	privateLoadAwait(t, owner.done)
	waiting, returned := make(chan struct{}), make(chan struct{})
	results := make(chan error, 1)
	rescue := sync.OnceFunc(func() { close(owner.ready) })
	defer func() { rescue(); owner.Close(); privateLoadAwait(t, returned) }()
	go func() {
		defer close(returned)
		results <- owner.Ready(t.Context(), func() { close(waiting) })
	}()
	select {
	case err := <-results:
		if err == nil {
			t.Error("dead producer qualified as ready")
		}
	case <-waiting:
		t.Error("readiness waited on an already completed producer")
	}
	rescue()
	privateLoadAwait(t, returned)
}
