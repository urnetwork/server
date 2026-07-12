package model

import (
	"testing"

	"github.com/go-playground/assert/v2"
)

// TestProAbsent pins the contract for an environment with NO pro.yml.
//
// pro.yml is the product spec. Every limit, price, grant and duration comes from it and
// nowhere else. A missing one used to PANIC the process (RequireSimpleResource) -- so a
// partial deploy, or an env that had not been updated yet, would take the server down.
//
// It must not panic. But "not panicking" is the easy half: the hard half is that the ZERO
// values it falls back to are each dangerous in a DIFFERENT direction, which is why every
// one of them is asserted here:
//
//	concurrent limit 0  -> `0 <= connectedCount` is true for every count, so EVERY
//	                       client would be refused. The whole network locked out.
//	features all false  -> SOCKS and WireGuard refused for everyone, Pro included.
//	max_referrals 0     -> `count >= 0` is always true, so NO referral could ever be
//	                       created and none would ever pay a bonus.
//	data amounts 0      -> a "grant" of zero bytes, written as a real balance row.
//	duration 0          -> a purchased data code expires the instant it is created.
//	price 0             -> the plan is quoted at $0.00 and given away.
//
// So an absent pro.yml is INERT, not zeroed: nothing is enforced, nothing is granted,
// everything is allowed, and nothing is for sale.
//
// Every guard keys off the VALUE, never off a "was pro.yml loaded" flag. A flag has to be
// remembered, and a ProConfig literal that forgot to set it would be silently inert --
// which is exactly what happened when this was first written with an Enabled field, and
// what TestFeatureAllowed/TestConcurrentClientsExceeded caught. Keying off the value also
// covers a pro.yml that IS present but mis-specified (`data: 0`, `duration: 0`).
//
// This test runs in BOTH harnesses -- normally (pro.yml present) it asserts the configured
// contract, and under a WARP_CONFIG_HOME stripped of pro.yml it asserts the inert one --
// so the two cannot drift apart.
func TestProAbsent(t *testing.T) {
	c := Pro()

	if 0 < c.MaxConcurrentClients(true) {
		t.Log("pro.yml IS present; asserting the configured contract")
		assert.Equal(t, true, 0 < c.DataAmount(false))
		assert.Equal(t, true, 0 < c.DataAmount(true))
		assert.Equal(t, true, 0 < c.DataCodeDuration)
		assert.Equal(t, true, 0 < c.ReferralGrantPeriod())
		assert.Equal(t, true, 0 < c.PriceMonthlyUsd())
		assert.Equal(t, true, 0 < c.MaxReferrals)
		return
	}

	// --- the contract when pro.yml is NOT on the config path ---
	t.Log("pro.yml is NOT present; asserting the inert contract")

	// 1. concurrent limits report DISABLED -- nothing is ever refused, at any count.
	//    A zero limit would otherwise mean `0 <= n` for all n: everyone locked out.
	for _, pro := range []bool{false, true} {
		for _, connectedCount := range []int{0, 1, 2, 3, 100, 1_000_000} {
			assert.Equal(t, false, c.ConcurrentClientsExceeded(pro, connectedCount))
		}
	}

	// 2. every feature is ALLOWED, on both tiers. Zeroed features would otherwise read as
	//    "false = not in your plan" and cut off SOCKS/WireGuard for everybody.
	for _, pro := range []bool{false, true} {
		for _, feature := range []string{
			FeatureHttpProxy,
			FeatureHttpsProxy,
			FeatureSocksProxy,
			FeatureWireguardProxy,
			"a-feature-that-does-not-exist",
		} {
			assert.Equal(t, true, c.FeatureAllowed(pro, feature))
		}
	}

	// 3. referrals are UNCAPPED, not capped at zero.
	for _, referralCount := range []int{0, 1, 9, 10, 11, 1000} {
		assert.Equal(t, false, c.ReferralsCapped(referralCount))
		assert.Equal(t, 0, ReferralBonusCount(referralCount))
	}

	// 4. the amounts are zero -- which is why the grant tasks check the AMOUNT and noop,
	//    rather than granting what they read here.
	assert.Equal(t, ByteCount(0), c.DataAmount(false))
	assert.Equal(t, ByteCount(0), c.DataAmount(true))
	assert.Equal(t, ByteCount(0), c.ReferralBonus)

	// 5. no price and no duration -- which is why every purchase path refuses. A zero
	//    duration would sell a code that expires on arrival; a zero price would let the
	//    webhook's `amount >= price - tolerance` check pass for ANY payment, including
	//    none.
	assert.Equal(t, float64(0), c.PriceMonthlyUsd())
	assert.Equal(t, float64(0), c.PriceYearlyUsd())
	assert.Equal(t, true, c.DataCodeDuration <= 0)
	assert.Equal(t, 0, len(c.DataCodeSkus))

	// 6. the referral grant period is NEVER zero, even here. The referral task reschedules
	//    itself at now+period, so a zero period is not "no delay" -- it is a HOT LOOP:
	//    fire, schedule for now, fire again, forever. A missing config file must not spin
	//    the task worker.
	assert.Equal(t, true, 0 < c.ReferralGrantPeriod())
}

// skipWithoutProYml skips a test that asserts the CONFIGURED product spec (caps,
// amounts, prices from pro.yml) when this environment has no pro.yml -- the stripped
// harness described in TestProAbsent, which owns the absent contract. Without this,
// running the suite in that harness reads as a wall of defects when the runtime is
// doing exactly what it should: noop grants, refused purchases, uncapped referrals.
func skipWithoutProYml(t testing.TB) {
	if Pro().MaxConcurrentClients(true) == 0 {
		t.Skip("pro.yml is not present in this environment; see TestProAbsent")
	}
}
