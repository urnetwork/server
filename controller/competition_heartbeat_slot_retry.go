// A single reviewed job-heartbeat UPDATE failure may continue after an owned
// deadline, never after COMMIT or unknown work. Cleanup remains pool-owned.
package controller

import "github.com/urnetwork/server"

// This is source-owned current-main evidence, not a persisted event, phase
// label, database error category, or another store's retry assertion.
func competitionHeartbeatSlotUpdateDeadline(err error) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	if !ok {
		return false
	}
	snapshot := proof.snapshot
	transaction := snapshot.Transaction
	if snapshot.SqlShape != competitionHeartbeatLegacySlotShape || !snapshot.SqlShapeValid || snapshot.SqlStatements != uint64(len(competitionHeartbeatLegacySlotStatements())) ||
		snapshot.CallbackEntries != 1 || snapshot.Step != competitionHeartbeatDatabaseSlotUpdate || snapshot.UnknownStatements != 0 ||
		transaction.AcquireAttempts != 1 || transaction.AcquireSucceeded != 1 || transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 1 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged > 1 || !transaction.LastRollback.Returned || !transaction.LastBegin.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 ||
		!competitionHeartbeatLiveBudget(snapshot.AtEntry) || snapshot.AtReturn.State != "deadline" || !snapshot.AtReturn.HasDeadline || snapshot.AtReturn.RemainingNs > 0 ||
		snapshot.Acquire.Attempt != 1 || !snapshot.Acquire.Returned || !snapshot.Acquire.Admitted || !competitionHeartbeatLiveBudget(snapshot.Acquire.BeforeAcquire) ||
		!competitionHeartbeatLiveBudget(snapshot.Acquire.AtReturn) || !competitionHeartbeatLiveBudget(transaction.LastBegin.Before) || !competitionHeartbeatLiveBudget(transaction.LastBegin.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastRollback.Before) || !competitionHeartbeatLiveBudget(transaction.LastRollback.AtReturn) {
		return false
	}
	switch transaction.LastRelease.Disposition {
	case server.DbTxReleaseIdle:
		if transaction.RollbackAcknowledged != 1 {
			return false
		}
	case server.DbTxReleaseDestroyTransferred:
		// A returned error can leave physical cleanup pending. The exact
		// no-hook pool retains its bounded destructor and capacity ownership.
	default:
		return false
	}
	expected := competitionHeartbeatLegacySlotStatements()
	if len(snapshot.Statements) != len(expected) {
		return false
	}
	ordinals := map[competitionHeartbeatDatabaseStep]uint8{}
	for index, want := range expected {
		ordinals[want.step]++
		actual := snapshot.Statements[index]
		if actual.Step != want.step || actual.Ordinal != ordinals[want.step] || actual.Kind != want.kind || actual.Count != 1 ||
			!actual.Returned || actual.AtReturn == nil || !competitionHeartbeatLiveBudget(actual.Entered) {
			return false
		}
		if index != len(expected)-1 {
			if !actual.Succeeded || !competitionHeartbeatLiveBudget(*actual.AtReturn) {
				return false
			}
		} else if actual.Succeeded || actual.AtReturn.State != "deadline" || !actual.AtReturn.HasDeadline || actual.AtReturn.RemainingNs > 0 {
			return false
		}
	}
	return competitionHeartbeatDeadlineCausesInPhase(err, false, server.DbOperationCallback)
}

// Required samples belong to the actual caller or detached bounded cleanup,
// never inferred from error text or the last SQL-bearing semantic step.
func competitionHeartbeatLiveBudget(value server.DbContextBudget) bool {
	return value.State == "live" && value.HasDeadline && value.RemainingNs > 0
}
