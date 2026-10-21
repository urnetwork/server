// Query-result causes preserve observation uncertainty separately from authority.
package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Do not recursively inspect the deliberately cyclic original in assertions.
func providerBoundaryCauseTestHas(err, target error) bool {
	for _, node := range InspectErrorCauses(err).Nodes {
		if node.Err == target {
			return true
		}
	}
	return false
}

// Failed cause observation never becomes missing schema or permission to retry.
func TestProviderBoundaryIncompleteCauseCensusStaysUnavailable(t *testing.T) {
	cycle := &errorCauseTestOne{}
	cycle.cause = cycle
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = context.DeadlineExceeded
	}
	for index, cause := range []error{cycle, errorCauseTestDepth(40, context.DeadlineExceeded),
		&errorCauseTestMany{causes: wide}, &errorCauseTestMany{causes: []error{nil, nil}}, &errorCauseTestOne{}} {
		present, err := requireProviderPayoutBoundarySchema(t.Context(), providerBoundaryErrorQuery{err: cause}, true)
		if present || !providerBoundaryCauseTestHas(err, ErrProviderEarningBoundaryUnavailable) ||
			!providerBoundaryCauseTestHas(err, cause) || providerBoundaryCauseTestHas(err, ErrProviderEarningBoundarySchema) ||
			ProviderEarningBoundaryRetryable(t.Context(), err) {
			t.Fatal("unknown payout-boundary cause became authority or retry", index)
		}
	}
	matcher := &errorCauseTestMatch{}
	err := providerBoundaryObservationError(matcher)
	if !providerBoundaryCauseTestHas(err, ErrProviderEarningBoundaryUnavailable) || !ProviderEarningBoundaryRetryable(t.Context(), err) {
		t.Fatal("complete unknown observation lost existing unavailable retry policy")
	}
}

// A positive schema/identity observation remains hard beside incomplete causes.
func TestProviderBoundaryObservedHardCauseSurvivesCyclicSibling(t *testing.T) {
	cycle := &errorCauseTestOne{}
	cycle.cause = cycle
	for _, item := range []struct{ cause, sentinel error }{
		{cause: ErrProviderEarningBoundaryMismatch, sentinel: ErrProviderEarningBoundaryMismatch},
		{cause: ErrProviderEarningBoundaryUnprepared, sentinel: ErrProviderEarningBoundaryUnprepared},
		{cause: ErrProviderEarningBoundarySchema, sentinel: ErrProviderEarningBoundarySchema},
		{cause: &pgconn.PgError{Code: "42501"}, sentinel: ErrProviderEarningBoundarySchema},
	} {
		original := &errorCauseTestMany{causes: []error{cycle, item.cause}}
		_, err := readProviderPayoutBoundary(t.Context(), providerBoundaryErrorQuery{err: original})
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok || len(joined.Unwrap()) != 2 || joined.Unwrap()[0] != item.sentinel || joined.Unwrap()[1] != original ||
			ProviderEarningBoundaryRetryable(t.Context(), err) {
			t.Fatal("observed hard cause lost sentinel or original error custody")
		}
	}
}

// NoRows is absence only when every completed leaf establishes that fact.
func TestProviderBoundaryAbsentReadRequiresCompleteExclusiveNoRows(t *testing.T) {
	for _, cause := range []error{pgx.ErrNoRows, &errorCauseTestOne{cause: pgx.ErrNoRows}, errors.Join(pgx.ErrNoRows, pgx.ErrNoRows)} {
		binding, err := readProviderPayoutBoundary(t.Context(), providerBoundaryErrorQuery{err: cause})
		if binding != nil || err != nil {
			t.Fatal("complete absence was not preserved")
		}
	}
	for index, cause := range []error{errors.Join(pgx.ErrNoRows, context.DeadlineExceeded), sql.ErrNoRows,
		&errorCauseTestMany{causes: []error{nil, nil}}, errorCauseTestDepth(40, pgx.ErrNoRows)} {
		binding, err := readProviderPayoutBoundary(t.Context(), providerBoundaryErrorQuery{err: cause})
		if binding != nil || !providerBoundaryCauseTestHas(err, ErrProviderEarningBoundaryUnavailable) || !providerBoundaryCauseTestHas(err, cause) {
			t.Fatal("failed observation became completed payout-boundary absence", index)
		}
	}
}
