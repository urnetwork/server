package geo

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// Tests of the place list export: the weighted representative and its
// tie-breaks, the tallies, name collisions, and the marshaled file.

// a network of London, England unless a test overrides a field
func testNetwork(edit func(network *ExportNetwork)) *ExportNetwork {
	network := &ExportNetwork{
		CityGeonameId:    2643743,
		City:             "London",
		RegionGeonameId:  6269131,
		Region:           "England",
		CountryCode:      "GB",
		CountryGeonameId: 2635167,
		Country:          "United Kingdom",
		ContinentCode:    "EU",
		Continent:        "Europe",
		HasCoordinates:   true,
		Latitude:         51.5081,
		Longitude:        -0.1278,
		AccuracyRadiusKm: 10,
		TimeZone:         "Europe/London",
	}
	if edit != nil {
		edit(network)
	}
	return network
}

// Exports what add adds, and loads the marshaled file back.
func testExport(t *testing.T, add func(exporter *Exporter)) (*Export, *Places) {
	t.Helper()
	exporter := NewExporter()
	add(exporter)
	export, err := exporter.Export("test", 1790110762)
	if err != nil {
		t.Fatal(err)
	}
	placesBytes, err := export.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	places, err := LoadPlaces(placesBytes)
	if err != nil {
		t.Fatalf("the export does not load: %s\n%s", err, placesBytes)
	}
	return export, places
}

// Fails the test unless a place is at exactly the wanted coordinate.
func assertCoordinate(t *testing.T, place *Place, latitude float64, longitude float64) {
	t.Helper()
	if place == nil {
		t.Fatal("no place")
	}
	if place.Latitude != latitude || place.Longitude != longitude {
		t.Fatalf("%s is at (%v, %v), want (%v, %v)", place.City, place.Latitude, place.Longitude, latitude, longitude)
	}
}

// The representative coordinate is the one the most networks carry, whatever
// their radius.
func TestExportRepresentativeIsTheWeightedMode(t *testing.T) {
	_, places := testExport(t, func(exporter *Exporter) {
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.5, -0.1, 5
		}), 3)
		// the most networks, despite the widest radius
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.6, -0.2, 100
		}), 5)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.4, -0.3, 1
		}), 2)
		// the same coordinate again, from another record
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.5, -0.1, 5
		}), 1)
	})
	assertCoordinate(t, places.CityByGeonameId(2643743), 51.6, -0.2)
}

// Networks at one coordinate weigh together across accuracy radii.
func TestExportCoordinateWeighsAcrossRadii(t *testing.T) {
	// one coordinate carried by 3 + 3 networks of different radii outweighs
	// another carried by 5 networks of one radius
	_, places := testExport(t, func(exporter *Exporter) {
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.5, -0.1, 5
		}), 3)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.5, -0.1, 200
		}), 3)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.Latitude, network.Longitude, network.AccuracyRadiusKm = 51.6, -0.2, 1
		}), 5)
	})
	assertCoordinate(t, places.CityByGeonameId(2643743), 51.5, -0.1)
}

