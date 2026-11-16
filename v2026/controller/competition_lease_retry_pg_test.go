// Deferred disposable-PostgreSQL regressions exercise actual lock-timeout
// rollback, fresh renewal clocks and atomic slot/job authority updates.
package controller

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Claim genuine synthetic work through existing archive/admission fixtures.
func newCompetitionLeasePgFixture(t testing.TB, ctx context.Context, name string) (*submissionArchiveFixture, *queuedJob) {
	t.Helper()
	fixture := newSubmissionArchiveFixture(t, ctx, "synthetic-lease-"+name)
	round := fixture.openStagingRound(t, ctx)
	if _, _, err := fixture.store.Enqueue(ctx, fixture.settings, round.RoundId, fixture.patch(t, "synthetic-lease"), "synthetic-miner", testApiImageDigest()); err != nil {
		t.Fatal(err)
	}
	job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-lease-worker", testWorkerImageDigest())
	if err != nil || job == nil {
		t.Fatalf("claim synthetic lease: %v", err)
	}
	return fixture, job
}

// Exact before/after values expose any partial extension or fresh claim budget.
type competitionLeasePgSnapshot struct {
	slotWorker                       *string
	slotJob                          *server.Id
	slotExpires, jobExpires, started *time.Time
	slotHeartbeat                    time.Time
	jobOwner                         *string
	attempts                         int
}

// One independent read observes both rows after the renewal transaction ends.
func readCompetitionLeasePgSnapshot(ctx context.Context, jobId server.Id) (snapshot competitionLeasePgSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT s.worker_id,s.job_id,s.lease_expires_at,s.heartbeat_at,
		 j.lease_owner,j.lease_expires_at,j.started_at,j.attempt_count
		 FROM competition_worker_slot s CROSS JOIN competition_job j WHERE s.slot_id=1 AND j.job_id=$1`, jobId).Scan(
			&snapshot.slotWorker, &snapshot.slotJob, &snapshot.slotExpires, &snapshot.slotHeartbeat,
			&snapshot.jobOwner, &snapshot.jobExpires, &snapshot.started, &snapshot.attempts))
	})
	return snapshot
}

// An independent transaction holds a real authority row until explicit release.
// No sleep or activity/queue-length polling determines the test's ordering.
func holdCompetitionLeasePgRow(t testing.TB, ctx context.Context, query string, arguments ...any) (func(), <-chan error) {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	releaseRow := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseRow)
	go func() {
		done <- captureDatabaseError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				var value int
				server.Raise(tx.QueryRow(ctx, query, arguments...).Scan(&value))
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}, server.OptNoRetry())
		})
		close(done)
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("authority row could not be held: %v", err)
	case <-ctx.Done():
		t.Fatal("authority row holder did not reach its barrier")
	}
	return releaseRow, done
}

// A suppressed second UPDATE must roll back the first slot UPDATE. Merely
// rejecting a missing row before any write would not exercise this root cause.
func TestCompetitionHeartbeatPgRollsBackSlotWhenJobUpdateIsSuppressed(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "suppressed-update")
		before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_suppress_lease_update() RETURNS trigger AS $body$
			 BEGIN RETURN NULL; END $body$ LANGUAGE plpgsql;
			 CREATE TRIGGER synthetic_suppress_lease_update BEFORE UPDATE OF lease_expires_at ON competition_job
			 FOR EACH ROW EXECUTE FUNCTION synthetic_suppress_lease_update();`))
		})
		fixture.clock = fixture.clock.Add(time.Second)
		if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("suppressed job update did not lose authority: %v", err)
		}
		if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, before) {
			t.Fatal("failed second update committed a partial slot extension or changed the claim budget")
		}
	})
}

// Both initial authorities must be present, owned and live, including their
// exact expiry boundary; rejection must leave both rows wholly unchanged.
func TestCompetitionHeartbeatPgRejectsMissingAndExpiredAuthority(t *testing.T) {
	for _, check := range []struct {
		name  string
		query string
	}{
		{name: "missing-slot-owner", query: `UPDATE competition_worker_slot SET worker_id='synthetic-other-worker' WHERE slot_id=1`},
		{name: "missing-job-owner", query: `UPDATE competition_job SET lease_owner='synthetic-other-worker' WHERE job_id=$1`},
		{name: "expired-slot", query: `UPDATE competition_worker_slot SET lease_expires_at=$1 WHERE slot_id=1`},
		{name: "expired-job", query: `UPDATE competition_job SET lease_expires_at=$2 WHERE job_id=$1`},
		{name: "null-slot", query: `UPDATE competition_worker_slot SET lease_expires_at=NULL WHERE slot_id=1`},
		{name: "null-job", query: `UPDATE competition_job SET lease_expires_at=NULL WHERE job_id=$1`},
	} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx := context.Background()
			fixture, job := newCompetitionLeasePgFixture(t, ctx, check.name)
			server.Db(ctx, func(conn server.PgConn) {
				// Synthetic SQL statements use only their named positional inputs.
				arguments := []any{}
				switch check.name {
				case "missing-job-owner", "null-job":
					arguments = []any{job.JobId}
				case "expired-slot":
					arguments = []any{fixture.clock}
				case "expired-job":
					arguments = []any{job.JobId, fixture.clock}
				}
				server.RaisePgResult(conn.Exec(ctx, check.query, arguments...))
			}, server.OptReadWrite())
			before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
			if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("%s renewed invalid authority: %v", check.name, err)
			}
			if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s partially changed authority", check.name)
			}
		})
	}
}

