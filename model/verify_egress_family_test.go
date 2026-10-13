// Per-family source attribution (VALIDATOR.md §8.2): a provider may hold one
// live address in each family, an IPv6 /64 counting as one address, and a hop
// resolves when its source is its family's only live address for the
// provider. Fixtures use documentation addresses in the fixture's isolated
// Redis database.
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
// the v2 reverse index, the shared forward entry, a lease until `expires`.
func testVerifyFeedV2Address(ctx context.Context, clientId server.Id, ip netip.Addr, owner string, now time.Time, expires time.Time, settings *VerifySettings) {
	legacyHash := verifyEgressLegacyIndexHash(ip, settings)
	server.Redis(ctx, func(r server.RedisClient) {
		updateVerifyEgressLease(ctx, r, verifyClientEgressV2Key(clientId), clientId, hex.EncodeToString(legacyHash[:]), owner, now, expires)
	})
	updateVerifyEligibleMembership(ctx, clientId)
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
// its pinned IPv6 transport's. The /64 is one address: a hop from the rotated
// address resolves, and a transport reconnecting from it adds no second IPv6
// address, so the provider stays assignable.
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
		server.Redis(ctx, func(r server.RedisClient) {
			fields, err := r.HGetAll(ctx, verifyClientEgressKey(clientId)).Result()
			server.Raise(err)
			if len(fields) != 2 {
				t.Fatalf("one /64 and one IPv4 address left %d reverse fields: %v", len(fields), fields)
			}
		})
		requireVerifyAssignable(t, ctx, clientId, settings)
	})
}

// §8.2: two providers in one IPv6 /64 contend exactly as two behind one IPv4
// address do: the /64 resolves to neither, while each stays assignable
// through its own IPv4 address.
func TestVerifyEgressIpv6NeighborsInOneSlash64Contend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("one-slash-64-neighbors")
		first, second := server.NewId(), server.NewId()
		firstV6, secondV6 := netip.MustParseAddr("2001:db8:65::1"), netip.MustParseAddr("2001:db8:65::2")
		testVerifySetProvideModes(ctx, first)
		testVerifySetProvideModes(ctx, second)
		testVerifyFeedAddresses(ctx, first, settings, netip.MustParseAddr("192.0.2.65"), firstV6)
		testVerifyFeedAddresses(ctx, second, settings, netip.MustParseAddr("192.0.2.66"), secondV6)
		for _, ip := range []netip.Addr{firstV6, secondV6} {
			if got := ResolveVerifyEgress(ctx, ip, settings); got != nil {
				t.Fatalf("%s in a /64 two providers share resolved to %s", ip, got)
			}
		}
		// draw each alone: the other leaves the eligible set first
		for _, pair := range [][2]server.Id{{first, second}, {second, first}} {
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

// §8.2: a live address whose forward entry names another client disqualifies
// a provider even when its other family attributes, since a hop from it would
// resolve to that client and be refused as a mismatch, not retried. An
// ambiguous address does not.
func TestVerifyEgressAddressAttributedToAnotherClientDisqualifies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("foreign-address")
		clientId := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.67"), netip.MustParseAddr("2001:db8:67::1")
		testVerifySetProvideModes(ctx, clientId)
		testVerifyFeedAddresses(ctx, clientId, settings, v4, v6)
		_, v4HashHex := verifyClientEgressField(v4, settings)
		for _, forward := range []string{verifyEgressAmbiguous, server.NewId().String()} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, verifyEgressKeyFromHex(v4HashHex), forward, settings.EgressTtl).Err())
				server.Raise(r.SAdd(ctx, verifyEligibleKey, clientId.String()).Err())
			})
			next, _ := SampleVerifyNextHop(ctx, nil, settings)
			if assignable := next != nil && *next == clientId; assignable != (forward == verifyEgressAmbiguous) {
				t.Fatalf("with its IPv4 forward entry %q the provider was assignable=%t", forward, assignable)
			}
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
			testVerifySetProvideModes(ctx, clientId)
			now := server.NowUtc()
			server.Redis(ctx, func(r server.RedisClient) {
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, lapsingField, "lapsing", now, now.Add(time.Minute))
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, keptField, "kept", now, now.Add(time.Minute))
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, keptField, "kept", now.Add(time.Minute/2), now.Add(10*time.Minute))
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
// address hash and family; no current writer feeds the v2 index. A close
// withdraws only its own family's field, lease and forward entry; reaping
// withdraws every family's.
func TestVerifyEgressReverseIndexCarriesTheFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("reverse-index-family-test")
		clientId := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.45"), netip.MustParseAddr("2001:db8:45::1")
		testVerifySetProvideModes(ctx, clientId)
		connectionIds := testVerifyFeedAddresses(ctx, clientId, settings, v4, v6)
		reverseKey := verifyClientEgressKey(clientId)
		fields, hashHexes := map[netip.Addr]string{}, map[netip.Addr]string{}
		for _, ip := range []netip.Addr{v4, v6} {
			indexHash := VerifyEgressIndexHashWithSettings(ip, settings)
			hashHexes[ip] = hex.EncodeToString(indexHash[:])
			fields[ip] = fmt.Sprintf("%s_%d", hashHexes[ip], server.IpVersionForAddr(ip))
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
					if got[fields[ip]] == "" {
						t.Fatalf("reverse fields = %v, want %s", got, fields[ip])
					}
				}
			})
			requireVerifyKeysAbsent(t, ctx, verifyClientEgressV2Key(clientId))
		}
		requireFields(v4, v6)

		ClearVerifyConnectionEgress(ctx, clientId, connectionIds[v6], v6, settings)
		requireFields(v4)
		requireVerifyKeysAbsent(t, ctx, verifyClientEgressLeaseKey(reverseKey, hashHexes[v6]), verifyEgressKeyFromHex(hashHexes[v6]))
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)

		testVerifyFeedAddresses(ctx, clientId, settings, v6)
		requireFields(v4, v6)
		RemoveVerifyEgressForClient(ctx, clientId)
		for _, ip := range []netip.Addr{v4, v6} {
			requireVerifyKeysAbsent(t, ctx, verifyClientEgressLeaseKey(reverseKey, hashHexes[ip]), verifyEgressKeyFromHex(hashHexes[ip]))
		}
		requireVerifyKeysAbsent(t, ctx, reverseKey)
		if testVerifyEligible(ctx, clientId) {
			t.Fatal("reaped provider stayed eligible")
		}
	})
}

