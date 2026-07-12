package model

import (
	"testing"

	"github.com/go-playground/assert/v2"
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
