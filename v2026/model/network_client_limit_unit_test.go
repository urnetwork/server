package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Pure tests: no database or redis.

func TestNetworkClientLimitLocalCache(t *testing.T) {
	cache := newNetworkClientLimitLocalCache()
	networkId := server.NewId()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	_, ok := cache.Get(networkId, now)
	connect.AssertEqual(t, ok, false)

	cache.Put(networkId, networkClientLimitLocalEntry{limit: 5000, override: true, expiry: now.Add(30 * time.Second)})
	entry, ok := cache.Get(networkId, now.Add(29*time.Second))
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, entry.limit, 5000)
	connect.AssertEqual(t, entry.override, true)

	// an entry is dropped at its expiry
	_, ok = cache.Get(networkId, now.Add(30*time.Second))
	connect.AssertEqual(t, ok, false)
	_, ok = cache.Get(networkId, now)
	connect.AssertEqual(t, ok, false)

	// Remove drops one network; Clear drops all
	networkIdB := server.NewId()
	cache.Put(networkId, networkClientLimitLocalEntry{limit: 1, override: true, expiry: now.Add(time.Minute)})
	cache.Put(networkIdB, networkClientLimitLocalEntry{limit: 2, override: true, expiry: now.Add(time.Minute)})
	cache.Remove(networkId)
	_, ok = cache.Get(networkId, now)
	connect.AssertEqual(t, ok, false)
	_, ok = cache.Get(networkIdB, now)
	connect.AssertEqual(t, ok, true)
	cache.Clear()
	_, ok = cache.Get(networkIdB, now)
	connect.AssertEqual(t, ok, false)
}

// A full cache restarts instead of growing past its bound; replacing an entry
// that is already cached does not.
func TestNetworkClientLimitLocalCacheBounded(t *testing.T) {
	cache := newNetworkClientLimitLocalCache()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	entry := networkClientLimitLocalEntry{limit: 100, expiry: now.Add(time.Minute)}

	firstNetworkId := server.NewId()
	cache.Put(firstNetworkId, entry)
	for range networkClientLimitLocalCacheMaxSize - 1 {
		cache.Put(server.NewId(), entry)
	}
	connect.AssertEqual(t, len(cache.entries), networkClientLimitLocalCacheMaxSize)

	// a re-put of a cached network keeps the cache
	cache.Put(firstNetworkId, networkClientLimitLocalEntry{limit: 7, override: true, expiry: now.Add(time.Minute)})
	connect.AssertEqual(t, len(cache.entries), networkClientLimitLocalCacheMaxSize)

	// one more network restarts it
	nextNetworkId := server.NewId()
	cache.Put(nextNetworkId, entry)
	connect.AssertEqual(t, len(cache.entries), 1)
	_, ok := cache.Get(nextNetworkId, now)
	connect.AssertEqual(t, ok, true)
}

// The concurrent check folds in the rollout switch: dark refuses nothing; a
// limit of zero or less is unlimited; otherwise the limit is reached at count.
func TestConcurrentClientLimitExceeded(t *testing.T) {
	func() {
		defer Testing_SetEnforceConcurrentClients(false)()
		connect.AssertEqual(t, concurrentClientLimitExceeded(1, 1_000_000), false)
		connect.AssertEqual(t, concurrentClientLimitExceeded(5000, 5000), false)
	}()

	defer Testing_SetEnforceConcurrentClients(true)()
	connect.AssertEqual(t, concurrentClientLimitExceeded(0, 1_000_000), false)
	connect.AssertEqual(t, concurrentClientLimitExceeded(-1, 1_000_000), false)
	connect.AssertEqual(t, concurrentClientLimitExceeded(3, 2), false)
	connect.AssertEqual(t, concurrentClientLimitExceeded(3, 3), true)
	connect.AssertEqual(t, concurrentClientLimitExceeded(3, 4), true)
	connect.AssertEqual(t, concurrentClientLimitExceeded(5000, 4999), false)
	connect.AssertEqual(t, concurrentClientLimitExceeded(5000, 5000), true)
}

// With an Embed plan row cached, the network's concurrent limit is the row's
// limit and the tier is never read (this test has no database to read it from).
func TestNetworkConcurrentClientLimitUsesTheEmbedPlanOverride(t *testing.T) {
	defer Testing_ClearNetworkClientLimitCache()
	defer Testing_SetConcurrentClientsLimit(3, 10)()

	networkId := server.NewId()
	networkClientLimitLocal.Put(networkId, networkClientLimitLocalEntry{
		limit:    5000,
		override: true,
		expiry:   server.NowUtc().Add(time.Minute),
	})
	ctx := context.Background()
	connect.AssertEqual(t, networkConcurrentClientLimit(ctx, networkId), 5000)
	// the provider intent's normal client limit follows the same allowance
	connect.AssertEqual(t, networkNormalClientLimit(ctx, networkId), 5000)

	limit, override := networkClientLimitOverride(ctx, networkId)
	connect.AssertEqual(t, limit, 5000)
	connect.AssertEqual(t, override, true)
}
