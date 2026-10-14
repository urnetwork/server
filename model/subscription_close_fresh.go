// A bounded fresh tail is admitted independently of retained historical passes.
package model

import (
	"time"

	"github.com/urnetwork/server"
)

const forceCloseFreshWindow = time.Hour

// One subpage of the sweep. A row a lane leaves open after an operational
// failure sits behind that lane's cursor, and a pass whose backlog is complete
// can keep revisiting only its fresh and catch-up windows. So a pass that has
// deferred any row ends once its backlog is complete; the next pass starts at
// the oldest open contract and revisits it. A pass without deferrals is unchanged.
func forceCloseContractExpiryFreshPage(minTime, now time.Time, after *ContractExpirySweepCursor,
	page func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error),
) (int64, *ContractExpirySweepCursor, error) {
	deferred := false
	count, next, err := forceCloseContractExpiryLanePage(minTime, now, after,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			count, cursor, err := page(position)
			if visited, ok := err.(*ForceCloseVisitError); ok && visited.CanCheckpoint() && 0 < len(visited.deferredContractIds) {
				deferred = true
			}
			return count, cursor, err
		})
	if next == nil || next == after || !forceClosePageCanAdvance(err) {
		return count, next, err
	}
	next.Deferred = next.Deferred || deferred
	if next.BacklogDone && next.Deferred {
		return count, nil, err
	}
	return count, next, err
}

// Every other raw subpage belongs to the fresh tail while that pass has work.
// The remaining subpages rotate through historical, complete post-epoch and
// catch-up work. Each retained pass keeps its exact position and fixed bounds.
// Repeating the fresh window revisits a fresh report that becomes quiet after
// an earlier pass. Rows older than this window still belong to the full passes;
// this is a latency improvement, not an unqualified expiry deadline guarantee.
func forceCloseContractExpiryLanePage(minTime, now time.Time, after *ContractExpirySweepCursor,
	page func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error),
) (int64, *ContractExpirySweepCursor, error) {
	next := &ContractExpirySweepCursor{}
	if after != nil {
		*next = *after
	}
	freshDue := next.Fresh != nil || next.FreshBefore.Before(minTime)
	freshPage := func() (int64, *ContractExpirySweepCursor, error) {
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
		if !forceClosePageCanAdvance(err) {
			return count, after, err
		}
		next.Fresh = cursor
		next.FreshNext = false
		if cursor == nil {
			next.FreshBefore = position.ScanBefore
			if next.BacklogDone && next.Catchup == nil && next.CatchupAfter.IsZero() {
				return count, nil, err
			}
		}
		return count, next, err
	}
	if freshDue && (next.FreshNext || next.FreshBefore.IsZero() && next.Fresh == nil || next.BacklogDone && next.Catchup == nil && next.CatchupAfter.IsZero()) {
		return freshPage()
	}
	if count, catchup, handled, err := forceCloseContractExpiryCatchupPage(minTime, next, page); handled {
		if !forceClosePageCanAdvance(err) {
			return count, after, err
		}
		return count, catchup, err
	} else {
		next = catchup
	}
	if next.BacklogDone {
		if freshDue {
			return freshPage()
		}
		return 0, nil, nil
	}
	count, backlog, err := forceCloseContractExpirySweepPage(minTime, now, next, page)
	if !forceClosePageCanAdvance(err) {
		return count, after, err
	}
	if backlog == nil {
		next.Historical = nil
		next.HistoricalDone = true
		next.Recent = nil
		next.BacklogDone = true
		if next.Fresh == nil && next.Catchup == nil && next.CatchupAfter.IsZero() {
			return count, nil, err
		}
	} else {
		next = backlog
	}
	if next.CatchupTurn < 2 {
		next.CatchupTurn++
	}
	next.FreshNext = true
	return count, next, err
}
