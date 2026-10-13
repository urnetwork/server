// Per-family source attribution (VALIDATOR.md §8.2): a provider may hold one
// live egress in each family, an IPv6 /64 counting as one egress, and a hop
// resolves when its source is its family's only live egress for the
// provider: by its exact address first, else by its /64 when one client holds
// it. Fixtures use documentation addresses in the fixture's isolated Redis
// database.
package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func testVerifyFamilySettings(name string) *VerifySettings {
	settings := DefaultVerifySettings()
	settings.EgressHashKey = []byte(name)
	return settings
}

// Feeds one connection lease per address, as a provider's family-pinned
// transports do (IPV6.md A), and returns each address's connection id.
func testVerifyFeedAddresses(ctx context.Context, clientId server.Id, settings *VerifySettings, ips ...netip.Addr) map[netip.Addr]server.Id {
	connectionIds := map[netip.Addr]server.Id{}
	for _, ip := range ips {
		connectionIds[ip] = server.NewId()
		FeedVerifyConnectionEgress(ctx, clientId, connectionIds[ip], ip, settings)
	}
	return connectionIds
}

// Requires the keys to be absent, one EXISTS per key as on a cluster.
func requireVerifyKeysAbsent(t testing.TB, ctx context.Context, keys ...string) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		for _, key := range keys {
			count, err := r.Exists(ctx, key).Result()
			server.Raise(err)
			if count != 0 {
				t.Fatalf("key %s survived", key)
			}
		}
	})
}

// Feeds one address the way a v2 writer did: the bare exact-address hash in
// the v2 reverse index with the expiry alone, the shared forward entry, a
// lease until `expires`.
func testVerifyFeedV2Address(ctx context.Context, clientId server.Id, ip netip.Addr, owner string, now time.Time, expires time.Time, settings *VerifySettings) {
	addressHash := VerifyEgressIndexHashWithSettings(ip, settings)
	server.Redis(ctx, func(r server.RedisClient) {
		updateVerifyEgressLease(ctx, r, verifyClientEgressV2Key(clientId), clientId, hex.EncodeToString(addressHash[:]), "", owner, now, expires)
	})
	updateVerifyEligibleMembership(ctx, clientId)
}

// The client's live egress as the resolver reads it now.
func testVerifyLiveEgress(ctx context.Context, clientId server.Id) (live verifyLiveEgress) {
	server.Redis(ctx, func(r server.RedisClient) {
		live = readVerifyLiveEgress(ctx, r, clientId, uint64(server.NowUtc().UnixMilli()))
	})
	return
}

// Requires the eligible-set sampler to draw exactly this provider.
func requireVerifyAssignable(t testing.TB, ctx context.Context, clientId server.Id, settings *VerifySettings) {
	t.Helper()
	if next, _ := SampleVerifyNextHop(ctx, nil, settings); next == nil || *next != clientId {
		t.Fatalf("provider %s was not assignable: drew %v", clientId, next)
	}
}

// §8.2, IPV6.md C7: the validator alternates the family each hop tries first,
// so a provider's IPv4 and IPv6 addresses must both attribute. Each is its
// family's only live address; a hop from either, an IPv4-mapped source
// included, resolves to the provider, which stays eligible and assignable.
func TestVerifyEgressOneAddressPerFamilyBothResolve(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("one-address-per-family-test")
		clientId := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.41"), netip.MustParseAddr("2001:db8:41::1")
		testVerifySetProvideModes(ctx, clientId)
		testVerifyFeedAddresses(ctx, clientId, settings, v4, v6)
		for _, ip := range []netip.Addr{v4, v6, netip.AddrFrom16(v4.As16())} {
			requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		}
		requireVerifyAssignable(t, ctx, clientId, settings)
	})
}

// §8.2: two live addresses of one family (two IPv4 addresses, or two IPv6
// /64s) stay ambiguous and refused: neither resolves. The provider's address
// in the other family still resolves and keeps it assignable, since a hop
// that lands on the refused family is retried on the other. Closing the
// duplicate restores both families.
func TestVerifyEgressSecondAddressOfAFamilyIsRefused(t *testing.T) {
	for _, duplicate := range []netip.Addr{netip.MustParseAddr("192.0.2.43"), netip.MustParseAddr("2001:db8:43::1")} {
		t.Run(fmt.Sprintf("ipv%d", server.IpVersionForAddr(duplicate)), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				settings := testVerifyFamilySettings("second-address-of-a-family-test")
				clientId := server.NewId()
				v4, v6 := netip.MustParseAddr("192.0.2.42"), netip.MustParseAddr("2001:db8:42::1")
				testVerifySetProvideModes(ctx, clientId)
				connectionIds := testVerifyFeedAddresses(ctx, clientId, settings, v4, v6, duplicate)
				refused, other := []netip.Addr{v4, duplicate}, v6
				if duplicate.Is6() {
					refused, other = []netip.Addr{v6, duplicate}, v4
				}
				for _, ip := range refused {
					if got := ResolveVerifyEgress(ctx, ip, settings); got != nil {
						t.Fatalf("%s resolved beside another live address of its family: %s", ip, got)
					}
				}
				if got := ResolveVerifyEgress(ctx, other, settings); got == nil || *got != clientId {
					t.Fatalf("the other family's only address did not resolve: %v", got)
				}
				requireVerifyAssignable(t, ctx, clientId, settings)
				ClearVerifyConnectionEgress(ctx, clientId, connectionIds[duplicate], duplicate, settings)
				requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)
				requireVerifyEgressLeaseOwner(t, ctx, v6, clientId, settings)
			})
		})
	}
}

// §8.2: IPv6 privacy (temporary) addresses rotate inside the host's /64, so
// the provider's later egress connections leave from another address than
// its pinned IPv6 transport's. The /64 is one egress: a hop from the rotated
// address resolves through the /64, which one client holds, and a transport
// reconnecting from it adds its address to that egress, not a second one, so
// the provider stays assignable.
func TestVerifyEgressIpv6TemporaryAddressRotation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("temporary-address-rotation")
		clientId := server.NewId()
		v4 := netip.MustParseAddr("192.0.2.61")
		transportV6, rotatedV6 := netip.MustParseAddr("2001:db8:61::a1"), netip.MustParseAddr("2001:db8:61::b2")
		testVerifySetProvideModes(ctx, clientId)
		testVerifyFeedAddresses(ctx, clientId, settings, v4, transportV6)
		requireVerifyEgressLeaseOwner(t, ctx, rotatedV6, clientId, settings)

		// an H3 transport reconnects after rotation from the new address
		testVerifyFeedAddresses(ctx, clientId, settings, rotatedV6)
		for _, ip := range []netip.Addr{v4, transportV6, rotatedV6} {
			requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		}
		if live := testVerifyLiveEgress(ctx, clientId); len(live.networks(4)) != 1 || len(live.networks(6)) != 1 || len(live.byFamily[6]) != 2 {
			t.Fatalf("one IPv4 address and two addresses of one /64 = %+v, want one egress a family", live)
		}
		requireVerifyAssignable(t, ctx, clientId, settings)
	})
}

