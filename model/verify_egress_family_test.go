// Per-family attribution: one live address per family stays attributable, a
// repeated or unrecorded family fails closed, and an address shared by two
// providers still resolves to nobody.
package model

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/server"
)

func TestVerifyEgressFamily(t *testing.T) {
	for address, want := range map[string]string{
		"192.0.2.10":          "4",
		"::ffff:192.0.2.10":   "4",
		"2001:db8::10":        "6",
		"2604:2d81:3389:24::": "6",
	} {
		if got := verifyEgressFamily(netip.MustParseAddr(address)); got != want {
			t.Fatalf("family of %s = %s, want %s", address, got, want)
		}
	}
}

func TestVerifyEgressFamiliesDistinct(t *testing.T) {
	for _, test := range []struct {
		families []any
		want     bool
	}{
		{[]any{"4", "6"}, true},
		{[]any{"6", "4"}, true},
		{[]any{"4", "4"}, false},
		{[]any{"6", "6"}, false},
		{[]any{"4", nil}, false},
		{[]any{nil, "6"}, false},
		{[]any{"4", "6", "6"}, false},
		{[]any{"4", "x"}, false},
	} {
		if got := verifyEgressFamiliesDistinct(test.families); got != test.want {
			t.Fatalf("distinct %v = %t, want %t", test.families, got, test.want)
		}
	}
}

func perFamilyTestSettings(perFamily bool) *VerifySettings {
	settings := DefaultVerifySettings()
	settings.EgressHashKey = []byte("per-family-egress-test")
	settings.EgressPerFamily = perFamily
	return settings
}

// One IPv4 and one IPv6 transport: strict attribution refuses both, per-family
// attribution resolves either and keeps the provider assignable.
func TestVerifyConnectionEgressPerFamilyAttributesDualStack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, perFamily := range []bool{false, true} {
			settings := perFamilyTestSettings(perFamily)
			clientId := server.NewId()
			v4, v6 := netip.MustParseAddr("192.0.2.41"), netip.MustParseAddr("2001:db8:41::1")
			if perFamily {
				v4, v6 = netip.MustParseAddr("192.0.2.42"), netip.MustParseAddr("2001:db8:42::1")
			}
			testVerifySetProvideModes(ctx, clientId)
			FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), v4, settings)
			FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), v6, settings)
			for _, ip := range []netip.Addr{v4, v6} {
				got := ResolveVerifyEgress(ctx, ip, settings)
				if perFamily && (got == nil || *got != clientId) {
					t.Fatalf("per-family %s resolved to %v, want %s", ip, got, clientId)
				}
				if !perFamily && got != nil {
					t.Fatalf("strict attribution resolved %s with two live addresses", ip)
				}
			}
			if testVerifyEligible(ctx, clientId) != perFamily {
				t.Fatalf("per-family=%t eligibility = %t", perFamily, !perFamily)
			}
		}
	})
}

// Two live addresses of one family, or a family fed before the setting was
// on, fail closed exactly as the strict rule does.
func TestVerifyConnectionEgressPerFamilyRefusesRepeatedOrUnknownFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := perFamilyTestSettings(true)

		repeated := server.NewId()
		first, second := netip.MustParseAddr("192.0.2.51"), netip.MustParseAddr("192.0.2.52")
		testVerifySetProvideModes(ctx, repeated)
		FeedVerifyConnectionEgress(ctx, repeated, server.NewId(), first, settings)
		FeedVerifyConnectionEgress(ctx, repeated, server.NewId(), second, settings)
		if ResolveVerifyEgress(ctx, first, settings) != nil || ResolveVerifyEgress(ctx, second, settings) != nil || testVerifyEligible(ctx, repeated) {
			t.Fatal("two live IPv4 addresses passed per-family attribution")
		}

		unknown := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("2001:db8:53::1")
		testVerifySetProvideModes(ctx, unknown)
		FeedVerifyConnectionEgress(ctx, unknown, server.NewId(), v4, perFamilyTestSettings(false))
		FeedVerifyConnectionEgress(ctx, unknown, server.NewId(), v6, settings)
		if ResolveVerifyEgress(ctx, v4, settings) != nil || ResolveVerifyEgress(ctx, v6, settings) != nil || testVerifyEligible(ctx, unknown) {
			t.Fatal("an unrecorded family passed per-family attribution")
		}
	})
}

// A NAT address shared with another provider stays ambiguous. The provider's
// own IPv6 still resolves, but it is not assignable as a hop, because a
// request through its IPv4 could not be attributed.
func TestVerifyConnectionEgressPerFamilyKeepsSharedAddressAmbiguous(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := perFamilyTestSettings(true)
		clientId, neighbor := server.NewId(), server.NewId()
		shared, v6 := netip.MustParseAddr("198.51.100.61"), netip.MustParseAddr("2001:db8:61::1")
		testVerifySetProvideModes(ctx, clientId)
		testVerifySetProvideModes(ctx, neighbor)
		FeedVerifyConnectionEgress(ctx, neighbor, server.NewId(), shared, settings)
		FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), shared, settings)
		FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), v6, settings)
		if ResolveVerifyEgress(ctx, shared, settings) != nil {
			t.Fatal("a shared address resolved to one of its providers")
		}
		if got := ResolveVerifyEgress(ctx, v6, settings); got == nil || *got != clientId {
			t.Fatalf("own IPv6 resolved to %v, want %s", got, clientId)
		}
		if testVerifyEligible(ctx, clientId) {
			t.Fatal("a provider behind an ambiguous address became assignable")
		}
	})
}
