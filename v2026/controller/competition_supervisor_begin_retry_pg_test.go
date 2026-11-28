// Disposable PostgreSQL supplies the real BEGIN/rollback/release observations.
// The held BEGIN reply crosses an owned deadline before any business callback.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// A session lock delays one actual BEGIN batch. Its independent holder releases
// only after the exact attempt stops; it never borrows the worker connection.
func TestCompetitionSupervisorWorkerBeginPgRecoversActualClosedTransaction(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-begin-parent", 2*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		const lockKey int64 = 718235
		held, beginWaiting, stopHolder := make(chan struct{}), make(chan struct{}), make(chan struct{})
		attempts := make(chan context.Context, 1)
		holderDone := make(chan error, 1)
		beginMarker := "synthetic-fence-begin-" + server.NewId().String()
		beginQuery := fmt.Sprintf("begin; SELECT pg_advisory_xact_lock(%d) /* %s */", lockKey, beginMarker)
		var stopOnce sync.Once
		stop := func() { stopOnce.Do(func() { close(stopHolder) }) }
		go func() {
			holderDone <- captureDatabaseError(func() {
				server.MaintenanceDb(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey))
					defer func() {
						cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						defer stop()
						if _, err := conn.Exec(cleanup, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
							t.Error("synthetic session lock did not join cleanup", err)
						}
					}()
					close(held)
					select {
					case attempt := <-attempts:
						// Observe this unique query actually waiting for the server lock
						// before the attempt stops, not merely dispatch into the caller.
						for {
							if err := attempt.Err(); err != nil {
								panic(errors.New("owned attempt stopped before the server acknowledged BEGIN wait"))
							}
							var waiting bool
							server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
							 WHERE pid<>pg_backend_pid() AND strpos(query,$1)>0 AND wait_event_type='Lock')`, beginMarker).Scan(&waiting))
							if waiting {
								if err := attempt.Err(); err != nil {
									panic(errors.New("BEGIN wait was observed only after its owned deadline"))
								}
								close(beginWaiting)
								break
							}
							select {
							case <-attempt.Done():
								panic(errors.New("owned attempt stopped before the server acknowledged BEGIN wait"))
							case <-stopHolder:
								return
							case <-ctx.Done():
								panic(ctx.Err())
							case <-time.After(10 * time.Millisecond):
							}
						}
						select {
						case <-attempt.Done():
						case <-stopHolder:
						}
					case <-stopHolder:
					}
				}, server.OptNoRetry())
			})
		}()
		holderJoined := false
		defer func() {
			stop()
			if !holderJoined {
				if err := <-holderDone; err != nil {
					t.Error("synthetic BEGIN holder failed its joined cleanup", err)
				}
			}
		}()
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal("synthetic BEGIN holder did not acknowledge its lock")
		}
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks, accepted := make(chan time.Time), make(chan struct{}, 1)
		settings.tickForTest, settings.confirmedForTest = ticks, func() { accepted <- struct{}{} }
		workerCtx, stopWorker := context.WithCancel(ctx)
		defer stopWorker()
		jobStartedAt := time.Now()
		originalDeadline := jobStartedAt.Add(3 * time.Hour)
		type evaluationOwner struct {
			context             context.Context
			deadline            time.Time
			declaredJobDeadline time.Time
			hasDeadline         bool
		}
		evaluationContext := make(chan evaluationOwner, 1)
		done := make(chan error, 1)
		calls, runs, callbacks := 0, 0, 0
		var firstError error
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(workerCtx, lease.LeaseExpiresAt, settings,
				func(attempt context.Context) (time.Time, error) {
					calls++
					if calls != 1 {
						return readCompetitionSupervisorWorkerFence(attempt, fixture.store, fixture.settings, *lease)
					}
					attempts <- attempt
					observation := newCompetitionSupervisorRenewObservation()
					firstError = observation.captureContext(attempt, lease.LeaseExpiresAt, func() {
						// The ordinary fence reader supplies no alternate BEGIN. This test
						// batch holds only the same pre-callback acknowledgment boundary.
						server.Tx(attempt, func(server.PgTx) {
							callbacks++
							observation.callbackEntered, observation.callbacks = true, observation.callbacks+1
							panic(errors.New("late BEGIN entered synthetic business work"))
						}, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite, DeferrableMode: pgx.NotDeferrable, BeginQuery: beginQuery}, server.OptNoRetry(),
							&observation.phase, &observation.timing, &observation.acquire, observation.read, &observation.transaction)
					})
					return time.Time{}, firstError
				}, func(parent context.Context) error {
					runs++
					evaluation, stop := context.WithDeadline(parent, originalDeadline)
					defer stop()
					deadline, hasDeadline := evaluation.Deadline()
					evaluationContext <- evaluationOwner{context: evaluation, deadline: deadline, declaredJobDeadline: originalDeadline, hasDeadline: hasDeadline}
					<-evaluation.Done()
					return evaluation.Err()
				})
		}()
		workerJoined := false
		defer func() {
			stopWorker()
			stop()
			if !workerJoined {
				<-done
			}
		}()
		var initialEvaluation evaluationOwner
		select {
		case initialEvaluation = <-evaluationContext:
		case early := <-done:
			workerJoined = true
			t.Fatal("worker refused before its evaluation entry", early)
		case <-ctx.Done():
			t.Fatal("worker did not enter its bounded evaluation owner", ctx.Err())
		}
		evaluation := initialEvaluation.context
		parentDeadline, parentHasDeadline := workerCtx.Deadline()
		if !initialEvaluation.hasDeadline || !parentHasDeadline || !initialEvaluation.deadline.Equal(parentDeadline) ||
			!initialEvaluation.deadline.Before(initialEvaluation.declaredJobDeadline) || !initialEvaluation.declaredJobDeadline.Equal(originalDeadline) ||
			originalDeadline.Sub(jobStartedAt) != 3*time.Hour || evaluation.Err() != nil {
			t.Fatal("synthetic shorter-parent fixture did not preserve its initial effective/job clocks", initialEvaluation.deadline, parentDeadline, originalDeadline, evaluation.Err())
		}
		select {
		case ticks <- time.Now():
		case early := <-done:
			workerJoined = true
			t.Fatal("worker ended before its fence tick", early)
		case <-ctx.Done():
			t.Fatal("worker did not accept its bounded fence tick", ctx.Err())
		}
		select {
		case <-beginWaiting:
		case err := <-holderDone:
			holderJoined = true
			t.Fatal("synthetic holder never acknowledged the actual BEGIN wait", err)
		case early := <-done:
			workerJoined = true
			t.Fatal("worker ended before the actual BEGIN wait", early)
		case <-ctx.Done():
			t.Fatal("server did not acknowledge the bounded BEGIN wait", ctx.Err())
		}
		select {
		case <-accepted:
		case early := <-done:
			workerJoined = true
			t.Fatal("actual closed BEGIN canceled rather than recovered its original worker", early)
		case <-ctx.Done():
			t.Fatal("actual BEGIN did not return/join its original bounded owner", ctx.Err())
		}
		if err := <-holderDone; err != nil {
			t.Fatal("synthetic BEGIN holder failed", err)
		}
		holderJoined = true
		proof, ok := firstError.(competitionSupervisorRenewError)
		if !ok || !competitionSupervisorWorkerBeginDeadline(firstError) || proof.phase != server.DbOperationBegin ||
			proof.step != competitionSupervisorRenewalUnknown || proof.readPrefix != (competitionSupervisorReadPrefixSnapshot{valid: true}) ||
			proof.callbackEntered || proof.callbacks != 0 || callbacks != 0 || proof.transaction.BeginSucceeded != 1 ||
			proof.transaction.LastBegin.AtReturn.State != "deadline" || proof.transaction.CallbackEntered != 0 || proof.transaction.CommitAttempts != 0 ||
			proof.transaction.RollbackAcknowledged != 1 || proof.transaction.LastRelease.Disposition != server.DbTxReleaseIdle || calls != 2 || runs != 1 {
			t.Fatal("real late-BEGIN shape did not authenticate no-business rollback/recovery", calls, runs, callbacks, proof.transaction, firstError)
		}
		if deadline, present := evaluation.Deadline(); !present || !deadline.Equal(initialEvaluation.deadline) ||
			!originalDeadline.Equal(initialEvaluation.declaredJobDeadline) || originalDeadline.Sub(jobStartedAt) != 3*time.Hour || evaluation.Err() != nil {
			t.Fatal("BEGIN read recovery changed the original effective/job clocks or cancellation", deadline, initialEvaluation.deadline, originalDeadline, evaluation.Err())
		}
		var durable time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&durable))
		}, server.OptNoRetry())
		if !durable.Equal(lease.LeaseExpiresAt) {
			t.Fatal("fence read renewed the parent's lease")
		}
		stopWorker()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("recovered worker failed joined cancellation", err)
		}
		workerJoined = true
	})
}