// §8.2 (probe TestProbeVerifyEgressTwoProvidersInOneSlash64StayAttributable):
// two providers in one dual-stack home, a phone and a laptop, each with one
// transport that landed on IPv6, hold distinct addresses in the home's /64.
// Each address resolves to its own provider, which stays assignable. Only an
// address of the /64 that neither holds, as a rotated temporary address,
// resolves to neither: two clients hold the /64.
func TestVerifyEgressTwoProvidersInOneSlash64StayAttributable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("two-providers-one-slash-64")
		phone, laptop := server.NewId(), server.NewId()
		addresses := map[server.Id]netip.Addr{
			phone:  netip.MustParseAddr("2001:db8:e0::1"),
			laptop: netip.MustParseAddr("2001:db8:e0::2"),
		}
		for clientId, ip := range addresses {
			testVerifySetProvideModes(ctx, clientId)
			testVerifyFeedAddresses(ctx, clientId, settings, ip)
		}
		for clientId, ip := range addresses {
			requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		}
		if got := ResolveVerifyEgress(ctx, netip.MustParseAddr("2001:db8:e0::99"), settings); got != nil {
			t.Fatalf("an address of a /64 two providers hold resolved to %s", got)
		}
		// draw each alone: the other leaves the eligible set first
		for _, pair := range [][2]server.Id{{phone, laptop}, {laptop, phone}} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.SRem(ctx, verifyEligibleKey, pair[1].String()).Err())
				server.Raise(r.SAdd(ctx, verifyEligibleKey, pair[0].String()).Err())
			})
			requireVerifyAssignable(t, ctx, pair[0], settings)
		}
	})
}

// §5.1, §8.2: eligibility needs one family that attributes to the provider. A
// dual-stack provider behind a shared IPv4 (CGNAT, or a NAT64 pool its IPv4
// leaves through over 464XLAT, IPV6.md A5) with a unique IPv6 /64 stays
// assignable when any other client connects from the shared IPv4.
func TestVerifyEgressSharedIpv4KeepsProviderAssignable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("shared-ipv4")
		victim, neighbor := server.NewId(), server.NewId()
		shared := netip.MustParseAddr("198.51.100.62")
		victimV6, neighborV6 := netip.MustParseAddr("2001:db8:62::1"), netip.MustParseAddr("2001:db8:63::1")
		testVerifySetProvideModes(ctx, victim)
		testVerifyFeedAddresses(ctx, victim, settings, shared, victimV6)
		requireVerifyAssignable(t, ctx, victim, settings)
		// the neighbor need not even be a provider: any client's transport
		// from the shared address marks it ambiguous
		testVerifyFeedAddresses(ctx, neighbor, settings, shared, neighborV6)
		if got := ResolveVerifyEgress(ctx, shared, settings); got != nil {
			t.Fatalf("the shared IPv4 resolved to %s", got)
		}
		requireVerifyEgressLeaseOwner(t, ctx, victimV6, victim, settings)
		requireVerifyAssignable(t, ctx, victim, settings)
	})
}

// §8.2: a live address whose forward entry names another client, or marks
// it ambiguous, does not attribute its family: it does not resolve to the
// provider, and a hop from it is answered as retryable (a co-located
// provider's address). The provider stays assignable through its other
// family, whose address still resolves to it.
func TestVerifyEgressAddressAttributedToAnotherClientDoesNotAttribute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("foreign-address")
		clientId := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.67"), netip.MustParseAddr("2001:db8:67::1")
		testVerifySetProvideModes(ctx, clientId)
		testVerifyFeedAddresses(ctx, clientId, settings, v4, v6)
		_, v4Field := verifyClientEgressField(v4, settings)
		for _, forward := range []string{verifyEgressAmbiguous, server.NewId().String()} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, verifyEgressKeyFromHex(v4Field.address), forward, settings.EgressTtl).Err())
				server.Raise(r.SAdd(ctx, verifyEligibleKey, clientId.String()).Err())
			})
			if got := ResolveVerifyEgress(ctx, v4, settings); got != nil && *got == clientId {
				t.Fatalf("with its IPv4 forward entry %q the address resolved to the provider", forward)
			}
			requireVerifyEgressLeaseOwner(t, ctx, v6, clientId, settings)
			requireVerifyAssignable(t, ctx, clientId, settings)
		}
	})
}

// §8.2: each address keeps its own lease. A refresh of one family's address
// does not extend the other's; once the other lapses, the refreshed family
// still attributes alone and the provider stays eligible.
func TestVerifyEgressFamilyLeaseExpiresIndependently(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("family-lease-expiry-test")
		for _, pair := range []struct{ lapsing, kept netip.Addr }{
			{lapsing: netip.MustParseAddr("192.0.2.44"), kept: netip.MustParseAddr("2001:db8:44::1")},
			{lapsing: netip.MustParseAddr("2001:db8:44:1::2"), kept: netip.MustParseAddr("192.0.2.54")},
		} {
			clientId, lapsing, kept := server.NewId(), pair.lapsing, pair.kept
			reverseKey := verifyClientEgressKey(clientId)
			lapsingField, _ := verifyClientEgressField(lapsing, settings)
			keptField, _ := verifyClientEgressField(kept, settings)
			lapsingSuffix, keptSuffix := verifyClientEgressValueSuffix(lapsing, settings), verifyClientEgressValueSuffix(kept, settings)
			testVerifySetProvideModes(ctx, clientId)
			now := server.NowUtc()
			server.Redis(ctx, func(r server.RedisClient) {
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, lapsingField, lapsingSuffix, "lapsing", now, now.Add(time.Minute))
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, keptField, keptSuffix, "kept", now, now.Add(time.Minute))
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, keptField, keptSuffix, "kept", now.Add(time.Minute/2), now.Add(10*time.Minute))
			})
			before := uint64(now.Add(time.Minute / 2).UnixMilli())
			for _, ip := range []netip.Addr{lapsing, kept} {
				if got := resolveVerifyEgressAt(ctx, ip, settings, before); got == nil || *got != clientId {
					t.Fatalf("%s did not resolve before either lease lapsed: %v", ip, got)
				}
			}
			after := uint64(now.Add(2 * time.Minute).UnixMilli())
			if got := resolveVerifyEgressAt(ctx, lapsing, settings, after); got != nil {
				t.Fatalf("lapsed %s lease still attributed: %s", lapsing, got)
			}
			if got := resolveVerifyEgressAt(ctx, kept, settings, after); got == nil || *got != clientId {
				t.Fatalf("%s lost attribution when the other family lapsed: %v", kept, got)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				if !verifyClientHasLiveEgress(ctx, r, clientId, after) {
					t.Fatal("provider lost eligibility when one family lapsed")
				}
			})
		}
	})
}

