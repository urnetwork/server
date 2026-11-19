// Disposable PostgreSQL authenticates the exact automated singleton heartbeat
// graph. Artificial trigger latency models the mechanism, not incident cause.
package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Force the public AfterFunc fallback while preserving the real timer,
// deadline error and application values. WithoutCancel's value lookup hides
// exactly the private cancelCtx key, forcing child-cancel registration through
// this test's acknowledged fallback. Arbitrary custom causes are not preserved.
type competitionHeartbeatPgDeadlineContext struct {
	context.Context
	stopped chan struct{}
	once    sync.Once
}

func (self *competitionHeartbeatPgDeadlineContext) Value(key any) any {
	return context.WithoutCancel(self.Context).Value(key)
}

// The callback has marked the actual AfterFunc child canceled before this
// witness closes, so stopping it at statement return cannot win a scheduler race.
func (self *competitionHeartbeatPgDeadlineContext) AfterFunc(callback func()) func() bool {
	return context.AfterFunc(self.Context, func() {
		callback()
		self.once.Do(func() { close(self.stopped) })
	})
}

// Use the real automated claim/fence rather than manufacturing a proof class.
func newCompetitionHeartbeatSlotPgFixture(t testing.TB, ctx context.Context) (*competitionRankingFixture, *queuedJob) {
	t.Helper()
	fixture := newCompetitionAutomationFixture(t)
	fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
	lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-slot-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
	jobId := queueCompetitionAutomationPollJob(t, fixture)
	job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-slot-worker", testWorkerImageDigest())
	if err != nil || job == nil || job.JobId != jobId {
		t.Fatal("synthetic automated heartbeat could not claim its exact job", err)
	}
	return fixture, job
}

