package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Forced pools keep providers in their original family facet. A dualstack
// provider classified as IPv4 must still be found by every compatible class draw.
func TestLoadFindProviders2ExportedForcedDowngrades(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		scores := map[server.Id]*ClientScore{}
		for _, families := range []uint8{ClientScoreIpFamilyV4, ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6, ClientScoreIpFamilyV6} {
			for _, downgrades := range []uint8{0, ClientScoreCapabilityDowngradeIpv6, ClientScoreCapabilityDowngradeUdp, ClientScoreCapabilityDowngradeIpv6 | ClientScoreCapabilityDowngradeUdp} {
				score := clientScoreForTest(server.NewId(), families)
				score.CapabilityDowngrades, score.Online = downgrades, true
				scores[score.ClientId] = score
			}
		}
		locationId := server.NewId()
		payload := exportClientScores(true, RankModeQuality, scores)
		server.Redis(ctx, func(r server.RedisClient) {
			for facet, pages := range payload.facets {
				server.Raise(r.Set(ctx, clientScoreLocationFacetCountsKey(true, RankModeQuality, locationId, server.Id{}, facet), pages.countsBytes, time.Hour).Err())
				for index := range pages.counts {
					server.Raise(r.Set(ctx, clientScoreLocationFacetSampleKey(true, RankModeQuality, locationId, server.Id{}, facet, index), pages.encodeSample(index), time.Hour).Err())
				}
			}
		})
		for _, classes := range []providerCapabilityClassSet{
			newProviderCapabilityClassSet(providerCapabilityClassB),
			newProviderCapabilityClassSet(providerCapabilityClassD),
			newProviderCapabilityClassSet(providerCapabilityClassB, providerCapabilityClassD),
			newProviderCapabilityClassSet(providerCapabilityClassA),
			newProviderCapabilityClassSet(providerCapabilityClassC),
			newProviderCapabilityClassSet(providerCapabilityClasses...),
		} {
			loaded, _, err := loadFindProviders2PoolUnion(true, RankModeQuality, &findProviders2PoolRead{
				ctx: ctx, locationIds: map[server.Id]bool{locationId: true},
				n: 1000, partition: clientScorePartitionOff, observation: &findProviders2LoadObservation{},
			}, newFindProviders2PoolFacets(classes, true))
			if err != nil {
				t.Fatal(err)
			}
			for clientId, score := range scores {
				want := classes.has(score.capabilityClass()) && score.IpFamily() != connect.IpFamilyV6Only
				if (loaded[clientId] != nil) != want {
					t.Errorf("classes %v: original family %s, downgrades %d: loaded %t, want %t", classes, score.IpFamily(), score.CapabilityDowngrades, loaded[clientId] != nil, want)
				}
			}
		}
	})
}

// The records of a selector-mode read, by client id.
func selectReadTestIds(scores map[server.Id]*ClientScore) []server.Id {
	clientIds := []server.Id{}
	for clientId := range scores {
		clientIds = append(clientIds, clientId)
	}
	slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return clientIds
}

func selectReadTestSorted(clientIds ...server.Id) []server.Id {
	clientIds = slices.Clone(clientIds)
	slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return clientIds
}

// One page of records under a non-forced Quality facet of the location.
func selectReadTestWritePage(ctx context.Context, t testing.TB, locationId server.Id, facet ipFamilyFacet, records ...*ClientScore) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		counts := []int{}
		if 0 < len(records) {
			counts = []int{len(records)}
			server.Raise(r.Set(ctx, clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, server.Id{}, facet, 0), gobEncodeForTest(t, records), time.Minute).Err())
		}
		server.Raise(r.Set(ctx, clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, facet), gobEncodeForTest(t, counts), time.Minute).Err())
	})
}

