// A returned, wholly unadmitted heartbeat acquisition may recover while its
// original confirmed authority remains live. The job-only adapter separately
// recognizes one closed no-COMMIT slot-update failure; host renewal does not.
package controller

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// These existing budgets belong to one worker, independently of manager
// renewal and completion. Nonpositive values disable deadline continuation.
type competitionHeartbeatAcquireRetrySettings struct {
	Timeout        time.Duration
	AttemptTimeout time.Duration
}

// Three calls share twenty seconds, each no longer than the original eight.
// The confirmed lease and live worker parent can only shorten them.
func defaultCompetitionHeartbeatAcquireRetrySettings() competitionHeartbeatAcquireRetrySettings {
	return competitionHeartbeatAcquireRetrySettings{Timeout: 20 * time.Second, AttemptTimeout: 8 * time.Second}
}

// Startup/idle registration retains its original single-call behavior while
// recording only a genuine timely acknowledgement for later heartbeat use.
func (self *Worker) registerHost(ctx context.Context, check HostSelfCheck) error {
	expiresAt, err := self.registerHostAcknowledged(ctx, check)
	if err == nil {
		self.confirmedHostLeaseExpiresAt = expiresAt
	}
	return err
}

// The idle owner retains its acknowledgement privately until it has joined;
// only the existing job owner writes the worker's shared confirmed bound.
func (self *Worker) registerHostAcknowledged(ctx context.Context, check HostSelfCheck) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	startedAt := self.heartbeatNow()
	if err := self.store.RegisterHost(ctx, self.settings, check); err != nil {
		return time.Time{}, err
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	expiresAt := startedAt.Add(time.Duration(self.settings.HostHeartbeatMaxAgeSeconds) * time.Second)
	if !self.heartbeatNow().Before(expiresAt) {
		return time.Time{}, ErrLeaseLost
	}
	return expiresAt, nil
}

// An unknown host bound preserves the historical lock-only first refresh; it
// cannot authorize new acquisition continuation using a job lease as a proxy.
func (self *Worker) refreshHostHeartbeat(ctx context.Context, jobExpiresAt time.Time, check HostSelfCheck) error {
	operation := func(attemptCtx context.Context) error {
		return self.store.RegisterHost(attemptCtx, self.settings, check)
	}
	return self.refreshHostHeartbeatWithOperation(ctx, jobExpiresAt, operation)
}

// The worker can withdraw a cleanup dispatch when its evaluator has already
// finished, without changing the host retry predicate, budget or ACK rules.
func (self *Worker) refreshHostHeartbeatWithOperation(ctx context.Context, jobExpiresAt time.Time, operation func(context.Context) error) error {
	var startedAt time.Time
	var err error
	if self.confirmedHostLeaseExpiresAt.IsZero() {
		startedAt, err = retryCompetitionLeaseOperation(ctx, jobExpiresAt, self.heartbeatSettings.LeaseRetry, operation)
	} else {
		expiresAt := self.confirmedHostLeaseExpiresAt
		if !jobExpiresAt.IsZero() && jobExpiresAt.Before(expiresAt) {
			expiresAt = jobExpiresAt
		}
		startedAt, err = retryCompetitionHeartbeatOperation(ctx, expiresAt, self.heartbeatSettings, true, operation)
	}
	if err == nil {
		self.confirmedHostLeaseExpiresAt = startedAt.Add(time.Duration(self.settings.HostHeartbeatMaxAgeSeconds) * time.Second)
	}
	return err
}

// All confirmed bounds use the same per-worker clock as the retry policy.
func (self *Worker) heartbeatNow() time.Time {
	if now := self.heartbeatSettings.LeaseRetry.Now; now != nil {
		return now()
	}
	return server.NowUtc()
}

// The returned start is adopted only after the whole public operation and its
// cleanup return within every authority/deadline check of the shared engine.
func retryCompetitionHeartbeatOperation(ctx context.Context, leaseExpiresAt time.Time, settings competitionHeartbeatSettings, host bool, operation func(context.Context) error) (time.Time, error) {
	proof := competitionHeartbeatUnadmittedDeadline
	if host {
		proof = competitionHostHeartbeatUnadmittedDeadline
	}
	return retryCompetitionHeartbeatWithProof(ctx, leaseExpiresAt, settings, operation, proof)
}

// A job can continue its closed no-COMMIT transaction only while BOTH of its
// previously confirmed bounds remain live. Unknown host authority preserves
// the acquisition-only behavior; it cannot authorize an admitted continuation.
func retryCompetitionJobHeartbeatOperation(ctx context.Context, jobExpiresAt, hostExpiresAt time.Time, settings competitionHeartbeatSettings, operation func(context.Context) error) (time.Time, error) {
	return retryCompetitionJobHeartbeatOperationObserved(ctx, jobExpiresAt, hostExpiresAt, settings, operation, nil)
}