// §8.2: the v4 reverse index holds one field per live address, naming its
// address hash and family, and an IPv6 address's /64; its value carries the
// address's egress-IP-hash. No current writer feeds the v2 index. A close
// withdraws only its own family's field, lease and forward entries, an IPv6
// address's /64 entry included; reaping withdraws every family's.
func TestVerifyEgressReverseIndexCarriesTheFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("reverse-index-family-test")
		clientId := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.45"), netip.MustParseAddr("2001:db8:45::1")
		testVerifySetProvideModes(ctx, clientId)
		connectionIds := testVerifyFeedAddresses(ctx, clientId, settings, v4, v6)
		reverseKey := verifyClientEgressKey(clientId)
		fields, parsed := map[netip.Addr]string{}, map[netip.Addr]verifyEgressField{}
		for _, ip := range []netip.Addr{v4, v6} {
			fields[ip], parsed[ip] = verifyClientEgressField(ip, settings)
		}
		requireFields := func(want ...netip.Addr) {
			t.Helper()
			server.Redis(ctx, func(r server.RedisClient) {
				got, err := r.HGetAll(ctx, reverseKey).Result()
				server.Raise(err)
				if len(got) != len(want) {
					t.Fatalf("reverse fields = %v, want %d", got, len(want))
				}
				for _, ip := range want {
					scoreHash := VerifyEgressIpHashWithSettings(ip, settings)
					if _, score := parseVerifyClientEgressValue(got[fields[ip]]); score != hex.EncodeToString(scoreHash[:]) {
						t.Fatalf("reverse fields = %v, want %s carrying its egress-IP-hash", got, fields[ip])
					}
				}
			})
			requireVerifyKeysAbsent(t, ctx, verifyClientEgressV2Key(clientId))
		}
		requireWithdrawn := func(ip netip.Addr) {
			t.Helper()
			requireVerifyKeysAbsent(t, ctx, verifyClientEgressLeaseKey(reverseKey, parsed[ip].address))
			for _, egressHashHex := range parsed[ip].forwardHashes() {
				requireVerifyKeysAbsent(t, ctx, verifyEgressKeyFromHex(egressHashHex), verifyForwardEgressLeaseKey(egressHashHex))
			}
		}
		requireFields(v4, v6)
		if len(parsed[v6].forwardHashes()) != 2 {
			t.Fatalf("an IPv6 field feeds %v, want its address and its /64", parsed[v6].forwardHashes())
		}

		ClearVerifyConnectionEgress(ctx, clientId, connectionIds[v6], v6, settings)
		requireFields(v4)
		requireWithdrawn(v6)
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)

		testVerifyFeedAddresses(ctx, clientId, settings, v6)
		requireFields(v4, v6)
		RemoveVerifyEgressForClient(ctx, clientId)
		for _, ip := range []netip.Addr{v4, v6} {
			requireWithdrawn(ip)
		}
		requireVerifyKeysAbsent(t, ctx, reverseKey)
		if testVerifyEligible(ctx, clientId) {
			t.Fatal("reaped provider stayed eligible")
		}
	})
}

// §8.2: a v4 field is the address hash and family, and for IPv6 the /64's
// hash; anything else, a bare v2 field included, is a bare address with no
// family. A v4 value is the expiry and the address's egress-IP-hash.
func TestVerifyEgressFieldNamesItsAddressAndFamily(t *testing.T) {
	settings := &VerifySettings{EgressHashKey: []byte("field-parse-test"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}
	hexOf := func(hash [32]byte) string { return hex.EncodeToString(hash[:]) }
	v4, v6 := netip.MustParseAddr("192.0.2.46"), netip.MustParseAddr("2001:db8:46::1")
	v4Address := hexOf(VerifyEgressIndexHashWithSettings(v4, settings))
	v6Address, v6Network := hexOf(VerifyEgressIndexHashWithSettings(v6, settings)), hexOf(verifyEgressNetworkHash(v6, settings))
	for ip, want := range map[netip.Addr]struct {
		field  string
		parsed verifyEgressField
	}{
		v4: {v4Address + "_4", verifyEgressField{address: v4Address, family: 4, network: v4Address}},
		v6: {v6Address + "_6_" + v6Network, verifyEgressField{address: v6Address, family: 6, network: v6Network}},
	} {
		field, parsed := verifyClientEgressField(ip, settings)
		if field != want.field || parsed != want.parsed || parseVerifyClientEgressField(field) != want.parsed {
			t.Fatalf("field for %s = %q %+v, want %q %+v", ip, field, parsed, want.field, want.parsed)
		}
	}
	for _, field := range []string{"abc", "abc_5", "abc_4_def", "abc_6", "abc_6_def_ghi"} {
		if got := parseVerifyClientEgressField(field); got != (verifyEgressField{address: "abc", network: "abc"}) {
			t.Fatalf("field %q parsed as %+v, want a bare address", field, got)
		}
	}
	for value, want := range map[string]struct {
		expireMs uint64
		score    string
	}{"123_ab": {123, "ab"}, "123": {123, ""}, "x": {0, ""}} {
		if expireMs, score := parseVerifyClientEgressValue(value); expireMs != want.expireMs || score != want.score {
			t.Fatalf("value %q parsed as %d %q", value, expireMs, score)
		}
	}
	live := newVerifyLiveEgress(map[string]string{"a_4": "5_aa", "c_6_n": "5", "b_6_n": "5_bb", "d_7": "5"}, map[string]string{"e": "5"})
	wantV6 := []verifyLiveEgressAddress{{verifyEgressField{address: "b", family: 6, network: "n"}, "bb"}, {verifyEgressField{address: "c", family: 6, network: "n"}, ""}}
	if !slices.Equal(live.byFamily[4], []verifyLiveEgressAddress{{verifyEgressField{address: "a", family: 4, network: "a"}, "aa"}}) || !slices.Equal(live.byFamily[6], wantV6) || len(live.byFamily) != 2 || !slices.Equal(live.legacy, []string{"e"}) {
		t.Fatalf("live egress = %+v", live)
	}
	if !slices.Equal(live.networks(4), []string{"a"}) || !slices.Equal(live.networks(6), []string{"n"}) {
		t.Fatalf("live egresses = %v and %v, want one a family", live.networks(4), live.networks(6))
	}
}

// §8.2: the egress a family counts an address as is an IPv4 address itself,
// or an IPv6 address's /64: addresses in one /64 share it and other /64s do
// not, and it is apart from every address's own hash.
func TestVerifyEgressNetworkHashCoversAnIpv6Slash64(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testVerifyEgressNetworkHashCoversAnIpv6Slash64(t)
	})
}