// A selector-mode read of today's facets skips the record of a downgraded
// provider, whose classified copy it reads from the capability facet of its
// class, and never reads the un-faceted pages of an older exporter, which
// hold every class (FP2-PROTOCOL.md §6.4).
func TestLoadFindProviders2PoolSkipsDowngradedFamilyRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
		locationId := server.NewId()
		aId, bId, bDualId, cId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		record := func(clientId server.Id, ipFamilies uint8, downgrades uint8) *ClientScore {
			score := clientScoreForTest(clientId, ipFamilies)
			score.CapabilityDowngrades = downgrades
			score.Online = true
			return score
		}
		// today's facets: the strict records, the downgraded ones included
		selectReadTestWritePage(ctx, t, locationId, ipFamilyFacetDualstack,
			record(aId, dualstack, 0),
			record(bDualId, dualstack, ClientScoreCapabilityDowngradeIpv6),
			record(cId, dualstack, ClientScoreCapabilityDowngradeUdp),
		)
		selectReadTestWritePage(ctx, t, locationId, ipFamilyFacetV4Only, record(bId, ClientScoreIpFamilyV4, 0))
		selectReadTestWritePage(ctx, t, locationId, ipFamilyFacetV6Only)
		// the capability facets: the classified copies
		selectReadTestWritePage(ctx, t, locationId, clientScoreCapabilityFacetIpv4Dualproto, record(bDualId, dualstack, ClientScoreCapabilityDowngradeIpv6))
		selectReadTestWritePage(ctx, t, locationId, clientScoreCapabilityFacetDualstackTcp, record(cId, dualstack, ClientScoreCapabilityDowngradeUdp))
		selectReadTestWritePage(ctx, t, locationId, clientScoreCapabilityFacetIpv4Tcp)

		read := func(locationId server.Id, classes ...providerCapabilityClass) (map[server.Id]*ClientScore, *findProviders2LoadObservation) {
			observation := &findProviders2LoadObservation{}
			scores, _, err := loadFindProviders2PoolUnion(false, RankModeQuality, &findProviders2PoolRead{
				ctx:              ctx,
				locationIds:      map[server.Id]bool{locationId: true},
				locationGroupIds: map[server.Id]bool{},
				n:                1000,
				partition:        clientScorePartitionOff,
				observation:      observation,
			}, newFindProviders2PoolFacets(newProviderCapabilityClassSet(classes...), false))
			connect.AssertEqual(t, err, nil)
			return scores, observation
		}
		scores, _ := read(locationId, providerCapabilityClassA, providerCapabilityClassB)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(aId, bId, bDualId))
		connect.AssertEqual(t, scores[bDualId].capabilityClass(), providerCapabilityClassB)
		scores, _ = read(locationId, providerCapabilityClassC, providerCapabilityClassD)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(cId))
		scores, _ = read(locationId, providerCapabilityClassA)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(aId))

		// an older exporter's un-faceted pages are never a selector source
		legacyLocationId := server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientScoreLocationCountsKey(false, RankModeQuality, legacyLocationId, server.Id{}), gobEncodeForTest(t, []int{1}), time.Minute).Err())
			server.Raise(r.Set(ctx, clientScoreLocationSampleKey(false, RankModeQuality, legacyLocationId, server.Id{}, 0), gobEncodeForTest(t, []*ClientScore{record(server.NewId(), 0, 0)}), time.Minute).Err())
		})
		scores, observation := read(legacyLocationId, providerCapabilityClassC, providerCapabilityClassD)
		connect.AssertEqual(t, len(scores), 0)
		connect.AssertEqual(t, observation.missingTargets, 1)
	})
}

// The rolling deploy (§5.4): an exporter that predates the capability
// facets wrote today's facets only, with no downgrade bits. Selector mode
// answers classes A and B from them, and C and D are empty.
func TestLoadFindProviders2PoolFromAnExporterBeforeTheCapabilityFacets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		aId, bId := server.NewId(), server.NewId()
		a := clientScoreForTest(aId, ClientScoreIpFamilyV4|ClientScoreIpFamilyV6)
		b := clientScoreForTest(bId, ClientScoreIpFamilyV4)
		selectReadTestWritePage(ctx, t, locationId, ipFamilyFacetDualstack, a)
		selectReadTestWritePage(ctx, t, locationId, ipFamilyFacetV4Only, b)
		selectReadTestWritePage(ctx, t, locationId, ipFamilyFacetV6Only)
		read := func(classes ...providerCapabilityClass) (map[server.Id]*ClientScore, *clientScoreCursor) {
			scores, cursor, err := loadFindProviders2PoolUnion(false, RankModeQuality, &findProviders2PoolRead{
				ctx:              ctx,
				locationIds:      map[server.Id]bool{locationId: true},
				locationGroupIds: map[server.Id]bool{},
				n:                1000,
				partition:        clientScorePartitionOff,
				observation:      &findProviders2LoadObservation{},
			}, newFindProviders2PoolFacets(newProviderCapabilityClassSet(classes...), false))
			connect.AssertEqual(t, err, nil)
			return scores, cursor
		}
		scores, cursor := read(providerCapabilityClassA, providerCapabilityClassB)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(aId, bId))
		// the absent capability facet marks the source incomplete, not empty
		connect.AssertEqual(t, cursor.sourceIncomplete, true)
		for _, score := range scores {
			ip, protocol := score.CapabilityLabels()
			connect.AssertEqual(t, protocol, ProviderCapabilityProtocolDualproto)
			if score.ClientId == aId {
				connect.AssertEqual(t, ip, ProviderCapabilityIpDualstack)
			} else {
				connect.AssertEqual(t, ip, ProviderCapabilityIpIpv4)
			}
		}
		scores, _ = read(providerCapabilityClassC, providerCapabilityClassD)
		connect.AssertEqual(t, len(scores), 0)
	})
}

