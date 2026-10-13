// A busy balance owner defers its rows without ending the walk, and a pass that
// deferred a row ends at its backlog so the next pass revisits that row.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The receipt of a fully visited subpage whose failed rows were all refused
// because another owner held their transfer balance.
func busyWalkReceipt(count int64, busyRows int, progressed bool) *ForceCloseVisitError {
	causes := make([]error, busyRows)
	ids := make([]server.Id, busyRows)
	for index := range busyRows {
		ids[index] = server.NewId()
		causes[index] = &forceCloseRowError{contractId: ids[index], index: index, cause: errTransferBalanceOwnershipBusy}
	}
	return &ForceCloseVisitError{cause: server.NewErrorCauseBatch(causes), attemptedCloseCount: count, complete: true,
		progressed: progressed, deferredContractIds: ids}
}

// The same with one row-local deadline instead, the other deferral class.
func deadlineWalkReceipt(count int64) *ForceCloseVisitError {
	id := server.NewId()
	return &ForceCloseVisitError{
		cause:               server.NewErrorCauseBatch([]error{&forceCloseRowError{contractId: id, index: 0, cause: context.DeadlineExceeded}}),
		attemptedCloseCount: count, complete: true, progressed: true, deferredContractIds: []server.Id{id},
	}
}

type busyWalkStep struct {
	count int64
	next  *int
	err   error
}

type busyWalkResult struct {
	closed int64
	next   *int
	err    error
	pages  int
}

// Runs the production dispatcher over scripted subpages. Each subpage advances
// the clock by step, so the elapsed budget is crossed deterministically.
func runBusyWalk(t *testing.T, steps []busyWalkStep, step time.Duration) busyWalkResult {
	t.Helper()
	clock := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	start := 0
	result := busyWalkResult{}
	result.closed, result.next, result.err = forceCloseContractPagesDispatched(context.Background(), 64*forceCloseRawSubpageSize, &start,
		forceClosePageBudget, forceClosePageDispatchLimit, forceCloseRawSubpageSize, func() time.Time { return clock },
		func(size int, cursor *int, dispatch func() bool) (int64, *int, error) {
			if len(steps) <= result.pages {
				t.Fatal("the walk read past its scripted pass")
			}
			current := steps[result.pages]
			result.pages += 1
			clock = clock.Add(step)
			return current.count, current.next, current.err
		})
	return result
}

func busyWalkCursor(value int) *int {
	return &value
}

// The production failure shape: one busy row in a moving page ended every run.
// The walk must defer it and continue to the end of its pass.
func TestForceCloseWalkContinuesPastBusyDeferrals(t *testing.T) {
	result := runBusyWalk(t, []busyWalkStep{
		{count: 10, next: busyWalkCursor(1), err: busyWalkReceipt(10, 2, true)},
		{count: 20, next: busyWalkCursor(2)},
		{count: 5, next: busyWalkCursor(3), err: busyWalkReceipt(5, 1, true)},
		{count: 7, next: nil},
	}, time.Second)
	visited, ok := result.err.(*ForceCloseVisitError)
	if result.pages != 4 || result.closed != 42 || result.next != nil {
		t.Fatal("a busy deferral ended the walk", result.pages, result.closed, result.next)
	}
	if !ok || !visited.CanCheckpoint() || visited.AttemptedCloseCount() != 42 || len(visited.DeferredContractIds()) != 3 || !visited.Progressed() {
		t.Fatal("the walked deferrals lost their complete receipt", result.err)
	}
}

// Any other failure after deferrals stops the walk just before that subpage,
// whose ordinary outcome the next run meets first.
func TestForceCloseWalkStopsBeforeOtherFailureAfterBusyDeferrals(t *testing.T) {
	result := runBusyWalk(t, []busyWalkStep{
		{count: 10, next: busyWalkCursor(1), err: busyWalkReceipt(10, 1, true)},
		{count: 7, next: busyWalkCursor(2), err: deadlineWalkReceipt(7)},
	}, time.Second)
	visited, ok := result.err.(*ForceCloseVisitError)
	if result.pages != 2 || result.closed != 10 || result.next == nil || *result.next != 1 {
		t.Fatal("the walk did not stop just before the other failure", result.pages, result.closed, result.next)
	}
	if !ok || !visited.CanCheckpoint() || visited.AttemptedCloseCount() != 10 || len(visited.DeferredContractIds()) != 1 {
		t.Fatal("the walked deferrals lost their receipt", result.err)
	}
}

// A subpage where every row was busy proves no progress; the walk stops before it.
func TestForceCloseWalkStopsBeforePageWithoutProgress(t *testing.T) {
	result := runBusyWalk(t, []busyWalkStep{
		{count: 10, next: busyWalkCursor(1), err: busyWalkReceipt(10, 1, true)},
		{count: 4, next: busyWalkCursor(2), err: busyWalkReceipt(4, 4, false)},
	}, time.Second)
	if result.pages != 2 || result.closed != 10 || result.next == nil || *result.next != 1 {
		t.Fatal("the walk did not stop just before the page without progress", result.pages, result.closed, result.next)
	}
}

