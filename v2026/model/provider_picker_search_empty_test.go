// Fixed search-empty stages and the model returns eligible to increment them.
package model

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Gathers the registered producer without copying request or result data to the log.
func pickerSearchEmptyTestCounters(t testing.TB) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_provider_picker_search_empty_total" {
			continue
		}
		counts := map[string]float64{}
		for _, sample := range family.Metric {
			if len(sample.Label) != 1 || sample.Label[0].GetName() != "stage" || sample.Counter == nil {
				t.Fatal("empty-search diagnostic gained an unrelated label or metric type")
			}
			stage := sample.Label[0].GetValue()
			if _, duplicate := counts[stage]; duplicate {
				t.Fatal("duplicate empty-search stage")
			}
			counts[stage] = sample.Counter.GetValue()
		}
		return counts
	}
	t.Fatal("the serving API is missing the empty-search diagnostic")
	return nil
}

func TestProviderPickerSearchEmptyStages(t *testing.T) {
	empty := &FindLocationsResult{}
	hidden := &FindLocationsResult{Locations: []*LocationResult{{LocationType: "synthetic-hidden", MatchDistance: 1}}}
	visible := &FindLocationsResult{Locations: []*LocationResult{{LocationType: LocationTypeCity, MatchDistance: 1}}}
	exact := &FindLocationsResult{Locations: []*LocationResult{{LocationType: "synthetic-exact", MatchDistance: 0}}}
	matched := &providerPickerSearchInputs{indexMatches: true, locationMetadata: true, positiveFilter: true}
	for _, tt := range []struct {
		name    string
		surface string
		inputs  *providerPickerSearchInputs
		result  *FindLocationsResult
		err     error
		outcome string
		stage   string
	}{
		{"missing observation", "search", nil, empty, nil, "empty", "unobserved"},
		{"blank POST", "search", &providerPickerSearchInputs{blankQuery: true, locationMetadata: true}, empty, nil, "empty", "blank_query"},
		{"no index matches", "search", &providerPickerSearchInputs{}, empty, nil, "empty", "no_index_matches"},
		{"no metadata", "search", &providerPickerSearchInputs{indexMatches: true}, empty, nil, "empty", "no_location_metadata"},
		{"no positive filter", "search", &providerPickerSearchInputs{indexMatches: true, locationMetadata: true}, empty, nil, "empty", "no_positive_filter"},
		{"hidden result", "search", matched, hidden, nil, "empty", "positive_filter_no_rendered_rows"},
		{"visible city", "search", matched, visible, nil, "nonempty", ""},
		{"visible exact", "search", matched, exact, nil, "nonempty", ""},
		{"failed with result", "search", matched, hidden, errors.New("private synthetic error"), "error", ""},
		{"nil result", "search", nil, nil, nil, "error", ""},
		{"empty GET", "initial", matched, empty, nil, "empty", ""},
		{"empty direct", "direct", matched, empty, nil, "empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := pickerSearchEmptyTestCounters(t)
			want := maps.Clone(before)
			if tt.stage != "" {
				want[tt.stage]++
			}
			parent := providerPickerMetrics.children[tt.surface+"/"+tt.outcome]
			start := testutil.ToFloat64(parent)
			providerPickerMetrics.observeFindLocations(tt.surface, tt.result, tt.err, tt.inputs)
			if testutil.ToFloat64(parent) != start+1 || !maps.Equal(pickerSearchEmptyTestCounters(t), want) {
				t.Fatal("expected one parent outcome and only the designated empty-search stage")
			}
		})
	}
}

func TestProviderPickerSearchEmptyMatchedMetadataMiss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, requested, _, _ := providerPickerMetadataFixture(t, "missing")
		for id := range requested {
			locationSearch().Add(ctx, "syntheticmissingmetadata", id, 0)
		}
		want := pickerSearchEmptyTestCounters(t)
		want["no_location_metadata"]++
		parent := providerPickerMetrics.children["search/empty"]
		start := testutil.ToFloat64(parent)
		result, err := FindProviderLocations(&FindLocationsArgs{Query: "syntheticmissingmetadata"}, &session.ClientSession{Ctx: ctx, ClientAddress: "127.0.0.1:1234"})
		if err != nil || result == nil || len(result.Locations) != 0 || testutil.ToFloat64(parent) != start+1 || !maps.Equal(pickerSearchEmptyTestCounters(t), want) {
			t.Fatal("a successful search for indexed, absent metadata did not count one empty at the metadata read")
		}
	})
}

// A direct lookup and a search rejected before caller geolocation take the
// real API model entry without touching a database or incrementing an empty.
func TestProviderPickerSearchEmptyRegisteredAndExclusive(t *testing.T) {
	before := pickerSearchEmptyTestCounters(t)
	stages := []string{"blank_query", "no_index_matches", "no_location_metadata", "no_positive_filter", "positive_filter_no_rendered_rows", "unobserved"}
	if len(before) != len(stages) {
		t.Fatal("unexpected cardinality for the empty-search diagnostic")
	}
	fresh := newProviderPickerMetricSet()
	for stage := range before {
		if !slices.Contains(stages, stage) || testutil.ToFloat64(fresh.searchEmpty.WithLabelValues(stage)) != 0 {
			t.Fatal("unexpected empty-search stage or nonzero initial counter")
		}
	}
	ctx := server.WithoutPostgres(t.Context())
	client := &session.ClientSession{Ctx: ctx, ClientAddress: "invalid-private-address"}
	result, err := FindProviderLocations(&FindLocationsArgs{Query: (server.Id{1}).String()}, client)
	if err != nil || result == nil || len(result.Devices) != 1 {
		t.Fatal("direct device lookup changed")
	}
	result, err = FindProviderLocations(&FindLocationsArgs{Query: "private search"}, client)
	if err == nil || result != nil || server.PacketPostgresAttempts(ctx) != 0 {
		t.Fatal("invalid caller did not stop before the source read")
	}
	for stage, after := range pickerSearchEmptyTestCounters(t) {
		if after != before[stage] {
			t.Fatal("direct or failed search was counted as successfully empty")
		}
	}
}
