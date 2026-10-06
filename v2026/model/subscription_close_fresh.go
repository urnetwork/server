// A bounded fresh tail is admitted independently of retained historical passes.
package model

import (
	"time"

	"github.com/urnetwork/server/v2026"
)

const forceCloseFreshWindow = time.Hour

// Every other raw subpage belongs to the fresh tail while that pass has work.
// The existing historical and complete post-epoch passes retain their exact
// positions and continue to cover the entire middle, including long-lived rows.
// Repeating the fresh window revisits a fresh report that becomes quiet after
// an earlier pass. Rows older than this window still belong to the full passes;
// this is a latency improvement, not an unqualified expiry deadline guarantee.
func forceCloseContractExpiryFreshPage(minTime, now time.Time, after *ContractExpirySweepCursor,
	page func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error),
) (int64, *ContractExpirySweepCursor, error) {
	next := &ContractExpirySweepCursor{}
	if after != nil {
		*next = *after
	}
	freshDue := next.Fresh != nil || next.FreshBefore.Before(minTime)
	if freshDue && (next.FreshNext || next.FreshBefore.IsZero() && next.Fresh == nil || next.BacklogDone) {
		position := next.Fresh
		if position == nil {
			// The fixed upper bound admits only already-old creations. An
			// authenticated recent report still withdraws a row in the shared
			// selector and is rechecked by the financial owner under its lock.
			lower := &ContractExpiryPosition{CreateTime: minTime.Add(-forceCloseFreshWindow), ContractId: server.Id{
				255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255,
			}}
			position = &ContractExpiryCursor{ScanBefore: minTime, Open: lower, Dispute: lower}
		}
		count, cursor, err := page(position)
		if err != nil {
			if _, classified := err.(*ForceCloseAccountingError); !classified {
				return count, after, err
			}
		}
		next.Fresh = cursor
		next.FreshNext = false
		if cursor == nil {
			next.FreshBefore = position.ScanBefore
			if next.BacklogDone {
				return count, nil, err
			}
		}
		return count, next, err
	}
	if next.BacklogDone {
		return 0, nil, nil
	}
	count, backlog, err := forceCloseContractExpirySweepPage(minTime, now, next, page)
	if err != nil {
		if _, classified := err.(*ForceCloseAccountingError); !classified {
			return count, after, err
		}
	}
	if backlog == nil {
		next.Historical = nil
		next.HistoricalDone = true
		next.Recent = nil
		next.BacklogDone = true
		if next.Fresh == nil {
			return count, nil, err
		}
	} else {
		next = backlog
	}
	next.FreshNext = true
	return count, next, err
}
