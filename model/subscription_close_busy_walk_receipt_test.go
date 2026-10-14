// The walk marks only a run that deferred busy rows and nothing else, and a
// pass carries its deferral until its backlog completes.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Only a walk that kept going past busy rows is deferral-only. A stopped walk
// still reports its walked part that way; any other receipt keeps its meaning.
func TestForceCloseWalkReportsDeferredOnlyReceipt(t *testing.T) {
	walked := runBusyWalk(t, []busyWalkStep{
		{count: 10, next: busyWalkCursor(1), err: busyWalkReceipt(10, 2, true)},
		{count: 7, next: nil},
	}, time.Second)
	stopped := runBusyWalk(t, []busyWalkStep{
		{count: 10, next: busyWalkCursor(1), err: busyWalkReceipt(10, 1, true)},
		{count: 7, next: busyWalkCursor(2), err: deadlineWalkReceipt(7)},
	}, time.Second)
	other := runBusyWalk(t, []busyWalkStep{{count: 7, next: busyWalkCursor(1), err: deadlineWalkReceipt(7)}}, time.Second)
	idle := runBusyWalk(t, []busyWalkStep{{count: 4, next: busyWalkCursor(1), err: busyWalkReceipt(4, 4, false)}}, time.Second)
	clean := runBusyWalk(t, []busyWalkStep{{count: 4, next: nil}}, time.Second)
	for _, c := range []struct {
		name         string
		result       busyWalkResult
		deferredOnly bool
	}{
		{name: "walked to the end", result: walked, deferredOnly: true},
		{name: "stopped before another failure", result: stopped, deferredOnly: true},
		{name: "another failure first", result: other, deferredOnly: false},
		{name: "page without progress first", result: idle, deferredOnly: false},
	} {
		visited, ok := c.result.err.(*ForceCloseVisitError)
		if !ok || visited.DeferredOnly() != c.deferredOnly {
			t.Errorf("%s: deferred only %t, want %t", c.name, ok && visited.DeferredOnly(), c.deferredOnly)
		}
	}
	if clean.err != nil {
		t.Error("a walk without failures reported one", clean.err)
	}
}

// Both production shapes of a busy refusal qualify; anything else does not.
func TestForceCloseRowOwnershipBusyAcceptsOnlyBusyRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		busy bool
	}{
		{name: "deadline reconciliation refusal", err: errTransferBalanceOwnershipBusy, busy: true},
		{name: "refused close with its open confirmation", err: errors.Join(errTransferBalanceOwnershipBusy, &forceCloseNonfinalError{}), busy: true},
		{name: "refused close without cleanup error", err: errors.Join(errTransferBalanceOwnershipBusy, nil), busy: true},
		{name: "wrapped refusal", err: fmt.Errorf("synthetic phase: %w", errTransferBalanceOwnershipBusy), busy: true},
		{name: "refused close that entered dispute", err: errors.Join(errTransferBalanceOwnershipBusy, &forceCloseNonfinalError{disputed: true}), busy: false},
		{name: "refusal joined with a deadline", err: errors.Join(errTransferBalanceOwnershipBusy, context.DeadlineExceeded), busy: false},
		{name: "open confirmation alone", err: &forceCloseNonfinalError{}, busy: false},
		{name: "other failure", err: errors.New("synthetic refused write"), busy: false},
	}
	for _, c := range cases {
		if busy := forceCloseRowOwnershipBusy(c.err); busy != c.busy {
			t.Errorf("%s: busy=%t, want %t", c.name, busy, c.busy)
		}
	}
}

// A historical deferral early in the pass is carried until the backlog ends.
func TestContractExpirySweepCarriesDeferralToBacklog(t *testing.T) {
	now := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	historical := &ContractExpiryCursor{ScanBefore: now.Add(-2 * time.Hour), DisputeDone: true,
		Open: &ContractExpiryPosition{CreateTime: now.Add(-9 * 24 * time.Hour), ContractId: server.NewId()}}
	state := &ContractExpirySweepCursor{RecentAfter: historical.ScanBefore, Historical: historical, HistoricalNext: true,
		Fresh: &ContractExpiryCursor{ScanBefore: now.Add(-13 * time.Minute), DisputeDone: true}, FreshBefore: now.Add(-70 * time.Minute)}
	moved := &ContractExpiryCursor{ScanBefore: historical.ScanBefore, DisputeDone: true,
		Open: &ContractExpiryPosition{CreateTime: historical.Open.CreateTime.Add(time.Hour), ContractId: server.NewId()}}
	_, next, err := forceCloseContractExpiryFreshPage(now.Add(-12*time.Minute), now, state,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != historical {
				t.Fatal("the subpage did not visit the historical lane", position)
			}
			return 3, moved, busyWalkReceipt(3, 1, true)
		})
	if err == nil || next == nil || !next.Deferred || next.BacklogDone || next.Historical != moved {
		t.Fatal("the pass lost its deferral or ended before its backlog", next)
	}
	tail := busyWalkBacklogTail(now)
	tail.Deferred = true
	_, next, err = forceCloseContractExpiryFreshPage(now.Add(-12*time.Minute), now, tail,
		func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 3, nil, nil })
	if err != nil || next != nil {
		t.Fatal("a pass carrying a deferral kept circling after its backlog", next)
	}
}

// A pass already past its backlog ends on its next deferral, from any lane.
func TestContractExpirySweepEndsCompletedBacklogPassOnDeferral(t *testing.T) {
	now := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	fresh := &ContractExpiryCursor{ScanBefore: now.Add(-13 * time.Minute), DisputeDone: true,
		Open: &ContractExpiryPosition{CreateTime: now.Add(-40 * time.Minute), ContractId: server.NewId()}}
	state := &ContractExpirySweepCursor{RecentAfter: now.Add(-9 * 24 * time.Hour), HistoricalDone: true, BacklogDone: true,
		Fresh: fresh, FreshNext: true, CatchupAfter: now.Add(-7 * 24 * time.Hour)}
	moved := &ContractExpiryCursor{ScanBefore: fresh.ScanBefore, DisputeDone: true,
		Open: &ContractExpiryPosition{CreateTime: now.Add(-35 * time.Minute), ContractId: server.NewId()}}
	_, next, err := forceCloseContractExpiryFreshPage(now.Add(-12*time.Minute), now, state,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != fresh {
				t.Fatal("the subpage did not visit the fresh lane", position)
			}
			return 3, moved, busyWalkReceipt(3, 1, true)
		})
	if err == nil || next != nil {
		t.Fatal("a completed-backlog pass kept its deferred fresh row behind the window", next)
	}
	_, next, err = forceCloseContractExpiryFreshPage(now.Add(-12*time.Minute), now, state,
		func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 3, moved, nil })
	if err != nil || next == nil || next.Fresh != moved {
		t.Fatal("a completed-backlog pass without a deferral ended", next, err)
	}
}
