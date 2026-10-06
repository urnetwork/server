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

// ForceCloseOpenContractIdsBudgetedPage checkpoints complete raw subpages before
// the task deadline. The elapsed budget stops new subpages; it does not cancel
// in-flight closes, whose required payout posts follow their outcome commit.
// One complete subpage may overrun the budget. Parent cancellation stays an error.
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
	return forceCloseContractPagesBudgeted(ctx, maxCount, after, budget, subpageSize, time.Now,
		func(size int, cursor *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			return ForceCloseOpenContractIdsPage(ctx, minTime, size, parallel, blockSize, blockIndex, cursor)
		})
}

// Both scan policies share the same row/time budget and completed-page error
// boundary. The callback owns selection; financial errors retain their meaning.
func forceCloseContractPagesBudgeted[Cursor any](ctx context.Context, maxCount int, after *Cursor,
	budget time.Duration, subpageSize int, now func() time.Time, page func(int, *Cursor) (int64, *Cursor, error),
) (closed int64, next *Cursor, returnErr error) {
	if maxCount <= 0 || budget <= 0 || subpageSize <= 0 {
		return 0, after, fmt.Errorf("invalid force close page budget")
	}
	budgetEnd := now().Add(budget)
	next = after
	for remaining := maxCount; remaining > 0; {
		size := min(remaining, subpageSize)
		var count int64
		var cursor *Cursor
		var pageErr error
		server.HandleError(func() {
			count, cursor, pageErr = page(size, next)
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
