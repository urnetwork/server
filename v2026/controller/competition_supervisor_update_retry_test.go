// Virtual deadlines and exact private facts exercise only the supervisor
// writer's new boundary; every ambiguous or incomplete neighbor stays closed.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// These synthetic values are not a retained production diagnostic or grant.
func competitionSupervisorUpdateFailure() competitionSupervisorRenewError {
	proof := competitionSupervisorReadLockFailure()
	proof.step = competitionSupervisorRenewalLeaseUpdate
	proof.readPrefix = competitionSupervisorReadPrefixSnapshot{valid: true, scopeIndependent: true, renewalWriter: true, count: 5, returned: 5, succeeded: 4}
	proof.transaction.LastRelease.PoolReturned = true
	return proof
}

// Removing the new predicate from the actual retry entry point makes the
// first deadline terminal. The same owner must get one healthy second call.
func TestCompetitionSupervisorUpdateRetryRecoversOwnedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, calls := time.Now(), 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return competitionSupervisorUpdateFailure()
			}
			return nil
		})
		if err != nil || calls != 2 || time.Since(started) != settings.AttemptTimeout+settings.LockRetry.Interval {
			t.Fatal("complete actual-writer proof did not preserve original recovery budget", calls, time.Since(started), err)
		}
	})
}

// The first incident's read-region failure has no acknowledged rollback and
// transfers destruction. Its remaining lease time cannot itself grant replay.
func TestCompetitionSupervisorUpdateRetryDoesNotReviveUnacknowledgedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, calls := time.Now(), 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(30*time.Second), settings, func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			time.Sleep(5 * time.Second)
			proof := competitionSupervisorReadLockFailure()
			proof.step = competitionSupervisorRenewalAuthority
			proof.readPrefix = competitionSupervisorReadPrefixSnapshot{valid: true, scopeIndependent: true, renewalWriter: true, count: 2, returned: 2, succeeded: 1}
			proof.transaction.RollbackAcknowledged = 0
			proof.transaction.LastRelease = server.DbTxReleaseObservation{Disposition: server.DbTxReleaseDestroyTransferred}
			return proof
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || time.Since(started) != 13*time.Second {
			t.Fatal("unacknowledged old read failure borrowed new writer authority", calls, time.Since(started), err)
		}
	})
}

// A returned physical pool receipt does not claim that its live socket closed.
func TestCompetitionSupervisorUpdateRetryDoesNotConfusePhysicalReturnAndCleanup(t *testing.T) {
	proof := competitionSupervisorUpdateFailure()
	if !competitionSupervisorRolledBackUpdateDeadline(proof) || proof.transaction.LastRelease.CleanupComplete ||
		competitionSupervisorReadLockDeadline(proof) || competitionSupervisorWorkerFenceDeadline(proof) {
		t.Fatal("writer proof required a false cleanup join or crossed the read-only classes")
	}
	proof.transaction.LastRelease.PoolReturned = false
	proof.transaction.LastRelease.CleanupComplete = true
	if competitionSupervisorRolledBackUpdateDeadline(proof) {
		t.Fatal("closed socket cleanup replaced actual idle pool return")
	}
}