// The unkeyed hashes use the client ip hash pepper, which the test
// environment provides.
func testVerifyEgressNetworkHashCoversAnIpv6Slash64(t testing.TB) {
	for _, settings := range []*VerifySettings{{EgressHashKey: []byte("slash-64-test"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}, {EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}} {
		inside := []netip.Addr{netip.MustParseAddr("2001:db8:68::1"), netip.MustParseAddr("2001:db8:68::ffff:1"), netip.MustParseAddr("2001:db8:68:0:a:b:c:d")}
		for _, ip := range inside[1:] {
			if verifyEgressNetworkHash(ip, settings) != verifyEgressNetworkHash(inside[0], settings) {
				t.Fatalf("%s and %s in one /64 have two network hashes (keyed=%t)", ip, inside[0], 0 < len(settings.EgressHashKey))
			}
			if VerifyEgressIndexHashWithSettings(ip, settings) == VerifyEgressIndexHashWithSettings(inside[0], settings) {
				t.Fatalf("%s and %s share an address hash", ip, inside[0])
			}
		}
		if verifyEgressNetworkHash(netip.MustParseAddr("2001:db8:68:1::1"), settings) == verifyEgressNetworkHash(inside[0], settings) {
			t.Fatal("two /64s share a network hash")
		}
		for _, ip := range inside {
			if verifyEgressNetworkHash(ip, settings) == VerifyEgressIndexHashWithSettings(ip, settings) {
				t.Fatalf("the /64 of %s hashes as an address", ip)
			}
		}
		v4 := netip.MustParseAddr("192.0.2.68")
		if verifyEgressNetworkHash(v4, settings) != VerifyEgressIndexHashWithSettings(v4, settings) {
			t.Fatal("an IPv4 address is not its own egress")
		}
	}
}

// §8.2 rolling deploy: a v2 writer's entry, a bare exact-address hash, still
// attributes its address by the v2 rule (the client's one live v2 address)
// and keeps the provider eligible. An IPv6 v2 entry resolves for its exact
// address, under the address's hash. A v4 address beside a v2 entry resolves
// too: the versions are judged apart. No current writer feeds v2, so an
// entry ends one EgressTtl after its last write.
func TestVerifyEgressV2EntryAttributesForOneTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("v2-dual-read-test")
		clientId, legacyV6Client := server.NewId(), server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.47"), netip.MustParseAddr("2001:db8:47::1")
		legacyV6 := netip.MustParseAddr("2001:db8:48::1")
		testVerifySetProvideModes(ctx, clientId)
		testVerifySetProvideModes(ctx, legacyV6Client)
		oldWrite := server.NowUtc().Add(-time.Second)
		testVerifyFeedV2Address(ctx, clientId, v4, server.NewId().String(), oldWrite, oldWrite.Add(settings.EgressTtl), settings)
		testVerifyFeedV2Address(ctx, legacyV6Client, legacyV6, server.NewId().String(), oldWrite, oldWrite.Add(settings.EgressTtl), settings)
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)
		requireVerifyEgressLeaseOwner(t, ctx, legacyV6, legacyV6Client, settings)
		// the v2 rule is the exact address: another in its /64 is unknown
		if got := ResolveVerifyEgress(ctx, netip.MustParseAddr("2001:db8:48::2"), settings); got != nil {
			t.Fatalf("an address beside a v2 IPv6 entry resolved to %s", got)
		}

		testVerifyFeedAddresses(ctx, clientId, settings, v6)
		requireVerifyEgressLeaseOwner(t, ctx, v6, clientId, settings)
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			fields, err := r.HGetAll(ctx, verifyClientEgressV2Key(clientId)).Result()
			server.Raise(err)
			legacyHash := VerifyEgressIndexHashWithSettings(v4, settings)
			if len(fields) != 1 || fields[hex.EncodeToString(legacyHash[:])] != fmt.Sprint(oldWrite.Add(settings.EgressTtl).UnixMilli()) {
				t.Fatalf("a current writer fed the v2 index: %v", fields)
			}
		})

		end := uint64(oldWrite.Add(settings.EgressTtl).UnixMilli())
		for _, ip := range []netip.Addr{v4, legacyV6} {
			if got := resolveVerifyEgressAt(ctx, ip, settings, end); got != nil {
				t.Fatalf("v2 entry for %s attributed one EgressTtl after its last write: %s", ip, got)
			}
		}
		if got := resolveVerifyEgressAt(ctx, v6, settings, end); got == nil || *got != clientId {
			t.Fatalf("v4 address did not attribute alone after the v2 entry ended: %v", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if !verifyClientHasLiveEgress(ctx, r, clientId, end) || verifyClientHasLiveEgress(ctx, r, legacyV6Client, end) {
				t.Fatal("eligibility after the v2 entries ended did not follow the v4 index alone")
			}
		})
	})
}

