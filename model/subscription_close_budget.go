package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

const forceClosePageBudget = 15 * time.Second
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
			return closed, next, nil
		}
	}
	return closed, next, nil
}
