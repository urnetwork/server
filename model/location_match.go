package model

import (
	"cmp"
	"math/bits"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/geo"
)

// A stored row is resolved to a place before it is new (connect/GEOMAP.md
// §4.2). Neither the old city list nor a lookup spells a place exactly one
// way, so a row that predates geoname ids is resolved to a place of the
// canonical place list (GeoLite2's) before a lookup of that place creates
// another row, and the de-duplication merges the rows that resolve to the same
// place:
//
//  1. a row that carries a geoname id is that place
//  2. anchor: a row whose normalized name (normalizePlaceName) is the
//     normalized name of a place in its region -- of a region of its country,
//     for a region row -- is that place. Every place of the list anchors
//     exactly, so a real place is never taken for a misspelling of another:
//     "Cambridge" is Cambridge, never Abridge
//  3. loose: a name that is no place's normalized name is the one place within
//     the optimal string alignment distance of placeNameMatchLimit, when there
//     is exactly one; a second within the distance, or none, leaves the row as
//     it is. "Kiev" is Kyiv; "Bartun", within reach of both Barton and Burton,
//     is neither
//
// A city is resolved among the cities of the list's region its region row
// resolves to, or of its whole country when that row resolves to none. A row
// that resolves to nothing is left alone. This file is pure; the place list is
// loaded in location_place_names.go and the rows read in
// network_client_location_model.go and location_deduplicate_model.go.

// Folds a place name to the key the matcher compares: case-folded, decomposed
// (NFKD) with the combining marks dropped, and every run of anything that is
// not a letter or a digit -- punctuation, symbols, whitespace -- collapsed to
// one space, trimmed at both ends. So "São Paulo", "SAO PAULO" and "Sao-Paulo"
// all fold to "sao paulo", "St. Denis" to "st denis", and "Straße" to
// "strasse".
func normalizePlaceName(name string) string {
	// fold before decomposing: folding can itself produce a combining mark
	// (İ folds to i and a combining dot), which the decomposed pass then drops
	decomposed := norm.NFKD.String(cases.Fold().String(name))
	var normalized strings.Builder
	normalized.Grow(len(decomposed))
	separated := false
	for _, r := range decomposed {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
			// the accent NFKD split off the letter before it
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mc, r):
			// a spacing mark is part of its letter in the scripts that have
			// them, not a separator
			if separated && 0 < normalized.Len() {
				normalized.WriteByte(' ')
			}
			separated = false
			normalized.WriteRune(r)
		default:
			separated = true
		}
	}
	return normalized.String()
}

// The optimal string alignment distance between two strings, rune by rune:
// the fewest insertions, deletions, substitutions and swaps of two adjacent
// runes that turn one into the other, where no rune is edited again after a
// swap (the restricted form of the Damerau–Levenshtein distance).
func placeNameDistance(a string, b string) int {
	ar := []rune(a)
	br := []rune(b)
	return placeRuneDistance(ar, br, len(ar)+len(br))
}

// The placeNameDistance when that is at most limit, and limit + 1 otherwise,
// which lets it stop as soon as no alignment can come in under the limit.
//
// d[i][j] is the distance between the first i runes of a and the first j of b:
// the least of deleting a[i-1] (d[i-1][j] + 1), inserting b[j-1]
// (d[i][j-1] + 1), matching or substituting (d[i-1][j-1] + 0 or 1), and, when
// the last two runes of each are the same two swapped, the swap
// (d[i-2][j-2] + 1). Three rows of d are kept.
func placeRuneDistance(a []rune, b []rune, limit int) int {
	lengthDifference := len(a) - len(b)
	if lengthDifference < 0 {
		lengthDifference = -lengthDifference
	}
	// every rune of the length difference is an insertion or a deletion
	if limit < lengthDifference {
		return limit + 1
	}

	n := len(b)
	previousPrevious := make([]int, n+1)
	previous := make([]int, n+1)
	current := make([]int, n+1)
	for j := 0; j <= n; j += 1 {
		previous[j] = j
	}
	previousMinimum := 0
	for i := 1; i <= len(a); i += 1 {
		current[0] = i
		minimum := i
		for j := 1; j <= n; j += 1 {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if 1 < i && 1 < j && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d = min(d, previousPrevious[j-2]+1)
			}
			current[j] = d
			minimum = min(minimum, d)
		}
		// Every later cell descends from this row or, through a swap, from the
		// row before it at one more edit; once neither can come in under the
		// limit, nothing after them can.
		if limit < min(minimum, previousMinimum+1) {
			return limit + 1
		}
		previousMinimum = minimum
		previousPrevious, previous, current = previous, current, previousPrevious
	}
	return min(previous[n], limit+1)
}

