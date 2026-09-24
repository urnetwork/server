package geo

import (
	"fmt"
	"math"
	mathrand "math/rand"
	"strings"
	"testing"
)

// Tests of the containment hinges: their keys, their values on a hand-built
// list, and a brute-force check over random ones.

// The fivePlaces list with Gabon split into three regions: a second region,
// Woleu-Ntem, whose one city is north of Estuaire's cities, and a city
// GeoLite2 files under no subdivision, so its region is named for the country
// and has no id.
var containmentPlaces = strings.Replace(
	fivePlaces,
	"  ga:\n    Estuaire:\n",
	"  ga:\n"+
		"    Gabon:\n"+
		"      Regionless: {geoname_id: 107, latitude: 0.5, longitude: 11.5, spread_km: 0, time_zone: Africa/Libreville}\n"+
		"    Woleu-Ntem:\n"+
		"      Northside: {geoname_id: 106, region_geoname_id: 16, latitude: 1.6, longitude: 10.5, spread_km: 0, time_zone: Africa/Libreville}\n"+
		"    Estuaire:\n",
	1,
)

// The containment of a places.yml, failing the test if it does not load.
func loadContainment(t *testing.T, placesYaml string) *PlaceContainment {
	t.Helper()
	places, err := LoadPlaces([]byte(placesYaml))
	if err != nil {
		t.Fatal(err)
	}
	return NewPlaceContainment(places)
}

// Fails the test unless a hinge is the wanted value, to within rounding.
func assertHinge(t *testing.T, name string, gotKm float64, wantKm float64) {
	t.Helper()
	if 1e-9 < math.Abs(gotKm-wantKm) {
		t.Fatalf("%s: hinge %.9f km, want %.9f km", name, gotKm, wantKm)
	}
}

// A region key takes the id form or the name form, and a name that looks like
// an id stays a name.
func TestContainmentKeys(t *testing.T) {
	for _, test := range []struct {
		countryCode     string
		regionGeonameId uint32
		region          string
		want            string
	}{
		{countryCode: "GA", regionGeonameId: 11, region: "Estuaire", want: "ga/11"},
		{countryCode: "ga", regionGeonameId: 0, region: "Gabon", want: "ga:Gabon"},
		// a name that looks like an id stays a name
		{countryCode: "ga", regionGeonameId: 0, region: "11", want: "ga:11"},
	} {
		if got := ContainmentRegionKey(test.countryCode, test.regionGeonameId, test.region); got != test.want {
			t.Errorf("ContainmentRegionKey(%q, %d, %q) = %q, want %q", test.countryCode, test.regionGeonameId, test.region, got, test.want)
		}
	}
	if got := ContainmentCountryKey("GA"); got != "ga" {
		t.Errorf("ContainmentCountryKey(GA) = %q", got)
	}
}

// The country hinge is zero inside, the whole way back outside, and correct
// across the antimeridian and the pole.
func TestContainmentCountryHinge(t *testing.T) {
	containment := loadContainment(t, fivePlaces)
	at := func(latitude float64, longitude float64) LatLon {
		return LatLon{Latitude: latitude, Longitude: longitude}
	}
	nearestGabonKm := func(latitude float64, longitude float64) float64 {
		return min(DistanceKm(latitude, longitude, 1.01, 10.5), DistanceKm(latitude, longitude, 0.1, 10.5))
	}

	// inside: the nearest city is in the country
	assertHinge(t, "on Cellside", containment.CountryHinge(at(1.01, 10.5), "ga"), 0)
	assertHinge(t, "between the Gabonese cities", containment.CountryHinge(at(0.5, 10.6), "ga"), 0)
	// the key is matched in either case
	assertHinge(t, "upper case key", containment.CountryHinge(at(0.5, 10.6), "GA"), 0)

	// a Gabonese genesis carried to Fiji pays the whole way back
	assertHinge(t, "Gabon genesis on Eastedge", containment.CountryHinge(at(-17.0, 179.95), "ga"), nearestGabonKm(-17.0, 179.95))

	// across the antimeridian from Eastedge: still nearest to Fiji
	assertHinge(t, "Fiji across the antimeridian", containment.CountryHinge(at(-17.0, -179.95), "fj"), 0)
	assertHinge(
		t,
		"Svalbard genesis across the antimeridian",
		containment.CountryHinge(at(-17.0, -179.95), "sj"),
		DistanceKm(-17.0, -179.95, 88.2, 179)-DistanceKm(-17.0, -179.95, -17.0, 179.95),
	)

	// over the pole: Polenear is nearest from the far side
	assertHinge(t, "Svalbard over the pole", containment.CountryHinge(at(89.5, 180), "sj"), 0)
	assertHinge(t, "Gabon genesis at the pole", containment.CountryHinge(at(89.5, 180), "ga"), nearestGabonKm(89.5, 180)-DistanceKm(89.5, 180, 89.5, 0))

	// no such country, or no point: no bias
	assertHinge(t, "unknown country", containment.CountryHinge(at(0.5, 10.6), "zz"), 0)
	assertHinge(t, "NaN latitude", containment.CountryHinge(at(math.NaN(), 10.6), "fj"), 0)
	assertHinge(t, "infinite longitude", containment.CountryHinge(at(0.5, math.Inf(1)), "fj"), 0)
	var nilContainment *PlaceContainment
	assertHinge(t, "nil containment", nilContainment.CountryHinge(at(-17.0, 179.95), "ga"), 0)
}

