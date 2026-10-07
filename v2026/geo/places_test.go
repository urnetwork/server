package geo

import (
	"fmt"
	"math"
	mathrand "math/rand"
	"strings"
	"testing"
)

// Tests of the place list loader and the reverse geocoder: the distance, the
// loader's refusals, and the nearest-city search across cell boundaries, the
// antimeridian and the poles, checked against a brute-force scan.

// The synthetic five-city set the reverse geocoder tests share. Each city
// exists to break one naive search:
//   - cellside/cellfar: the nearest city is across a 1° cell boundary, while a
//     farther one shares the query's own cell
//   - eastedge: the nearest city is across the antimeridian
//   - polenear/poledecoy: the nearest city is across the pole, 180° of
//     longitude away, while a farther one is in the query's own column
const fivePlaces = `
version: 1
source: test
build_epoch: 1
countries:
  ga: {name: Gabon, geoname_id: 2400553, continent_code: af, continent: Africa}
  fj: {name: Fiji, geoname_id: 2205218, continent_code: oc, continent: Oceania}
  sj: {name: Svalbard and Jan Mayen, geoname_id: 607072, continent_code: eu, continent: Europe}
places:
  ga:
    Estuaire:
      Cellside: {geoname_id: 101, region_geoname_id: 11, latitude: 1.01, longitude: 10.5, spread_km: 0, time_zone: Africa/Libreville}
      Cellfar: {geoname_id: 102, region_geoname_id: 11, latitude: 0.1, longitude: 10.5, spread_km: 2.5, time_zone: Africa/Libreville}
  fj:
    Northern:
      Eastedge: {geoname_id: 103, region_geoname_id: 13, latitude: -17.0, longitude: 179.95, spread_km: 0, time_zone: Pacific/Fiji}
  sj:
    Svalbard:
      Polenear: {geoname_id: 104, region_geoname_id: 14, latitude: 89.5, longitude: 0, spread_km: 0, time_zone: Arctic/Longyearbyen}
      Poledecoy: {geoname_id: 105, region_geoname_id: 14, latitude: 88.2, longitude: 179, spread_km: 0, time_zone: Arctic/Longyearbyen}
`

// The list of fivePlaces.
func loadFivePlaces(t *testing.T) *Places {
	t.Helper()
	places, err := LoadPlaces([]byte(fivePlaces))
	if err != nil {
		t.Fatal(err)
	}
	return places
}

// Fails the test unless the nearest city to a point is the wanted one, at the
// wanted distance to within 50 m.
func assertNearest(t *testing.T, places *Places, latitude float64, longitude float64, countryCode string, wantCity string, wantKm float64) {
	t.Helper()
	place, distanceKm := places.NearestCity(latitude, longitude, countryCode)
	if place == nil {
		t.Fatalf("NearestCity(%v, %v, %q) = nil, want %s", latitude, longitude, countryCode, wantCity)
	}
	if place.City != wantCity {
		t.Fatalf("NearestCity(%v, %v, %q) = %s (%.3f km), want %s", latitude, longitude, countryCode, place.City, distanceKm, wantCity)
	}
	if 0.05 < math.Abs(distanceKm-wantKm) {
		t.Fatalf("NearestCity(%v, %v, %q) distance = %.3f km, want %.3f km", latitude, longitude, countryCode, distanceKm, wantKm)
	}
}