// The worker alone opts into finite diagnostics; errors stay original here.
func retryCompetitionJobHeartbeatOperationObserved(ctx context.Context, jobExpiresAt, hostExpiresAt time.Time, settings competitionHeartbeatSettings, operation func(context.Context) error, observation *competitionHeartbeatOwnerObservation) (time.Time, error) {
	proof := competitionHeartbeatUnadmittedDeadline
	expiresAt := jobExpiresAt
	var continuation *competitionAuthorityLockContinuation
	if !jobExpiresAt.IsZero() && !hostExpiresAt.IsZero() {
		if hostExpiresAt.Before(expiresAt) {
			expiresAt = hostExpiresAt
		}
		proof = func(err error) bool {
			return competitionHeartbeatUnadmittedDeadline(err) || competitionHeartbeatSlotUpdateDeadline(err) || competitionHeartbeatJobLockDeadline(err) || competitionHeartbeatDistributedSlotUpdateDeadline(err) ||
				settings.DistributedReturnedRollbackMaximumAttempts > 0 && competitionHeartbeatDistributedReturnedRollbackDeadline(err)
		}
		if settings.ScopeLockMaximumAttempts > settings.LeaseRetry.MaximumAttempts || settings.DistributedReturnedRollbackMaximumAttempts > settings.LeaseRetry.MaximumAttempts {
			continuation = &competitionAuthorityLockContinuation{}
			if settings.ScopeLockMaximumAttempts > settings.LeaseRetry.MaximumAttempts {
				continuation.MaximumAttempts, continuation.Completed = settings.ScopeLockMaximumAttempts, competitionHeartbeatScopeLockTimeout
			}
			if settings.DistributedReturnedRollbackMaximumAttempts > settings.LeaseRetry.MaximumAttempts {
				continuation.DeadlineMaximumAttempts = settings.DistributedReturnedRollbackMaximumAttempts
				continuation.CompletedDeadline = competitionHeartbeatDistributedReturnedRollbackDeadline
			}
		}
	}
	return retryCompetitionHeartbeatWithLockContinuation(ctx, expiresAt, settings, operation, proof, observation, continuation)
}

// Only the job adapter above broadens its own predicate. Supervisor/host
// callers retain their original proofs, twenty-second total and shared cap.
func retryCompetitionHeartbeatWithProof(ctx context.Context, leaseExpiresAt time.Time, settings competitionHeartbeatSettings, operation func(context.Context) error, proof func(error) bool) (time.Time, error) {
	return retryCompetitionHeartbeatWithProofObserved(ctx, leaseExpiresAt, settings, operation, proof, nil)
}

// Neither observation nor completed-store capture can wrap a predicate input.
func retryCompetitionHeartbeatWithProofObserved(ctx context.Context, leaseExpiresAt time.Time, settings competitionHeartbeatSettings, operation func(context.Context) error, proof func(error) bool, observation *competitionHeartbeatOwnerObservation) (time.Time, error) {
	return retryCompetitionHeartbeatWithLockContinuation(ctx, leaseExpiresAt, settings, operation, proof, observation, nil)
}

// Only the exact job adapter opts into the scoped first-lock continuation.
func retryCompetitionHeartbeatWithLockContinuation(ctx context.Context, leaseExpiresAt time.Time, settings competitionHeartbeatSettings, operation func(context.Context) error, proof func(error) bool, observation *competitionHeartbeatOwnerObservation, continuation *competitionAuthorityLockContinuation) (time.Time, error) {
	policy := competitionSupervisorRenewalSettings{Timeout: settings.AcquireRetry.Timeout, AttemptTimeout: settings.AcquireRetry.AttemptTimeout, LockRetry: settings.LeaseRetry}
	var acknowledgedStart time.Time
	err := retryCompetitionAuthorityRenewalWithLockContinuation(ctx, leaseExpiresAt, policy, func(attemptCtx context.Context) error {
		now := settings.LeaseRetry.Now
		if now == nil {
			now = server.NowUtc
		}
		startedAt := now()
		var err error
		if observation == nil {
			err = operation(attemptCtx)
		} else {
			database := &competitionHeartbeatCompletedDatabase{}
			observation.beginStore(attemptCtx)
			err = operation(context.WithValue(attemptCtx, competitionHeartbeatCompletedDatabaseKey{}, database))
			observation.finishStore(attemptCtx, err == nil, database)
		}
		if err == nil {
			acknowledgedStart = startedAt
		}
		return err
	}, proof, observation, continuation)
	if err != nil {
		return time.Time{}, err
	}
	return acknowledgedStart, nil
}

