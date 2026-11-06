// Disposable PostgreSQL authenticates the exact step-three authority prefix,
// acknowledged rollback and later same-fence renewal. No live pool is touched.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The real transaction reaches Authority while its caller still lives, then
// waits for that exact owned deadline before the real EXISTS scan refuses.
// A healthy second call must renew the original owner, never reacquire it.
func TestCompetitionSupervisorReadLockRetryPgAuthenticatesAuthorityPrefix(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-prefix-supervisor", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		readExpiry := func() time.Time {
			var expiry time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease
				 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&expiry))
			})
			return expiry
		}
		policy := defaultCompetitionSupervisorRenewalSettings()
		policy.AttemptTimeout = time.Second
		policy.LockRetry.Timeout = time.Second
		policy.LockRetry.Now = func() time.Time { return fixture.now }
		var attempt context.Context
		calls, waits, authorityEntries := 0, 0, 0
		fixture.store.beforeAutomationLeaseAuthorityForTest = func() {
			authorityEntries++
			if authorityEntries == 1 {
				if attempt.Err() != nil {
					panic(errors.New("synthetic authority barrier was entered without a live original caller"))
				}
				<-attempt.Done()
			}
		}
		var firstError error
		policy.LockRetry.Wait = func(wait context.Context, _ time.Duration) error {
			waits++
			if wait.Err() != nil || !competitionSupervisorReadLockDeadline(firstError) || !readExpiry().Equal(lease.LeaseExpiresAt) {
				return errors.New("closed real authority prefix changed durable expiry or lost its live recovery owner")
			}
			fixture.now = fixture.now.Add(time.Second)
			return nil
		}
		var next *model.CompetitionSupervisorLease
		err = retryCompetitionSupervisorRenewal(ctx, lease.LeaseExpiresAt, policy, func(child context.Context) error {
			calls++
			attempt = child
			var err error
			next, err = fixture.store.RenewAutomationLease(child, fixture.settings, *lease, time.Minute)
			if calls == 1 {
				firstError = err
				if content, marshalError := json.Marshal(CompetitionSupervisorRenewalFailureDetails(err)); marshalError == nil {
					t.Logf("synthetic_actual_prefix_observation=%s", content)
				}
				causes := server.InspectErrorCauseBatch(err)
				for _, node := range causes.Nodes {
					t.Logf("synthetic_actual_prefix_cause_type=%T leaf=%t", node.Err, node.Leaf)
				}
			}
			return err
		})
		proof, ok := firstError.(competitionSupervisorRenewError)
		if err != nil || calls != 2 || waits != 1 || authorityEntries != 2 || !ok || proof.step != competitionSupervisorRenewalAuthority ||
			proof.readPrefix.count != 3 || !proof.readPrefix.valid || proof.transaction.CommitAttempts != 0 || proof.transaction.RollbackAcknowledged != 1 ||
			proof.transaction.ReleaseReturned != 1 || proof.transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
			next == nil || next.OwnerId != lease.OwnerId || next.FencingToken != lease.FencingToken || !next.LeaseExpiresAt.After(lease.LeaseExpiresAt) || !readExpiry().Equal(next.LeaseExpiresAt) {
			t.Fatal("real authority-prefix rollback did not recover the original fence", calls, waits, authorityEntries, proof.readPrefix, err)
		}
	})
}

// A successful worker read learns only the parent's already-renewed database
// expiry, never writes a new one, and refuses the same token after expiry.
func TestCompetitionSupervisorWorkerFencePgObservesExpiryWithoutRenewing(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-read-only-parent", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fixture.now = fixture.now.Add(time.Second)
		next, err := fixture.store.RenewAutomationLease(ctx, fixture.settings, *lease, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		observed, err := readCompetitionSupervisorWorkerFence(ctx, fixture.store, fixture.settings, *lease)
		if err != nil || !observed.Equal(next.LeaseExpiresAt) || !observed.After(lease.LeaseExpiresAt) {
			t.Fatal("worker reader retained the request's stale expiry", observed, err)
		}
		var durable time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&durable))
		})
		if !durable.Equal(next.LeaseExpiresAt) {
			t.Fatal("worker reader extended authority on behalf of its parent")
		}
		fixture.now = next.LeaseExpiresAt
		observed, err = readCompetitionSupervisorWorkerFence(ctx, fixture.store, fixture.settings, *lease)
		if !observed.IsZero() || !errors.Is(err, ErrLeaseLost) || competitionSupervisorWorkerFenceDeadline(err) || competitionLivenessPoolerRefusal(err) {
			t.Fatal("explicit expired authority became retryable or renewed", observed, err)
		}
	})
}