// The haversine distance is right at every separation and symmetric.
func TestDistanceKm(t *testing.T) {
	for _, test := range []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		wantKm                 float64
	}{
		{name: "same point", lat1: 51.5, lon1: -0.1, lat2: 51.5, lon2: -0.1, wantKm: 0},
		// one degree of arc on the 6371 km sphere
		{name: "one degree of latitude", lat1: 0, lon1: 0, lat2: 1, lon2: 0, wantKm: 111.195},
		{name: "one degree of longitude at the equator", lat1: 0, lon1: 0, lat2: 0, lon2: 1, wantKm: 111.195},
		// a degree of longitude shrinks with cos(latitude)
		{name: "one degree of longitude at 60°", lat1: 60, lon1: 0, lat2: 60, lon2: 1, wantKm: 55.597},
		{name: "across the antimeridian", lat1: 0, lon1: 179.5, lat2: 0, lon2: -179.5, wantKm: 111.195},
		{name: "across the pole", lat1: 89, lon1: 0, lat2: 89, lon2: 180, wantKm: 222.390},
		{name: "antipodes", lat1: 0, lon1: 0, lat2: 0, lon2: 180, wantKm: math.Pi * EarthRadiusKm},
		{name: "pole to pole", lat1: 90, lon1: 0, lat2: -90, lon2: 0, wantKm: math.Pi * EarthRadiusKm},
		// London to Paris
		{name: "london paris", lat1: 51.5074, lon1: -0.1278, lat2: 48.8566, lon2: 2.3522, wantKm: 343.556},
	} {
		distanceKm := DistanceKm(test.lat1, test.lon1, test.lat2, test.lon2)
		if 0.01 < math.Abs(distanceKm-test.wantKm) {
			t.Errorf("%s: DistanceKm = %.4f, want %.4f", test.name, distanceKm, test.wantKm)
		}
		if reverse := DistanceKm(test.lat2, test.lon2, test.lat1, test.lon1); 1e-9 < math.Abs(reverse-distanceKm) {
			t.Errorf("%s: DistanceKm is not symmetric: %v != %v", test.name, reverse, distanceKm)
		}
	}
}

// A loaded list keeps its header, its order and every field of its places.
func TestLoadPlaces(t *testing.T) {
	places := loadFivePlaces(t)

	if places.Version != 1 || places.Source != "test" || places.BuildEpoch != 1 {
		t.Fatalf("header = %d %q %d", places.Version, places.Source, places.BuildEpoch)
	}
	if places.CityCount() != 5 {
		t.Fatalf("CityCount = %d, want 5", places.CityCount())
	}

	// the export's order: country, region, city
	cityPaths := []string{}
	for place := range places.Cities() {
		cityPaths = append(cityPaths, place.CountryCode+"/"+place.Region+"/"+place.City)
	}
	if got, want := strings.Join(cityPaths, " "), "fj/Northern/Eastedge ga/Estuaire/Cellfar ga/Estuaire/Cellside sj/Svalbard/Poledecoy sj/Svalbard/Polenear"; got != want {
		t.Fatalf("Cities = %s, want %s", got, want)
	}

	countryCodes := []string{}
	for _, country := range places.Countries() {
		countryCodes = append(countryCodes, country.Code)
	}
	if got, want := strings.Join(countryCodes, " "), "fj ga sj"; got != want {
		t.Fatalf("Countries = %s, want %s", got, want)
	}

	country := places.Country("GA")
	if country == nil || country.Name != "Gabon" || country.GeonameId != 2400553 || country.ContinentCode != "af" || country.Continent != "Africa" {
		t.Fatalf("Country(GA) = %+v", country)
	}
	if places.Country("zz") != nil {
		t.Fatal("Country(zz) should be nil")
	}

	place := places.CityByGeonameId(102)
	if place == nil {
		t.Fatal("CityByGeonameId(102) = nil")
	}
	if place.City != "Cellfar" || place.Region != "Estuaire" || place.RegionGeonameId != 11 || place.CountryCode != "ga" ||
		place.Latitude != 0.1 || place.Longitude != 10.5 || place.SpreadKm != 2.5 || place.TimeZone != "Africa/Libreville" {
		t.Fatalf("CityByGeonameId(102) = %+v", place)
	}
	for _, geonameId := range []uint32{0, 100, 106, math.MaxUint32} {
		if place := places.CityByGeonameId(geonameId); place != nil {
			t.Fatalf("CityByGeonameId(%d) = %+v, want nil", geonameId, place)
		}
	}

	if got := places.RegionGeonameId("GA", "Estuaire"); got != 11 {
		t.Fatalf("RegionGeonameId = %d, want 11", got)
	}
	if got := places.RegionGeonameId("ga", "Nowhere"); got != 0 {
		t.Fatalf("RegionGeonameId(unknown) = %d, want 0", got)
	}
}

