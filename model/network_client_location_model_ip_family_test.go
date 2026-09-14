package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// find-providers2 draws the preferred facet first and labels every provider
// with its proven category (connect/IPV6.md A8).
func TestFindProviders2IpFamily(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		locationGroup := &LocationGroup{
			Name:     StrongPrivacyLaws,
			Promoted: true,
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, locationGroup)

		type testConnection struct {
			address string
			intent  int
		}
		type testProvider struct {
			name        string
			connections []testConnection
			family      connect.IpFamily
		}
		testProviders := []testProvider{
			{"dualstack-a", []testConnection{{"10.0.0.1:1", 4}, {"[2001:db8:a::1]:1", 6}}, connect.IpFamilyDualstack},
			{"dualstack-b", []testConnection{{"10.0.0.2:1", 4}, {"[2001:db8:b::1]:1", 6}}, connect.IpFamilyDualstack},
			{"v4-legacy-c", []testConnection{{"10.0.0.3:1", 0}}, connect.IpFamilyV4Only},
			{"v4-d", []testConnection{{"10.0.0.4:1", 4}}, connect.IpFamilyV4Only},
			{"v6-e", []testConnection{{"[2001:db8:e::1]:1", 6}}, connect.IpFamilyV6Only},
			// a v6 intent that arrived over v4 proves nothing and reads as v4-only
			{"mismatch-f", []testConnection{{"10.0.0.6:1", 6}}, connect.IpFamilyV4Only},
		}

		clientIds := map[string]server.Id{}
		families := map[server.Id]connect.IpFamily{}
		var callerSession *session.ClientSession
		for i, p := range testProviders {
			networkId := server.NewId()
			userId := server.NewId()
			clientSession := session.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(networkId, userId, fmt.Sprintf("network%d", i), false, false),
			)
			if callerSession == nil {
				callerSession = clientSession
			}

			clientId := server.NewId()
			clientIds[p.name] = clientId
			families[clientId] = p.family
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			handlerId := CreateNetworkClientHandler(ctx)

			var clientAddressHash [32]byte
			for j, c := range p.connections {
				connectionId, _, _, hash, err := ConnectNetworkClientWithIpFamily(ctx, clientId, c.address, handlerId, c.intent)
				connect.AssertEqual(t, err, nil)
				err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
				connect.AssertEqual(t, err, nil)
				if j == 0 {
					clientAddressHash = hash
				}
			}

			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: make([]byte, 32),
			})
			AddClientReliabilityStats(
				ctx,
				networkId,
				clientId,
				clientAddressHash,
				server.NowUtc(),
				&ClientReliabilityStats{
					ConnectionEstablishedCount: 1,
					ProvideEnabledCount:        1,
					ReceiveMessageCount:        1,
					ReceiveByteCount:           1024,
					SendMessageCount:           1,
					SendByteCount:              1024,
				},
			)
		}

		UpdateClientReliabilityScores(ctx, server.NowUtc().Add(time.Hour), true)
		err := UpdateClientScores(ctx, 5*time.Minute, 1)
		connect.AssertEqual(t, err, nil)

		find := func(ipFamily string, count int) []*FindProvidersProvider {
			t.Helper()
			result, err := FindProviders2(&FindProviders2Args{
				Specs: []*ProviderSpec{
					{LocationGroupId: &locationGroup.LocationGroupId},
				},
				Count:        count,
				ForceCount:   true,
				ForceMinimum: true,
				IpFamily:     ipFamily,
			}, callerSession)
			connect.AssertEqual(t, err, nil)
			return result.Providers
		}
		names := func(providers []*FindProvidersProvider) []string {
			names := []string{}
			for _, provider := range providers {
				for name, clientId := range clientIds {
					if clientId == provider.ClientId {
						names = append(names, name)
					}
				}
			}
			slices.Sort(names)
			return names
		}
		expectNames := func(providers []*FindProvidersProvider, want ...string) {
			t.Helper()
			slices.Sort(want)
			got := names(providers)
			if !slices.Equal(got, want) {
				t.Fatalf("providers = %v, want %v", got, want)
			}
			// every provider is labeled with its proven category
			for _, provider := range providers {
				connect.AssertEqual(t, connect.IpFamily(provider.IpFamily), families[provider.ClientId])
			}
		}

		// the default and v4-capable filters are every v4 carrier
		expectNames(find("", 20), "dualstack-a", "dualstack-b", "v4-legacy-c", "v4-d", "mismatch-f")
		expectNames(find("v4-capable", 20), "dualstack-a", "dualstack-b", "v4-legacy-c", "v4-d", "mismatch-f")
		// v6-capable is dualstack plus v6-only
		expectNames(find("v6-capable", 20), "dualstack-a", "dualstack-b", "v6-e")
		// the exact categories
		expectNames(find("dualstack", 20), "dualstack-a", "dualstack-b")
		expectNames(find("v4-only", 20), "v4-legacy-c", "v4-d", "mismatch-f")
		expectNames(find("v6-only", 20), "v6-e")

		// the preferred facet fills the count first: two v4-capable slots go
		// to the two dualstack providers, and a third is the first v4-only one
		for range 8 {
			expectNames(find("v4-capable", 2), "dualstack-a", "dualstack-b")
			providers := find("v4-capable", 3)
			connect.AssertEqual(t, len(providers), 3)
			connect.AssertEqual(t, connect.IpFamily(providers[0].IpFamily), connect.IpFamilyDualstack)
			connect.AssertEqual(t, connect.IpFamily(providers[1].IpFamily), connect.IpFamilyDualstack)
			connect.AssertEqual(t, connect.IpFamily(providers[2].IpFamily), connect.IpFamilyV4Only)
			providers = find("v6-capable", 1)
			connect.AssertEqual(t, len(providers), 1)
			connect.AssertEqual(t, connect.IpFamily(providers[0].IpFamily), connect.IpFamilyDualstack)
		}

		// an unknown filter is a 400 even when nothing would be discovered
		fixedClientId := clientIds["v4-d"]
		_, err = FindProviders2(&FindProviders2Args{
			Specs:    []*ProviderSpec{{ClientId: &fixedClientId}},
			Count:    1,
			IpFamily: "v4",
		}, callerSession)
		if err == nil || !strings.HasPrefix(err.Error(), "400 ") {
			t.Fatalf("unknown ip_family err = %v, want a 400", err)
		}
	})
}

