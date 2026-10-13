// Per-family source attribution (VALIDATOR.md §8.2): a provider may hold one
// live address in each family, and a hop resolves when its source is its
// family's only live address for the provider. Fixtures use documentation
// addresses in the fixture's isolated Redis database.
package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
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
		if next, _ := SampleVerifyNextHop(ctx, nil, settings); next == nil || *next != clientId {
			t.Fatalf("provider with one address per family was not assignable: %v", next)
		}
	})
}

// §8.2: two live addresses of one family stay ambiguous and refused: neither
// resolves and the provider is not eligible, while its address in the other
// family still resolves. Closing the duplicate restores both families.
func TestVerifyEgressSecondAddressOfAFamilyIsRefused(t *testing.T) {
	for _, duplicate := range []netip.Addr{netip.MustParseAddr("192.0.2.43"), netip.MustParseAddr("2001:db8:42::2")} {
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
				if testVerifyEligible(ctx, clientId) {
					t.Fatal("a provider with two live addresses of one family stayed eligible")
				}
				ClearVerifyConnectionEgress(ctx, clientId, connectionIds[duplicate], duplicate, settings)
				requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)
				requireVerifyEgressLeaseOwner(t, ctx, v6, clientId, settings)
			})
		})
	}
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
			{lapsing: netip.MustParseAddr("2001:db8:44::2"), kept: netip.MustParseAddr("192.0.2.54")},
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
				if got, _ := resolveVerifyEgressAt(ctx, ip, settings, before); got == nil || *got != clientId {
					t.Fatalf("%s did not resolve before either lease lapsed: %v", ip, got)
				}
			}
			after := uint64(now.Add(2 * time.Minute).UnixMilli())
			if got, _ := resolveVerifyEgressAt(ctx, lapsing, settings, after); got != nil {
				t.Fatalf("lapsed %s lease still attributed: %s", lapsing, got)
			}
			if got, _ := resolveVerifyEgressAt(ctx, kept, settings, after); got == nil || *got != clientId {
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

// §8.2: the v3 reverse index holds one field per live address, naming its
// address hash, family and head-score prefix hash; no current writer feeds the
// v2 index. A close withdraws only its own family's field, lease and forward
// entry; reaping withdraws every family's.
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
			fields[ip] = fmt.Sprintf("%s_%d_%x", hashHexes[ip], server.IpVersionForAddr(ip), VerifyEgressIpHashWithSettings(ip, settings))
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

// §8.2: a field without a valid family and score hash — a v2 field, or a
// malformed one — is an address of unknown family, which counts against both
// families; a v3 field yields its address hash, family and score hash.
func TestVerifyEgressFieldWithoutAFamilyIsUnknown(t *testing.T) {
	settings := &VerifySettings{EgressHashKey: []byte("field-parse-test"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}
	ip := netip.MustParseAddr("2001:db8:46::1")
	field, hashHex := verifyClientEgressField(ip, settings)
	entry := parseVerifyClientEgressField(field)
	if entry.indexHashHex != hashHex || entry.family != 6 || entry.scoreHash != VerifyEgressIpHashWithSettings(ip, settings) {
		t.Fatalf("v3 field %q parsed as %+v", field, entry)
	}
	scoreHex := hex.EncodeToString(entry.scoreHash[:])
	for _, unknown := range []string{
		hashHex,
		hashHex + "_6_" + scoreHex[:62],
		hashHex + "_6_" + strings.Repeat("z", 64),
		hashHex + "_5_" + scoreHex,
	} {
		entry := parseVerifyClientEgressField(unknown)
		if entry.indexHashHex != hashHex || entry.family != 0 || entry.scoreHash != ([32]byte{}) {
			t.Fatalf("field %q parsed as %+v, want an unknown family", unknown, entry)
		}
	}
	entries := map[string]verifyEgressEntry{hashHex: {indexHashHex: hashHex}}
	if len(verifyFamilyEgressEntries(entries, 4)) != 1 || len(verifyFamilyEgressEntries(entries, 6)) != 1 {
		t.Fatal("an unknown-family address did not count against both families")
	}
}

// §8.2: one entry stands for each address. An entry of known family wins over
// an unknown one whatever their expiries, and otherwise the later expiry
// wins, so a changed score prefix does not split one address in two.
// Expired fields are returned for pruning instead.
func TestVerifyEgressEntriesAreOnePerAddress(t *testing.T) {
	settings := &VerifySettings{EgressHashKey: []byte("entries-test"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}
	ip := netip.MustParseAddr("192.0.2.46")
	field, hashHex := verifyClientEgressField(ip, settings)
	changedSettings := *settings
	changedSettings.EgressHashV4Prefix = 24
	changedScore := VerifyEgressIpHashWithSettings(ip, &changedSettings)
	changedField := fmt.Sprintf("%s_4_%x", hashHex, changedScore)
	entries := map[string]verifyEgressEntry{}
	steps := []struct {
		field    string
		expireMs string
		family   int
		expire   uint64
	}{
		{field: hashHex, expireMs: "3000", family: 0, expire: 3000},
		{field: field, expireMs: "1500", family: 4, expire: 1500},
		{field: hashHex, expireMs: "4000", family: 4, expire: 1500},
		{field: changedField, expireMs: "2500", family: 4, expire: 2500},
		{field: field, expireMs: "2000", family: 4, expire: 2500},
	}
	for _, step := range steps {
		if expired := addVerifyEgressEntries(entries, map[string]string{step.field: step.expireMs}, 1000); len(expired) != 0 {
			t.Fatalf("live field %q reported expired", step.field)
		}
		if entry := entries[hashHex]; len(entries) != 1 || entry.family != step.family || entry.expireMs != step.expire {
			t.Fatalf("after %q at %s the address is %+v, want family %d expiring %d", step.field, step.expireMs, entries, step.family, step.expire)
		}
	}
	if entries[hashHex].scoreHash != changedScore {
		t.Fatal("the later known entry did not supply the score hash")
	}
	expired := addVerifyEgressEntries(entries, map[string]string{field: "1000"}, 1000)
	if len(expired) != 1 || expired[field] != "1000" || entries[hashHex].expireMs != 2500 {
		t.Fatalf("expired field = %v, entries = %+v", expired, entries)
	}
}

// §8.2 rolling deploy: a pre-v3 writer's v2 entry, a bare address hash, still
// attributes its address and keeps the provider eligible. Its family is
// unknown, so another address beside it is refused; the same address fed by a
// current writer is still one address. No current writer feeds v2, so the
// entry ends one EgressTtl after its last write and the v3 address then
// attributes alone.
func TestVerifyEgressV2EntryAttributesForOneTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("v2-dual-read-test")
		clientId := server.NewId()
		v4, v6 := netip.MustParseAddr("192.0.2.47"), netip.MustParseAddr("2001:db8:47::1")
		_, v4HashHex := verifyClientEgressField(v4, settings)
		testVerifySetProvideModes(ctx, clientId)
		oldWrite := server.NowUtc().Add(-time.Second)
		server.Redis(ctx, func(r server.RedisClient) {
			// what a pre-v3 writer ran: the same scripts, on the v2 index
			updateVerifyEgressLease(ctx, r, verifyClientEgressV2Key(clientId), clientId, v4HashHex, server.NewId().String(), oldWrite, oldWrite.Add(settings.EgressTtl))
		})
		updateVerifyEligibleMembership(ctx, clientId)
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)

		connectionIds := testVerifyFeedAddresses(ctx, clientId, settings, v4)
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)
		ClearVerifyConnectionEgress(ctx, clientId, connectionIds[v4], v4, settings)
		requireVerifyEgressLeaseOwner(t, ctx, v4, clientId, settings)

		testVerifyFeedAddresses(ctx, clientId, settings, v6)
		if got := ResolveVerifyEgress(ctx, v6, settings); got != nil {
			t.Fatalf("an address resolved beside an entry of unknown family: %s", got)
		}
		if got := ResolveVerifyEgress(ctx, v4, settings); got == nil || *got != clientId {
			t.Fatalf("v2 entry stopped attributing during the rollout: %v", got)
		}
		if testVerifyEligible(ctx, clientId) {
			t.Fatal("an address beside an entry of unknown family left the provider eligible")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			fields, err := r.HGetAll(ctx, verifyClientEgressV2Key(clientId)).Result()
			server.Raise(err)
			if len(fields) != 1 || fields[v4HashHex] != fmt.Sprint(oldWrite.Add(settings.EgressTtl).UnixMilli()) {
				t.Fatalf("a current writer fed the v2 index: %v", fields)
			}
		})

		end := uint64(oldWrite.Add(settings.EgressTtl).UnixMilli())
		if got, _ := resolveVerifyEgressAt(ctx, v4, settings, end); got != nil {
			t.Fatalf("v2 entry attributed one EgressTtl after its last write: %s", got)
		}
		if got, _ := resolveVerifyEgressAt(ctx, v6, settings, end); got == nil || *got != clientId {
			t.Fatalf("v3 address did not attribute alone after the v2 entry ended: %v", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if !verifyClientHasLiveEgress(ctx, r, clientId, end) {
				t.Fatal("provider was not eligible on its v3 address after the v2 entry ended")
			}
		})
	})
}