// The loader refuses another version, a missing country, a repeated or
// missing id, an invalid coordinate, a city with no fields, and non-YAML.
func TestLoadPlacesRejects(t *testing.T) {
	valid := func(edit func(string) string) []byte {
		return []byte(edit(fivePlaces))
	}
	for _, test := range []struct {
		name        string
		placesBytes []byte
		wantErr     string
	}{
		{
			name:        "another version",
			placesBytes: valid(func(s string) string { return strings.Replace(s, "version: 1", "version: 2", 1) }),
			wantErr:     "version 2",
		},
		{
			name: "a country missing from the country list",
			placesBytes: valid(func(s string) string {
				return strings.Replace(s, "  fj: {name: Fiji, geoname_id: 2205218, continent_code: oc, continent: Oceania}\n", "", 1)
			}),
			wantErr: "not in the country list",
		},
		{
			name:        "a repeated geoname id",
			placesBytes: valid(func(s string) string { return strings.Replace(s, "geoname_id: 105", "geoname_id: 104", 1) }),
			wantErr:     "geoname id 104",
		},
		{
			name:        "a city without a geoname id",
			placesBytes: valid(func(s string) string { return strings.Replace(s, "geoname_id: 105, ", "", 1) }),
			wantErr:     "no geoname id",
		},
		{
			name:        "a latitude off the sphere",
			placesBytes: valid(func(s string) string { return strings.Replace(s, "latitude: 88.2", "latitude: 91", 1) }),
			wantErr:     "invalid coordinate",
		},
		{
			name: "a city with no fields",
			placesBytes: valid(func(s string) string {
				return strings.Replace(s, "Poledecoy: {geoname_id: 105, region_geoname_id: 14, latitude: 88.2, longitude: 179, spread_km: 0, time_zone: Arctic/Longyearbyen}", "Poledecoy:", 1)
			}),
			wantErr: "no fields",
		},
		{
			name:        "not yaml",
			placesBytes: []byte("version: [1"),
			wantErr:     "places:",
		},
	} {
		_, err := LoadPlaces(test.placesBytes)
		if err == nil {
			t.Errorf("%s: LoadPlaces accepted it", test.name)
			continue
		}
		if !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: error %q does not mention %q", test.name, err, test.wantErr)
		}
	}
}

// The nearest city across a cell boundary beats a farther one in the query's
// own cell.
func TestNearestCityAcrossCellBoundary(t *testing.T) {
	places := loadFivePlaces(t)

	// (0.99, 10.5) shares cell (0°, 10°) with Cellfar, 99 km south, while
	// Cellside is 2.2 km north across the 1° boundary
	assertNearest(t, places, 0.99, 10.5, "", "Cellside", DistanceKm(0.99, 10.5, 1.01, 10.5))
	assertNearest(t, places, 0.2, 10.5, "", "Cellfar", DistanceKm(0.2, 10.5, 0.1, 10.5))
	// exactly on a place
	assertNearest(t, places, 1.01, 10.5, "", "Cellside", 0)
}

// A country code restricts the search, which widens as far as it must.
func TestNearestCityCountryRestriction(t *testing.T) {
	places := loadFivePlaces(t)

	assertNearest(t, places, 1.0, 10.5, "ga", "Cellside", DistanceKm(1.0, 10.5, 1.01, 10.5))
	// the nearest Fijian city is on the far side of the earth; the search
	// widens until it covers it
	assertNearest(t, places, 1.0, 10.5, "fj", "Eastedge", DistanceKm(1.0, 10.5, -17.0, 179.95))
	// the code is matched in either case
	assertNearest(t, places, 1.0, 10.5, "FJ", "Eastedge", DistanceKm(1.0, 10.5, -17.0, 179.95))
	assertNearest(t, places, 1.0, 10.5, "sj", "Polenear", DistanceKm(1.0, 10.5, 89.5, 0))

	for _, countryCode := range []string{"zz", "gb"} {
		if place, distanceKm := places.NearestCity(1.0, 10.5, countryCode); place != nil || distanceKm != 0 {
			t.Fatalf("NearestCity restricted to %q = %+v, %v; want nil, 0", countryCode, place, distanceKm)
		}
	}
}

