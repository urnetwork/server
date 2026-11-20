package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Pure tests: no database or redis.

// An app's sign-in skips the concurrent limit's creation gate: a device
// create, with a network token, from a caller whose client info names an app.
// A hosted proxy, an API key, and any other caller keep the gate.
func TestConcurrentLimitCreationExemptOnlyForAppSignIns(t *testing.T) {
	sessionFor := func(deviceType string, apiKey bool) *session.ClientSession {
		return &session.ClientSession{
			ClientInfo:          connect.ClientInfo{Version: 1, DeviceType: deviceType},
			ByJwt:               &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()},
			ApiKeyAuthenticated: apiKey,
		}
	}
	device := &AuthNetworkClientArgs{Description: "device"}
	proxy := &AuthNetworkClientArgs{Description: "proxy", ProxyConfig: &ProxyConfig{}}
	for _, deviceType := range []string{"android", "ios", "macos", "windows", "linux"} {
		if !concurrentLimitCreationExempt(device, sessionFor(deviceType, false)) {
			t.Errorf("the %s app's sign-in is not exempt", deviceType)
		}
		if concurrentLimitCreationExempt(proxy, sessionFor(deviceType, false)) {
			t.Errorf("the %s app's hosted proxy is exempt", deviceType)
		}
		if concurrentLimitCreationExempt(device, sessionFor(deviceType, true)) {
			t.Errorf("an api key claiming the %s app is exempt", deviceType)
		}
	}
	for _, deviceType := range []string{"", "web", "cli", "server", "unknown"} {
		if concurrentLimitCreationExempt(device, sessionFor(deviceType, false)) {
			t.Errorf("a caller with client info device type %q is exempt", deviceType)
		}
	}
}

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

// The concurrent limit's refusal asks for an upgrade only when an upgrade lifts
// the limit: a free network with no Embed plan row whose Pro tier allows more
// than are connected. Every refusal says the client limit is exceeded, so the
// apps can tell it from any other refusal and say how to free a slot.
func TestConcurrentClientLimitRefusalOffersUpgradeOnlyWhenItLiftsTheLimit(t *testing.T) {
	cases := []struct {
		override        bool
		isPro           bool
		proLimit        int
		connectedCount  int
		upgradeRequired bool
	}{
		// free at the free limit, the Pro tier has room
		{override: false, isPro: false, proLimit: 1000, connectedCount: 2, upgradeRequired: true},
		// free, the Pro tier is unlimited
		{override: false, isPro: false, proLimit: 0, connectedCount: 2, upgradeRequired: true},
		// free, but as many are connected as the Pro tier allows
		{override: false, isPro: false, proLimit: 2, connectedCount: 2, upgradeRequired: false},
		// Pro at the Pro limit
		{override: false, isPro: true, proLimit: 1000, connectedCount: 1000, upgradeRequired: false},
		// an Embed plan row, whatever the tier would allow
		{override: true, isPro: false, proLimit: 1000, connectedCount: 5, upgradeRequired: false},
	}
	for _, c := range cases {
		refusal := concurrentClientLimitRefusal(c.override, c.isPro, c.proLimit, c.connectedCount)
		if !refusal.ClientLimitExceeded {
			t.Errorf("%+v: the refusal does not say the client limit is exceeded", c)
		}
		if refusal.UpgradeRequired != c.upgradeRequired {
			t.Errorf("%+v: upgrade_required = %t, want %t", c, refusal.UpgradeRequired, c.upgradeRequired)
		}
		if refusal.Message == "" {
			t.Errorf("%+v: the refusal has no message", c)
		}
	}
}

// While enforcement is dark the refusal is nil at once, with no lookup: this
// test has no database or redis for one to reach.
func TestNetworkConcurrentClientLimitRefusalIsDarkWithoutLookup(t *testing.T) {
	defer Testing_SetEnforceConcurrentClients(false)()
	if refusal := networkConcurrentClientLimitRefusal(context.Background(), server.NewId()); refusal != nil {
		t.Fatalf("refusal while dark = %+v, want none", refusal)
	}
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

// The client_limit_upgrade claim is true only when UR Pro raises the
// network's limit: a free network without an Embed allowance, while Pro's
// limit is higher than free's (unlimited counts as higher).
func TestClientLimitUpgradeClaimSaysWhenProRaisesTheLimit(t *testing.T) {
	claim := func(override bool, isPro bool) bool {
		return *clientLimitUpgradeClaim(override, isPro)
	}
	func() {
		defer Testing_SetConcurrentClientsLimit(2, 1000)()
		connect.AssertEqual(t, claim(false, false), true)
		connect.AssertEqual(t, claim(false, true), false)
		connect.AssertEqual(t, claim(true, false), false)
		connect.AssertEqual(t, claim(true, true), false)
	}()
	func() {
		// Pro no higher than free: nothing to gain
		defer Testing_SetConcurrentClientsLimit(3, 3)()
		connect.AssertEqual(t, claim(false, false), false)
	}()
	func() {
		// unlimited Pro
		defer Testing_SetConcurrentClientsLimit(2, 0)()
		connect.AssertEqual(t, claim(false, false), true)
	}()
	func() {
		// unlimited free: nothing to gain
		defer Testing_SetConcurrentClientsLimit(0, 1000)()
		connect.AssertEqual(t, claim(false, false), false)
	}()
}
