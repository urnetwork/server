package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAddDefaultLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		AddDefaultLocations(ctx, 10)
	})
}

func TestCanonicalLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		us1 := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, us1)

		connect.AssertEqual(t, us1.LocationId, us1.CountryLocationId)

		us2 := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, us2)

		connect.AssertEqual(t, us2.LocationId, us1.LocationId)
		connect.AssertEqual(t, us2.LocationId, us2.CountryLocationId)

		a := &Location{
			LocationType: LocationTypeRegion,
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, a)

		connect.AssertEqual(t, a.LocationId, a.RegionLocationId)
		connect.AssertEqual(t, a.CountryLocationId, us1.LocationId)

		b := &Location{
			LocationType: LocationTypeRegion,
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, b)

		connect.AssertEqual(t, a.LocationId, b.LocationId)
		connect.AssertEqual(t, a.RegionLocationId, b.RegionLocationId)
		connect.AssertEqual(t, a.CountryLocationId, b.CountryLocationId)

		c := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, c)

		connect.AssertEqual(t, c.RegionLocationId, a.LocationId)
		connect.AssertEqual(t, c.CountryLocationId, a.CountryLocationId)

		d := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, d)

		connect.AssertEqual(t, d.LocationId, c.LocationId)
		connect.AssertEqual(t, d.RegionLocationId, c.RegionLocationId)
		connect.AssertEqual(t, d.CountryLocationId, c.CountryLocationId)
	})
}

func TestCanonicalLocationsParallel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		n := 1000
		out := make(chan server.Id, n)

		for i := 0; i < n; i += 1 {
			go func() {
				c := &Location{
					LocationType: LocationTypeCity,
					City:         "Palo Alto",
					Region:       "California",
					Country:      "United States",
					CountryCode:  "us",
				}
				CreateLocation(ctx, c)
				out <- c.LocationId
			}()
		}

		locationIds := map[server.Id]bool{}
		for i := 0; i < n; i += 1 {
			locationId := <-out
			locationIds[locationId] = true
		}

		connect.AssertEqual(t, 1, len(locationIds))
	})
}

func TestBestAvailableProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()
		guestMode := false
		isPro := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		clientId := server.NewId()

		Testing_CreateDevice(
			ctx,
			networkIdA,
			server.NewId(),
			clientId,
			"",
			"",
		)

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)
		connect.AssertEqual(t, err, nil)

		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
		}

		SetProvide(ctx, clientId, secretKeys)

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		state := &Location{
			LocationType: LocationTypeRegion,
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, state)

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})

		createLocationGroup := &LocationGroup{
			Name:     StrongPrivacyLaws,
			Promoted: true,
			MemberLocationIds: []server.Id{
				country.LocationId,
				city.LocationId,
				state.LocationId,
			},
		}

		CreateLocationGroup(ctx, createLocationGroup)

		bestAvailable := true
		findProviders2Args := &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: bestAvailable,
				},
			},
			ForceMinimum: true,
		}

		clientAddressHash, _, err := clientSessionA.ClientAddressHashPort()
		connect.AssertEqual(t, err, nil)
		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}
		AddClientReliabilityStats(
			ctx,
			networkIdA,
			clientId,
			clientAddressHash,
			server.NowUtc(),
			stats,
		)
		UpdateClientReliabilityScores(ctx, server.NowUtc(), true)
		UpdateClientScores(ctx, 5*time.Second, 1)

		res, err := FindProviders2(findProviders2Args, clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), 1)
	})
}

func TestFindProviders2WithExclude(t *testing.T) {
	// create providers
	// search for providers with client exclude
	// search for providers with destination exclude

	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		createLocationGroup := &LocationGroup{
			Name:     StrongPrivacyLaws,
			Promoted: true,
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}

		CreateLocationGroup(ctx, createLocationGroup)

		clientSessions := map[server.Id]*session.ClientSession{}
		n := 16

		for i := range n {
			networkId := server.NewId()

			userId := server.NewId()
			guestMode := false
			isPro := false

			clientSession := session.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(
					networkId,
					userId,
					fmt.Sprintf("network%d", i),
					guestMode,
					isPro,
				),
			)

			clientId := server.NewId()

			clientSessions[clientId] = clientSession

			Testing_CreateDevice(
				ctx,
				networkId,
				server.NewId(),
				clientId,
				"",
				"",
			)

			handlerId := CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := ConnectNetworkClient(
				ctx,
				clientId,
				// use a unique ip per connection
				fmt.Sprintf("0.0.0.%d:0", i),
				handlerId,
			)
			connect.AssertEqual(t, err, nil)

			secretKeys := map[ProvideMode][]byte{
				ProvideModePublic: make([]byte, 32),
			}

			SetProvide(ctx, clientId, secretKeys)

			SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})

			clientAddressHash, _, err := clientSession.ClientAddressHashPort()
			connect.AssertEqual(t, err, nil)
			stats := &ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			}
			AddClientReliabilityStats(
				ctx,
				networkId,
				clientId,
				clientAddressHash,
				server.NowUtc(),
				stats,
			)
		}

		UpdateClientReliabilityScores(ctx, server.NowUtc().Add(time.Hour), true)
		// The TTL is the redis expiry on the score/sample keys FindProviders2
		// reads, and a cache miss there returns zero providers with a nil error
		// (see the counts-key `continue` in loadClientScores) — not an error and
		// not a db fallback. The sampling loop below runs 1024 queries and takes
		// ~5.4s, so the original 5s TTL expired mid-loop and every remaining
		// query came back empty ("0 does not equal 6"). Keep this comfortably
		// longer than the whole test.
		UpdateClientScores(ctx, 5*time.Minute, 1)

		clientIds := slices.Collect(maps.Keys(clientSessions))
		clientIdA := clientIds[0]
		clientSessionA := clientSessions[clientIdA]

		findProviders2Args := &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					LocationGroupId: &createLocationGroup.LocationGroupId,
				},
			},
			Count:        2 * n,
			ForceMinimum: true,
		}
		res, err := FindProviders2(findProviders2Args, clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), n)

		bestAvailable := true
		findProviders2Args = &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: bestAvailable,
				},
			},
			Count:        2 * n,
			ForceMinimum: true,
		}
		res, err = FindProviders2(findProviders2Args, clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), n)

		findProviders2Args = &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: bestAvailable,
				},
			},
			Count:            2 * n,
			ExcludeClientIds: []server.Id{clientIdA},
			ForceMinimum:     true,
		}
		res, err = FindProviders2(findProviders2Args, clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), n-1)

		findProviders2Args = &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: bestAvailable,
				},
			},
			ExcludeClientIds: []server.Id{clientIds[0]},
			ExcludeDestinations: [][]server.Id{
				[]server.Id{
					clientIds[1], clientIds[2], clientIds[3],
				},
				[]server.Id{
					clientIds[4], clientIds[5], clientIds[6],
				},
				[]server.Id{
					clientIds[7], clientIds[8], clientIds[9],
				},
			},
			ForceMinimum: true,
		}

		// client ids not in the exclude destinations intermediaries will come first
		priorityClientIds := map[server.Id]bool{}
		for _, clientId := range clientIds[10:] {
			priorityClientIds[clientId] = true
		}
		// the exclude destination intermediaries (not the egress hop) will come next
		// exclude [3], [6], [9] which are the egress in `ExcludeDestination`
		otherClientIds := map[server.Id]bool{}
		otherClientIds[clientIds[1]] = true
		otherClientIds[clientIds[2]] = true
		otherClientIds[clientIds[4]] = true
		otherClientIds[clientIds[5]] = true
		otherClientIds[clientIds[7]] = true
		otherClientIds[clientIds[8]] = true
		excludeClientIds := map[server.Id]bool{}
		excludeClientIds[clientIds[0]] = true
		excludeClientIds[clientIds[3]] = true
		excludeClientIds[clientIds[6]] = true
		excludeClientIds[clientIds[9]] = true

		// the match is a weighted shuffle so we should expect over
		//   sufficient iterations the priority client ids will come first
		netProviderIncludedCounts := map[server.Id]int{}
		for range 1024 {
			findProviders2Args.Count = len(priorityClientIds)
			// prevent oversampling
			findProviders2Args.ForceCount = true
			res, err = FindProviders2(findProviders2Args, clientSessionA)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(res.Providers), len(priorityClientIds))
			for _, provider := range res.Providers {
				netProviderIncludedCounts[provider.ClientId] += 1
			}
		}
		// descending by included count
		orderedClientIds := slices.Collect(maps.Keys(netProviderIncludedCounts))
		slices.SortStableFunc(orderedClientIds, func(a server.Id, b server.Id) int {
			return netProviderIncludedCounts[b] - netProviderIncludedCounts[a]
		})
		for _, clientId := range orderedClientIds[:len(priorityClientIds)] {
			ok := excludeClientIds[clientId]
			connect.AssertEqual(t, ok, false)
			ok = otherClientIds[clientId]
			connect.AssertEqual(t, ok, false)
			ok = priorityClientIds[clientId]
			connect.AssertEqual(t, ok, true)
		}
		for _, clientId := range orderedClientIds[len(priorityClientIds):] {
			ok := excludeClientIds[clientId]
			connect.AssertEqual(t, ok, false)
			ok = otherClientIds[clientId]
			connect.AssertEqual(t, ok, true)
		}

	})
}

func TestRankMode(t *testing.T) {
	// the first letter of the rank mode is used for various redis keys
	rankModes := []RankMode{RankModeQuality, RankModeSpeed}
	firstLetters := map[rune]int{}
	for _, rankMode := range rankModes {
		r, _ := utf8.DecodeRuneInString(rankMode)
		firstLetters[r] += 1
	}
	connect.AssertEqual(t, len(rankModes), len(firstLetters))
}

// write -> read round trips through the redis provider caches:
// `UpdateClientLocations` -> `loadClientLocations`/`loadInitialClientLocations` and
// `UpdateClientScores` -> `loadClientScores`/`loadLocationStables`/`FindProviders2`.
// the cache keys hash tag on the per-target ids so the families spread across
// cluster slots. the test redis is a single node, so slot spreading is invisible
// here; this proves functional equivalence of the write and read paths.
func TestClientLocationScoreCacheRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		guestMode := false
		isPro := false

		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, userId, "a", guestMode, isPro),
		)

		clientId := server.NewId()

		Testing_CreateDevice(
			ctx,
			networkId,
			server.NewId(),
			clientId,
			"",
			"",
		)

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)
		connect.AssertEqual(t, err, nil)

		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
		}
		SetProvide(ctx, clientId, secretKeys)

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		createLocationGroup := &LocationGroup{
			Name:     StrongPrivacyLaws,
			Promoted: true,
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, createLocationGroup)

		SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})

		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		connect.AssertEqual(t, err, nil)
		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}
		AddClientReliabilityStats(
			ctx,
			networkId,
			clientId,
			clientAddressHash,
			server.NowUtc(),
			stats,
		)
		UpdateClientReliabilityScores(ctx, server.NowUtc(), true)

		// UpdateClientLocations counts only a provider a probe measured
		// healthy AND observed egressing from the country it claims (see
		// providerCountFilter); this fixture claims "us", so it must be
		// observed in "us" too, or the round trip under test never happens.
		testing_setProviderEgressHealthy(ctx, clientId)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: clientId, LocationId: city.LocationId,
			CountryCode: "us", Verdict: "verified", ObservedAt: server.NowUtc(),
		})

		// client location cache round trip

		err = UpdateClientLocations(ctx, 5*time.Minute)
		connect.AssertEqual(t, err, nil)

		clientLocations, err := loadClientLocations(ctx, map[server.Id]bool{
			city.LocationId: true,
		})
		connect.AssertEqual(t, err, nil)
		// the load expands the city to its region and country
		connect.AssertEqual(t, len(clientLocations), 3)

		cityClientLocation, ok := clientLocations[city.LocationId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, cityClientLocation.LocationId, city.LocationId)
		connect.AssertEqual(t, cityClientLocation.LocationType, LocationTypeCity)
		connect.AssertEqual(t, cityClientLocation.Name, "Palo Alto")
		connect.AssertEqual(t, cityClientLocation.ClientCount, 1)
		connect.AssertEqual(t, cityClientLocation.CityLocationId, city.CityLocationId)
		connect.AssertEqual(t, cityClientLocation.RegionLocationId, city.RegionLocationId)
		connect.AssertEqual(t, cityClientLocation.CountryLocationId, city.CountryLocationId)
		connect.AssertEqual(t, cityClientLocation.CountryCode, "us")
		connect.AssertEqual(t, cityClientLocation.StrongPrivacy, true)
		connect.AssertEqual(t, len(cityClientLocation.TopCityLocationIdCounts), 0)
		connect.AssertEqual(t, len(cityClientLocation.TopRegionLocationIdCounts), 0)

		regionClientLocation, ok := clientLocations[city.RegionLocationId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, regionClientLocation.LocationType, LocationTypeRegion)
		connect.AssertEqual(t, regionClientLocation.Name, "California")
		connect.AssertEqual(t, regionClientLocation.ClientCount, 1)
		connect.AssertEqual(
			t,
			regionClientLocation.TopCityLocationIdCounts,
			map[server.Id]int{city.LocationId: 1},
		)

		countryClientLocation, ok := clientLocations[city.CountryLocationId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, countryClientLocation.LocationType, LocationTypeCountry)
		connect.AssertEqual(t, countryClientLocation.ClientCount, 1)
		connect.AssertEqual(
			t,
			countryClientLocation.TopCityLocationIdCounts,
			map[server.Id]int{city.LocationId: 1},
		)
		connect.AssertEqual(
			t,
			countryClientLocation.TopRegionLocationIdCounts,
			map[server.Id]int{city.RegionLocationId: 1},
		)

		initialClientLocations, err := loadInitialClientLocations(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(initialClientLocations.Locations), 1)
		connect.AssertEqual(t, initialClientLocations.Locations[0].LocationId, city.CountryLocationId)
		connect.AssertEqual(t, len(initialClientLocations.LocationGroups), 1)
		connect.AssertEqual(t, initialClientLocations.LocationGroups[0].LocationGroupId, createLocationGroup.LocationGroupId)
		connect.AssertEqual(t, initialClientLocations.LocationGroups[0].Name, StrongPrivacyLaws)
		connect.AssertEqual(t, initialClientLocations.LocationGroups[0].Promoted, true)

		// client score cache round trip

		err = UpdateClientScores(ctx, 5*time.Minute, 2)
		connect.AssertEqual(t, err, nil)

		locationIds := map[server.Id]bool{
			city.LocationId: true,
		}
		locationGroupIds := map[server.Id]bool{
			createLocationGroup.LocationGroupId: true,
		}
		usLocationId := countryCodeLocationIds()["us"]
		connect.AssertEqual(t, usLocationId, city.CountryLocationId)

		// the scores are written per caller location. the content must read back
		// identically for the no-match caller location and the us caller location.
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			clientScoresNoMatch, err := loadClientScores(
				true,
				rankMode,
				ctx,
				locationIds,
				locationGroupIds,
				server.Id{},
				100,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(clientScoresNoMatch), 1)

			clientScore, ok := clientScoresNoMatch[clientId]
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, clientScore.ClientId, clientId)
			connect.AssertEqual(t, clientScore.NetworkId, networkId)
			connect.AssertEqual(t, 0 < clientScore.ReliabilityWeight, true)
			_, ok = clientScore.Scores[rankMode]
			connect.AssertEqual(t, ok, true)
			_, ok = clientScore.Tiers[rankMode]
			connect.AssertEqual(t, ok, true)

			clientScoresUs, err := loadClientScores(
				true,
				rankMode,
				ctx,
				locationIds,
				locationGroupIds,
				usLocationId,
				100,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, clientScoresNoMatch, clientScoresUs)

			// the client has no latency or speed tests, which deterministically
			// fails the strict minimums. the force minimum false variant is
			// written but exports zero clients.
			clientScoresStrict, err := loadClientScores(
				false,
				rankMode,
				ctx,
				locationIds,
				locationGroupIds,
				usLocationId,
				100,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(clientScoresStrict), 0)
		}

		// the location stables read the force minimum false filter keys.
		// the filter is present with a zero count, so no entries are stable.
		locationStables, err := loadLocationStables(
			ctx,
			[]server.Id{city.LocationId, city.RegionLocationId, city.CountryLocationId},
			false,
			RankModeQuality,
			usLocationId,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(locationStables), 0)

		// end to end through the public api
		res, err := FindProviders2(
			&FindProviders2Args{
				Specs: []*ProviderSpec{
					{
						LocationId: &city.LocationId,
					},
				},
				Count:        10,
				ForceMinimum: true,
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), 1)
		connect.AssertEqual(t, res.Providers[0].ClientId, clientId)
	})
}