// The search wraps across the antimeridian.
func TestNearestCityAntimeridian(t *testing.T) {
	places := loadFivePlaces(t)

	// Eastedge is at 179.95°E; a query at 179.95°W is 0.1° of longitude away
	// across the antimeridian, in the cell at the opposite end of the grid
	wantKm := DistanceKm(-17.0, -179.95, -17.0, 179.95)
	if 11 < wantKm {
		t.Fatalf("test geometry: %.1f km", wantKm)
	}
	assertNearest(t, places, -17.0, -179.95, "", "Eastedge", wantKm)
	assertNearest(t, places, -17.0, 180, "", "Eastedge", DistanceKm(-17.0, 180, -17.0, 179.95))
	assertNearest(t, places, -17.0, -180, "", "Eastedge", DistanceKm(-17.0, -180, -17.0, 179.95))
}

// The search reaches over the pole, where every longitude is one point.
func TestNearestCityPolar(t *testing.T) {
	places := loadFivePlaces(t)

	// From (89.5, 180), Polenear at (89.5, 0) is 1° of arc away over the pole,
	// while Poledecoy in the query's own column is 1.3° away
	assertNearest(t, places, 89.5, 180, "", "Polenear", DistanceKm(89.5, 180, 89.5, 0))
	assertNearest(t, places, 89.5, -179, "", "Polenear", DistanceKm(89.5, -179, 89.5, 0))
	// at the pole every longitude is the same point
	assertNearest(t, places, 90, 0, "", "Polenear", DistanceKm(90, 0, 89.5, 0))
	assertNearest(t, places, 90, 123, "", "Polenear", DistanceKm(90, 0, 89.5, 0))
	// from the south pole every city is thousands of km north; the nearest
	// is the southernmost
	assertNearest(t, places, -90, 0, "", "Eastedge", DistanceKm(-90, 0, -17.0, 179.95))
}

// A query is taken modulo 360 in longitude and clamped in latitude, and a
// non-finite one finds nothing.
func TestNearestCityNormalizesTheQuery(t *testing.T) {
	places := loadFivePlaces(t)

	// longitude modulo 360, latitude clamped to the poles
	assertNearest(t, places, 0.99, 10.5+360, "", "Cellside", DistanceKm(0.99, 10.5, 1.01, 10.5))
	assertNearest(t, places, 0.99, 10.5-720, "", "Cellside", DistanceKm(0.99, 10.5, 1.01, 10.5))
	assertNearest(t, places, 95, 0, "", "Polenear", DistanceKm(90, 0, 89.5, 0))

	for _, point := range [][2]float64{
		{math.NaN(), 0},
		{0, math.NaN()},
		{math.Inf(1), 0},
		{0, math.Inf(-1)},
	} {
		if place, distanceKm := places.NearestCity(point[0], point[1], ""); place != nil || distanceKm != 0 {
			t.Fatalf("NearestCity(%v, %v) = %+v, %v; want nil, 0", point[0], point[1], place, distanceKm)
		}
	}
}

