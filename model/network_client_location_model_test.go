package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAddDefaultLocations(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		AddDefaultLocations(ctx, 10)
	})
}

func TestCanonicalLocations(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		us1 := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, us1)

		assert.Equal(t, us1.LocationId, us1.CountryLocationId)

		us2 := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, us2)

		assert.Equal(t, us2.LocationId, us1.LocationId)
		assert.Equal(t, us2.LocationId, us2.CountryLocationId)

		a := &Location{
			LocationType: LocationTypeRegion,
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, a)

		assert.Equal(t, a.LocationId, a.RegionLocationId)
		assert.Equal(t, a.CountryLocationId, us1.LocationId)

		b := &Location{
			LocationType: LocationTypeRegion,
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, b)

		assert.Equal(t, a.LocationId, b.LocationId)
		assert.Equal(t, a.RegionLocationId, b.RegionLocationId)
		assert.Equal(t, a.CountryLocationId, b.CountryLocationId)

		c := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, c)

		assert.Equal(t, c.RegionLocationId, a.LocationId)
		assert.Equal(t, c.CountryLocationId, a.CountryLocationId)

		d := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, d)

		assert.Equal(t, d.LocationId, c.LocationId)
		assert.Equal(t, d.RegionLocationId, c.RegionLocationId)
		assert.Equal(t, d.CountryLocationId, c.CountryLocationId)
	})
}

func TestCanonicalLocationsParallel(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
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

		assert.Equal(t, 1, len(locationIds))
	})
}

func TestBestAvailableProviders(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
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
		assert.Equal(t, err, nil)

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
					BestAvailable: &bestAvailable,
				},
			},
		}

		clientAddressHash, _, err := clientSessionA.ClientAddressHashPort()
		assert.Equal(t, err, nil)
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
		UpdateClientReliabilityScores(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc(), true)
		UpdateClientScores(ctx, 5*time.Second)

		res, err := FindProviders2(findProviders2Args, clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(res.Providers), 1)
	})
}

func TestFindProviders2WithExclude(t *testing.T) {
	// create providers
	// search for providers with client exclude
	// search for providers with destination exclude

	server.DefaultTestEnv().Run(func() {

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

			clientSession := session.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(networkId, userId, fmt.Sprintf("network%d", i), guestMode),
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
			assert.Equal(t, err, nil)

			secretKeys := map[ProvideMode][]byte{
				ProvideModePublic: make([]byte, 32),
			}

			SetProvide(ctx, clientId, secretKeys)

			SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})

			clientAddressHash, _, err := clientSession.ClientAddressHashPort()
			assert.Equal(t, err, nil)
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

		UpdateClientReliabilityScores(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour), true)
		UpdateClientScores(ctx, 5*time.Second)

		clientIds := maps.Keys(clientSessions)
		clientIdA := clientIds[0]
		clientSessionA := clientSessions[clientIdA]

		findProviders2Args := &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					LocationGroupId: &createLocationGroup.LocationGroupId,
				},
			},
			Count: 2 * n,
		}
		res, err := FindProviders2(findProviders2Args, clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(res.Providers), n)

		bestAvailable := true
		findProviders2Args = &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: &bestAvailable,
				},
			},
			Count: 2 * n,
		}
		res, err = FindProviders2(findProviders2Args, clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(res.Providers), n)

		findProviders2Args = &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: &bestAvailable,
				},
			},
			Count:            2 * n,
			ExcludeClientIds: []server.Id{clientIdA},
		}
		res, err = FindProviders2(findProviders2Args, clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(res.Providers), n-1)

		findProviders2Args = &FindProviders2Args{
			Specs: []*ProviderSpec{
				{
					BestAvailable: &bestAvailable,
				},
			},
			Count:            2 * n,
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
		}

		// client ids not in the exclude destinations intermediaries will come first
		priorityClientIds := map[server.Id]bool{}
		for _, clientId := range clientIds[10:] {
			priorityClientIds[clientId] = true
		}
		// the exclude destination intermediaries (not the egress hop) will come next
		otherClientIds := map[server.Id]bool{}
		otherClientIds[clientIds[1]] = true
		otherClientIds[clientIds[2]] = true
		otherClientIds[clientIds[4]] = true
		otherClientIds[clientIds[5]] = true
		otherClientIds[clientIds[7]] = true
		otherClientIds[clientIds[8]] = true

		res, err = FindProviders2(findProviders2Args, clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(res.Providers), len(priorityClientIds)+len(otherClientIds))
		for _, provider := range res.Providers[:len(priorityClientIds)] {
			ok := priorityClientIds[provider.ClientId]
			assert.Equal(t, ok, true)
		}
		for _, provider := range res.Providers[len(priorityClientIds):] {
			ok := otherClientIds[provider.ClientId]
			assert.Equal(t, ok, true)
		}

	})
}

// func TestFindLocationGroupByName(t *testing.T) {
// 	server.DefaultTestEnv().Run(func() {

// 		ctx := context.Background()

// 		createLocationGroup := &LocationGroup{
// 			Name:     StrongPrivacyLaws,
// 			Promoted: true,
// 		}

// 		CreateLocationGroup(ctx, createLocationGroup)

// 		server.Tx(ctx, func(tx server.PgTx) {
// 			// query existing
// 			locationGroup := findLocationGroupByNameInTx(ctx, StrongPrivacyLaws, tx)
// 			assert.Equal(t, locationGroup.Name, StrongPrivacyLaws)
// 			assert.Equal(t, locationGroup.Promoted, true)

// 			// locationGroupId := locationGroup.LocationGroupId

// 			// query with incorrect case should still return
// 			// locationGroup = findLocationGroupByNameInTx(ctx, "strong privacy Laws And internet freedom", tx)
// 			// assert.Equal(t, locationGroup.Name, StrongPrivacyLaws)
// 			// assert.Equal(t, locationGroup.LocationGroupId, locationGroupId)
// 			// assert.Equal(t, locationGroup.Promoted, true)

// 			// query should return nil if no match
// 			locationGroup = findLocationGroupByNameInTx(ctx, "invalid", tx)
// 			assert.Equal(t, locationGroup, nil)

// 		})
// 	})
// }