// func TestFindLocationGroupByName(t *testing.T) {
// 	server.DefaultTestEnv().Run(t, func(t testing.TB) {

// 		ctx := context.Background()

// 		createLocationGroup := &LocationGroup{
// 			Name:     StrongPrivacyLaws,
// 			Promoted: true,
// 		}

// 		CreateLocationGroup(ctx, createLocationGroup)

// 		server.Tx(ctx, func(tx server.PgTx) {
// 			// query existing
// 			locationGroup := findLocationGroupByNameInTx(ctx, StrongPrivacyLaws, tx)
// 			connect.AssertEqual(t, locationGroup.Name, StrongPrivacyLaws)
// 			connect.AssertEqual(t, locationGroup.Promoted, true)

// 			// locationGroupId := locationGroup.LocationGroupId

// 			// query with incorrect case should still return
// 			// locationGroup = findLocationGroupByNameInTx(ctx, "strong privacy Laws And internet freedom", tx)
// 			// connect.AssertEqual(t, locationGroup.Name, StrongPrivacyLaws)
// 			// connect.AssertEqual(t, locationGroup.LocationGroupId, locationGroupId)
// 			// connect.AssertEqual(t, locationGroup.Promoted, true)

// 			// query should return nil if no match
// 			locationGroup = findLocationGroupByNameInTx(ctx, "invalid", tx)
// 			connect.AssertEqual(t, locationGroup, nil)

// 		})
// 	})
// }