// No scalar label can stand in for the complete admitted/returned SQL and
// custody history. The original no-ACK/destruction case is explicitly retained.
func TestCompetitionSupervisorUpdateRetryRejectsIncompletePhysicalOrSqlProof(t *testing.T) {
	for _, check := range []struct {
		name   string
		mutate func(*competitionSupervisorRenewError)
	}{
		{name: "unknown phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationUnknown }},
		{name: "no callback", mutate: func(p *competitionSupervisorRenewError) { p.callbackEntered = false }},
		{name: "extra callback", mutate: func(p *competitionSupervisorRenewError) { p.callbacks++ }},
		{name: "callback return", mutate: func(p *competitionSupervisorRenewError) { p.step = competitionSupervisorRenewalCallbackReturn }},
		{name: "no writer class", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.renewalWriter = false }},
		{name: "worker class", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.workerFence = true }},
		{name: "unexpected scope shape", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.scopeIndependent = false }},
		{name: "changed SQL", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.valid = false }},
		{name: "manual transaction", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.bypasses++ }},
		{name: "missing SQL", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.count-- }},
		{name: "extra SQL", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.count++; p.readPrefix.returned++ }},
		{name: "unreturned SQL", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.returned-- }},
		{name: "earlier SQL failed", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.succeeded-- }},
		{name: "no failed SQL", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.succeeded++ }},
		{name: "unknown acquire", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Returned = false }},
		{name: "no admission", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Admitted = false }},
		{name: "changed acquire census", mutate: func(p *competitionSupervisorRenewError) { p.transaction.AcquireAttempts++ }},
		{name: "earlier admission", mutate: func(p *competitionSupervisorRenewError) { p.transaction.AcquireSucceeded++ }},
		{name: "earlier begin", mutate: func(p *competitionSupervisorRenewError) { p.transaction.BeginAttempts++ }},
		{name: "unreturned begin", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.Returned = false }},
		{name: "extra transaction callback", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CallbackEntered++ }},
		{name: "possible commit", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAttempts++ }},
		{name: "commit acknowledged", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAcknowledged++ }},
		{name: "hidden commit boundary", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastCommit.Returned = true }},
		{name: "extra rollback", mutate: func(p *competitionSupervisorRenewError) { p.transaction.RollbackAttempts++ }},
		{name: "rollback unacknowledged", mutate: func(p *competitionSupervisorRenewError) { p.transaction.RollbackAcknowledged = 0 }},
		{name: "rollback not returned", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastRollback.Returned = false }},
		{name: "release not entered", mutate: func(p *competitionSupervisorRenewError) { p.transaction.ReleaseAttempts = 0 }},
		{name: "release not returned", mutate: func(p *competitionSupervisorRenewError) { p.transaction.ReleaseReturned = 0 }},
		{name: "pool return not proved", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastRelease.PoolReturned = false }},
		{name: "destruction transferred", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		}},
		{name: "unknown custody", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		}},
		{name: "canceled caller", mutate: func(p *competitionSupervisorRenewError) { p.returned.State = "canceled" }},
		{name: "live caller", mutate: func(p *competitionSupervisorRenewError) { p.returned.State = "live" }},
		{name: "nondeadline caller", mutate: func(p *competitionSupervisorRenewError) { p.returned.HasDeadline = false }},
		{name: "entry already stopped", mutate: func(p *competitionSupervisorRenewError) { p.entered.State = "deadline" }},
		{name: "late begin", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastBegin.AtReturn.State = "deadline" }},
		{name: "late rollback", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastRollback.AtReturn.State = "deadline" }},
	} {
		proof := competitionSupervisorUpdateFailure()
		check.mutate(&proof)
		if competitionSupervisorRolledBackUpdateDeadline(proof) {
			t.Errorf("%s gained writer continuation", check.name)
		}
	}
}

