package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
)

func TestAddDefaultLocations(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		AddDefaultLocations(ctx, 10)
	})
}

func TestCanonicalLocations(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
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
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		n := 1000
		out := make(chan bringyour.Id, n)

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

		locationIds := map[bringyour.Id]bool{}
		for i := 0; i < n; i += 1 {
			locationId := <-out
			locationIds[locationId] = true
		}

		assert.Equal(t, 1, len(locationIds))
	})
}

func TestBestAvailableProviders(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkIdA := bringyour.NewId()

		userIdA := bringyour.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		clientId := bringyour.NewId()

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)

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
			MemberLocationIds: []bringyour.Id{
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

		res, err := FindProviders2(findProviders2Args, clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(res.Providers), 1)
	})
}

// FIXME test find provider2 with exclude
// FIXME

func TestFindProviders2WithExclude(t *testing.T) {
	// create providers
	// search for providers with client exclude
	// search for providers with destination exclude
}

func TestFindLocationGroupByName(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		createLocationGroup := &LocationGroup{
			Name:     StrongPrivacyLaws,
			Promoted: true,
		}

		CreateLocationGroup(ctx, createLocationGroup)

		bringyour.Tx(ctx, func(tx bringyour.PgTx) {
			// query existing
			locationGroup := findLocationGroupByNameInTx(ctx, StrongPrivacyLaws, tx)
			assert.Equal(t, locationGroup.Name, StrongPrivacyLaws)
			assert.Equal(t, locationGroup.Promoted, true)

			// locationGroupId := locationGroup.LocationGroupId

			// query with incorrect case should still return
			// locationGroup = findLocationGroupByNameInTx(ctx, "strong privacy Laws And internet freedom", tx)
			// assert.Equal(t, locationGroup.Name, StrongPrivacyLaws)
			// assert.Equal(t, locationGroup.LocationGroupId, locationGroupId)
			// assert.Equal(t, locationGroup.Promoted, true)

			// query should return nil if no match
			locationGroup = findLocationGroupByNameInTx(ctx, "invalid", tx)
			assert.Equal(t, locationGroup, nil)

		})
	})
}