func TestCreateLocationCityCoordinates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		cityCoordinates := func(locationId server.Id) (latitude *float64, longitude *float64) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT latitude, longitude
					FROM location
					WHERE location_id = $1
					`,
					locationId,
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&latitude, &longitude))
				})
			})
			return
		}

		// created without coordinates: stored as NULL, not 0,0
		a := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, a)

		latitude, longitude := cityCoordinates(a.LocationId)
		connect.AssertEqual(t, latitude, nil)
		connect.AssertEqual(t, longitude, nil)

		// the same city created again with coordinates self-heals the row
		b := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
			Latitude:     37.4419,
			Longitude:    -122.1430,
		}
		CreateLocation(ctx, b)
		connect.AssertEqual(t, b.LocationId, a.LocationId)

		latitude, longitude = cityCoordinates(a.LocationId)
		connect.AssertEqual(t, *latitude, 37.4419)
		connect.AssertEqual(t, *longitude, -122.1430)

		// known coordinates are not overwritten
		c := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
			Latitude:     1,
			Longitude:    2,
		}
		CreateLocation(ctx, c)

		latitude, longitude = cityCoordinates(a.LocationId)
		connect.AssertEqual(t, *latitude, 37.4419)
		connect.AssertEqual(t, *longitude, -122.1430)

		// created with coordinates: stored on insert
		d := &Location{
			LocationType: LocationTypeCity,
			City:         "San Francisco",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
			Latitude:     37.7749,
			Longitude:    -122.4194,
		}
		CreateLocation(ctx, d)

		latitude, longitude = cityCoordinates(d.LocationId)
		connect.AssertEqual(t, *latitude, 37.7749)
		connect.AssertEqual(t, *longitude, -122.4194)
	})
}

func TestFindProviders2ProviderLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		guestMode := false
		isPro := false

		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, userId, "a", guestMode, isPro),
		)

		clientId := server.NewId()

		Testing_CreateDevice(
			ctx,
			networkId,
			server.NewId(),
			clientId,
			"",
			"",
		)

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)
		connect.AssertEqual(t, err, nil)

		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
		}
		SetProvide(ctx, clientId, secretKeys)

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
			Latitude:     37.4419,
			Longitude:    -122.1430,
		}
		CreateLocation(ctx, city)

		createLocationGroup := &LocationGroup{
			Name:     StrongPrivacyLaws,
			Promoted: true,
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, createLocationGroup)

		SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})

		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		connect.AssertEqual(t, err, nil)
		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}
		AddClientReliabilityStats(
			ctx,
			networkId,
			clientId,
			clientAddressHash,
			server.NowUtc(),
			stats,
		)
		UpdateClientReliabilityScores(ctx, server.NowUtc(), true)

		err = UpdateClientScores(ctx, 5*time.Minute, 1)
		connect.AssertEqual(t, err, nil)

		// the reliability rows exist now, so the directory covers the city chain
		loadLocationDirectory()

		findProviders2 := func(spec *ProviderSpec) *FindProvidersProvider {
			res, err := FindProviders2(
				&FindProviders2Args{
					Specs:        []*ProviderSpec{spec},
					Count:        10,
					ForceMinimum: true,
				},
				clientSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(res.Providers), 1)
			connect.AssertEqual(t, res.Providers[0].ClientId, clientId)
			return res.Providers[0]
		}

		// a location spec resolves the full location from the score cache
		provider := findProviders2(&ProviderSpec{
			LocationId: &city.LocationId,
		})
		location := provider.Location
		connect.AssertEqual(t, location != nil, true)
		connect.AssertEqual(t, location.Country, "United States")
		connect.AssertEqual(t, location.CountryCode, "us")
		connect.AssertEqual(t, location.Region, "California")
		connect.AssertEqual(t, location.City, "Palo Alto")
		connect.AssertEqual(t, *location.CountryLocationId, city.CountryLocationId)
		connect.AssertEqual(t, *location.RegionLocationId, city.RegionLocationId)
		connect.AssertEqual(t, *location.CityLocationId, city.CityLocationId)
		regionLat, regionLon, ok := centroidFor("us", "California")
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, location.RegionCoordinates != nil, true)
		connect.AssertEqual(t, *location.RegionCoordinates, LocationCoordinates{Lat: regionLat, Lon: regionLon})
		connect.AssertEqual(t, location.CityCoordinates != nil, true)
		connect.AssertEqual(t, *location.CityCoordinates, LocationCoordinates{Lat: 37.4419, Lon: -122.1430})

		// group-keyed samples carry no location ids, so the location is omitted
		provider = findProviders2(&ProviderSpec{
			LocationGroupId: &createLocationGroup.LocationGroupId,
		})
		connect.AssertEqual(t, provider.Location, nil)

		// before the directory has loaded, the location is omitted rather than
		// blocking the request path
		resetLocationDirectory()
		provider = findProviders2(&ProviderSpec{
			LocationId: &city.LocationId,
		})
		connect.AssertEqual(t, provider.Location, nil)
	})
}

func TestFindProvidersProviderCarriesFreshSessionSelectionMetadata(t *testing.T) {
	clientId := server.NewId()
	score := &ClientScore{
		ClientId:              clientId,
		Tiers:                 map[string]int{RankModeQuality: 0},
		MaxBytesPerSecond:     5_000_000,
		HasSpeedTest:          true,
		NetworkOnly:           true,
		ReputationFailedNames: "bloomberg",
	}
	provider := findProvidersProviderFromClientScore(score, RankModeQuality, nil)
	connect.AssertEqual(t, provider.ClientId, clientId)
	connect.AssertEqual(t, provider.Tier, 0)
	connect.AssertEqual(t, provider.EstimatedBytesPerSecond, ByteCount(5_000_000))
	connect.AssertEqual(t, provider.HasEstimatedBytesPerSecond, true)
	connect.AssertEqual(t, provider.NetworkOnly, true)
	connect.AssertEqual(t, provider.ReputationFailedNames, "bloomberg")
	connect.AssertEqual(t, provider.Location, nil)
}

// FindProviders2 gates providers on reliability minimums (0.99 independent
// reliability weight on the hour lookback). The reliability sink is
// asynchronous: the announce hot path buffers per-block counters in redis and
// the rollup flushes them to pg on its own cadence, so at ranking time the
// most recent blocks are always unflushed. Clients emitting perfect
// reliability every block must still rank at a full 1.0 weight — if the
// unflushed tail were counted as missing reliability, every provider would
// fall below the threshold and the provider market would empty.
//
// This simulates the cadences with explicit block times: emit every block
// (N), flush every 2 blocks (2N), rank every block (N).
func TestFindProviders2ReliabilityFlushLag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

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

		type testProvider struct {
			networkId         server.Id
			clientId          server.Id
			clientAddressHash [32]byte
		}

		n := 4
		providers := []*testProvider{}
		var callerSession *session.ClientSession

		for i := range n {
			networkId := server.NewId()
			clientSession := session.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(networkId, server.NewId(), fmt.Sprintf("network%d", i), false, false),
			)

			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

			handlerId := CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := ConnectNetworkClient(
				ctx,
				clientId,
				fmt.Sprintf("0.0.0.%d:0", i),
				handlerId,
			)
			connect.AssertEqual(t, err, nil)

			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: make([]byte, 32),
			})
			err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)

			// good latency and speed tests so the quality score gate passes
			// and the reliability minimums are the deciding filter
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_client_latency (connection_id, latency_ms, sample_count)
					VALUES ($1, $2, $3)
					`,
					connectionId,
					30,
					1,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count)
					VALUES ($1, $2, $3)
					`,
					connectionId,
					100*1024*1024,
					1,
				))
			})

			// measured healthy, so the reliability minimums under test are the
			// deciding filter and not the egress-health gate
			testing_setProviderEgressHealthy(ctx, clientId)

			clientAddressHash, _, err := clientSession.ClientAddressHashPort()
			connect.AssertEqual(t, err, nil)

			providers = append(providers, &testProvider{
				networkId:         networkId,
				clientId:          clientId,
				clientAddressHash: clientAddressHash,
			})
			callerSession = clientSession
		}

		perfectStats := func() *ClientReliabilityStats {
			return &ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			}
		}

		// the recorder refuses blocks older than the previous wall-clock
		// block, so the simulation runs on future block times
		base := server.NowUtc()
		blockTime := func(step int) time.Time {
			return base.Add(time.Duration(step) * ReliabilityBlockDuration)
		}

		// history: every provider has been perfectly reliable for the entire
		// max lookback before the simulation starts (direct pg backfill)
		maxLookback := ClientLookbacks[len(ClientLookbacks)-1]
		for _, p := range providers {
			AddClientReliabilityStatsRange(
				ctx,
				p.networkId,
				p.clientId,
				p.clientAddressHash,
				base.Add(-maxLookback-time.Hour),
				base,
				perfectStats(),
			)
		}

		// the drain has been live before the simulation starts, so the score
		// windows have a high-water mark to clamp to. (In production the
		// rollup task runs continuously; the work layer additionally refuses
		// to compute scores when the mark is stale, see
		// ClientReliabilityRollupSynced.)
		RollupClientReliabilityStats(ctx, base)

		// live simulation: emit every block, flush every 2 blocks, rank every
		// block. The correct implementation ranks a full 1.0 at every step
		// even though the newest 1-4 blocks are always unflushed.
		eps := 0.005
		steps := 12
		scoredSteps := 0
		for step := 1; step <= steps; step += 1 {
			now := blockTime(step)

			for _, p := range providers {
				RecordClientReliabilityStatsRange(
					ctx,
					p.networkId,
					p.clientId,
					p.clientAddressHash,
					now,
					now,
					perfectStats(),
				)
			}

			if step%2 == 0 {
				RollupClientReliabilityStats(ctx, now)
			}

			UpdateClientReliabilityScores(ctx, now, true)

			lookbackClientScores := GetAllClientReliabilityScores(ctx)
			if len(lookbackClientScores) == 0 {
				// the first drain has not run yet
				continue
			}
			scoredSteps += 1
			for lookbackIndex, clientScores := range lookbackClientScores {
				for _, p := range providers {
					score, ok := clientScores[p.clientId]
					connect.AssertEqual(t, ok, true)
					if d := score.IndependentReliabilityWeight - 1.0; d < -eps || eps < d {
						t.Errorf(
							"step %d lookback %d client %s: independent reliability weight %f != 1.0 (unflushed tail counted as unreliability)",
							step,
							lookbackIndex,
							p.clientId,
							score.IndependentReliabilityWeight,
						)
					}
				}
			}
		}
		// the loop must actually have scored (no vacuous pass)
		connect.AssertEqual(t, true, 8 <= scoredSteps)

		// end to end through the strict FindProviders2 gate (no ForceMinimum):
		// every provider must pass the reliability minimums and be returned
		err := UpdateClientScores(ctx, 5*time.Second, 1)
		connect.AssertEqual(t, err, nil)

		res, err := FindProviders2(&FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					LocationGroupId: &locationGroup.LocationGroupId,
				},
			},
			Count: 2 * n,
		}, callerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), n)
	})
}

// A connect deploy rotates handlers: the providers on a restarted host cannot
// announce for a block or two, and the block in which they reconnect is
// invalid by the `connection_new_count = 0` rule. That is TWO lost blocks out
// of a 60-block hour — far below the 0.99 threshold FindProviders2 gates on —
// so without excusing platform-caused blocks, every provider on a rotated
// handler drops out of the market for a full hour after every deploy.
//
// The blocks a deploy takes out show up as a synchronized collapse in the
// per-block client count, which the drain records. Those blocks are excused
// for everyone, so providers that were up the whole time keep a full 1.0
// weight and stay in the market.
func TestFindProviders2ReliabilityDeployGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

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

		type testProvider struct {
			networkId         server.Id
			clientId          server.Id
			clientAddressHash [32]byte
		}

		// enough providers that the degraded-block median is meaningful
		n := 40
		providers := []*testProvider{}
		var callerSession *session.ClientSession

		for i := range n {
			networkId := server.NewId()
			clientSession := session.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(networkId, server.NewId(), fmt.Sprintf("network%d", i), false, false),
			)

			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

			handlerId := CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := ConnectNetworkClient(
				ctx,
				clientId,
				fmt.Sprintf("0.0.%d.%d:0", i/256, i%256),
				handlerId,
			)
			connect.AssertEqual(t, err, nil)

			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: make([]byte, 32),
			})
			err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)

			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
					connectionId, 30, 1,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
					connectionId, 100*1024*1024, 1,
				))
			})

			// measured healthy, so the reliability minimums under test are the
			// deciding filter and not the egress-health gate
			testing_setProviderEgressHealthy(ctx, clientId)

			clientAddressHash, _, err := clientSession.ClientAddressHashPort()
			connect.AssertEqual(t, err, nil)

			providers = append(providers, &testProvider{
				networkId:         networkId,
				clientId:          clientId,
				clientAddressHash: clientAddressHash,
			})
			callerSession = clientSession
		}

		perfectStats := func() *ClientReliabilityStats {
			return &ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			}
		}
		// what a handler rotation looks like in the block the client comes
		// back in: the announce path marks the first sync of the new
		// connection with ConnectionNewCount, and the next sync (~half a
		// block later) reports the re-established connection. One reconnect
		// in a block is tolerated (`client_reliability_valid`), so this block
		// stays valid — the client lost only the block it was away for.
		reconnectStats := func() *ClientReliabilityStats {
			return &ClientReliabilityStats{
				ConnectionNewCount:         1,
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			}
		}

		base := server.NowUtc()
		blockTime := func(step int) time.Time {
			return base.Add(time.Duration(step) * ReliabilityBlockDuration)
		}

		// perfect history over the whole max lookback
		maxLookback := ClientLookbacks[len(ClientLookbacks)-1]
		for _, p := range providers {
			AddClientReliabilityStatsRange(
				ctx,
				p.networkId,
				p.clientId,
				p.clientAddressHash,
				base.Add(-maxLookback-time.Hour),
				base,
				perfectStats(),
			)
		}
		// AddClientReliabilityStatsRange is the direct pg fixture path, so it
		// intentionally does not maintain the block-health table owned by the
		// redis drain. Seed the established healthy baseline that a running
		// production rollup has before exercising the deploy collapse. Without
		// it, the first deploy candidate has fewer than the minimum ten prior
		// observations and is deliberately left unclassified.
		baseBlockNumber := reliabilityBlockNumber(base)
		for blockNumber := baseBlockNumber - reliabilityDegradedMinBlockCount + 1; blockNumber <= baseBlockNumber; blockNumber += 1 {
			recordClientReliabilityBlockHealth(ctx, blockNumber)
		}
		RollupClientReliabilityStats(ctx, base)

		// a rolling deploy at step 6: the first half of the providers are on
		// the rotated handler. They go silent for that block and reconnect in
		// the next one (an invalid block for them).
		deployStep := 6
		steps := 12
		for step := 1; step <= steps; step += 1 {
			now := blockTime(step)

			for i, p := range providers {
				rotated := i < n/2
				stats := perfectStats()
				if rotated && step == deployStep {
					// silent: the handler is gone, nothing is announced
					continue
				}
				if rotated && step == deployStep+1 {
					stats = reconnectStats()
				}
				RecordClientReliabilityStatsRange(
					ctx,
					p.networkId,
					p.clientId,
					p.clientAddressHash,
					now,
					now,
					stats,
				)
			}

			RollupClientReliabilityStats(ctx, now)
		}

		scoreTime := blockTime(steps)
		UpdateClientReliabilityScores(ctx, scoreTime, true)

		// the deploy blocks are excused, so every provider — including the
		// ones that were rotated — keeps a full reliability weight
		eps := 0.005
		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		checkedCount := 0
		for lookbackIndex, clientScores := range lookbackClientScores {
			for i, p := range providers {
				score, ok := clientScores[p.clientId]
				connect.AssertEqual(t, ok, true)
				checkedCount += 1
				if d := score.IndependentReliabilityWeight - 1.0; d < -eps || eps < d {
					t.Errorf(
						"lookback %d client %d: independent reliability weight %f != 1.0 (a connect deploy was counted as client unreliability)",
						lookbackIndex,
						i,
						score.IndependentReliabilityWeight,
					)
				}
			}
		}
		// the weight assertions above must not pass vacuously: every provider
		// is scored in every lookback
		connect.AssertEqual(t, checkedCount, len(ClientLookbacks)*n)

		// and every provider still passes the strict FindProviders2 gate
		err := UpdateClientScores(ctx, 5*time.Second, 1)
		connect.AssertEqual(t, err, nil)

		res, err := FindProviders2(&FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					LocationGroupId: &locationGroup.LocationGroupId,
				},
			},
			Count: 2 * n,
		}, callerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(res.Providers), n)
	})
}

// fix(beta): UpdateClientLocations must count a client toward its location
// from network_client_location_reliability (connected + valid) alone, even
// when client_connection_reliability_score has no row for it at all -- that
// table is populated by a separate multi-stage rollup (raw events -> redis
// drain -> client_reliability_running -> reliability scores) that, at
// small/cold-start scale, can go indefinitely without producing a single
// row even though real, currently-connected/valid clients exist. Upstream
// used an INNER JOIN here, which silently produced an empty provider
// locations list in that scenario despite real data existing; this asserts
// the beta-only LEFT JOIN keeps counting it.
func TestUpdateClientLocationsCountsClientsWithoutReliabilityScores(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
		connect.AssertEqual(t, err, nil)

		err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		// only clients holding a Public provide key are counted (see
		// TestUpdateClientLocationsCountsOnlyPublicProviders); the
		// reliability-score join is what is under test here, so satisfy that
		// precondition explicitly
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: []byte("public-secret"),
		})

		// populates network_client_location_reliability straight from the
		// live connection tables -- independent of, and deliberately without
		// ever touching, the reliability-scoring pipeline below
		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		// confirm the bug scenario is actually set up: a real connected+valid
		// row exists, but no reliability score row does
		var connectedAndValid bool
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT connected AND valid FROM network_client_location_reliability WHERE client_id = $1`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&connectedAndValid))
				}
			})
		})
		connect.AssertEqual(t, connectedAndValid, true)

		var scoreCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM client_connection_reliability_score WHERE client_id = $1`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&scoreCount))
				}
			})
		})
		connect.AssertEqual(t, scoreCount, 0)

		// UpdateClientLocations counts only a provider a probe measured
		// healthy AND observed egressing from the country it claims (see
		// providerCountFilter); this fixture claims "us", so it must be
		// observed in "us" too, or the LEFT JOIN behavior under test is never
		// reached.
		testing_setProviderEgressHealthy(ctx, clientId)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: clientId, LocationId: city.LocationId,
			CountryCode: "us", Verdict: "verified", ObservedAt: server.NowUtc(),
		})

		err = UpdateClientLocations(ctx, time.Hour)
		connect.AssertEqual(t, err, nil)

		initialClientLocations, err := loadInitialClientLocations(ctx)
		connect.AssertEqual(t, err, nil)
		if initialClientLocations == nil {
			t.Fatal("expected a populated client locations cache, got nil")
		}

		// the top-level locations list is country-level entries only (city
		// and region roll up into a country entry's TopCityLocationIdCounts
		// / TopRegionLocationIdCounts, see UpdateClientLocations), so the
		// country this client's city belongs to is what must show up here.
		found := false
		for _, clientLocation := range initialClientLocations.Locations {
			if clientLocation.LocationId == city.CountryLocationId {
				found = true
			}
		}
		connect.AssertEqual(t, found, true)
	})
}

// fix(beta): GetProviderLocations has a second filter beyond
// UpdateClientLocations -- it only shows a location if loadLocationStables
// has an entry for it, which is populated by UpdateClientScores. That
// function had the identical INNER JOIN-against-client_connection_reliability_score
// pattern (twice), so fixing UpdateClientLocations alone was not sufficient:
// the locations list stayed empty because this second stage still silently
// dropped every location with no scored clients. This asserts the
// beta-only LEFT JOIN + COALESCE defaults keep an unscored client counted
// here too.
func TestUpdateClientScoresCountsClientsWithoutReliabilityScores(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
		connect.AssertEqual(t, err, nil)

		err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		// only clients holding a Public provide key are counted (see
		// TestUpdateClientLocationsCountsOnlyPublicProviders); the
		// reliability-score join is what is under test here, so satisfy that
		// precondition explicitly
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: []byte("public-secret"),
		})

		// good latency and speed tests so the quality score gate passes and
		// the reliability-score join is what's actually being tested here
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 30, 1,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 100*1024*1024, 1,
			))
		})

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		// confirm the bug scenario: no reliability score row for this client
		var scoreCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM client_connection_reliability_score WHERE client_id = $1`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&scoreCount))
				}
			})
		})
		connect.AssertEqual(t, scoreCount, 0)

		// measured healthy, so the missing reliability score under test is the
		// only thing that could exclude this provider
		testing_setProviderEgressHealthy(ctx, clientId)

		err = UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		locationStables, err := loadLocationStables(ctx, []server.Id{city.CountryLocationId}, false, RankModeQuality, server.Id{})
		connect.AssertEqual(t, err, nil)
		_, ok := locationStables[city.CountryLocationId]
		connect.AssertEqual(t, ok, true)
	})
}

