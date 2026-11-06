// Renewal diagnostics expose completed local boundaries without granting
// replay, extending a lease, or retaining SQL, arguments or cause payloads.
package controller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
)

// Fixed ordinals are source boundaries, not assertions that SQL reached the
// server. A later inner acquisition may fail after an earlier callback step.
type competitionSupervisorRenewalStep uint8

const (
	competitionSupervisorRenewalUnknown competitionSupervisorRenewalStep = iota
	competitionSupervisorRenewalScopeLock
	competitionSupervisorRenewalLeaseRowLock
	competitionSupervisorRenewalAuthority
	competitionSupervisorRenewalLeaseUpdate
	competitionSupervisorRenewalCallbackReturn
)

// Each field is a bounded scalar or a fixed-size observation. A returned
// release can transfer asynchronous cleanup; it is not a physical cleanup join.
// In particular, these counts alone do not grant the worker's no-commit retry.
type CompetitionSupervisorRenewalDatabaseFailure struct {
	Schema                  int                              `json:"schema"`
	Kind                    string                           `json:"kind"`
	DatabasePhase           server.DbOperationPhase          `json:"database_phase"`
	Step                    competitionSupervisorRenewalStep `json:"step"`
	CallbackEntries         uint64                           `json:"callback_entries"`
	ConfirmedLeaseExpiresAt time.Time                        `json:"confirmed_lease_expires_at"`
	AtEntry                 server.DbContextBudget           `json:"at_entry"`
	AtReturn                server.DbContextBudget           `json:"at_return"`
	Acquire                 server.DbAcquireObservation      `json:"acquire"`
	Transaction             server.DbTxObservationSnapshot   `json:"transaction"`
}

// Preserve the original capture and error graph, then snapshot only after all
// database callback, rollback and pool-release ownership has unwound.
func (self *competitionSupervisorRenewObservation) captureContext(ctx context.Context, confirmedExpiry time.Time, run func()) error {
	entered := server.ObserveDbContextBudget(ctx)
	err := self.capture(run)
	proof, ok := err.(competitionSupervisorRenewError)
	if !ok {
		return err
	}
	acquire := proof.acquire
	if acquire.PoolReturn != nil {
		pool := *acquire.PoolReturn
		acquire.PoolReturn = &pool
	}
	proof.database = &CompetitionSupervisorRenewalDatabaseFailure{Schema: 1, Kind: "competition-supervisor-renewal-database/v1",
		DatabasePhase: proof.phase, Step: self.step, CallbackEntries: self.callbacks, ConfirmedLeaseExpiresAt: confirmedExpiry,
		AtEntry: entered, AtReturn: server.ObserveDbContextBudget(ctx), Acquire: acquire, Transaction: self.transaction.Snapshot()}
	return proof
}

// Only the complete production-root error owns this evidence. Searching an
// arbitrary joined/wrapped cause could project one branch as the whole failure.
// Copies cannot mutate the stored error's final pool snapshot or retry proof.
func CompetitionSupervisorRenewalFailureDetails(err error) *CompetitionSupervisorRenewalDatabaseFailure {
	proof, ok := err.(competitionSupervisorRenewError)
	if !ok || proof.database == nil {
		return nil
	}
	copy := *proof.database
	if copy.Acquire.PoolReturn != nil {
		pool := *copy.Acquire.PoolReturn
		copy.Acquire.PoolReturn = &pool
	}
	return &copy
}

// This is the supervisor CLI's actual machine report. Existing state/error
// bytes remain unchanged when no authenticated renewal observation is present.
// Diagnostics are observational only; no new retryable or accepted flag exists.
func MarshalCompetitionSupervisorReport(state string, err error) ([]byte, error) {
	result := struct {
		State           string                                       `json:"state"`
		Error           string                                       `json:"error,omitempty"`
		RenewalDatabase *CompetitionSupervisorRenewalDatabaseFailure `json:"renewal_database,omitempty"`
	}{State: state}
	if err != nil {
		result.Error = err.Error()
		result.RenewalDatabase = CompetitionSupervisorRenewalFailureDetails(err)
	}
	return json.Marshal(result)
}
