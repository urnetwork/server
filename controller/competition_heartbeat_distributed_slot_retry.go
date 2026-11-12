// The distributed job-before-slot graph has its own exact no-COMMIT proof.
// A failed final slot update cannot commit the preceding job lease update.
package controller

import "github.com/urnetwork/server"

// The original twenty-second owner and both confirmed leases remain bounds.
// Unlike the existing legacy proof, this new class requires acknowledged
// rollback plus a returned idle pool release, never pending destruction.
func competitionHeartbeatDistributedSlotUpdateDeadline(err error) bool {
	return competitionHeartbeatDistributedStatementDeadline(err, competitionHeartbeatDistributedSlotStatements())
}

// Every known failed-last position shares the same returned rollback proof.
// A statement which succeeded before a later callback/commit failure is not
// this class; unknown SQL and incomplete source prefixes remain refusal.
func competitionHeartbeatDistributedReturnedRollbackDeadline(err error) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	expected := competitionHeartbeatDistributedSlotStatements()
	if !ok || proof.snapshot.SqlStatements == 0 || proof.snapshot.SqlStatements > uint64(len(expected)) {
		return false
	}
	return competitionHeartbeatDistributedStatementDeadline(err, expected[:proof.snapshot.SqlStatements])
}

// Compatibility retains the original read/config-only meaning. Job/slot
// updates and a non-final failure are not members of this narrower class,
// even though the generic returned-rollback recognizer covers known updates.
func competitionHeartbeatDistributedPreBusinessDeadline(err error) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	expected := competitionHeartbeatDistributedPreBusinessStatements()
	if !ok || proof.snapshot.SqlStatements == 0 || proof.snapshot.SqlStatements > uint64(len(expected)) {
		return false
	}
	return competitionHeartbeatDistributedStatementDeadline(err, expected[:proof.snapshot.SqlStatements])
}

// Both closed source graphs require the same physically returned transaction,
// live entry budgets, one last deadline and exact unique diagnostic positions.
func competitionHeartbeatDistributedStatementDeadline(err error, expected []competitionHeartbeatSqlStatement) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	if !ok || len(expected) == 0 {
		return false
	}
	snapshot, transaction := proof.snapshot, proof.snapshot.Transaction
	if snapshot.SqlShape != competitionHeartbeatDistributedSlotShape || !snapshot.SqlShapeValid || snapshot.SqlStatements != uint64(len(expected)) || snapshot.CallbackEntries != 1 || snapshot.Step != expected[len(expected)-1].step || snapshot.UnknownStatements != 0 ||
		transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != snapshot.Acquire.Attempt || transaction.AcquireSucceeded != 1 || transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 1 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged != 1 || !transaction.LastRollback.Returned || !transaction.LastBegin.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 || transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
		!competitionHeartbeatLiveBudget(snapshot.AtEntry) || snapshot.AtReturn.State != "deadline" || !snapshot.AtReturn.HasDeadline || snapshot.AtReturn.RemainingNs > 0 ||
		!snapshot.Acquire.Returned || !snapshot.Acquire.Admitted || !competitionHeartbeatLiveBudget(snapshot.Acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(snapshot.Acquire.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastBegin.Before) || !competitionHeartbeatLiveBudget(transaction.LastBegin.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastRollback.Before) || !competitionHeartbeatLiveBudget(transaction.LastRollback.AtReturn) || len(snapshot.Statements) != len(expected) {
		return false
	}
	// Diagnostics retain fixed step order. The independent source SQL trace
	// above proves execution order; authenticate each unique step/ordinal.
	type position struct {
		step    competitionHeartbeatDatabaseStep
		ordinal uint8
	}
	statements := map[position]competitionHeartbeatStatementObservation{}
	for _, actual := range snapshot.Statements {
		key := position{step: actual.Step, ordinal: actual.Ordinal}
		if _, duplicate := statements[key]; duplicate {
			return false
		}
		statements[key] = actual
	}
	ordinals := map[competitionHeartbeatDatabaseStep]uint8{}
	for index, want := range expected {
		ordinals[want.step]++
		actual, present := statements[position{step: want.step, ordinal: ordinals[want.step]}]
		if !present || actual.Kind != want.kind || actual.Count != 1 || !actual.Returned || actual.AtReturn == nil || !competitionHeartbeatLiveBudget(actual.Entered) {
			return false
		}
		if index == len(expected)-1 {
			if actual.Succeeded || actual.AtReturn.State != "deadline" || !actual.AtReturn.HasDeadline || actual.AtReturn.RemainingNs > 0 {
				return false
			}
		} else if !actual.Succeeded || !competitionHeartbeatLiveBudget(*actual.AtReturn) {
			return false
		}
	}
	return competitionHeartbeatDeadlineCausesInPhase(err, false, server.DbOperationCallback)
}
