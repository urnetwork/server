// Supervisor renewal may recover wholly unadmitted or closed read-only
// deadlines. Historical lock budgets, confirmed fencing and workers stay separate.
package controller

import (
	"context"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// One renewal invocation owns all observations, across every inner Tx retry.
// Read them only after capture returns; no mutable observation escapes its owner.
type competitionSupervisorRenewObservation struct {
	phase            server.DbPhaseObservation
	timing           server.DbTiming
	acquire          server.DbAcquireObservation
	read             *server.DbReadObservation
	transaction      server.DbTxObservation
	step             competitionSupervisorRenewalStep
	callbacks        uint64
	callbackEntered  bool
	readPrefix       competitionSupervisorReadPrefixSnapshot
	readPrefixBypass *competitionHeartbeatDatabaseObservation
}

// Fresh cumulative counters cannot forget an earlier admitted inner attempt.
func newCompetitionSupervisorRenewObservation() *competitionSupervisorRenewObservation {
	return &competitionSupervisorRenewObservation{read: server.NewDbReadObservation(),
		readPrefix: competitionSupervisorReadPrefixSnapshot{valid: true}, readPrefixBypass: &competitionHeartbeatDatabaseObservation{}}
}

// Immutable proof is private to the trusted store, not an optional driver claim.
// Error text and Is/As causes retain their original behavior through both wrappers.
type competitionSupervisorRenewError struct {
	cause           error
	phase           server.DbOperationPhase
	acquire         server.DbAcquireObservation
	read            server.DbReadObservationSnapshot
	callbackEntered bool
	transaction     server.DbTxObservationSnapshot
	step            competitionSupervisorRenewalStep
	callbacks       uint64
	entered         server.DbContextBudget
	returned        server.DbContextBudget
	readPrefix      competitionSupervisorReadPrefixSnapshot
	database        *CompetitionSupervisorRenewalDatabaseFailure
}

// Retains the original diagnostic bytes without serializing observations.
func (self competitionSupervisorRenewError) Error() string { return self.cause.Error() }

// Existing Is/As consumers still inspect the exact underlying causes.
func (self competitionSupervisorRenewError) Unwrap() error { return self.cause }

// Preserve the existing phase diagnostic and capture only a fully unwound call.
func (self *competitionSupervisorRenewObservation) capture(run func()) error {
	err := captureDatabaseError(run)
	self.read.Finish(err == nil)
	if err == nil {
		return nil
	}
	prefix := self.readPrefix
	prefix.bypasses = self.readPrefixBypass.unknown
	return competitionSupervisorRenewError{cause: competitionDatabasePhaseError{cause: err, phase: self.phase.Phase()},
		phase: self.phase.Phase(), acquire: self.acquire, read: self.read.Snapshot(), callbackEntered: self.callbackEntered,
		transaction: self.transaction.Snapshot(), step: self.step, callbacks: self.callbacks, readPrefix: prefix}
}

// Exact transparent standard wrappers are allowed; an arbitrary Unwrap/Is/As
// implementation or a network wrapper cannot turn an unknown cause into proof.
func competitionSupervisorTransparentCause(err error) bool {
	typeOf := reflect.TypeOf(err)
	if typeOf == nil || typeOf.Kind() != reflect.Pointer {
		return false
	}
	typeOf = typeOf.Elem()
	return typeOf.PkgPath() == "errors" && typeOf.Name() == "joinError" ||
		typeOf.PkgPath() == "fmt" && (typeOf.Name() == "wrapError" || typeOf.Name() == "wrapErrors")
}

// Phase alone is insufficient: zero cumulative admissions and a complete
// returned-acquire census positively exclude BEGIN, callback and commit history.
// Cause classification covers the returned graph, not unreported earlier
// acquisition refusals that the existing inner database policy already retried.
func competitionSupervisorUnadmittedDeadline(err error) bool {
	// The production driver returns the store's complete proof at the root.
	// A proof in one branch cannot authenticate an unrelated outer Join branch.
	if _, ok := err.(competitionSupervisorRenewError); !ok {
		return false
	}
	causes := server.InspectErrorCauseBatch(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	foundProof, foundDeadline := false, false
	for _, node := range causes.Nodes {
		switch cause := node.Err.(type) {
		case competitionSupervisorRenewError:
			if foundProof || cause.phase != server.DbOperationAcquire || cause.callbackEntered || !cause.acquire.Returned || cause.acquire.Admitted || cause.acquire.Attempt == 0 ||
				cause.acquire.BeforeAcquire.State != "live" || !cause.acquire.BeforeAcquire.HasDeadline || cause.acquire.BeforeAcquire.RemainingNs <= 0 ||
				cause.acquire.AtReturn.State != "deadline" || cause.acquire.AtReturn.RemainingNs > 0 || !cause.acquire.AtReturn.HasDeadline ||
				!cause.read.Finished || cause.read.Phase != server.DbReadError || cause.read.AcquireSucceeded != 0 ||
				cause.read.PhaseCounts[server.DbReadAcquireBegin] != cause.acquire.Attempt || cause.read.PhaseCounts[server.DbReadAcquireDone] != cause.acquire.Attempt ||
				cause.read.PhaseCounts[server.DbReadQueryBegin] != 0 || cause.read.PhaseCounts[server.DbReadQueryDone] != 0 || cause.read.QuerySucceeded != 0 || cause.read.Rows != 0 {
				return false
			}
			foundProof = true
		case competitionDatabasePhaseError:
			if cause.phase != server.DbOperationAcquire {
				return false
			}
		default:
			if node.Leaf {
				if node.Err == context.DeadlineExceeded {
					foundDeadline = true
				} else if node.Err != server.DbContextDoneError {
					return false
				}
			} else if !competitionSupervisorTransparentCause(node.Err) {
				return false
			}
		}
	}
	return foundProof && foundDeadline
}

// Match the unchanged worker's existing complete, lock-only rollback rule.
func competitionSupervisorLockTimeout(err error) bool {
	causes := server.InspectErrorCauseBatch(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	found := false
	for _, node := range causes.Nodes {
		if node.Leaf {
			cause, ok := node.Err.(*pgconn.PgError)
			if !ok || cause == nil || cause.Code != "55P03" {
				return false
			}
			found = true
		}
	}
	return found
}

// Only periodic supervisor renewal owns the additional acquisition allowance.
// LockRetry remains the inherited policy, including its original absolute budget.
type competitionSupervisorRenewalSettings struct {
	// Positive total allowance; parent cancellation and confirmed expiry win.
	Timeout time.Duration
	// Positive per-call allowance, never a replacement total or lease duration.
	AttemptTimeout time.Duration
	// Original lock allowance, shared call cap, backoff and owned clock/wait seams.
	LockRetry competitionLeaseRetrySettings
}

// Manager headroom never changes worker heartbeat/completion defaults.
func defaultCompetitionSupervisorRenewalSettings() competitionSupervisorRenewalSettings {
	lockRetry := defaultCompetitionLeaseRetrySettings()
	return competitionSupervisorRenewalSettings{Timeout: 20 * time.Second, AttemptTimeout: lockRetry.Timeout, LockRetry: lockRetry}
}

// A recovery call beyond the old lock deadline needs fresh positive proof from
// its immediate predecessor. A lock error can never grant that continuation.
func retryCompetitionSupervisorRenewal(ctx context.Context, leaseExpiresAt time.Time, settings competitionSupervisorRenewalSettings, operation func(context.Context) error) error {
	return retryCompetitionAuthorityRenewal(ctx, leaseExpiresAt, settings, operation, func(err error) bool {
		return competitionSupervisorUnadmittedDeadline(err) || competitionSupervisorReadLockDeadline(err)
	})
}

// Each owner supplies its exact completed-deadline predicate. Supervisor/host
// predicates remain acquisition-only; the job heartbeat owns its separate
// closed no-COMMIT proof. Lock allowance, call cap and expiry remain unchanged.
func retryCompetitionAuthorityRenewal(ctx context.Context, leaseExpiresAt time.Time, settings competitionSupervisorRenewalSettings, operation func(context.Context) error, completedDeadline func(error) bool) error {
	return retryCompetitionAuthorityRenewalObserved(ctx, leaseExpiresAt, settings, operation, completedDeadline, nil)
}

// Optional facts mark the selected return branch, never alter its decision,
// clock reads, predicate input, error identity, timeout, cap or authority bound.
func retryCompetitionAuthorityRenewalObserved(ctx context.Context, leaseExpiresAt time.Time, settings competitionSupervisorRenewalSettings, operation func(context.Context) error, completedDeadline func(error) bool, observation *competitionHeartbeatOwnerObservation) error {
	return retryCompetitionAuthorityRenewalWithLockContinuation(ctx, leaseExpiresAt, settings, operation, completedDeadline, observation, nil)
}

// Only the job heartbeat supplies this narrower returned transaction proof.
// The call ceiling is cumulative and the original total/authority never reset.
type competitionAuthorityLockContinuation struct {
	MaximumAttempts         int
	Completed               func(error) bool
	DeadlineMaximumAttempts int
	CompletedDeadline       func(error) bool
}

// Historical callers supply no extension. An exact early read-only failure
// may spend the already-owned reserve, not enlarge it or relax late ACK rules.
func retryCompetitionAuthorityRenewalWithLockContinuation(ctx context.Context, leaseExpiresAt time.Time, settings competitionSupervisorRenewalSettings, operation func(context.Context) error, completedDeadline func(error) bool, observation *competitionHeartbeatOwnerObservation, continuation *competitionAuthorityLockContinuation) error {
	defaults := defaultCompetitionSupervisorRenewalSettings()
	if settings.LockRetry.Now == nil {
		settings.LockRetry.Now = defaults.LockRetry.Now
	}
	if settings.LockRetry.Wait == nil {
		settings.LockRetry.Wait = defaults.LockRetry.Wait
	}
	if err := ctx.Err(); err != nil {
		observation.returning(competitionHeartbeatOwnerParentBeforeDispatch, ctx, nil)
		return err
	}
	// Preserve monotonic duration ownership even though the authority clock is UTC.
	startedAt := time.Now()
	now := settings.LockRetry.Now()
	if !leaseExpiresAt.IsZero() && !now.Before(leaseExpiresAt) {
		observation.returning(competitionHeartbeatOwnerAuthorityBeforeDispatch, ctx, nil)
		return ErrLeaseLost
	}
	totalDeadline, lockDeadline := leaseExpiresAt, leaseExpiresAt
	if settings.Timeout > 0 && (totalDeadline.IsZero() || now.Add(settings.Timeout).Before(totalDeadline)) {
		totalDeadline = now.Add(settings.Timeout)
	}
	if settings.LockRetry.Timeout > 0 && (lockDeadline.IsZero() || now.Add(settings.LockRetry.Timeout).Before(lockDeadline)) {
		lockDeadline = now.Add(settings.LockRetry.Timeout)
	}
	totalCtx, cancel := context.WithCancel(ctx)
	if !totalDeadline.IsZero() {
		cancel()
		totalCtx, cancel = context.WithDeadline(ctx, startedAt.Add(totalDeadline.Sub(now)))
	}
	defer cancel()
	lockCtx, lockCancel := context.WithCancel(totalCtx)
	if !lockDeadline.IsZero() {
		lockCancel()
		lockCtx, lockCancel = context.WithDeadline(totalCtx, startedAt.Add(lockDeadline.Sub(now)))
	}
	defer lockCancel()
	deadlineRecovery := false
	var lastError error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			observation.returning(competitionHeartbeatOwnerParentBeforeDispatch, ctx, totalCtx)
			return err
		}
		now = settings.LockRetry.Now()
		if !leaseExpiresAt.IsZero() && !now.Before(leaseExpiresAt) {
			observation.returning(competitionHeartbeatOwnerAuthorityBeforeDispatch, ctx, totalCtx)
			return ErrLeaseLost
		}
		if err := totalCtx.Err(); err != nil {
			observation.returning(competitionHeartbeatOwnerTotalBeforeDispatch, ctx, totalCtx)
			return err
		}
		if !deadlineRecovery && lockCtx.Err() != nil {
			observation.returning(competitionHeartbeatOwnerLockBeforeDispatch, ctx, totalCtx)
			if lastError == nil {
				return context.DeadlineExceeded
			}
			return lastError
		}
		if !totalDeadline.IsZero() && !now.Before(totalDeadline) || !deadlineRecovery && settings.LockRetry.Timeout > 0 && !lockDeadline.IsZero() && !now.Before(lockDeadline) {
			boundary := competitionHeartbeatOwnerLockBeforeDispatch
			if !totalDeadline.IsZero() && !now.Before(totalDeadline) {
				boundary = competitionHeartbeatOwnerTotalBeforeDispatch
			}
			observation.returning(boundary, ctx, totalCtx)
			if lastError == nil {
				return context.DeadlineExceeded
			}
			return lastError
		}
		attemptDeadline := totalDeadline
		if !deadlineRecovery && settings.LockRetry.Timeout > 0 && (attemptDeadline.IsZero() || lockDeadline.Before(attemptDeadline)) {
			attemptDeadline = lockDeadline
		}
		if settings.AttemptTimeout > 0 && (attemptDeadline.IsZero() || now.Add(settings.AttemptTimeout).Before(attemptDeadline)) {
			attemptDeadline = now.Add(settings.AttemptTimeout)
		}
		attemptParent := lockCtx
		if deadlineRecovery {
			attemptParent = totalCtx
		}
		attemptCtx, attemptCancel := context.WithCancel(attemptParent)
		if settings.AttemptTimeout > 0 {
			attemptCancel()
			attemptCtx, attemptCancel = context.WithTimeout(attemptParent, settings.AttemptTimeout)
		}
		if parentError := ctx.Err(); parentError != nil {
			attemptCancel()
			observation.returning(competitionHeartbeatOwnerParentBeforeDispatch, ctx, totalCtx)
			return parentError
		}
		dispatchNow := settings.LockRetry.Now()
		if parentError := ctx.Err(); parentError != nil {
			attemptCancel()
			observation.returning(competitionHeartbeatOwnerParentBeforeDispatch, ctx, totalCtx)
			return parentError
		}
		if !leaseExpiresAt.IsZero() && !dispatchNow.Before(leaseExpiresAt) {
			attemptCancel()
			observation.returning(competitionHeartbeatOwnerAuthorityBeforeDispatch, ctx, totalCtx)
			return ErrLeaseLost
		}
		if attemptError := attemptCtx.Err(); attemptError != nil {
			attemptCancel()
			observation.returning(competitionHeartbeatOwnerAttemptBeforeDispatch, ctx, totalCtx)
			return attemptError
		}
		if !attemptDeadline.IsZero() && !dispatchNow.Before(attemptDeadline) {
			attemptCancel()
			observation.returning(competitionHeartbeatOwnerAttemptBeforeDispatch, ctx, totalCtx)
			return context.DeadlineExceeded
		}
		err := operation(attemptCtx)
		attemptError := attemptCtx.Err()
		attemptCancel()
		if parentError := ctx.Err(); parentError != nil {
			observation.returning(competitionHeartbeatOwnerParentAfterStore, ctx, totalCtx)
			return parentError
		}
		now = settings.LockRetry.Now()
		if parentError := ctx.Err(); parentError != nil {
			observation.returning(competitionHeartbeatOwnerParentAfterStore, ctx, totalCtx)
			return parentError
		}
		if err == nil {
			if !leaseExpiresAt.IsZero() && !now.Before(leaseExpiresAt) {
				observation.returning(competitionHeartbeatOwnerAuthorityAfterStore, ctx, totalCtx)
				return ErrLeaseLost
			}
			if attemptError != nil {
				observation.returning(competitionHeartbeatOwnerAttemptAfterSuccess, ctx, totalCtx)
				return attemptError
			}
			if !attemptDeadline.IsZero() && !now.Before(attemptDeadline) {
				observation.returning(competitionHeartbeatOwnerAttemptAfterSuccess, ctx, totalCtx)
				return context.DeadlineExceeded
			}
			observation.returning(competitionHeartbeatOwnerSuccess, ctx, totalCtx)
			return nil
		}
		// A positively unexecuted pooler refusal shares the historical lock
		// allowance; it cannot borrow the owned-deadline recovery reserve.
		lockRetry := attemptError == nil && (competitionSupervisorLockTimeout(err) || competitionLivenessPoolerRefusal(err))
		deadlineRetry := attemptError == context.DeadlineExceeded && completedDeadline != nil && completedDeadline(err)
		scopeLockRetry := lockRetry && continuation != nil && continuation.Completed != nil && continuation.Completed(err)
		maximumAttempts := settings.LockRetry.MaximumAttempts
		if scopeLockRetry && maximumAttempts < continuation.MaximumAttempts {
			maximumAttempts = continuation.MaximumAttempts
		}
		if deadlineRetry && continuation != nil && continuation.CompletedDeadline != nil && continuation.CompletedDeadline(err) && maximumAttempts < continuation.DeadlineMaximumAttempts {
			maximumAttempts = continuation.DeadlineMaximumAttempts
		}
		if !lockRetry && !deadlineRetry || leaseExpiresAt.IsZero() || settings.Timeout <= 0 || settings.AttemptTimeout <= 0 ||
			settings.LockRetry.Timeout <= 0 || settings.LockRetry.Interval <= 0 || attempt+1 >= maximumAttempts {
			observation.returning(competitionHeartbeatOwnerStoreError, ctx, totalCtx)
			return err
		}
		if !now.Before(leaseExpiresAt) {
			observation.returning(competitionHeartbeatOwnerAuthorityAfterStore, ctx, totalCtx)
			return ErrLeaseLost
		}
		totalExpired := totalCtx.Err() != nil || !now.Before(totalDeadline)
		if totalExpired || lockRetry && !scopeLockRetry && !now.Before(lockDeadline) {
			boundary := competitionHeartbeatOwnerLockAfterStore
			if totalExpired {
				boundary = competitionHeartbeatOwnerTotalAfterStore
			}
			observation.returning(boundary, ctx, totalCtx)
			return err
		}
		lastError, deadlineRecovery = err, deadlineRetry || scopeLockRetry
		waitDeadline := totalDeadline
		if !deadlineRecovery && lockDeadline.Before(waitDeadline) {
			waitDeadline = lockDeadline
		}
		delay := min(settings.LockRetry.Interval, waitDeadline.Sub(now))
		waitCtx := totalCtx
		if !deadlineRecovery {
			waitCtx = lockCtx
		}
		if err := settings.LockRetry.Wait(waitCtx, delay); err != nil {
			if parentError := ctx.Err(); parentError != nil {
				observation.returning(competitionHeartbeatOwnerParentDuringWait, ctx, totalCtx)
				return parentError
			}
			observation.returning(competitionHeartbeatOwnerWaitError, ctx, totalCtx)
			return err
		}
	}
}