// §8.2 rolling deploy: a v2 exact address inside a /64 another client holds
// in the v4 index resolves by its address first, to its own client. The v4
// index cannot see the v2 claim from the /64's other addresses, which
// resolve to the /64's one v4 holder until the v2 entry lapses.
func TestVerifyEgressV2AddressInAnotherClientsSlash64ResolvesByAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("cross-version-slash-64")
		legacyClient, currentClient := server.NewId(), server.NewId()
		legacyIp, currentIp := netip.MustParseAddr("2001:db8:69::1"), netip.MustParseAddr("2001:db8:69::2")
		testVerifySetProvideModes(ctx, legacyClient)
		testVerifySetProvideModes(ctx, currentClient)
		now := server.NowUtc()
		testVerifyFeedV2Address(ctx, legacyClient, legacyIp, server.NewId().String(), now, now.Add(settings.EgressTtl), settings)
		testVerifyFeedAddresses(ctx, currentClient, settings, currentIp)
		requireVerifyEgressLeaseOwner(t, ctx, legacyIp, legacyClient, settings)
		requireVerifyEgressLeaseOwner(t, ctx, currentIp, currentClient, settings)
		if got := ResolveVerifyEgress(ctx, netip.MustParseAddr("2001:db8:69::3"), settings); got == nil || *got != currentClient {
			t.Fatalf("another address of the /64 resolved to %v, want its one v4 holder", got)
		}
	})
}

// §8.2 rolling deploy: withdrawals reach what a v2 writer left in the v2
// index. Releasing a proxy allocation withdraws its v2 lease and forward
// entry, and reaping deletes the v2 index and its leases, so neither leaves a
// recycled address attributed for the rest of its TTL.
func TestVerifyEgressV2EntriesAreWithdrawn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("v2-withdrawal-test")
		released, reaped := server.NewId(), server.NewId()
		releasedIp, reapedIp := netip.MustParseAddr("192.0.2.48"), netip.MustParseAddr("192.0.2.49")
		now := server.NowUtc()
		for clientId, owner := range map[server.Id]string{released: verifyProxyEgressLeaseOwner, reaped: server.NewId().String()} {
			ip := releasedIp
			if clientId == reaped {
				ip = reapedIp
			}
			testVerifySetProvideModes(ctx, clientId)
			testVerifyFeedV2Address(ctx, clientId, ip, owner, now, now.Add(settings.EgressTtl), settings)
		}
		requireVerifyEgressLeaseOwner(t, ctx, releasedIp, released, settings)
		requireVerifyEgressLeaseOwner(t, ctx, reapedIp, reaped, settings)

		clearVerifyProxyEgressForClient(ctx, released)
		RemoveVerifyEgressForClient(ctx, reaped)
		for clientId, ip := range map[server.Id]netip.Addr{released: releasedIp, reaped: reapedIp} {
			legacyHash := VerifyEgressIndexHashWithSettings(ip, settings)
			hashHex := hex.EncodeToString(legacyHash[:])
			if got := ResolveVerifyEgress(ctx, ip, settings); got != nil || testVerifyEligible(ctx, clientId) {
				t.Fatalf("withdrawn v2 entry still attributed %s: %v", ip, got)
			}
			v2Key := verifyClientEgressV2Key(clientId)
			requireVerifyKeysAbsent(t, ctx, v2Key, verifyClientEgressLeaseKey(v2Key, hashHex), verifyEgressKeyFromHex(hashHex))
		}
	})
}

// §8.2 (Redis/Lua): a reverse field names the address alone, so its owners'
// leases always publish to one field, whose value carries the latest feed's
// egress-IP-hash and keeps it through another owner's close. Owners of one
// address opened before and after an egress_ipv4_prefix change leave no live
// field once both close, and the provider's next IPv4 address is its only one.
func TestVerifyEgressScorePrefixChangeLeavesNoPhantomAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		before := testVerifyFamilySettings("prefix-change")
		after := *before
		after.EgressHashV4Prefix = 24
		clientId := server.NewId()
		ip, nextIp := netip.MustParseAddr("192.0.2.66"), netip.MustParseAddr("198.51.100.66")
		testVerifySetProvideModes(ctx, clientId)
		older, newer := server.NewId(), server.NewId()
		FeedVerifyConnectionEgress(ctx, clientId, older, ip, before)
		FeedVerifyConnectionEgress(ctx, clientId, newer, ip, &after)
		ClearVerifyConnectionEgress(ctx, clientId, older, ip, before)
		// the field kept the latest feed's egress-IP-hash through the close
		afterHash := VerifyEgressIpHashWithSettings(ip, &after)
		if live := testVerifyLiveEgress(ctx, clientId); len(live.byFamily[4]) != 1 || live.byFamily[4][0].score != hex.EncodeToString(afterHash[:]) {
			t.Fatalf("the remaining owner's field = %+v, want the latest egress-IP-hash", live.byFamily[4])
		}
		ClearVerifyConnectionEgress(ctx, clientId, newer, ip, &after)
		requireVerifyKeysAbsent(t, ctx, verifyClientEgressKey(clientId))
		testVerifyFeedAddresses(ctx, clientId, &after, nextIp)
		requireVerifyEgressLeaseOwner(t, ctx, nextIp, clientId, &after)
	})
}

