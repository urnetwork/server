// Finite wire barriers separate pool admission from an admitted read. These
// controls do not identify the phase of a production census timeout.
package server

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A held sole connection must consume the read's own deadline without entering
// its callback; releasing that holder restores the same pool's healthy path.
func TestDbReadObservationHeldAcquireDoesNotEnterQuery(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	setupCtx, setupCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer setupCancel()
	held, err := pool.open().Acquire(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(held.Release) }
	defer release()
	fixture.stateLock.Lock()
	queriesBefore := len(fixture.queries)
	fixture.stateLock.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	var observation DbTiming
	readObservation := NewDbReadObservation()
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(ctx, pool, func(PgConn) { callbacks++ }, &observation, readObservation)
	})
	read := readObservation.Snapshot()
	if read.PhaseCounts[DbReadAcquireBegin] != 1 || read.PhaseCounts[DbReadAcquireDone] != 1 || read.AcquireDuration <= 0 {
		t.Fatalf("missing exact acquisition phase: %+v", read)
	}
	recoveredErr, _ := recovered.(error)
	fixture.stateLock.Lock()
	queriesAfter := len(fixture.queries)
	fixture.stateLock.Unlock()
	if !errors.Is(recoveredErr, context.DeadlineExceeded) || callbacks != 0 || queriesAfter != queriesBefore {
		t.Fatalf("held acquire crossed into read: deadline=%t callbacks=%d new_wire_queries=%d",
			errors.Is(recoveredErr, context.DeadlineExceeded), callbacks, queriesAfter-queriesBefore)
	}
	if observation.Phases[DbTimingAcquire].Count != 1 || observation.Phases[DbTimingAcquire].Duration <= 0 || pool.open().Stat().AcquiredConns() != 1 {
		t.Fatalf("missing held-pool acquisition evidence: %+v", observation)
	}
	for _, phase := range []DbTimingPhase{DbTimingBegin, DbTimingCommit, DbTimingRollback} {
		if observation.Phases[phase] != (DbTimingSample{}) {
			t.Fatalf("read acquire invented transaction phase %d", phase)
		}
	}

	if read.AcquireSucceeded != 0 || read.PhaseCounts[DbReadQueryBegin] != 0 || read.Rows != 0 {
		t.Fatalf("failed acquisition invented admitted work: %+v", read)
	}
	release()
	healthyCtx, healthyCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer healthyCancel()
	var healthy DbTiming
	if recovered := captureDbErrorPanic(func() {
		dbWithPool(healthyCtx, pool, func(conn PgConn) {
			callbacks++
			RaisePgResult(conn.Exec(healthyCtx, "SELECT 1"))
		}, &healthy)
	}); recovered != nil || callbacks != 1 || healthy.Phases[DbTimingAcquire].Count != 1 {
		t.Fatalf("released holder did not restore healthy read: callbacks=%d observation=%+v error=%v", callbacks, healthy, recovered)
	}
}

func TestDbReadObservationRetainsActualRetryAcquisitions(t *testing.T) {
	failed := false
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
		fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
			if query == "SELECT 1" && !failed {
				failed = true
				return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic read retry"}
			}
			return nil
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	retry := OptRetryDefault()
	retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
	observation := NewDbReadObservation()
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(ctx, pool, func(conn PgConn) {
			callbacks++
			observation.BeginQuery()
			succeeded := false
			defer func() { observation.FinishQuery(succeeded) }()
			RaisePgResult(conn.Exec(ctx, "SELECT 1"))
			succeeded = true
		}, retry, observation)
	})
	observation.Finish(recovered == nil)
	read := observation.Snapshot()
	if recovered != nil || callbacks != 2 || read.AcquireSucceeded != 2 || read.QuerySucceeded != 1 || read.PhaseCounts[DbReadAcquireBegin] != 2 || read.PhaseCounts[DbReadAcquireDone] != 2 || read.PhaseCounts[DbReadQueryBegin] != 2 || read.PhaseCounts[DbReadQueryDone] != 2 || read.PhaseCounts[DbReadComplete] != 1 || read.PhaseCounts[DbReadError] != 0 {
		t.Fatalf("read retry observations changed retry or omitted an attempt: callbacks=%d error=%v read=%+v", callbacks, recovered, read)
	}
}

