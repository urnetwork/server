// The actual distributed job-before-slot callback reaches a held slot trigger.
// Explicit server-wait and cancellation acknowledgements precede rollback.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestCompetitionHeartbeatDistributedSlotRetryPgAcknowledgedRollbackPreservesClaim(t *testing.T) {
	outer := t
	const interrupt = false
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 45*time.Second)
		defer stopWatchdog()
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		fixture := newCompetitionDistributedPgFixture(outer, t, true, true)
		selected := fixture.selected(0)
		job, claimErr := fixture.store.Claim(watchdog, selected, "synthetic-distributed-slot-worker", testWorkerImageDigest())
		if claimErr != nil || job == nil {
			t.Fatal("genuine distributed claim failed", claimErr)
		}
		before := readCompetitionDistributedMergedHeartbeat(watchdog, job.JobId)
		fixture.clock.Advance(time.Second)
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
			 BEFORE UPDATE OF lease_expires_at ON competition_execution_slot FOR EACH ROW
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
		settings.LeaseRetry.Now = func() time.Time { return fixture.clock.Now() }
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
			ack, err := retryCompetitionJobHeartbeatOperation(operationCtx, *job.LeaseExpiresAt, fixture.clock.Now().Add(time.Minute), settings, func(ctx context.Context) error {
				calls++
				actual := ctx
				if calls == 1 {
					actual = &competitionHeartbeatPgDeadlineContext{Context: ctx, stopped: deadlineObserved}
					attempts <- actual
				}
				err := fixture.store.Heartbeat(withCompetitionJobAuthority(actual, job), selected, "synthetic-distributed-slot-worker", job.JobId)
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
		if !ok || !competitionHeartbeatDistributedSlotUpdateDeadline(failed) || proof.snapshot.Transaction.CommitAttempts != 0 || proof.snapshot.Transaction.ReleaseReturned != 1 {
			t.Fatal("actual store failed to produce complete closed no-COMMIT evidence", competitionHeartbeatFailureDetails(failed))
		}
		if interrupt {
			if proof.snapshot.Transaction.RollbackAcknowledged != 0 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseDestroyTransferred {
				t.Fatal("interrupted statement was not transferred to its existing destructor", proof.snapshot.Transaction)
			}
		} else if proof.snapshot.Transaction.RollbackAcknowledged != 1 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseIdle {
			t.Fatal("acknowledged rollback did not return its actual idle session", proof.snapshot.Transaction)
		}
		if after := readCompetitionDistributedMergedHeartbeat(watchdog, job.JobId); !reflect.DeepEqual(before, after) {
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
		after := readCompetitionDistributedMergedHeartbeat(watchdog, job.JobId)
		if !after.jobLease.After(before.jobLease) || !after.jobLease.Equal(after.slotLease) || !after.startedAt.Equal(before.startedAt) || !after.deadline.Equal(before.deadline) || after.fence != before.fence || after.attempt != before.attempt {
			t.Fatal("renewal reset original distributed clock/fence/attempt or lost atomic lease extension", before, after)
		}
	})
}
