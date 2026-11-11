package model

import (
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"
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

// A proxy lists the Pro-only features it holds credentials for, the ones the
// proxy gates per network once enforcement is on: a SOCKS url, a WireGuard
// config.
func TestProxyClientProFeaturesListsWhatEnforcementGates(t *testing.T) {
	c := testProFeaturePlans()
	wg := &WgConfig{ClientIpv4: netip.MustParseAddr("10.255.0.1")}

	connect.AssertEqual(t, len(proxyClientProFeatures(c, nil)), 0)
	// issued under enforcement to a network without Pro: neither
	connect.AssertEqual(t, len(proxyClientProFeatures(c, &ProxyClient{HttpsProxyUrl: "https://x"})), 0)
	// issued while dark: every proxy holds a SOCKS url
	connect.AssertEqual(t, proxyClientProFeatures(c, &ProxyClient{SocksProxyUrl: "socks5h://x:1"}), []string{FeatureSocksProxy})
	connect.AssertEqual(
		t,
		proxyClientProFeatures(c, &ProxyClient{SocksProxyUrl: "socks5h://x:1", WgConfig: wg}),
		[]string{FeatureSocksProxy, FeatureWireguardProxy},
	)
	connect.AssertEqual(t, proxyClientProFeatures(c, &ProxyClient{WgConfig: wg}), []string{FeatureWireguardProxy})

	// a feature the free plan includes is no Pro feature
	freeSocks := testProFeaturePlans()
	freeSocks.Free.SocksProxy = true
	connect.AssertEqual(t, proxyClientProFeatures(freeSocks, &ProxyClient{SocksProxyUrl: "socks5h://x:1", WgConfig: wg}), []string{FeatureWireguardProxy})

	// with no pro.yml nothing is a Pro feature
	connect.AssertEqual(t, len(proxyClientProFeatures(&ProConfig{}, &ProxyClient{SocksProxyUrl: "socks5h://x:1", WgConfig: wg})), 0)
}
