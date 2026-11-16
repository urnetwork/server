package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// TestProConfig loads config/all/pro.yml through Pro() and asserts the product
// spec parses to the expected limits/features (binary GiB/TiB units).
func TestProConfig(t *testing.T) {
	gib := ByteCount(1024 * 1024 * 1024)
	tib := 1024 * gib

	c := Pro()

	// This asserts what pro.yml PARSES to, so it needs pro.yml. Skip in the stripped
	// harness (WARP_CONFIG_HOME pointed at a config dir with no pro.yml), where an absent
	// file is the thing under test -- TestProAbsent owns that case.
	if c.MaxConcurrentClients(false) == 0 {
		t.Skip("pro.yml is not present in this environment; see TestProAbsent")
	}

	// free (Community)
	connect.AssertEqual(t, c.MaxConcurrentClients(false), 2)
	connect.AssertEqual(t, c.DataAmount(false), ByteCount(30)*gib)
	connect.AssertEqual(t, c.DataPeriod(false), 24*time.Hour)
	connect.AssertEqual(t, c.FeatureEnabled(false, FeatureHttpProxy), true)
	connect.AssertEqual(t, c.FeatureEnabled(false, FeatureHttpsProxy), true)
	connect.AssertEqual(t, c.FeatureEnabled(false, FeatureSocksProxy), false)     // Pro-only
	connect.AssertEqual(t, c.FeatureEnabled(false, FeatureWireguardProxy), false) // Pro-only

	// pro (UR Pro)
	connect.AssertEqual(t, c.MaxConcurrentClients(true), 1000)
	connect.AssertEqual(t, c.DataAmount(true), ByteCount(10)*tib)
	connect.AssertEqual(t, c.DataPeriod(true), 720*time.Hour)
	connect.AssertEqual(t, c.FeatureEnabled(true, FeatureHttpProxy), true)
	connect.AssertEqual(t, c.FeatureEnabled(true, FeatureSocksProxy), true)
	connect.AssertEqual(t, c.FeatureEnabled(true, FeatureWireguardProxy), true)

	// referrals
	connect.AssertEqual(t, c.ReferralBonus, ByteCount(3)*gib)
	connect.AssertEqual(t, c.ReferredBonus, ByteCount(3)*gib)
	connect.AssertEqual(t, c.ReferralPeriod, 24*time.Hour)
	connect.AssertEqual(t, c.MaxReferrals, 20)

	// seeker
	connect.AssertEqual(t, c.SeekerDataMultiplier(), 2.0)

	// data codes
	connect.AssertEqual(t, c.DataCodeDuration, 8760*time.Hour)
	connect.AssertEqual(t, c.DataCodeSkus, []ProDataCodeSku{
		{Sku: "data_1tib", Data: 1 * tib, PriceUsd: 3, Rails: []string{DataRailCard, DataRailCrypto, DataRailX402, DataRailGamma}},
		{Sku: "data_10tib", Data: 10 * tib, PriceUsd: 20, Rails: []string{DataRailCard, DataRailCrypto, DataRailX402, DataRailGamma}},
		// the pack for clients with a $1 per-payment cap (the owner, 2026-11-11):
		// x402 and Gamma only, never by card or the other crypto coins
		{Sku: "data_256gib", Data: 256 * gib, PriceUsd: 0.75, Rails: []string{DataRailX402, DataRailGamma}},
	})

	// unknown feature -> false
	connect.AssertEqual(t, c.FeatureEnabled(true, "unknown_feature"), false)

}

// TestProDataCodeSkusByRail pins where pro.yml sells each data pack: the
// 256 GiB pack over x402 and with Gamma, and nowhere else.
func TestProDataCodeSkusByRail(t *testing.T) {
	c := Pro()
	if c.MaxConcurrentClients(false) == 0 {
		t.Skip("pro.yml is not present in this environment; see TestProAbsent")
	}

	ids := func(rail string) []string {
		ids := []string{}
		for _, sku := range c.DataCodeSkusOn(rail) {
			ids = append(ids, sku.Sku)
		}
		return ids
	}
	connect.AssertEqual(t, ids(DataRailCard), []string{"data_1tib", "data_10tib"})
	connect.AssertEqual(t, ids(DataRailCrypto), []string{"data_1tib", "data_10tib"})
	connect.AssertEqual(t, ids(DataRailX402), []string{"data_1tib", "data_10tib", "data_256gib"})
	connect.AssertEqual(t, ids(DataRailGamma), []string{"data_1tib", "data_10tib", "data_256gib"})

	pack := c.DataCodeSku("data_256gib")
	connect.AssertEqual(t, pack.Data, 256*Gib)
	connect.AssertEqual(t, pack.PriceUsd, 0.75)
	for _, rail := range []string{DataRailCard, DataRailCrypto} {
		connect.AssertEqual(t, pack.SoldOn(rail), false)
	}
	for _, rail := range []string{DataRailX402, DataRailGamma} {
		connect.AssertEqual(t, pack.SoldOn(rail), true)
	}
	// it is under the $1 an x402 client pays without raising its cap
	connect.AssertEqual(t, pack.PriceUsd <= 1, true)
	// at the 1 TiB pack's rate
	connect.AssertEqual(t, pack.PriceUsd, c.DataCodeSku("data_1tib").PriceUsd*float64(pack.Data)/float64(Tib))
	connect.AssertEqual(t, c.DataCodeSku("data_2tib") == nil, true)
}

