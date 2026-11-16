// Only the exact fenced renewal UPDATE may recover after a proved rollback
// and synchronous physical pool return. Serialized diagnostics grant nothing.
package controller

import (
	"context"

	"github.com/urnetwork/server"
)

// This is the existing renewal statement, not an extensible SQL allowlist.
const competitionSupervisorLeaseRenewalUpdateSql = `UPDATE competition_supervisor_lease SET lease_expires_at=$4,heartbeat_at=$5
	 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$6 AND fencing_token=$7 AND $5<lease_expires_at`

// The trusted renewal writer alone selects its complete SQL surface. Legacy
// read-only and worker-fence callers retain their separate prefix class.
func (self *competitionSupervisorRenewObservation) observeLeaseRenewalPrefix(tx server.PgTx) server.PgTx {
	self.readPrefix.renewalWriter = true
	return self.observeReadPrefix(tx, false)
}

// A callback step label or rollback count alone cannot prove a complete safe
// retry. Every inner admission and statement belongs to this returned root.
func competitionSupervisorRolledBackUpdateDeadline(err error) bool {
	proof, ok := err.(competitionSupervisorRenewError)
	if !ok || proof.phase != server.DbOperationCallback || !proof.callbackEntered || proof.callbacks != 1 ||
		proof.step != competitionSupervisorRenewalLeaseUpdate || !competitionHeartbeatLiveBudget(proof.entered) ||
		proof.returned.State != "deadline" || !proof.returned.HasDeadline || proof.returned.RemainingNs > 0 {
		return false
	}
	prefix := proof.readPrefix
	expected := competitionSupervisorObservedReadPrefix(prefix)
	if !prefix.valid || !prefix.renewalWriter || !prefix.scopeIndependent || prefix.workerFence || prefix.bypasses != 0 ||
		prefix.count == 0 || prefix.count != uint64(len(expected)) || prefix.returned != prefix.count || prefix.succeeded+1 != prefix.count ||
		expected[prefix.count-1].step != competitionSupervisorRenewalLeaseUpdate {
		return false
	}
	transaction := proof.transaction
	if transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != proof.acquire.Attempt || transaction.AcquireSucceeded != 1 ||
		transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 1 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged != 1 || !transaction.LastRollback.Returned || !transaction.LastBegin.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 || transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
		!transaction.LastRelease.PoolReturned || !proof.acquire.Returned || !proof.acquire.Admitted ||
		!competitionHeartbeatLiveBudget(proof.acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(proof.acquire.AtReturn) ||
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
