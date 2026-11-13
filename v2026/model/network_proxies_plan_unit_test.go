package model

import (
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Pure tests: no database or redis.

// pro.yml's plans: SOCKS and WireGuard are in Pro and not in free.
func testProFeaturePlans() *ProConfig {
	return &ProConfig{
		Free: ProTier{HttpProxy: true, HttpsProxy: true},
		Pro:  ProTier{HttpProxy: true, HttpsProxy: true, SocksProxy: true, WireguardProxy: true},
	}
}

// A Pro-only feature is one Pro has and free does not; with no pro.yml no
// plan has any, so none is Pro-only.
func TestProOnlyFeatureIsWhatAnUpgradeAdds(t *testing.T) {
	c := testProFeaturePlans()
	connect.AssertEqual(t, c.ProOnlyFeature(FeatureSocksProxy), true)
	connect.AssertEqual(t, c.ProOnlyFeature(FeatureWireguardProxy), true)
	connect.AssertEqual(t, c.ProOnlyFeature(FeatureHttpProxy), false)
	connect.AssertEqual(t, c.ProOnlyFeature(FeatureHttpsProxy), false)
	connect.AssertEqual(t, c.ProOnlyFeature("unknown_feature"), false)

	// the switch does not change the plans
	c.EnforceFeatures = true
	connect.AssertEqual(t, c.ProOnlyFeature(FeatureSocksProxy), true)

	inert := &ProConfig{}
	connect.AssertEqual(t, inert.ProOnlyFeature(FeatureSocksProxy), false)
	connect.AssertEqual(t, inert.ProOnlyFeature(FeatureWireguardProxy), false)
}

// A proxy lists the Pro-only features it used within the window, as the
// proxy recorded them, not the ones it merely holds credentials for. Every
// proxy issued while enforcement was dark holds a SOCKS url, so listing the
// credentials put the upgrade notice on every network with a proxy, the ones
// that only use HTTPS included (owner, 2026-11-11: only affected networks).
func TestProxyClientProFeaturesListsTheProFeaturesUsedInTheWindow(t *testing.T) {
	c := testProFeaturePlans()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	wg := &WgConfig{ClientIpv4: netip.MustParseAddr("10.255.0.1")}
	issuedWhileDark := &ProxyClient{SocksProxyUrl: "socks5h://x:1", HttpsProxyUrl: "https://x", WgConfig: wg}

	connect.AssertEqual(t, len(proxyClientProFeatures(c, nil, map[string]time.Time{FeatureSocksProxy: yesterday}, now)), 0)

	// the root cause: credentials alone are not use
	connect.AssertEqual(t, len(proxyClientProFeatures(c, issuedWhileDark, nil, now)), 0)
	connect.AssertEqual(t, len(proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{}, now)), 0)

	// a SOCKS sign-in yesterday, a WireGuard handshake yesterday, both
	connect.AssertEqual(
		t,
		proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{FeatureSocksProxy: yesterday}, now),
		[]string{FeatureSocksProxy},
	)
	connect.AssertEqual(
		t,
		proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{FeatureWireguardProxy: yesterday}, now),
		[]string{FeatureWireguardProxy},
	)
	connect.AssertEqual(
		t,
		proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{FeatureSocksProxy: yesterday, FeatureWireguardProxy: now}, now),
		[]string{FeatureSocksProxy, FeatureWireguardProxy},
	)

	// the window is inclusive at its start and ends there
	windowStart := now.Add(-ProxyFeatureUseWindow)
	connect.AssertEqual(
		t,
		proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{FeatureSocksProxy: windowStart}, now),
		[]string{FeatureSocksProxy},
	)
	connect.AssertEqual(
		t,
		len(proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{FeatureSocksProxy: windowStart.Add(-time.Millisecond)}, now)),
		0,
	)
	connect.AssertEqual(t, ProxyFeatureUseWindow, 14*24*time.Hour)

	// a sign-in the proxy refused once enforcement is on still counts, on a
	// proxy issued no SOCKS url too: the network is still trying to use it
	connect.AssertEqual(
		t,
		proxyClientProFeatures(c, &ProxyClient{HttpsProxyUrl: "https://x"}, map[string]time.Time{FeatureSocksProxy: yesterday}, now),
		[]string{FeatureSocksProxy},
	)

	// HTTP and HTTPS are in the free plan, and a use of an unknown feature is
	// none of the list's business
	connect.AssertEqual(
		t,
		len(proxyClientProFeatures(c, issuedWhileDark, map[string]time.Time{FeatureHttpsProxy: yesterday, "unknown_feature": yesterday}, now)),
		0,
	)

	// a feature the free plan includes is no Pro feature
	freeSocks := testProFeaturePlans()
	freeSocks.Free.SocksProxy = true
	connect.AssertEqual(
		t,
		proxyClientProFeatures(freeSocks, issuedWhileDark, map[string]time.Time{FeatureSocksProxy: yesterday, FeatureWireguardProxy: yesterday}, now),
		[]string{FeatureWireguardProxy},
	)

	// with no pro.yml nothing is a Pro feature
	connect.AssertEqual(
		t,
		len(proxyClientProFeatures(&ProConfig{}, issuedWhileDark, map[string]time.Time{FeatureSocksProxy: yesterday, FeatureWireguardProxy: yesterday}, now)),
		0,
	)
}