// §8.2: a v4 field is the address hash and family; anything else, a bare v2
// field included, has no family and is no v4 entry.
func TestVerifyEgressFieldNamesItsAddressAndFamily(t *testing.T) {
	settings := &VerifySettings{EgressHashKey: []byte("field-parse-test"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}
	for _, ip := range []netip.Addr{netip.MustParseAddr("192.0.2.46"), netip.MustParseAddr("2001:db8:46::1")} {
		field, hashHex := verifyClientEgressField(ip, settings)
		if field != hashHex+fmt.Sprintf("_%d", server.IpVersionForAddr(ip)) {
			t.Fatalf("field for %s = %q", ip, field)
		}
		if got, family := parseVerifyClientEgressField(field); got != hashHex || family != server.IpVersionForAddr(ip) {
			t.Fatalf("field %q parsed as %s, %d", field, got, family)
		}
	}
	for _, field := range []string{"abc", "abc_5", "abc_4_def"} {
		if got, family := parseVerifyClientEgressField(field); got != "abc" || family != 0 {
			t.Fatalf("field %q parsed as %s, %d, want no family", field, got, family)
		}
	}
	live := newVerifyLiveEgress([]string{"a_4", "b_6", "c_7"}, []string{"d"})
	if !slices.Equal(live.byFamily[4], []string{"a"}) || !slices.Equal(live.byFamily[6], []string{"b"}) || len(live.byFamily) != 2 || !slices.Equal(live.legacy, []string{"d"}) {
		t.Fatalf("live egress = %+v", live)
	}
}

// §8.2: the address index is an exact IPv4 address or an IPv6 /64. Addresses
// in one /64 share it and other /64s do not, and an IPv6 index hash is apart
// from the address's v2 exact-address hash; an IPv4 address keeps its v2 hash.
func TestVerifyEgressIndexHashCoversAnIpv6Slash64(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testVerifyEgressIndexHashCoversAnIpv6Slash64(t)
	})
}