// Both tests force the same first failed SlotUpdate. Only when the actual
// server wait is released determines ACK rollback versus owned destruction.
func testCompetitionHeartbeatSlotPgContinuation(t *testing.T, interrupt bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 45*time.Second)
		defer stopWatchdog()
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		fixture, job := newCompetitionHeartbeatSlotPgFixture(t, watchdog)
		before := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		fixture.now = fixture.now.Add(time.Second)
		class, object := int32(173301), int32(733011)
		const applicationName = "synthetic-slot-deadline"
		server.Tx(watchdog, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(watchdog, `CREATE SEQUENCE synthetic_slot_deadline_calls;
			 CREATE FUNCTION synthetic_slot_deadline_latency() RETURNS trigger AS $body$
			 BEGIN
			  IF NEW.job_id::text=TG_ARGV[0] AND NEW.lease_expires_at>OLD.lease_expires_at THEN
			   IF nextval('synthetic_slot_deadline_calls'::regclass)=1 THEN
			    PERFORM set_config('lock_timeout','0',true);
			    PERFORM set_config('application_name',TG_ARGV[3],true);
			    PERFORM pg_advisory_xact_lock(TG_ARGV[1]::integer,TG_ARGV[2]::integer);
			   END IF;
			  END IF;
			  RETURN NEW;
			 END $body$ LANGUAGE plpgsql;`))
			server.RaisePgResult(tx.Exec(watchdog, fmt.Sprintf(`CREATE TRIGGER synthetic_slot_deadline_latency
			 BEFORE UPDATE OF lease_expires_at ON competition_worker_slot FOR EACH ROW
			 EXECUTE FUNCTION synthetic_slot_deadline_latency('%s','%d','%d','%s')`, job.JobId.String(), class, object, applicationName)))
		})
		held, releaseHolder := make(chan int32, 1), make(chan struct{})
		holderDone := make(chan error, 1)
		var holderOnce sync.Once
		unlock := func() { holderOnce.Do(func() { close(releaseHolder) }) }
		go func() {
			holderDone <- captureDatabaseError(func() {
				server.MaintenanceTx(watchdog, func(tx server.PgTx) {
					var pid int32
					server.Raise(tx.QueryRow(watchdog, `SELECT pg_backend_pid() FROM pg_advisory_xact_lock($1::integer,$2::integer)`, class, object).Scan(&pid))
					held <- pid
					select {
					case <-releaseHolder:
					case <-watchdog.Done():
						panic(watchdog.Err())
					}
				}, server.OptNoRetry())
			})
		}()
		var holderPid int32
		select {
		case holderPid = <-held:
		case err := <-holderDone:
			t.Fatal("synthetic advisory holder failed before its acknowledgement", err)
		case <-watchdog.Done():
			t.Fatal("synthetic advisory holder did not acknowledge its exact lock")
		}
		operationCtx, cancelOperation := context.WithCancel(watchdog)
		defer cancelOperation()
		attempts := make(chan context.Context, 1)
		deadlineObserved := make(chan struct{})
		proofReady, allowRetry := make(chan error, 1), make(chan struct{})
		var retryOnce sync.Once
		releaseRetry := func() { retryOnce.Do(func() { close(allowRetry) }) }
		type result struct {
			ack   time.Time
			err   error
			calls int
		}
		done := make(chan result, 1)
		joined := make(chan struct{})
		defer func() {
			cancelOperation()
			unlock()
			releaseRetry()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(watchdog), 40*time.Second)
			defer cancel()
			select {
			case <-joined:
			case <-cleanup.Done():
				t.Error("actual heartbeat did not join its existing cleanup owner")
			}
			select {
			case err := <-holderDone:
				if err != nil && watchdog.Err() == nil {
					t.Error("synthetic advisory holder failed", err)
				}
			case <-cleanup.Done():
				t.Error("synthetic holder did not join")
			}
		}()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = func() time.Time { return fixture.now }
		originalWait := settings.LeaseRetry.Wait
		calls := 0
		var firstError error
		settings.LeaseRetry.Wait = func(ctx context.Context, delay time.Duration) error {
			if calls == 1 {
				proofReady <- firstError
				select {
				case <-allowRetry:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return originalWait(ctx, delay)
		}
		go func() {
			defer close(joined)
			ack, err := retryCompetitionJobHeartbeatOperation(operationCtx, *job.LeaseExpiresAt, fixture.now.Add(time.Minute), settings, func(ctx context.Context) error {
				calls++
				actual := ctx
				if calls == 1 {
					actual = &competitionHeartbeatPgDeadlineContext{Context: ctx, stopped: deadlineObserved}
					attempts <- actual
				}
				err := fixture.store.Heartbeat(actual, fixture.settings, "synthetic-slot-worker", job.JobId)
				if calls == 1 {
					firstError = err
				}
				return err
			})
			done <- result{ack: ack, err: err, calls: calls}
		}()
		var attempt context.Context
		select {
		case attempt = <-attempts:
		case <-watchdog.Done():
			t.Fatal("actual heartbeat did not dispatch")
		}
		waitingPid := int32(0)
		for waitingPid == 0 {
			var err error
			waitingPid, err = competitionHeartbeatCallbackWaitingBackend(watchdog, holderPid, applicationName)
			if err != nil {
				t.Fatal(err)
			}
			if waitingPid == 0 {
				select {
				case returned := <-done:
					t.Fatal("heartbeat returned before its positively observed server wait", returned.err)
				case <-watchdog.Done():
					t.Fatal("actual slot UPDATE did not reach its advisory barrier")
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		select {
		case <-attempt.Done():
		case <-watchdog.Done():
			t.Fatal("real default heartbeat deadline did not expire")
		}
		select {
		case <-deadlineObserved:
		case <-watchdog.Done():
			t.Fatal("actual statement stop did not acknowledge its real deadline")
		}
		if !interrupt {
			unlock()
		}
		var failed error
		select {
		case failed = <-proofReady:
		case returned := <-done:
			t.Fatal("actual no-COMMIT failure did not authorize the bounded continuation", returned.err)
		case <-watchdog.Done():
			t.Fatal("actual statement/rollback/release did not return")
		}
		proof, ok := failed.(competitionHeartbeatDatabaseError)
		if !ok || !competitionHeartbeatSlotUpdateDeadline(failed) || proof.snapshot.Transaction.CommitAttempts != 0 || proof.snapshot.Transaction.ReleaseReturned != 1 {
			t.Fatal("actual store failed to produce complete closed no-COMMIT evidence", competitionHeartbeatFailureDetails(failed))
		}
		if interrupt {
			if proof.snapshot.Transaction.RollbackAcknowledged != 0 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseDestroyTransferred {
				t.Fatal("interrupted statement was not transferred to its existing destructor", proof.snapshot.Transaction)
			}
		} else if proof.snapshot.Transaction.RollbackAcknowledged != 1 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseIdle {
			t.Fatal("acknowledged rollback did not return its actual idle session", proof.snapshot.Transaction)
		}
		if after := readCompetitionLeasePgSnapshot(watchdog, job.JobId); !reflect.DeepEqual(before, after) {
			t.Fatal("failed first UPDATE changed either committed authority or original claim", before, after)
		}
		unlock()
		releaseRetry()
		select {
		case returned := <-done:
			if returned.err != nil || returned.calls != 2 || returned.ack.IsZero() {
				t.Fatal("same-claim renewal did not finish inside its original reserve", returned.calls, returned.err)
			}
		case <-watchdog.Done():
			t.Fatal("bounded continuation did not finish")
		}
		after := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		if after.attempts != before.attempts || after.started == nil || before.started == nil || !after.started.Equal(*before.started) ||
			after.jobOwner == nil || before.jobOwner == nil || *after.jobOwner != *before.jobOwner || after.slotWorker == nil || before.slotWorker == nil || *after.slotWorker != *before.slotWorker ||
			after.slotJob == nil || *after.slotJob != job.JobId || after.jobExpires == nil || before.jobExpires == nil || !after.jobExpires.After(*before.jobExpires) ||
			after.slotExpires == nil || !after.slotExpires.Equal(*after.jobExpires) {
			t.Fatal("renewal reset the original attempt/start or failed atomic slot/job extension", before, after)
		}
	})
}

// A server reply after the caller deadline still rolls back before continuation.
func TestCompetitionHeartbeatSlotRetryPgAcknowledgedRollbackPreservesClaim(t *testing.T) {
	testCompetitionHeartbeatSlotPgContinuation(t, false)
}

// Holding through the unchanged statement grace reproduces the event-426
// class: zero COMMIT, unacknowledged rollback, returned owned destruction.
func TestCompetitionHeartbeatSlotRetryPgUnacknowledgedRollbackPreservesClaim(t *testing.T) {
	testCompetitionHeartbeatSlotPgContinuation(t, true)
}

// Positive proof does not excuse a stale supervisor fence on the next call.
func TestCompetitionHeartbeatSlotRetryPgRechecksSupervisorFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture, job := newCompetitionHeartbeatSlotPgFixture(t, t.Context())
		before := readCompetitionLeasePgSnapshot(t.Context(), job.JobId)
		previous := *fixture.settings.automationLease
		fixture.now = previous.LeaseExpiresAt
		if _, err := fixture.store.AcquireAutomationLease(t.Context(), fixture.settings, "synthetic-next-slot-supervisor", 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.Heartbeat(t.Context(), fixture.settings, "synthetic-slot-worker", job.JobId); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("stale supervisor retained heartbeat authority", err)
		}
		if after := readCompetitionLeasePgSnapshot(t.Context(), job.JobId); !reflect.DeepEqual(before, after) {
			t.Fatal("stale supervisor changed original job/slot authority", before, after)
		}
	})
}
