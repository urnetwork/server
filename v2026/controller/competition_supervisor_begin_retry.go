// A late successful BEGIN may recover only after its no-business transaction
// rolled back and returned idle. The worker's original authority still bounds it.
package controller

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

// Only the periodic worker fence reader selects this private predicate. Session
// setup, unknown BEGIN, callback work and commit history retain their old refusal.
func competitionSupervisorWorkerBeginDeadline(err error) bool {
	proof, ok := err.(competitionSupervisorRenewError)
	if !ok || proof.phase != server.DbOperationBegin || proof.callbackEntered || proof.callbacks != 0 || proof.step != competitionSupervisorRenewalUnknown ||
		proof.readPrefix != (competitionSupervisorReadPrefixSnapshot{valid: true}) || !competitionHeartbeatLiveBudget(proof.entered) ||
		proof.returned.State != "deadline" || !proof.returned.HasDeadline || proof.returned.RemainingNs > 0 {
		return false
	}
	read := proof.read
	if !read.Finished || read.Phase != server.DbReadError || read.AcquireSucceeded != 1 ||
		read.PhaseCounts[server.DbReadAcquireBegin] != proof.acquire.Attempt || read.PhaseCounts[server.DbReadAcquireDone] != proof.acquire.Attempt ||
		read.PhaseCounts[server.DbReadQueryBegin] != 0 || read.PhaseCounts[server.DbReadQueryDone] != 0 || read.QuerySucceeded != 0 || read.Rows != 0 {
		return false
	}
	transaction := proof.transaction
	if transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != proof.acquire.Attempt || transaction.AcquireSucceeded != 1 ||
		transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 0 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged != 1 || !transaction.LastRollback.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 || transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
		!proof.acquire.Returned || !proof.acquire.Admitted || !competitionHeartbeatLiveBudget(proof.acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(proof.acquire.AtReturn) ||
		!transaction.LastBegin.Returned || !competitionHeartbeatLiveBudget(transaction.LastBegin.Before) || transaction.LastBegin.AtReturn.State != "deadline" ||
		!transaction.LastBegin.AtReturn.HasDeadline || transaction.LastBegin.AtReturn.RemainingNs > 0 ||
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
			if foundPhase || cause.phase != server.DbOperationBegin {
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
