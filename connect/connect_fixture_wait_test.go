// Bounded fixture polling keeps a stalled protocol phase observable even when
// the package runs with -timeout=0. Synthetic time pins the liveness boundary.
package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Poll one condition within a single deadline; changing diagnostics never
// renew the budget. Checks must return promptly or honor their supplied context.
func TestingWaitForConnectCondition(
	ctx context.Context,
	timeout time.Duration,
	interval time.Duration,
	check func(context.Context) (bool, string),
) error {
	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	defer waitCancel()
	detail := "condition not checked"
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("%s: %w", detail, err)
		}
		var ready bool
		ready, detail = check(waitCtx)
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("%s: %w", detail, err)
		}
		if ready {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("%s: %w", detail, waitCtx.Err())
		case <-time.After(interval):
		}
	}
}

// The original control settle loop could hold two queued controls forever.
// Fake time reaches the phase deadline without a scheduler-dependent timeout.
func TestConnectFixtureWaitStalledControlsReachDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		checks := 0
		err := TestingWaitForConnectCondition(ctx, time.Minute, 10*time.Second, func(ctx context.Context) (bool, string) {
			checks++
			return false, "pending controls=2 resend_a=1 resend_b=1"
		})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "pending controls=2") {
			t.Fatalf("stalled controls lost their deadline or diagnostics: %v", err)
		}
		// The final poll and context timer share a timestamp; either can wake
		// first, but neither may extend the absolute phase budget.
		if elapsed := time.Since(startTime); elapsed != time.Minute || checks < 6 || 7 < checks {
			t.Fatalf("settle deadline was renewed: elapsed=%s checks=%d", elapsed, checks)
		}
	})
}

// Parent cancellation interrupts the polling wait, including an hour-long poll
// interval; an explicit first-check barrier forces cancellation while pending.
func TestConnectFixtureWaitCancellationInterruptsPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		checked := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- TestingWaitForConnectCondition(ctx, 2*time.Hour, time.Hour, func(context.Context) (bool, string) {
				close(checked)
				return false, "controls still pending"
			})
		}()
		<-checked
		synctest.Wait()
		cancelTime := time.Now()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("pending wait ignored cancellation: %v", err)
		}
		if elapsed := time.Since(cancelTime); elapsed != 0 {
			t.Fatalf("cancellation waited for the polling interval: %s", elapsed)
		}
	})
}

// Readiness is checked immediately and does not pay the polling interval.
func TestConnectFixtureWaitReadyReturnsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		err := TestingWaitForConnectCondition(context.Background(), time.Minute, time.Hour, func(context.Context) (bool, string) {
			return true, "queues empty"
		})
		if err != nil || time.Since(startTime) != 0 {
			t.Fatalf("ready condition waited: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}

// An explicit state transition drains a previously nonempty condition; the
// waiter must neither report premature success nor retain stale diagnostics.
func TestConnectFixtureWaitDrainsBeforeSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready atomic.Bool
		checked := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- TestingWaitForConnectCondition(context.Background(), time.Minute, time.Second, func(context.Context) (bool, string) {
				if ready.Load() {
					return true, "queues empty"
				}
				checked <- struct{}{}
				return false, "controls pending"
			})
		}()
		<-checked
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("wait ended before the queues drained: %v", err)
		default:
		}
		ready.Store(true)
		time.Sleep(time.Second)
		if err := <-done; err != nil {
			t.Fatalf("drained queues did not settle: %v", err)
		}
	})
}

// A canceled lifecycle cannot be reclassified as success by an empty snapshot.
func TestConnectFixtureWaitCanceledContextDoesNotCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := TestingWaitForConnectCondition(ctx, time.Minute, time.Second, func(context.Context) (bool, string) {
		t.Fatal("checked a condition after lifecycle cancellation")
		return true, "queues empty"
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lifecycle reported success: %v", err)
	}
}

// A check that finishes at its deadline must fail even if its last read is ready.
func TestConnectFixtureWaitCheckUsesPhaseContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		err := TestingWaitForConnectCondition(context.Background(), time.Minute, time.Second, func(ctx context.Context) (bool, string) {
			<-ctx.Done()
			return true, "check completed after deadline"
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(startTime) != time.Minute {
			t.Fatalf("check escaped its phase deadline: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}

// A shorter owner deadline wins even when neither phase nor poll timer is due.
func TestConnectFixtureWaitPreservesEarlierDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		startTime := time.Now()
		err := TestingWaitForConnectCondition(ctx, time.Minute, time.Hour, func(context.Context) (bool, string) {
			return false, "pending"
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(startTime) != 5*time.Second {
			t.Fatalf("phase extended its owner deadline: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}
