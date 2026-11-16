package work

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Task ownership is process-local. Retain an active peer if two invocations
// ever overlap here; one retiring invocation must not erase that evidence.
var urlProbeHeartbeatOwners = struct {
	sync.Mutex
	active map[int]int
}{active: map[int]int{}}

// A completed pass's final heartbeat is recent history, not a live shard
// owner. Publish an explicit zero after all refreshes join. Keeping the
// series avoids depending on scrape-staleness handling for a deleted label.
func runWithProviderUrlProbeFleetHeartbeat(ctx context.Context, args *ProviderEgressProbeArgs,
	refresh func(context.Context), run func() (*ProviderEgressProbeResult, error),
) (*ProviderEgressProbeResult, error) {
	if args.UrlProbe != nil {
		shard := args.ShardIndex
		urlProbeHeartbeatOwners.Lock()
		urlProbeHeartbeatOwners.active[shard]++
		urlProbeHeartbeatOwners.Unlock()
		defer func() {
			urlProbeHeartbeatOwners.Lock()
			defer urlProbeHeartbeatOwners.Unlock()
			urlProbeHeartbeatOwners.active[shard]--
			if urlProbeHeartbeatOwners.active[shard] == 0 {
				delete(urlProbeHeartbeatOwners.active, shard)
				urlProbeShardObservedAt.WithLabelValues(strconv.Itoa(shard)).Set(0)
			}
		}()
	}
	return runWithProviderEgressFleetHeartbeatLifetime(ctx, time.Minute, refresh, run, args.UrlProbe != nil)
}