// A separate transaction holds the exact scope advisory lock. Positive
// blocking-pid evidence precedes the owned deadline; releasing only after the
// actual stop witness produces a real failed step-one read-only prefix.
func TestCompetitionSupervisorReadLockRetryPgAuthenticatesScopePrefix(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		watchdog, stop := context.WithTimeout(t.Context(), 35*time.Second)
		defer stop()
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(watchdog, fixture.settings, "synthetic-scope-prefix-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		held, release := make(chan int32, 1), make(chan struct{})
		holderDone, holderJoined := make(chan error, 1), make(chan struct{})
		var holderOnce sync.Once
		unlock := func() { holderOnce.Do(func() { close(release) }) }
		go func() {
			defer close(holderJoined)
			holderDone <- captureDatabaseError(func() {
				server.MaintenanceTx(watchdog, func(tx server.PgTx) {
					automationScopeLockInTx(watchdog, tx, fixture.settings)
					var pid int32
					server.Raise(tx.QueryRow(watchdog, `SELECT pg_backend_pid()`).Scan(&pid))
					held <- pid
					select {
					case <-release:
					case <-watchdog.Done():
						panic(watchdog.Err())
					}
				}, server.OptNoRetry())
			})
		}()
		operation, cancelOperation := context.WithCancel(watchdog)
		attempts, stopped := make(chan context.Context, 1), make(chan struct{})
		proofReady, allowRetry := make(chan error, 1), make(chan struct{})
		var retryOnce sync.Once
		releaseRetry := func() { retryOnce.Do(func() { close(allowRetry) }) }
		type result struct {
			lease *model.CompetitionSupervisorLease
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
					t.Error("scope-prefix renewal did not join")
				}
			}
			select {
			case <-holderJoined:
			case <-cleanup.Done():
				t.Error("scope advisory holder did not join")
			}
		}()
		var holderPid int32
		select {
		case holderPid = <-held:
		case err := <-holderDone:
			t.Fatal("scope holder failed", err)
		case <-watchdog.Done():
			t.Fatal("scope holder did not acknowledge")
		}
		policy := defaultCompetitionSupervisorRenewalSettings()
		policy.AttemptTimeout = 1500 * time.Millisecond
		policy.LockRetry.Now = func() time.Time { return fixture.now }
		var firstError error
		policy.LockRetry.Wait = func(ctx context.Context, _ time.Duration) error {
			proofReady <- firstError
			select {
			case <-allowRetry:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		go func() {
			defer close(joined)
			calls := 0
			var next *model.CompetitionSupervisorLease
			err := retryCompetitionSupervisorRenewal(operation, lease.LeaseExpiresAt, policy, func(ctx context.Context) error {
				calls++
				actual := ctx
				if calls == 1 {
					actual = &competitionHeartbeatPgDeadlineContext{Context: ctx, stopped: stopped}
					attempts <- actual
				}
				var err error
				next, err = fixture.store.RenewAutomationLease(actual, fixture.settings, *lease, time.Minute)
				if calls == 1 {
					firstError = err
				}
				return err
			})
			done <- result{lease: next, err: err, calls: calls}
		}()
		started = true
		var attempt context.Context
		select {
		case attempt = <-attempts:
		case returned := <-done:
			t.Fatal("scope renewal ended before dispatch", returned.err)
		case <-watchdog.Done():
			t.Fatal("scope renewal did not dispatch")
		}
		waiting := false
		for !waiting {
			err := captureDatabaseError(func() {
				server.MaintenanceDb(watchdog, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(watchdog, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a
					 WHERE a.datname=current_database() AND a.wait_event_type='Lock' AND $1::integer=ANY(pg_blocking_pids(a.pid)))`, holderPid).Scan(&waiting))
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			if !waiting {
				select {
				case returned := <-done:
					t.Fatal("scope renewal ended before its positive wait", returned.err)
				case <-watchdog.Done():
					t.Fatal("scope wait was not observed")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if attempt.Err() != nil {
			t.Fatal("actual scope wait was not observed before the owned deadline")
		}
		select {
		case <-stopped:
		case <-watchdog.Done():
			t.Fatal("scope statement did not acknowledge its caller's stop")
		}
		unlock()
		select {
		case err := <-holderDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-watchdog.Done():
			t.Fatal("scope holder did not release")
		}
		var failed error
		select {
		case failed = <-proofReady:
		case returned := <-done:
			t.Fatal("scope prefix did not reach recovery", returned.err)
		case <-watchdog.Done():
			t.Fatal("scope rollback did not return")
		}
		proof, ok := failed.(competitionSupervisorRenewError)
		if !ok || !competitionSupervisorReadLockDeadline(failed) || proof.step != competitionSupervisorRenewalScopeLock || proof.readPrefix.count != 1 ||
			proof.transaction.CommitAttempts != 0 || proof.transaction.RollbackAcknowledged != 1 || proof.transaction.ReleaseReturned != 1 || proof.transaction.LastRelease.Disposition != server.DbTxReleaseIdle {
			t.Fatal("actual scope failure lost its closed read-only prefix", CompetitionSupervisorRenewalFailureDetails(failed))
		}
		var before time.Time
		server.Db(watchdog, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(watchdog, `SELECT lease_expires_at FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&before))
		})
		if !before.Equal(lease.LeaseExpiresAt) {
			t.Fatal("failed scope prefix durably changed authority")
		}
		fixture.now = fixture.now.Add(time.Second)
		releaseRetry()
		returned := <-done
		if returned.err != nil || returned.calls != 2 || returned.lease == nil || returned.lease.OwnerId != lease.OwnerId || returned.lease.FencingToken != lease.FencingToken || !returned.lease.LeaseExpiresAt.After(lease.LeaseExpiresAt) {
			t.Fatal("scope-prefix retry did not preserve its original fence", returned.calls, returned.err)
		}
	})
}
