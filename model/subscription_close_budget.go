package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The singleton sweep waits seconds to minutes between runs for its claim, so
// a short budget left it idle most of the time. One run now works for up to a
// minute and a half, below the dispatch limit and the two-minute duration band.
const forceClosePageBudget = 90 * time.Second
const forceCloseRawSubpageSize = 256

// New rows stop starting this long after a scan began, well inside the
// task's 30-minute ceiling. Running rows finish their financial and cleanup
// phases; the subpage then checkpoints exactly the prefix that started.
const forceClosePageDispatchLimit = 2 * time.Minute

// ForceCloseOpenContractIdsBudgetedPage checkpoints complete raw subpages before
// the task deadline. The elapsed budget stops new subpages; it does not cancel
// in-flight closes, whose required payout posts follow their outcome commit.
// One subpage may overrun the budget; past the dispatch limit it starts no new
// rows and checkpoints the prefix that started. Parent cancellation stays an error.
func ForceCloseOpenContractIdsBudgetedPage(ctx context.Context, minTime time.Time, maxCount, parallel, blockSize, blockIndex int,
	after *ContractExpiryCursor,
) (int64, *ContractExpiryCursor, error) {
	return forceCloseOpenContractIdsBudgetedPage(ctx, minTime, maxCount, parallel, blockSize, blockIndex, after, forceClosePageBudget, forceCloseRawSubpageSize)
}

func forceCloseOpenContractIdsBudgetedPage(ctx context.Context, minTime time.Time, maxCount, parallel, blockSize, blockIndex int,
	after *ContractExpiryCursor, budget time.Duration, subpageSize int,
) (closed int64, next *ContractExpiryCursor, returnErr error) {
	if maxCount <= 0 || parallel <= 0 || budget <= 0 || subpageSize <= 0 {
		return 0, nil, fmt.Errorf("invalid force close page budget")
	}
	return forceCloseContractPagesDispatched(ctx, maxCount, after, budget, forceClosePageDispatchLimit, subpageSize, time.Now,
		func(size int, cursor *ContractExpiryCursor, dispatch func() bool) (int64, *ContractExpiryCursor, error) {
			count, next, _, err := forceCloseOpenContractIdsPageDispatched(ctx, minTime, size, parallel, blockSize, blockIndex, cursor, dispatch)
			return count, next, err
		})
}

// Both scan policies share the same row/time budget and completed-page error
// boundary. The callback owns selection; financial errors retain their meaning.
func forceCloseContractPagesBudgeted[Cursor any](ctx context.Context, maxCount int, after *Cursor,
	budget time.Duration, subpageSize int, now func() time.Time, page func(int, *Cursor) (int64, *Cursor, error),
) (closed int64, next *Cursor, returnErr error) {
	return forceCloseContractPagesDispatched(ctx, maxCount, after, budget, forceClosePageDispatchLimit, subpageSize, now,
		func(size int, cursor *Cursor, _ func() bool) (int64, *Cursor, error) { return page(size, cursor) })
}

// The cooperative budget is checked between subpages. The separate dispatch
// limit also bounds one slow subpage: its callback receives a predicate that
// closes once that limit passes, measured on the same clock from the start.
// A subpage whose only failures are rows another owner held, while its other
// rows moved, does not end the call: those rows stay open behind the cursor and
// the walk continues. The call then ends on its budgets, at the end of its pass,
// once its merged deferrals fill one raw subpage, or just before any other
// failing subpage, which the next call meets first with its ordinary outcome.
func forceCloseContractPagesDispatched[Cursor any](ctx context.Context, maxCount int, after *Cursor,
	budget time.Duration, dispatchLimit time.Duration, subpageSize int, now func() time.Time,
	page func(int, *Cursor, func() bool) (int64, *Cursor, error),
) (closed int64, next *Cursor, returnErr error) {
	if maxCount <= 0 || budget <= 0 || dispatchLimit <= 0 || subpageSize <= 0 {
		return 0, after, fmt.Errorf("invalid force close page budget")
	}
	started := now()
	budgetEnd := started.Add(budget)
	dispatchEnd := started.Add(dispatchLimit)
	dispatch := func() bool { return now().Before(dispatchEnd) }
	next = after
	// The busy row receipts of the walked subpages, within one cause batch.
	var deferredCauses []error
	var deferredContractIds []server.Id
	deferred := func() error {
		if len(deferredCauses) == 0 {
			return nil
		}
		return &ForceCloseVisitError{cause: server.NewErrorCauseBatch(deferredCauses), attemptedCloseCount: closed,
			complete: true, progressed: true, deferredContractIds: deferredContractIds, deferredOnly: true}
	}
	for remaining := maxCount; remaining > 0; {
		size := min(remaining, subpageSize)
		var count int64
		var cursor *Cursor
		var pageErr error
		server.HandleError(func() {
			count, cursor, pageErr = page(size, next, dispatch)
		}, func(err error) { pageErr = errors.Join(pageErr, err) })
		if ctx.Err() != nil {
			// Parent cancellation is never a normal page yield, even after a
			// completed prefix or at the same instant as the elapsed budget.
			return closed + count, next, errors.Join(pageErr, ctx.Err())
		}
		if visited, ok := pageErr.(*ForceCloseVisitError); ok && visited.CanCheckpoint() && visited.AttemptedCloseCount() == count &&
			visited.Progressed() && visited.ownershipBusyOnly() && len(deferredCauses) <= forceCloseRawSubpageSize {
			// One more subpage keeps the merge within the batch's two subpages.
			deferredCauses = append(deferredCauses, visited.cause.(*server.ErrorCauseBatch).Unwrap()...)
			deferredContractIds = append(deferredContractIds, visited.deferredContractIds...)
			closed += count
			next = cursor
			remaining -= size
			if next == nil || !now().Before(budgetEnd) || forceCloseRawSubpageSize < len(deferredCauses) {
				return closed, next, deferred()
			}
			continue
		}
		if pageErr != nil && 0 < len(deferredCauses) {
			// Report only the walked subpages. This one's committed rows stay
			// committed, but its cursor is not taken; the next call revisits it.
			return closed, next, deferred()
		}
		if pageErr != nil {
			if accounting, ok := pageErr.(*ForceCloseAccountingError); ok {
				// Prior successful subpages contain only verified closes and
				// clean delegated/skipped visits. Preserve the current page's
				// explicit accounting error and fully classified retry cursor.
				return closed + count, cursor, &ForceCloseAccountingError{
					cause:                               accounting,
					verifiedCloseCount:                  closed + accounting.VerifiedCloseCount(),
					accountingRejectionCount:            accounting.AccountingRejectionCount(),
					quarantinedAccountingRejectionCount: accounting.QuarantinedAccountingRejectionCount(),
				}
			}
			if visited, ok := pageErr.(*ForceCloseVisitError); ok && visited.CanCheckpoint() && visited.AttemptedCloseCount() == count {
				// Prior successful subpages add only their count. Preserve the
				// exact completed-row receipt without adding another cause graph.
				// They also prove that this call already moved the raw cursor.
				progress := *visited
				progress.attemptedCloseCount += closed
				progress.progressed = progress.progressed || remaining < maxCount
				return closed + count, cursor, &progress
			}
			return closed + count, next, pageErr
		}
		closed += count
		next = cursor
		remaining -= size
		if next == nil || !now().Before(budgetEnd) {
			return closed, next, deferred()
		}
	}
	return closed, next, deferred()
}
