// Deterministic virtual deadlines force renewal's read-only prefix to unwind
// before a new attempt; negative controls retain every authority/custody bound.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Synthetic invocation facts are authenticated separately by disposable PG.
func competitionSupervisorReadLockFailure() competitionSupervisorRenewError {
	live := server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)}
	boundary := server.DbTxBoundaryObservation{Before: live, AtReturn: live, Returned: true}
	return competitionSupervisorRenewError{
		cause: competitionDatabasePhaseError{cause: errors.Join(server.DbContextDoneError, context.DeadlineExceeded), phase: server.DbOperationCallback},
		phase: server.DbOperationCallback, callbackEntered: true, callbacks: 1, step: competitionSupervisorRenewalLeaseRowLock,
		readPrefix: competitionSupervisorReadPrefixSnapshot{valid: true, count: 2, returned: 2, succeeded: 1},
		entered:    live, returned: server.DbContextBudget{State: "deadline", HasDeadline: true},
		acquire: server.DbAcquireObservation{Attempt: 1, Admitted: true, Returned: true, BeforeAcquire: live, AtReturn: live},
		transaction: server.DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1, CallbackEntered: 1,
			RollbackAttempts: 1, RollbackAcknowledged: 1, ReleaseAttempts: 1, ReleaseReturned: 1, LastBegin: boundary, LastRollback: boundary,
			LastRelease: server.DbTxReleaseObservation{Disposition: server.DbTxReleaseIdle}},
	}
}

// Removing only the new prefix predicate makes the first completed deadline
// terminal; a healthy second call must preserve the original operation owner.
func TestCompetitionSupervisorReadLockRetryRecoversOwnedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, calls := time.Now(), 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return competitionSupervisorReadLockFailure()
			}
			return nil
		})
		if err != nil || calls != 2 || time.Since(started) != settings.AttemptTimeout+settings.LockRetry.Interval {
			t.Fatal("acknowledged read-lock rollback did not preserve live renewal", calls, time.Since(started), err)
		}
	})
}

// Earlier refused acquisitions consume the same call budget but did no
// transaction work. Only the single admitted callback may own this prefix.
func TestCompetitionSupervisorReadLockRetryKeepsEarlierUnadmittedAcquisitions(t *testing.T) {
	for _, step := range []competitionSupervisorRenewalStep{competitionSupervisorRenewalScopeLock, competitionSupervisorRenewalLeaseRowLock, competitionSupervisorRenewalAuthority} {
		proof := competitionSupervisorReadLockFailure()
		proof.acquire.Attempt, proof.transaction.AcquireAttempts = 2, 2
		proof.step = step
		proof.readPrefix.count, proof.readPrefix.returned, proof.readPrefix.succeeded = uint64(step), uint64(step), uint64(step)-1
		if !competitionSupervisorReadLockDeadline(proof) {
			t.Fatal("earlier refused acquisition erased a closed read-only prefix", step)
		}
		proof.transaction.AcquireSucceeded = 2
		if competitionSupervisorReadLockDeadline(proof) {
			t.Fatal("earlier admission borrowed the last callback's prefix", step)
		}
	}
}

