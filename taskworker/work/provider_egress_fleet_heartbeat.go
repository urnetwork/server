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
	if refresh == nil {
		return run()
	}
	refreshCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if refreshCtx.Err() != nil {
				return
			}
			server.HandleError(func() { refresh(refreshCtx) })
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	result, err := run()
	cancel()
	<-done
	return result, err
}
