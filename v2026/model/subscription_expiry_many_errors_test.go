// Completed raw pages retain every independent row failure across all wrappers.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Revalidation must observe the current graph behind a retained row error.
type expiryVisitMutableCause struct{ cause error }

func (*expiryVisitMutableCause) Error() string      { return "synthetic mutable row failure" }
func (self *expiryVisitMutableCause) Unwrap() error { return self.cause }

// A fully visited two-stream raw page can contain more failed rows than one
// cause graph's node budget. Each lane and the elapsed-budget wrapper must
// preserve the exact receipt without changing prior task arguments or counts.
func TestExpiryManyCompletedVisitsKeepReceiptAcrossBudgetAndLanes(t *testing.T) {
	causes := make([]error, 512)
	for index := range causes {
		causes[index] = fmt.Errorf("synthetic completed row %d: %w", index, errors.New("synthetic proof failure"))
	}
	batch := server.NewErrorCauseBatch(causes)
	failure := &ForceCloseVisitError{cause: batch, attemptedCloseCount: 0, complete: true}
	if !failure.CanCheckpoint() || server.InspectErrorCauses(failure).Complete {
		t.Fatal("completed rows were subject to a whole-page cause budget")
	}
	epoch := time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC)
	before := &ContractExpiryCursor{ScanBefore: epoch, Open: &ContractExpiryPosition{CreateTime: epoch.Add(-time.Hour), ContractId: expiryFairTestId(1)}}
	middle := &ContractExpiryCursor{ScanBefore: epoch, Open: &ContractExpiryPosition{CreateTime: epoch.Add(-time.Minute), ContractId: expiryFairTestId(2)}}
	advanced := &ContractExpiryCursor{ScanBefore: epoch, OpenDone: true}
	pages := 0
	count, next, err := forceCloseContractPagesBudgeted(t.Context(), 2, before, time.Minute, 1, func() time.Time { return epoch },
		func(int, *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			pages++
			if pages == 1 {
				return 1, middle, nil
			}
			return 0, advanced, failure
		})
	visited, ok := err.(*ForceCloseVisitError)
	if !ok || pages != 2 || count != 1 || next != advanced || !visited.CanCheckpoint() ||
		visited.AttemptedCloseCount() != 1 || visited.cause != batch || failure.AttemptedCloseCount() != 0 {
		t.Fatal("budget continuation lost the exact completed rows or immutable count")
	}
	cutoff := epoch.Add(72 * time.Hour)
	for _, lane := range []string{"historical", "recent", "fresh", "catchup"} {
		after := &ContractExpirySweepCursor{Historical: before, RecentAfter: epoch, FreshBefore: cutoff}
		switch lane {
		case "historical":
			after.HistoricalNext = true
		case "recent":
			after.Recent = before
		case "fresh":
			after.Fresh, after.FreshNext = before, true
		case "catchup":
			after.Catchup, after.CatchupTurn = before, 2
		}
		count, next, err := forceCloseContractExpiryFreshPage(cutoff, cutoff.Add(time.Hour), after,
			func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 0, advanced, failure })
		if count != 0 || err != failure || next == nil {
			t.Fatal("completed many-row failure lost its lane", lane)
		}
		var actual *ContractExpiryCursor
		switch lane {
		case "historical":
			actual = next.Historical
		case "recent":
			actual = next.Recent
		case "fresh":
			actual = next.Fresh
		case "catchup":
			actual = next.Catchup
		}
		if actual != advanced || after.Historical != before {
			t.Fatal("many-row failure pinned its completed lane or mutated prior args", lane)
		}
	}
}

// Independent budgets never relax cancellation, runtime interruption, malformed
// per-row graphs or late mutation; the last row must be checked as well.
func TestExpiryManyCompletedVisitsRejectLateIncompleteMember(t *testing.T) {
	leaf := errors.New("synthetic completed row failure")
	mutable := &expiryVisitMutableCause{cause: leaf}
	causes := make([]error, 512)
	for index := range causes {
		causes[index] = leaf
	}
	causes[len(causes)-1] = mutable
	failure := &ForceCloseVisitError{cause: server.NewErrorCauseBatch(causes), complete: true}
	if !failure.CanCheckpoint() {
		t.Fatal("complete original receipt was refused")
	}
	wide := make([]error, 128)
	for index := range wide {
		wide[index] = leaf
	}
	for _, invalid := range []error{nil, context.Canceled, context.DeadlineExceeded, server.DbContextDoneError,
		&expiryVisitRuntimePanic{}, &expiryVisitPanicCause{}, errors.Join(wide...), mutable,
		server.NewErrorCauseBatch([]error{leaf})} {
		mutable.cause = invalid
		if failure.CanCheckpoint() || forceClosePageCanAdvance(failure) {
			t.Fatal("late malformed or interrupted row acquired completed-page progress")
		}
	}
	mutable.cause = leaf
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	before, after := &ContractExpiryCursor{}, &ContractExpiryCursor{OpenDone: true}
	_, cursor, err := forceCloseContractPagesBudgeted(ctx, 1, before, time.Minute, 1, time.Now,
		func(int, *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			cancel()
			return 0, after, failure
		})
	if cursor != before || err == nil {
		t.Fatal("parent cancellation retained a many-row completed checkpoint")
	}
}
