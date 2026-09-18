// Performance fixtures keep their historical quiescence window while placing
// a finite, cancellation-aware bound on continuously changing receive counts.
package connect_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	connectserver "github.com/urnetwork/server/v2026/connect"
)

// Require eight complete quiet intervals. The initial snapshot is not a quiet
// interval, and progress may reset stability but never renew the phase budget.
func waitForPerformanceCountToSettle(ctx context.Context, readCount func() int64) error {
	var lastCount int64
	initialized := false
	stableCount := 0
	return connectserver.TestingWaitForConnectCondition(ctx, perfSettleTimeout, 250*time.Millisecond, func(context.Context) (bool, string) {
		count := readCount()
		if initialized && count == lastCount {
			stableCount++
		} else {
			stableCount = 0
		}
		initialized = true
		lastCount = count
		return stableCount == 8, fmt.Sprintf("count=%d quiet intervals=%d/8", count, stableCount)
	})
}

// The old stability-only loops waited forever while each reading changed.
func TestPerformanceSettleChangingCountReachesDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		var count int64
		err := waitForPerformanceCountToSettle(context.Background(), func() int64 {
			count++
			return count
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(startTime) != perfSettleTimeout {
			t.Fatalf("changing count extended its settle budget: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}

// Preserve the measured two-second quiet window instead of returning as soon
// as the first pair of readings agrees.
func TestPerformanceSettleRequiresFullQuietWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startTime := time.Now()
		err := waitForPerformanceCountToSettle(context.Background(), func() int64 { return 42 })
		if err != nil || time.Since(startTime) != 2*time.Second {
			t.Fatalf("quiet window changed: err=%v elapsed=%s", err, time.Since(startTime))
		}
	})
}

// Cancel after the first observation, while the stability waiter is blocked.
func TestPerformanceSettleCancellationInterruptsQuietWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		observed := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- waitForPerformanceCountToSettle(ctx, func() int64 {
				close(observed)
				return 42
			})
		}()
		<-observed
		synctest.Wait()
		cancelTime := time.Now()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || time.Since(cancelTime) != 0 {
			t.Fatalf("settle ignored cancellation: err=%v elapsed=%s", err, time.Since(cancelTime))
		}
	})
}
