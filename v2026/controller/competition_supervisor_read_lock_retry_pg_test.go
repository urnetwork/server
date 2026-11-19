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

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
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
			proof.readPrefix.count != 2 || !proof.readPrefix.valid || proof.transaction.CommitAttempts != 0 || proof.transaction.RollbackAcknowledged != 1 ||
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

// The source holder stays live until the actual independent renewal returns.
func TestCompetitionSupervisorRenewalPgProgressesWithSourceScopeHeld(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-independent-renew-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		var releaseOnce sync.Once
		unlock := func() { releaseOnce.Do(func() { close(release) }) }
		defer func() {
			unlock()
			if err := <-done; err != nil {
				t.Error("source holder failed", err)
			}
		}()
		go func() {
			done <- captureDatabaseError(func() {
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					automationScopeLockInTx(ctx, tx, fixture.settings)
					close(held)
					select {
					case <-release:
					case <-ctx.Done():
						panic(ctx.Err())
					}
				}, server.OptNoRetry())
			})
		}()
		select {
		case <-held:
		case <-ctx.Done():
			t.Fatal("source scope was not acquired", ctx.Err())
		}
		fixture.now = fixture.now.Add(time.Second)
		next, err := fixture.store.RenewAutomationLease(ctx, fixture.settings, *lease, time.Minute)
		if err != nil || next == nil || next.FencingToken != lease.FencingToken || next.OwnerId != lease.OwnerId || !next.LeaseExpiresAt.After(lease.LeaseExpiresAt) {
			t.Fatal("held source scope blocked the unchanged renewal fence", next, err)
		}
	})
}
