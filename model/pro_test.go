package model

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
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
	assert.Equal(t, c.MaxConcurrentClients(false), 2)
	assert.Equal(t, c.DataAmount(false), ByteCount(30)*gib)
	assert.Equal(t, c.DataPeriod(false), 24*time.Hour)
	assert.Equal(t, c.FeatureEnabled(false, FeatureHttpProxy), true)
	assert.Equal(t, c.FeatureEnabled(false, FeatureHttpsProxy), true)
	assert.Equal(t, c.FeatureEnabled(false, FeatureSocksProxy), false)     // Pro-only
	assert.Equal(t, c.FeatureEnabled(false, FeatureWireguardProxy), false) // Pro-only

	// pro (UR Pro)
	assert.Equal(t, c.MaxConcurrentClients(true), 1000)
	assert.Equal(t, c.DataAmount(true), ByteCount(10)*tib)
	assert.Equal(t, c.DataPeriod(true), 720*time.Hour)
	assert.Equal(t, c.FeatureEnabled(true, FeatureHttpProxy), true)
	assert.Equal(t, c.FeatureEnabled(true, FeatureSocksProxy), true)
	assert.Equal(t, c.FeatureEnabled(true, FeatureWireguardProxy), true)

	// referrals
	assert.Equal(t, c.ReferralBonus, ByteCount(3)*gib)
	assert.Equal(t, c.ReferralPeriod, 24*time.Hour)
	assert.Equal(t, c.MaxReferrals, 10)

	// data codes
	assert.Equal(t, c.DataCodeDuration, 8760*time.Hour)
	assert.Equal(t, len(c.DataCodeSkus), 2)

	// unknown feature -> false
	assert.Equal(t, c.FeatureEnabled(true, "unknown_feature"), false)

}

// TestConcurrentClientsEnforcementIsDark pins the staged rollout: the connected
// top-level client limit ships dark, so ConcurrentClientsExceeded must never
// reject while enforce_concurrent_clients is false — even far over the limit.
func TestConcurrentClientsEnforcementIsDark(t *testing.T) {
	c := Pro()

	assert.Equal(t, c.EnforceConcurrentClients, false)

	// way over the free limit of 2, but enforcement is off -> no rejection
	assert.Equal(t, c.ConcurrentClientsExceeded(false, 999), false)
	assert.Equal(t, c.ConcurrentClientsExceeded(true, 999999), false)
}

// TestFeatureEnforcementIsDark pins the other staged rollout: SOCKS/WireGuard are
// Pro-only in the plan, but while enforce_features is false a free-tier client
// must still be ALLOWED to use them, so flipping the switch is what cuts them off
// -- not deploying the code.
func TestFeatureEnforcementIsDark(t *testing.T) {
	c := Pro()

	assert.Equal(t, c.EnforceFeatures, false)

	// the plan says free does not include SOCKS/WireGuard...
	assert.Equal(t, c.FeatureEnabled(false, FeatureSocksProxy), false)
	assert.Equal(t, c.FeatureEnabled(false, FeatureWireguardProxy), false)
	// ...but while enforcement is dark, free is still allowed to use them
	assert.Equal(t, c.FeatureAllowed(false, FeatureSocksProxy), true)
	assert.Equal(t, c.FeatureAllowed(false, FeatureWireguardProxy), true)
}

// TestFeatureAllowed covers the feature logic itself with enforcement turned on.
func TestFeatureAllowed(t *testing.T) {
	c := &ProConfig{
		EnforceFeatures: true,
		Free:            ProTier{HttpProxy: true, HttpsProxy: true},
		Pro:             ProTier{HttpProxy: true, HttpsProxy: true, SocksProxy: true, WireguardProxy: true},
	}

	// free: http/https only
	assert.Equal(t, c.FeatureAllowed(false, FeatureHttpProxy), true)
	assert.Equal(t, c.FeatureAllowed(false, FeatureHttpsProxy), true)
	assert.Equal(t, c.FeatureAllowed(false, FeatureSocksProxy), false)
	assert.Equal(t, c.FeatureAllowed(false, FeatureWireguardProxy), false)

	// pro: everything
	assert.Equal(t, c.FeatureAllowed(true, FeatureSocksProxy), true)
	assert.Equal(t, c.FeatureAllowed(true, FeatureWireguardProxy), true)

	// unknown feature is never allowed
	assert.Equal(t, c.FeatureAllowed(true, "unknown_feature"), false)
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
	assert.Equal(t, c.ConcurrentClientsExceeded(false, 0), false)
	assert.Equal(t, c.ConcurrentClientsExceeded(false, 1), false)
	assert.Equal(t, c.ConcurrentClientsExceeded(false, 2), true) // at limit -> no room
	assert.Equal(t, c.ConcurrentClientsExceeded(false, 3), true)

	// pro: much higher ceiling
	assert.Equal(t, c.ConcurrentClientsExceeded(true, 999), false)
	assert.Equal(t, c.ConcurrentClientsExceeded(true, 1000), true)

	// limit <= 0 means unlimited
	unlimited := &ProConfig{EnforceConcurrentClients: true, Free: ProTier{ConcurrentClients: 0}}
	assert.Equal(t, unlimited.ConcurrentClientsExceeded(false, 100000), false)
}