// fix(beta): SetConnectionLocation must not panic when the resolved location
// has no city (country-only, as the free geo db returns for most
// datacenter/mobile/VPN IPs). Before the fix, the NULL city_location_id
// insert panicked inside server.Tx, and that panic propagated out of the
// connection announce goroutine and tore down the whole connection -- the
// direct cause of country-only clients (including the app) being unable to
// hold a connect connection. This asserts a country-only location is stored
// (falling back to country granularity) with no panic and no error.
func TestSetConnectionLocationToleratesCountryOnlyLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// country-only location -- its row has NULL city_location_id and
		// NULL region_location_id, exactly what crashed the insert before
		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
		connect.AssertEqual(t, err, nil)

		// this call panicked before the fix; now it must succeed and store
		// the connection at country granularity
		err = SetConnectionLocation(ctx, connectionId, country.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		var city, region, cty *server.Id
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT city_location_id, region_location_id, country_location_id FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&city, &region, &cty))
				}
			})
		})
		if city == nil || region == nil || cty == nil {
			t.Fatal("expected all three location ids to be set (falling back to country), got a nil")
		}
		// city and region fall back to the country id for a country-only location
		connect.AssertEqual(t, *city, country.CountryLocationId)
		connect.AssertEqual(t, *region, country.CountryLocationId)
		connect.AssertEqual(t, *cty, country.CountryLocationId)
	})
}

func TestUpdateClientLocationsCountsOnlyPublicProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		handlerId := CreateNetworkClientHandler(ctx)

		// connect a client and give it the provide modes supplied
		connectOne := func(modes map[ProvideMode][]byte) server.Id {
			networkId := server.NewId()
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)
			if modes != nil {
				SetProvide(ctx, clientId, modes)
			}
			return clientId
		}

		// serves strangers -- must be counted
		publicClientId := connectOne(map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})
		// own network only -- must NOT be counted, it cannot accept a
		// contract from a user outside its network
		connectOne(map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-secret"),
		})
		// no provide key at all -- must NOT be counted
		connectOne(nil)

		// UpdateClientLocations counts only a provider a probe measured
		// healthy AND observed egressing from the country it claims (see
		// providerCountFilter). Only the Public-key client needs this -- the
		// other two are excluded upstream by the provide-mode filter under
		// test, before the count gate is ever reached.
		testing_setProviderEgressHealthy(ctx, publicClientId)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: publicClientId, LocationId: city.LocationId,
			CountryCode: "us", Verdict: "verified", ObservedAt: server.NowUtc(),
		})

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		err := UpdateClientLocations(ctx, time.Hour)
		connect.AssertEqual(t, err, nil)

		initialClientLocations, err := loadInitialClientLocations(ctx)
		connect.AssertEqual(t, err, nil)
		if initialClientLocations == nil {
			t.Fatal("expected a populated client locations cache, got nil")
		}

		found := false
		for _, clientLocation := range initialClientLocations.Locations {
			if clientLocation.LocationId == city.CountryLocationId {
				found = true
				// exactly one of the three connected clients holds a Public
				// key; counting the other two advertises supply no user can
				// reach (39 advertised vs 2 reachable, observed on beta)
				connect.AssertEqual(t, clientLocation.ClientCount, 1)
			}
		}
		connect.AssertEqual(t, found, true)
	})
}

// `UpdateClientScores` writes two things, and the provide-mode rule differs
// between them.
//
// The `ClientFilter` -- read by `loadLocationStables`, which
// `GetProviderLocations` gates the public `Stable` flag on -- counts only
// providers a stranger can reach, the same rule as the provider count in
// `UpdateClientLocations`. A location whose only supply is network-only is not
// stable and reports no providers at all.
//
// The sampled candidate pool is wider: it also carries `ProvideModeNetwork`
// providers, which are real usable supply for sources in their own network.
// `FindProviders2` filters those per request (see
// TestFindProviders2NetworkOnlyProviderVisibleOnlyToItsOwnNetwork); the pool
// itself is shared by every caller and cannot make that decision.
//
// Two connected+valid providers with identical latency/speed data in two
// different countries, differing ONLY in whether they hold a Public provide
// key.
func TestUpdateClientScoresCountsOnlyPublicProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		publicCity := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, publicCity)

		networkOnlyCity := &Location{
			LocationType: LocationTypeCity,
			City:         "Toronto",
			Region:       "Ontario",
			Country:      "Canada",
			CountryCode:  "ca",
		}
		CreateLocation(ctx, networkOnlyCity)

		publicClientId, networkOnlyClientId := connectPublicAndNetworkOnlyProviders(
			ctx,
			t,
			publicCity,
			networkOnlyCity,
		)

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		locationStables, err := loadLocationStables(
			ctx,
			[]server.Id{publicCity.CountryLocationId, networkOnlyCity.CountryLocationId},
			false,
			RankModeQuality,
			server.Id{},
		)
		connect.AssertEqual(t, err, nil)

		// the Public provider's country has a provider a stranger can use
		_, ok := locationStables[publicCity.CountryLocationId]
		connect.AssertEqual(t, ok, true)
		// the network-only provider's country has none. a missing entry is how
		// loadLocationStables says "no providers", so it must be absent
		_, ok = locationStables[networkOnlyCity.CountryLocationId]
		connect.AssertEqual(t, ok, false)

		// the pool FindProviders2 consumes carries both, each tagged with
		// whether a caller outside its network may use it
		clientScores, err := loadClientScores(
			true,
			RankModeQuality,
			ctx,
			map[server.Id]bool{
				publicCity.LocationId:      true,
				networkOnlyCity.LocationId: true,
			},
			map[server.Id]bool{},
			server.Id{},
			100,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientScores), 2)
		publicClientScore, ok := clientScores[publicClientId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, publicClientScore.NetworkOnly, false)
		networkOnlyClientScore, ok := clientScores[networkOnlyClientId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, networkOnlyClientScore.NetworkOnly, true)
	})
}

// the location *group* source query in `UpdateClientScores` follows the same
// Public-or-Network rule as the per-location one. It fills
// `locationGroupClientScores` -> the `clientScoreLocationGroup*` redis keys ->
// `loadClientScores` -> `FindProviders2` whenever the spec carries a
// `LocationGroupId`, so a user who selects a promoted group (e.g. "Strong
// Privacy Laws") has to be filtered by the same request-time network check as
// a user who selects a plain location -- and the group cache has to carry the
// flag that check reads.
//
// Dropping either `provide_mode` from the group query's `EXISTS` breaks this:
// without Network the group's own-network supply disappears, and without
// Public nothing is reachable at all. Dropping the `EXISTS` entirely would let
// in modes `CreateContract` can never accept.
//
// Same two providers, differing only in the Public provide key, each in its
// own location group.
func TestUpdateClientScoresGroupCarriesNetworkProvidersTagged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		publicCity := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, publicCity)

		networkOnlyCity := &Location{
			LocationType: LocationTypeCity,
			City:         "Toronto",
			Region:       "Ontario",
			Country:      "Canada",
			CountryCode:  "ca",
		}
		CreateLocation(ctx, networkOnlyCity)

		publicGroup := &LocationGroup{
			Name:     "Test Group Public",
			Promoted: true,
			MemberLocationIds: []server.Id{
				publicCity.CityLocationId,
				publicCity.RegionLocationId,
				publicCity.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, publicGroup)

		networkOnlyGroup := &LocationGroup{
			Name:     "Test Group Network Only",
			Promoted: true,
			MemberLocationIds: []server.Id{
				networkOnlyCity.CityLocationId,
				networkOnlyCity.RegionLocationId,
				networkOnlyCity.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, networkOnlyGroup)

		publicClientId, networkOnlyClientId := connectPublicAndNetworkOnlyProviders(
			ctx,
			t,
			publicCity,
			networkOnlyCity,
		)

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		// read the group cache only -- no location ids -- so this asserts on
		// the group query alone
		clientScores, err := loadClientScores(
			true,
			RankModeQuality,
			ctx,
			map[server.Id]bool{},
			map[server.Id]bool{
				publicGroup.LocationGroupId:      true,
				networkOnlyGroup.LocationGroupId: true,
			},
			server.Id{},
			100,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientScores), 2)
		publicClientScore, ok := clientScores[publicClientId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, publicClientScore.NetworkOnly, false)
		networkOnlyClientScore, ok := clientScores[networkOnlyClientId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, networkOnlyClientScore.NetworkOnly, true)

		// the network-only group on its own exports exactly its one provider,
		// tagged network-only so FindProviders2 hands it to that provider's
		// own network and nobody else
		networkOnlyGroupClientScores, err := loadClientScores(
			true,
			RankModeQuality,
			ctx,
			map[server.Id]bool{},
			map[server.Id]bool{networkOnlyGroup.LocationGroupId: true},
			server.Id{},
			100,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(networkOnlyGroupClientScores), 1)
		networkOnlyClientScore, ok = networkOnlyGroupClientScores[networkOnlyClientId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, networkOnlyClientScore.NetworkOnly, true)
	})
}

// connects one provider per location that is identical in every respect the
// scoring pipeline looks at -- connected, valid, same latency and speed
// samples so both clear the strict minimums -- except that the first holds a
// Public provide key and the second holds only a Network one. Anything that
// separates the two downstream is the provide-mode filter and nothing else.
func connectPublicAndNetworkOnlyProviders(
	ctx context.Context,
	t testing.TB,
	publicLocation *Location,
	networkOnlyLocation *Location,
) (publicClientId server.Id, networkOnlyClientId server.Id) {
	handlerId := CreateNetworkClientHandler(ctx)

	connectProvider := func(location *Location, modes map[ProvideMode][]byte) server.Id {
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
		connect.AssertEqual(t, err, nil)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)
		SetProvide(ctx, clientId, modes)

		// good latency and speed samples so the client passes the strict
		// minimums; loadLocationStables reads the force-minimum-false filter
		// key, which is empty for a client that fails them
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 30, 1,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 100*1024*1024, 1,
			))
		})

		// measured healthy, so the egress-health gate is not what separates
		// these two providers -- the provide mode under test is
		testing_setProviderEgressHealthy(ctx, clientId)

		return clientId
	}

	// serves strangers
	publicClientId = connectProvider(publicLocation, map[ProvideMode][]byte{
		ProvideModePublic:  []byte("public-secret"),
		ProvideModeNetwork: []byte("network-secret"),
	})
	// own network only -- cannot accept a contract from a user outside it
	networkOnlyClientId = connectProvider(networkOnlyLocation, map[ProvideMode][]byte{
		ProvideModeNetwork: []byte("network-secret"),
	})
	return
}

// A client whose geo lookup resolved no city and no region is stored with
// city_location_id = region_location_id = country_location_id -- the coarsest
// granularity available is written into all three columns. Both fan-out loops
// then walked city, region and country unconditionally, so one such client
// added 3 to its own country: `/network/provider-locations` advertised three
// times the supply that exists, and the inflation is worst exactly where geo
// resolution is coarsest (datacenter, mobile and VPN egress).
//
// That inflation is real on beta, which has the coarsest-granularity fallback
// in `SetConnectionLocation`. It is NOT reachable on `upstream/main`, which has
// no country-only fallback and raises instead, so against main this is a
// forward guard; the fallback arrives with PR #407. The fixture builds the
// collapsed row with a direct UPDATE for exactly that reason -- it does not
// depend on which branch's write path could produce it.
//
// The second half of this test is the guard on the fix: a genuinely
// city-granular client must still roll up into its region and its country. A
// dedupe keyed on the client alone rather than on (client, location) would
// silently stop city clients counting toward their country, which is a far
// worse regression than the one being fixed.
func TestUpdateClientLocationsCountsEachClientOncePerLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		countryOnlyCity, city, _, _ := createCountryOnlyAndCityProviders(ctx, t)

		err := UpdateClientLocations(ctx, time.Hour)
		connect.AssertEqual(t, err, nil)

		clientLocations, err := loadClientLocations(ctx, map[server.Id]bool{
			countryOnlyCity.CountryLocationId: true,
			city.CityLocationId:               true,
			city.RegionLocationId:             true,
			city.CountryLocationId:            true,
		})
		connect.AssertEqual(t, err, nil)

		countOf := func(locationId server.Id) int {
			clientLocation, ok := clientLocations[locationId]
			if !ok {
				t.Fatalf("expected a cached client location for %s", locationId)
			}
			return clientLocation.ClientCount
		}

		// one client, counted once -- not once per city/region/country column
		connect.AssertEqual(t, countOf(countryOnlyCity.CountryLocationId), 1)

		// and the roll-up for a real city client is untouched
		connect.AssertEqual(t, countOf(city.CityLocationId), 1)
		connect.AssertEqual(t, countOf(city.RegionLocationId), 1)
		connect.AssertEqual(t, countOf(city.CountryLocationId), 1)
	})
}

// The `UpdateClientScores` half of the fan-out. This deliberately does NOT
// assert "counted once": the per-location and per-group accumulators are maps
// keyed by client id, so a repeated client is absorbed whether or not the loop
// dedupes, and an earlier version of this test asserting a pool size of 1 for
// the country-only provider returned 1 both before and after the dedupe. It
// could not fail, so it was not testing anything.
//
// What the code here CAN get wrong is the shape of the dedupe. Keying it on the
// client rather than on (client, location) -- the tempting simplification, since
// the map already absorbs repeats -- would stop a city-granular provider
// appearing in its region's and its country's pools, silently emptying every
// coarse-grained search. So the assertions are on membership at each
// granularity, by client id rather than by count, which also catches a provider
// leaking into another country's pool.
func TestUpdateClientScoresRollsCityProviderUpToRegionAndCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		countryOnlyCity, city, countryOnlyClientId, cityClientId := createCountryOnlyAndCityProviders(ctx, t)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		poolAt := func(locationId server.Id) map[server.Id]*ClientScore {
			clientScores, err := loadClientScores(
				true,
				RankModeQuality,
				ctx,
				map[server.Id]bool{locationId: true},
				map[server.Id]bool{},
				server.Id{},
				100,
			)
			connect.AssertEqual(t, err, nil)
			return clientScores
		}
		assertPoolIs := func(label string, locationId server.Id, wantClientId server.Id) {
			clientScores := poolAt(locationId)
			if _, ok := clientScores[wantClientId]; !ok {
				t.Errorf("%s pool is missing provider %s", label, wantClientId)
			}
			if len(clientScores) != 1 {
				t.Errorf("%s pool holds %d clients, want only %s", label, len(clientScores), wantClientId)
			}
		}

		// the city provider must reach every granularity above it
		assertPoolIs("city", city.CityLocationId, cityClientId)
		assertPoolIs("region", city.RegionLocationId, cityClientId)
		assertPoolIs("country", city.CountryLocationId, cityClientId)

		// and the country-only provider stays in its own country, not the
		// city provider's
		assertPoolIs("country-only country", countryOnlyCity.CountryLocationId, countryOnlyClientId)
	})
}