// §9: the provider meters count per resolved provider whichever family its
// source ip is in, so a provider with an address in each family keeps one
// seed and one EXTEND budget. An excluded source skips them, as it skips the
// source-ip meters, and an unresolved source's pad counts on a scratch key of
// its own, never on a provider's or on one key all pads share.
func TestVerifyProviderRateMetersCountPerProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		// one fixed window for the whole test
		settings.SeedRateWindow = 1000 * time.Hour
		clientId, otherId := server.NewId(), server.NewId()
		for i, ip := range []string{"198.51.100.50", "2001:db8:50::1"} {
			if got := IncrVerifySeedProviderRate(ctx, ip, clientId, settings); got != int64(i+1) {
				t.Fatalf("seed from %s = %d, want %d", ip, got, i+1)
			}
			if got := IncrVerifyExtendProviderRate(ctx, ip, clientId, settings); got != int64(i+1) {
				t.Fatalf("extend from %s = %d, want %d", ip, got, i+1)
			}
		}
		if got := IncrVerifySeedProviderRate(ctx, "198.51.100.50", otherId, settings); got != 1 {
			t.Fatalf("another provider's seed = %d, want its own budget", got)
		}
		if got := incrVerifyProviderRateForClient(ctx, true, verifySeedProviderRateKey(clientId), settings); got != 0 {
			t.Fatalf("excluded source seed = %d, want 0", got)
		}
		if got := IncrVerifySeedProviderRate(ctx, "2001:db8:50::1", clientId, settings); got != 3 {
			t.Fatalf("seed after an excluded one = %d, want 3", got)
		}
		// sources of two rate-limit subnets
		PadVerifyExtendProviderRate(ctx, "198.51.100.51", settings)
		PadVerifyExtendProviderRate(ctx, "203.0.113.51", settings)
		if got := IncrVerifyExtendProviderRate(ctx, "198.51.100.50", clientId, settings); got != 3 {
			t.Fatalf("extend after a pad = %d, want 3", got)
		}
		for _, ip := range []string{"198.51.100.51", "203.0.113.51"} {
			rateLimitClient, err := server.NewRateLimitClientIp(ip)
			server.Raise(err)
			if got := incrVerifyProviderRateForClient(ctx, false, verifyExtendPadRateKey(rateLimitClient.IpHash()), settings); got != 2 {
				t.Fatalf("%s's scratch counter after its one pad = %d, want 2 now", ip, got)
			}
		}
		if got := IncrVerifyExtendProviderRate(ctx, "198.51.100.51", server.Id{}, settings); got != 1 {
			t.Fatalf("the pads counted on one shared key: %d", got)
		}
	})
}

// §8.2, IPV6.md C7: retryable answers to an unattributed source are counted
// per step: per trail and depth for an EXTEND, with the last answer's time,
// per signature for a SEED, each for as long as a step can last.
func TestVerifyUnattributedAttemptsCountPerStep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		trailId := server.NewId()
		for i := int64(1); i <= 3; i++ {
			if got := IncrVerifyUnattributedExtend(ctx, trailId, 1, uint64(100*i), settings); got != i {
				t.Fatalf("depth 1 attempt %d counted %d", i, got)
			}
		}
		if got := GetVerifyUnattributedExtendAnsweredMs(ctx, trailId, 1); got != 300 {
			t.Fatalf("depth 1's last answer = %d, want 300", got)
		}
		if got := IncrVerifyUnattributedExtend(ctx, trailId, 2, 400, settings); got != 1 {
			t.Fatalf("the next step shared the count: %d", got)
		}
		if got := GetVerifyUnattributedExtendAnsweredMs(ctx, trailId, 2); got != 400 {
			t.Fatalf("depth 2's last answer = %d, want its own", got)
		}
		if got := GetVerifyUnattributedExtendAnsweredMs(ctx, trailId, 3); got != 0 {
			t.Fatalf("a step without a retryable answer answered at %d", got)
		}
		if got := IncrVerifyUnattributedExtend(ctx, server.NewId(), 1, 500, settings); got != 1 {
			t.Fatalf("another trail shared the count: %d", got)
		}
		seedSig, otherSig := []byte("seed-signature"), []byte("other-signature")
		IncrVerifyUnattributedSeed(ctx, seedSig, settings)
		if got := IncrVerifyUnattributedSeed(ctx, seedSig, settings); got != 2 {
			t.Fatalf("second attempt of one SEED counted %d", got)
		}
		if got := IncrVerifyUnattributedSeed(ctx, otherSig, settings); got != 1 {
			t.Fatalf("another SEED shared the count: %d", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{verifyUnattributedExtendKey(trailId, 1), verifyUnattributedSeedKey(seedSig)} {
				ttl, err := r.PTTL(ctx, key).Result()
				server.Raise(err)
				if ttl <= 0 || settings.StepTimeout+settings.StepTimeoutGrace < ttl {
					t.Fatalf("%s lives %s, want at most one step", key, ttl)
				}
			}
		})
	})
}

// §8.1, D27: the server-side head estimate scores each provider by one
// address: its IPv4 address while it has one, else its IPv6 /64, else a v2
// entry; the least hash when a family has several.
func TestVerifyLiveEgressHashesScoreOneAddressPerProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-estimate-test")
		dual, v6Only, twoV4, legacy, absent := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		dualV4, dualV6 := netip.MustParseAddr("192.0.2.51"), netip.MustParseAddr("2001:db8:51::1")
		v6OnlyIp, legacyIp := netip.MustParseAddr("2001:db8:52::1"), netip.MustParseAddr("192.0.2.53")
		twoV4Ips := []netip.Addr{netip.MustParseAddr("192.0.2.55"), netip.MustParseAddr("192.0.2.56")}
		testVerifyFeedAddresses(ctx, dual, settings, dualV4, dualV6)
		testVerifyFeedAddresses(ctx, v6Only, settings, v6OnlyIp)
		testVerifyFeedAddresses(ctx, twoV4, settings, twoV4Ips...)
		now := server.NowUtc()
		testVerifyFeedV2Address(ctx, legacy, legacyIp, server.NewId().String(), now, now.Add(settings.EgressTtl), settings)
		got := GetVerifyLiveEgressHashes(ctx, []server.Id{dual, v6Only, twoV4, legacy, absent})
		_, dualField := verifyClientEgressField(dualV4, settings)
		_, v6OnlyField := verifyClientEgressField(v6OnlyIp, settings)
		_, firstField := verifyClientEgressField(twoV4Ips[0], settings)
		_, secondField := verifyClientEgressField(twoV4Ips[1], settings)
		legacyHash := VerifyEgressIndexHashWithSettings(legacyIp, settings)
		want := map[server.Id][]string{
			dual:   {dualField.network},
			v6Only: {v6OnlyField.network},
			twoV4:  {min(firstField.network, secondField.network)},
			legacy: {hex.EncodeToString(legacyHash[:])},
		}
		if len(got) != len(want) {
			t.Fatalf("scored hashes = %v, want %v", got, want)
		}
		for clientId, hashes := range want {
			if !slices.Equal(got[clientId], hashes) {
				t.Fatalf("client %s scored %v, want %v", clientId, got[clientId], hashes)
			}
		}
	})
}

