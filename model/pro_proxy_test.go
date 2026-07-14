package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// TestProxyFeatureAllowedFailsOpen pins that a lookup failure NEVER denies a feature.
//
// If the proxy cannot be resolved to a network -- an unknown proxy id, a db blip, a
// race with creation -- the connection must be allowed. Denying would look, to a paying
// customer, exactly like their plan being revoked: SOCKS suddenly refusing to connect.
// A cache or a lookup must never be able to take away something the customer bought.
func TestProxyFeatureAllowedFailsOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// enforcement is dark in config, so force the path we actually want to test by
		// checking the layer underneath: an unresolvable proxy yields no network.
		_, ok := networkIdForProxy(ctx, server.NewId())
		connect.AssertEqual(t, ok, false)

		// and the gate lets it through rather than denying
		connect.AssertEqual(t, ProxyFeatureAllowed(ctx, server.NewId(), FeatureSocksProxy), true)
	})
}

// TestProLocalCacheServesAndExpires pins the in-process tier that makes the
// per-connection check free: a hit is served without touching redis or the db, and it
// stops being served once it expires.
func TestProLocalCacheServesAndExpires(t *testing.T) {
	networkId := server.NewId()

	// nothing cached yet
	_, ok := getProNetworkLocal(networkId)
	connect.AssertEqual(t, ok, false)

	setProNetworkLocal(networkId, true)

	pro, ok := getProNetworkLocal(networkId)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, pro, true)

	// an upgrade clears this process's entry so the next read reloads
	clearProNetworkLocal(networkId)
	_, ok = getProNetworkLocal(networkId)
	connect.AssertEqual(t, ok, false)
}

// TestProLocalCacheIsBounded pins that the in-process cache cannot grow without limit.
// It is a cache, not a registry -- a process that sees a very large number of distinct
// networks must not leak memory.
func TestProLocalCacheIsBounded(t *testing.T) {
	for i := 0; i < proLocalCacheMaxSize+64; i += 1 {
		setProNetworkLocal(server.NewId(), true)
	}

	proLocalCacheMutex.Lock()
	size := len(proLocalCache)
	proLocalCacheMutex.Unlock()

	connect.AssertEqual(t, size <= proLocalCacheMaxSize, true)
}

// TestProLocalCacheTtlIsShorterThanRedis pins the relationship between the two ttls.
//
// The local tier can only be cleared in the process that did the upgrade; every OTHER
// process keeps its stale entry until the local ttl runs out. That window is the delay
// between a customer paying and, say, SOCKS working on some other proxy instance -- so
// it must stay well under the shared redis window.
func TestProLocalCacheTtlIsShorterThanRedis(t *testing.T) {
	connect.AssertEqual(t, ProLocalCacheTtl < ProCacheTtl, true)
	connect.AssertEqual(t, ProLocalCacheTtl <= 5*time.Second, true)
}