// The native capability snapshot (§4.4): its own pointer and manifest of
// exactly i, t and x, which today's reader refuses as it refuses any
// manifest whose facet count is not three, while a selector-mode read takes
// natives from both snapshots. Before any export wrote it, a selector read
// takes today's natives alone.
func TestLoadFindProviders2PoolNativeReadsBothSnapshots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		countsKey := clientScoreLocationCountsKey(false, RankModeQuality, locationId, server.Id{})
		aId, bDualId, cId := server.NewId(), server.NewId(), server.NewId()
		native := func(clientId server.Id, ipFamilies uint8, downgrades uint8) *ClientScore {
			score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
			score.ClientId = clientId
			score.IpFamilies = ipFamilies
			score.CapabilityDowngrades = downgrades
			return score
		}
		dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
		read := func(classes ...providerCapabilityClass) (map[server.Id]*ClientScore, bool) {
			scores, _, available, err := loadFindProviders2PoolNative(RankModeQuality, &findProviders2PoolRead{
				ctx:              ctx,
				locationIds:      map[server.Id]bool{locationId: true},
				locationGroupIds: map[server.Id]bool{},
				n:                1000,
				partition:        clientScorePartitionOff,
				observation:      &findProviders2LoadObservation{},
			}, newFindProviders2PoolFacets(newProviderCapabilityClassSet(classes...), false))
			connect.AssertEqual(t, err, nil)
			return scores, available
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(writeClientScoreNativeSnapshot(ctx, r, clientScoreNativeKey(countsKey), time.Hour, nativeTestFacets(t, map[ipFamilyFacet][]*ClientScore{
				ipFamilyFacetDualstack: {native(aId, dualstack, 0)},
			})))
		})
		// before the capability snapshot: today's natives alone, and C and D
		// have no native source
		scores, available := read(providerCapabilityClassA, providerCapabilityClassB)
		connect.AssertEqual(t, available, true)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(aId))
		_, available = read(providerCapabilityClassC, providerCapabilityClassD)
		connect.AssertEqual(t, available, false)

		capabilityFacets := map[ipFamilyFacet]clientScoreFacetPayload{}
		for facet, records := range map[ipFamilyFacet][]*ClientScore{
			clientScoreCapabilityFacetIpv4Dualproto: {native(bDualId, dualstack, ClientScoreCapabilityDowngradeIpv6)},
			clientScoreCapabilityFacetDualstackTcp:  {native(cId, dualstack, ClientScoreCapabilityDowngradeUdp)},
			clientScoreCapabilityFacetIpv4Tcp:       {},
		} {
			counts := []int{}
			pages := [][]byte{}
			if 0 < len(records) {
				counts = []int{len(records)}
				pages = [][]byte{gobEncodeForTest(t, records)}
			}
			capabilityFacets[facet] = clientScoreFacetPayload{
				counts: counts, countsBytes: gobEncodeForTest(t, counts),
				encodeSample: func(index int) []byte { return pages[index] },
			}
		}
		capabilityKey := clientScoreNativeCapabilityKey(countsKey)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(writeClientScoreNativeSnapshotOf(ctx, r, capabilityKey, time.Hour, clientScoreCapabilityFacets, capabilityFacets))
			pointer := r.Get(ctx, capabilityKey).Val()
			// today's reader refuses the capability manifest
			if _, _, err := readClientScoreNativeManifest(ctx, r, capabilityKey, pointer); err == nil {
				t.Fatal("today's native reader accepted the capability snapshot")
			}
			manifest, _, err := readClientScoreNativeManifestOf(ctx, r, capabilityKey, pointer, clientScoreCapabilityFacets)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(manifest.Facets), 3)
			// and today's snapshot is untouched by it
			_, _, err = readClientScoreNativeManifest(ctx, r, clientScoreNativeKey(countsKey), r.Get(ctx, clientScoreNativeKey(countsKey)).Val())
			connect.AssertEqual(t, err, nil)
		})
		scores, available = read(providerCapabilityClassA, providerCapabilityClassB)
		connect.AssertEqual(t, available, true)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(aId, bDualId))
		scores, available = read(providerCapabilityClassC, providerCapabilityClassD)
		connect.AssertEqual(t, available, true)
		connect.AssertEqual(t, selectReadTestIds(scores), selectReadTestSorted(cId))
	})
}
