package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	"maps"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
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
		UpdateClientScores(ctx, 5*time.Second, 1)

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