// connects two Public providers in two different countries and rolls the
// reliability table forward:
//
//   - one whose `network_client_location` row has all three location columns
//     collapsed to its country id, which is what a country-granularity geo
//     lookup produces (`SetConnectionLocation` writes the coarsest available id
//     into the NOT NULL city/region columns), and
//   - one at genuine city granularity, with three distinct ids.
//
// The collapse is applied with a direct UPDATE rather than by handing
// `SetConnectionLocation` a country-only `location` row, so the fixture depends
// only on the shape of the stored row and not on which branch's fallback
// produced it.
func createCountryOnlyAndCityProviders(ctx context.Context, t testing.TB) (
	countryOnlyCity *Location,
	city *Location,
	countryOnlyClientId server.Id,
	cityClientId server.Id,
) {
	countryOnlyCity = &Location{
		LocationType: LocationTypeCity,
		City:         "Toronto",
		Region:       "Ontario",
		Country:      "Canada",
		CountryCode:  "ca",
	}
	CreateLocation(ctx, countryOnlyCity)

	city = &Location{
		LocationType: LocationTypeCity,
		City:         "Palo Alto",
		Region:       "California",
		Country:      "United States",
		CountryCode:  "us",
	}
	CreateLocation(ctx, city)

	handlerId := CreateNetworkClientHandler(ctx)
	connectPublicProvider := func(location *Location, ip string) server.Id {
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, ip, handlerId)
		connect.AssertEqual(t, err, nil)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: []byte("public-secret"),
		})

		// UpdateClientScores joins client_connection_reliability_score, so
		// every provider needs a score row or the join, not the logic under
		// test, is what excludes it. The address hash is shared across the
		// fixture on purpose: validity is derived from
		// network_client_connection, not from these stats.
		AddClientReliabilityStats(
			ctx,
			networkId,
			clientId,
			[32]byte{},
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

		// measured healthy, so the egress-health gate is not what excludes a
		// provider here -- the location roll-up under test is
		testing_setProviderEgressHealthy(ctx, clientId)

		// UpdateClientLocations also requires a probe-observed egress country
		// matching the CLAIMED one (see providerCountFilter). The two
		// providers this helper builds claim different countries -- observe
		// each in its own (location.CountryCode), not a shared one, or the
		// country-only provider (claims "ca") would wrongly fail closed
		// against an "us" observation.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: clientId, LocationId: location.LocationId,
			CountryCode: location.CountryCode, Verdict: "verified", ObservedAt: server.NowUtc(),
		})

		return clientId
	}

	countryOnlyClientId = connectPublicProvider(countryOnlyCity, "0.0.0.1:0")
	cityClientId = connectPublicProvider(city, "0.0.0.2:0")

	// collapse the first provider to country granularity
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_client_location
			SET
				city_location_id = $2,
				region_location_id = $2
			WHERE client_id = $1
			`,
			countryOnlyClientId,
			countryOnlyCity.CountryLocationId,
		))
	})

	UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
	UpdateClientReliabilityScores(ctx, server.NowUtc(), true)
	return
}

// `UpdateClientLocations` counts only providers holding a Public provide key,
// because that count is shown to everyone and a stranger genuinely cannot use a
// `Network`-only provider. The candidate pool is a different question: a
// `Network`-only provider serves sources inside its own network today (via
// `CreateContractNoEscrow`) and is discoverable to them. Restricting the pool
// to Public would make those providers invisible to the very users they exist
// for -- a live regression for anyone running providers for their own
// organisation.
//
// So the pool carries both, and eligibility is decided per request against the
// caller's network. All four cases:
//
//	                       same-network caller   other-network caller
//	Public provider        returned              returned
//	Network-only provider  returned              NOT returned
//
// This has to be a request-time filter, not a cache-time one: the client-score
// redis entries are keyed by (forceMinimum, rankMode, locationId,
// callerLocationId) and are not network-scoped, so one cached set is shared by
// callers from every network.
func TestFindProviders2NetworkOnlyProviderVisibleOnlyToItsOwnNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		handlerId := CreateNetworkClientHandler(ctx)

		connectProvider := func(networkId server.Id, ip string, modes map[ProvideMode][]byte) server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, ip, handlerId)
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)
			SetProvide(ctx, clientId, modes)

			// upstream joins client_connection_reliability_score with an INNER
			// JOIN, so a provider with no reliability row never reaches the
			// pool at all. Give every provider one so this test asserts the
			// provide-mode rule and nothing else.
			AddClientReliabilityStats(
				ctx,
				networkId,
				clientId,
				[32]byte{},
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

			// measured healthy, so the caller's network -- not the
			// egress-health gate -- is what decides visibility here
			testing_setProviderEgressHealthy(ctx, clientId)

			return clientId
		}

		publicNetworkId := server.NewId()
		publicClientId := connectProvider(publicNetworkId, "0.0.0.1:0", map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})

		networkOnlyNetworkId := server.NewId()
		networkOnlyClientId := connectProvider(networkOnlyNetworkId, "0.0.0.2:0", map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-secret"),
		})
		// The low-rate external observer found this provider healthy overall but
		// rejected by Bloomberg. Selection must publish both independent facts:
		// same-network eligibility and the domain-specific reputation result.
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:              networkOnlyClientId,
			MeasuredAt:            server.NowUtc(),
			OKCount:               131,
			Total:                 131,
			ReputationOK:          2,
			ReputationTotal:       3,
			ReputationFailedNames: "bloomberg",
		})

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
		UpdateClientReliabilityScores(ctx, server.NowUtc(), true)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		findFrom := func(networkId server.Id, name string) map[server.Id]*FindProvidersProvider {
			clientSession := session.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(networkId, server.NewId(), name, false, false),
			)
			res, err := FindProviders2(
				&FindProviders2Args{
					Specs: []*ProviderSpec{
						{LocationId: &city.LocationId},
					},
					Count:        100,
					ForceMinimum: true,
				},
				clientSession,
			)
			connect.AssertEqual(t, err, nil)
			found := map[server.Id]*FindProvidersProvider{}
			for _, provider := range res.Providers {
				found[provider.ClientId] = provider
			}
			return found
		}

		// the network-only provider's own network sees both
		sameNetwork := findFrom(networkOnlyNetworkId, "same-network")
		connect.AssertEqual(t, sameNetwork[publicClientId] != nil, true)
		networkProvider := sameNetwork[networkOnlyClientId]
		connect.AssertEqual(t, networkProvider != nil, true)
		connect.AssertEqual(t, networkProvider.NetworkOnly, true)
		connect.AssertEqual(t, networkProvider.ReputationFailedNames, "bloomberg")

		// a stranger sees only the Public one. handing them the network-only
		// provider would produce a `CreateContract` NoPermission rejection.
		otherNetwork := findFrom(server.NewId(), "other-network")
		connect.AssertEqual(t, otherNetwork[publicClientId] != nil, true)
		connect.AssertEqual(t, otherNetwork[networkOnlyClientId] != nil, false)
	})
}

// the prober enumerates locations through `GET /network/provider-locations` ->
// `GetProviderLocations` -> `loadLocationStables`, and only then asks
// `find-providers2` for the providers at each one. `force_minimum` on that
// second call cannot recover a location the first call never listed, and the
// read side hardcoded the `forceMinimum=false` filter key -- so a location
// whose every provider fails the minimums gate was invisible, and those
// providers could never be probed and so could never graduate probation.
//
// One connected+valid Public provider with no latency or speed samples, which
// deterministically fails the strict minimums. The writer populates both key
// families, so the same location must be absent under `forceMinimum=false`
// (today's user-facing behaviour, unchanged) and present under `true`.
func TestLoadLocationStablesHonoursForceMinimum(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, userId, "a", false, false),
		)

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
		connect.AssertEqual(t, err, nil)

		err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		// a provider a stranger can actually use, so the provide-mode filter is
		// not what excludes it
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})

		// a lookback_index = 0 reliability score row, so the source query's join
		// against `client_connection_reliability_score` is not what excludes it
		// either. deliberately no `network_client_latency` / `network_client_speed`
		// rows: the missing latency and speed tests are the only reason this
		// provider fails the minimums.
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		connect.AssertEqual(t, err, nil)
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
		UpdateClientReliabilityScores(ctx, server.NowUtc(), true)

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		err = UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		locationIds := []server.Id{city.CountryLocationId}

		// user-facing listing: the provider is below the bar, so the location
		// has no entry at all -- a missing entry is how loadLocationStables says
		// "no providers"
		strict, err := loadLocationStables(ctx, locationIds, false, RankModeQuality, server.Id{})
		connect.AssertEqual(t, err, nil)
		_, ok := strict[city.CountryLocationId]
		connect.AssertEqual(t, ok, false)

		// operator census: the same location must be listed, otherwise its
		// providers can never be reached
		forced, err := loadLocationStables(ctx, locationIds, true, RankModeQuality, server.Id{})
		connect.AssertEqual(t, err, nil)
		_, ok = forced[city.CountryLocationId]
		connect.AssertEqual(t, ok, true)
	})
}

// The pool predicate in `UpdateClientScores` is the whole reason this work
// exists, and until now nothing failed when it was deleted.
//
// Both source queries carry `EXISTS (... provide_mode IN (Public, Network))`.
// Deleting either one refills the candidate pool with the entire consumer
// fleet, because **every** client registers `ProvideMode_Stream` on connect
// (`connect/transfer_contract_manager.go`) whether or not it provides anything.
// Those clients cannot settle a contract -- `resolveNonCompanionProvideMode`
// can resolve a Stream-only destination as a companion, but that dead-ends at
// `CreateCompanionTransferEscrow`, which needs a pre-existing reverse origin
// contract and so can never bootstrap a session. That is the
// 39-providers-advertised-against-2-usable failure this filter fixes.
//
// The existing provide-mode tests do not catch a deletion: their fixtures are
// Public and Network-only, both of which the predicate admits, so removing it
// changes nothing they look at. These build the populations that would flood
// back in -- a Stream-only client and a keyless one -- and assert the pool
// still holds exactly the provider that can serve a contract.
//
// connectProvidersOfEveryProvideMode puts four providers in one location:
// Public, Network-only, Stream-only, and keyless. The first two belong in the
// pool (Network-only is filtered per request by `FindProviders2`); the last two
// never do.
func connectProvidersOfEveryProvideMode(ctx context.Context, t testing.TB, location *Location) (
	publicClientId server.Id,
	networkOnlyClientId server.Id,
	streamOnlyClientId server.Id,
	keylessClientId server.Id,
) {
	handlerId := CreateNetworkClientHandler(ctx)

	connectProvider := func(i int, modes map[ProvideMode][]byte) server.Id {
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(
			ctx,
			clientId,
			fmt.Sprintf("0.0.0.%d:0", i),
			handlerId,
		)
		connect.AssertEqual(t, err, nil)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)
		if modes != nil {
			SetProvide(ctx, clientId, modes)
		}

		// a reliability score row for every client. The per-location query
		// joins client_connection_reliability_score, so without one the join --
		// not the provide-mode predicate under test -- is what decides who is
		// in the pool.
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

		// identical latency and speed samples for every client, so all four
		// clear the strict minimums and the provide mode is the only thing
		// that can separate them
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 30, 1,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 100*1024*1024, 1,
			))
		})

		// measured healthy for every provider, so the egress-health gate is not
		// what separates them -- the provide mode under test is
		testing_setProviderEgressHealthy(ctx, clientId)

		return clientId
	}

	publicClientId = connectProvider(1, map[ProvideMode][]byte{
		ProvideModePublic:  []byte("public-secret"),
		ProvideModeNetwork: []byte("network-secret"),
	})
	networkOnlyClientId = connectProvider(2, map[ProvideMode][]byte{
		ProvideModeNetwork: []byte("network-secret"),
	})
	// the population the deleted predicate would readmit: every consumer
	// client registers Stream on connect, and Stream alone can never settle a
	// contract
	streamOnlyClientId = connectProvider(3, map[ProvideMode][]byte{
		ProvideModeStream: []byte("stream-secret"),
	})
	keylessClientId = connectProvider(4, nil)

	UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
	UpdateClientReliabilityScores(ctx, server.NowUtc(), true)
	return
}

// The per-location source query. Fails against a deletion of its
// `WHERE EXISTS`, which is what leaves the pool full of Stream-only consumers.
func TestUpdateClientScoresPoolExcludesStreamOnlyAndKeylessProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		publicClientId, networkOnlyClientId, streamOnlyClientId, keylessClientId :=
			connectProvidersOfEveryProvideMode(ctx, t, city)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores, err := loadClientScores(
			true,
			RankModeQuality,
			ctx,
			map[server.Id]bool{city.LocationId: true},
			map[server.Id]bool{},
			server.Id{},
			100,
		)
		connect.AssertEqual(t, err, nil)

		assertPoolAdmitsOnlyContractableProviders(
			t,
			clientScores,
			publicClientId,
			networkOnlyClientId,
			streamOnlyClientId,
			keylessClientId,
		)
	})
}

// Derived window clients and inactive top-level clients can hold Network
// provide keys, but neither is provider supply. Publishing either one feeds a
// consumer's own short-lived connection identities back into provider
// selection and amplifies replacement churn.
func TestUpdateClientScoresExcludesDerivedAndInactiveClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(
			providerConfigResourceName,
			[]byte("enable_egress_test: false\n"),
		))

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Provider City",
			Region:       "Provider Region",
			Country:      "Provider Country",
			CountryCode:  "pc",
		}
		CreateLocation(ctx, city)
		group := &LocationGroup{
			Name:              "Top-level Providers",
			Promoted:          true,
			MemberLocationIds: []server.Id{city.LocationId},
		}
		CreateLocationGroup(ctx, group)
		handlerId := CreateNetworkClientHandler(ctx)

		connectCandidate := func(clientId server.Id, clientAddress string) {
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)
			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic:  []byte("public-secret"),
				ProvideModeNetwork: []byte("network-secret"),
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, 30, 1)`,
					connectionId,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, 1)`,
					connectionId,
					100*1024*1024,
				))
			})
		}

		activeNetworkId := server.NewId()
		activeClientId := server.NewId()
		Testing_CreateDevice(ctx, activeNetworkId, server.NewId(), activeClientId, "", "")
		connectCandidate(activeClientId, "10.40.0.1:20000")

		childNetworkId := server.NewId()
		childDeviceId := server.NewId()
		parentClientId := server.NewId()
		childClientId := server.NewId()
		Testing_CreateDevice(ctx, childNetworkId, childDeviceId, parentClientId, "", "")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client (
					client_id,
					network_id,
					device_id,
					description,
					create_time,
					auth_time,
					source_client_id
				)
				VALUES ($1, $2, $3, '', now(), now(), $4)
				`,
				childClientId,
				childNetworkId,
				childDeviceId,
				parentClientId,
			))
		})
		connectCandidate(childClientId, "10.40.0.2:20000")

		inactiveNetworkId := server.NewId()
		inactiveClientId := server.NewId()
		Testing_CreateDevice(ctx, inactiveNetworkId, server.NewId(), inactiveClientId, "", "")
		connectCandidate(inactiveClientId, "10.40.0.3:20000")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET active = false WHERE client_id = $1`,
				inactiveClientId,
			))
		})

		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		for _, source := range []struct {
			name             string
			locationIds      map[server.Id]bool
			locationGroupIds map[server.Id]bool
		}{
			{name: "location", locationIds: map[server.Id]bool{city.LocationId: true}, locationGroupIds: map[server.Id]bool{}},
			{name: "location group", locationIds: map[server.Id]bool{}, locationGroupIds: map[server.Id]bool{group.LocationGroupId: true}},
		} {
			clientScores, err := loadClientScores(
				true,
				RankModeQuality,
				ctx,
				source.locationIds,
				source.locationGroupIds,
				server.Id{},
				100,
			)
			connect.AssertEqual(t, err, nil)
			if _, ok := clientScores[activeClientId]; !ok {
				t.Errorf("%s cache is missing active top-level provider %s", source.name, activeClientId)
			}
			for label, clientId := range map[string]server.Id{
				"derived":                    childClientId,
				"inactive":                   inactiveClientId,
				"parent without provide key": parentClientId,
			} {
				if _, ok := clientScores[clientId]; ok {
					t.Errorf("%s cache published %s client %s as provider supply", source.name, label, clientId)
				}
			}
		}

		err = UpdateClientLocations(ctx, time.Hour)
		connect.AssertEqual(t, err, nil)
		clientLocations, err := loadClientLocations(
			ctx,
			map[server.Id]bool{city.CountryLocationId: true},
		)
		connect.AssertEqual(t, err, nil)
		clientLocation, ok := clientLocations[city.CountryLocationId]
		if !ok {
			t.Fatalf("public location %s was not published", city.CountryLocationId)
		}
		connect.AssertEqual(t, clientLocation.ClientCount, 1)
	})
}

// The location-*group* source query, which is a separate statement with its own
// copy of the predicate and its own redis keys
// (`clientScoreLocationGroup*` -> `loadClientScores` -> `FindProviders2`
// whenever the spec carries a `LocationGroupId`). A user who picks a promoted
// group must get the same pool a user who picks a plain location gets; deleting
// this query's `WHERE EXISTS` alone leaves the per-location test above green.
//
// Only group ids are passed to `loadClientScores`, so this reads the group
// cache and nothing else.
func TestUpdateClientScoresGroupPoolExcludesStreamOnlyAndKeylessProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		group := &LocationGroup{
			Name:     "Test Group Provide Modes",
			Promoted: true,
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}
		CreateLocationGroup(ctx, group)

		publicClientId, networkOnlyClientId, streamOnlyClientId, keylessClientId :=
			connectProvidersOfEveryProvideMode(ctx, t, city)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores, err := loadClientScores(
			true,
			RankModeQuality,
			ctx,
			map[server.Id]bool{},
			map[server.Id]bool{group.LocationGroupId: true},
			server.Id{},
			100,
		)
		connect.AssertEqual(t, err, nil)

		assertPoolAdmitsOnlyContractableProviders(
			t,
			clientScores,
			publicClientId,
			networkOnlyClientId,
			streamOnlyClientId,
			keylessClientId,
		)
	})
}

// Errorf rather than Fatalf throughout, so one run reports every provider the
// pool got wrong instead of stopping at the first.
func assertPoolAdmitsOnlyContractableProviders(
	t testing.TB,
	clientScores map[server.Id]*ClientScore,
	publicClientId server.Id,
	networkOnlyClientId server.Id,
	streamOnlyClientId server.Id,
	keylessClientId server.Id,
) {
	if _, ok := clientScores[streamOnlyClientId]; ok {
		t.Errorf(
			"Stream-only client %s is in the candidate pool; every consumer client registers ProvideMode_Stream, so this is the whole consumer fleet advertised as supply",
			streamOnlyClientId,
		)
	}
	if _, ok := clientScores[keylessClientId]; ok {
		t.Errorf("client %s holds no provide key at all and is in the candidate pool", keylessClientId)
	}

	// and the providers that CAN settle a contract are still there -- a filter
	// that excluded them would "pass" the assertions above for the wrong reason
	publicClientScore, ok := clientScores[publicClientId]
	if !ok {
		t.Errorf("Public client %s is missing from the candidate pool", publicClientId)
	} else if publicClientScore.NetworkOnly {
		t.Errorf("Public client %s is tagged network-only", publicClientId)
	}
	networkOnlyClientScore, ok := clientScores[networkOnlyClientId]
	if !ok {
		t.Errorf("Network-only client %s is missing from the candidate pool; it serves its own network today", networkOnlyClientId)
	} else if !networkOnlyClientScore.NetworkOnly {
		t.Errorf("Network-only client %s is not tagged network-only, so FindProviders2 would hand it to strangers", networkOnlyClientId)
	}

	if len(clientScores) != 2 {
		t.Errorf("candidate pool holds %d clients, want exactly the 2 that can settle a contract", len(clientScores))
	}
}

// ---------------------------------------------------------------------------
// The egress-health gate on the public provider list.
//
// UpdateClientScores is the single writer of the precomputed score/filter sets
// that both GET /network/provider-locations and POST /network/find-providers2
// read, so a gate applied where PassesMinimums is set covers both surfaces.
// Until it existed the system only MEASURED health and nothing acted on it: a
// proxy answering 0 of 131 destinations still passed, because the pre-existing
// minimums are reliability weight and score, and a blackholed proxy stays
// perfectly *connected*.
// ---------------------------------------------------------------------------

// testing_setProviderEgressHealth records one egress-health measurement.
func testing_setProviderEgressHealth(ctx context.Context, clientId server.Id, okCount int, total int) {
	SetProviderEgressHealth(ctx, &ProviderEgressHealth{
		ClientId:   clientId,
		MeasuredAt: server.NowUtc(),
		OKCount:    okCount,
		Total:      total,
	})
}

// testing_setProviderEgressHealthy marks providers measured healthy at the rate
// the real healthy fleet measures. Every test that expects a provider to be
// SELECTABLE has to call this: a provider with no health record at all is
// excluded by design (fail closed -- out until you pass), so without it the
// fixture is testing exclusion rather than whatever it meant to test.
func testing_setProviderEgressHealthy(ctx context.Context, clientIds ...server.Id) {
	for _, clientId := range clientIds {
		testing_setProviderEgressHealth(ctx, clientId, 131, 131)
	}
}

func testing_enableProviderEgressTest(t testing.TB) {
	t.Helper()
	t.Cleanup(server.Config.PushSimpleResource(
		providerConfigResourceName,
		[]byte("enable_egress_test: true\n"),
	))
}

func TestProviderEgressTestEnabledConfiguration(t *testing.T) {
	if providerEgressTestEnabledFromResource(nil, fmt.Errorf("missing provider config")) {
		t.Fatal("a missing provider.yml enabled the egress-test gate")
	}

	tests := []struct {
		name    string
		config  string
		enabled bool
	}{
		{name: "missing key", config: "unrelated: true\n", enabled: false},
		{name: "explicit false", config: "enable_egress_test: false\n", enabled: false},
		{name: "explicit true", config: "enable_egress_test: true\n", enabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pop := server.Config.PushSimpleResource(
				providerConfigResourceName,
				[]byte(test.config),
			)
			defer pop()
			assert.Equal(t, providerEgressTestEnabled(), test.enabled)
		})
	}
}

// testing_connectQualifyingProviders connects n providers into one location,
// each of which clears every minimum that existed BEFORE the health gate:
// connected, valid, a Public provide key, and the latency and speed samples
// whose absence is otherwise the usual reason a fixture provider fails.
//
// It deliberately mirrors connectPublicAndNetworkOnlyProviders and adds no
// reliability stats of its own. A provider with no client reliability score row
// clears the lookback thresholds (see
// TestUpdateClientScoresCountsClientsWithoutReliabilityScores); one carrying a
// single freshly-added sample does not, so seeding stats here would make every
// fixture provider fail the pre-existing minimums and the health assertions
// below would pass for the wrong reason.
//
// Health is deliberately not set here, so each test states its own.
//
// Because these providers carry no reliability history, they clear the lookback
// thresholds trivially, which is what makes health the only discriminator in the
// tests below -- but it means these tests alone do not show that the health gate
// and the reliability minimums COMPOSE on a provider with real scores, which is
// what every provider in production has.
//
// TestFindProviders2ReliabilityFlushLag and TestFindProviders2ReliabilityDeployGap
// are that coverage. They accumulate genuine per-block reliability (they assert
// the scoring loop ran, so the pass is not vacuous) and then go end to end
// through FindProviders2 with ForceMinimum unset, asserting every provider comes
// back. They fail if a healthy provider with real reliability scores is
// wrongly excluded -- which is exactly how they behaved before the
// testing_setProviderEgressHealthy call was added to them.
func testing_connectQualifyingProviders(
	ctx context.Context,
	t testing.TB,
	location *Location,
	n int,
) []server.Id {
	t.Helper()

	handlerId := CreateNetworkClientHandler(ctx)
	clientIds := []server.Id{}

	for i := range n {
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			fmt.Sprintf("0.0.0.%d:0", i+1),
			handlerId,
		)
		connect.AssertEqual(t, err, nil)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})

		// identical latency and speed for every provider, so nothing but the
		// health record can separate them
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 30, 1,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
				connectionId, 100*1024*1024, 1,
			))
		})

		clientIds = append(clientIds, clientId)
	}

	UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

	return clientIds
}

// testing_selectableClientScores is what a user-facing caller sees: the
// precomputed pool for a location with forceMinimum off, which is the mode
// GetProviderLocations and FindProviders2 read.
func testing_selectableClientScores(
	ctx context.Context,
	t testing.TB,
	location *Location,
	forceMinimum bool,
) map[server.Id]*ClientScore {
	t.Helper()
	clientScores, err := loadClientScores(
		forceMinimum,
		RankModeQuality,
		ctx,
		map[server.Id]bool{location.LocationId: true},
		map[server.Id]bool{},
		server.Id{},
		100,
	)
	connect.AssertEqual(t, err, nil)
	return clientScores
}

func testing_healthGateCity(ctx context.Context, t testing.TB) *Location {
	testing_enableProviderEgressTest(t)
	city := &Location{
		LocationType: LocationTypeCity,
		City:         "Palo Alto",
		Region:       "California",
		Country:      "United States",
		CountryCode:  "us",
	}
	CreateLocation(ctx, city)
	return city
}

// A disabled gate restores the pre-egress-test selection rule. This covers
// both missing evidence and explicit unhealthy evidence so the setting cannot
// accidentally be implemented as only an empty-table fallback.
func TestUpdateClientScoresDoesNotRequireEgressHealthWhenDisabled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(
			providerConfigResourceName,
			[]byte("enable_egress_test: false\n"),
		))

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 2)
		measuredUnhealthy, neverMeasured := clientIds[0], clientIds[1]
		testing_setProviderEgressHealth(ctx, measuredUnhealthy, 0, 131)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)
		if _, ok := clientScores[measuredUnhealthy]; !ok {
			t.Fatal("a measured-unhealthy provider was gated while enable_egress_test=false")
		}
		if _, ok := clientScores[neverMeasured]; !ok {
			t.Fatal("a never-measured provider was gated while enable_egress_test=false")
		}
	})
}

// The whole point of the feature. A provider measured 0 of 131 -- every
// destination blackholed, the exact reading 158 seeded beta proxies gave -- must
// not be offered to a user, while an identically-configured provider measured
// healthy still is. Before the gate BOTH were selectable, because a blackholed
// proxy is still connected and connectivity is all the pre-existing minimums
// measure.
func TestUpdateClientScoresExcludesMeasuredUnhealthyProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 2)
		healthyClientId, deadClientId := clientIds[0], clientIds[1]

		testing_setProviderEgressHealth(ctx, healthyClientId, 131, 131)
		testing_setProviderEgressHealth(ctx, deadClientId, 0, 131)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)

		if _, ok := clientScores[healthyClientId]; !ok {
			t.Fatal("a provider measured 131/131 is not selectable; the gate is excluding healthy providers")
		}
		if _, ok := clientScores[deadClientId]; ok {
			t.Fatal("a provider measured 0/131 is still selectable; the measurement is not being acted on")
		}
	})
}

// Fail closed. A provider nobody has probed is not known to work, and the
// user's rule is out until you pass. This is the case that hides the ~2038
// never-tested providers in the beta pool, and it is the one a well-meaning
// change is most likely to soften ("no record means no evidence of harm"), so
// it gets its own test.
func TestUpdateClientScoresExcludesNeverMeasuredProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 2)
		measuredClientId, neverMeasuredClientId := clientIds[0], clientIds[1]

		// deliberately no health record at all for neverMeasuredClientId
		testing_setProviderEgressHealthy(ctx, measuredClientId)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)

		if _, ok := clientScores[measuredClientId]; !ok {
			t.Fatal("a measured-healthy provider is not selectable")
		}
		if _, ok := clientScores[neverMeasuredClientId]; ok {
			t.Fatal("a provider with no health record at all is selectable; the gate must fail closed")
		}
		if got := GetProviderEgressHealth(ctx, neverMeasuredClientId); got != nil {
			t.Fatalf("fixture is wrong: the never-measured provider has a health record %+v", got)
		}
	})
}

// The gate must be a gate and nothing else. Two providers identical in every
// scored respect, differing only in that one measured 131/131 and the other
// 129/131 -- both comfortably healthy -- must come out with exactly the same
// score and the same scaled weight, because health is not an input to the
// ranking arithmetic. If health ever gets folded into the weight, the better-
// measured provider outranks the other here and this fails.
func TestUpdateClientScoresHealthDoesNotRescoreQualifyingProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 2)
		perfectClientId, goodClientId := clientIds[0], clientIds[1]

		testing_setProviderEgressHealth(ctx, perfectClientId, 131, 131)
		testing_setProviderEgressHealth(ctx, goodClientId, 129, 131)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)

		perfect, ok := clientScores[perfectClientId]
		if !ok {
			t.Fatal("the 131/131 provider is not selectable")
		}
		good, ok := clientScores[goodClientId]
		if !ok {
			t.Fatal("the 129/131 provider is not selectable; 129/131 is well above the 90% bar")
		}

		if perfect.Scores[RankModeQuality] != good.Scores[RankModeQuality] {
			t.Fatalf(
				"scores diverged: 131/131 scored %d, 129/131 scored %d -- health must not enter the score",
				perfect.Scores[RankModeQuality],
				good.Scores[RankModeQuality],
			)
		}
		if perfect.ScaledWeights[RankModeQuality] != good.ScaledWeights[RankModeQuality] {
			t.Fatalf(
				"scaled weights diverged: 131/131 weighted %v, 129/131 weighted %v -- this is a gate, not a re-ranking",
				perfect.ScaledWeights[RankModeQuality],
				good.ScaledWeights[RankModeQuality],
			)
		}
		if perfect.ReliabilityWeight != good.ReliabilityWeight {
			t.Fatalf(
				"reliability weights diverged: %v vs %v",
				perfect.ReliabilityWeight,
				good.ReliabilityWeight,
			)
		}
	})
}

// The boundary is at exactly 90%: >= passes, below fails. Written at a
// denominator of 100 on purpose -- 90% of 131 is 117.9, so 131 destinations
// cannot express the boundary at all and a test using it would prove nothing
// about which side of the line `>=` falls on.
func TestUpdateClientScoresEgressHealthBoundaryIsInclusive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 2)
		atBarClientId, belowBarClientId := clientIds[0], clientIds[1]

		testing_setProviderEgressHealth(ctx, atBarClientId, 90, 100)
		testing_setProviderEgressHealth(ctx, belowBarClientId, 89, 100)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)

		if _, ok := clientScores[atBarClientId]; !ok {
			t.Fatal("a provider at exactly 90/100 is excluded; the bar is >= 90%, not > 90%")
		}
		if _, ok := clientScores[belowBarClientId]; ok {
			t.Fatal("a provider at 89/100 is selectable; it is below the bar")
		}
	})
}

// A run that sampled no destinations measured nothing, so it is not evidence of
// health and must not pass -- and computing ok/total on it must not divide by
// zero. total_count is an ordinary integer column with no positive constraint,
// so this row is reachable from any prober that reports an empty sample.
func TestUpdateClientScoresZeroTotalHealthDoesNotPassOrPanic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 2)
		emptyRunClientId, healthyClientId := clientIds[0], clientIds[1]

		testing_setProviderEgressHealth(ctx, emptyRunClientId, 0, 0)
		testing_setProviderEgressHealthy(ctx, healthyClientId)

		// the pass itself must survive the row
		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)

		if _, ok := clientScores[emptyRunClientId]; ok {
			t.Fatal("a provider whose health run sampled 0 destinations is selectable; 0/0 measures nothing")
		}
		if _, ok := clientScores[healthyClientId]; !ok {
			t.Fatal("the healthy provider disappeared alongside the empty-run one")
		}
	})
}

// forceMinimum exists so an operator census can see providers that fail the
// minimums. The health gate is folded into exactly that same PassesMinimums
// flag, so a health-excluded provider must stay visible to a forceMinimum
// caller for the same reason -- otherwise the providers most in need of
// inspection are the ones an operator can no longer see.
func TestUpdateClientScoresForceMinimumStillSeesHealthExcludedProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 3)
		healthyClientId, deadClientId, neverMeasuredClientId := clientIds[0], clientIds[1], clientIds[2]

		testing_setProviderEgressHealthy(ctx, healthyClientId)
		testing_setProviderEgressHealth(ctx, deadClientId, 0, 131)
		// neverMeasuredClientId gets no record

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		// the user-facing view has only the healthy one
		strict := testing_selectableClientScores(ctx, t, city, false)
		if len(strict) != 1 {
			t.Fatalf("user-facing pool holds %d providers, want only the measured-healthy one", len(strict))
		}
		if _, ok := strict[healthyClientId]; !ok {
			t.Fatal("the wrong provider survived the user-facing gate")
		}

		// the operator census has all three
		forced := testing_selectableClientScores(ctx, t, city, true)
		for _, clientId := range []server.Id{healthyClientId, deadClientId, neverMeasuredClientId} {
			if _, ok := forced[clientId]; !ok {
				t.Fatalf("forceMinimum=true cannot see provider %s; the operator census must be unaffected", clientId)
			}
		}

		// and the location itself is still listed to a forceMinimum caller
		forcedStables, err := loadLocationStables(
			ctx,
			[]server.Id{city.CountryLocationId},
			true,
			RankModeQuality,
			server.Id{},
		)
		connect.AssertEqual(t, err, nil)
		if _, ok := forcedStables[city.CountryLocationId]; !ok {
			t.Fatal("forceMinimum=true no longer lists the location")
		}
	})
}

// THE GRADUATION PATH. An excluded provider has to keep being probed, or it can
// never measure healthy again and is stuck out permanently -- the gate would
// become a one-way door.
//
// GetProviderEgressLocationDue reads network_client_location_reliability,
// provide_key, provider_egress_location and provider_egress_probe_attempt. It
// does not read PassesMinimums, the redis score sets, or provider_egress_health,
// so structurally it cannot see the gate. This test is the regression guard on
// that: it asserts both halves at once -- gone from the selection pool, still in
// the probe queue -- so a future change that wires selection state into the
// queue fails here rather than silently stranding every excluded provider.
func TestProbeDueQueueIgnoresTheEgressHealthGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 1)
		deadClientId := clientIds[0]

		// measured blackholed, and probed an hour ago -- so this goes through the
		// stale-but-probed pass of the due query, which is the realistic state for
		// a provider that has a health record at all
		testing_setProviderEgressHealth(ctx, deadClientId, 0, 131)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    deadClientId,
			LocationId:  city.LocationId,
			CountryCode: "us",
			ObservedAt:  now.Add(-time.Hour),
		})

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)

		clientScores := testing_selectableClientScores(ctx, t, city, false)
		if _, ok := clientScores[deadClientId]; ok {
			t.Fatal("fixture is wrong: the provider was not excluded, so this proves nothing about the queue")
		}

		due := GetProviderEgressLocationDue(
			ctx,
			now.Add(-time.Minute),
			now.Add(-time.Minute),
			100,
		)
		if !slices.Contains(due, deadClientId) {
			t.Fatal(
				"an excluded provider is not in the probe due-queue: it can never be re-measured, " +
					"so it can never graduate back into the public list",
			)
		}
	})
}

// Recovery is automatic and needs no manual step: the gate reads the health
// table fresh on every pass, and SetProviderEgressHealth replaces the row, so
// the next pass after a good measurement puts the provider back.
//
// This is also what makes the deliberate absence of any staleness cutoff safe.
// A stale BAD record keeps a provider hidden until it is probed again, and the
// ungated due-queue above is what guarantees that re-probe happens.
func TestUpdateClientScoresRestoresAProviderWhoseHealthRecovers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)

		clientIds := testing_connectQualifyingProviders(ctx, t, city, 1)
		clientId := clientIds[0]

		testing_setProviderEgressHealth(ctx, clientId, 0, 131)

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)
		if _, ok := testing_selectableClientScores(ctx, t, city, false)[clientId]; ok {
			t.Fatal("the blackholed provider was selectable before it recovered")
		}

		// a later probe finds it healthy. nothing else happens -- no operator
		// action, no re-registration, no cache flush
		testing_setProviderEgressHealth(ctx, clientId, 131, 131)

		err = UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)
		if _, ok := testing_selectableClientScores(ctx, t, city, false)[clientId]; !ok {
			t.Fatal("a provider that measured healthy again did not come back on the next pass")
		}
	})
}

func TestProviderCountFilter(t *testing.T) {
	healthy := server.NewId()
	degraded := server.NewId()
	unmeasured := server.NewId()
	zeroTotal := server.NewId()
	wrongCountry := server.NewId()
	unobserved := server.NewId()

	f := providerCountFilter{
		healthCounts: map[server.Id]ProviderEgressHealthCounts{
			// 118/131 is the first passing value: 10*118 >= 9*131 (1180 >= 1179)
			healthy: {OKCount: 118, Total: 131},
			// 117/131 is the last failing value: 1170 < 1179
			degraded:     {OKCount: 117, Total: 131},
			zeroTotal:    {OKCount: 0, Total: 0},
			wrongCountry: {OKCount: 131, Total: 131},
			unobserved:   {OKCount: 131, Total: 131},
		},
		countryCodes: map[server.Id]string{
			healthy:      "us",
			degraded:     "us",
			zeroTotal:    "us",
			wrongCountry: "gb",
			// unobserved deliberately absent
		},
	}

	// the 90% boundary, asserted on the integer comparison
	assert.Equal(t, f.passesHealth(healthy), true)
	assert.Equal(t, f.passesHealth(degraded), false)

	// fail closed: never probed, and probed-with-no-destinations
	assert.Equal(t, f.passesHealth(unmeasured), false)
	assert.Equal(t, f.passesHealth(zeroTotal), false)

	// counts only where health passes AND the observed country matches
	assert.Equal(t, f.countsTowardCountry(healthy, "us"), true)
	assert.Equal(t, f.countsTowardCountry(degraded, "us"), false)

	// healthy but egressing from somewhere else: not counted in the claim
	assert.Equal(t, f.countsTowardCountry(wrongCountry, "us"), false)
	// ...and it does count where it actually is
	assert.Equal(t, f.countsTowardCountry(wrongCountry, "gb"), true)

	// healthy but never located: fail closed
	assert.Equal(t, f.countsTowardCountry(unobserved, "us"), false)

	// comparison is case insensitive on the caller's side
	assert.Equal(t, f.countsTowardCountry(healthy, "US"), true)
}

// TestProviderCountFilterShouldSkipCountGate covers the fleet-wide floor in
// UpdateClientLocations: health and observed-country come from two
// INDEPENDENT pipelines (an external push endpoint and a separate internal
// job, respectively) that can stall separately, so either map being empty --
// not just health -- must skip the count gate for the pass. This is pure
// in-memory logic, no database required.
func TestProviderCountFilterShouldSkipCountGate(t *testing.T) {
	someProvider := server.NewId()

	// both maps empty -- neither pipeline has produced anything: skip
	assert.Equal(
		t,
		providerCountFilter{
			healthCounts: map[server.Id]ProviderEgressHealthCounts{},
			countryCodes: map[server.Id]string{},
		}.shouldSkipCountGate(),
		true,
	)

	// health empty, countryCodes populated -- the health pipeline alone
	// stalled: skip
	assert.Equal(
		t,
		providerCountFilter{
			healthCounts: map[server.Id]ProviderEgressHealthCounts{},
			countryCodes: map[server.Id]string{someProvider: "us"},
		}.shouldSkipCountGate(),
		true,
	)

	// countryCodes empty, health populated -- the location pipeline alone
	// stalled: skip. This is the case the previous, health-only guard missed.
	assert.Equal(
		t,
		providerCountFilter{
			healthCounts: map[server.Id]ProviderEgressHealthCounts{someProvider: {OKCount: 131, Total: 131}},
			countryCodes: map[server.Id]string{},
		}.shouldSkipCountGate(),
		true,
	)

	// neither empty -- both pipelines are producing data: do not skip, apply
	// the gate normally
	assert.Equal(
		t,
		providerCountFilter{
			healthCounts: map[server.Id]ProviderEgressHealthCounts{someProvider: {OKCount: 131, Total: 131}},
			countryCodes: map[server.Id]string{someProvider: "us"},
		}.shouldSkipCountGate(),
		false,
	)
}

// TestProviderCountFilterShouldRecountUngated covers the OUTPUT-side half of
// the fleet-wide floor. shouldSkipCountGate looks at the two input maps before
// the pass; shouldRecountUngated looks at what the pass produced. Non-empty
// inputs can still yield an empty count -- a fleet-wide claimed/observed
// mismatch, or every claimed country resolving to NULL -- and an empty count
// wipes every location out of redis. Pure in-memory logic, no database.
func TestProviderCountFilterShouldRecountUngated(t *testing.T) {
	// the failure this exists to catch: rows existed, the gate ate all of
	// them, nothing counted anywhere. Do not publish that; recount ungated.
	assert.Equal(t, shouldRecountUngated(true, 152, 0), true)

	// the gate counted something: publish it, however small
	assert.Equal(t, shouldRecountUngated(true, 152, 1), false)

	// no connected + valid + Public rows at all. Supply really is gone and
	// emptying the published list is the correct answer, not a fallback.
	assert.Equal(t, shouldRecountUngated(true, 0, 0), false)

	// the gate was already skipped for this pass (shouldSkipCountGate fired).
	// An empty count is then the ungated answer already -- recounting would
	// produce the identical result and loop the reasoning.
	assert.Equal(t, shouldRecountUngated(false, 152, 0), false)
	assert.Equal(t, shouldRecountUngated(false, 0, 0), false)
}

// Testing_CreateProviderAtLocation inserts exactly the rows a provider needs
// to clear the pre-existing UpdateClientLocations gate: a network_client row,
// a Public provide_key, and a network_client_location_reliability row that is
// connected, valid, and pinned to countryId at all three granularities (city =
// region = country), mirroring the country-only fallback in
// SetConnectionLocation (a geo lookup with no city/region resolves to the
// country id at every column -- see the fix(beta) comment there).
//
// It also creates the `location` row for countryId itself: the gated query
// under test resolves the provider's CLAIMED country by joining
// network_client_location_reliability.country_location_id back to `location`,
// and loadClientLocations only surfaces a location that has a `location` row.
// Both go through raw SQL rather than CreateLocation/SetConnectionLocation
// because the caller picks countryId up front (so a later lookup can key on
// it), and CreateLocation always mints its own id.
//
// health and observed egress location are deliberately NOT set here -- every
// caller states its own via SetProviderEgressHealth/SetProviderEgressLocation,
// exactly as the pre-gate minimums (connected/valid/Public key) are set here
// while health is layered on top by each test.
func Testing_CreateProviderAtLocation(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	countryId server.Id,
	countryCode string,
) {
	countryCode = strings.ToLower(countryCode)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO location (
					location_id,
					location_type,
					location_name,
					country_location_id,
					country_code,
					location_full_name
				)
				VALUES ($1, $2, $3, $1, $4, $5)
				ON CONFLICT (location_id) DO NOTHING
			`,
			countryId,
			LocationTypeCountry,
			// One value, bound three times as three separate parameters.
			// Reusing a single $3 across location_name (varchar), country_code
			// (char(2)) and location_full_name (varchar) makes postgres try to
			// deduce one type for it and fail the whole statement with
			// "inconsistent types deduced for parameter $3" (SQLSTATE 42P08).
			// Caught only by running against a real database -- it builds and
			// vets clean.
			countryCode,
			countryCode,
			countryCode,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client (
					client_id,
					network_id
				)
				VALUES ($1, $2)
				ON CONFLICT (client_id) DO NOTHING
			`,
			clientId,
			networkId,
		))

		// client_address_hash_count = 1 AND location_count = 1 AND
		// country_location_id IS NOT NULL is exactly the GENERATED `valid`
		// expression on this table (see the CREATE TABLE in db_migrations.go);
		// `valid` itself cannot be assigned directly.
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_location_reliability (
					client_id,
					network_id,
					update_block_number,
					city_location_id,
					region_location_id,
					country_location_id,
					client_address_hash_count,
					location_count,
					connected
				)
				VALUES ($1, $2, 0, $3, $3, $3, 1, 1, true)
				ON CONFLICT (client_id) DO UPDATE SET
					network_id = $2,
					city_location_id = $3,
					region_location_id = $3,
					country_location_id = $3,
					client_address_hash_count = 1,
					location_count = 1,
					connected = true
			`,
			clientId,
			networkId,
			countryId,
		))
	})

	SetProvide(ctx, clientId, map[ProvideMode][]byte{
		ProvideModePublic: []byte("testing-public-provide-secret"),
	})
}