// A tie in network count resolves by radius, then latitude, then longitude,
// in either insertion order.
func TestExportRepresentativeTieBreaks(t *testing.T) {
	for _, test := range []struct {
		name      string
		variants  []ExportNetwork
		latitude  float64
		longitude float64
	}{
		{
			name: "the smaller accuracy radius",
			variants: []ExportNetwork{
				{Latitude: 51.1, Longitude: -0.1, AccuracyRadiusKm: 20},
				{Latitude: 51.9, Longitude: -0.9, AccuracyRadiusKm: 10},
			},
			latitude:  51.9,
			longitude: -0.9,
		},
		{
			name: "a known radius before an unknown one",
			variants: []ExportNetwork{
				{Latitude: 51.1, Longitude: -0.1, AccuracyRadiusKm: 0},
				{Latitude: 51.9, Longitude: -0.9, AccuracyRadiusKm: 1000},
			},
			latitude:  51.9,
			longitude: -0.9,
		},
		{
			name: "then the smaller latitude",
			variants: []ExportNetwork{
				{Latitude: 51.9, Longitude: -0.9, AccuracyRadiusKm: 10},
				{Latitude: 51.1, Longitude: -0.1, AccuracyRadiusKm: 10},
			},
			latitude:  51.1,
			longitude: -0.1,
		},
		{
			name: "then the smaller longitude",
			variants: []ExportNetwork{
				{Latitude: 51.5, Longitude: -0.1, AccuracyRadiusKm: 10},
				{Latitude: 51.5, Longitude: -0.9, AccuracyRadiusKm: 10},
			},
			latitude:  51.5,
			longitude: -0.9,
		},
	} {
		// both orders, so the tie-break cannot be the insertion order
		for _, reverse := range []bool{false, true} {
			_, places := testExport(t, func(exporter *Exporter) {
				for i := range test.variants {
					variant := test.variants[i]
					if reverse {
						variant = test.variants[len(test.variants)-1-i]
					}
					exporter.Add(testNetwork(func(network *ExportNetwork) {
						network.Latitude, network.Longitude, network.AccuracyRadiusKm = variant.Latitude, variant.Longitude, variant.AccuracyRadiusKm
					}), 4)
				}
			})
			place := places.CityByGeonameId(2643743)
			if place.Latitude != test.latitude || place.Longitude != test.longitude {
				t.Fatalf("%s (reverse %t): (%v, %v), want (%v, %v)", test.name, reverse, place.Latitude, place.Longitude, test.latitude, test.longitude)
			}
		}
	}
}

// The spread is the farthest variant from the representative, written to
// 0.1 km, and the summary counts the multi-coordinate cities.
func TestExportSpread(t *testing.T) {
	export, places := testExport(t, func(exporter *Exporter) {
		// one coordinate: no spread
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 1, "One"
			network.Latitude, network.Longitude = 10, 10
		}), 7)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 1, "One"
			network.Latitude, network.Longitude = 10, 10
			network.AccuracyRadiusKm = 500
		}), 2)

		// the representative (0, 0), and variants 1° and 2° away: the spread
		// is the farthest variant from the representative
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 2, "Two"
			network.Latitude, network.Longitude = 0, 0
		}), 5)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 2, "Two"
			network.Latitude, network.Longitude = 0, 1
		}), 2)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 2, "Two"
			network.Latitude, network.Longitude = 0, -2
		}), 1)
	})

	if spread := places.CityByGeonameId(1).SpreadKm; spread != 0 {
		t.Fatalf("one coordinate: spread %v, want 0", spread)
	}
	// written to 0.1 km
	want := math.Round(DistanceKm(0, 0, 0, -2)*10) / 10
	if spread := places.CityByGeonameId(2).SpreadKm; spread != want {
		t.Fatalf("spread %v, want %v", spread, want)
	}
	if export.Summary.MultiCoordinateCityCount != 1 {
		t.Fatalf("MultiCoordinateCityCount = %d, want 1", export.Summary.MultiCoordinateCityCount)
	}
	if 0.001 < math.Abs(export.Summary.MaxSpreadKm-DistanceKm(0, 0, 0, -2)) {
		t.Fatalf("MaxSpreadKm = %v", export.Summary.MaxSpreadKm)
	}
}

// The time zone is the weighted mode, a tie goes to the lexically smallest,
// and a network with no zone casts no vote.
func TestExportTimeZoneMode(t *testing.T) {
	_, places := testExport(t, func(exporter *Exporter) {
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 1, "Weighted", "Europe/London"
		}), 3)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 1, "Weighted", "Europe/Dublin"
		}), 2)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 1, "Weighted", "Europe/Dublin"
		}), 2)
		// a network without a zone is no vote
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 1, "Weighted", ""
		}), 10)

		// a tie resolves to the lexically smallest zone
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 2, "Tied", "Europe/London"
		}), 2)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 2, "Tied", "Europe/Dublin"
		}), 2)

		// no zone at all
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City, network.TimeZone = 3, "Zoneless", ""
		}), 2)
	})
	for geonameId, want := range map[uint32]string{1: "Europe/Dublin", 2: "Europe/Dublin", 3: ""} {
		if timeZone := places.CityByGeonameId(geonameId).TimeZone; timeZone != want {
			t.Fatalf("city %d time zone %q, want %q", geonameId, timeZone, want)
		}
	}
}

