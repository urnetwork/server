// A deferred private trigger holds real PostgreSQL COMMIT across the caller
// deadline. Its successful late reply must never cause a heartbeat replay.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The normal transaction's COMMIT is deliberately detached from its caller;
// no amount of no-COMMIT recovery may reinterpret that actual late success.
func TestCompetitionHeartbeatSlotRetryPgLateCommitNeverReplays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 40*time.Second)
		defer stopWatchdog()
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		fixture, job := newCompetitionHeartbeatSlotPgFixture(t, watchdog)
		before := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		fixture.now = fixture.now.Add(time.Second)
		class, object := int32(173311), int32(733021)
		const applicationName = "synthetic-slot-commit-latency"
		server.Tx(watchdog, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(watchdog, `CREATE FUNCTION synthetic_slot_commit_latency() RETURNS trigger AS $body$
			 BEGIN
			  IF NEW.job_id::text=TG_ARGV[0] AND NEW.lease_expires_at>OLD.lease_expires_at THEN
			   PERFORM set_config('lock_timeout','0',true);
			   PERFORM set_config('application_name',TG_ARGV[3],true);
			   PERFORM pg_advisory_xact_lock(TG_ARGV[1]::integer,TG_ARGV[2]::integer);
			  END IF;
			  RETURN NEW;
			 END $body$ LANGUAGE plpgsql;`))
			server.RaisePgResult(tx.Exec(watchdog, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER synthetic_slot_commit_latency
			 AFTER UPDATE ON competition_worker_slot DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
			 EXECUTE FUNCTION synthetic_slot_commit_latency('%s','%d','%d','%s')`, job.JobId.String(), class, object, applicationName)))
		})
		held, release := make(chan int32, 1), make(chan struct{})
		holderDone := make(chan error, 1)
		var releaseOnce sync.Once
		unlock := func() { releaseOnce.Do(func() { close(release) }) }
		go func() {
			holderDone <- captureDatabaseError(func() {
				server.MaintenanceTx(watchdog, func(tx server.PgTx) {
					var pid int32
					server.Raise(tx.QueryRow(watchdog, `SELECT pg_backend_pid() FROM pg_advisory_xact_lock($1::integer,$2::integer)`, class, object).Scan(&pid))
					held <- pid
					select {
					case <-release:
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
			t.Fatal("private COMMIT holder failed before acknowledgement", err)
		case <-watchdog.Done():
			t.Fatal("private COMMIT holder did not acquire its lock")
		}
		operationCtx, cancelOperation := context.WithCancel(watchdog)
		defer cancelOperation()
		attempts := make(chan context.Context, 1)
		type result struct {
			ack               time.Time
			err               error
			calls             int
			storeAcknowledged bool
		}
		done, joined := make(chan result, 1), make(chan struct{})
		defer func() {
			cancelOperation()
			unlock()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(watchdog), 40*time.Second)
			defer cancel()
			select {
			case <-joined:
			case <-cleanup.Done():
				t.Error("late COMMIT caller did not join")
			}
			select {
			case err := <-holderDone:
				if err != nil && watchdog.Err() == nil {
					t.Error("private COMMIT holder failed", err)
				}
			case <-cleanup.Done():
				t.Error("private COMMIT holder did not join")
			}
		}()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = func() time.Time { return fixture.now }
		go func() {
			defer close(joined)
			calls, storeAcknowledged := 0, false
			ack, err := retryCompetitionJobHeartbeatOperation(operationCtx, *job.LeaseExpiresAt, fixture.now.Add(time.Minute), settings, func(ctx context.Context) error {
				calls++
				if calls == 1 {
					attempts <- ctx
				}
				err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-slot-worker", job.JobId)
				storeAcknowledged = err == nil
				return err
			})
			done <- result{ack: ack, err: err, calls: calls, storeAcknowledged: storeAcknowledged}
		}()
		var attempt context.Context
		select {
		case attempt = <-attempts:
		case <-watchdog.Done():
			t.Fatal("late COMMIT heartbeat did not dispatch")
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
					t.Fatal("COMMIT returned before its deferred server barrier", returned.err)
				case <-watchdog.Done():
					t.Fatal("actual COMMIT did not reach its deferred barrier")
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		select {
		case <-attempt.Done():
		case <-watchdog.Done():
			t.Fatal("original heartbeat deadline did not expire")
		}
		unlock()
		select {
		case returned := <-done:
			if !errors.Is(returned.err, context.DeadlineExceeded) || !returned.ack.IsZero() || returned.calls != 1 || !returned.storeAcknowledged {
				t.Fatal("late actual COMMIT was adopted/replayed or lost its reply", returned)
			}
		case <-watchdog.Done():
			t.Fatal("late COMMIT did not return after release")
		}
		after := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		if after.attempts != before.attempts || after.started == nil || before.started == nil || !after.started.Equal(*before.started) ||
			after.jobExpires == nil || before.jobExpires == nil || !after.jobExpires.After(*before.jobExpires) || after.slotExpires == nil || !after.slotExpires.Equal(*after.jobExpires) {
			t.Fatal("real committed lease pair was absent or original claim changed", before, after)
		}
	})
}
