// The counted rollup hierarchy and current location metadata can differ.
// Exercise SQL selection, top-link construction, and Redis publication together.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// One counted city/region/country and two metadata-only alternative parents.
type clientLocationParentFixture struct {
	clientId       server.Id
	cityId         server.Id
	regionId       server.Id
	countryId      server.Id
	otherRegionId  server.Id
	otherCountryId server.Id
}

// Counted supply is real connected/valid/Public SQL state; no cache is mocked.
func testClientLocationParentFixture(ctx context.Context, t testing.TB) clientLocationParentFixture {
	t.Helper()
	t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("enable_egress_test: false\n")))
	fixture := clientLocationParentFixture{
		clientId:       server.NewId(),
		cityId:         server.NewId(),
		regionId:       server.NewId(),
		countryId:      server.NewId(),
		otherRegionId:  server.NewId(),
		otherCountryId: server.NewId(),
	}
	Testing_CreateProviderAtLocation(ctx, server.NewId(), fixture.clientId, fixture.countryId, "zz")
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO location (
				location_id, location_type, location_name, city_location_id,
				region_location_id, country_location_id, country_code, location_full_name
			) VALUES
				($1, 'city', 'Synthetic Harbor', $1, $2, $3, 'zz', 'Synthetic Harbor, Test Region, zz'),
				($2, 'region', 'Test Region', NULL, $2, $3, 'zz', 'Test Region, zz'),
				($4, 'region', 'Other Test Region', NULL, $4, $3, 'zz', 'Other Test Region, zz'),
				($5, 'country', 'Other Test Country', NULL, NULL, $5, 'zx', 'Other Test Country, zx')
		`, fixture.cityId, fixture.regionId, fixture.countryId, fixture.otherRegionId, fixture.otherCountryId))
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE network_client_location_reliability
			SET city_location_id = $2, region_location_id = $3
			WHERE client_id = $1
		`, fixture.clientId, fixture.cityId, fixture.regionId))
	})
	SetProviderEgressLocation(ctx, &ProviderEgressLocation{
		ClientId: fixture.clientId, LocationId: fixture.cityId,
		CountryCode: "zz", Verdict: "verified", ObservedAt: server.NowUtc(),
	})
	return fixture
}

// Recover only the owning production call so RED is a precise assertion,
// not a package panic or an environment-setup failure.
func testClientLocationParentPublish(ctx context.Context, t testing.TB, fixture clientLocationParentFixture) map[server.Id]*ClientLocation {
	t.Helper()
	var updateErr error
	if panicValue := server.HandleError(func() { updateErr = UpdateClientLocations(ctx, time.Hour) }); panicValue != nil {
		t.Fatalf("location publication panicked at sparse parent boundary: %v", panicValue)
	}
	if updateErr != nil {
		t.Fatalf("location publication failed: %v", updateErr)
	}
	locations, err := loadClientLocations(ctx, map[server.Id]bool{
		fixture.cityId: true, fixture.regionId: true, fixture.countryId: true,
		fixture.otherRegionId: true, fixture.otherCountryId: true,
	})
	if err != nil {
		t.Fatalf("read published locations: %v", err)
	}
	if locations[fixture.otherRegionId] != nil || locations[fixture.otherCountryId] != nil {
		t.Fatal("metadata-only parents were invented as counted supply")
	}
	initial, err := loadInitialClientLocations(ctx)
	if err != nil || initial == nil || len(initial.Locations) != 1 || initial.Locations[0].LocationId != fixture.countryId || initial.Locations[0].ClientCount != 1 {
		t.Fatalf("initial counted country changed: initial=%+v error=%v", initial, err)
	}
	return locations
}

// A missing link does not change counts, and independent valid links survive.
func testClientLocationParentLinks(t testing.TB, fixture clientLocationParentFixture, locations map[server.Id]*ClientLocation, regionCity bool, countryCity bool, countryRegion bool) {
	t.Helper()
	for _, id := range []server.Id{fixture.cityId, fixture.regionId, fixture.countryId} {
		if locations[id] == nil || locations[id].ClientCount != 1 {
			t.Fatal("an independently counted location lost its exact supply")
		}
	}
	for _, check := range []struct {
		name   string
		counts map[server.Id]int
		child  server.Id
		linked bool
	}{
		{name: "region-to-city", counts: locations[fixture.regionId].TopCityLocationIdCounts, child: fixture.cityId, linked: regionCity},
		{name: "country-to-city", counts: locations[fixture.countryId].TopCityLocationIdCounts, child: fixture.cityId, linked: countryCity},
		{name: "country-to-region", counts: locations[fixture.countryId].TopRegionLocationIdCounts, child: fixture.regionId, linked: countryRegion},
	} {
		want := 0
		if check.linked {
			want = 1
		}
		if len(check.counts) != want || check.counts[check.child] != want {
			t.Errorf("%s link count=%d entries=%d want=%d", check.name, check.counts[check.child], len(check.counts), want)
		}
	}
}

