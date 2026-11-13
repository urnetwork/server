// Pooler assignment refusal may recover only before any business callback.
// Every retry shares existing caps and confirmed authority, never an unknown commit.
package controller

import (
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Cumulative invocation facts exclude a callback hidden by a later acquire.
// A failed BEGIN may transfer socket destruction, but cannot have issued work.
func competitionLivenessNoCallback(transaction server.DbTxObservationSnapshot, acquire server.DbAcquireObservation) bool {
	if transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != acquire.Attempt || !acquire.Returned ||
		!competitionHeartbeatLiveBudget(acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(acquire.AtReturn) ||
		transaction.CallbackEntered != 0 || transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 ||
		transaction.LastCommit != (server.DbTxBoundaryObservation{}) || transaction.RollbackAttempts != 0 || transaction.RollbackAcknowledged != 0 ||
		transaction.LastRollback != (server.DbTxBoundaryObservation{}) {
		return false
	}
	if !acquire.Admitted {
		return transaction.AcquireSucceeded == 0 && transaction.BeginAttempts == 0 && transaction.BeginSucceeded == 0 &&
			transaction.LastBegin == (server.DbTxBoundaryObservation{}) && transaction.ReleaseAttempts == 0 && transaction.ReleaseReturned == 0 &&
			transaction.LastRelease == (server.DbTxReleaseObservation{})
	}
	return transaction.AcquireSucceeded == 1 && transaction.BeginAttempts == 1 && transaction.BeginSucceeded == 0 && transaction.LastBegin.Returned &&
		competitionHeartbeatLiveBudget(transaction.LastBegin.Before) && competitionHeartbeatLiveBudget(transaction.LastBegin.AtReturn) &&
		transaction.ReleaseAttempts == 1 && transaction.ReleaseReturned == 1 && transaction.LastRelease.Disposition == server.DbTxReleaseDestroyTransferred
}

// Exact source-owned roots enclose the entire failure. Error categories,
// persisted diagnostics and an arbitrary driver's phase claim grant nothing.
func competitionLivenessPoolerRefusal(err error) bool {
	phase := server.DbOperationUnknown
	switch proof := err.(type) {
	case competitionSupervisorRenewError:
		phase = proof.phase
		if proof.callbackEntered || proof.callbacks != 0 || proof.step != competitionSupervisorRenewalUnknown ||
			!competitionLivenessNoCallback(proof.transaction, proof.acquire) {
			return false
		}
	case competitionHeartbeatDatabaseError:
		snapshot := proof.snapshot
		if snapshot.CallbackEntries != 0 || snapshot.Step != competitionHeartbeatDatabaseUnknown || snapshot.UnknownStatements != 0 || len(snapshot.Statements) != 0 ||
			!competitionLivenessNoCallback(snapshot.Transaction, snapshot.Acquire) {
			return false
		}
		if value, ok := proof.cause.(competitionDatabasePhaseError); ok {
			phase = value.phase
		}
	case competitionHostHeartbeatAcquireError:
		if proof.callbackEntered || !proof.acquire.Returned || proof.acquire.Admitted || proof.acquire.Attempt == 0 || !proof.read.Finished ||
			!competitionHeartbeatLiveBudget(proof.acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(proof.acquire.AtReturn) ||
			proof.read.Phase != server.DbReadError || proof.read.AcquireSucceeded != 0 ||
			proof.read.PhaseCounts[server.DbReadAcquireBegin] != proof.acquire.Attempt || proof.read.PhaseCounts[server.DbReadAcquireDone] != proof.acquire.Attempt ||
			proof.read.PhaseCounts[server.DbReadQueryBegin] != 0 || proof.read.PhaseCounts[server.DbReadQueryDone] != 0 || proof.read.QuerySucceeded != 0 || proof.read.Rows != 0 {
			return false
		}
		phase = server.DbOperationAcquire
	default:
		return false
	}
	if phase != server.DbOperationAcquire && phase != server.DbOperationBegin {
		return false
	}
	causes := server.InspectErrorCauseBatch(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	found, foundProof, foundPhase := false, false, false
	for _, node := range causes.Nodes {
		switch cause := node.Err.(type) {
		case competitionSupervisorRenewError, competitionHeartbeatDatabaseError, competitionHostHeartbeatAcquireError:
			if foundProof {
				return false
			}
			foundProof = true
		case competitionDatabasePhaseError:
			if foundPhase || cause.phase != phase {
				return false
			}
			foundPhase = true
		case *pgconn.PgError:
			if cause == nil || cause.Code != "08P01" || cause.Severity != "FATAL" || cause.Message != "query_wait_timeout" {
				return false
			}
			found = true
		default:
			if node.Leaf || !competitionSupervisorTransparentCause(node.Err) {
				return false
			}
		}
	}
	return found && foundProof && foundPhase
}
