package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
	// "github.com/urnetwork/server/v2025/jwt"
	// "github.com/urnetwork/server/v2025/session"
)

// TODO
// index locations
// search index to see that locations return correctly

func TestLocationsSearch(t *testing.T) {
	// create locations

	// index locations

	// search around for lcoation and group

	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		server.Tx(ctx, func(tx server.PgTx) {

			locationSanFrancisco := &Location{
				LocationType: LocationTypeCity,
				City:         "San Francisco",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			}
			CreateLocation(ctx, locationSanFrancisco)
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Mateo",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Leandro",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Diego",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "San Luis Obispo",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "Tokyo San Fran",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})
			CreateLocation(ctx, &Location{
				LocationType: LocationTypeCity,
				City:         "Lima San Fran",
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			})

			locationGroupWhosTheBest := &LocationGroup{
				Name:     "Who's the best",
				Promoted: false,
			}
			CreateLocationGroup(ctx, locationGroupWhosTheBest)
			CreateLocationGroup(ctx, &LocationGroup{
				Name:     "What's the best",
				Promoted: false,
			})
			CreateLocationGroup(ctx, &LocationGroup{
				Name:     "Best who's the",
				Promoted: false,
			})

			IndexSearchLocationsInTx(ctx, tx)

			r1 := locationSearch().AroundIds(ctx, "san fra", 0)
			assert.Equal(t, len(r1), 3)
			assert.Equal(t, 0, r1[locationSanFrancisco.LocationId].ValueDistance)

			r2 := locationSearch().AroundIds(ctx, "san frn", 1)
			assert.Equal(t, len(r2), 3)
			assert.Equal(t, 1, r2[locationSanFrancisco.LocationId].ValueDistance)

			r3 := locationGroupSearch().AroundIds(ctx, "who's the", 0)
			assert.Equal(t, len(r3), 2)
			assert.Equal(t, 0, r3[locationGroupWhosTheBest.LocationGroupId].ValueDistance)

		})

	})

}