func gobEncodeForTest(t testing.TB, value any) []byte {
	t.Helper()
	b := bytes.NewBuffer(nil)
	if err := gob.NewEncoder(b).Encode(value); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func clientScoreForTest(clientId server.Id, ipFamilies uint8) *ClientScore {
	return &ClientScore{
		ClientId:       clientId,
		IpFamilies:     ipFamilies,
		Scores:         map[string]int{RankModeQuality: 0},
		Tiers:          map[string]int{RankModeQuality: 0},
		ScaledWeights:  map[string]float32{RankModeQuality: 1},
		PassesMinimums: map[string]bool{RankModeQuality: true},
	}
}

// A cache written by an exporter without facets is read through its
// un-faceted buckets, and a faceted cache hides them.
func TestLoadClientScoresIpFamilyFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		locationId := server.NewId()
		legacyClientId := server.NewId()
		facetClientId := server.NewId()
		ttl := time.Minute

		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(
				ctx,
				clientScoreLocationCountsKey(false, RankModeQuality, locationId, server.Id{}),
				gobEncodeForTest(t, []int{1}),
				ttl,
			).Err())
			server.Raise(r.Set(
				ctx,
				clientScoreLocationSampleKey(false, RankModeQuality, locationId, server.Id{}, 0),
				gobEncodeForTest(t, []*ClientScore{clientScoreForTest(legacyClientId, 0)}),
				ttl,
			).Err())
		})

		load := func(facets ...ipFamilyFacet) map[server.Id]*ClientScore {
			t.Helper()
			clientScores, err := loadClientScores(
				false,
				RankModeQuality,
				ctx,
				map[server.Id]bool{locationId: true},
				map[server.Id]bool{},
				server.Id{},
				100,
				facets,
			)
			connect.AssertEqual(t, err, nil)
			return clientScores
		}

		// the un-faceted buckets are the fallback for every filter; their
		// legacy scores read as v4-only, which find-providers2 then filters
		clientScores := load(ipFamilyFacetDualstack, ipFamilyFacetV4Only)
		connect.AssertEqual(t, len(clientScores), 1)
		connect.AssertEqual(t, clientScores[legacyClientId].IpFamily(), connect.IpFamilyV4Only)
		clientScores = load(ipFamilyFacetDualstack, ipFamilyFacetV6Only)
		connect.AssertEqual(t, len(clientScores), 1)

		// a faceted cache, every facet present, hides the un-faceted buckets
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(
				ctx,
				clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetDualstack),
				gobEncodeForTest(t, []int{}),
				ttl,
			).Err())
			server.Raise(r.Set(
				ctx,
				clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only),
				gobEncodeForTest(t, []int{1}),
				ttl,
			).Err())
			server.Raise(r.Set(
				ctx,
				clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only, 0),
				gobEncodeForTest(t, []*ClientScore{clientScoreForTest(facetClientId, ClientScoreIpFamilyV4)}),
				ttl,
			).Err())
			server.Raise(r.Set(
				ctx,
				clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV6Only),
				gobEncodeForTest(t, []int{}),
				ttl,
			).Err())
		})
		clientScores = load(ipFamilyFacetDualstack, ipFamilyFacetV4Only)
		connect.AssertEqual(t, len(clientScores), 1)
		_, ok := clientScores[facetClientId]
		connect.AssertEqual(t, ok, true)
		_, ok = clientScores[legacyClientId]
		connect.AssertEqual(t, ok, false)
		// the v6-capable facets are present and empty: nothing, not the fallback
		clientScores = load(ipFamilyFacetDualstack, ipFamilyFacetV6Only)
		connect.AssertEqual(t, len(clientScores), 0)
	})
}

