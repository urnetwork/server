// The exact read-only heartbeat prefix may continue only after acknowledged
// rollback and returned pool custody. No persisted diagnostic grants authority.
package controller

import "github.com/urnetwork/server/v2026"

// This is a fixed source shape, not a tunable number of successful statements.
const competitionHeartbeatJobLockPrefixLength = 3

// Prior failed acquisitions cannot hide an admitted callback: cumulative
// success/BEGIN/callback/release counts must all describe the single last one.
// Only scope lock, slot FOR UPDATE and job FOR UPDATE have run. No authority
// setup, UPDATE, manual operation or COMMIT is permitted in this new class.
// DbTxReleaseIdle describes protocol-idle release eligibility; pool lifetime
// expiry may still transfer physical destruction. No cleanup join is inferred.
func competitionHeartbeatJobLockDeadline(err error) bool {
	proof, ok := err.(competitionHeartbeatDatabaseError)
	if !ok {
		return false
	}
	snapshot := proof.snapshot
	transaction := snapshot.Transaction
	if snapshot.SqlShape != competitionHeartbeatLegacySlotShape || !snapshot.SqlShapeValid || snapshot.SqlStatements != competitionHeartbeatJobLockPrefixLength ||
		snapshot.CallbackEntries != 1 || snapshot.Step != competitionHeartbeatDatabaseJobLock || snapshot.UnknownStatements != 0 ||
		transaction.AcquireAttempts == 0 || transaction.AcquireAttempts != snapshot.Acquire.Attempt || transaction.AcquireSucceeded != 1 ||
		transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 || transaction.CallbackEntered != 1 ||
		transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 || transaction.LastCommit != (server.DbTxBoundaryObservation{}) ||
		transaction.RollbackAttempts != 1 || transaction.RollbackAcknowledged != 1 || !transaction.LastRollback.Returned || !transaction.LastBegin.Returned ||
		transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 || transaction.LastRelease.Disposition != server.DbTxReleaseIdle ||
		!competitionHeartbeatLiveBudget(snapshot.AtEntry) || snapshot.AtReturn.State != "deadline" || !snapshot.AtReturn.HasDeadline || snapshot.AtReturn.RemainingNs > 0 ||
		!snapshot.Acquire.Returned || !snapshot.Acquire.Admitted || !competitionHeartbeatLiveBudget(snapshot.Acquire.BeforeAcquire) || !competitionHeartbeatLiveBudget(snapshot.Acquire.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastBegin.Before) || !competitionHeartbeatLiveBudget(transaction.LastBegin.AtReturn) ||
		!competitionHeartbeatLiveBudget(transaction.LastRollback.Before) || !competitionHeartbeatLiveBudget(transaction.LastRollback.AtReturn) {
		return false
	}
	if len(snapshot.Statements) != competitionHeartbeatJobLockPrefixLength {
		return false
	}
	expected := competitionHeartbeatLegacySlotStatements()[:competitionHeartbeatJobLockPrefixLength]
	for index, want := range expected {
		actual := snapshot.Statements[index]
		if actual.Step != want.step || actual.Ordinal != 1 || actual.Kind != want.kind || actual.Count != 1 ||
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