// Cumulative transaction counts exclude earlier admission hidden by a later
// acquisition phase. The exact root proof must enclose every returned cause.
func competitionHeartbeatUnadmittedDeadline(err error) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	if !ok {
		return false
	}
	snapshot := proof.snapshot
	transaction := snapshot.Transaction
	if snapshot.CallbackEntries != 0 || snapshot.Step != competitionHeartbeatDatabaseUnknown || snapshot.UnknownStatements != 0 || len(snapshot.Statements) != 0 ||
		transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != snapshot.Acquire.Attempt || transaction.AcquireSucceeded != 0 ||
		transaction.BeginAttempts != 0 || transaction.BeginSucceeded != 0 || transaction.CallbackEntered != 0 || transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 ||
		transaction.RollbackAttempts != 0 || transaction.RollbackAcknowledged != 0 || transaction.LastBegin != (server.DbTxBoundaryObservation{}) || transaction.LastCommit != (server.DbTxBoundaryObservation{}) || transaction.LastRollback != (server.DbTxBoundaryObservation{}) ||
		transaction.ReleaseAttempts != 0 || transaction.ReleaseReturned != 0 || transaction.LastRelease != (server.DbTxReleaseObservation{}) ||
		!competitionReturnedUnadmittedAcquire(snapshot.Acquire) {
		return false
	}
	return competitionHeartbeatDeadlineCauses(err, false)
}

// Host registration is a Db callback, not a transaction. Independent read
// history plus callback entry excludes an earlier admitted inner retry.
type competitionHostHeartbeatObservation struct {
	phase           server.DbPhaseObservation
	timing          server.DbTiming
	acquire         server.DbAcquireObservation
	read            *server.DbReadObservation
	callbackEntered bool
}

type competitionHostHeartbeatAcquireError struct {
	cause           error
	acquire         server.DbAcquireObservation
	read            server.DbReadObservationSnapshot
	callbackEntered bool
}

// Cause bytes and identity remain identical to the original phase capture.
func (self competitionHostHeartbeatAcquireError) Error() string { return self.cause.Error() }
func (self competitionHostHeartbeatAcquireError) Unwrap() error { return self.cause }

// One fresh owner spans all inner Db retries and captures only after unwind.
func newCompetitionHostHeartbeatObservation() *competitionHostHeartbeatObservation {
	return &competitionHostHeartbeatObservation{read: server.NewDbReadObservation()}
}

func (self *competitionHostHeartbeatObservation) capture(run func()) error {
	err := captureDatabaseError(run)
	self.read.Finish(err == nil)
	if err == nil {
		return nil
	}
	return competitionHostHeartbeatAcquireError{cause: competitionDatabasePhaseError{cause: err, phase: self.phase.Phase()}, acquire: self.acquire,
		read: self.read.Snapshot(), callbackEntered: self.callbackEntered}
}

// A host proof may not borrow a successful job heartbeat's authority window.
func competitionHostHeartbeatUnadmittedDeadline(err error) bool {
	proof, ok := err.(competitionHostHeartbeatAcquireError)
	if !ok || proof.callbackEntered || !competitionReturnedUnadmittedAcquire(proof.acquire) || !proof.read.Finished || proof.read.Phase != server.DbReadError ||
		proof.read.AcquireSucceeded != 0 || proof.read.PhaseCounts[server.DbReadAcquireBegin] != proof.acquire.Attempt || proof.read.PhaseCounts[server.DbReadAcquireDone] != proof.acquire.Attempt ||
		proof.read.PhaseCounts[server.DbReadQueryBegin] != 0 || proof.read.PhaseCounts[server.DbReadQueryDone] != 0 || proof.read.QuerySucceeded != 0 || proof.read.Rows != 0 {
		return false
	}
	return competitionHeartbeatDeadlineCauses(err, true)
}

// Last acquisition must have physically returned without admission after its
// own live deadline elapsed. Neither phase nor error text is sufficient.
func competitionReturnedUnadmittedAcquire(acquire server.DbAcquireObservation) bool {
	return acquire.Attempt > 0 && acquire.Returned && !acquire.Admitted && acquire.BeforeAcquire.State == "live" && acquire.BeforeAcquire.HasDeadline && acquire.BeforeAcquire.RemainingNs > 0 &&
		acquire.AtReturn.State == "deadline" && acquire.AtReturn.HasDeadline && acquire.AtReturn.RemainingNs <= 0
}

// Only exact source-owned proof wrappers, phase wrappers and standard fmt/join
// transparency may surround deadline/Done leaves. Unknown graphs fail closed.
func competitionHeartbeatDeadlineCauses(root error, host bool) bool {
	return competitionHeartbeatDeadlineCausesInPhase(root, host, server.DbOperationAcquire)
}

// The no-COMMIT caller must request Callback explicitly; this never changes
// what qualifies as an unadmitted host/job acquisition failure.
func competitionHeartbeatDeadlineCausesInPhase(root error, host bool, phase server.DbOperationPhase) bool {
	causes := server.InspectErrorCauseBatch(root)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	foundProof, foundPhase, foundDeadline := false, false, false
	for _, node := range causes.Nodes {
		switch value := node.Err.(type) {
		case competitionHeartbeatDatabaseError:
			if host || foundProof {
				return false
			}
			foundProof = true
		case competitionHostHeartbeatAcquireError:
			if !host || foundProof {
				return false
			}
			foundProof = true
		case competitionDatabasePhaseError:
			if foundPhase || value.phase != phase {
				return false
			}
			foundPhase = true
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
	return foundProof && foundPhase && foundDeadline
}
