package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
	// "bringyour.com/bringyour/jwt"
	// "bringyour.com/bringyour/session"
)

// TODO
// index locations
// search index to see that locations return correctly

func TestLocationsSearch(t *testing.T) {
	// create locations

	// index locations

	// search around for lcoation and group

	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		bringyour.Tx(ctx, func(tx bringyour.PgTx) {

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

			indexSearchLocationsInTx(ctx, tx)

			r1 := locationSearch.AroundIds(ctx, "san fra", 0)
			assert.Equal(t, len(r1), 1)
			assert.Equal(t, 0, r1[locationSanFrancisco.LocationId].ValueDistance)

			r2 := locationGroupSearch.AroundIds(ctx, "who's the", 0)
			assert.Equal(t, len(r2), 1)
			assert.Equal(t, 0, r2[locationGroupWhosTheBest.LocationGroupId].ValueDistance)

		})

	})

}