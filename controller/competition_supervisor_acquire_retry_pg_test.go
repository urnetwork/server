// Deferred private-PostgreSQL tests authenticate real cumulative acquisition
// proof and unchanged fencing. Never run these against a production environment.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The existing pool is constrained only inside the disposable TestEnv owner.
// Cleanup joins holders before resetting/restoring either pool configuration.
func competitionSupervisorAcquirePgPool(t testing.TB) func() {
	t.Helper()
	pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
	server.PgReset()
	return func() { pop(); server.PgReset() }
}

// One acknowledged borrower occupies the sole main-pool slot until release.
func holdCompetitionSupervisorAcquirePgPool(t testing.TB, ctx context.Context) func() {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- captureDatabaseError(func() {
			server.Db(ctx, func(server.PgConn) {
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}, server.OptNoRetry())
		})
	}()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Errorf("synthetic sole-pool holder failed its joined cleanup: %v", err)
			}
		})
	}
	t.Cleanup(cleanup)
	select {
	case <-held:
	case <-ctx.Done():
		cleanup()
		t.Fatal("synthetic sole-pool holder did not acknowledge admission")
	}
	return cleanup
}

// Real pool refusal returns the exact production proof with no callback or
// transaction admission. Releasing the holder restores the same fenced renewal.
func TestCompetitionSupervisorAcquireRetryPgAuthenticatesUnadmittedRenewal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-acquire-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		release := holdCompetitionSupervisorAcquirePgPool(t, ctx)
		defer release()
		child, stop := context.WithTimeout(ctx, time.Second)
		result, err := fixture.store.RenewAutomationLease(child, fixture.settings, *lease, time.Minute)
		stop()
		if result != nil || !errors.Is(err, context.DeadlineExceeded) || !competitionSupervisorUnadmittedDeadline(err) {
			t.Fatal("real wholly unadmitted renewal did not authenticate its completed deadline", result, err)
		}
		proof := err.(competitionSupervisorRenewError)
		if proof.acquire.PoolEntry.Acquired != 1 || proof.acquire.PoolEntry.Maximum != 1 || proof.acquire.PoolReturn == nil || proof.read.AcquireSucceeded != 0 || proof.callbackEntered {
			t.Fatal("real pool/whole-invocation observations did not prove refusal", proof.acquire, proof.read)
		}
		release()
		var unchanged time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3`,
				lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&unchanged))
		})
		if !unchanged.Equal(lease.LeaseExpiresAt) {
			t.Fatal("unadmitted renewal changed durable authority")
		}
		fixture.now = fixture.now.Add(time.Second)
		next, err := fixture.store.RenewAutomationLease(ctx, fixture.settings, *lease, time.Minute)
		if err != nil || next == nil || next.OwnerId != lease.OwnerId || next.FencingToken != lease.FencingToken || !next.LeaseExpiresAt.After(lease.LeaseExpiresAt) {
			t.Fatal("released pool did not restore the original live fence", next, err)
		}
	})
}

// A real earlier admitted/committed transaction followed by a refused acquire
// retains cumulative history although its last observed phase becomes Acquire.
func TestCompetitionSupervisorAcquireRetryPgRejectsEarlierCommittedHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		observation := newCompetitionSupervisorRenewObservation()
		var release func()
		defer func() {
			if release != nil {
				release()
			}
		}()
		err := observation.capture(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				observation.callbackEntered = true
				server.RaisePgResult(tx.Exec(ctx, `SELECT 1`))
			}, &observation.phase, &observation.timing, observation.read)
			release = holdCompetitionSupervisorAcquirePgPool(t, ctx)
			child, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			server.Tx(child, func(server.PgTx) { panic("synthetic later acquire must not admit") },
				&observation.phase, &observation.timing, observation.read)
		})
		proof, ok := err.(competitionSupervisorRenewError)
		if !ok || !errors.Is(err, context.DeadlineExceeded) || proof.phase != server.DbOperationAcquire || proof.acquire.Admitted ||
			proof.read.AcquireSucceeded != 1 || !proof.callbackEntered || observation.timing.Phases[server.DbTimingCommit].Count != 1 || competitionSupervisorUnadmittedDeadline(err) {
			t.Fatal("final acquire erased real earlier commit/callback history", err, proof.read)
		}
	})
}

// A refused acquisition cannot authorize an old token after another owner
// takes over. The real renewal must reject and leave the new lease untouched.
func TestCompetitionSupervisorAcquireRetryPgPreservesTakeoverFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newCompetitionAutomationFixture(t)
		first, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-acquire-former", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		release := holdCompetitionSupervisorAcquirePgPool(t, ctx)
		defer release()
		child, stop := context.WithTimeout(ctx, time.Second)
		refused, refusal := fixture.store.RenewAutomationLease(child, fixture.settings, *first, time.Minute)
		stop()
		if refused != nil || !competitionSupervisorUnadmittedDeadline(refusal) {
			t.Fatal("synthetic prior acquisition was not positively refused", refusal)
		}
		release()
		fixture.now = first.LeaseExpiresAt
		next, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-acquire-successor", time.Minute)
		if err != nil || next == nil || next.FencingToken <= first.FencingToken {
			t.Fatal("synthetic takeover failed", err)
		}
		stale, err := fixture.store.RenewAutomationLease(ctx, fixture.settings, *first, time.Minute)
		if stale != nil || !errors.Is(err, ErrLeaseLost) || competitionSupervisorUnadmittedDeadline(err) {
			t.Fatal("old token acquired new recovery authority", stale, err)
		}
		var owner string
		var token int64
		var expiry time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT owner_id,fencing_token,lease_expires_at FROM competition_supervisor_lease
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, next.Environment, next.CompetitionId, next.SeasonId).Scan(&owner, &token, &expiry))
		})
		if owner != next.OwnerId || token != next.FencingToken || !expiry.Equal(next.LeaseExpiresAt) {
			t.Fatal("rejected stale renewal changed the successor's authority")
		}
	})
}
