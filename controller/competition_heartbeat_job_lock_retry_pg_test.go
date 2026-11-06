// Disposable PostgreSQL proves the actual job-lock prefix and rollback path.
// A shorter test-owned call crosses its real deadline before the unchanged
// two-second lock timeout; this is not a claim about incident/network latency.
package controller

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The independent holder owns only the exact job row. Positive server-side
// blocking-pid evidence determines ordering; the timer only limits polling.
func competitionHeartbeatJobLockWaitingBackend(ctx context.Context, holderPid int32) (int32, error) {
	var waitingPid int32
	err := captureDatabaseError(func() {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT a.pid FROM pg_stat_activity a
			 WHERE a.datname=current_database() AND a.wait_event_type='Lock'
			 AND a.wait_event IN ('transactionid','tuple') AND $1::integer=ANY(pg_blocking_pids(a.pid))
			 ORDER BY a.pid LIMIT 1),0)`, holderPid).Scan(&waitingPid))
		})
	})
	return waitingPid, err
}

// Both modes authenticate a real read-only failure first. The second then
// changes only supervisor authority before allowing the ordinary successor.
func testCompetitionHeartbeatJobLockPgContinuation(t *testing.T, replaceFence bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 35*time.Second)
		defer stopWatchdog()
		server.Db(watchdog, func(conn server.PgConn) {
			var head int
			server.Raise(conn.QueryRow(watchdog, `SELECT COALESCE(MAX(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&head))
			if head != server.MigrationCount() {
				t.Fatal("actual job-lock fixture schema did not reach source head", head, server.MigrationCount())
			}
			t.Logf("actual_job_lock_fixture_schema_head=%d", head)
		})
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		fixture, job := newCompetitionHeartbeatSlotPgFixture(t, watchdog)
		before := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		fixture.now = fixture.now.Add(time.Second)
		held, releaseHolder := make(chan int32, 1), make(chan struct{})
		holderDone, holderJoined := make(chan error, 1), make(chan struct{})
		var holderOnce sync.Once
		unlock := func() { holderOnce.Do(func() { close(releaseHolder) }) }
		go func() {
			defer close(holderJoined)
			holderDone <- captureDatabaseError(func() {
				server.MaintenanceTx(watchdog, func(tx server.PgTx) {
					var pid int32
					server.Raise(tx.QueryRow(watchdog, `SELECT pg_backend_pid() FROM competition_job WHERE job_id=$1 FOR UPDATE`, job.JobId).Scan(&pid))
					held <- pid
					select {
					case <-releaseHolder:
					case <-watchdog.Done():
						panic(watchdog.Err())
					}
				}, server.OptNoRetry())
			})
		}()
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
		done, joined := make(chan result, 1), make(chan struct{})
		started := false
		defer func() {
			cancelOperation()
			unlock()
			releaseRetry()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(watchdog), 40*time.Second)
			defer cancel()
			if started {
				select {
				case <-joined:
				case <-cleanup.Done():
					t.Error("job-lock heartbeat did not join its owned statement cleanup")
				}
			}
			select {
			case <-holderJoined:
			case <-cleanup.Done():
				t.Error("exact job-row holder did not join")
			}
		}()
		var holderPid int32
		select {
		case holderPid = <-held:
		case err := <-holderDone:
			t.Fatal("exact job-row holder failed before acknowledgement", err)
		case <-watchdog.Done():
			t.Fatal("exact job-row holder did not acknowledge its lock")
		}
		settings := defaultCompetitionHeartbeatSettings()
		// Shorten only this mechanism test; production keeps eight seconds.
		settings.AcquireRetry.AttemptTimeout = 1500 * time.Millisecond
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
		started = true
		var attempt context.Context
		select {
		case attempt = <-attempts:
		case returned := <-done:
			t.Fatal("job-lock heartbeat ended before dispatch", returned.err)
		case <-watchdog.Done():
			t.Fatal("job-lock heartbeat did not dispatch")
		}
		waitingPid := int32(0)
		for waitingPid == 0 {
			var err error
			waitingPid, err = competitionHeartbeatJobLockWaitingBackend(watchdog, holderPid)
			if err != nil {
				t.Fatal(err)
			}
			if waitingPid == 0 {
				select {
				case returned := <-done:
					t.Fatal("job-lock heartbeat returned before its positive row wait", returned.err)
				case <-watchdog.Done():
					t.Fatal("job-lock heartbeat did not reach its exact row holder")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if attempt.Err() != nil {
			t.Fatal("test did not observe its actual row wait before the owned deadline")
		}
		select {
		case <-attempt.Done():
		case <-watchdog.Done():
			t.Fatal("test-owned job-lock deadline did not expire")
		}
		select {
		case <-deadlineObserved:
		case <-watchdog.Done():
			t.Fatal("actual statement stop did not acknowledge its deadline")
		}
		unlock()
		select {
		case err := <-holderDone:
			if err != nil {
				t.Fatal("exact row holder failed to acknowledge release", err)
			}
		case <-watchdog.Done():
			t.Fatal("exact row holder did not return inside statement-stop grace")
		}
		var failed error
		select {
		case failed = <-proofReady:
		case returned := <-done:
			t.Fatal("actual read-only prefix did not authorize its bounded continuation", returned.err)
		case <-watchdog.Done():
			t.Fatal("job-lock rollback/release did not return")
		}
		proof, ok := failed.(competitionHeartbeatDatabaseError)
		if !ok || !competitionHeartbeatJobLockDeadline(failed) || competitionHeartbeatSlotUpdateDeadline(failed) ||
			proof.snapshot.Transaction.CommitAttempts != 0 || proof.snapshot.Transaction.RollbackAcknowledged != 1 ||
			proof.snapshot.Transaction.ReleaseReturned != 1 || proof.snapshot.Transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
			proof.snapshot.SqlStatements != competitionHeartbeatJobLockPrefixLength {
			t.Fatal("actual row wait did not produce exact acknowledged read-only-prefix evidence", competitionHeartbeatFailureDetails(failed))
		}
		if after := readCompetitionLeasePgSnapshot(watchdog, job.JobId); !reflect.DeepEqual(before, after) {
			t.Fatal("read-only failure changed the committed lease/attempt/start", before, after)
		}
		if replaceFence {
			previous := *fixture.settings.automationLease
			futureStore := fixture.store
			futureStore.now = func() time.Time { return previous.LeaseExpiresAt }
			if _, err := futureStore.AcquireAutomationLease(watchdog, fixture.settings, "synthetic-replacement-prefix-supervisor", 5*time.Minute); err != nil {
				t.Fatal("synthetic replacement could not fence the next heartbeat", err)
			}
		}
		releaseRetry()
		select {
		case returned := <-done:
			if returned.calls != 2 {
				t.Fatal("prefix continuation changed its exact outer call census", returned.calls, returned.err)
			}
			if replaceFence {
				if !errors.Is(returned.err, ErrLeaseLost) || !returned.ack.IsZero() {
					t.Fatal("successor borrowed stale supervisor authority", returned.err)
				}
			} else if returned.err != nil || returned.ack.IsZero() {
				t.Fatal("same-claim prefix continuation did not finish", returned.err)
			}
		case <-watchdog.Done():
			t.Fatal("bounded job-lock successor did not join")
		}
		after := readCompetitionLeasePgSnapshot(watchdog, job.JobId)
		if replaceFence {
			if !reflect.DeepEqual(before, after) {
				t.Fatal("stale-fence successor changed committed job/slot authority", before, after)
			}
		} else if after.attempts != before.attempts || after.started == nil || before.started == nil || !after.started.Equal(*before.started) ||
			after.jobOwner == nil || before.jobOwner == nil || *after.jobOwner != *before.jobOwner ||
			after.slotWorker == nil || before.slotWorker == nil || *after.slotWorker != *before.slotWorker ||
			after.slotJob == nil || *after.slotJob != job.JobId || after.jobExpires == nil || before.jobExpires == nil || !after.jobExpires.After(*before.jobExpires) ||
			after.slotExpires == nil || !after.slotExpires.Equal(*after.jobExpires) {
			t.Fatal("continuation changed the original claim or lost atomic lease extension", before, after)
		}
	})
}

// A real read-only prefix returns its original locks/connection before retry.
func TestCompetitionHeartbeatJobLockPgAcknowledgedRollbackPreservesClaim(t *testing.T) {
	testCompetitionHeartbeatJobLockPgContinuation(t, false)
}

// Retrying the read does not bypass the ordinary supervisor fence afterward.
func TestCompetitionHeartbeatJobLockPgContinuationRechecksSupervisorFence(t *testing.T) {
	testCompetitionHeartbeatJobLockPgContinuation(t, true)
}