// TestUpdateClientLocationsCountIsGated is the core assertion for this task:
// UpdateClientLocations must count a provider toward provider_count only where
// a probe measured it healthy AND observed it egressing from the country it
// claims. Before this change, connected + valid + a Public provide key was
// enough on its own -- an unreachable or misrepresenting provider still
// inflated the count.
//
// It covers all three exclusion paths against a real database, because each
// one leans on SQL the in-memory providerCountFilter test cannot exercise: the
// health check, the claimed-vs-observed MISMATCH, and a NULL claimed country
// arriving from the LEFT JOIN onto `location`.
func TestUpdateClientLocationsCountIsGated(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		testing_enableProviderEgressTest(t)

		networkId := server.NewId()
		countryId := server.NewId()

		healthy := server.NewId()
		unhealthy := server.NewId()
		unprobed := server.NewId()
		// healthy, but a probe watched it egress from GB while it claims US.
		// This is the MISMATCH path: it exercises the LEFT JOIN onto
		// `location`, the char(2) country_code round trip, and the
		// strings.ToLower comparison inside countsTowardCountry -- none of
		// which the in-memory filter test can reach.
		wrongCountry := server.NewId()
		// healthy AND observed exactly where it claims to be, but its
		// country_location_id points at a `location` row that does not exist,
		// so the LEFT JOIN yields a NULL claimed country. This is the
		// NULL-claimed-country path: unverifiable against anything, so it must
		// fail closed like an unobserved provider.
		orphanClaim := server.NewId()
		orphanCountryId := server.NewId()

		// four providers in the claimed country, all connected with a Public
		// provide key; only `healthy` is measured healthy and observed in US
		for _, clientId := range []server.Id{healthy, unhealthy, unprobed, wrongCountry} {
			Testing_CreateProviderAtLocation(ctx, networkId, clientId, countryId, "US")
		}
		// created at its own country id, whose `location` row is then removed
		// out from under it. This schema declares no foreign keys, so the
		// reliability row survives pointing at nothing -- which is exactly the
		// state the LEFT JOIN has to handle.
		//
		// Uses "ZZ" rather than "US" because `location` also carries
		// UNIQUE(location_full_name) (see db_migrations.go), and
		// Testing_CreateProviderAtLocation's location insert is only
		// ON CONFLICT (location_id) DO NOTHING -- a second "us" row at a
		// different location id collides with that unique index instead of
		// being ignored. The observed country code below is never actually
		// compared for this provider (the NULL claimed country short-circuits
		// first), but keeping it "ZZ" too keeps the fixture internally
		// consistent with what it claims to be.
		Testing_CreateProviderAtLocation(ctx, networkId, orphanClaim, orphanCountryId, "ZZ")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM location WHERE location_id = $1`,
				orphanCountryId,
			))
		})

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: healthy, OKCount: 131, Total: 131, MeasuredAt: server.NowUtc(),
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: unhealthy, OKCount: 0, Total: 131, MeasuredAt: server.NowUtc(),
		})
		// both new providers pass health, so health is not what excludes them:
		// the claimed-vs-observed country check is.
		for _, clientId := range []server.Id{wrongCountry, orphanClaim} {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId: clientId, OKCount: 131, Total: 131, MeasuredAt: server.NowUtc(),
			})
		}
		// ObservedAt must be now: GetAllProviderEgressCountryCodes returns only
		// rows observed within ProviderEgressLocationMaxAge, so a zero-value
		// ObservedAt would make these fixtures invisible and the assertions
		// below would pass for the wrong reason.
		for _, clientId := range []server.Id{healthy, unhealthy} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, CountryCode: "US",
				Verdict: "verified", ObservedAt: server.NowUtc(),
			})
		}
		// matches the "ZZ" it claims (see Testing_CreateProviderAtLocation
		// call above) so the fixture stays internally consistent, even though
		// the NULL claimed country short-circuits the comparison before this
		// observed code is ever read.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: orphanClaim, CountryCode: "ZZ",
			Verdict: "verified", ObservedAt: server.NowUtc(),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: wrongCountry, CountryCode: "GB",
			Verdict: "verified", ObservedAt: server.NowUtc(),
		})

		UpdateClientLocations(ctx, 1*time.Hour)

		clientLocations, err := loadClientLocations(ctx, map[server.Id]bool{countryId: true})
		assert.Equal(t, err, nil)

		// only the measured-healthy, observed-in-US provider is counted.
		// Before this change all three of the original providers counted; the
		// GB-egressing and NULL-claimed-country providers must not add to it
		// either, so the total is still exactly 1.
		assert.Equal(t, clientLocations[countryId].ClientCount, 1)
	})
}

// Disabling the test bypasses the per-provider egress predicate even when the
// probe tables are partially populated. Keeping one provider healthy prevents
// the existing all-zero output fallback from making this assertion vacuous.
func TestUpdateClientLocationsCountIsUngatedWhenEgressTestDisabled(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(
			providerConfigResourceName,
			[]byte("enable_egress_test: false\n"),
		))

		networkId := server.NewId()
		countryId := server.NewId()
		healthy := server.NewId()
		unhealthy := server.NewId()
		for _, clientId := range []server.Id{healthy, unhealthy} {
			Testing_CreateProviderAtLocation(ctx, networkId, clientId, countryId, "US")
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, CountryCode: "US",
				Verdict: "verified", ObservedAt: server.NowUtc(),
			})
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: healthy, OKCount: 131, Total: 131, MeasuredAt: server.NowUtc(),
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: unhealthy, OKCount: 0, Total: 131, MeasuredAt: server.NowUtc(),
		})

		UpdateClientLocations(ctx, time.Hour)

		clientLocations, err := loadClientLocations(ctx, map[server.Id]bool{countryId: true})
		assert.Equal(t, err, nil)
		assert.Equal(t, clientLocations[countryId].ClientCount, 2)
	})
}

// TestFindProviders2ClientIdBypassesHealthGate pins a deliberate exception to
// the health/location gate: a spec.ClientId entry is appended straight to the
// result (see the `if spec.ClientId != nil` branch in FindProviders2) without
// ever touching loadClientScores or PassesMinimums. An explicit client id is
// a caller's deliberate choice -- e.g. reconnecting to a known provider -- so
// the public-list gate that excludes measured-unhealthy providers elsewhere
// must not apply here. Nothing tested this before; if the bypass were ever
// folded into the gated path, this test would start failing because the
// provider marked comprehensively unhealthy below would be dropped from the
// result instead of being returned.
func TestFindProviders2ClientIdBypassesHealthGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		countryId := server.NewId()
		unhealthy := server.NewId()

		Testing_CreateProviderAtLocation(ctx, networkId, unhealthy, countryId, "US")
		// measured comprehensively dead, so the gate excludes it everywhere else
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: unhealthy, OKCount: 0, Total: 131, MeasuredAt: server.NowUtc(),
		})

		clientSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkId, server.NewId(), "test", false, false),
		)

		result, err := FindProviders2(&FindProviders2Args{
			Specs: []*ProviderSpec{{ClientId: &unhealthy}},
			Count: 1,
		}, clientSession)
		assert.Equal(t, err, nil)

		// an explicit client id is a deliberate choice by the caller, so the
		// public-list health gate must not apply to it
		assert.Equal(t, len(result.Providers), 1)
		assert.Equal(t, result.Providers[0].ClientId, unhealthy)
	})
}
