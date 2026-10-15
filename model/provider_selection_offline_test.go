// A provider that the request's own live read (the subscriber validation of
// readQuality) finds without a live connection: refused Quality as before,
// and now lent by no other draw of that request either, native, borrowed from
// the other bucket or Online fallback. The verdict is the request's alone.
// Named providers, force_minimum and online requests answer as they did.
package model

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// One location's providers as the score export publishes them, and the
// Quality native that loses its connection after that publication.
type offlineTestFixture struct {
	city *Location
	// live, verified: Quality and Speed natives
	natives []server.Id
	// verified when the export ran, then disconnected: still a Quality and
	// Speed native and an online member in every published bucket
	offline server.Id
	// live, ARIN non-quality: Speed natives that Quality refuses
	speedOnly []server.Id
	// live, with a failing URL ratio: online, in neither native bucket
	onlineOnly server.Id
}

func newOfflineTestFixture(ctx context.Context, t testing.TB, name string) *offlineTestFixture {
	t.Helper()
	fixture := &offlineTestFixture{
		city: egressTestCity(ctx, name+" City", name+" Region", "United States", "us"),
	}
	for range 2 {
		provider := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
		egressTestProbed(ctx, provider, fixture.city, 0, "us")
		fixture.natives = append(fixture.natives, provider.clientId)
	}
	offline := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
	egressTestProbed(ctx, offline, fixture.city, 0, "us")
	fixture.offline = offline.clientId
	for range 2 {
		fixture.speedOnly = append(fixture.speedOnly, egressTestOverLine(ctx, t, fixture.city, egressTestFast).clientId)
	}
	onlineOnly := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
	egressTestHealth(ctx, onlineOnly.clientId, server.NowUtc(), 3, 1)
	fixture.onlineOnly = onlineOnly.clientId
	egressTestPasses(ctx, t)

	published := egressTestCachedScores(ctx, t, fixture.city, RankModeQuality, false)
	for _, member := range []struct {
		clientId       server.Id
		quality, speed bool
	}{
		{fixture.natives[0], true, true},
		{fixture.natives[1], true, true},
		{fixture.offline, true, true},
		{fixture.speedOnly[0], false, true},
		{fixture.speedOnly[1], false, true},
		{fixture.onlineOnly, false, false},
	} {
		score := published[member.clientId]
		if score == nil || !score.Online || score.PassesMinimums[RankModeQuality] != member.quality || score.PassesMinimums[RankModeSpeed] != member.speed {
			t.Fatal("fixture bucket membership changed")
		}
	}

	if err := DisconnectNetworkClient(ctx, offline.connectionId); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Every live provider of the fixture's location at its tier in a Quality
// answer: the natives, then the Speed natives Quality refuses at the
// borrowed tier, then the online member at the Online fallback's.
func (self *offlineTestFixture) qualityAnswer() map[server.Id]int {
	offset := egressTestBackfillOffset()
	return map[server.Id]int{
		self.natives[0]:   0,
		self.natives[1]:   0,
		self.speedOnly[0]: offset,
		self.speedOnly[1]: offset,
		self.onlineOnly:   2 * offset,
	}
}

// The same in a Speed answer, where all four live Speed natives are native.
func (self *offlineTestFixture) speedAnswer() map[server.Id]int {
	return map[server.Id]int{
		self.natives[0]:   0,
		self.natives[1]:   0,
		self.speedOnly[0]: 0,
		self.speedOnly[1]: 0,
		self.onlineOnly:   2 * egressTestBackfillOffset(),
	}
}

// Runs each case under both cache readers: the union pages, and the native
// pages that Main reads (native_reader_enabled).
func offlineTestForEachReader(t testing.TB, run func(nativeReader bool)) {
	for _, nativeReader := range []bool{false, true} {
		func() {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
			defer pop()
			requestEgressIndexSettingsSnapshot.Store(nil)
			defer requestEgressIndexSettingsSnapshot.Store(nil)
			run(nativeReader)
		}()
	}
}

// Fails unless the answer is exactly `want`, each at its tier, in the
// client's order, and says whether the offline provider was lent.
func assertOfflineTestAnswer(t testing.TB, label string, providers []*FindProvidersProvider, offline server.Id, want map[server.Id]int) {
	t.Helper()
	assertEgressTestNoRepeats(t, providers)
	assertEgressTestTiersKeepOrder(t, providers)
	for _, provider := range providers {
		if provider.ClientId == offline {
			t.Fatalf("%s: lent the provider without a live connection at tier %d", label, provider.Tier)
		}
	}
	if len(providers) != len(want) {
		t.Fatalf("%s: answered %d providers, want %d", label, len(providers), len(want))
	}
	for _, provider := range providers {
		tier, ok := want[provider.ClientId]
		if !ok {
			t.Fatalf("%s: answered a provider outside the location", label)
		}
		if provider.Tier != tier {
			t.Fatalf("%s: answered tier %d, want %d", label, provider.Tier, tier)
		}
	}
}

// A copy of the tiers `want` with clientId at tier.
func offlineTestWith(want map[server.Id]int, clientId server.Id, tier int) map[server.Id]int {
	with := maps.Clone(want)
	with[clientId] = tier
	return with
}

// The real export path publishes three Quality natives; one then
// disconnects. A Quality request's live read finds it without a live
// connection. Before this change that took it out of Quality only, and the
// Speed bucket, where it is still a native, lent it at the borrowed tier. It
// is now in no draw of the request, and the answer is the location's five
// live providers.
func TestFindProviders2OfflineNativeIsNotLent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Offline")
		offlineTestForEachReader(t, func(nativeReader bool) {
			for i := range 2 {
				quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
				assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t request=%d Quality", nativeReader, i), quality, fixture.offline, fixture.qualityAnswer())
			}
		})
	})
}

