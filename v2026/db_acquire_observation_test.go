package server

// Actual pgx/Puddle and the existing wire peer own every admission outcome.
// These controls add no production seam, database server or configuration.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A returned deadline can coexist with a healthy, idle pool. Observation must
// preserve the exact error and add no wire work before the next healthy caller.
func TestDbAcquireObservationExpiredCallerKeepsHealthyPool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, pgFreshnessFixtureConfig)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conn.Release()
		synctest.Wait()
		beforeQueries, beforePings, beforeDials := len(fixture.queries), fixture.pingCount, fixture.dialCount
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		observation := &DbAcquireObservation{}
		timing := &DbTiming{Acquire: observation}
		phase := &DbPhaseObservation{}
		calls := 0
		original := captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(PgTx) { calls++ }, OptNoRetry(), timing, phase)
		})
		if original != context.DeadlineExceeded || calls != 0 || phase.Phase() != DbOperationAcquire ||
			!observation.Returned || observation.Admitted || observation.BeforeOpen.State != "deadline" ||
			observation.BeforeAcquire.State != "deadline" || observation.AtReturn.State != "deadline" ||
			!observation.BeforeAcquire.HasDeadline || observation.BeforeAcquire.RemainingNs >= 0 ||
			observation.PoolEntry.Idle != 1 || observation.PoolEntry.Acquired != 0 || observation.PoolReturn == nil {
			t.Fatal("expired healthy-pool caller lost its exact admission evidence", original, calls, observation)
		}
		if len(fixture.queries) != beforeQueries || fixture.pingCount != beforePings || fixture.dialCount != beforeDials {
			t.Fatal("observation added wire work to a stopped acquisition")
		}
		for _, sample := range timing.Phases[DbTimingBegin:] {
			if sample != (DbTimingSample{}) {
				t.Fatal("failed admission fabricated a transaction phase")
			}
		}
		dbWithPool(t.Context(), pool, func(conn PgConn) {
			calls++
			RaisePgResult(conn.Exec(t.Context(), "SELECT 1"))
		}, OptNoRetry(), timing, phase)
		if calls != 1 || !observation.Admitted || observation.PoolReturn != nil || observation.Attempt != 2 ||
			fixture.dialCount != beforeDials || fixture.pingCount != beforePings || len(fixture.queries) != beforeQueries+1 {
			t.Fatal("next healthy acquisition retained a stale refusal or added validation", observation)
		}
	})
}

// A held sole resource consumes the real caller deadline. Releasing it proves
// the same pool can proceed; the failed call never enters BEGIN or its body.
func TestDbAcquireObservationHeldConnectionThenHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, pgFreshnessFixtureConfig)
		held, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(held.Release) }
		defer release()
		queries := len(fixture.queries)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		observation := &DbAcquireObservation{}
		timing := &DbTiming{Acquire: observation}
		phase := &DbPhaseObservation{}
		calls := 0
		joined := make(chan any, 1)
		go func() {
			joined <- captureDbErrorPanic(func() {
				txWithPool(ctx, pool, func(PgTx) { calls++ }, OptNoRetry(), timing, phase)
			})
		}()
		synctest.Wait()
		synctest.Sleep(time.Second)
		if original := <-joined; original != context.DeadlineExceeded {
			t.Fatal("held acquire changed its original error", original)
		}
		if calls != 0 || len(fixture.queries) != queries || phase.Phase() != DbOperationAcquire ||
			observation.PoolOpenNs != 0 || observation.AcquireNs != int64(time.Second) ||
			observation.BeforeAcquire.State != "live" || observation.AtReturn.State != "deadline" ||
			observation.PoolEntry.Acquired != 1 || observation.PoolEntry.Maximum != 1 || observation.PoolEntry.Idle != 0 ||
			observation.PoolReturn == nil || observation.PoolReturn.Acquired != 1 || observation.PoolReturn.Canceled <= observation.PoolEntry.Canceled {
			t.Fatal("held acquisition timing, pool state or callback boundary changed", observation)
		}
		release()
		txWithPool(t.Context(), pool, func(PgTx) { calls++ }, OptNoRetry(), timing, phase)
		if calls != 1 || !observation.Admitted || observation.PoolReturn != nil {
			t.Fatal("released pool did not recover independently")
		}
	})
}

