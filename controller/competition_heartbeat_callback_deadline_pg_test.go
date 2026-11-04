// A private trigger models server latency past an owned heartbeat deadline.
// Its lock_timeout override is artificial: this proves a callback/stop mechanism,
// not the physical cause of an incident or ordinary two-second lock behavior.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Observe an actual server-side advisory wait on the acknowledged holder, not
// a queued goroutine or time-based guess that the heartbeat reached its query.
// The polling timer only rate-limits observation; a positive backend is required.
func competitionHeartbeatCallbackWaitingBackend(ctx context.Context, holderPid int32, applicationName string) (int32, error) {
	var waitingPid int32
	err := captureDatabaseError(func() {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((
			 SELECT activity.pid FROM pg_stat_activity activity
			 WHERE activity.datname=current_database() AND activity.application_name=$2
			 AND activity.wait_event_type='Lock' AND activity.wait_event='advisory'
			 AND $1::integer=ANY(pg_blocking_pids(activity.pid))),0)`, holderPid, applicationName).Scan(&waitingPid))
		})
	})
	return waitingPid, err
}

// The slot write precedes a deliberately held job-update statement. Releasing
// it after the real default eight-second caller deadline, within the unchanged
// statement-stop grace, returns a plain callback deadline and rolls back both
// authorities. No acquire-only recovery or fresh submission clock is permitted.
func TestCompetitionHeartbeatPgCallbackDeadlineRollsBackWithoutAcquireRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 30*time.Second)
		defer stopWatchdog()
		// The holder and exact server observer own separate maintenance slots.
		// This configuration belongs only to the disposable TestEnv lifecycle.
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		fixture, job := newCompetitionLeasePgFixture(t, watchdog, "callback-statement-deadline")
		before := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		clock := &competitionLeaseRetryClock{now: fixture.clock}
		clock.Advance(time.Second)
		fixture.store.now = clock.Now
		advisoryClass, advisoryObject := int32(170129), int32(731021)
		applicationName := "synthetic-heartbeat-callback-latency"
		server.Tx(watchdog, func(tx server.PgTx) {
			// nextval is the private nontransactional completion witness: after
			// rollback it still proves the server proceeded beyond the held wait.
			server.RaisePgResult(tx.Exec(watchdog, `CREATE SEQUENCE synthetic_heartbeat_callback_after_wait;
			 CREATE FUNCTION synthetic_heartbeat_callback_latency() RETURNS trigger AS $function$
			 BEGIN
			  IF NEW.job_id::text<>TG_ARGV[0] THEN RETURN NEW; END IF;
			  IF NEW.lease_expires_at<=OLD.lease_expires_at OR NOT EXISTS(
			   SELECT 1 FROM competition_worker_slot slot WHERE slot.slot_id=1
			   AND slot.job_id=NEW.job_id AND slot.worker_id=NEW.lease_owner
			   AND slot.lease_expires_at=NEW.lease_expires_at AND slot.heartbeat_at>OLD.started_at) THEN
			   RAISE EXCEPTION 'synthetic callback did not observe the preceding slot extension';
			  END IF;
			  PERFORM set_config('lock_timeout','0',true);
			  PERFORM set_config('application_name',TG_ARGV[3],true);
			  PERFORM pg_advisory_xact_lock(TG_ARGV[1]::integer,TG_ARGV[2]::integer);
			  PERFORM nextval('synthetic_heartbeat_callback_after_wait'::regclass);
			  RETURN NEW;
			 END $function$ LANGUAGE plpgsql;`))
			server.RaisePgResult(tx.Exec(watchdog, fmt.Sprintf(`CREATE TRIGGER synthetic_heartbeat_callback_latency
			 BEFORE UPDATE OF lease_expires_at ON competition_job FOR EACH ROW
			 EXECUTE FUNCTION synthetic_heartbeat_callback_latency('%s','%d','%d','%s')`, job.JobId.String(), advisoryClass, advisoryObject, applicationName)))
		})
		held, release := make(chan int32, 1), make(chan struct{})
		holderResult, holderJoined := make(chan error, 1), make(chan struct{})
		var releaseOnce sync.Once
		releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
		go func() {
			holderResult <- captureDatabaseError(func() {
				server.MaintenanceTx(watchdog, func(tx server.PgTx) {
					var pid int32
					server.Raise(tx.QueryRow(watchdog, `SELECT pg_backend_pid()
					 FROM pg_advisory_xact_lock($1::integer,$2::integer)`, advisoryClass, advisoryObject).Scan(&pid))
					held <- pid
					select {
					case <-release:
					case <-watchdog.Done():
						panic(watchdog.Err())
					}
				}, server.OptNoRetry())
			})
			close(holderJoined)
		}()
		operationCtx, cancelOperation := context.WithCancel(watchdog)
		defer cancelOperation()
		heartbeatJoined := make(chan struct{})
		heartbeatStarted := false
		defer func() {
			cancelOperation()
			releaseHolder()
			// A failed main watchdog must not skip joining the database owner's
			// independently bounded statement-stop and rollback cleanup.
			cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(watchdog), 40*time.Second)
			defer cancelCleanup()
			select {
			case <-holderJoined:
			case <-cleanupCtx.Done():
				t.Error("synthetic advisory holder did not join cleanup")
			}
			if heartbeatStarted {
				select {
				case <-heartbeatJoined:
				case <-cleanupCtx.Done():
					t.Error("synthetic callback heartbeat did not join cleanup")
				}
			}
		}()
		var holderPid int32
		select {
		case holderPid = <-held:
		case err := <-holderResult:
			t.Fatal("synthetic advisory holder failed before acknowledgment", err)
		case <-watchdog.Done():
			t.Fatal("synthetic advisory holder did not acknowledge its exact lock")
		}
		// Capture the real helper-owned child, not a substituted error/context.
		attemptContexts := make(chan context.Context, 3)
		// One joined result owns the exact outer-call and backoff census.
		type heartbeatResult struct {
			startedAt    time.Time
			err          error
			calls, waits int
		}
		result := make(chan heartbeatResult, 1)
		settings := defaultCompetitionLeaseRetrySettings()
		if settings.Timeout != 8*time.Second {
			t.Fatal("mechanism fixture requires the unchanged default eight-second heartbeat owner")
		}
		settings.Now = clock.Now
		go func() {
			defer close(heartbeatJoined)
			calls, waits := 0, 0
			settings.Wait = func(context.Context, time.Duration) error {
				waits++
				return errors.New("synthetic callback deadline must not reach acquire/lock retry backoff")
			}
			startedAt, err := retryCompetitionLeaseOperation(operationCtx, *job.LeaseExpiresAt, settings, func(ctx context.Context) error {
				calls++
				attemptContexts <- ctx
				return fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId)
			})
			result <- heartbeatResult{startedAt: startedAt, err: err, calls: calls, waits: waits}
		}()
		heartbeatStarted = true
		var attemptCtx context.Context
		select {
		case attemptCtx = <-attemptContexts:
		case early := <-result:
			t.Fatal("heartbeat ended before its actual store call", early.err)
		case <-watchdog.Done():
			t.Fatal("heartbeat did not dispatch its owned context")
		}
		var waitingPid int32
		for waitingPid == 0 {
			var err error
			waitingPid, err = competitionHeartbeatCallbackWaitingBackend(watchdog, holderPid, applicationName)
			if err != nil {
				t.Fatal("could not observe the exact server-side callback wait", err)
			}
			if waitingPid != 0 {
				break
			}
			select {
			case early := <-result:
				t.Fatal("heartbeat ended before a positively observed backend wait", early.err)
			case <-watchdog.Done():
				t.Fatal("heartbeat did not reach the acknowledged advisory-lock holder")
			case <-time.After(10 * time.Millisecond):
			}
		}
		if attemptCtx.Err() != nil {
			t.Fatal("heartbeat owner expired before the exact server wait was acknowledged")
		}
		select {
		case <-attemptCtx.Done():
			if attemptCtx.Err() != context.DeadlineExceeded || watchdog.Err() != nil {
				t.Fatal("fixture did not exhaust only the real heartbeat's owned deadline")
			}
		case early := <-result:
			t.Fatal("held server statement ended before its owning deadline", early.err)
		case <-watchdog.Done():
			t.Fatal("owned heartbeat deadline did not expire within the watchdog")
		}
		// No sleep grants permission to proceed. The exact lock remains held
		// until after the owner is done; release immediately inside stop grace.
		releaseHolder()
		select {
		case err := <-holderResult:
			if err != nil {
				t.Fatal("synthetic advisory holder did not acknowledge its release", err)
			}
		case <-watchdog.Done():
			t.Fatal("synthetic advisory holder did not join before stop grace")
		}
		var completed heartbeatResult
		select {
		case completed = <-result:
		case <-watchdog.Done():
			t.Fatal("stop-bounded callback did not complete after its server lock was released")
		}
		expected := errors.Join(server.DbContextDoneError, context.DeadlineExceeded, context.DeadlineExceeded)
		expectedDigest := sha256.Sum256([]byte(expected.Error()))
		detail := competitionHeartbeatFailureDetails(completed.err)
		if completed.calls != 1 || completed.waits != 0 || !completed.startedAt.IsZero() || completed.err == nil || completed.err.Error() != expected.Error() ||
			detail.Category != "deadline_exceeded" || detail.DatabasePhase != server.DbOperationCallback || detail.SqlState != "" || detail.DetailSha256 != hex.EncodeToString(expectedDigest[:]) ||
			competitionSupervisorUnadmittedDeadline(completed.err) || competitionSupervisorLockTimeout(completed.err) {
			t.Fatal("late callback completion gained acquire retry or lost its exact deadline/phase evidence", completed, detail)
		}
		causes := server.InspectErrorCauseBatch(completed.err)
		deadlines, doneSentinels := 0, 0
		if !causes.Complete || causes.NilBranches != 0 {
			t.Fatal("callback deadline cause graph is incomplete")
		}
		for _, node := range causes.Nodes {
			if _, ok := node.Err.(*pgconn.PgError); ok {
				t.Fatal("synthetic stop completion unexpectedly returned PostgreSQL SQLSTATE")
			}
			if node.Leaf {
				switch node.Err {
				case context.DeadlineExceeded:
					deadlines++
				case server.DbContextDoneError:
					doneSentinels++
				default:
					t.Fatal("late callback completion gained an unrelated leaf", node.Err)
				}
			}
		}
		if deadlines != 2 || doneSentinels != 1 {
			t.Fatal("late callback completion changed its exact plain deadline leaves", deadlines, doneSentinels)
		}
		var witnessCalled bool
		var witnessValue int64
		server.MaintenanceDb(watchdog, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(watchdog, `SELECT last_value,is_called FROM synthetic_heartbeat_callback_after_wait`).Scan(&witnessValue, &witnessCalled))
		})
		if !witnessCalled || witnessValue != 1 {
			t.Fatal("server did not positively complete its trigger after the owning deadline")
		}
		if operationCtx.Err() != nil || watchdog.Err() != nil {
			t.Fatal("completed child deadline canceled its independently live parent")
		}
		if after := readCompetitionLeasePgSnapshot(watchdog, job.JobId); !reflect.DeepEqual(after, before) {
			t.Fatal("late callback completion partially renewed an authority or reset the original claim clock")
		}
	})
}