// §8.1, D27: a provider's confirmed hops record one egress-IP-hash a scoring
// window, the pin of its first one, whatever family each lands on and however
// its IPv4 transport closes, reconnects or overlaps a new lease. The
// validators union a fleet's hop hashes per window, so it scores one prefix,
// not two. The pin is its IPv4 address's, though the first hop lands on IPv6.
func TestVerifyHeadHashOnePerProviderAcrossAWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-window")
		clientId := server.NewId()
		v4, v6, renewedV4 := netip.MustParseAddr("192.0.2.64"), netip.MustParseAddr("2001:db8:64::1"), netip.MustParseAddr("192.0.2.65")
		testVerifySetProvideModes(ctx, clientId)
		connectionIds := testVerifyFeedAddresses(ctx, clientId, settings, v4, v6)
		recorded := map[[32]byte][]string{}
		record := func(stage string, sources ...netip.Addr) {
			t.Helper()
			for _, source := range sources {
				if resolved := ResolveVerifyEgress(ctx, source, settings); resolved == nil || *resolved != clientId {
					t.Fatalf("control: the hop from %s %s did not resolve: %v", source, stage, resolved)
				}
				hash := PinVerifyHopEgressIpHash(ctx, clientId, source, "e7", false, settings)
				recorded[hash] = append(recorded[hash], fmt.Sprintf("%s %s", source, stage))
			}
		}
		record("with one live IPv4 address", v6, v4)
		ClearVerifyConnectionEgress(ctx, clientId, connectionIds[v4], v4, settings)
		record("while the IPv4 transport reconnects", v6)
		testVerifyFeedAddresses(ctx, clientId, settings, v4)
		record("after the IPv4 transport reconnected", v4, v6)
		testVerifyFeedAddresses(ctx, clientId, settings, renewedV4)
		record("while two IPv4 leases overlap", v6)
		if len(recorded) != 1 || recorded[VerifyEgressIpHashWithSettings(v4, settings)] == nil {
			t.Fatalf("one provider recorded %d head-score hashes in one window, want its IPv4 prefix: %v", len(recorded), recorded)
		}
	})
}

// §8.1, D27 (probe TestProbeVerifyPoisonTrailDoesNotPinTheHeadHash, N3): the
// prefix a provider pins for a window is a rule of its index, not of the
// family its first hop lands on: its IPv4 address's while that family
// attributes to it, else its IPv6 address's, else the hop's own. A caller
// choosing the family of a provider's first hop does not steer it.
func TestVerifyHeadHashPinFollowsTheProvidersIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-index-rule")
		v6 := func(i int) netip.Addr { return netip.MustParseAddr(fmt.Sprintf("2001:db8:a%d::1", i)) }
		v4 := func(i int) netip.Addr { return netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 100+8*i)) }
		for i, c := range []struct {
			name   string
			feed   []netip.Addr
			mark   netip.Addr
			source netip.Addr
			want   netip.Addr
		}{
			{name: "both families attribute", feed: []netip.Addr{v4(1), v6(1)}, source: v6(1), want: v4(1)},
			{name: "the IPv4 family is refused", feed: []netip.Addr{v4(2), v4(3), v6(2)}, source: v6(2), want: v6(2)},
			{name: "the IPv4 address is ambiguous", feed: []netip.Addr{v4(4), v6(4)}, mark: v4(4), source: v6(4), want: v6(4)},
			{name: "only IPv6 is indexed", feed: []netip.Addr{v6(5)}, source: v6(5), want: v6(5)},
			{name: "only IPv4 is indexed", feed: []netip.Addr{v4(6)}, source: v4(6), want: v4(6)},
			{name: "nothing attributes", source: v4(7), want: v4(7)},
		} {
			clientId := server.NewId()
			testVerifyFeedAddresses(ctx, clientId, settings, c.feed...)
			if c.mark.IsValid() {
				_, marked := verifyClientEgressField(c.mark, settings)
				server.Redis(ctx, func(r server.RedisClient) {
					server.Raise(r.Set(ctx, verifyEgressKeyFromHex(marked.address), verifyEgressAmbiguous, settings.EgressTtl).Err())
				})
			}
			window := fmt.Sprintf("e%d", 20+i)
			if got := PinVerifyHopEgressIpHash(ctx, clientId, c.source, window, false, settings); got != VerifyEgressIpHashWithSettings(c.want, settings) {
				t.Fatalf("%s: the first hop from %s pinned %x, want %s's prefix", c.name, c.source, got, c.want)
			}
		}
	})
}

// §8.1, §9 (probe TestProbeVerifyPoisonTrailDoesNotPinTheHeadHash, N3): a
// poison trail's hop reads the provider's pin, or records the candidate, but
// never sets a pin, so a caller with no registered key cannot fix a
// provider's scored prefix for the window. A real hop after it pins.
func TestVerifyHeadHashPoisonHopDoesNotPin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-poison")
		clientId := server.NewId()
		v6, other := netip.MustParseAddr("2001:db8:b1::1"), netip.MustParseAddr("2001:db8:b2::1")
		connectionIds := testVerifyFeedAddresses(ctx, clientId, settings, v6)
		ownHash, otherHash := VerifyEgressIpHashWithSettings(v6, settings), VerifyEgressIpHashWithSettings(other, settings)
		if got := PinVerifyHopEgressIpHash(ctx, clientId, v6, "e9", true, settings); got != ownHash {
			t.Fatalf("the poison hop recorded %x, want the provider's candidate", got)
		}
		requireVerifyKeysAbsent(t, ctx, verifyHeadHashKey(clientId, "e9"))
		// the provider's IPv6 /64 moves: a real hop pins the new candidate
		ClearVerifyConnectionEgress(ctx, clientId, connectionIds[v6], v6, settings)
		testVerifyFeedAddresses(ctx, clientId, settings, other)
		if got := PinVerifyHopEgressIpHash(ctx, clientId, other, "e9", false, settings); got != otherHash {
			t.Fatalf("the real hop after a poison one pinned %x, want its candidate", got)
		}
		if got := PinVerifyHopEgressIpHash(ctx, clientId, v6, "e9", true, settings); got != otherHash {
			t.Fatalf("a poison hop after the pin recorded %x, want the pin", got)
		}
	})
}

