// The singleton expiry sweep keeps one run working long enough to matter.
package model

import (
	"context"
	"testing"
	"time"
)

// The sweep is one singleton task that waits seconds to minutes for its next
// claim after each run. With a 15 second budget a run yielded after 15 one
// second subpages, so the sweep sat idle most of the time and closed about
// 1.5k contracts a minute. A run of the production page now keeps starting
// subpages for 90 seconds, inside the dispatch limit, before it checkpoints.
func TestForceCloseSweepRunKeepsDispatchingForNinetySeconds(t *testing.T) {
	type cursor struct{ page int }
	clock := time.Unix(1_800_000_000, 0)
	pages := 0
	// The scheduled sweep's per-run cap, which holds 98 subpages.
	const maxCount = 25_000
	closed, next, err := forceCloseContractPagesDispatched(context.Background(), maxCount, &cursor{}, forceClosePageBudget,
		forceClosePageDispatchLimit, forceCloseRawSubpageSize, func() time.Time { return clock },
		func(size int, after *cursor, dispatch func() bool) (int64, *cursor, error) {
			if !dispatch() {
				t.Fatal("a subpage started past the dispatch limit")
			}
			pages++
			clock = clock.Add(time.Second)
			return int64(size), &cursor{page: after.page + 1}, nil
		})
	if err != nil || next == nil || next.page != 90 || pages != 90 || closed != 90*forceCloseRawSubpageSize {
		t.Fatalf("a sweep run started %d one-second subpages before it checkpointed; want 90 (closed=%d err=%v)", pages, closed, err)
	}
	if forceClosePageDispatchLimit <= forceClosePageBudget {
		t.Fatal("the run budget reaches the dispatch limit")
	}
}