// An empty list finds nothing.
func TestNearestCityEmptyList(t *testing.T) {
	places, err := LoadPlaces([]byte("version: 1\nsource: test\nbuild_epoch: 1\ncountries: {}\nplaces: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if place, distanceKm := places.NearestCity(0, 0, ""); place != nil || distanceKm != 0 {
		t.Fatalf("NearestCity on an empty list = %+v, %v", place, distanceKm)
	}
}

// Two cities at one distance resolve to the smaller geoname id, whatever the
// scan order.
func TestNearestCityTieBreaksByGeonameId(t *testing.T) {
	// two cities at the same distance on either side of the query, filed so
	// that the larger id sorts (and is scanned) first
	places, err := LoadPlaces([]byte(`
version: 1
source: test
build_epoch: 1
countries:
  ga: {name: Gabon, geoname_id: 1, continent_code: af, continent: Africa}
places:
  ga:
    A:
      A: {geoname_id: 9, latitude: 0.5, longitude: 10.25}
      B: {geoname_id: 7, latitude: 0.5, longitude: 10.75}
`))
	if err != nil {
		t.Fatal(err)
	}
	place, _ := places.NearestCity(0.5, 10.5, "")
	if place == nil || place.GeonameId != 7 {
		t.Fatalf("NearestCity = %+v, want geoname id 7", place)
	}
}

// Checks the grid search against a scan of every place, over places and
// queries spread across the whole sphere (poles and antimeridian included)
// and a dense cluster where cells matter most.
func TestNearestCityMatchesBruteForce(t *testing.T) {
	random := mathrand.New(mathrand.NewSource(1))
	// uniform on the sphere, so the polar caps get their share
	randomPoint := func() (float64, float64) {
		latitude := math.Asin(2*random.Float64()-1) * 180 / math.Pi
		longitude := random.Float64()*360 - 180
		return latitude, longitude
	}
	countryCodes := []string{"aa", "bb", "cc", "dd"}

	var yaml strings.Builder
	yaml.WriteString("version: 1\nsource: test\nbuild_epoch: 1\ncountries:\n")
	for _, countryCode := range countryCodes {
		fmt.Fprintf(&yaml, "  %s: {name: %s, geoname_id: 1, continent_code: eu, continent: Europe}\n", countryCode, strings.ToUpper(countryCode))
	}
	yaml.WriteString("places:\n")
	// one generated place
	type point struct {
		latitude, longitude float64
		countryCode         string
		geonameId           uint32
	}
	points := []point{}
	for i, countryCode := range countryCodes {
		fmt.Fprintf(&yaml, "  %s:\n    R:\n", countryCode)
		for j := 0; j < 150; j += 1 {
			latitude, longitude := randomPoint()
			if j < 30 {
				// a cluster around (47.5, 7.5), a few km apart
				latitude = 47.5 + random.Float64() - 0.5
				longitude = 7.5 + random.Float64() - 0.5
			}
			geonameId := uint32(1000*(i+1) + j)
			fmt.Fprintf(&yaml, "      C%d: {geoname_id: %d, latitude: %v, longitude: %v}\n", geonameId, geonameId, latitude, longitude)
			points = append(points, point{latitude: latitude, longitude: longitude, countryCode: countryCode, geonameId: geonameId})
		}
	}
	places, err := LoadPlaces([]byte(yaml.String()))
	if err != nil {
		t.Fatal(err)
	}

	bruteForce := func(latitude float64, longitude float64, countryCode string) (uint32, float64) {
		var bestGeonameId uint32
		bestKm := math.Inf(1)
		for _, p := range points {
			if countryCode != "" && p.countryCode != countryCode {
				continue
			}
			distanceKm := DistanceKm(latitude, longitude, p.latitude, p.longitude)
			if distanceKm < bestKm || (distanceKm == bestKm && p.geonameId < bestGeonameId) {
				bestGeonameId = p.geonameId
				bestKm = distanceKm
			}
		}
		return bestGeonameId, bestKm
	}

	for i := 0; i < 4000; i += 1 {
		latitude, longitude := randomPoint()
		switch i % 4 {
		case 1:
			latitude = 47.5 + 2*random.Float64() - 1
			longitude = 7.5 + 2*random.Float64() - 1
		case 2:
			// hug the antimeridian and the poles
			longitude = 180 - random.Float64()*2
			if random.Intn(2) == 0 {
				longitude = -longitude
			}
			latitude = (90 - random.Float64()*5) * float64(1-2*random.Intn(2))
		}
		countryCode := ""
		if i%3 == 0 {
			countryCode = countryCodes[random.Intn(len(countryCodes))]
		}
		place, distanceKm := places.NearestCity(latitude, longitude, countryCode)
		// compare at the point the search itself measures from
		wantGeonameId, wantKm := bruteForce(latitude, normalizeLongitude(longitude), countryCode)
		if place == nil {
			t.Fatalf("NearestCity(%v, %v, %q) = nil, want %d", latitude, longitude, countryCode, wantGeonameId)
		}
		if place.GeonameId != wantGeonameId || distanceKm != wantKm {
			t.Fatalf(
				"NearestCity(%v, %v, %q) = %d at %v km, want %d at %v km",
				latitude,
				longitude,
				countryCode,
				place.GeonameId,
				distanceKm,
				wantGeonameId,
				wantKm,
			)
		}
	}
}
