package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgLifecycleHeldCloseConn struct {
	net.Conn
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (self *pgLifecycleHeldCloseConn) Close() error {
	self.once.Do(func() { close(self.entered) })
	<-self.release
	return self.Conn.Close()
}

func TestPgPoolWrapperLifecycleObservesSynchronousRelease(t *testing.T) {
	entered, allowClose := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowClose) }) }
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &pgLifecycleHeldCloseConn{Conn: conn, entered: entered, release: allowClose}, nil
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	joined := make(chan struct{})
	original := &pgconn.PgError{Code: "08006", Message: "synthetic classified connection failure"}
	var outcome any
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-joined:
		case <-time.After(2 * time.Second):
			t.Error("synchronous release worker did not join")
		}
	})
	go func() {
		defer close(joined)
		outcome = captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, "SELECT 1"))
				panic(original)
			}, OptNoRetry())
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("discard did not enter its physical close")
	}
	assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 1, 0)
	if pool.open().Stat().AcquiredConns() != 1 {
		t.Fatal("synchronous close escaped original pool capacity")
	}
	release()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("synchronous close did not join")
	}
	if outcome != original {
		t.Fatal("release telemetry changed the classified connection failure")
	}
	assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 0, 0)
}

func TestPgPoolWrapperLifecycleBoundsIncompleteCleanup(t *testing.T) {
	tracker := &pgPoolWrapperLifecycle{owned: 2, limit: 1, pending: make(map[<-chan struct{}]struct{}), trackingDropped: &atomic.Uint64{}}
	first, second := make(chan struct{}), make(chan struct{})
	tracker.beginRelease()
	if got := tracker.snapshot(); got.owned != 1 || got.releasing != 1 || got.cleanupPending != 0 {
		t.Fatal("release entry did not retain its own lifecycle state")
	}
	tracker.finishRelease(first, true)
	tracker.beginRelease()
	tracker.finishRelease(second, true)
	if got := tracker.snapshot(); got.owned != 0 || got.releasing != 0 || got.cleanupPending != 1 || got.trackingDropped != 1 || len(tracker.pending) != 1 {
		t.Fatal("cleanup registry exceeded its bound or hid incomplete tracking")
	}
	close(first)
	if got := tracker.snapshot(); got.cleanupPending != 0 || got.trackingDropped != 1 {
		t.Fatal("completed channel erased the unobserved cleanup qualification")
	}
	close(second)
}

func TestPgPoolWrapperLifecycleScopesActualPoolGeneration(t *testing.T) {
	_, first := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	_, second := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	old := first.observeBorrow(first.open())
	pending := make(chan struct{})
	old.beginRelease()
	old.finishRelease(pending, true)
	// A dropped observation is process-role cumulative, while ownership and
	// pending channels belong only to the actual pool generation.
	first.lifecycleDropped.Add(1)
	current := first.observeBorrow(second.open())
	current.beginRelease()
	current.finishRelease(nil, false)
	want := pgPoolWrapperSnapshot{trackingDropped: 1}
	if first.wrapperSnapshot(first.open()) != want || first.wrapperSnapshot(second.open()) != want {
		t.Fatal("an older generation contaminated the current pool observation")
	}
	if old.snapshot().cleanupPending != 1 {
		t.Fatal("generation transition fabricated completion of an old cleanup")
	}
	close(pending)
	if current.snapshot() != want {
		t.Fatal("old completion changed the replacement generation")
	}
}

// Payment reconciliation already owns a Hijack+Close escape when advisory
// unlock fails. Telemetry cannot dereference the released pooled Conn or replace
// the callback's original outcome in that existing path.
func TestPgPoolWrapperLifecyclePreservesCallbackHijack(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	original := errors.New("synthetic original reconciliation failure")
	got := captureDbErrorPanic(func() {
		dbWithPool(ctx, pool, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, "SELECT 1"))
			if err := conn.Hijack().Close(ctx); err != nil {
				t.Fatal("synthetic hijacked connection did not close")
			}
			panic(original)
		}, OptNoRetry())
	})
	if got != original {
		t.Fatal("telemetry replaced the callback's post-hijack error")
	}
	assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 0, 0)
}

// Reset holds safePgPool.mutex while pgx waits for borrowers. Lifecycle updates
// must use their own lock so the borrower can release and let reset complete.
func TestPgPoolWrapperLifecycleResetJoinsBorrower(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered, allowReturn := make(chan struct{}), make(chan struct{})
	joined, resetJoined := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowReturn) }) }
	var outcome any
	resetStarted := false
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-joined:
		case <-time.After(2 * time.Second):
			t.Error("borrower did not join during cleanup")
		}
		if resetStarted {
			select {
			case <-resetJoined:
			case <-time.After(2 * time.Second):
				t.Error("reset did not join during cleanup")
			}
		}
	})
	go func() {
		defer close(joined)
		outcome = captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, "SELECT 1"))
				close(entered)
				<-allowReturn
			}, OptNoRetry())
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("borrower did not enter its owned callback")
	}
	assertPgPoolWrapperLifecycleMetrics(t, pool, 1, 0, 0)
	resetStarted = true
	go func() { defer close(resetJoined); pool.reset() }()
	for pool.mutex.TryLock() {
		pool.mutex.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("reset did not take the pool generation lock")
		case <-time.After(time.Millisecond):
		}
	}
	release()
	for _, done := range []<-chan struct{}{joined, resetJoined} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("pool reset and wrapper release deadlocked")
		}
	}
	if outcome != nil {
		t.Fatal("reset changed the completed callback outcome")
	}
	if _, ready := pool.metricSnapshot(); ready {
		t.Fatal("reset pool emitted a live-generation metric")
	}
}

// Both successful commit and failed-body rollback remain wrapper-owned until
// their existing transaction cleanup completes. No Tx-level double counting.
func TestPgPoolWrapperLifecycleOwnsTransactionFinish(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name, command := "commit", "commit"
		if rollback {
			name, command = "rollback", "rollback"
		}
		t.Run(name, func(t *testing.T) {
			entered, allowFinish := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(allowFinish) }) }
			_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
				if query == command {
					close(entered)
					<-allowFinish
				}
				return true
			}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			joined := make(chan struct{})
			original := errors.New("synthetic transaction body refusal")
			var outcome any
			t.Cleanup(func() {
				cancel()
				release()
				select {
				case <-joined:
				case <-time.After(2 * time.Second):
					t.Error("transaction finish worker did not join")
				}
			})
			go func() {
				defer close(joined)
				outcome = captureDbErrorPanic(func() {
					txWithPool(ctx, pool, func(tx PgTx) {
						RaisePgResult(tx.Exec(ctx, "SELECT 1"))
						if rollback {
							panic(original)
						}
					}, OptNoRetry())
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("transaction never entered its finish statement")
			}
			assertPgPoolWrapperLifecycleMetrics(t, pool, 1, 0, 0)
			release()
			select {
			case <-joined:
			case <-ctx.Done():
				t.Fatal("transaction finish did not join")
			}
			if (!rollback && outcome != nil) || (rollback && outcome != original) {
				t.Fatal("lifecycle observation changed transaction outcome authority")
			}
			assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 0, 0)
		})
	}
}
