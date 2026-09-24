package model

import (
	"fmt"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/geo"
)

// Tests of the location matcher: the name normalization, the distance and its
// limit, the resolution of stored rows, and the merge plan, over real
// GeoLite2 entries and, when the checkout has it, the whole place list.

// Diacritics, case, punctuation and compatibility forms fold away, and a
// letter without a decomposition stays itself.
func TestNormalizePlaceName(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		// diacritics
		{name: "São Paulo", want: "sao paulo"},
		{name: "Sao Paulo", want: "sao paulo"},
		{name: "Zürich", want: "zurich"},
		{name: "Kraków", want: "krakow"},
		{name: "Sant Julià de Lòria", want: "sant julia de loria"},
		// case
		{name: "SAO PAULO", want: "sao paulo"},
		{name: "Frankfurt Am Main", want: "frankfurt am main"},
		// St./Saint: the abbreviation's dot is punctuation; the two spellings
		// stay three edits apart
		{name: "St. Denis", want: "st denis"},
		{name: "ST DENIS", want: "st denis"},
		{name: "Saint-Denis", want: "saint denis"},
		// hyphen and space are one separator; runs collapse; ends trim
		{name: "Saint Denis", want: "saint denis"},
		{name: "  Saint -- Denis  ", want: "saint denis"},
		{name: "Stratford-upon-Avon", want: "stratford upon avon"},
		{name: "L’Aldosa", want: "l aldosa"},
		{name: "'Ain Deheb", want: "ain deheb"},
		{name: "Town: the \"Old\" #1, [A]", want: "town the old 1 a"},
		// full case folding and compatibility decomposition
		{name: "Straße", want: "strasse"},
		{name: "ﬁnsbury", want: "finsbury"},
		{name: "İzmir", want: "izmir"},
		// a letter that has no decomposition stays itself
		{name: "Tromsø", want: "tromsø"},
		// nothing to compare
		{name: "", want: ""},
		{name: " - ", want: ""},
	} {
		if got := normalizePlaceName(test.name); got != test.want {
			t.Errorf("normalizePlaceName(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

// The restricted edit distance, by rune, in both directions.
func TestPlaceNameDistance(t *testing.T) {
	for _, test := range []struct {
		a    string
		b    string
		want int
	}{
		{a: "", b: "", want: 0},
		{a: "london", b: "london", want: 0},
		{a: "london", b: "", want: 6},
		// one insertion, deletion, substitution, swap
		{a: "hesse", b: "hessen", want: 1},
		{a: "hessen", b: "hesse", want: 1},
		{a: "barton", b: "burton", want: 1},
		{a: "tromsø", b: "tromso", want: 1},
		{a: "sao paulo", b: "sao pualo", want: 1},
		// the restricted form: a swapped pair is not edited again, so "ca" to
		// "abc" costs 3 (the unrestricted distance would be 2)
		{a: "ca", b: "abc", want: 3},
		{a: "st denis", b: "saint denis", want: 3},
		{a: "frankfurt", b: "frankfurt am main", want: 8},
		{a: "kitten", b: "sitting", want: 3},
		// by rune, not by byte
		{a: "zürich", b: "zurich", want: 1},
	} {
		if got := placeNameDistance(test.a, test.b); got != test.want {
			t.Errorf("placeNameDistance(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
		if got := placeNameDistance(test.b, test.a); got != test.want {
			t.Errorf("placeNameDistance(%q, %q) = %d, want %d (not symmetric)", test.b, test.a, got, test.want)
		}
	}
}

// The early exit returns the exact distance up to the limit and limit + 1
// beyond it, on random pairs of short strings over a small alphabet (so that
// swaps and near misses are common).
func TestPlaceRuneDistanceWithinLimitMatchesFullDistance(t *testing.T) {
	random := mathrand.New(mathrand.NewSource(1))
	randomName := func() []rune {
		name := make([]rune, random.Intn(10))
		for i := range name {
			name[i] = rune('a' + random.Intn(4))
		}
		return name
	}
	for i := 0; i < 20000; i += 1 {
		a := randomName()
		b := randomName()
		full := placeRuneDistance(a, b, len(a)+len(b))
		for limit := 0; limit <= 4; limit += 1 {
			want := min(full, limit+1)
			if got := placeRuneDistance(a, b, limit); got != want {
				t.Fatalf("placeRuneDistance(%q, %q, %d) = %d, want %d (full %d)", string(a), string(b), limit, got, want, full)
			}
		}
	}
}

// The shorter name decides the limit: 2 below 8 runes, 3 from 8.
func TestPlaceNameMatchLimit(t *testing.T) {
	limit := func(a string, b string) int {
		return placeNameMatchLimit([]rune(normalizePlaceName(a)), []rune(normalizePlaceName(b)))
	}
	// the shorter name decides
	connect.AssertEqual(t, limit("Bath", "Bathgate"), 2)
	connect.AssertEqual(t, limit("Hessen", "Hesse"), 2)
	// 7 characters: 2
	connect.AssertEqual(t, limit("St Ives", "Saint Ives"), 2)
	// 8 characters: 3
	connect.AssertEqual(t, limit("St Denis", "Saint Denis"), 3)
	connect.AssertEqual(t, limit("Sao Paulo", "Sao Pualo"), 3)
}

// A place list of real GeoLite2 entries (names, ids, coordinates of the
// 2026-09 build) chosen for the matcher's cases: a region's cities that are
// near each other in spelling, a suffixed twin, a city with no GeoLite2
// misspelling ("Kyiv"), and a country whose cities have no subdivision.
const matchTestPlacesYaml = `
version: 1
source: test
build_epoch: 1
countries:
  br: {name: Brazil, geoname_id: 3469034, continent_code: sa, continent: South America}
  gb: {name: United Kingdom, geoname_id: 2635167, continent_code: eu, continent: Europe}
  sg: {name: Singapore, geoname_id: 1880251, continent_code: as, continent: Asia}
  ua: {name: Ukraine, geoname_id: 690791, continent_code: eu, continent: Europe}
  us: {name: United States, geoname_id: 6252001, continent_code: na, continent: North America}
places:
  br:
    São Paulo:
      Campinas: {geoname_id: 3467865, region_geoname_id: 3448433, latitude: -22.8951, longitude: -47.0439}
      Santana: {geoname_id: 8535094, region_geoname_id: 3448433, latitude: -23.4963, longitude: -46.639}
      Santos: {geoname_id: 3449433, region_geoname_id: 3448433, latitude: -23.9569, longitude: -46.3446}
      São Paulo: {geoname_id: 3448439, region_geoname_id: 3448433, latitude: -23.6293, longitude: -46.6351}
      São Pedro: {geoname_id: 3448403, region_geoname_id: 3448433, latitude: -22.5558, longitude: -47.9052}
  gb:
    England:
      Abridge: {geoname_id: 9072588, region_geoname_id: 6269131, latitude: 51.6473, longitude: 0.1909}
      Aldbourne: {geoname_id: 2657562, region_geoname_id: 6269131, latitude: 51.4785, longitude: -1.6131}
      Alfold: {geoname_id: 2657512, region_geoname_id: 6269131, latitude: 51.0955, longitude: -0.5116}
      Alford: {geoname_id: 2657510, region_geoname_id: 6269131, latitude: 53.2467, longitude: 0.1869}
      Cambridge: {geoname_id: 2653941, region_geoname_id: 6269131, latitude: 52.198, longitude: 0.118}
      Dorking: {geoname_id: 2651095, region_geoname_id: 6269131, latitude: 51.2344, longitude: -0.3336}
      Eastbourne: {geoname_id: 2650497, region_geoname_id: 6269131, latitude: 50.7666, longitude: 0.2852}
      Forest Hill: {geoname_id: 2649216, region_geoname_id: 6269131, latitude: 51.4504, longitude: -0.0367}
      Forest Hill (geoname 11593192): {geoname_id: 11593192, region_geoname_id: 6269131, latitude: 51.7561, longitude: -1.1475}
      Ilford: {geoname_id: 2646277, region_geoname_id: 6269131, latitude: 51.5564, longitude: 0.0715}
      Melbourne: {geoname_id: 2642800, region_geoname_id: 6269131, latitude: 53.8889, longitude: -0.8512}
      North Shields: {geoname_id: 2641267, region_geoname_id: 6269131, latitude: 55.0168, longitude: -1.451}
      Salford: {geoname_id: 2638671, region_geoname_id: 6269131, latitude: 53.4836, longitude: -2.2862}
      Selby: {geoname_id: 2638235, region_geoname_id: 6269131, latitude: 53.7833, longitude: -1.0594}
      South Shields: {geoname_id: 2637329, region_geoname_id: 6269131, latitude: 54.9684, longitude: -1.4002}
      Stratford-upon-Avon: {geoname_id: 2636713, region_geoname_id: 6269131, latitude: 52.1824, longitude: -1.6992}
      Uxbridge: {geoname_id: 2635042, region_geoname_id: 6269131, latitude: 51.5513, longitude: -0.4845}
    Scotland:
      Alford: {geoname_id: 2657509, region_geoname_id: 2638360, latitude: 57.2353, longitude: -2.6976}
  sg:
    Singapore:
      Bedok New Town: {geoname_id: 1884382, latitude: 1.3264, longitude: 103.9394}
  ua:
    Kyiv City:
      Kyiv: {geoname_id: 703448, region_geoname_id: 703447, latitude: 50.458, longitude: 30.5303}
  us:
    Alabama:
      Clanton: {geoname_id: 4055577, region_geoname_id: 4829764, latitude: 32.8327, longitude: -86.6431}
      Clayton: {geoname_id: 4055696, region_geoname_id: 4829764, latitude: 31.8898, longitude: -85.4506}
`

// The matcher's index of matchTestPlacesYaml.
func loadMatchTestPlaceNames(t *testing.T) *locationPlaceNames {
	t.Helper()
	places, err := geo.LoadPlaces([]byte(matchTestPlacesYaml))
	if err != nil {
		t.Fatal(err)
	}
	return newLocationPlaceNames(places)
}

// Resolves a city row by the rule: its region row's name to the list's region
// first, then its own name within that region, or within the whole country
// when the region row resolves to none.
func resolveStoredCity(placeNames *locationPlaceNames, countryCode string, regionRowName string, name string) placeResolution {
	return placeNames.resolveCity(countryCode, placeNames.regionOfRow(countryCode, regionRowName, 0), name)
}

// The rule on stored rows without ids: every variant of a place resolves to
// it, every real place resolves to itself -- so no two distinct places can
// ever merge -- and a name in reach of two places resolves to neither.
func TestResolveStoredPlaceNames(t *testing.T) {
	placeNames := loadMatchTestPlaceNames(t)
	for _, test := range []struct {
		countryCode string
		regionRow   string
		name        string
		geonameId   uint32
		kind        placeResolutionKind
		distance    int
	}{
		// variants of one place
		{countryCode: "br", regionRow: "São Paulo", name: "Sao Paulo", geonameId: 3448439, kind: placeAnchored},
		{countryCode: "br", regionRow: "Sao Paulo", name: "SÃO PAULO", geonameId: 3448439, kind: placeAnchored},
		{countryCode: "ua", regionRow: "Kyiv City", name: "Kiev", geonameId: 703448, kind: placeLooselyMatched, distance: 2},
		// a region row that resolves to no region: the whole country is the scope
		{countryCode: "ua", regionRow: "Kiev Oblast (old)", name: "Kiev", geonameId: 703448, kind: placeLooselyMatched, distance: 2},
		{countryCode: "gb", regionRow: "England", name: "Stratford-upon-Avn", geonameId: 2636713, kind: placeLooselyMatched, distance: 1},
		{countryCode: "gb", regionRow: "England", name: "Stratfrd-upon-Avn", geonameId: 2636713, kind: placeLooselyMatched, distance: 2},
		{countryCode: "gb", regionRow: "England", name: "Uxbridg", geonameId: 2635042, kind: placeLooselyMatched, distance: 1},
		{countryCode: "gb", regionRow: "England", name: "Forest Hill (geoname 11593192)", geonameId: 11593192, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "Forest Hill", geonameId: 2649216, kind: placeAnchored},
		{countryCode: "sg", regionRow: "Singapore", name: "BEDOK NEW TOWN", geonameId: 1884382, kind: placeAnchored},
		// distinct places that are near in spelling: each is itself
		{countryCode: "gb", regionRow: "England", name: "Cambridge", geonameId: 2653941, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "Uxbridge", geonameId: 2635042, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "Abridge", geonameId: 9072588, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "Melbourne", geonameId: 2642800, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "Aldbourne", geonameId: 2657562, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "Eastbourne", geonameId: 2650497, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "North Shields", geonameId: 2641267, kind: placeAnchored},
		{countryCode: "gb", regionRow: "England", name: "South Shields", geonameId: 2637329, kind: placeAnchored},
		{countryCode: "us", regionRow: "Alabama", name: "Clayton", geonameId: 4055696, kind: placeAnchored},
		{countryCode: "us", regionRow: "Alabama", name: "Clanton", geonameId: 4055577, kind: placeAnchored},
		{countryCode: "br", regionRow: "São Paulo", name: "São Pedro", geonameId: 3448403, kind: placeAnchored},
		{countryCode: "br", regionRow: "São Paulo", name: "São Paulo", geonameId: 3448439, kind: placeAnchored},
		// in reach of two places (Alfold and Alford, one edit each): neither
		{countryCode: "gb", regionRow: "England", name: "Alfod", kind: placeAmbiguous},
		// the same name where only one of them is in scope: that one
		{countryCode: "gb", regionRow: "Scotland", name: "Alfod", geonameId: 2657509, kind: placeLooselyMatched, distance: 1},
		// in reach of nothing
		{countryCode: "gb", regionRow: "England", name: "Atlantis", kind: placeUnresolved},
		{countryCode: "fr", regionRow: "Ile-de-France", name: "Paris", kind: placeUnresolved},
	} {
		resolution := resolveStoredCity(placeNames, test.countryCode, test.regionRow, test.name)
		var geonameId uint32
		resolvedName := "-"
		if resolution.resolved() {
			geonameId = resolution.candidate.geonameId
			resolvedName = resolution.candidate.name
		}
		t.Logf("| %s | %s | %s | %s | %d | %s |", test.countryCode, test.regionRow, test.name, resolution.kind, resolution.distance, resolvedName)
		if geonameId != test.geonameId || resolution.kind != test.kind || resolution.distance != test.distance {
			t.Errorf(
				"%s, %s, %q resolves to %d (%s, distance %d), want %d (%s, distance %d)",
				test.countryCode,
				test.regionRow,
				test.name,
				geonameId,
				resolution.kind,
				resolution.distance,
				test.geonameId,
				test.kind,
				test.distance,
			)
		}
	}
}

// A region row resolves among its country's regions, by anchor or loosely,
// and the region named for a country has no id.
func TestResolveStoredRegionNames(t *testing.T) {
	placeNames := loadMatchTestPlaceNames(t)
	for _, test := range []struct {
		countryCode string
		name        string
		geonameId   uint32
		kind        placeResolutionKind
	}{
		{countryCode: "br", name: "Sao Paulo", geonameId: 3448433, kind: placeAnchored},
		{countryCode: "gb", name: "ENGLAND", geonameId: 6269131, kind: placeAnchored},
		{countryCode: "gb", name: "Englnd", geonameId: 6269131, kind: placeLooselyMatched},
		// the region named for a country whose cities have no subdivision has
		// no id of its own
		{countryCode: "sg", name: "Singapore", geonameId: 0, kind: placeAnchored},
		{countryCode: "ua", name: "Kiev", kind: placeUnresolved},
		{countryCode: "zz", name: "Anywhere", kind: placeUnresolved},
	} {
		resolution := placeNames.resolveRegion(test.countryCode, test.name)
		var geonameId uint32
		if resolution.resolved() {
			geonameId = resolution.candidate.geonameId
		}
		if geonameId != test.geonameId || resolution.kind != test.kind {
			t.Errorf("%s, %q resolves to %d (%s), want %d (%s)", test.countryCode, test.name, geonameId, resolution.kind, test.geonameId, test.kind)
		}
	}
}

// A row as the matcher reads it, with a fixed id.
func testLocationRow(id string, name string, countryCode string, regionLocationId server.Id, geonameId uint32) *locationRow {
	return &locationRow{
		locationId:       server.RequireParseId(id),
		name:             name,
		countryCode:      countryCode,
		regionLocationId: regionLocationId,
		geonameId:        geonameId,
	}
}

// Only rows that resolve to the same place merge; the row that carries the
// place's id is kept, else the most referenced, else the oldest.
func TestPlanLocationMergesByResolvedPlace(t *testing.T) {
	placeNames := loadMatchTestPlaceNames(t)
	regionLocationId := server.RequireParseId("00000000-0000-0000-0000-0000000000aa")
	locationIdRegionNames := map[server.Id]string{regionLocationId: "England"}

	keyedSaoPaulo := testLocationRow("00000000-0000-0000-0000-000000000009", "São Paulo", "br", regionLocationId, 3448439)
	saoPaulo := testLocationRow("00000000-0000-0000-0000-000000000001", "Sao Paulo", "br", regionLocationId, 0)
	saoPauloUpper := testLocationRow("00000000-0000-0000-0000-000000000002", "SÃO PAULO", "br", regionLocationId, 0)
	kyiv := testLocationRow("00000000-0000-0000-0000-000000000003", "Kyiv", "ua", regionLocationId, 0)
	kiev := testLocationRow("00000000-0000-0000-0000-000000000004", "Kiev", "ua", regionLocationId, 0)
	kiev.referenceCount = 9
	cambridge := testLocationRow("00000000-0000-0000-0000-000000000005", "Cambridge", "gb", regionLocationId, 0)
	uxbridge := testLocationRow("00000000-0000-0000-0000-000000000006", "Uxbridge", "gb", regionLocationId, 0)
	abridge := testLocationRow("00000000-0000-0000-0000-000000000007", "Abridge", "gb", regionLocationId, 0)
	alfod := testLocationRow("00000000-0000-0000-0000-000000000008", "Alfod", "gb", regionLocationId, 0)

	resolve := func(row *locationRow) (placeKey, placeResolution, bool) {
		if row.geonameId != 0 {
			return placeKey{countryCode: row.countryCode, geonameId: row.geonameId}, placeResolution{}, true
		}
		// the region row resolves in its own country: England in gb, and to
		// nothing (so the whole country) elsewhere
		resolution := resolveStoredCity(placeNames, row.countryCode, locationIdRegionNames[row.regionLocationId], row.name)
		if !resolution.resolved() {
			return placeKey{}, resolution, false
		}
		return placeKey{countryCode: row.countryCode, geonameId: resolution.candidate.geonameId}, resolution, true
	}
	merges := planLocationMerges(
		[]*locationRow{saoPaulo, cambridge, kiev, saoPauloUpper, uxbridge, keyedSaoPaulo, abridge, kyiv, alfod},
		resolve,
	)
	got := map[string][]string{}
	for _, merge := range merges {
		memberNames := []string{}
		for _, member := range merge.memberRows {
			memberNames = append(memberNames, member.name)
		}
		got[merge.canonicalRow.name] = memberNames
	}
	want := map[string][]string{
		// the row with the id, whatever the others' references
		"São Paulo": {"Sao Paulo", "SÃO PAULO"},
		// neither has the id: the more referenced is kept though younger
		"Kiev": {"Kyiv"},
		// alone in their places: kept, merged with nothing
		"Cambridge": {},
		"Uxbridge":  {},
		"Abridge":   {},
	}
	if len(got) != len(want) {
		t.Fatalf("merges = %v, want %v", got, want)
	}
	for canonicalName, memberNames := range want {
		gotMemberNames, ok := got[canonicalName]
		if !ok || !slices.Equal(gotMemberNames, memberNames) {
			t.Fatalf("merges = %v, want %v", got, want)
		}
	}

	// planned again over what is kept, nothing merges
	keptRows := []*locationRow{keyedSaoPaulo, kiev, cambridge, uxbridge, abridge, alfod}
	for _, merge := range planLocationMerges(keptRows, resolve) {
		connect.AssertEqual(t, len(merge.memberRows), 0)
	}
}

// Every GeoLite2 city, stored as a row without an id under a region row
// without an id, resolves to itself, so the de-duplication merges none of
// them: by construction, a real place is never a misspelling of another.
func TestGeoLite2CitiesAsLegacyRowsResolveToThemselves(t *testing.T) {
	// the newest place list of the sibling config checkout ($PLACES_YML
	// overrides), or "" when there is none or under -short
	geoLite2PlacesFile := func() string {
		if testing.Short() {
			return ""
		}
		if path := os.Getenv("PLACES_YML"); path != "" {
			return path
		}
		paths, _ := filepath.Glob(filepath.Join("..", "..", "config", "all", "mmdb", "*", "places.yml"))
		if len(paths) == 0 {
			return ""
		}
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
	path := geoLite2PlacesFile()
	if path == "" {
		t.Skip("no GeoLite2 place list: set PLACES_YML or check out config beside server")
	}
	placesBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	places, err := geo.LoadPlaces(placesBytes)
	if err != nil {
		t.Fatal(err)
	}
	placeNames := newLocationPlaceNames(places)

	// one region row per region of the list, and one city row per city
	regionRows := []*locationRow{}
	regionRowIds := map[string]server.Id{}
	cityRows := []*locationRow{}
	for place := range places.Cities() {
		regionKey := place.CountryCode + "\x00" + place.Region
		regionRowId, ok := regionRowIds[regionKey]
		if !ok {
			regionRowId = server.NewId()
			regionRowIds[regionKey] = regionRowId
			regionRows = append(regionRows, &locationRow{locationId: regionRowId, name: place.Region, countryCode: place.CountryCode})
		}
		cityRows = append(cityRows, &locationRow{locationId: server.NewId(), name: place.City, countryCode: place.CountryCode, regionLocationId: regionRowId})
	}

	kindCounts := map[placeResolutionKind]int{}
	regionMerges := planLocationMerges(regionRows, func(row *locationRow) (placeKey, placeResolution, bool) {
		resolution := placeNames.resolveRegion(row.countryCode, row.name)
		if !resolution.resolved() {
			return placeKey{}, resolution, false
		}
		return placeKey{countryCode: row.countryCode, geonameId: resolution.candidate.geonameId}, resolution, true
	})
	locationIdListRegions := map[server.Id]*geo.RegionNames{}
	mergedRegionRowCount := 0
	for _, merge := range regionMerges {
		mergedRegionRowCount += len(merge.memberRows)
	}
	for _, row := range regionRows {
		if region := placeNames.regionOfRow(row.countryCode, row.name, 0); region != nil {
			locationIdListRegions[row.locationId] = region
		}
	}

	wrongResolutions := []string{}
	cityMerges := planLocationMerges(cityRows, func(row *locationRow) (placeKey, placeResolution, bool) {
		resolution := placeNames.resolveCity(row.countryCode, locationIdListRegions[row.regionLocationId], row.name)
		kindCounts[resolution.kind] += 1
		if !resolution.resolved() {
			return placeKey{}, resolution, false
		}
		if resolution.candidate.name != row.name && len(wrongResolutions) < 10 {
			wrongResolutions = append(wrongResolutions, fmt.Sprintf("%q -> %q", row.name, resolution.candidate.name))
		}
		return placeKey{countryCode: row.countryCode, geonameId: resolution.candidate.geonameId}, resolution, true
	})
	mergedCityRowCount := 0
	for _, merge := range cityMerges {
		mergedCityRowCount += len(merge.memberRows)
	}
	t.Logf(
		"%d cities in %d regions: %d anchored, %d loose, %d ambiguous, %d unresolved; %d regions resolved; merges: %d region rows, %d city rows",
		len(cityRows),
		len(regionRows),
		kindCounts[placeAnchored],
		kindCounts[placeLooselyMatched],
		kindCounts[placeAmbiguous],
		kindCounts[placeUnresolved],
		len(locationIdListRegions),
		mergedRegionRowCount,
		mergedCityRowCount,
	)
	connect.AssertEqual(t, len(wrongResolutions), 0)
	connect.AssertEqual(t, mergedRegionRowCount, 0)
	connect.AssertEqual(t, mergedCityRowCount, 0)
	connect.AssertEqual(t, kindCounts[placeAnchored], len(cityRows))
	connect.AssertEqual(t, len(locationIdListRegions), len(regionRows))
}
