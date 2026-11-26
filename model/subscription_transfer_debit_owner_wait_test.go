package model

import (
	"context"
	"testing"
	"time"
)

// Replaces the flush's bounded wait for a held owner (transferDebitOwnerWait).
func testingWithTransferDebitOwnerWait(ctx context.Context, wait time.Duration) context.Context {
	return context.WithValue(ctx, transferDebitOwnerWaitKey{}, wait)
}

// The wait is bounded by the constant, or the seam, and by what the page
// keeps for the work after it, at a fixed clock.
func TestTransferDebitOwnerWaitKeepsThePageReserve(t *testing.T) {
	now := time.Date(2026, 11, 25, 23, 0, 0, 0, time.UTC)
	page := func(left time.Duration) context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(left))
		t.Cleanup(cancel)
		return ctx
	}
	for _, sample := range []struct {
		name    string
		ctx     context.Context
		reserve time.Duration
		want    time.Duration
	}{
		{name: "no page deadline", ctx: context.Background(), reserve: transferDebitApplyWaitReserve, want: transferDebitOwnerWait},
		{name: "a fresh page", ctx: page(15 * time.Second), reserve: transferDebitApplyWaitReserve, want: transferDebitOwnerWait},
		{name: "the seam", ctx: testingWithTransferDebitOwnerWait(page(time.Hour), time.Minute), reserve: transferDebitApplyWaitReserve, want: time.Minute},
		{name: "apply keeps its reserve", ctx: page(8 * time.Second), reserve: transferDebitApplyWaitReserve, want: 3 * time.Second},
		{name: "apply inside its reserve", ctx: page(4 * time.Second), reserve: transferDebitApplyWaitReserve, want: 0},
		{name: "cleanup keeps its reserve", ctx: page(4 * time.Second), reserve: transferDebitCleanupWaitReserve, want: 3 * time.Second},
		{name: "cleanup at its reserve", ctx: page(time.Second), reserve: transferDebitCleanupWaitReserve, want: 0},
		{name: "an expired page", ctx: page(-time.Second), reserve: transferDebitCleanupWaitReserve, want: 0},
	} {
		if wait := transferDebitOwnerWaitAt(sample.ctx, sample.reserve, now); wait != sample.want {
			t.Fatalf("%s: wait %s, want %s", sample.name, wait, sample.want)
		}
	}
}
