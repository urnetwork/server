// Disposable PostgreSQL authenticates the real public Heartbeat/RegisterHost
// proofs. A sole acknowledged pool holder, not sleeps, forces acquisition loss.
package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The real store cannot admit BEGIN, SQL or a partial lease update while the
// only main-pool connection is held. Retry releases it only after full unwind.
func TestCompetitionHeartbeatAcquireRetryPgComposedPreservesOriginalClaim(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "acquire-continuation")
		before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		clock := &competitionLeaseRetryClock{now: fixture.clock}
		fixture.store.now = clock.Now
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		release := holdCompetitionSupervisorAcquirePgPool(t, ctx)
		defer release()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = clock.Now
		calls, waits := 0, 0
		var firstError error
		settings.LeaseRetry.Wait = func(wait context.Context, delay time.Duration) error {
			waits++
			if wait.Err() != nil || !competitionHeartbeatUnadmittedDeadline(firstError) {
				return errors.New("real unadmitted heartbeat deadline did not reach its live recovery reserve")
			}
			release()
			if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(before, after) {
				return errors.New("unadmitted heartbeat changed durable claim or either lease")
			}
			clock.Advance(delay)
			return nil
		}
		ack, err := retryCompetitionHeartbeatOperation(ctx, *job.LeaseExpiresAt, settings, false, func(attempt context.Context) error {
			calls++
			err := fixture.store.Heartbeat(attempt, fixture.settings, "synthetic-lease-worker", job.JobId)
			if calls == 1 {
				firstError = err
			}
			return err
		})
		if err != nil || calls != 2 || waits != 1 || ack.IsZero() || !competitionHeartbeatUnadmittedDeadline(firstError) {
			t.Fatal("real pool deadline canceled authority instead of continuing", calls, waits, err)
		}
		proof := firstError.(competitionHeartbeatDatabaseError).snapshot
		if proof.Acquire.Admitted || proof.Acquire.PoolEntry.Maximum != 1 || proof.Acquire.PoolEntry.Acquired != 1 || proof.Acquire.PoolReturn == nil ||
			proof.Transaction.AcquireSucceeded != 0 || proof.Transaction.BeginAttempts != 0 || proof.Transaction.CallbackEntered != 0 || proof.Transaction.CommitAttempts != 0 || len(proof.Statements) != 0 {
			t.Fatal("actual pool/transaction observations did not prove complete refusal", proof)
		}
		after := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		if after.slotWorker == nil || before.slotWorker == nil || *after.slotWorker != *before.slotWorker || after.jobOwner == nil || before.jobOwner == nil || *after.jobOwner != *before.jobOwner ||
			after.slotJob == nil || *after.slotJob != job.JobId || after.started == nil || before.started == nil || !after.started.Equal(*before.started) || after.attempts != before.attempts ||
			after.jobExpires == nil || before.jobExpires == nil || !after.jobExpires.After(*before.jobExpires) || after.slotExpires == nil || !after.slotExpires.Equal(*after.jobExpires) {
			t.Fatal("recovered heartbeat changed original claim/budget or failed atomic renewal", before, after)
		}
	})
}

// The adjacent host path supplies its own real no-admission observation and
// acknowledges only its own registration, without modifying job authority.
func TestCompetitionHostAcquireRetryPgComposedKeepsJobAuthoritySeparate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "host-acquire-continuation")
		before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		clock := &competitionLeaseRetryClock{now: fixture.clock}
		fixture.store.now = clock.Now
		check := passingHostCheck(fixture.settings)
		if err := fixture.store.RegisterHost(ctx, fixture.settings, check); err != nil {
			t.Fatal(err)
		}
		readHost := func() time.Time {
			var heartbeat time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT heartbeat_at FROM competition_evaluator_host WHERE host_id=$1`, check.HostId).Scan(&heartbeat))
			})
			return heartbeat
		}
		originalHost := readHost()
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		release := holdCompetitionSupervisorAcquirePgPool(t, ctx)
		defer release()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = clock.Now
		calls, waits := 0, 0
		var firstError error
		settings.LeaseRetry.Wait = func(wait context.Context, delay time.Duration) error {
			waits++
			if wait.Err() != nil || !competitionHostHeartbeatUnadmittedDeadline(firstError) {
				return errors.New("real host acquisition has no live complete recovery proof")
			}
			release()
			if !readHost().Equal(originalHost) {
				return errors.New("unadmitted host registration updated eligibility freshness")
			}
			clock.Advance(delay)
			return nil
		}
		expires := originalHost.Add(time.Duration(fixture.settings.HostHeartbeatMaxAgeSeconds) * time.Second)
		ack, err := retryCompetitionHeartbeatOperation(ctx, expires, settings, true, func(attempt context.Context) error {
			calls++
			err := fixture.store.RegisterHost(attempt, fixture.settings, check)
			if calls == 1 {
				firstError = err
			}
			return err
		})
		if err != nil || calls != 2 || waits != 1 || ack.IsZero() || !competitionHostHeartbeatUnadmittedDeadline(firstError) || !readHost().After(originalHost) {
			t.Fatal("real host acquisition did not recover within its confirmed bound", calls, waits, err)
		}
		if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(before, after) {
			t.Fatal("host acquisition recovery renewed job authority or claim budget")
		}
	})
}

// A genuine committed inner attempt followed by a refused acquisition retains
// admission history even though its last finite database phase is Acquire.
func TestCompetitionHeartbeatAcquireRetryPgRejectsEarlierTransactionHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		var release func()
		defer func() {
			if release != nil {
				release()
			}
		}()
		err := captureCompetitionHeartbeatDatabase(ctx, func(observation *competitionHeartbeatDatabaseObservation) {
			server.Tx(ctx, func(tx server.PgTx) {
				tx = observation.beginCallback(tx)
				observation.enterStep(competitionHeartbeatDatabaseSlotLock)
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1`))
			}, &observation.phase, &observation.timing, &observation.transaction)
			release = holdCompetitionSupervisorAcquirePgPool(t, ctx)
			child, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			server.Tx(child, func(server.PgTx) { panic("synthetic later acquisition must never admit") }, &observation.phase, &observation.timing, &observation.transaction)
		})
		proof, ok := err.(competitionHeartbeatDatabaseError)
		if !ok || !errors.Is(err, context.DeadlineExceeded) || proof.snapshot.Transaction.AcquireSucceeded != 1 || proof.snapshot.Transaction.CommitAcknowledged != 1 ||
			proof.snapshot.CallbackEntries != 1 || competitionHeartbeatUnadmittedDeadline(err) {
			t.Fatal("final acquisition erased real prior SQL/commit history", err)
		}
	})
}
