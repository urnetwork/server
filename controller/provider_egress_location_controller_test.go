// Tests of the probed-exit ingest (connect/GEOMAP.md §11.3): the prober
// submits the address the operator's /ip echo saw, the server places it with
// its own GeoLite2, stores the place and never the address, and refuses the
// retired vendor-consensus shape. The addresses are the MaxMind test-database
// ones the suite already places (24.48.0.1 in Montreal, 67.43.156.1
// country-only in the United States).
package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The exit address a city-precise GeoLite2 record covers: Montreal, a 5 km
// radius.
const testEgressCityExitIp = "24.48.0.1"

// The exit address a country-only record covers: the United States. It is
// MaxMind's published test data, which the GeoLite2 build places only in its
// country, at a country's radius and with no region or city.
const testEgressCountryExitIp = "67.43.156.1"

// A provider with a device and a network, as the ingest's unknown-client check
// needs.
func testEgressProvider(ctx context.Context) server.Id {
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
	return clientId
}

// The stored row as text, every column, so a test can prove what it does not
// hold.
func testEgressLocationRowText(ctx context.Context, clientId server.Id) string {
	var text string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT to_jsonb(provider_egress_location)::text FROM provider_egress_location WHERE client_id = $1`,
			clientId,
		).Scan(&text))
	})
	return text
}

// A synthetic GeoLite2 record: a city with a radius, in a region and a country.
func testEgressIpInfo(radiusKm int) *server.IpInfo {
	return &server.IpInfo{
		ContinentCode:    "eu",
		Continent:        "Europe",
		CountryCode:      "fr",
		Country:          "France",
		Region:           "Île-de-France",
		Regions:          []string{"Île-de-France"},
		City:             "Paris",
		AccuracyRadiusKm: radiusKm,
		CityGeonameId:    1,
		RegionGeonameId:  2,
		CountryGeonameId: 3,
	}
}

// A place is a city exactly when GeoLite2 names one within the confident
// radius; a wider one is its region, and a record without a region its
// country.
func TestProviderEgressExitFromIpInfoCityConfidence(t *testing.T) {
	cases := []struct {
		radiusKm      int
		limitKm       int
		cityConfident bool
		locationType  model.LocationType
	}{
		{radiusKm: 20, limitKm: 25, cityConfident: true, locationType: model.LocationTypeCity},
		{radiusKm: 25, limitKm: 25, cityConfident: true, locationType: model.LocationTypeCity},
		{radiusKm: 26, limitKm: 25, cityConfident: false, locationType: model.LocationTypeRegion},
		{radiusKm: 0, limitKm: 25, cityConfident: false, locationType: model.LocationTypeRegion},
		{radiusKm: 20, limitKm: 19, cityConfident: false, locationType: model.LocationTypeRegion},
	}
	for _, c := range cases {
		exit, err := providerEgressExitFromIpInfo(testEgressIpInfo(c.radiusKm), c.limitKm)
		if err != nil {
			t.Fatalf("radius %d limit %d: %v", c.radiusKm, c.limitKm, err)
		}
		if exit.CityConfident != c.cityConfident || exit.Location.LocationType != c.locationType {
			t.Errorf("radius %d limit %d: city_confident=%t type=%s, want %t %s", c.radiusKm, c.limitKm, exit.CityConfident, exit.Location.LocationType, c.cityConfident, c.locationType)
		}
		if exit.CountryCode != "fr" || exit.AccuracyRadiusKm != c.radiusKm {
			t.Errorf("radius %d: country=%q radius=%d", c.radiusKm, exit.CountryCode, exit.AccuracyRadiusKm)
		}
		if !c.cityConfident && (exit.Location.City != "" || exit.Location.CityGeonameId != 0) {
			t.Errorf("radius %d: a place too coarse for its city kept the city %+v", c.radiusKm, exit.Location)
		}
	}

	countryOnly := testEgressIpInfo(1000)
	countryOnly.Region = ""
	countryOnly.Regions = nil
	countryOnly.City = ""
	exit, err := providerEgressExitFromIpInfo(countryOnly, 25)
	if err != nil || exit.Location.LocationType != model.LocationTypeCountry || exit.CityConfident {
		t.Fatalf("a country-only record = %+v, %v; want the country", exit, err)
	}

	noCountry := testEgressIpInfo(5)
	noCountry.CountryCode = ""
	if _, err := providerEgressExitFromIpInfo(noCountry, 25); err == nil {
		t.Fatal("a record with no country was placed")
	}
}

// The lookup the ingest uses resolves a documented test address, and refuses
// an address that does not parse or that GeoLite2 places nowhere.
func TestResolveProviderEgressExitLooksUpGeoLite2(t *testing.T) {
	exit, err := ResolveProviderEgressExit(testEgressCityExitIp, 25)
	if err != nil {
		t.Fatal(err)
	}
	if exit.CountryCode != "ca" || !exit.CityConfident || exit.Location.City != "Montreal" {
		t.Fatalf("%s placed at %+v, want Montreal, city-confident", testEgressCityExitIp, exit)
	}
	for _, exitIp := range []string{"not-an-address", "", "192.0.2.1"} {
		if _, err := ResolveProviderEgressExit(exitIp, 25); err == nil {
			t.Errorf("%q was placed", exitIp)
		}
	}
}

// The ingest stores the GeoLite2 place of the exit -- its city row when the
// radius allows, the country, city_confident -- and never the address.
func TestSubmitProviderEgressLocationStoresTheGeoLite2PlaceAndNoAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := testEgressProvider(ctx)

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:   clientId,
			ExitIp:     testEgressCityExitIp,
			ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.LocationId, res.LocationId)
		connect.AssertEqual(t, stored.CountryCode, "ca")
		connect.AssertEqual(t, stored.CityConfident, true)
		location := model.GetLocation(ctx, stored.LocationId)
		if location == nil || location.LocationType != model.LocationTypeCity || location.City != "Montreal" {
			t.Fatalf("stored location = %+v, want the Montreal city row", location)
		}
		connect.AssertEqual(t, stored.Verdict, "verified")

		// the same row the lookup on a connection address creates, so a
		// probed and a looked-up provider share it
		lookup, _, err := GetLocationForIp(ctx, testEgressCityExitIp)
		connect.AssertEqual(t, err, nil)
		model.CreateLocation(ctx, lookup)
		connect.AssertEqual(t, lookup.LocationId, stored.LocationId)

		if text := testEgressLocationRowText(ctx, clientId); strings.Contains(text, testEgressCityExitIp) {
			t.Fatalf("the stored row holds the exit address: %s", text)
		}
	})
}

// A radius wider than the configured city radius stores the region, and a
// country-only record the country.
func TestSubmitProviderEgressLocationCoarserPlaces(t *testing.T) {
	// the Montreal record's radius is 5 km, so a 4 km city radius makes it a
	// region-precise exit
	pop := server.Config.PushSimpleResource(model.ProviderEgressProbeResourceName, []byte("city_confident_radius_km: 4\n"))
	defer pop()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		regionClientId := testEgressProvider(ctx)
		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: regionClientId, ExitIp: testEgressCityExitIp, ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		stored := model.GetProviderEgressLocation(ctx, regionClientId)
		connect.AssertEqual(t, stored.CityConfident, false)
		if location := model.GetLocation(ctx, stored.LocationId); location == nil || location.LocationType != model.LocationTypeRegion {
			t.Fatalf("a wide-radius exit stored at %+v, want its region", location)
		}

		countryClientId := testEgressProvider(ctx)
		_, err = SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: countryClientId, ExitIp: testEgressCountryExitIp, ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		stored = model.GetProviderEgressLocation(ctx, countryClientId)
		connect.AssertEqual(t, stored.CountryCode, "us")
		connect.AssertEqual(t, stored.CityConfident, false)
		if location := model.GetLocation(ctx, stored.LocationId); location == nil || location.LocationType != model.LocationTypeCountry {
			t.Fatalf("a country-only exit stored at %+v, want the country", location)
		}
	})
}

// The retired vendor-consensus shape -- a country and no exit address -- is
// refused, whatever it asserts.
func TestSubmitProviderEgressLocationRefusesTheConsensusShape(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := testEgressProvider(ctx)

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil || !strings.Contains(err.Error(), "exit_ip") {
			t.Fatalf("err = %v, want a refusal naming exit_ip", err)
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("a refused submission was stored")
		}
	})
}

// The legacy fields are accepted beside an exit address and ignored: the
// country is GeoLite2's, and no verdict of a vendor's is stored.
func TestSubmitProviderEgressLocationIgnoresTheRetiredFields(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := testEgressProvider(ctx)

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			ExitIp:           testEgressCountryExitIp,
			ObservedAt:       server.NowUtc(),
			CountryCode:      "jp",
			Country:          "Japan",
			CountryConfident: true,
			ASN:              64500,
			Org:              "Synthetic Org",
			Hosting:          true,
			Proxy:            true,
			Mobile:           true,
		})
		connect.AssertEqual(t, err, nil)
		stored := model.GetProviderEgressLocation(ctx, clientId)
		connect.AssertEqual(t, stored.CountryCode, "us")
		connect.AssertEqual(t, stored.ASN, 0)
		connect.AssertEqual(t, stored.Org, "")
		if text := testEgressLocationRowText(ctx, clientId); strings.Contains(text, `"hosting": true`) ||
			strings.Contains(text, `"proxy": true`) || strings.Contains(text, `"mobile": true`) {
			t.Fatalf("a retired verdict was stored: %s", text)
		}
	})
}

// An address that does not parse, or one GeoLite2 places nowhere, is refused
// and nothing is stored.
func TestSubmitProviderEgressLocationRefusesAnUnplaceableExit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, exitIp := range []string{"not-an-address", "192.0.2.1"} {
			clientId := testEgressProvider(ctx)
			_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId: clientId, ExitIp: exitIp, ObservedAt: server.NowUtc(),
			})
			if err == nil {
				t.Errorf("%q was accepted", exitIp)
			} else if strings.Contains(err.Error(), exitIp) {
				t.Errorf("the refusal repeats the address: %v", err)
			}
			if model.GetProviderEgressLocation(ctx, clientId) != nil {
				t.Errorf("%q was stored", exitIp)
			}
		}
	})
}

// An unknown client is refused before any lookup is stored.
func TestSubmitProviderEgressLocationRejectsUnknownClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:   server.NewId(),
			ExitIp:     testEgressCityExitIp,
			ObservedAt: server.NowUtc(),
		})
		if err == nil {
			t.Fatal("unknown client_id must be rejected")
		}
	})
}

// Observation times outside the accepted window are refused: long past (a
// replay), or far in the future (which would win every later upsert); within
// the skew they are accepted.
func TestSubmitProviderEgressLocationObservedAtWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases := []struct {
			offset   time.Duration
			accepted bool
		}{
			{offset: -30 * 24 * time.Hour, accepted: false},
			{offset: 10 * 365 * 24 * time.Hour, accepted: false},
			{offset: time.Minute, accepted: true},
			{offset: -time.Hour, accepted: true},
		}
		for _, c := range cases {
			clientId := testEgressProvider(ctx)
			_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId: clientId, ExitIp: testEgressCityExitIp, ObservedAt: server.NowUtc().Add(c.offset),
			})
			if (err == nil) != c.accepted {
				t.Errorf("observed %s from now: err=%v, want accepted=%t", c.offset, err, c.accepted)
			}
			if (model.GetProviderEgressLocation(ctx, clientId) != nil) != c.accepted {
				t.Errorf("observed %s from now: stored mismatch", c.offset)
			}
		}
	})
}

// The country is GeoLite2's own answer for the exit, so it is always
// confident: a first probe with no history is `verified`, never the
// consensus-era `unverified`/`no_consensus`.
func TestSubmitProviderEgressLocationVerdictGeoLite2IsConfident(t *testing.T) {
	verdict := providerEgressVerdict("us", nil)
	if verdict.State != "verified" {
		t.Errorf("state = %q, want %q", verdict.State, "verified")
	}
	if verdict.Reason != "" {
		t.Errorf("reason = %q, want empty", verdict.Reason)
	}
}

// A country that flips inside probeverdict's instability window is suspect.
// The two probes go backwards in time, since a future observed_at is refused.
func TestSubmitProviderEgressLocationVerdictSuspectOnCountryFlipFlop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := testEgressProvider(ctx)

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: clientId, ExitIp: testEgressCountryExitIp, ObservedAt: server.NowUtc().Add(-2 * time.Hour),
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, model.GetProviderEgressLocation(ctx, clientId).Verdict, "verified")

		_, err = SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: clientId, ExitIp: testEgressCityExitIp, ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		stored := model.GetProviderEgressLocation(ctx, clientId)
		connect.AssertEqual(t, stored.CountryCode, "ca")
		connect.AssertEqual(t, stored.Verdict, "suspect")
		connect.AssertEqual(t, stored.VerdictReason, "unstable")
	})
}

// The property probing exists for: an exit placed in a country other than the
// one the provider's control address looks up in is the finding, not a fault,
// and is verified.
func TestSubmitProviderEgressLocationVerdictLookupDivergenceIsNotSuspect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := testEgressProvider(ctx)

		// the control address looks up in Canada
		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, testEgressCityExitIp+":0", handlerId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, SetConnectionLocation(ctx, connectionId, testEgressCityExitIp), nil)

		// the exit is placed in the United States
		_, err = SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: clientId, ExitIp: testEgressCountryExitIp, ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		stored := model.GetProviderEgressLocation(ctx, clientId)
		connect.AssertEqual(t, stored.CountryCode, "us")
		connect.AssertEqual(t, stored.Verdict, "verified")
		connect.AssertEqual(t, stored.VerdictReason, "")
	})
}