// Unknown admission, later work, an unacknowledged rollback, and transferred
// destruction cannot be mistaken for the closed idle read-only prefix.
func TestCompetitionSupervisorReadLockRetryRejectsIncompleteOrMutatingHistory(t *testing.T) {
	for _, check := range []struct {
		name   string
		mutate func(*competitionSupervisorRenewError)
	}{
		{name: "unknown phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationUnknown }},
		{name: "no callback", mutate: func(p *competitionSupervisorRenewError) { p.callbackEntered = false }},
		{name: "two callbacks", mutate: func(p *competitionSupervisorRenewError) { p.callbacks = 2 }},
		{name: "lease update", mutate: func(p *competitionSupervisorRenewError) { p.step = competitionSupervisorRenewalLeaseUpdate }},
		{name: "callback return", mutate: func(p *competitionSupervisorRenewError) { p.step = competitionSupervisorRenewalCallbackReturn }},
		{name: "earlier admission", mutate: func(p *competitionSupervisorRenewError) { p.transaction.AcquireSucceeded++ }},
		{name: "earlier begin", mutate: func(p *competitionSupervisorRenewError) { p.transaction.BeginAttempts++ }},
		{name: "possible commit", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAttempts = 1 }},
		{name: "commit acknowledgment", mutate: func(p *competitionSupervisorRenewError) { p.transaction.CommitAcknowledged = 1 }},
		{name: "rollback not returned", mutate: func(p *competitionSupervisorRenewError) { p.transaction.LastRollback.Returned = false }},
		{name: "rollback not acknowledged", mutate: func(p *competitionSupervisorRenewError) { p.transaction.RollbackAcknowledged = 0 }},
		{name: "release pending", mutate: func(p *competitionSupervisorRenewError) { p.transaction.ReleaseReturned = 0 }},
		{name: "destruction transferred", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		}},
		{name: "unknown custody", mutate: func(p *competitionSupervisorRenewError) {
			p.transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		}},
		{name: "canceled caller", mutate: func(p *competitionSupervisorRenewError) { p.returned.State = "canceled" }},
		{name: "still live caller", mutate: func(p *competitionSupervisorRenewError) { p.returned.State = "live" }},
		{name: "unknown acquire", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Returned = false }},
		{name: "changed SQL prefix", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.valid = false }},
		{name: "unreturned statement", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.returned-- }},
		{name: "manual control", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.bypasses = 1 }},
		{name: "hidden earlier failure", mutate: func(p *competitionSupervisorRenewError) { p.readPrefix.succeeded-- }},
	} {
		proof := competitionSupervisorReadLockFailure()
		check.mutate(&proof)
		if competitionSupervisorReadLockDeadline(proof) {
			t.Errorf("%s granted continuation", check.name)
		}
	}
	proof := competitionSupervisorReadLockFailure()
	for _, cause := range []error{context.Canceled, ErrLeaseLost, errors.New("synthetic unknown"), errors.Join(context.DeadlineExceeded, context.Canceled),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		&os.SyscallError{Syscall: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorUnknownDeadline{cause: context.DeadlineExceeded},
		competitionSupervisorClaimedRetryDeadline{cause: context.DeadlineExceeded},
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		proof.cause = competitionDatabasePhaseError{cause: cause, phase: server.DbOperationCallback}
		if competitionSupervisorReadLockDeadline(proof) {
			t.Error("unknown/canceled cause granted continuation", cause)
		}
	}
	proof = competitionSupervisorReadLockFailure()
	for _, cause := range []error{fmt.Errorf("synthetic outer: %w", proof), errors.Join(proof, context.DeadlineExceeded)} {
		if competitionSupervisorReadLockDeadline(cause) {
			t.Error("unenclosed proof granted continuation")
		}
	}
}

// A user-defined SafeToRetry claim is never equivalent to a source-owned pgx
// wrapper with independently closed transaction and SQL-prefix evidence.
type competitionSupervisorClaimedRetryDeadline struct{ cause error }

// Synthetic wrapper text is irrelevant to the policy.
func (self competitionSupervisorClaimedRetryDeadline) Error() string {
	return "synthetic claimed safe deadline"
}

// Its deadline leaf alone cannot authenticate its wrapper semantics.
func (self competitionSupervisorClaimedRetryDeadline) Unwrap() error { return self.cause }

// An arbitrary driver is not allowed to assert retry authority for itself.
func (self competitionSupervisorClaimedRetryDeadline) SafeToRetry() bool { return true }

// Parent cancellation and exact confirmed expiry win before any new dispatch.
func TestCompetitionSupervisorReadLockRetryPreservesExpiryAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, cancelParent := range []bool{false, true} {
			settings := defaultCompetitionSupervisorRenewalSettings()
			ctx, cancel := context.WithCancel(t.Context())
			expires, calls := time.Now().Add(settings.AttemptTimeout), 0
			if cancelParent {
				expires = time.Now().Add(time.Minute)
			}
			err := retryCompetitionSupervisorRenewal(ctx, expires, settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				if cancelParent {
					cancel()
				}
				return competitionSupervisorReadLockFailure()
			})
			cancel()
			if calls != 1 || cancelParent && !errors.Is(err, context.Canceled) || !cancelParent && !errors.Is(err, ErrLeaseLost) {
				t.Fatal("read-only prefix escaped original authority", cancelParent, calls, err)
			}
		}
	})
}
