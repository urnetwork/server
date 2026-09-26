package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/geo"
)

// Tests of the export command: a full export of a real GeoLite2-City database
// when one is at hand, the refusal of another file, and the atomic write.

// Finds the GeoLite2-City database to test against: the one $GEOLITE2_MMDB
// names, or the newest dated copy in the sibling config checkout
// (config/all/mmdb/<version>/geolite2.mmdb). The export walks the whole
// database, so the test is skipped when neither exists or under -short.
func geoLite2Database(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("walks the whole GeoLite2 database")
	}
	if path := os.Getenv("GEOLITE2_MMDB"); path != "" {
		return path
	}
	paths, _ := filepath.Glob(filepath.Join("..", "..", "..", "config", "all", "mmdb", "*", "geolite2.mmdb"))
	if len(paths) == 0 {
		t.Skip("no GeoLite2 database: set GEOLITE2_MMDB or check out config beside server")
	}
	// the dated directories are versions (2026.9.23), so compare them as such
	version := func(path string) []int {
		parts := []int{}
		for _, part := range strings.Split(filepath.Base(filepath.Dir(path)), ".") {
			n, _ := strconv.Atoi(part)
			parts = append(parts, n)
		}
		return parts
	}
	return slices.MaxFunc(paths, func(a string, b string) int {
		return slices.Compare(version(a), version(b))
	})
}

// A full export loads, holds every country its places name, places known
// towns where they are, reverse-geocodes, and is the same file on a rerun.
func TestExportGeoLite2City(t *testing.T) {
	mmdbPath := geoLite2Database(t)
	outPath := filepath.Join(t.TempDir(), "places.yml")

	var log bytes.Buffer
	if err := run(mmdbPath, outPath, &log); err != nil {
		t.Fatal(err)
	}
	t.Log(strings.TrimSpace(log.String()))
	for _, field := range []string{"networks=", "cities=", "countries=", "multi_coordinate_cities=", "collisions="} {
		if !strings.Contains(log.String(), field) {
			t.Fatalf("the summary has no %q: %s", field, log.String())
		}
	}

	placesBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	places, err := geo.LoadPlaces(placesBytes)
	if err != nil {
		t.Fatal(err)
	}
	if places.Source != "GeoLite2-City" || places.BuildEpoch == 0 {
		t.Fatalf("source %q, build epoch %d", places.Source, places.BuildEpoch)
	}
	// the 2026-09 build has 77,753 cities and 250 countries; any build is in
	// this range
	if places.CityCount() < 50000 || len(places.Countries()) < 240 {
		t.Fatalf("%d cities, %d countries", places.CityCount(), len(places.Countries()))
	}

	// every place's country is in the country list, and no id repeats
	// (LoadPlaces refuses both; this is the export's side of the contract)
	seenGeonameIds := map[uint32]bool{}
	for place := range places.Cities() {
		if places.Country(place.CountryCode) == nil {
			t.Fatalf("%s, %s is in %q, which is not in the country list", place.City, place.Region, place.CountryCode)
		}
		if seenGeonameIds[place.GeonameId] {
			t.Fatalf("geoname id %d repeats", place.GeonameId)
		}
		seenGeonameIds[place.GeonameId] = true
	}
	gb := places.Country("gb")
	if gb == nil || gb.Name != "United Kingdom" || gb.ContinentCode != "eu" || gb.Continent != "Europe" {
		t.Fatalf("gb = %+v", gb)
	}

	for _, want := range []struct {
		geonameId uint32
		city      string
		latitude  float64
		longitude float64
	}{
		{geonameId: 2650444, city: "East Finchley", latitude: 51.5967, longitude: -0.1593},
		{geonameId: 2651095, city: "Dorking", latitude: 51.2344, longitude: -0.3336},
		{geonameId: 2643743, city: "London", latitude: 51.5081, longitude: -0.1278},
	} {
		place := places.CityByGeonameId(want.geonameId)
		if place == nil {
			t.Fatalf("no city has geoname id %d (%s)", want.geonameId, want.city)
		}
		if place.City != want.city || place.Region != "England" || place.CountryCode != "gb" || place.TimeZone != "Europe/London" {
			t.Fatalf("geoname id %d = %+v", want.geonameId, place)
		}
		// the representative is one of MaxMind's own coordinates, so it can
		// move between builds; it stays in its town
		if distanceKm := geo.DistanceKm(place.Latitude, place.Longitude, want.latitude, want.longitude); 10 < distanceKm {
			t.Fatalf("%s is %.1f km from (%v, %v)", place.City, distanceKm, want.latitude, want.longitude)
		}
		if place.RegionGeonameId == 0 || place.SpreadKm < 0 {
			t.Fatalf("%s = %+v", place.City, place)
		}
	}

	// the reverse geocoder over the export
	place, distanceKm := places.NearestCity(51.5967, -0.1593, "gb")
	if place == nil || place.CountryCode != "gb" || 5 < distanceKm {
		t.Fatalf("NearestCity(East Finchley, gb) = %+v at %.1f km", place, distanceKm)
	}
	place, distanceKm = places.NearestCity(51.5967, -0.1593, "")
	if place == nil || place.CountryCode != "gb" || 5 < distanceKm {
		t.Fatalf("NearestCity(East Finchley) = %+v at %.1f km", place, distanceKm)
	}
	place, distanceKm = places.NearestCity(51.5967, -0.1593, "us")
	if place == nil || place.CountryCode != "us" {
		t.Fatalf("NearestCity(East Finchley, us) = %+v at %.1f km", place, distanceKm)
	}
	// the nearest of the United States to London is across the Atlantic
	if distanceKm < 3000 {
		t.Fatalf("NearestCity(East Finchley, us) = %s at %.1f km", place.City, distanceKm)
	}

	// a second export of the same database is the same file, byte for byte
	export, err := exportPlaces(mmdbPath)
	if err != nil {
		t.Fatal(err)
	}
	again, err := export.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(placesBytes, again) {
		t.Fatal("two exports of one database differ")
	}
}

// A file that is not a City database is refused.
func TestExportRefusesAnotherDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geolite2.mmdb")
	if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exportPlaces(path); err == nil {
		t.Fatal("exported a file that is not a database")
	}
}

// The write replaces the file whole, leaves it 0644 with no temporary file
// beside it, and fails cleanly into a missing directory.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "places.yml")
	for _, content := range []string{"first\n", "second\n"} {
		if err := writeFileAtomic(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		written, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(written) != content {
			t.Fatalf("wrote %q, want %q", written, content)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", info.Mode().Perm())
	}
	// no temporary file is left beside it
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d files beside the export", len(entries))
	}

	// a directory that does not exist fails without writing anything
	if err := writeFileAtomic(filepath.Join(dir, "missing", "places.yml"), []byte("x")); err == nil {
		t.Fatal("wrote into a missing directory")
	}
}