// The fanout writes every facet always and the un-faceted payload only while
// the compatibility flag is on.
func TestEmitClientScoreTargetFanoutFacets(t *testing.T) {
	keys := clientScoreTargetKeys{
		counts: func(callerId server.Id) string { return "c" },
		filter: func(callerId server.Id) string { return "f" },
		sample: func(callerId server.Id, index int) string { return fmt.Sprintf("s_%d", index) },
		alias:  func(callerId server.Id) string { return "a" },
		facetCounts: func(callerId server.Id, facet ipFamilyFacet) string {
			return fmt.Sprintf("c_%s", facet)
		},
		facetSample: func(callerId server.Id, facet ipFamilyFacet, index int) string {
			return fmt.Sprintf("s_%s_%d", facet, index)
		},
	}
	encode := func(map[server.Id]*ClientScore) clientScoreExportPayload {
		encodeSample := func(int) []byte { return []byte("sample") }
		return clientScoreExportPayload{
			countsBytes:  []byte("counts"),
			filterBytes:  []byte("filter"),
			counts:       []int{1},
			encodeSample: encodeSample,
			facets: map[ipFamilyFacet]clientScoreFacetPayload{
				ipFamilyFacetDualstack: {countsBytes: []byte("counts"), counts: []int{1}, encodeSample: encodeSample},
				ipFamilyFacetV4Only:    {countsBytes: []byte("counts"), counts: []int{}, encodeSample: encodeSample},
				ipFamilyFacetV6Only:    {countsBytes: []byte("counts"), counts: []int{}, encodeSample: encodeSample},
			},
		}
	}
	emitted := func(writeUnfacetedPayload bool) []string {
		emittedKeys := []string{}
		err := emitClientScoreTargetFanout(
			[]server.Id{{}},
			map[server.Id]*ClientScore{},
			map[server.Id]map[server.Id]bool{},
			keys,
			encode,
			false,
			writeUnfacetedPayload,
			func(set clientScoreRedisSet) error {
				emittedKeys = append(emittedKeys, set.key)
				return nil
			},
		)
		connect.AssertEqual(t, err, nil)
		slices.Sort(emittedKeys)
		return emittedKeys
	}
	connect.AssertEqual(t, emitted(false), []string{"c_4", "c_6", "c_d", "f", "s_d_0"})
	connect.AssertEqual(t, emitted(true), []string{"c", "c_4", "c_6", "c_d", "f", "s_0", "s_d_0"})
}
