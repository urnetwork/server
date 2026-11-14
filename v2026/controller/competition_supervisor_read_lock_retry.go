// Renewal's read-only prefix may recover a completed owned deadline after an
// acknowledged rollback and returned protocol-idle custody. No update or ambiguous commit qualifies.
package controller

import (
	"context"
	"reflect"

	"github.com/urnetwork/server/v2026"
)

// The private invocation facts are separate from the optional public report.
// Scope/row reads and transaction-local guards precede every lease mutation.
func competitionSupervisorReadLockDeadline(err error) bool {
	return competitionSupervisorReadOnlyDeadline(err, false)
}

// The worker's separate store only reads authority and sets transaction-local
// guards. It never grants the renewal writer's post-prefix steps a retry.
func competitionSupervisorWorkerFenceDeadline(err error) bool {
	return competitionSupervisorReadOnlyDeadline(err, true)
}

// The caller chooses an exact trusted source path, never a serialized claim.
func competitionSupervisorReadOnlyDeadline(err error, workerFence bool) bool {
	proof, ok := err.(competitionSupervisorRenewError)
	if !ok || proof.phase != server.DbOperationCallback || !proof.callbackEntered || proof.callbacks != 1 ||
		!competitionHeartbeatLiveBudget(proof.entered) || proof.returned.State != "deadline" || !proof.returned.HasDeadline || proof.returned.RemainingNs > 0 {
		return false
	}
	if workerFence {
		if proof.step != competitionSupervisorWorkerFenceRead {
			return false
		}
	} else if proof.step != competitionSupervisorRenewalScopeLock && proof.step != competitionSupervisorRenewalLeaseRowLock && proof.step != competitionSupervisorRenewalAuthority {
		return false
	}
	prefix := proof.readPrefix
	expected := competitionSupervisorObservedReadPrefix(prefix)
	if !prefix.valid || prefix.workerFence != workerFence || prefix.bypasses != 0 || prefix.count == 0 || prefix.count > uint64(len(expected)) ||
		prefix.returned != prefix.count || prefix.succeeded+1 != prefix.count || expected[prefix.count-1].step != proof.step {
		return false
	}
	transaction := proof.transaction
	if transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != proof.acquire.Attempt || transaction.AcquireSucceeded != 1 ||
		transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 1 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged != 1 || !transaction.LastRollback.Returned || !transaction.LastBegin.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 || transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
		!proof.acquire.Returned || !proof.acquire.Admitted || !competitionHeartbeatLiveBudget(proof.acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(proof.acquire.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastBegin.Before) || !competitionHeartbeatLiveBudget(transaction.LastBegin.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastRollback.Before) || !competitionHeartbeatLiveBudget(transaction.LastRollback.AtReturn) {
		return false
	}
	causes := server.InspectErrorCauseBatch(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	foundProof, foundPhase, foundDeadline := false, false, false
	for _, node := range causes.Nodes {
		switch cause := node.Err.(type) {
		case competitionSupervisorRenewError:
			if foundProof {
				return false
			}
			foundProof = true
		case competitionDatabasePhaseError:
			if foundPhase || cause.phase != server.DbOperationCallback {
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
			} else if !competitionSupervisorReadOnlyDeadlineCause(node.Err) {
				return false
			}
		}
	}
	return foundProof && foundPhase && foundDeadline
}

// Pgx's own timeout and already-stopped wrappers add no independent cause.
// The complete graph still must end only in the owned deadline/Done sentinels;
// arbitrary network, syscall, driver or user wrappers remain disallowed.
func competitionSupervisorReadOnlyDeadlineCause(err error) bool {
	if competitionSupervisorTransparentCause(err) {
		return true
	}
	typeOf := reflect.TypeOf(err)
	if typeOf == nil || typeOf.Kind() != reflect.Pointer {
		return false
	}
	typeOf = typeOf.Elem()
	return typeOf.PkgPath() == "github.com/jackc/pgx/v5/pgconn" && (typeOf.Name() == "errTimeout" || typeOf.Name() == "contextAlreadyDoneError")
}
