package model

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestCentroidFor(t *testing.T) {
	// region match (California)
	lat, lon, ok := centroidFor("US", "California")
	assert.Equal(t, ok, true)
	if lat < 30 || lat > 43 || lon > -110 || lon < -125 {
		t.Fatalf("California centroid out of range: %f,%f", lat, lon)
	}

	// native-name match: Bavaria is also stored as Bayern
	_, _, ok = centroidFor("DE", "Bavaria")
	assert.Equal(t, ok, true)

	// unknown region falls back to the country centroid
	_, _, ok = centroidFor("US", "Nonexistent Region ZZ")
	assert.Equal(t, ok, true)

	// unknown country -> not ok
	_, _, ok = centroidFor("ZZ", "Nowhere")
	assert.Equal(t, ok, false)

	// case- and space-insensitive lookup
	_, _, ok = centroidFor("us", "  california ")
	assert.Equal(t, ok, true)
}

func TestBuildProvidersMap(t *testing.T) {
	rows := []regionProviderCount{
		{"US", "California", 1200},
		{"US", "New York", 820},
		{"DE", "Bavaria", 540},
		{"US", "California", 100}, // duplicate -> summed
		{"US", "", 5},             // empty region -> skipped
		{"US", "Ghostland", 7},    // unknown region -> country-centroid fallback (kept)
		{"ZZ", "Nowhere", 3},      // unknown country -> skipped
		{"US", "New York", -1},    // non-positive -> skipped
	}
	m := buildProvidersMap(rows)

	us, ok := m["US"]
	assert.Equal(t, ok, true)
	if us["California"] == nil || us["New York"] == nil || us["Ghostland"] == nil {
		t.Fatal("expected US California, New York, Ghostland entries")
	}
	assert.Equal(t, us["California"].ProviderCount, 1300) // 1200 + 100
	assert.Equal(t, us["New York"].ProviderCount, 820)    // -1 duplicate skipped
	assert.Equal(t, us["Ghostland"].ProviderCount, 7)     // country fallback
	if us["California"].Lat == 0 && us["California"].Lon == 0 {
		t.Fatal("California coordinates missing")
	}

	de, ok := m["DE"]
	assert.Equal(t, ok, true)
	assert.Equal(t, de["Bavaria"].ProviderCount, 540)

	// empty region and unknown country produce no entries
	if _, present := us[""]; present {
		t.Fatal("empty region should be skipped")
	}
	if _, present := m["ZZ"]; present {
		t.Fatal("unknown country should be skipped")
	}
}

// TestGetProvidersMapAggregatesRegions drives the real aggregation end to end
// against the db: seed regions and reliability rows, export, and read back the
// blob the /stats/providers-map route would serve.
//
// This exists because the first version of GetProvidersMap read the
// InitialClientLocations snapshot and filtered it for region rows — but that
// snapshot only ever contains country rows, so the exported map was `{}` in
// every environment and the /ip coverage globe sat on its loading state
// forever. This test would have caught that: it asserts actual regions come
// out the other end.
func TestGetProvidersMapAggregatesRegions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		nsw := &Location{
			LocationType: LocationTypeRegion,
			Region:       "New South Wales",
			Country:      "Australia",
			CountryCode:  "au",
		}
		CreateLocation(ctx, nsw)
		vic := &Location{
			LocationType: LocationTypeRegion,
			Region:       "Victoria",
			Country:      "Australia",
			CountryCode:  "au",
		}
		CreateLocation(ctx, vic)

		// one provider in a region: a connected, VALID location-reliability row
		// (valid is GENERATED: country set + one address hash + one location)
		// plus a lookback-0 score row, mirroring what the reliability pipeline
		// writes.
		addProvider := func(region *Location, connected bool, scored bool) {
			clientId := server.NewId()
			networkId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client_location_reliability (
							client_id,
							network_id,
							update_block_number,
							region_location_id,
							country_location_id,
							client_address_hash_count,
							location_count,
							connected
						)
						VALUES ($1, $2, 1, $3, $4, 1, 1, $5)
					`,
					clientId,
					networkId,
					region.LocationId,
					region.CountryLocationId,
					connected,
				))
				if scored {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
							INSERT INTO client_connection_reliability_score (
								client_id,
								lookback_index,
								independent_reliability_score,
								independent_reliability_weight,
								reliability_score,
								reliability_weight,
								min_block_number,
								max_block_number,
								region_location_id,
								country_location_id
							)
							VALUES ($1, 0, 1, 1, 1, 1, 1, 1, $2, $3)
						`,
						clientId,
						region.LocationId,
						region.CountryLocationId,
					))
				}
			})
		}

		addProvider(nsw, true, true)
		addProvider(nsw, true, true)
		addProvider(vic, true, true)
		addProvider(vic, false, true) // disconnected: must not count
		addProvider(vic, true, false) // never scored: must not count

		// the full chain the taskworker runs: aggregate -> export -> serve
		err := ExportProvidersMap(ctx)
		assert.Equal(t, err, nil)

		exportedJson := GetExportedProvidersMapJson(ctx)
		assert.NotEqual(t, exportedJson, nil)

		var exported map[string]map[string]*RegionProviders
		assert.Equal(t, json.Unmarshal([]byte(*exportedJson), &exported), nil)

		au := exported["au"]
		assert.NotEqual(t, au, nil)
		assert.Equal(t, au["New South Wales"].ProviderCount, 2)
		assert.Equal(t, au["Victoria"].ProviderCount, 1)

		// centroids attached from the embedded dataset — this is what places
		// the dots on the globe
		assert.Equal(t, true, au["New South Wales"].Lat < -28 && au["New South Wales"].Lat > -38)
		assert.Equal(t, true, au["New South Wales"].Lon > 140)
	})
}
