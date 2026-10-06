// A retained middle pass and a moving fresh window leave a separately owned gap.
package model

import (
	"time"

	"github.com/urnetwork/server"
)

// One of every three non-fresh subpages visits the interval strictly after the
// retained recent upper bound through the fresh lower bound, including its ties.
// A continuation never changes its upper bound. Completed passes reopen when
// logical expiry time advances past their starting check. Reports quieting later
// need not wait for the full recent backlog; later passes also admit a wider gap.
// Capture the original lower bound before recent can complete or start a new pass.
// Older readers ignore these additive fields and retain all three original lanes.
func forceCloseContractExpiryCatchupPage(minTime time.Time, after *ContractExpirySweepCursor,
	page func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error),
) (int64, *ContractExpirySweepCursor, bool, error) {
	next := *after
	upper := minTime.Add(-forceCloseFreshWindow)
	if next.Fresh != nil {
		upper = next.Fresh.ScanBefore.Add(-forceCloseFreshWindow)
	}
	if next.CatchupAfter.IsZero() && next.Recent != nil && !next.Recent.ScanBefore.IsZero() && upper.After(next.Recent.ScanBefore) {
		next.CatchupAfter = next.Recent.ScanBefore
	}
	if next.CatchupTurn < 2 && !next.BacklogDone {
		return 0, &next, false, nil
	}
	position := next.Catchup
	if position == nil {
		if next.CatchupAfter.IsZero() || !upper.After(next.CatchupAfter) ||
			!minTime.After(next.CatchupChecked) && !upper.After(next.CatchupBefore) {
			return 0, &next, false, nil
		}
		lower := &ContractExpiryPosition{CreateTime: next.CatchupAfter, ContractId: server.Id{
			255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255,
		}}
		position = &ContractExpiryCursor{ScanBefore: upper, Open: lower, Dispute: lower}
	}
	count, cursor, err := page(position)
	if err != nil {
		if _, classified := err.(*ForceCloseAccountingError); !classified {
			return count, after, true, err
		}
	}
	next.Catchup = cursor
	next.CatchupTurn = 0
	next.FreshNext = true
	if after.Catchup == nil {
		// A later page may use a later quiet cutoff. Retain the earliest
		// check so completion cannot claim every row saw that later cutoff.
		next.CatchupChecked = minTime
	}
	if cursor == nil {
		next.CatchupBefore = position.ScanBefore
		if next.BacklogDone && next.Fresh == nil {
			return count, nil, true, err
		}
	}
	return count, &next, true, err
}