// A city under no subdivision, or an unnamed one, is filed under a region
// named for its country, with no region id.
func TestExportRegionlessCity(t *testing.T) {
	export, places := testExport(t, func(exporter *Exporter) {
		// GeoLite2 files Singapore's cities under no subdivision
		exporter.Add(&ExportNetwork{
			CityGeonameId:    1884382,
			City:             "Bedok New Town",
			CountryCode:      "SG",
			CountryGeonameId: 1880251,
			Country:          "Singapore",
			ContinentCode:    "AS",
			Continent:        "Asia",
			HasCoordinates:   true,
			Latitude:         1.3264,
			Longitude:        103.9394,
			TimeZone:         "Asia/Singapore",
		}, 4)
		// a subdivision without an English name cannot be filed by name
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 7, "Unnamed Subdivision Town"
			network.RegionGeonameId, network.Region = 99, ""
		}), 1)
	})

	place := places.CityByGeonameId(1884382)
	if place.Region != "Singapore" || place.RegionGeonameId != 0 || place.CountryCode != "sg" {
		t.Fatalf("Bedok New Town = %+v, want region Singapore with no region id", place)
	}
	place = places.CityByGeonameId(7)
	if place.Region != "United Kingdom" || place.RegionGeonameId != 0 {
		t.Fatalf("a city under an unnamed subdivision = %+v, want the country-named region", place)
	}
	if export.Summary.RegionlessCityCount != 2 {
		t.Fatalf("RegionlessCityCount = %d, want 2", export.Summary.RegionlessCityCount)
	}
}

// Cities of one name in one region keep the name for the smallest id and take
// the geoname suffix otherwise.
func TestExportNameCollisions(t *testing.T) {
	export, places := testExport(t, func(exporter *Exporter) {
		// three cities of one name in one region: the smallest id keeps the
		// name, whatever the order they arrive in
		for _, geonameId := range []uint32{300, 100, 200} {
			exporter.Add(testNetwork(func(network *ExportNetwork) {
				network.CityGeonameId, network.City = geonameId, "Springfield"
				network.Latitude = 51 + float64(geonameId)/1000
			}), 1)
		}
		// the same name in another region is no collision
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 400, "Springfield"
			network.RegionGeonameId, network.Region = 2641364, "Scotland"
		}), 1)
	})

	for geonameId, want := range map[uint32]string{
		100: "Springfield",
		200: "Springfield (geoname 200)",
		300: "Springfield (geoname 300)",
		400: "Springfield",
	} {
		if city := places.CityByGeonameId(geonameId).City; city != want {
			t.Fatalf("city %d is named %q, want %q", geonameId, city, want)
		}
	}
	if export.Summary.NameCollisionCount != 2 {
		t.Fatalf("NameCollisionCount = %d, want 2", export.Summary.NameCollisionCount)
	}
	if GeonameSuffixedName("Springfield", 200) != "Springfield (geoname 200)" {
		t.Fatal("GeonameSuffixedName")
	}
}

// Subdivisions of one name in one country follow the same rule as cities.
func TestExportRegionNameCollisions(t *testing.T) {
	export, places := testExport(t, func(exporter *Exporter) {
		// two subdivisions of one country with one English name
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 1, "North Town"
			network.RegionGeonameId, network.Region = 20, "Central"
		}), 1)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 2, "South Town"
			network.RegionGeonameId, network.Region = 10, "Central"
		}), 1)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 3, "West Town"
			network.RegionGeonameId, network.Region = 20, "Central"
		}), 1)
	})

	for geonameId, want := range map[uint32]string{
		1: "Central (geoname 20)",
		2: "Central",
		3: "Central (geoname 20)",
	} {
		if region := places.CityByGeonameId(geonameId).Region; region != want {
			t.Fatalf("city %d is in %q, want %q", geonameId, region, want)
		}
	}
	if export.Summary.RegionNameCollisionCount != 1 || export.Summary.NameCollisionCount != 0 {
		t.Fatalf("RegionNameCollisionCount = %d, NameCollisionCount = %d", export.Summary.RegionNameCollisionCount, export.Summary.NameCollisionCount)
	}
}