// TestConcurrentClientsEnforcementIsDark pins the staged rollout: the connected
// top-level client limit ships dark, so ConcurrentClientsExceeded must never
// reject while enforce_concurrent_clients is false — even far over the limit.
func TestConcurrentClientsEnforcementIsDark(t *testing.T) {
	c := Pro()

	connect.AssertEqual(t, c.EnforceConcurrentClients, false)

	// way over the free limit of 2, but enforcement is off -> no rejection
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(false, 999), false)
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(true, 999999), false)
}

// TestFeatureEnforcementIsDark pins the other staged rollout: SOCKS/WireGuard are
// Pro-only in the plan, but while enforce_features is false a free-tier client
// must still be ALLOWED to use them, so flipping the switch is what cuts them off
// -- not deploying the code.
func TestFeatureEnforcementIsDark(t *testing.T) {
	c := Pro()

	connect.AssertEqual(t, c.EnforceFeatures, false)

	// the plan says free does not include SOCKS/WireGuard...
	connect.AssertEqual(t, c.FeatureEnabled(false, FeatureSocksProxy), false)
	connect.AssertEqual(t, c.FeatureEnabled(false, FeatureWireguardProxy), false)
	// ...but while enforcement is dark, free is still allowed to use them
	connect.AssertEqual(t, c.FeatureAllowed(false, FeatureSocksProxy), true)
	connect.AssertEqual(t, c.FeatureAllowed(false, FeatureWireguardProxy), true)
}

// TestFeatureAllowed covers the feature logic itself with enforcement turned on.
func TestFeatureAllowed(t *testing.T) {
	c := &ProConfig{
		EnforceFeatures: true,
		Free:            ProTier{HttpProxy: true, HttpsProxy: true},
		Pro:             ProTier{HttpProxy: true, HttpsProxy: true, SocksProxy: true, WireguardProxy: true},
	}

	// free: http/https only
	connect.AssertEqual(t, c.FeatureAllowed(false, FeatureHttpProxy), true)
	connect.AssertEqual(t, c.FeatureAllowed(false, FeatureHttpsProxy), true)
	connect.AssertEqual(t, c.FeatureAllowed(false, FeatureSocksProxy), false)
	connect.AssertEqual(t, c.FeatureAllowed(false, FeatureWireguardProxy), false)

	// pro: everything
	connect.AssertEqual(t, c.FeatureAllowed(true, FeatureSocksProxy), true)
	connect.AssertEqual(t, c.FeatureAllowed(true, FeatureWireguardProxy), true)

	// unknown feature is never allowed
	connect.AssertEqual(t, c.FeatureAllowed(true, "unknown_feature"), false)
}

// TestConcurrentClientsExceeded covers the limit logic itself, independent of
// the rollout switch, by exercising a config with enforcement turned on.
func TestConcurrentClientsExceeded(t *testing.T) {
	c := &ProConfig{
		EnforceConcurrentClients: true,
		Free:                     ProTier{ConcurrentClients: 2},
		Pro:                      ProTier{ConcurrentClients: 1000},
	}

	// free: room for 2 connected clients
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(false, 0), false)
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(false, 1), false)
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(false, 2), true) // at limit -> no room
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(false, 3), true)

	// pro: much higher ceiling
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(true, 999), false)
	connect.AssertEqual(t, c.ConcurrentClientsExceeded(true, 1000), true)

	// limit <= 0 means unlimited
	unlimited := &ProConfig{EnforceConcurrentClients: true, Free: ProTier{ConcurrentClients: 0}}
	connect.AssertEqual(t, unlimited.ConcurrentClientsExceeded(false, 100000), false)
}