// §8.2 rolling deploy: withdrawals reach what a pre-v3 writer left in the v2
// index. Releasing a proxy allocation withdraws its v2 lease and forward
// entry, and reaping deletes the v2 index and its leases, so neither leaves a
// recycled address attributed for the rest of its TTL.
func TestVerifyEgressV2EntriesAreWithdrawn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("v2-withdrawal-test")
		released, reaped := server.NewId(), server.NewId()
		releasedIp, reapedIp := netip.MustParseAddr("192.0.2.48"), netip.MustParseAddr("192.0.2.49")
		_, releasedHashHex := verifyClientEgressField(releasedIp, settings)
		_, reapedHashHex := verifyClientEgressField(reapedIp, settings)
		now := server.NowUtc()
		for clientId, owner := range map[server.Id]string{released: verifyProxyEgressLeaseOwner, reaped: server.NewId().String()} {
			hashHex := releasedHashHex
			if clientId == reaped {
				hashHex = reapedHashHex
			}
			testVerifySetProvideModes(ctx, clientId)
			server.Redis(ctx, func(r server.RedisClient) {
				updateVerifyEgressLease(ctx, r, verifyClientEgressV2Key(clientId), clientId, hashHex, owner, now, now.Add(settings.EgressTtl))
			})
			updateVerifyEligibleMembership(ctx, clientId)
		}
		requireVerifyEgressLeaseOwner(t, ctx, releasedIp, released, settings)
		requireVerifyEgressLeaseOwner(t, ctx, reapedIp, reaped, settings)

		clearVerifyProxyEgressForClient(ctx, released)
		RemoveVerifyEgressForClient(ctx, reaped)
		for clientId, ip := range map[server.Id]netip.Addr{released: releasedIp, reaped: reapedIp} {
			_, hashHex := verifyClientEgressField(ip, settings)
			if got := ResolveVerifyEgress(ctx, ip, settings); got != nil || testVerifyEligible(ctx, clientId) {
				t.Fatalf("withdrawn v2 entry still attributed %s: %v", ip, got)
			}
			v2Key := verifyClientEgressV2Key(clientId)
			requireVerifyKeysAbsent(t, ctx, v2Key, verifyClientEgressLeaseKey(v2Key, hashHex), verifyEgressKeyFromHex(hashHex))
		}
	})
}