// Deferrals merge into one cause batch; the walk ends before it could overflow.
func TestForceCloseWalkBoundsMergedDeferrals(t *testing.T) {
	steps := []busyWalkStep{}
	for index := range 4 {
		steps = append(steps, busyWalkStep{count: 256, next: busyWalkCursor(index + 1), err: busyWalkReceipt(256, 200, true)})
	}
	result := runBusyWalk(t, steps, time.Second)
	visited, ok := result.err.(*ForceCloseVisitError)
	if result.pages != 2 || result.closed != 512 || result.next == nil || *result.next != 2 {
		t.Fatal("the walk did not end at its deferral bound", result.pages, result.closed, result.next)
	}
	if !ok || !visited.CanCheckpoint() || len(visited.DeferredContractIds()) != 400 {
		t.Fatal("the bounded deferrals lost their complete receipt", result.err)
	}
}

// Busy subpages still respect the elapsed budget between subpages.
func TestForceCloseWalkStopsOnBudgetAfterBusyDeferrals(t *testing.T) {
	result := runBusyWalk(t, []busyWalkStep{
		{count: 10, next: busyWalkCursor(1), err: busyWalkReceipt(10, 1, true)},
		{count: 10, next: busyWalkCursor(2), err: busyWalkReceipt(10, 1, true)},
		{count: 10, next: busyWalkCursor(3)},
	}, forceClosePageBudget/2)
	if result.pages != 2 || result.closed != 20 || result.next == nil || *result.next != 2 {
		t.Fatal("the walk ran past its elapsed budget", result.pages, result.closed, result.next)
	}
}

// Without busy deferrals a failing subpage keeps today's outcome exactly.
func TestForceCloseWalkKeepsOtherFailureOutcome(t *testing.T) {
	receipt := deadlineWalkReceipt(7)
	result := runBusyWalk(t, []busyWalkStep{
		{count: 7, next: busyWalkCursor(1), err: receipt},
		{count: 10, next: nil},
	}, time.Second)
	visited, ok := result.err.(*ForceCloseVisitError)
	if result.pages != 1 || result.closed != 7 || result.next == nil || *result.next != 1 || !ok ||
		visited.AttemptedCloseCount() != 7 || len(visited.DeferredContractIds()) != 1 {
		t.Fatal("a failure without busy deferrals changed its outcome", result.pages, result.closed, result.err)
	}
}

// A pass whose historical lane is done and whose recent lane is about to
// finish, while its fresh lane still has work. Synthetic times.
func busyWalkBacklogTail(now time.Time) *ContractExpirySweepCursor {
	epoch := now.Add(-2 * time.Hour)
	return &ContractExpirySweepCursor{
		RecentAfter:    epoch,
		HistoricalDone: true,
		Recent: &ContractExpiryCursor{ScanBefore: now.Add(-15 * time.Minute), DisputeDone: true,
			Open: &ContractExpiryPosition{CreateTime: now.Add(-20 * time.Minute), ContractId: server.NewId()}},
		Fresh: &ContractExpiryCursor{ScanBefore: now.Add(-13 * time.Minute), DisputeDone: true,
			Open: &ContractExpiryPosition{CreateTime: now.Add(-30 * time.Minute), ContractId: server.NewId()}},
		FreshBefore: now.Add(-70 * time.Minute),
	}
}

// The recent lane finishes its pass on a subpage that deferred a busy row. The
// row sits behind every backlog cursor, so the pass must end for the next one
// to revisit it, instead of circling its fresh and catch-up windows.
func TestContractExpirySweepEndsDeferredPassAtBacklog(t *testing.T) {
	now := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	state := busyWalkBacklogTail(now)
	_, next, err := forceCloseContractExpiryFreshPage(now.Add(-12*time.Minute), now, state,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != state.Recent {
				t.Fatal("the subpage did not visit the recent lane", position)
			}
			return 3, nil, busyWalkReceipt(3, 1, true)
		})
	if err == nil || next != nil {
		t.Fatal("the pass that deferred a row kept circling after its backlog", next)
	}
}

// Without a deferral the same subpage keeps the pass and its fresh lane.
func TestContractExpirySweepKeepsPassWithoutDeferralsAtBacklog(t *testing.T) {
	now := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	state := busyWalkBacklogTail(now)
	_, next, err := forceCloseContractExpiryFreshPage(now.Add(-12*time.Minute), now, state,
		func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 3, nil, nil })
	if err != nil || next == nil || !next.BacklogDone || next.Fresh == nil {
		t.Fatal("a pass without deferrals ended at its backlog", next, err)
	}
}