// §8.1, D27 (probe TestProbeVerifyHeadHashPinOutlivesAMainnetEpoch, N2): a
// pin lasts its scoring window, the settlement epoch, and a grace. A mirror
// that does not carry the epoch's length gets a mainnet epoch's: sn
// deploy/mainnet/policy-v1.yml settles every 50400 12-second blocks, seven
// days, so a pin of a shorter fixed life would lapse mid-epoch and let a
// dual-stack provider record both prefixes in one epoch.
func TestVerifyHeadHashPinOutlivesAMainnetEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-ttl")
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		testVerifyCohortSetEpoch(ctx, 9)
		clientId := server.NewId()
		PinVerifyHopEgressIpHash(ctx, clientId, netip.MustParseAddr("192.0.2.9"), verifyHeadHashWindow(ctx, settings), false, settings)
		mainnetEpoch := 50400 * 12 * time.Second
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, verifyHeadHashKey(clientId, "e9")).Result()
			server.Raise(err)
			if ttl < mainnetEpoch || mainnetEpoch+verifyHeadHashGrace < ttl {
				t.Errorf("the epoch's head-hash pin lives %s, want a mainnet settlement epoch (%s) and the grace", ttl, mainnetEpoch)
			}
		})
	})
}

// §8.1: a pin of an epoch the mirror gives the length of lives that epoch and
// the grace, and a pin of the fallback hourly window an hour and the grace.
func TestVerifyHeadHashPinLivesItsWindowAndAGrace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-lifetime")
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		SetStEpochSummaryCache(ctx, testVerifyCohortDeployment, &StEpochSummary{Epoch: 12, TEpochBlocks: 360}, time.Hour)
		for window, want := range map[string]time.Duration{
			"e12":   360*12*time.Second + verifyHeadHashGrace,
			"h4000": time.Hour + verifyHeadHashGrace,
		} {
			clientId := server.NewId()
			PinVerifyHopEgressIpHash(ctx, clientId, netip.MustParseAddr("192.0.2.12"), window, false, settings)
			server.Redis(ctx, func(r server.RedisClient) {
				ttl, err := r.PTTL(ctx, verifyHeadHashKey(clientId, window)).Result()
				server.Raise(err)
				if ttl <= want-time.Minute || want < ttl {
					t.Fatalf("the %s pin lives %s, want %s", window, ttl, want)
				}
			})
		}
	})
}

// §8.1 (N7): a trail pins in the scoring window of its SEED, kept for the
// trail's lifetime, so a trail that outlasts an epoch boundary never mixes
// two windows' pins. A trail whose SEED kept none takes the current window.
func TestVerifyTrailHeadWindowIsItsSeeds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("trail-head-window")
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		testVerifyCohortSetEpoch(ctx, 7)
		trailId := server.NewId()
		SetVerifyTrailHeadWindow(ctx, trailId, 4, settings)
		testVerifyCohortSetEpoch(ctx, 8)
		if window := GetVerifyTrailHeadWindow(ctx, trailId, settings); window != "e7" {
			t.Fatalf("a trail seeded in epoch 7 pins in %s", window)
		}
		if window := GetVerifyTrailHeadWindow(ctx, server.NewId(), settings); window != "e8" {
			t.Fatalf("a trail without a kept window pins in %s, want the current one", window)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, verifyTrailHeadWindowKey(trailId)).Result()
			server.Raise(err)
			if ttl <= 0 || settings.TrailTtl(4) < ttl {
				t.Fatalf("the kept window lives %s, want the trail's lifetime", ttl)
			}
		})
	})
}

// §8.1, D27: the scoring window is the settlement epoch from the st mirror,
// so the next epoch pins afresh; with no epoch it is the UTC hour. Each
// provider has its own pin.
func TestVerifyHeadHashWindowFollowsTheSettlementEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-epoch")
		clientId, otherId := server.NewId(), server.NewId()
		// unindexed sources, so each first hop pins its own prefix
		first, second := netip.MustParseAddr("192.0.2.71"), netip.MustParseAddr("192.0.2.72")
		firstHash, secondHash := VerifyEgressIpHashWithSettings(first, settings), VerifyEgressIpHashWithSettings(second, settings)
		if window := verifyHeadHashWindow(ctx, settings); window != fmt.Sprintf("h%d", server.NowUtc().Unix()/3600) {
			t.Fatalf("window without a deployment = %s", window)
		}
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		if window := verifyHeadHashWindow(ctx, settings); window[0] != 'h' {
			t.Fatalf("window without an epoch mirror = %s", window)
		}
		testVerifyCohortSetEpoch(ctx, 7)
		window := verifyHeadHashWindow(ctx, settings)
		if window != "e7" {
			t.Fatalf("window of epoch 7 = %s", window)
		}
		if got := PinVerifyHopEgressIpHash(ctx, clientId, first, window, false, settings); got != firstHash {
			t.Fatalf("the first hop of epoch 7 pinned %x", got)
		}
		if got := PinVerifyHopEgressIpHash(ctx, clientId, second, window, false, settings); got != firstHash {
			t.Fatalf("a later hop of epoch 7 recorded %x, want the pinned %x", got, firstHash)
		}
		if got := PinVerifyHopEgressIpHash(ctx, otherId, second, window, false, settings); got != secondHash {
			t.Fatalf("another provider took the pin %x", got)
		}
		testVerifyCohortSetEpoch(ctx, 8)
		if got := PinVerifyHopEgressIpHash(ctx, clientId, second, verifyHeadHashWindow(ctx, settings), false, settings); got != secondHash {
			t.Fatalf("the first hop of epoch 8 recorded %x, want its own", got)
		}
	})
}

// A pin or a stored egress-IP-hash that is not a 32-byte hash is refused,
// like every malformed verify state read.
func TestVerifyHeadHashRefusesAMalformedPin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-malformed")
		clientId := server.NewId()
		source := netip.MustParseAddr("192.0.2.73")
		requireRefused := func(what string) {
			t.Helper()
			defer func() {
				if recover() == nil {
					t.Fatalf("%s was used", what)
				}
			}()
			PinVerifyHopEgressIpHash(ctx, clientId, source, "e7", false, settings)
		}
		for _, value := range []string{"not-hex", "abcd"} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, verifyHeadHashKey(clientId, "e7"), value, time.Minute).Err())
			})
			requireRefused(fmt.Sprintf("pin %q", value))
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, verifyHeadHashKey(clientId, "e7")).Err())
		})
		testVerifyFeedAddresses(ctx, clientId, settings, source)
		field, _ := verifyClientEgressField(source, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, verifyClientEgressKey(clientId), field, fmt.Sprintf("%d_abcd", server.NowUtc().Add(time.Minute).UnixMilli())).Err())
		})
		requireRefused("a stored egress-IP-hash of 2 bytes")
	})
}
