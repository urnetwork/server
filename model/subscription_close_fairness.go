// Historical and post-epoch expiry pages share one bounded worker budget.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The historical pass retains its original epoch and cursor. The other pass
// repeatedly visits everything created after that epoch, including active rows
// skipped on earlier visits. Its lower bound never slides with wall-clock age.
// Each component has a fixed upper bound, so new arrivals cannot extend a pass.
type ContractExpirySweepCursor struct {
	Historical     *ContractExpiryCursor `json:"historical,omitempty"`
	Recent         *ContractExpiryCursor `json:"recent,omitempty"`
	RecentAfter    time.Time             `json:"recent_after"`
	HistoricalDone bool                  `json:"historical_done,omitempty"`
	HistoricalNext bool                  `json:"historical_next,omitempty"`
	Fresh          *ContractExpiryCursor `json:"fresh,omitempty"`
	FreshBefore    time.Time             `json:"fresh_before,omitzero"`
	FreshNext      bool                  `json:"fresh_next,omitempty"`
	BacklogDone    bool                  `json:"backlog_done,omitempty"`
}

// Alternate complete subpages, not independent workers. A slow subpage keeps
// its required financial posts and hands the next task run to the other lane.
// Total raw-row limits, parallelism and the cooperative time budget are shared.
func ForceCloseOpenContractIdsFairPage(ctx context.Context, minTime time.Time, maxCount, parallel, blockSize, blockIndex int,
	after *ContractExpirySweepCursor,
) (int64, *ContractExpirySweepCursor, error) {
	if maxCount <= 0 || parallel <= 0 {
		return 0, nil, fmt.Errorf("invalid force close page budget")
	}
	return forceCloseContractPagesBudgeted(ctx, maxCount, after, forceClosePageBudget, forceCloseRawSubpageSize, time.Now,
		func(size int, cursor *ContractExpirySweepCursor) (int64, *ContractExpirySweepCursor, error) {
			return forceCloseContractExpiryFreshPage(minTime, server.NowUtc(), cursor,
				func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
					return ForceCloseOpenContractIdsPage(ctx, minTime, size, parallel, blockSize, blockIndex, position)
				})
		})
}

// A page can advance only after success or a fully classified accounting
// refusal. The shared budget wrapper rejects cancellation and all other errors.
func forceCloseContractExpirySweepPage(minTime, now time.Time, after *ContractExpirySweepCursor,
	page func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error),
) (int64, *ContractExpirySweepCursor, error) {
	next := &ContractExpirySweepCursor{}
	if after != nil {
		*next = *after
	}
	if next.Historical == nil && !next.HistoricalDone {
		next.Historical = &ContractExpiryCursor{ScanBefore: now}
	}
	if next.RecentAfter.IsZero() {
		// A legacy task enters here with its exact persisted historical
		// cursor; do not reset its position or replace its original epoch.
		if next.Historical == nil || next.Historical.ScanBefore.IsZero() {
			return 0, after, fmt.Errorf("invalid historical expiry epoch")
		}
		next.RecentAfter = next.Historical.ScanBefore
	}
	historical := next.HistoricalNext && !next.HistoricalDone
	position := next.Historical
	if !historical {
		position = next.Recent
		if position == nil {
			// The old pass includes every id at its epoch, so the new pass
			// starts strictly after that timestamp using the largest id.
			lower := &ContractExpiryPosition{CreateTime: next.RecentAfter, ContractId: server.Id{
				255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255,
			}}
			upper := minTime
			if upper.Before(next.RecentAfter) {
				upper = next.RecentAfter
			}
			position = &ContractExpiryCursor{ScanBefore: upper, Open: lower, Dispute: lower}
		}
	}
	count, cursor, err := page(position)
	if err != nil {
		if _, ok := err.(*ForceCloseAccountingError); !ok {
			return count, after, err
		}
	}
	if historical {
		next.Historical = cursor
		next.HistoricalDone = cursor == nil
	} else {
		next.Recent = cursor
	}
	next.HistoricalNext = !historical
	if next.HistoricalDone && next.Recent == nil {
		return count, nil, err
	}
	return count, next, err
}