// §9: the provider meters count per resolved provider whichever family its
// source ip is in, so a provider with an address in each family keeps one
// seed and one EXTEND budget. An excluded source skips them, as it skips the
// source-ip meters.
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
	})
}

// §8.1, D27: the server-side head estimate scores each provider by one
// address: its IPv4 address while it has one, else its other addresses.
func TestVerifyLiveEgressHashesScoreOneAddressPerProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("head-estimate-test")
		dual, v6Only, preV3, absent := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		dualV4, dualV6 := netip.MustParseAddr("192.0.2.51"), netip.MustParseAddr("2001:db8:51::1")
		v6OnlyIp, preV3Ip := netip.MustParseAddr("2001:db8:52::1"), netip.MustParseAddr("192.0.2.53")
		testVerifyFeedAddresses(ctx, dual, settings, dualV4, dualV6)
		testVerifyFeedAddresses(ctx, v6Only, settings, v6OnlyIp)
		_, preV3HashHex := verifyClientEgressField(preV3Ip, settings)
		now := server.NowUtc()
		server.Redis(ctx, func(r server.RedisClient) {
			updateVerifyEgressLease(ctx, r, verifyClientEgressV2Key(preV3), preV3, preV3HashHex, server.NewId().String(), now, now.Add(settings.EgressTtl))
		})
		got := GetVerifyLiveEgressHashes(ctx, []server.Id{dual, v6Only, preV3, absent})
		want := map[server.Id][]string{}
		for clientId, ip := range map[server.Id]netip.Addr{dual: dualV4, v6Only: v6OnlyIp, preV3: preV3Ip} {
			_, hashHex := verifyClientEgressField(ip, settings)
			want[clientId] = []string{hashHex}
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