// With the Speed bucket gone, the disconnected Quality native's only way into
// a Quality answer is the Online fallback, which lent it as an online member
// before this change. It lends the location's other online members and not
// that one.
func TestFindProviders2OfflineNativeIsNotLentByTheOnlineFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Offline Fallback")
		server.Redis(ctx, func(r server.RedisClient) {
			keys, err := r.Keys(ctx, fmt.Sprintf("{cs_0_s_*_%s}*", fixture.city.LocationId)).Result()
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) == 0 {
				t.Fatal("fixture is wrong: there is no Speed cache to remove")
			}
			for _, key := range keys {
				if err := r.Del(ctx, key).Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
		offset := egressTestBackfillOffset()
		want := map[server.Id]int{
			fixture.natives[0]:   0,
			fixture.natives[1]:   0,
			fixture.speedOnly[0]: 2 * offset,
			fixture.speedOnly[1]: 2 * offset,
			fixture.onlineOnly:   2 * offset,
		}
		offlineTestForEachReader(t, func(nativeReader bool) {
			quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
			assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t Quality", nativeReader), quality, fixture.offline, want)
		})
	})
}

// A short Speed request draws the disconnected provider as one of its own
// natives, which needs no live read. Its Quality backfill then reads it live
// as a Quality native the answer already holds, finds no live connection and
// revokes it, as a risk verdict found there revokes one. The answer is the
// location's five live providers; before this change it kept the sixth.
func TestFindProviders2OfflineNativeIsRevokedFromShortSpeedAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Offline Speed")
		offlineTestForEachReader(t, func(nativeReader bool) {
			speed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, false, server.NewId())
			assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t Speed", nativeReader), speed, fixture.offline, fixture.speedAnswer())
		})
	})
}