// The city metadata's current region need not be counted by an older rollup.
func TestClientLocationParentUncountedCityRegion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE location SET region_location_id = $2 WHERE location_id = $1`, fixture.cityId, fixture.otherRegionId))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), false, true, true)
	})
}

// SQL permits a NULL city-region pointer; it means no link, not a panic.
func TestClientLocationParentNullCityRegion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE location SET region_location_id = NULL WHERE location_id = $1`, fixture.cityId))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), false, true, true)
	})
}

// A country metadata mismatch must not fabricate supply for that country.
func TestClientLocationParentUncountedCityCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE location SET country_location_id = $2 WHERE location_id = $1`, fixture.cityId, fixture.otherCountryId))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), true, false, true)
	})
}

// Missing city-country metadata does not erase its valid region link.
func TestClientLocationParentNullCityCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE location SET country_location_id = NULL WHERE location_id = $1`, fixture.cityId))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), true, false, true)
	})
}

// Regions have the same independently filtered country-parent boundary.
func TestClientLocationParentUncountedRegionCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE location SET country_location_id = $2 WHERE location_id = $1`, fixture.regionId, fixture.otherCountryId))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), true, true, false)
	})
}

// A NULL region-country pointer is not grounds to abort the whole fleet cache.
func TestClientLocationParentNullRegionCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE location SET country_location_id = NULL WHERE location_id = $1`, fixture.regionId))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), true, true, false)
	})
}

// A referenced row can also be physically absent; no replacement count is made.
func TestClientLocationParentMissingRegionRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM location WHERE location_id = $1`, fixture.regionId))
		})
		locations := testClientLocationParentPublish(ctx, t, fixture)
		if len(locations) != 2 || locations[fixture.regionId] != nil || locations[fixture.cityId] == nil || locations[fixture.countryId] == nil || locations[fixture.cityId].ClientCount != 1 || locations[fixture.countryId].ClientCount != 1 {
			t.Fatal("missing region discarded valid child/country or manufactured a parent")
		}
		country := locations[fixture.countryId]
		if len(country.TopCityLocationIdCounts) != 1 || country.TopCityLocationIdCounts[fixture.cityId] != 1 || len(country.TopRegionLocationIdCounts) != 0 {
			t.Fatal("valid country-city link lost or absent region linked")
		}
	})
}

// Complete counted hierarchy retains its three existing links and exact counts.
func TestClientLocationParentHealthyHierarchy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), true, true, true)
	})
}

// An uncounted malformed child stays absent and does not affect counted supply.
func TestClientLocationParentUncountedMalformedChild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := testClientLocationParentFixture(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO location (location_id, location_type, location_name, country_code, location_full_name)
				VALUES ($1, 'city', 'Uncounted Synthetic City', 'zz', 'Uncounted Synthetic City, zz')
			`, server.NewId()))
		})
		testClientLocationParentLinks(t, fixture, testClientLocationParentPublish(ctx, t, fixture), true, true, true)
	})
}

// Country-only rollups retain distinct-ID counting without needing region/city.
func TestClientLocationParentCountryOnlyControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, []byte("enable_egress_test: false\n")))
		countryId := server.NewId()
		Testing_CreateProviderAtLocation(ctx, server.NewId(), server.NewId(), countryId, "zz")
		var updateErr error
		if panicValue := server.HandleError(func() { updateErr = UpdateClientLocations(ctx, time.Hour) }); panicValue != nil || updateErr != nil {
			t.Fatalf("country-only publication: panic=%v error=%v", panicValue, updateErr)
		}
		locations, err := loadClientLocations(ctx, map[server.Id]bool{countryId: true})
		if err != nil || len(locations) != 1 || locations[countryId] == nil || locations[countryId].ClientCount != 1 || len(locations[countryId].TopCityLocationIdCounts) != 0 || len(locations[countryId].TopRegionLocationIdCounts) != 0 {
			t.Fatalf("country fallback counting changed: locations=%+v error=%v", locations, err)
		}
	})
}
