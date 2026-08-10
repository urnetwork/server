package model

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestCentroidFor(t *testing.T) {
	// region match (California)
	lat, lon, ok := centroidFor("US", "California")
	connect.AssertEqual(t, ok, true)
	if lat < 30 || lat > 43 || lon > -110 || lon < -125 {
		t.Fatalf("California centroid out of range: %f,%f", lat, lon)
	}

	// native-name match: Bavaria is also stored as Bayern
	_, _, ok = centroidFor("DE", "Bavaria")
	connect.AssertEqual(t, ok, true)

	// unknown region falls back to the country centroid
	_, _, ok = centroidFor("US", "Nonexistent Region ZZ")
	connect.AssertEqual(t, ok, true)

	// unknown country -> not ok
	_, _, ok = centroidFor("ZZ", "Nowhere")
	connect.AssertEqual(t, ok, false)

	// case- and space-insensitive lookup
	_, _, ok = centroidFor("us", "  california ")
	connect.AssertEqual(t, ok, true)
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
	connect.AssertEqual(t, ok, true)
	if us["California"] == nil || us["New York"] == nil || us["Ghostland"] == nil {
		t.Fatal("expected US California, New York, Ghostland entries")
	}
	connect.AssertEqual(t, us["California"].ProviderCount, 1300) // 1200 + 100
	connect.AssertEqual(t, us["New York"].ProviderCount, 820)    // -1 duplicate skipped
	connect.AssertEqual(t, us["Ghostland"].ProviderCount, 7)     // country fallback
	if us["California"].Lat == 0 && us["California"].Lon == 0 {
		t.Fatal("California coordinates missing")
	}

	de, ok := m["DE"]
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, de["Bavaria"].ProviderCount, 540)

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
			// the map counts only providers holding a Public provide key (see
			// TestProviderStatsCountOnlyPublicProviders); every provider this
			// helper builds is meant to count, so give them all one
			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: []byte("public-secret"),
			})
		}

		addProvider(nsw, true, true)
		addProvider(nsw, true, true)
		addProvider(vic, true, true)
		addProvider(vic, false, true) // disconnected: must not count
		addProvider(vic, true, false) // never scored: must not count

		// the full chain the taskworker runs: aggregate -> export -> serve
		err := ExportProvidersMap(ctx)
		connect.AssertEqual(t, err, nil)

		exportedJson := GetExportedProvidersMapJson(ctx)
		connect.AssertNotEqual(t, exportedJson, nil)

		var exported map[string]map[string]*RegionProviders
		connect.AssertEqual(t, json.Unmarshal([]byte(*exportedJson), &exported), nil)

		au := exported["au"]
		connect.AssertNotEqual(t, au, nil)
		connect.AssertEqual(t, au["New South Wales"].ProviderCount, 2)
		connect.AssertEqual(t, au["Victoria"].ProviderCount, 1)

		// centroids attached from the embedded dataset — this is what places
		// the dots on the globe
		connect.AssertEqual(t, true, au["New South Wales"].Lat < -28 && au["New South Wales"].Lat > -38)
		connect.AssertEqual(t, true, au["New South Wales"].Lon > 140)
	})
}

// Both public provider numbers must answer the same question the public
// provider *list* answers: how much supply a stranger can actually pick.
//
// `/network/provider-locations` (UpdateClientLocations) counts only providers
// holding a Public provide key -- GetProvideRelationship returns
// ProvideModePublic for a cross-network pair, so without a Public key a
// provider can only ever serve its own network and is effectively private.
// `/stats/providers-map` and CountProviderCountries applied no provide-mode
// filter at all, so they reported supply that no user outside the provider's
// own network could select: the map and the country count would both exceed
// the list, in the same deployment, for the same population.
//
// Deleting either EXISTS predicate must fail this test.
func TestProviderStatsCountOnlyPublicProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// two countries so the country count distinguishes them: the Public
		// provider is the only one in Australia, the Network-only provider the
		// only one in Japan. A correct count is 1 country, not 2.
		nsw := &Location{
			LocationType: LocationTypeRegion,
			Region:       "New South Wales",
			Country:      "Australia",
			CountryCode:  "au",
		}
		CreateLocation(ctx, nsw)
		tokyo := &Location{
			LocationType: LocationTypeRegion,
			Region:       "Tokyo",
			Country:      "Japan",
			CountryCode:  "jp",
		}
		CreateLocation(ctx, tokyo)

		// a connected, valid, scored provider in `region` holding exactly the
		// provide modes given (nil for none at all)
		addProvider := func(region *Location, modes map[ProvideMode][]byte) {
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
						VALUES ($1, $2, 1, $3, $4, 1, 1, true)
					`,
					clientId,
					networkId,
					region.LocationId,
					region.CountryLocationId,
				))
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
			})
			if modes != nil {
				SetProvide(ctx, clientId, modes)
			}
		}

		// serves strangers -- must be counted by both surfaces
		addProvider(nsw, map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})
		// own network only -- must NOT be counted by either surface
		addProvider(tokyo, map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-secret"),
		})
		// no provide key at all -- must NOT be counted by either surface
		addProvider(tokyo, nil)

		// surface 1: /stats/providers-map
		providersMap, err := GetProvidersMap(ctx)
		connect.AssertEqual(t, err, nil)

		au := providersMap["au"]
		connect.AssertNotEqual(t, au, nil)
		connect.AssertEqual(t, au["New South Wales"].ProviderCount, 1)

		// Japan's only supply is network-only/keyless, so it must not appear on
		// the public map at all -- not as a region with a zero count, and not
		// as a country
		// Errorf, not Fatalf: the country count below is the *other* surface
		// this test covers, and a run that stopped here would never exercise
		// it -- both predicates need to be seen failing independently.
		if jp, found := providersMap["jp"]; found {
			t.Errorf("providers map contains jp = %v; its only providers are network-only/keyless and unreachable to any user outside their own network", jp)
		}

		// surface 2: the /stats country count
		countries := CountProviderCountries(ctx)
		if countries != 1 {
			t.Fatalf("CountProviderCountries() = %d, want 1 (only au has a Public provider; jp's are network-only/keyless)", countries)
		}
	})
}