// The region hinge is zero inside, grows continuously past the bisector, and
// finds a region by either form of its key.
func TestContainmentRegionHinge(t *testing.T) {
	containment := loadContainment(t, containmentPlaces)
	at := func(latitude float64, longitude float64) LatLon {
		return LatLon{Latitude: latitude, Longitude: longitude}
	}
	estuaire := ContainmentRegionKey("ga", 11, "Estuaire")
	woleuNtem := ContainmentRegionKey("ga", 16, "Woleu-Ntem")
	regionless := ContainmentRegionKey("ga", 0, "Gabon")

	// inside Estuaire, on and near its cities
	assertHinge(t, "on Cellside", containment.RegionHinge(at(1.01, 10.5), estuaire), 0)
	assertHinge(t, "north of Cellside, still nearer it", containment.RegionHinge(at(1.3, 10.5), estuaire), 0)

	// Past the bisector between Cellside (1.01°) and Northside (1.6°) the
	// hinge is the margin by which Northside has become nearer.
	assertHinge(
		t,
		"past the bisector",
		containment.RegionHinge(at(1.31, 10.5), estuaire),
		DistanceKm(1.31, 10.5, 1.01, 10.5)-DistanceKm(1.31, 10.5, 1.6, 10.5),
	)
	assertHinge(
		t,
		"near Northside",
		containment.RegionHinge(at(1.5, 10.5), estuaire),
		DistanceKm(1.5, 10.5, 1.01, 10.5)-DistanceKm(1.5, 10.5, 1.6, 10.5),
	)
	// the same points from the other region's side
	assertHinge(t, "Woleu-Ntem near Northside", containment.RegionHinge(at(1.5, 10.5), woleuNtem), 0)
	assertHinge(t, "Woleu-Ntem genesis on Cellside", containment.RegionHinge(at(1.01, 10.5), woleuNtem), DistanceKm(1.01, 10.5, 1.6, 10.5))

	// a region with no id is keyed by its name
	assertHinge(t, "on Regionless", containment.RegionHinge(at(0.5, 11.5), regionless), 0)
	assertHinge(t, "regionless genesis on Cellfar", containment.RegionHinge(at(0.1, 10.5), regionless), DistanceKm(0.1, 10.5, 0.5, 11.5))
	// a region with an id also answers to its name
	for _, latitude := range []float64{1.01, 1.31, 1.5} {
		assertHinge(
			t,
			fmt.Sprintf("Estuaire by name at %v", latitude),
			containment.RegionHinge(at(latitude, 10.5), ContainmentRegionKey("ga", 0, "Estuaire")),
			containment.RegionHinge(at(latitude, 10.5), estuaire),
		)
	}

	// a region hinge is never larger than its country's cities allow: all of
	// these points are nearest a Gabonese city
	assertHinge(t, "country hinge near Northside", containment.CountryHinge(at(1.5, 10.5), "ga"), 0)

	// the hinge rises continuously from zero across the bisector (1.305°),
	// twice as fast as the point moves: one distance grows as the other shrinks
	previousKm := 0.0
	for step := 0; step <= 10; step += 1 {
		latitude := 1.30 + 0.001*float64(step)
		hingeKm := containment.RegionHinge(at(latitude, 10.5), estuaire)
		if hingeKm < previousKm {
			t.Fatalf("hinge fell from %v to %v at %v°", previousKm, hingeKm, latitude)
		}
		if 0.25 < hingeKm-previousKm {
			t.Fatalf("hinge jumped from %v to %v at %v°", previousKm, hingeKm, latitude)
		}
		previousKm = hingeKm
	}
	wantKm := 2 * (1.31 - 1.305) * math.Pi / 180 * EarthRadiusKm
	if 1e-6 < math.Abs(previousKm-wantKm) {
		t.Fatalf("hinge 0.005° past the bisector = %v km, want %v km", previousKm, wantKm)
	}

	// no such region, or no point: no bias
	assertHinge(t, "unknown region id", containment.RegionHinge(at(1.5, 10.5), "ga/999"), 0)
	assertHinge(t, "unknown region name", containment.RegionHinge(at(1.5, 10.5), "zz:Nowhere"), 0)
	assertHinge(t, "NaN longitude", containment.RegionHinge(at(1.5, math.NaN()), estuaire), 0)
	var nilContainment *PlaceContainment
	assertHinge(t, "nil containment", nilContainment.RegionHinge(at(1.5, 10.5), estuaire), 0)

	// the other countries' single regions: across the antimeridian and the pole
	assertHinge(t, "Northern across the antimeridian", containment.RegionHinge(at(-17.0, -179.95), ContainmentRegionKey("fj", 13, "Northern")), 0)
	assertHinge(t, "Svalbard over the pole", containment.RegionHinge(at(89.5, 180), ContainmentRegionKey("sj", 14, "Svalbard")), 0)
}