// The first budget read is an entry barrier, not a replacement clock. Keeping
// the open mutex held until the real child stops consumes its allowance there.
type pgAcquireOpenBudgetContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (self *pgAcquireOpenBudgetContext) Deadline() (time.Time, bool) {
	self.once.Do(func() { close(self.entered) })
	return self.Context.Deadline()
}

func TestDbAcquireObservationOpenConsumesCallerBudget(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	pool.mutex.Lock()
	locked := true
	defer func() {
		if locked {
			pool.mutex.Unlock()
		}
	}()
	caller, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx := &pgAcquireOpenBudgetContext{Context: caller, entered: make(chan struct{})}
	observation := &DbAcquireObservation{}
	timing := &DbTiming{Acquire: observation}
	joined := make(chan any, 1)
	go func() {
		joined <- captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(PgConn) { panic("synthetic callback must not run") }, OptNoRetry(), timing)
		})
	}()
	<-ctx.entered
	<-caller.Done()
	pool.mutex.Unlock()
	locked = false
	if original := <-joined; original != context.DeadlineExceeded {
		t.Fatal("open-budget stop changed original error", original)
	}
	if observation.BeforeOpen.State != "live" || observation.BeforeAcquire.State != "deadline" ||
		observation.BeforeAcquire.RemainingNs > 0 || observation.PoolOpenNs <= 0 || observation.AcquireNs < 0 ||
		observation.Admitted || !observation.Returned {
		t.Fatal("open wait was mislabeled as live-budget admission", observation)
	}
}

// Puddle asks for Done only after dbWithPool has its actual pool reference.
type pgAcquireWaitingContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (self *pgAcquireWaitingContext) Done() <-chan struct{} {
	self.once.Do(func() { close(self.entered) })
	return self.Context.Done()
}

// Holding reset's mutex while publishing another generation must neither
// block failure reporting nor substitute the replacement's larger capacity.
func TestDbAcquireObservationDoesNotTakeResetMutexOrReplacement(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	_, replacement := newPgPoolWireFixture(t, nil, neverPingTestPool,
		func(_ *pgPoolWireFixture, config *pgxpool.Config) { config.MaxConns = 3 })
	originalPool := pool.open()
	held, err := originalPool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &pgAcquireWaitingContext{Context: caller, entered: make(chan struct{})}
	observation := &DbAcquireObservation{}
	joined := make(chan any, 1)
	go func() {
		joined <- captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(PgConn) { panic("synthetic callback must not run") }, OptNoRetry(), &DbTiming{Acquire: observation})
		})
	}()
	<-ctx.entered
	pool.mutex.Lock()
	pool.pool = replacement.open()
	cancel()
	var result any
	returned := false
	select {
	case result = <-joined:
		returned = true
	case <-time.After(2 * time.Second):
	}
	pool.pool = originalPool
	pool.mutex.Unlock()
	if !returned {
		select {
		case <-joined:
		case <-time.After(2 * time.Second):
			t.Fatal("acquire owner did not join after reset barrier release")
		}
		t.Fatal("failure observation waited behind the pool reset mutex")
	}
	if result != context.Canceled || observation.PoolReturn == nil ||
		observation.PoolEntry.Maximum != 1 || observation.PoolReturn.Maximum != 1 || observation.PoolReturn.Acquired != 1 {
		t.Fatal("failure observation sampled a replacement pool or changed cancellation", result, observation)
	}
}

