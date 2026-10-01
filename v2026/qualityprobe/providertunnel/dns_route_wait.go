package providertunnel

import (
	"context"
	"github.com/urnetwork/connect/v2026"
)

// Await routing eligibility under the caller's existing deadline. Each observer
// owns only a coalesced wake-up; no provider identity or pending packet is kept.
func waitProviderRoute(ctx context.Context, lost context.Context, monitor connect.MultiClientMonitor) error {
	if monitor == nil {
		return nil
	}
	changed := make(chan struct{}, 1)
	unwatch := monitor.AddMonitorEventCallback(func(*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent, bool) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer unwatch()
	var loss <-chan struct{}
	if lost != nil {
		loss = lost.Done()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lost != nil && lost.Err() != nil {
			return context.Cause(lost)
		}
		_, providers := monitor.Events()
		for _, event := range providers {
			if event != nil && event.State.IsActive() {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-loss:
			return context.Cause(lost)
		case <-changed:
		}
	}
}
