package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// The activity set records per-(host, block) proxy activity with time-window
// reads, so a replacement instance can pre-warm the drained instance's
// active clients (PROXYDRAIN1.md §3.3).
func TestProxyClientActivity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// unique per run: the redis set outlives the test db reset, and a
		// rerun within the window would otherwise see the previous run's ids
		proxyHost := fmt.Sprintf("activitytest%d", time.Now().UnixNano())
		block := "g1"
		now := server.NowUtc()

		activeProxyId := server.NewId()
		staleProxyId := server.NewId()
		otherBlockProxyId := server.NewId()

		// empty set: no active clients
		connect.AssertEqual(t, 0, len(GetActiveProxyClients(ctx, proxyHost, block, 10*time.Minute, now)))

		// one recent, one stale (outside the window), one on another block
		TouchProxyClientActivity(ctx, proxyHost, block, now.Add(-1*time.Minute), activeProxyId)
		TouchProxyClientActivity(ctx, proxyHost, block, now.Add(-30*time.Minute), staleProxyId)
		TouchProxyClientActivity(ctx, proxyHost, "g2", now.Add(-1*time.Minute), otherBlockProxyId)

		activeProxyIds := GetActiveProxyClients(ctx, proxyHost, block, 10*time.Minute, now)
		connect.AssertEqual(t, 1, len(activeProxyIds))
		connect.AssertEqual(t, activeProxyId, activeProxyIds[0])

		// a wider window includes the stale entry
		windowProxyIds := GetActiveProxyClients(ctx, proxyHost, block, 45*time.Minute, now)
		connect.AssertEqual(t, 2, len(windowProxyIds))
		connect.AssertEqual(t, true, slices.Contains(windowProxyIds, activeProxyId))
		connect.AssertEqual(t, true, slices.Contains(windowProxyIds, staleProxyId))

		// re-touching moves a client forward in time (zadd overwrites the score)
		TouchProxyClientActivity(ctx, proxyHost, block, now, staleProxyId)
		activeProxyIds = GetActiveProxyClients(ctx, proxyHost, block, 10*time.Minute, now)
		connect.AssertEqual(t, 2, len(activeProxyIds))

		// entries past the retention are trimmed by any later touch
		trimmedProxyId := server.NewId()
		TouchProxyClientActivity(ctx, proxyHost, block, now.Add(-2*proxyClientActivityRetention), trimmedProxyId)
		TouchProxyClientActivity(ctx, proxyHost, block, now, activeProxyId)
		allProxyIds := GetActiveProxyClients(ctx, proxyHost, block, 100*proxyClientActivityRetention, now)
		connect.AssertEqual(t, false, slices.Contains(allProxyIds, trimmedProxyId))
	})
}