// Unknown leaves, arbitrary wrappers, pointer/branch projections and serialized
// diagnostic text never authenticate themselves as the whole store operation.
func TestCompetitionSupervisorUpdateRetryRejectsUnenclosedOrUnknownCause(t *testing.T) {
	proof := competitionSupervisorUpdateFailure()
	for _, cause := range []error{
		context.Canceled, ErrLeaseLost, errors.New("synthetic unknown"),
		errors.Join(context.DeadlineExceeded, context.Canceled),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		&os.SyscallError{Syscall: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorUnknownDeadline{cause: context.DeadlineExceeded},
		competitionSupervisorClaimedRetryDeadline{cause: context.DeadlineExceeded},
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		copy := proof
		copy.cause = competitionDatabasePhaseError{cause: cause, phase: server.DbOperationCallback}
		if competitionSupervisorRolledBackUpdateDeadline(copy) {
			t.Errorf("unknown cause %T authenticated UPDATE", cause)
		}
	}
	for _, cause := range []error{&proof, fmt.Errorf("synthetic outer: %w", proof), errors.Join(proof, context.DeadlineExceeded), errors.Join(proof, proof), errors.New(proof.Error())} {
		if competitionSupervisorRolledBackUpdateDeadline(cause) {
			t.Errorf("unenclosed projection %T authenticated UPDATE", cause)
		}
	}
}

// The actual helper's full fenced UPDATE must remain the last and only write.
func TestCompetitionSupervisorUpdatePrefixAuthenticatesActualWriter(t *testing.T) {
	observation := newCompetitionSupervisorRenewObservation()
	expiry := time.Now().Add(time.Minute)
	calls := 0
	original := competitionHeartbeatDiagnosticTx{
		exec: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			calls++
			if sql == competitionSupervisorLeaseRenewalUpdateSql {
				return pgconn.CommandTag{}, context.DeadlineExceeded
			}
			return pgconn.NewCommandTag("SELECT 1"), nil
		},
		queryRow: func(context.Context, string, ...any) pgx.Row {
			calls++
			return competitionHeartbeatDiagnosticRow(func(dest ...any) error {
				switch value := dest[0].(type) {
				case *time.Time:
					*value = expiry
				case *bool:
					*value = true
				default:
					return errors.New("synthetic unknown destination")
				}
				return nil
			})
		},
	}
	settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{
		Enabled: true, Environment: "synthetic", SeasonId: "synthetic-season",
	}}
	lease := model.CompetitionSupervisorLease{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId,
		SeasonId: settings.AutomationPolicy.SeasonId, OwnerId: "synthetic-owner", FencingToken: 7, LeaseExpiresAt: expiry}
	err := captureDatabaseError(func() {
		_ = (PostgresStore{}).renewAutomationLeaseInTx(t.Context(), observation.observeLeaseRenewalPrefix(original), settings, lease, time.Minute, observation)
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 5 || observation.step != competitionSupervisorRenewalLeaseUpdate ||
		!observation.readPrefix.valid || observation.readPrefix.count != 5 || observation.readPrefix.returned != 5 || observation.readPrefix.succeeded != 4 {
		t.Fatal("real renewal writer escaped the exact closed SQL surface", calls, observation.readPrefix, err)
	}
	proof := competitionSupervisorUpdateFailure()
	proof.readPrefix = observation.readPrefix
	if !competitionSupervisorRolledBackUpdateDeadline(proof) {
		t.Fatal("actual writer prefix was not recognized")
	}
}

// Mutation, suffixes, extra writes, and manual raw/nested methods invalidate
// the history before delegation; no business statement is silently discarded.
func TestCompetitionSupervisorUpdatePrefixRejectsChangedOrBypassedSurface(t *testing.T) {
	for _, changed := range []string{
		competitionSupervisorLeaseRenewalUpdateSql + "; COMMIT",
		strings.Replace(competitionSupervisorLeaseRenewalUpdateSql, "AND $5<lease_expires_at", "", 1),
		"UPDATE synthetic_other SET value=1",
	} {
		observation := newCompetitionSupervisorRenewObservation()
		observation.readPrefix.scopeIndependent = true
		original := &competitionHeartbeatSurfaceTx{}
		wrapped := observation.observeLeaseRenewalPrefix(original)
		prefix := competitionSupervisorObservedReadPrefix(observation.readPrefix)
		// Positive earlier statements are independent of this final changed call.
		observation.readPrefix.count = uint64(len(prefix) - 1)
		observation.step = competitionSupervisorRenewalLeaseUpdate
		_, _ = wrapped.Exec(t.Context(), changed)
		if observation.readPrefix.valid || original.calls != 1 {
			t.Fatal("changed writer SQL escaped observation/delegation")
		}
	}
	for _, invoke := range []func(pgx.Tx){
		func(tx pgx.Tx) { _, _ = tx.Begin(t.Context()) },
		func(tx pgx.Tx) { _ = tx.Commit(t.Context()) },
		func(tx pgx.Tx) { _ = tx.Rollback(t.Context()) },
		func(tx pgx.Tx) { _, _ = tx.Query(t.Context(), "synthetic query") },
		func(tx pgx.Tx) { _ = tx.SendBatch(t.Context(), &pgx.Batch{}) },
		func(tx pgx.Tx) { _, _ = tx.CopyFrom(t.Context(), nil, nil, nil) },
		func(tx pgx.Tx) { _, _ = tx.Prepare(t.Context(), "synthetic", "synthetic query") },
		func(tx pgx.Tx) { _ = tx.Conn() },
		func(tx pgx.Tx) { _ = tx.LargeObjects() },
	} {
		observation := newCompetitionSupervisorRenewObservation()
		original := &competitionHeartbeatSurfaceTx{}
		invoke(observation.observeLeaseRenewalPrefix(original))
		if observation.readPrefixBypass.unknown != 1 || original.calls != 1 {
			t.Fatal("manual transaction surface escaped writer proof")
		}
	}
}

// A more-demanding synthetic return delay still leaves the next entire eight
// seconds in the original twenty-second reserve, without moving lease expiry.
func TestCompetitionSupervisorUpdateRetryPreservesFullAttemptAndSafetyMargin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started := time.Now()
		expiry := started.Add(30 * time.Second)
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), expiry, settings, func(ctx context.Context) error {
			calls++
			if deadline, ok := ctx.Deadline(); !ok || deadline.Sub(time.Now()) != 8*time.Second {
				t.Fatal("fresh attempt lost full unchanged allowance")
			}
			if calls == 1 {
				<-ctx.Done()
				time.Sleep(200 * time.Millisecond)
				return competitionSupervisorUpdateFailure()
			}
			time.Sleep(8*time.Second - time.Nanosecond)
			return nil
		})
		if err != nil || calls != 2 || !time.Now().Before(expiry) || expiry.Sub(time.Now()) < 13*time.Second ||
			time.Since(started) >= settings.Timeout {
			t.Fatal("observed-shape retry consumed confirmed authority or enlarged budgets", calls, time.Since(started), err)
		}
	})
}

