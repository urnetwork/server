// A supervised invocation may retain one failed periodic fence owner. Facts
// are diagnostic only; no error wrapper, retry permission or global is added.
package controller

import (
	"context"
	"reflect"

	"github.com/urnetwork/server/v2026"
)

// This closed projection excludes store errors, query text and arguments.
// Budgets describe the selected owner return before its deferred cancellation.
type CompetitionWorkerFenceOwnerReport struct {
	Schema                     int                    `json:"schema"`
	Boundary                   uint8                  `json:"boundary"`
	StoreCalls                 uint64                 `json:"store_calls"`
	LastStoreOutcome           uint8                  `json:"last_store_outcome"`
	ParentAtEntry              server.DbContextBudget `json:"parent_at_entry"`
	ParentAtReturn             server.DbContextBudget `json:"parent_at_return"`
	TotalAtReturn              server.DbContextBudget `json:"total_at_return"`
	AttemptAtEntry             server.DbContextBudget `json:"attempt_at_entry"`
	AttemptAtReturn            server.DbContextBudget `json:"attempt_at_return"`
	HasConfirmedLeaseExpiry    bool                   `json:"has_confirmed_lease_expiry"`
	ConfirmedLeaseExpiryUnixNs int64                  `json:"confirmed_lease_expiry_unix_ns"`
}

// Owned by one supervised call. Only its joined fence wrapper publishes; the
// caller reads after that call returns. Reuse or concurrent access is unsupported.
type CompetitionWorkerFailureObservation struct {
	workerFenceCause error
	workerFenceOwner *CompetitionWorkerFenceOwnerReport
}

// This private key cannot be supplied by an encoded request or persisted row.
type competitionWorkerFailureObservationKey struct{}

// A context value preserves the exact parent's cancellation, clock and cause.
// The caller creates one collector per supervised invocation, never per retry.
func ObserveCompetitionWorkerFailure(ctx context.Context) (context.Context, *CompetitionWorkerFailureObservation) {
	observation := &CompetitionWorkerFailureObservation{}
	return context.WithValue(ctx, competitionWorkerFailureObservationKey{}, observation), observation
}

// Exact comparable identity never relies on inspection methods or private text.
// An uncomparable original simply retains the legacy diagnostic projection.
func sameCompetitionWorkerFailureIdentity(first, second error) bool {
	return first != nil && second != nil && reflect.TypeOf(first) == reflect.TypeOf(second) && reflect.ValueOf(first).Comparable() && reflect.ValueOf(second).Comparable() && first == second
}

// Called only after the worker and reader have joined, on a selected authority
// cause return. A direct run error can never borrow a completed fence check.
func observeCompetitionWorkerFenceReturn(observation *CompetitionWorkerFailureObservation, returned, fenceError error, owner *competitionHeartbeatOwnerObservation) error {
	// Both the independent watchdog and the retry owner can return this exact
	// singleton. Equality cannot establish which won; retain legacy facts only.
	if observation == nil || owner == nil || returned == ErrLeaseLost || !sameCompetitionWorkerFailureIdentity(returned, fenceError) {
		return returned
	}
	value := owner.snapshotCopy()
	if value.Boundary == competitionHeartbeatOwnerUnknown || value.Boundary >= competitionHeartbeatOwnerSuccess {
		return returned
	}
	observation.workerFenceCause = returned
	observation.workerFenceOwner = &CompetitionWorkerFenceOwnerReport{Schema: 1, Boundary: uint8(value.Boundary), StoreCalls: value.StoreCalls,
		LastStoreOutcome: uint8(value.LastStoreOutcome), ParentAtEntry: value.ParentAtEntry, ParentAtReturn: value.ParentAtReturn,
		TotalAtReturn: value.TotalAtReturn, AttemptAtEntry: value.AttemptAtEntry, AttemptAtReturn: value.AttemptAtReturn,
		HasConfirmedLeaseExpiry: value.HasConfirmedJobExpiry, ConfirmedLeaseExpiryUnixNs: value.ConfirmedJobExpiryUnixNs}
	return returned
}

// A report for another error, even an equivalent wrapper, stays legacy. The
// detached value has no nested pointer that could mutate retained facts.
func (self *CompetitionWorkerFailureObservation) workerFenceSnapshot(err error) *CompetitionWorkerFenceOwnerReport {
	if self == nil || self.workerFenceOwner == nil || !sameCompetitionWorkerFailureIdentity(err, self.workerFenceCause) {
		return nil
	}
	value := *self.workerFenceOwner
	if !validCompetitionWorkerFenceOwnerReport(&value) {
		return nil
	}
	return &value
}

// Unknown enum/text values never cross the trusted child's closed schema.
func validCompetitionWorkerFenceOwnerReport(value *CompetitionWorkerFenceOwnerReport) bool {
	if value == nil {
		return true
	}
	if value.Schema != 1 || value.Boundary == uint8(competitionHeartbeatOwnerUnknown) || value.Boundary >= uint8(competitionHeartbeatOwnerSuccess) || value.LastStoreOutcome > uint8(competitionHeartbeatStoreFailed) ||
		value.StoreCalls > uint64(defaultCompetitionSupervisorWorkerFenceSettings().Retry.LockRetry.MaximumAttempts) ||
		value.StoreCalls == 0 && value.LastStoreOutcome != uint8(competitionHeartbeatStoreNotCalled) || value.StoreCalls > 0 && value.LastStoreOutcome < uint8(competitionHeartbeatStoreSucceeded) ||
		!value.HasConfirmedLeaseExpiry && value.ConfirmedLeaseExpiryUnixNs != 0 {
		return false
	}
	for _, budget := range []server.DbContextBudget{value.ParentAtEntry, value.ParentAtReturn, value.TotalAtReturn, value.AttemptAtEntry, value.AttemptAtReturn} {
		switch budget.State {
		case "", "live", "deadline", "canceled", "other":
		default:
			return false
		}
	}
	return true
}
