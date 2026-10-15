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

	"github.com/urnetwork/server/v2026"
)

// One location's providers as the score export publishes them, and the
// Quality native that loses its connection after that publication.
type offlineTestFixture struct {
	city *Location
	// live, verified: Quality and Speed natives
	natives []server.Id
	// verified when the export ran, then disconnected: still a Quality and
	// Speed native and an online member in every published bucket
	offline             server.Id
	offlineConnectionId server.Id
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
	fixture.offline, fixture.offlineConnectionId = offline.clientId, offline.connectionId
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

// The real export path publishes three Quality natives; one then
// disconnects. A Quality request's live read finds it without a live
// connection, and before this change took it out of Quality only: the Speed
// bucket, where it is still a native, lent it at a borrowed tier, or the
// Online fallback did. It is now in no draw of the request. A short Speed
// request draws it as a Speed native before its Quality backfill reads it
// live; that read revokes it the way a later risk verdict does.
//
// The verdict is kept nowhere, not even in the one-second cache of Quality
// refusals: with that cache's clock held still, the provider's reconnection
// is seen by the very next request. force_minimum, online and named requests
// answer as before.
func TestFindProviders2OfflineNativeIsNotLent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		// a cache of Quality refusals whose entries never expire here
		_, cacheNow := subscriberCacheTestClock()
		previousCache := providerSubscriberNegativeCache
		providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, cacheNow)
		providerSubscriberNegativeCache.observe = observeSubscriberEligibilityEvent
		t.Cleanup(func() { providerSubscriberNegativeCache = previousCache })
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Offline")
		offset := egressTestBackfillOffset()

		// every live provider of the location at its tier in each answer
		qualityWant := map[server.Id]int{
			fixture.natives[0]:   0,
			fixture.natives[1]:   0,
			fixture.speedOnly[0]: offset,
			fixture.speedOnly[1]: offset,
			fixture.onlineOnly:   2 * offset,
		}
		speedWant := map[server.Id]int{
			fixture.natives[0]:   0,
			fixture.natives[1]:   0,
			fixture.speedOnly[0]: 0,
			fixture.speedOnly[1]: 0,
			fixture.onlineOnly:   2 * offset,
		}
		offlineTestForEachReader(t, func(nativeReader bool) {
			for range 2 {
				quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
				assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t Quality", nativeReader), quality, fixture.offline, qualityWant)
				speed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, false, server.NewId())
				assertOfflineTestAnswer(t, fmt.Sprintf("reader=%t Speed", nativeReader), speed, fixture.offline, speedWant)
			}

			// force_minimum Quality refused it before and still does; its
			// forced pool has no minimums, so the verified online member is
			// in it. Forced Speed reads no live fact and answers it.
			forced := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, true, server.NewId())
			if !slices.Equal(onlineTestSorted(egressTestIds(forced)...), onlineTestSorted(append(slices.Clone(fixture.natives), fixture.onlineOnly)...)) {
				t.Fatalf("reader=%t: forced Quality changed", nativeReader)
			}
			forcedSpeed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, true, server.NewId())
			if !slices.Contains(egressTestIds(forcedSpeed), fixture.offline) {
				t.Fatalf("reader=%t: forced Speed changed", nativeReader)
			}
			// Online reads no live fact, so it answers the published bucket.
			online := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeOnline, 10, false, server.NewId())
			everyone := append(slices.Clone(fixture.natives), fixture.offline, fixture.speedOnly[0], fixture.speedOnly[1], fixture.onlineOnly)
			if !slices.Equal(onlineTestSorted(egressTestIds(online)...), onlineTestSorted(everyone...)) {
				t.Fatalf("reader=%t: online changed", nativeReader)
			}
		})

		// Named, it is answered as before: refused by Quality's live read,
		// returned by Speed, which reads none, and kept by a Speed discovery
		// request that names it even after that request's Quality backfill
		// reads it live.
		named := []*ProviderSpec{{ClientId: &fixture.offline}}
		if providers := egressTestFind(ctx, t, named, RankModeQuality, 10, false, server.NewId()); len(providers) != 0 {
			t.Fatal("named Quality answered the provider without a live connection")
		}
		if providers := egressTestFind(ctx, t, named, RankModeSpeed, 10, false, server.NewId()); !slices.Equal(egressTestIds(providers), []server.Id{fixture.offline}) {
			t.Fatal("named Speed changed")
		}
		mixed := egressTestFind(ctx, t, append(slices.Clone(named), egressTestLocationSpec(fixture.city)...), RankModeSpeed, 10, false, server.NewId())
		if len(mixed) == 0 || mixed[0].ClientId != fixture.offline {
			t.Fatal("a Speed discovery request lost the provider it names")
		}
		assertOfflineTestAnswer(t, "named and discovered Speed", mixed, server.Id{}, offlineTestWith(speedWant, fixture.offline, 0))

		// It reconnects, verified. The next request reads it live again and
		// draws it as a Quality native: no refusal outlived the requests that
		// read it, though the refusal cache's clock has not moved.
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, fixture.offline, "192.0.2.241:0", handlerId)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, connectionId, fixture.city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
			t.Fatal(err)
		}
		quality := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, 10, false, server.NewId())
		assertOfflineTestAnswer(t, "reconnected Quality", quality, server.Id{}, offlineTestWith(qualityWant, fixture.offline, 0))
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

// A copy of the tiers `want` with clientId at tier.
func offlineTestWith(want map[server.Id]int, clientId server.Id, tier int) map[server.Id]int {
	with := maps.Clone(want)
	with[clientId] = tier
	return with
}