// Initial validation is detached from the stopped acquisition. A snapshot may
// show construction, but must not invent a request-owned Ping failure.
func TestDbAcquireObservationHeldInitialPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, releasePing := make(chan struct{}), make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(releasePing) }) }
		defer release()
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			if query == "-- ping" {
				close(entered)
				<-releasePing
			}
			return true
		}, neverPingTestPool, pgFreshnessFixtureConfig)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		observation := &DbAcquireObservation{}
		joined := make(chan any, 1)
		go func() {
			joined <- captureDbErrorPanic(func() {
				dbWithPool(ctx, pool, func(PgConn) { panic("synthetic callback must not run") }, OptNoRetry(), &DbTiming{Acquire: observation})
			})
		}()
		<-entered
		cancel()
		if original := <-joined; original != context.Canceled {
			t.Fatal("stopped constructor wait changed cause", original)
		}
		if observation.Admitted || observation.PoolReturn == nil || observation.PoolReturn.Constructing != 1 {
			t.Fatal("detached construction was hidden", observation)
		}
		release()
		dbWithPool(t.Context(), pool, func(PgConn) {}, OptNoRetry())
		if fixture.pingCount != 1 || fixture.dialCount != 1 {
			t.Fatal("observation added or replaced initial validation")
		}
	})
}

// An idle resource can still need validation. Its presence must not become a
// healthy admission or a queue-only claim, and ordinary disposal stays intact.
func TestDbAcquireObservationIdleValidationRemainsBroad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, releasePing := make(chan struct{}), make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(releasePing) }) }
		defer release()
		pings := 0
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			if query == "-- ping" {
				pings++
				if pings == 2 {
					close(entered)
					<-releasePing
				}
			}
			return true
		}, nil, pgFreshnessFixtureConfig)
		warm, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		warm.Release()
		synctest.Sleep(time.Second + time.Nanosecond)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		observation := &DbAcquireObservation{}
		phase := &DbPhaseObservation{}
		joined := make(chan any, 1)
		go func() {
			joined <- captureDbErrorPanic(func() {
				dbWithPool(ctx, pool, func(PgConn) { panic("synthetic callback must not run") }, OptNoRetry(), &DbTiming{Acquire: observation}, phase)
			})
		}()
		<-entered
		cancel()
		if original := <-joined; original != context.Canceled {
			t.Fatal("idle validation changed original acquisition stop", original)
		}
		if observation.Admitted || !observation.Returned || phase.Phase() != DbOperationAcquire || observation.PoolEntry.Idle != 1 {
			t.Fatal("idle validation was mislabeled as admitted work", observation)
		}
		release()
		synctest.Wait()
		dbWithPool(t.Context(), pool, func(PgConn) {}, OptNoRetry())
		if fixture.dialCount != 2 || fixture.pingCount != 3 {
			t.Fatal("observation changed idle disposal or replacement validation")
		}
	})
}

// A successful acquisition followed by BEGIN, callback or commit refusal must
// never be reported as failed admission, and its exact error remains visible.
func TestDbAcquireObservationPostAcquireRefusalsRemainDistinct(t *testing.T) {
	for _, stage := range []string{"begin", "callback", "commit"} {
		t.Run(stage, func(t *testing.T) {
			_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool,
				func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
					fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
						if (stage == "begin" && strings.HasPrefix(query, "begin")) || (stage == "commit" && query == "commit") {
							return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "23514", Message: "synthetic transaction refusal"}
						}
						return nil
					}
				})
			observation := &DbAcquireObservation{}
			phase := &DbPhaseObservation{}
			original := errors.New("synthetic callback refusal")
			calls := 0
			outcome := captureDbErrorPanic(func() {
				txWithPool(t.Context(), pool, func(PgTx) {
					calls++
					if stage == "callback" {
						panic(original)
					}
				}, OptNoRetry(), &DbTiming{Acquire: observation}, phase)
			})
			if outcome == nil || !observation.Admitted || !observation.Returned || observation.PoolReturn != nil || phase.Phase() == DbOperationAcquire {
				t.Fatal("post-acquisition refusal became failed admission", outcome, observation)
			}
			if stage == "callback" {
				if outcome != original || calls != 1 {
					t.Fatal("callback error replaced or replayed", outcome, calls)
				}
			} else {
				var pgErr *pgconn.PgError
				err, _ := outcome.(error)
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" || (stage == "begin" && calls != 0) || (stage == "commit" && calls != 1) {
					t.Fatal("database refusal lost its code or callback boundary", outcome, calls)
				}
			}
		})
	}
}