// The unkeyed index hash uses the client ip hash pepper, which the test
// environment provides.
func testVerifyEgressIndexHashCoversAnIpv6Slash64(t testing.TB) {
	for _, settings := range []*VerifySettings{{EgressHashKey: []byte("slash-64-test"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}, {EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}} {
		inside := []netip.Addr{netip.MustParseAddr("2001:db8:68::1"), netip.MustParseAddr("2001:db8:68::ffff:1"), netip.MustParseAddr("2001:db8:68:0:a:b:c:d")}
		for _, ip := range inside[1:] {
			if VerifyEgressIndexHashWithSettings(ip, settings) != VerifyEgressIndexHashWithSettings(inside[0], settings) {
				t.Fatalf("%s and %s in one /64 have two index hashes (keyed=%t)", ip, inside[0], 0 < len(settings.EgressHashKey))
			}
		}
		if VerifyEgressIndexHashWithSettings(netip.MustParseAddr("2001:db8:68:1::1"), settings) == VerifyEgressIndexHashWithSettings(inside[0], settings) {
			t.Fatal("two /64s share an index hash")
		}
		if VerifyEgressIndexHashWithSettings(inside[0], settings) == verifyEgressLegacyIndexHash(inside[0], settings) {
			t.Fatal("an IPv6 index hash equals its v2 exact-address hash")
		}
		v4 := netip.MustParseAddr("192.0.2.68")
		if VerifyEgressIndexHashWithSettings(v4, settings) != verifyEgressLegacyIndexHash(v4, settings) {
			t.Fatal("an IPv4 index hash moved off its v2 hash")
		}
	}
}

// §8.2 rolling deploy: a v2 writer's entry, a bare exact-address hash, still
// attributes its address by the v2 rule (the client's one live v2 address)
// and keeps the provider eligible. An IPv6 v2 entry resolves for its exact
// address, under its v2 hash. A v4 address beside a v2 entry resolves too:
// the versions are judged apart. No current writer feeds v2, so an entry ends
// one EgressTtl after its last write.
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
			legacyHash := verifyEgressLegacyIndexHash(v4, settings)
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
// in the v4 index is two clients in one /64, so it resolves to neither. The
// v4 index cannot see the v2 claim from the /64's other addresses, which stay
// the current client's until the v2 entry lapses.
func TestVerifyEgressV2AddressInAnotherClientsSlash64ResolvesToNeither(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("cross-version-slash-64")
		legacyClient, currentClient := server.NewId(), server.NewId()
		legacyIp, currentIp := netip.MustParseAddr("2001:db8:69::1"), netip.MustParseAddr("2001:db8:69::2")
		testVerifySetProvideModes(ctx, currentClient)
		now := server.NowUtc()
		testVerifyFeedV2Address(ctx, legacyClient, legacyIp, server.NewId().String(), now, now.Add(settings.EgressTtl), settings)
		testVerifyFeedAddresses(ctx, currentClient, settings, currentIp)
		requireVerifyEgressLeaseOwner(t, ctx, currentIp, currentClient, settings)
		if got := ResolveVerifyEgress(ctx, legacyIp, settings); got != nil {
			t.Fatalf("a /64 two clients claim across versions resolved to %s", got)
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
			legacyHash := verifyEgressLegacyIndexHash(ip, settings)
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
// leases always publish to one field. Owners of one address opened before and
// after an egress_ipv4_prefix change leave no live field once both close, and
// the provider's next IPv4 address is its only one.
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
		ClearVerifyConnectionEgress(ctx, clientId, newer, ip, &after)
		requireVerifyKeysAbsent(t, ctx, verifyClientEgressKey(clientId))
		testVerifyFeedAddresses(ctx, clientId, &after, nextIp)
		requireVerifyEgressLeaseOwner(t, ctx, nextIp, clientId, &after)
	})
}

// §9: the provider meters count per resolved provider whichever family its
// source ip is in, so a provider with an address in each family keeps one
// seed and one EXTEND budget. An excluded source skips them, as it skips the
// source-ip meters, and an unresolved source's pad counts on a scratch key.
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
		PadVerifyExtendProviderRate(ctx, "198.51.100.51", settings)
		if got := IncrVerifyExtendProviderRate(ctx, "198.51.100.50", clientId, settings); got != 3 {
			t.Fatalf("extend after a pad = %d, want 3", got)
		}
		if got := incrVerifyProviderRate(ctx, "198.51.100.51", verifyExtendProviderRateKey(server.Id{}), settings); got != 2 {
			t.Fatalf("scratch counter after one pad = %d, want 2 now", got)
		}
	})
}