// Checks both hinges against a scan of every city, over regions with and
// without ids spread across the sphere and a dense cluster where regions
// interleave.
func TestContainmentMatchesBruteForce(t *testing.T) {
	random := mathrand.New(mathrand.NewSource(2))
	randomPoint := func() (float64, float64) {
		latitude := math.Asin(2*random.Float64()-1) * 180 / math.Pi
		longitude := random.Float64()*360 - 180
		return latitude, longitude
	}
	// one generated city and the keys it is filed under
	type city struct {
		latitude, longitude float64
		countryCode         string
		regionKey           string
	}
	countryCodes := []string{"aa", "bb", "cc"}
	var yaml strings.Builder
	yaml.WriteString("version: 1\nsource: test\nbuild_epoch: 1\ncountries:\n")
	for _, countryCode := range countryCodes {
		fmt.Fprintf(&yaml, "  %s: {name: %s, geoname_id: 1, continent_code: eu, continent: Europe}\n", countryCode, strings.ToUpper(countryCode))
	}
	yaml.WriteString("places:\n")
	cities := []city{}
	regionKeys := []string{}
	for i, countryCode := range countryCodes {
		fmt.Fprintf(&yaml, "  %s:\n", countryCode)
		for r := 0; r < 4; r += 1 {
			region := fmt.Sprintf("R%d", r)
			// the last region of each country has no id
			regionGeonameId := uint32(0)
			if r < 3 {
				regionGeonameId = uint32(100*(i+1) + r)
			}
			regionKey := ContainmentRegionKey(countryCode, regionGeonameId, region)
			regionKeys = append(regionKeys, regionKey)
			fmt.Fprintf(&yaml, "    %s:\n", region)
			for j := 0; j < 40; j += 1 {
				latitude, longitude := randomPoint()
				if j < 10 {
					latitude = 47.5 + 2*random.Float64() - 1
					longitude = 7.5 + 2*random.Float64() - 1
				}
				geonameId := 10000*(i+1) + 100*r + j
				fmt.Fprintf(&yaml, "      C%d: {geoname_id: %d, region_geoname_id: %d, latitude: %v, longitude: %v}\n", geonameId, geonameId, regionGeonameId, latitude, longitude)
				cities = append(cities, city{latitude: latitude, longitude: longitude, countryCode: countryCode, regionKey: regionKey})
			}
		}
	}
	containment := loadContainment(t, yaml.String())

	nearestKm := func(latitude float64, longitude float64, keep func(city) bool) float64 {
		bestKm := math.Inf(1)
		for _, c := range cities {
			if keep(c) {
				bestKm = min(bestKm, DistanceKm(latitude, longitude, c.latitude, c.longitude))
			}
		}
		return bestKm
	}
	for i := 0; i < 3000; i += 1 {
		latitude, longitude := randomPoint()
		if i%3 == 1 {
			latitude = 47.5 + 3*random.Float64() - 1.5
			longitude = 7.5 + 3*random.Float64() - 1.5
		}
		allKm := nearestKm(latitude, longitude, func(city) bool { return true })

		regionKey := regionKeys[random.Intn(len(regionKeys))]
		wantRegionKm := nearestKm(latitude, longitude, func(c city) bool { return c.regionKey == regionKey }) - allKm
		assertHinge(t, fmt.Sprintf("region %s at (%v, %v)", regionKey, latitude, longitude), containment.RegionHinge(LatLon{Latitude: latitude, Longitude: longitude}, regionKey), wantRegionKm)

		countryCode := countryCodes[random.Intn(len(countryCodes))]
		wantCountryKm := nearestKm(latitude, longitude, func(c city) bool { return c.countryCode == countryCode }) - allKm
		assertHinge(t, fmt.Sprintf("country %s at (%v, %v)", countryCode, latitude, longitude), containment.CountryHinge(LatLon{Latitude: latitude, Longitude: longitude}, countryCode), wantCountryKm)
	}
}