// Every country the database names is exported, including one whose networks
// never resolve below the country.
func TestExportCountriesWithoutCities(t *testing.T) {
	export, places := testExport(t, func(exporter *Exporter) {
		exporter.Add(testNetwork(nil), 1)
		// a network placed only at country precision
		exporter.Add(&ExportNetwork{
			CountryCode:      "DE",
			CountryGeonameId: 2921044,
			Country:          "Germany",
			ContinentCode:    "EU",
			Continent:        "Europe",
			HasCoordinates:   true,
			Latitude:         51.2993,
			Longitude:        9.491,
			AccuracyRadiusKm: 1000,
			TimeZone:         "Europe/Berlin",
		}, 12)
		// a network with no place at all
		exporter.Add(&ExportNetwork{ContinentCode: "EU", Continent: "Europe"}, 3)
	})

	country := places.Country("de")
	if country == nil || country.Name != "Germany" || country.GeonameId != 2921044 || country.ContinentCode != "eu" || country.Continent != "Europe" {
		t.Fatalf("Country(de) = %+v", country)
	}
	if places.Country("gb") == nil || len(places.Countries()) != 2 || places.CityCount() != 1 {
		t.Fatalf("countries %d, cities %d", len(places.Countries()), places.CityCount())
	}
	if export.Summary.NetworkCount != 16 || export.Summary.CityNetworkCount != 1 || export.Summary.CountryCount != 2 || export.Summary.CityCount != 1 {
		t.Fatalf("summary %+v", export.Summary)
	}
}

// A city with no name, no country or no valid coordinate is left out and
// counted.
func TestExportSkipsIncompleteCities(t *testing.T) {
	export, places := testExport(t, func(exporter *Exporter) {
		exporter.Add(testNetwork(nil), 1)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 2, ""
		}), 1)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.CountryCode = 3, ""
		}), 1)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.HasCoordinates = 4, false
		}), 1)
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.Latitude = 5, 123
		}), 1)
	})
	if places.CityCount() != 1 || export.Summary.SkippedCityCount != 4 {
		t.Fatalf("cities %d, skipped %d", places.CityCount(), export.Summary.SkippedCityCount)
	}
}

// the networks of a small world, including names YAML would read as another
// type, or as structure, if they were written plain
func testWorld() []*ExportNetwork {
	worldNetworks := []*ExportNetwork{}
	add := func(geonameId uint32, city string, regionGeonameId uint32, region string, countryCode string, country string, latitude float64, longitude float64) {
		worldNetworks = append(worldNetworks, &ExportNetwork{
			CityGeonameId:    geonameId,
			City:             city,
			RegionGeonameId:  regionGeonameId,
			Region:           region,
			CountryCode:      countryCode,
			CountryGeonameId: geonameId + 1000000,
			Country:          country,
			ContinentCode:    "EU",
			Continent:        "Europe",
			HasCoordinates:   true,
			Latitude:         latitude,
			Longitude:        longitude,
			AccuracyRadiusKm: 20,
			TimeZone:         "Europe/Andorra",
		})
	}
	add(3039181, "Sant Julià de Lòria", 3039162, "Sant Julià de Lòria", "AD", "Andorra", 42.4637, 1.4913)
	add(3041563, "L’Aldosa", 3041566, "La Massana", "AD", "Andorra", 42.5781, 1.5225)
	add(1, "No", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.5)
	add(2, "123", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.51)
	add(3, "true", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.52)
	add(4, "null", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.53)
	add(5, "~", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.54)
	add(6, "1:20", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.55)
	add(7, "Town: the \"Old\" #1, [A]", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.56)
	add(8, "- dash", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.57)
	add(9, "2024-01-01", 3041566, "La Massana", "AD", "Andorra", 42.5, 1.58)
	add(10, "Yes", 11, "on", "AD", "Andorra", 42.5, 1.59)
	add(12, "Zürich", 2657895, "Zurich", "CH", "Switzerland", 47.3667, 8.55)
	add(13, "Tromsø", 13, "Troms", "NO", "Bonaire, Sint Eustatius, and Saba", -0.0001, -179.9999)
	return worldNetworks
}