// §8.2, IPV6.md C7: retryable answers to an unresolved source are counted per
// step: per trail and depth for an EXTEND, per signature for a SEED, each for
// as long as a step can last.
func TestVerifyUnattributedAttemptsCountPerStep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		trailId := server.NewId()
		for i := int64(1); i <= 3; i++ {
			if got := IncrVerifyUnattributedExtend(ctx, trailId, 1, settings); got != i {
				t.Fatalf("depth 1 attempt %d counted %d", i, got)
			}
		}
		if got := IncrVerifyUnattributedExtend(ctx, trailId, 2, settings); got != 1 {
			t.Fatalf("the next step shared the count: %d", got)
		}
		if got := IncrVerifyUnattributedExtend(ctx, server.NewId(), 1, settings); got != 1 {
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
		_, dualHash := verifyClientEgressField(dualV4, settings)
		_, v6OnlyHash := verifyClientEgressField(v6OnlyIp, settings)
		_, firstHash := verifyClientEgressField(twoV4Ips[0], settings)
		_, secondHash := verifyClientEgressField(twoV4Ips[1], settings)
		legacyHash := verifyEgressLegacyIndexHash(legacyIp, settings)
		want := map[server.Id][]string{
			dual:   {dualHash},
			v6Only: {v6OnlyHash},
			twoV4:  {min(firstHash, secondHash)},
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
// window, the first one's, whatever family each lands on and however its
// IPv4 transport closes, reconnects or overlaps a new lease. The validators
// union a fleet's hop hashes per window, so it scores one prefix, not two.
func TestVerifyHeadHashOnePerProviderAcrossAWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-window")
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		testVerifyCohortSetEpoch(ctx, 7)
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
				hash := PinVerifyHopEgressIpHash(ctx, clientId, VerifyEgressIpHashWithSettings(source, settings), settings)
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
		if len(recorded) != 1 || recorded[VerifyEgressIpHashWithSettings(v6, settings)] == nil {
			t.Fatalf("one provider recorded %d head-score hashes in one window, want its first hop's: %v", len(recorded), recorded)
		}
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
		first, second := [32]byte{1}, [32]byte{2}
		if window := verifyHeadHashWindow(ctx, settings); window != fmt.Sprintf("h%d", server.NowUtc().Unix()/3600) {
			t.Fatalf("window without a deployment = %s", window)
		}
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		if window := verifyHeadHashWindow(ctx, settings); window[0] != 'h' {
			t.Fatalf("window without an epoch mirror = %s", window)
		}
		testVerifyCohortSetEpoch(ctx, 7)
		if got := PinVerifyHopEgressIpHash(ctx, clientId, first, settings); got != first {
			t.Fatalf("the first hop of epoch 7 pinned %x", got)
		}
		if got := PinVerifyHopEgressIpHash(ctx, clientId, second, settings); got != first {
			t.Fatalf("a later hop of epoch 7 recorded %x, want the pinned %x", got, first)
		}
		if got := PinVerifyHopEgressIpHash(ctx, otherId, second, settings); got != second {
			t.Fatalf("another provider took the pin %x", got)
		}
		testVerifyCohortSetEpoch(ctx, 8)
		if got := PinVerifyHopEgressIpHash(ctx, clientId, second, settings); got != second {
			t.Fatalf("the first hop of epoch 8 recorded %x, want its own", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, verifyHeadHashKey(clientId, "e8")).Result()
			server.Raise(err)
			if ttl <= 0 || verifyHeadHashTtl < ttl {
				t.Fatalf("pin lives %s", ttl)
			}
		})
	})
}

// A pin that is not a 32-byte hash is refused, like every malformed verify
// state read.
func TestVerifyHeadHashRefusesAMalformedPin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-hash-malformed")
		settings.CohortDeploymentKey = testVerifyCohortDeployment
		testVerifyCohortSetEpoch(ctx, 7)
		clientId := server.NewId()
		for _, value := range []string{"not-hex", "abcd"} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, verifyHeadHashKey(clientId, "e7"), value, time.Minute).Err())
			})
			func() {
				defer func() {
					if recover() == nil {
						t.Fatalf("pin %q was used", value)
					}
				}()
				PinVerifyHopEgressIpHash(ctx, clientId, [32]byte{3}, settings)
			}()
		}
	})
}
