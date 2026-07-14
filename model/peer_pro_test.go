package model

import (
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// TestIsPublicProvider pins the exemption from the connected top-level client
// limit: a client counts against the limit UNLESS it is a public provider, which
// means it offers BOTH public and stream provide modes. Offering only one of the
// two is not a public provider and still counts.
func TestIsPublicProvider(t *testing.T) {
	// exempt: public + stream
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{
		ProvideModes: []ProvideMode{ProvideModePublic, ProvideModeStream},
	}), true)
	// order does not matter
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{
		ProvideModes: []ProvideMode{ProvideModeStream, ProvideModePublic},
	}), true)
	// extra modes alongside both are still exempt
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{
		ProvideModes: []ProvideMode{ProvideModeNetwork, ProvideModePublic, ProvideModeStream},
	}), true)

	// not exempt: only one of the pair
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{
		ProvideModes: []ProvideMode{ProvideModePublic},
	}), false)
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{
		ProvideModes: []ProvideMode{ProvideModeStream},
	}), false)

	// not exempt: neither
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{
		ProvideModes: []ProvideMode{ProvideModeNetwork, ProvideModeFriendsAndFamily},
	}), false)
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{ProvideModes: []ProvideMode{}}), false)
	connect.AssertEqual(t, isPublicProvider(&NetworkPeer{}), false)

	// defensive: no peer
	connect.AssertEqual(t, isPublicProvider(nil), false)
}

// TestReferralBonusCount pins the referral payout cap: a referrer is paid for at
// most pro.yml referral.max_referrals referrals (10), no matter how many it has.
func TestReferralBonusCount(t *testing.T) {
	skipWithoutProYml(t)

	maxReferrals := Pro().MaxReferrals
	connect.AssertEqual(t, maxReferrals, 20)

	connect.AssertEqual(t, ReferralBonusCount(0), 0)
	connect.AssertEqual(t, ReferralBonusCount(1), 1)
	connect.AssertEqual(t, ReferralBonusCount(19), 19)
	connect.AssertEqual(t, ReferralBonusCount(20), 20)

	// capped
	connect.AssertEqual(t, ReferralBonusCount(21), 20)
	connect.AssertEqual(t, ReferralBonusCount(1000), 20)

	// defensive
	connect.AssertEqual(t, ReferralBonusCount(-1), 0)
}

// TestConcurrentGateIsFreeWhileDark pins that the concurrent-client gate does NO work
// while the rollout is dark. It runs on the auth hot path for every top-level client,
// so if it hit redis (an HGetAll of the peer meta) and the db (the Pro lookup) on
// every call, shipping the gate would cost every user real latency for a limit that
// is not even enforced yet.
//
// The proof is that it returns false with NO server context at all: a nil ctx would
// panic the moment either lookup ran.
func TestConcurrentGateIsFreeWhileDark(t *testing.T) {
	connect.AssertEqual(t, Pro().EnforceConcurrentClients, false)

	// no ctx, no network -- if this touched redis or the db it would panic
	connect.AssertEqual(t, NetworkConcurrentClientsExceeded(nil, server.NewId()), false)
}

// TestFeatureGateIsFreeWhileDark is the same property for the proxy feature gate:
// while enforce_features is dark it must not resolve Pro at all, and every tier is
// allowed. Same nil-ctx proof.
func TestFeatureGateIsFreeWhileDark(t *testing.T) {
	connect.AssertEqual(t, Pro().EnforceFeatures, false)

	connect.AssertEqual(t, NetworkFeatureAllowed(nil, server.NewId(), FeatureSocksProxy), true)
	connect.AssertEqual(t, NetworkFeatureAllowed(nil, server.NewId(), FeatureWireguardProxy), true)
}

// TestProxyGateIsFreeWhileDark pins the per-connection proxy gate's cost. It runs on
// EVERY SOCKS/HTTP connection the proxy accepts, so while the rollout is dark it must
// do no work at all -- no db lookup to resolve the proxy's network, no redis lookup for
// the entitlement. Shipping a disabled limit must not tax every connection.
//
// The proof is a nil ctx: either lookup would panic the moment it ran.
func TestProxyGateIsFreeWhileDark(t *testing.T) {
	connect.AssertEqual(t, Pro().EnforceFeatures, false)

	connect.AssertEqual(t, ProxyFeatureAllowed(nil, server.NewId(), FeatureSocksProxy), true)
	connect.AssertEqual(t, ProxyFeatureAllowed(nil, server.NewId(), FeatureWireguardProxy), true)
	connect.AssertEqual(t, ProxyFeatureAllowed(nil, server.NewId(), FeatureHttpsProxy), true)
}