// The verdict lives only in the request that read it: like a risk verdict,
// it stays out of the one-second cache of Quality refusals. Here that
// cache's clock is held still, so any refusal it kept would hold for the
// rest of the test. The provider reconnects, verified, and the very next
// Quality request reads it live and draws it as a native at tier 0. Before
// this change the cached refusal from the earlier request kept it out of
// Quality, and it was borrowed from Speed instead.
func TestFindProviders2OfflineVerdictIsNotKept(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		_, cacheNow := subscriberCacheTestClock()
		previousCache := providerSubscriberNegativeCache
		providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, cacheNow)
		providerSubscriberNegativeCache.observe = observeSubscriberEligibilityEvent
		t.Cleanup(func() { providerSubscriberNegativeCache = previousCache })
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Offline Verdict")

		// the live read, and with it the refusal a cache could keep
		named := []*ProviderSpec{{ClientId: &fixture.offline}}
		if providers := egressTestFind(ctx, t, named, RankModeQuality, 10, false, server.NewId()); len(providers) != 0 {
			t.Fatal("named Quality answered the provider without a live connection")
		}
		// A named request no longer reads the native Quality guard; read it here,
		// so the cache the guard owns sees the offline verdict.
		if !subscriberQualityTestRefused(ctx, t, fixture.offline) {
			t.Fatal("the native Quality read admitted the provider without a live connection")
		}

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, fixture.offline, "192.0.2.241:0", handlerId)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, connectionId, fixture.city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
			t.Fatal(err)
		}
		quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
		assertOfflineTestAnswer(t, "reconnected Quality", quality, server.Id{}, offlineTestWith(fixture.qualityAnswer(), fixture.offline, 0))
	})
}

// The requests that must not change, on the same location. force_minimum
// Quality refused the disconnected provider before and still does; its
// forced pool has no minimums, so the verified online member is in it.
// Forced Speed and online requests read no live fact and answer it. Named,
// Quality refuses it and Speed returns it, and a Speed discovery request that
// names it keeps it although that request's Quality backfill reads it live.
func TestFindProviders2OfflineNativeLeavesOtherRequestsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Offline Unchanged")
		everyone := append(slices.Clone(fixture.natives), fixture.offline, fixture.speedOnly[0], fixture.speedOnly[1], fixture.onlineOnly)
		offlineTestForEachReader(t, func(nativeReader bool) {
			forced := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, true, server.NewId())
			if !slices.Equal(onlineTestSorted(egressTestIds(forced)...), onlineTestSorted(append(slices.Clone(fixture.natives), fixture.onlineOnly)...)) {
				t.Fatalf("reader=%t: forced Quality changed", nativeReader)
			}
			forcedSpeed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, true, server.NewId())
			if !slices.Equal(onlineTestSorted(egressTestIds(forcedSpeed)...), onlineTestSorted(everyone...)) {
				t.Fatalf("reader=%t: forced Speed changed", nativeReader)
			}
			online := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeOnline, 10, false, server.NewId())
			if !slices.Equal(onlineTestSorted(egressTestIds(online)...), onlineTestSorted(everyone...)) {
				t.Fatalf("reader=%t: online changed", nativeReader)
			}

			named := []*ProviderSpec{{ClientId: &fixture.offline}}
			if providers := egressTestFind(ctx, t, named, RankModeQuality, 10, false, server.NewId()); len(providers) != 0 {
				t.Fatalf("reader=%t: named Quality answered the provider without a live connection", nativeReader)
			}
			if providers := egressTestFind(ctx, t, named, RankModeSpeed, 10, false, server.NewId()); !slices.Equal(egressTestIds(providers), []server.Id{fixture.offline}) {
				t.Fatalf("reader=%t: named Speed changed", nativeReader)
			}
			mixed := egressTestFind(ctx, t, append(slices.Clone(named), egressTestLocationSpec(fixture.city)...), RankModeSpeed, 10, false, server.NewId())
			if len(mixed) == 0 || mixed[0].ClientId != fixture.offline {
				t.Fatalf("reader=%t: a Speed discovery request lost the provider it names", nativeReader)
			}
			assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t named and discovered Speed", nativeReader), mixed, server.Id{}, offlineTestWith(fixture.speedAnswer(), fixture.offline, 0))
		})
	})
}
