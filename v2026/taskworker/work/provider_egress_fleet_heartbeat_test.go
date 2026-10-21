package work

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestProviderEgressFleetHeartbeatKeepsLongPassVisibleAndStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var refreshed atomic.Int32
		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err := runWithProviderEgressFleetHeartbeat(ctx, 5*time.Minute, func(context.Context) {
				refreshed.Add(1)
			}, func() (*ProviderEgressProbeResult, error) {
				close(started)
				<-release
				return &ProviderEgressProbeResult{}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
		<-started
		synctest.Wait()
		if got := refreshed.Load(); got != 1 {
			t.Fatalf("initial in-flight snapshot refreshes=%d, want 1", got)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got := refreshed.Load(); got != 2 {
			t.Fatalf("long pass snapshot refreshes=%d, want 2", got)
		}
		close(release)
		<-done
		time.Sleep(15 * time.Minute)
		synctest.Wait()
		if got := refreshed.Load(); got != 2 {
			t.Fatalf("completed pass kept refreshing stale snapshot: %d", got)
		}
	})
}
