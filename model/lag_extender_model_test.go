package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The derived places the extender sample reads (lag.dev/PLAN.md A4): an
// extender's derived location with the names of its city, region and
// country, for many extenders in one query, and none for an extender without
// one or for a provider's row.
func TestGetLagExtenderDerivedPlaces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		frankfurt := testStatsCityLocation(ctx, "Frankfurt am Main", "Hesse", "Germany", "de")
		osaka := testStatsCityLocation(ctx, "Osaka", "Osaka", "Japan", "jp")
		now := server.NowUtc()

		placed := server.NewId()
		other := server.NewId()
		unplaced := server.NewId()
		provider := server.NewId()
		placedRow := testDerivedLocation(DerivedLocationNodeKindExtender, placed, frankfurt, now)
		placedRow.Latitude = 50.11
		placedRow.Longitude = 8.68
		ReplaceDerivedLocations(ctx, []*DerivedLocation{
			placedRow,
			testDerivedLocation(DerivedLocationNodeKindExtender, other, osaka, now),
			// a provider's row is never an extender's place, whatever its id
			testDerivedLocation(DerivedLocationNodeKindProvider, provider, osaka, now),
		})

		places := GetLagExtenderDerivedPlaces(ctx, []server.Id{placed, other, unplaced, provider})
		connect.AssertEqual(t, len(places), 2)
		connect.AssertEqual(t, places[placed], &LagPlace{
			City:        "Frankfurt am Main",
			Region:      "Hesse",
			CountryCode: "de",
			CountryName: "Germany",
			Latitude:    50.11,
			Longitude:   8.68,
		})
		connect.AssertEqual(t, places[other].City, "Osaka")
		connect.AssertEqual(t, places[other].CountryCode, "jp")
		connect.AssertEqual(t, places[unplaced], (*LagPlace)(nil))

		connect.AssertEqual(t, len(GetLagExtenderDerivedPlaces(ctx, nil)), 0)
	})
}