// The largest distance at which a name may be taken for a place's.
func placeNameMatchLimit(a []rune, b []rune) int {
	if 8 <= min(len(a), len(b)) {
		return 3
	}
	return 2
}

// the largest placeNameMatchLimit
const placeNameMaxMatchLimit = 3

// The set of runes a name contains, folded to 64 bits (a-z and 0-9 each their
// own bit, anything else hashed onto the rest). An edit changes at most two
// members of the set, so two names within distance d differ in at most 2d
// bits: a necessary condition that rejects most unlike pairs with one XOR,
// before any distance is computed.
func placeRuneSignature(name []rune) uint64 {
	var signature uint64
	for _, r := range name {
		switch {
		case 'a' <= r && r <= 'z':
			signature |= 1 << (r - 'a')
		case '0' <= r && r <= '9':
			signature |= 1 << (26 + r - '0')
		default:
			signature |= 1 << (36 + uint64(r)%28)
		}
	}
	return signature
}

// A name of the place list a row may resolve to: a region of a country, or a
// city.
type geoNameCandidate struct {
	// as the list names it
	name       string
	normalized []rune
	signature  uint64
	// the region's or city's GeoNames id; 0 for the region named for a country
	geonameId uint32
	// one of the two is set
	region *geo.RegionNames
	city   *geo.NamedPlace
}

// A candidate for a name and its normalized form; the caller sets the region
// or the city it stands for.
func newGeoNameCandidate(name string, normalized string, geonameId uint32) *geoNameCandidate {
	runes := []rune(normalized)
	return &geoNameCandidate{
		name:       name,
		normalized: runes,
		signature:  placeRuneSignature(runes),
		geonameId:  geonameId,
	}
}

// The names of the place list one row may resolve to: the regions of a
// country, or the cities of a region or of a country. Immutable once built.
type geoNameScope struct {
	normalizedNameCandidates map[string][]*geoNameCandidate
	lengthCandidates         map[int][]*geoNameCandidate
}

// Indexes candidates by normalized name, for anchors, and by length in runes,
// for the loose rule; a candidate with an empty normalized name is left out.
func newGeoNameScope(candidates []*geoNameCandidate) *geoNameScope {
	scope := &geoNameScope{
		normalizedNameCandidates: map[string][]*geoNameCandidate{},
		lengthCandidates:         map[int][]*geoNameCandidate{},
	}
	for _, candidate := range candidates {
		if len(candidate.normalized) == 0 {
			continue
		}
		scope.normalizedNameCandidates[string(candidate.normalized)] = append(scope.normalizedNameCandidates[string(candidate.normalized)], candidate)
		scope.lengthCandidates[len(candidate.normalized)] = append(scope.lengthCandidates[len(candidate.normalized)], candidate)
	}
	return scope
}

// How a stored name resolved within a scope.
type placeResolutionKind int

const (
	// a name of no place, and within reach of none
	placeUnresolved placeResolutionKind = iota
	// the normalized name of exactly one place
	placeAnchored
	// the normalized name of no place, and within the distance of exactly one
	placeLooselyMatched
	// could be two or more places, so none
	placeAmbiguous
)

// Implements fmt.Stringer, for logs and test tables.
func (self placeResolutionKind) String() string {
	switch self {
	case placeAnchored:
		return "anchored"
	case placeLooselyMatched:
		return "loose"
	case placeAmbiguous:
		return "ambiguous"
	default:
		return "unresolved"
	}
}

// What a stored name resolves to within a scope.
type placeResolution struct {
	kind      placeResolutionKind
	candidate *geoNameCandidate
	// 0 for an anchor
	distance int
}

// Whether the name resolved to a place, by anchor or loosely.
func (self placeResolution) resolved() bool {
	return self.kind == placeAnchored || self.kind == placeLooselyMatched
}