// Every new attempt consumes the same total/call ceiling; its final allowance
// is shortened by the original total, not by resetting the deadline reserve.
func TestCompetitionSupervisorUpdateRetryKeepsCumulativeCallAndTotalBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, calls := time.Now(), 0
		budgets := []time.Duration{}
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			deadline, _ := ctx.Deadline()
			budgets = append(budgets, deadline.Sub(time.Now()))
			<-ctx.Done()
			return competitionSupervisorUpdateFailure()
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 3 || len(budgets) != 3 || budgets[0] != 8*time.Second ||
			budgets[1] != 8*time.Second || budgets[2] != 3500*time.Millisecond || time.Since(started) != 20*time.Second {
			t.Fatal("writer continuation reset original call/total ceilings", calls, budgets, time.Since(started), err)
		}
	})
}

// The immediate predecessor must independently prove continuation. An earlier
// safe rollback cannot cover a later unknown failure or historical lock budget.
func TestCompetitionSupervisorUpdateRetryCannotReuseEarlierProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, nextError := range []error{errors.New("synthetic next failure"), &pgconn.PgError{Code: "55P03"}} {
			settings := defaultCompetitionSupervisorRenewalSettings()
			started, calls := time.Now(), 0
			err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
				calls++
				if calls == 1 {
					<-ctx.Done()
					return competitionSupervisorUpdateFailure()
				}
				return nextError
			})
			if err != nextError || calls != 2 {
				t.Fatal("earlier UPDATE proof leaked to a later failed call", calls, err)
			}
		}
	})
}

// Parent cancellation and exact original expiry precede every new dispatch.
func TestCompetitionSupervisorUpdateRetryPreservesCancellationAndExactExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, cancelParent := range []bool{false, true} {
			settings := defaultCompetitionSupervisorRenewalSettings()
			ctx, cancel := context.WithCancel(t.Context())
			expiry := time.Now().Add(8 * time.Second)
			if cancelParent {
				expiry = time.Now().Add(time.Minute)
			}
			calls := 0
			err := retryCompetitionSupervisorRenewal(ctx, expiry, settings, func(child context.Context) error {
				calls++
				<-child.Done()
				if cancelParent {
					cancel()
				}
				return competitionSupervisorUpdateFailure()
			})
			cancel()
			if calls != 1 || cancelParent && !errors.Is(err, context.Canceled) || !cancelParent && !errors.Is(err, ErrLeaseLost) {
				t.Fatal("writer proof overrode parent or expired authority", cancelParent, calls, err)
			}
		}
	})
}
