// The first automation advisory lock has executed no business mutation. Only
// exact source-owned, returned rollback/pool proof can spend an owned reserve.
package controller

import (
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// No COMMIT, hidden callback retry, later statement or pending release is safe
// continuation authority. Idle release does not claim physical destruction.
func competitionHeartbeatScopeLockTimeout(err error) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	if !ok {
		return false
	}
	snapshot, transaction := proof.snapshot, proof.snapshot.Transaction
	if snapshot.SqlShape != competitionHeartbeatScopeLockShape && snapshot.SqlShape != competitionHeartbeatLegacySlotShape && snapshot.SqlShape != competitionHeartbeatDistributedSlotShape || !snapshot.SqlShapeValid || snapshot.SqlStatements != 1 || snapshot.CallbackEntries != 1 || snapshot.Step != competitionHeartbeatDatabaseScopeLock || snapshot.UnknownStatements != 0 ||
		transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != snapshot.Acquire.Attempt || transaction.AcquireSucceeded != 1 || transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 1 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged != 1 || !transaction.LastRollback.Returned || !transaction.LastBegin.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 || transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
		!competitionHeartbeatLiveBudget(snapshot.AtEntry) || !competitionHeartbeatLiveBudget(snapshot.AtReturn) || !snapshot.Acquire.Returned || !snapshot.Acquire.Admitted ||
		!competitionHeartbeatLiveBudget(snapshot.Acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(snapshot.Acquire.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastBegin.Before) || !competitionHeartbeatLiveBudget(transaction.LastBegin.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastRollback.Before) || !competitionHeartbeatLiveBudget(transaction.LastRollback.AtReturn) || len(snapshot.Statements) != 1 {
		return false
	}
	statement := snapshot.Statements[0]
	if statement.Step != competitionHeartbeatDatabaseScopeLock || statement.Ordinal != 1 || statement.Kind != "exec" || statement.Count != 1 || !statement.Returned || statement.Succeeded || statement.AtReturn == nil ||
		!competitionHeartbeatLiveBudget(statement.Entered) || !competitionHeartbeatLiveBudget(*statement.AtReturn) {
		return false
	}
	causes := server.InspectErrorCauseBatch(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	foundProof, foundPhase, foundLock := false, false, false
	for _, node := range causes.Nodes {
		switch value := node.Err.(type) {
		case competitionHeartbeatDatabaseError:
			if foundProof {
				return false
			}
			foundProof = true
		case competitionDatabasePhaseError:
			if foundPhase || value.phase != server.DbOperationCallback {
				return false
			}
			foundPhase = true
		default:
			if node.Leaf {
				lock, ok := node.Err.(*pgconn.PgError)
				if !ok || lock == nil || lock.Code != "55P03" {
					return false
				}
				foundLock = true
			} else if !competitionSupervisorTransparentCause(node.Err) {
				return false
			}
		}
	}
	return foundProof && foundPhase && foundLock
}