func TestDbReadObservationSnapshotsRemainImmutableAndTerminal(t *testing.T) {
	observation := NewDbReadObservation()
	starting := observation.Snapshot()
	observation.BeginAcquire()
	observation.FinishAcquire(true)
	observation.BeginQuery()
	observation.Row()
	observation.FinishQuery(true)
	observation.Finish(true)
	finished := observation.Snapshot()
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				observation.Row()
				observation.Finish(false)
				if got := observation.Snapshot(); !reflect.DeepEqual(got, finished) {
					t.Error("late event revised a terminal observation")
				}
			}
		}()
	}
	readers.Wait()
	if starting.Phase != DbReadStarting || starting.Rows != 0 || starting.Finished || finished.Phase != DbReadComplete || !finished.Finished || finished.Rows != 1 || finished.AcquireSucceeded != 1 || finished.QuerySucceeded != 1 {
		t.Fatal("snapshot aliases mutable or terminal state")
	}
	var absent *DbReadObservation
	absent.BeginAcquire()
	absent.FinishAcquire(false)
	absent.BeginQuery()
	absent.Row()
	absent.FinishQuery(false)
	absent.Finish(false)
	if absent.Snapshot() != (DbReadObservationSnapshot{}) {
		t.Fatal("disabled observer manufactured evidence")
	}
}

// An admitted query can exhaust the same context after a successful acquire.
// The server-side barrier remains held until the deadline result has joined.
func TestDbReadObservationAfterAdmissionRemainsQueryFailure(t *testing.T) {
	const query = "SELECT 17 /* synthetic census query barrier */"
	const queryBudget = 2 * time.Second
	const joinBudget = queryBudget + PgCloseTimeout + 2*time.Second
	queryEntered := make(chan struct{})
	releaseQuery := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseQuery) }) }
	defer release()
	_, pool := newPgPoolWireFixture(t, func(_ int, got string) bool {
		if got == query {
			enteredOnce.Do(func() { close(queryEntered) })
			<-releaseQuery
			return false
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	setupCtx, setupCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer setupCancel()
	warm, err := pool.open().Acquire(setupCtx)
	if err != nil {
		t.Fatal(err)
	}
	warm.Release()

	ctx, cancel := context.WithTimeout(t.Context(), queryBudget)
	defer cancel()
	var observation DbTiming
	readObservation := NewDbReadObservation()
	var recovered any
	callbacks := 0
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		recovered = captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(conn PgConn) {
				callbacks++
				readObservation.BeginQuery()
				succeeded := false
				defer func() { readObservation.FinishQuery(succeeded) }()
				RaisePgResult(conn.Exec(ctx, query))
				succeeded = true
			}, &observation, readObservation)
		})
	}()
	defer func() {
		cancel()
		release()
		select {
		case <-joined:
		case <-time.After(joinBudget):
			t.Error("admitted read did not join after cancellation and barrier release")
		}
	}()
	select {
	case <-queryEntered:
	case <-joined:
		t.Fatal("read ended before the wire query entered")
	case <-time.After(5 * time.Second):
		t.Fatal("wire query did not reach the finite barrier")
	}
	select {
	case <-joined:
	case <-time.After(joinBudget):
		t.Fatal("query deadline did not release its caller while the reply remained held")
	}
	read := readObservation.Snapshot()
	if read.PhaseCounts[DbReadAcquireBegin] != 1 || read.PhaseCounts[DbReadAcquireDone] != 1 || read.AcquireDuration <= 0 {
		t.Fatalf("missing exact acquisition phase: %+v", read)
	}
	recoveredErr, _ := recovered.(error)
	if !errors.Is(recoveredErr, context.DeadlineExceeded) || callbacks != 1 || observation.Phases[DbTimingAcquire].Count != 1 || observation.Phases[DbTimingAcquire].Duration <= 0 {
		t.Fatalf("query deadline lost successful admission: deadline=%t callbacks=%d observation=%+v",
			errors.Is(recoveredErr, context.DeadlineExceeded), callbacks, observation)
	}
	if read.AcquireSucceeded != 1 || read.PhaseCounts[DbReadQueryBegin] != 1 || read.PhaseCounts[DbReadQueryDone] != 1 || read.QuerySucceeded != 0 || read.QueryDuration <= 0 || read.Rows != 0 {
		t.Fatalf("query failure lost admitted phase evidence: %+v", read)
	}
	for _, phase := range []DbTimingPhase{DbTimingBegin, DbTimingCommit, DbTimingRollback} {
		if observation.Phases[phase] != (DbTimingSample{}) {
			t.Fatalf("admitted read invented transaction phase %d", phase)
		}
	}
}