// Every name of the small world, including those YAML would misread, reads
// back as it was written.
func TestExportRoundTrip(t *testing.T) {
	_, places := testExport(t, func(exporter *Exporter) {
		for _, network := range testWorld() {
			exporter.Add(network, 1)
		}
	})
	for _, network := range testWorld() {
		place := places.CityByGeonameId(network.CityGeonameId)
		if place == nil {
			t.Fatalf("%q did not round trip", network.City)
		}
		if place.City != network.City ||
			place.Region != network.Region ||
			place.RegionGeonameId != network.RegionGeonameId ||
			place.CountryCode != strings.ToLower(network.CountryCode) ||
			place.Latitude != network.Latitude ||
			place.Longitude != network.Longitude ||
			place.TimeZone != network.TimeZone {
			t.Fatalf("%+v did not round trip from %+v", place, network)
		}
		country := places.Country(network.CountryCode)
		if country == nil || country.Name != network.Country || country.GeonameId == 0 {
			t.Fatalf("country %+v did not round trip from %+v", country, network)
		}
	}
}

// The file depends only on the content, not on map order or the order the
// networks were added in, and is sorted by country, region and city.
func TestExportIsDeterministic(t *testing.T) {
	marshal := func(reverse bool) []byte {
		exporter := NewExporter()
		worldNetworks := testWorld()
		for i := range worldNetworks {
			network := worldNetworks[i]
			if reverse {
				network = worldNetworks[len(worldNetworks)-1-i]
			}
			exporter.Add(network, 1+i%3)
		}
		export, err := exporter.Export("test", 1)
		if err != nil {
			t.Fatal(err)
		}
		first, err := export.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		second, err := export.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatal("two marshals of one export differ")
		}
		return first
	}
	// the tallies differ between the two orders (1+i%3 networks each), but
	// every city here has one coordinate, so the content is the same
	forward := marshal(false)
	for i := 0; i < 5; i += 1 {
		if !bytes.Equal(forward, marshal(false)) {
			t.Fatal("two exports of the same networks differ")
		}
	}
	if !bytes.Equal(forward, marshal(true)) {
		t.Fatalf("the export depends on the order networks were added:\n%s\n---\n%s", forward, marshal(true))
	}

	// sorted by country, region, city
	text := string(forward)
	orderedFragments := []string{
		"  ad:\n",
		"    La Massana:\n",
		// byte order: '2' sorts before ':'
		"\"123\":",
		"\"1:20\":",
		"      L’Aldosa:",
		"      \"No\":",
		"    Sant Julià de Lòria:\n",
		"    \"on\":\n",
		"  ch:\n",
		"      Zürich:",
		// Norway's code is a YAML 1.1 boolean
		"  \"no\":\n",
	}
	position := 0
	for _, fragment := range orderedFragments {
		i := strings.Index(text[position:], fragment)
		if i < 0 {
			t.Fatalf("%q is missing or out of order in:\n%s", fragment, text)
		}
		position += i + len(fragment)
	}
}

// The file has the documented header, flow mappings and attribution.
func TestExportMarshalShape(t *testing.T) {
	export, _ := testExport(t, func(exporter *Exporter) {
		exporter.Add(testNetwork(func(network *ExportNetwork) {
			network.CityGeonameId, network.City = 2650444, "East Finchley"
			network.Latitude, network.Longitude = 51.5967, -0.1593
		}), 1)
	})
	placesBytes, err := export.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	text := string(placesBytes)
	for _, line := range []string{
		"version: 1\n",
		"source: test\n",
		"build_epoch: 1790110762\n",
		"countries:\n  gb: {name: United Kingdom, geoname_id: 2635167, continent_code: eu, continent: Europe}\n",
		"places:\n  gb:\n    England:\n      East Finchley: {geoname_id: 2650444, region_geoname_id: 6269131, latitude: 51.5967, longitude: -0.1593, spread_km: 0, time_zone: Europe/London}\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("the export does not contain %q:\n%s", line, text)
		}
	}
	// the attribution travels with the data
	if !strings.HasPrefix(text, "# ") || !strings.Contains(text, "GeoLite2 data created by MaxMind") {
		t.Fatalf("the export has no attribution header:\n%s", text)
	}
}