// Applies the anchor, then the loose rule, to a stored name.
func (self *geoNameScope) resolve(name string) placeResolution {
	normalized := []rune(normalizePlaceName(name))
	if len(normalized) == 0 {
		return placeResolution{kind: placeUnresolved}
	}

	if exact := self.normalizedNameCandidates[string(normalized)]; 0 < len(exact) {
		if len(exact) == 1 {
			return placeResolution{kind: placeAnchored, candidate: exact[0]}
		}
		// Two places whose names normalize alike ("Saint-Denis" and
		// "SAINT DENIS"): the one spelled exactly so, if there is one.
		var spelled *geoNameCandidate
		spelledCount := 0
		for _, candidate := range exact {
			if candidate.name == name {
				spelled = candidate
				spelledCount += 1
			}
		}
		if spelledCount == 1 {
			return placeResolution{kind: placeAnchored, candidate: spelled}
		}
		// an exact name is never matched loosely either
		return placeResolution{kind: placeAmbiguous}
	}

	signature := placeRuneSignature(normalized)
	var within *geoNameCandidate
	withinDistance := 0
	withinCount := 0
	for length := len(normalized) - placeNameMaxMatchLimit; length <= len(normalized)+placeNameMaxMatchLimit; length += 1 {
		for _, candidate := range self.lengthCandidates[length] {
			limit := placeNameMatchLimit(normalized, candidate.normalized)
			if 2*limit < bits.OnesCount64(signature^candidate.signature) {
				continue
			}
			distance := placeRuneDistance(normalized, candidate.normalized, limit)
			if limit < distance {
				continue
			}
			withinCount += 1
			if 1 < withinCount {
				// a second place in reach: the nearest is not unique enough to
				// be taken for either
				return placeResolution{kind: placeAmbiguous}
			}
			within = candidate
			withinDistance = distance
		}
	}
	if withinCount == 1 {
		return placeResolution{kind: placeLooselyMatched, candidate: within, distance: withinDistance}
	}
	return placeResolution{kind: placeUnresolved}
}

// A place of the list rows resolve to. The region named for a country has no
// geoname id and is keyed by its country alone.
type placeKey struct {
	countryCode string
	geonameId   uint32
}

// A stored region or city row as the matcher sees it.
type locationRow struct {
	locationId  server.Id
	name        string
	countryCode string
	// a city's region row
	regionLocationId server.Id
	geonameId        uint32
	// how many rows elsewhere name this one; decides only which of several
	// rows without the place's id is kept
	referenceCount int
}

// The stored rows that resolve to one place: the row kept, and the rows that
// merge into it.
type locationMerge struct {
	key          placeKey
	canonicalRow *locationRow
	memberRows   []*locationRow
	// how the canonical row resolved, when it did not carry the place's id
	resolution placeResolution
}

// Orders the rows of one place for the one to keep: the row that already
// carries the place's id, then the most referenced, then the oldest. Location
// ids are time-ordered (server.NewId), and the location table has no create
// time, so the oldest is the smallest id.
func compareCanonicalRows(key placeKey, a *locationRow, b *locationRow) int {
	aKeyed := a.geonameId != 0 && a.geonameId == key.geonameId
	bKeyed := b.geonameId != 0 && b.geonameId == key.geonameId
	if aKeyed != bKeyed {
		if aKeyed {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(b.referenceCount, a.referenceCount); c != 0 {
		return c
	}
	return a.locationId.Cmp(b.locationId)
}

// Groups rows by the place resolve says each is, and keeps one row per place.
// A row that resolves to nothing is in no group; two rows that resolve to
// different places are never merged, however alike their names. Every group
// is returned, a single row included (it may still need the place's id),
// ordered by key.
func planLocationMerges(
	rows []*locationRow,
	resolve func(row *locationRow) (placeKey, placeResolution, bool),
) []*locationMerge {
	keyMerges := map[placeKey]*locationMerge{}
	keyRows := map[placeKey][]*locationRow{}
	rowResolutions := map[*locationRow]placeResolution{}
	for _, row := range rows {
		key, resolution, ok := resolve(row)
		if !ok {
			continue
		}
		keyRows[key] = append(keyRows[key], row)
		rowResolutions[row] = resolution
	}
	keys := make([]placeKey, 0, len(keyRows))
	for key, placeRows := range keyRows {
		slices.SortFunc(placeRows, func(a *locationRow, b *locationRow) int {
			return compareCanonicalRows(key, a, b)
		})
		keyMerges[key] = &locationMerge{
			key:          key,
			canonicalRow: placeRows[0],
			memberRows:   placeRows[1:],
			resolution:   rowResolutions[placeRows[0]],
		}
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a placeKey, b placeKey) int {
		if c := cmp.Compare(a.countryCode, b.countryCode); c != 0 {
			return c
		}
		return cmp.Compare(a.geonameId, b.geonameId)
	})
	merges := make([]*locationMerge, 0, len(keys))
	for _, key := range keys {
		merges = append(merges, keyMerges[key])
	}
	return merges
}
