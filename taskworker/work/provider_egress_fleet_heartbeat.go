package work

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// Long provider passes can outlive the dashboard's 15-minute freshness window.
// Refresh only while this shard owns work; a canceled or completed task cannot
// keep a process-local fleet snapshot looking current. The existing one-minute
// refresh guard coalesces overlapping owners in the same Taskworker process.
const providerEgressFleetHeartbeatInterval = 5 * time.Minute

func runWithProviderEgressFleetHeartbeat(
	ctx context.Context,
	interval time.Duration,
	refresh func(context.Context),
	run func() (*ProviderEgressProbeResult, error),
) (*ProviderEgressProbeResult, error) {
	return runWithProviderEgressFleetHeartbeatLifetime(ctx, interval, refresh, run, false)
}

// A URL census has its own ten-second read deadline. Let that already-started
// read finish when a short pass returns normally, while stopping new periodic
// work. Parent cancellation still aborts the read and every exit joins it.
// Legacy refreshes retain their immediate cancellation at pass completion.
func runWithProviderEgressFleetHeartbeatLifetime(
	ctx context.Context,
	interval time.Duration,
	refresh func(context.Context),
	run func() (*ProviderEgressProbeResult, error),
	completeRefresh bool,
) (*ProviderEgressProbeResult, error) {
	if refresh == nil {
		return run()
	}
	refreshCtx, cancel := context.WithCancel(ctx)
	stopping := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-stopping:
				return
			default:
			}
			server.HandleError(func() { refresh(refreshCtx) })
			select {
			case <-refreshCtx.Done():
				return
			case <-stopping:
				return
			case <-ticker.C:
			}
		}
	}()
	// Stop and join on every exit, including a recovered task panic. The
	// caller may retire its published owner only after refresh has stopped.
	defer func() {
		close(stopping)
		if !completeRefresh {
			cancel()
		}
		<-done
		cancel()
	}()
	return run()
}