// The admitted boundary occurs before a real row lock held by another owner.
// Advancing the clock there proves it is sampled after that blocked boundary;
// this test does not claim to have observed a backend in pg_stat_activity.
func TestCompetitionHeartbeatPgSamplesClockAfterBothAuthorityLocks(t *testing.T) {
	for _, phase := range []string{"worker_slot", "job"} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fixture, job := newCompetitionLeasePgFixture(t, ctx, "held-"+phase)
			clock := &competitionLeaseRetryClock{now: fixture.clock}
			fixture.store.now = clock.Now
			query, arguments := `SELECT 1 FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`, []any{}
			if phase == "job" {
				query, arguments = `SELECT 1 FROM competition_job WHERE job_id=$1 FOR UPDATE`, []any{job.JobId}
			}
			releaseRow, holderDone := holdCompetitionLeasePgRow(t, ctx, query, arguments...)
			admitted, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			resumeRenewal := func() { resumeOnce.Do(func() { close(resume) }) }
			t.Cleanup(resumeRenewal)
			fixture.store.beforeLeaseRowLockForTest = func(actual string) {
				if actual == phase {
					close(admitted)
					select {
					case <-resume:
					case <-ctx.Done():
						panic(ctx.Err())
					}
				}
			}
			before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
			done := make(chan error, 1)
			go func() { done <- fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId) }()
			select {
			case <-admitted:
			case <-ctx.Done():
				t.Fatal("heartbeat did not admit the held row-lock boundary")
			}
			clock.Advance(time.Duration(fixture.settings.WorkerLeaseSeconds) * time.Second)
			releaseRow()
			if err := <-holderDone; err != nil {
				t.Fatal(err)
			}
			resumeRenewal()
			if err := <-done; !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("heartbeat revived authority across held %s lock: %v", phase, err)
			}
			if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, before) {
				t.Fatal("expiry after a held row lock changed lease or claim state")
			}
		})
	}
}

// The actual first transaction must return 55P03 while the controlled slot row
// remains held. Only the retry wait releases it, after transaction rollback.
func TestCompetitionHeartbeatPgLockTimeoutRetriesAfterRollback(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "real-lock-timeout")
		clock := &competitionLeaseRetryClock{now: fixture.clock}
		fixture.store.now = clock.Now
		releaseRow, holderDone := holdCompetitionLeasePgRow(t, ctx, `SELECT 1 FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`)
		settings := defaultCompetitionLeaseRetrySettings()
		settings.Timeout = time.Minute
		settings.Now = clock.Now
		settings.Wait = func(_ context.Context, delay time.Duration) error {
			releaseRow()
			if err := <-holderDone; err != nil {
				return err
			}
			clock.Advance(delay)
			return nil
		}
		calls := 0
		_, err := retryCompetitionLeaseOperation(ctx, *job.LeaseExpiresAt, settings, func(ctx context.Context) error {
			calls++
			err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId)
			if calls == 1 && !server.IsLockTimeout(err) {
				t.Fatalf("controlled held slot did not return PostgreSQL lock timeout: %v", err)
			}
			return err
		})
		if err != nil || calls != 2 {
			t.Fatalf("rolled-back lock timeout did not recover exactly once: calls=%d, %v", calls, err)
		}
		after := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		wantExpiry := clock.Now().Add(time.Duration(fixture.settings.WorkerLeaseSeconds) * time.Second)
		if after.slotExpires == nil || after.jobExpires == nil || !after.slotExpires.Equal(wantExpiry) || !after.jobExpires.Equal(wantExpiry) ||
			after.started == nil || !after.started.Equal(*job.StartedAt) || after.attempts != job.AttemptCount {
			t.Fatal("successful heartbeat changed the original claim or failed to renew both authorities")
		}
	})
}

// A held supervisor row cannot let a pre-wait timestamp revive an expired lease.
func TestCompetitionSupervisorRenewPgSamplesClockAfterAuthorityLock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		clock := &competitionLeaseRetryClock{now: fixture.now}
		fixture.store.now = clock.Now
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-held-supervisor", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		releaseRow, holderDone := holdCompetitionLeasePgRow(t, ctx, `SELECT 1 FROM competition_supervisor_lease
		 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 FOR UPDATE`, lease.Environment, lease.CompetitionId, lease.SeasonId)
		admitted, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		resumeRenewal := func() { resumeOnce.Do(func() { close(resume) }) }
		t.Cleanup(resumeRenewal)
		fixture.store.beforeLeaseRowLockForTest = func(phase string) {
			if phase == "supervisor" {
				close(admitted)
				select {
				case <-resume:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}
		}
		done := make(chan error, 1)
		go func() {
			result, err := fixture.store.RenewAutomationLease(ctx, fixture.settings, *lease, time.Minute)
			if err == nil && result == nil {
				err = errors.New("synthetic renewal lost its result")
			}
			done <- err
		}()
		select {
		case <-admitted:
		case <-ctx.Done():
			t.Fatal("supervisor did not admit its held authority-row boundary")
		}
		clock.Advance(time.Minute)
		releaseRow()
		if err := <-holderDone; err != nil {
			t.Fatal(err)
		}
		resumeRenewal()
		if err := <-done; !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("supervisor renewed expired held-row authority: %v", err)
		}
		var expires time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&expires))
		})
		if !expires.Equal(lease.LeaseExpiresAt) {
			t.Fatal("failed supervisor renewal changed its expired authority")
		}
	})
}
