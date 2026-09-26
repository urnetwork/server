// Fixed-schema and rendered-row controls; no caller/target identifiers survive.
package model

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestProviderPickerMetricsPreinitializedFiniteSchema(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.outcomes, metrics.reads)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, family := range families {
		counts[family.GetName()] = len(family.Metric)
		for _, metric := range family.Metric {
			if metric.GetCounter().GetValue() != 0 {
				t.Fatal("new producer has nonzero counter")
			}
			for _, label := range metric.Label {
				if label.GetName() != "surface" && label.GetName() != "outcome" && label.GetName() != "phase" {
					t.Fatal("unbounded label added")
				}
			}
		}
	}
	if counts["urnetwork_provider_picker_outcomes_total"] != 9 || counts["urnetwork_provider_picker_read_errors_total"] != 2 {
		t.Fatal("fixed preinitialized schema changed")
	}
}

func TestProviderPickerMetricsInitialRenderedRows(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	metrics.observe("initial", &FindLocationsResult{Locations: []*LocationResult{{LocationType: LocationTypeCity}, {LocationType: LocationTypeRegion}}, Groups: []*LocationGroupResult{{Promoted: false}}}, nil)
	if testutil.ToFloat64(metrics.children["initial/empty"]) != 1 {
		t.Fatal("hidden cities/regions or unpromoted group falsely made initial picker nonempty")
	}
	for _, result := range []*FindLocationsResult{
		{Locations: []*LocationResult{{LocationType: LocationTypeCountry}}},
		{Groups: []*LocationGroupResult{{Promoted: true}}},
		{Devices: []*LocationDeviceResult{{DeviceName: "Synthetic device"}}},
	} {
		metrics.observe("initial", result, nil)
	}
	if testutil.ToFloat64(metrics.children["initial/nonempty"]) != 3 {
		t.Fatal("visible initial row missed")
	}
}

func TestProviderPickerMetricsSearchAndDirectControls(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	metrics.observe("search", &FindLocationsResult{Locations: []*LocationResult{{LocationType: LocationTypeCity}}}, nil)
	metrics.observe("search", &FindLocationsResult{}, nil)
	metrics.observe("direct", &FindLocationsResult{Devices: []*LocationDeviceResult{{DeviceName: "Synthetic device"}}}, nil)
	if testutil.ToFloat64(metrics.children["search/nonempty"]) != 1 || testutil.ToFloat64(metrics.children["search/empty"]) != 1 || testutil.ToFloat64(metrics.children["direct/nonempty"]) != 1 || testutil.ToFloat64(metrics.children["initial/empty"]) != 0 {
		t.Fatal("search/direct response conflated with initial picker")
	}
}

func TestProviderPickerMetricsErrorNeverSuccessfulEmpty(t *testing.T) {
	metrics := newProviderPickerMetricSet()
	metrics.observe("initial", &FindLocationsResult{}, errors.New("synthetic read failure"))
	metrics.observe("initial", nil, nil)
	if testutil.ToFloat64(metrics.children["initial/error"]) != 2 || testutil.ToFloat64(metrics.children["initial/empty"]) != 0 {
		t.Fatal("failed or missing result became empty success")
	}
}

func TestProviderPickerMetricsActualGetReadFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "filter-failure")
		beforeRead := testutil.ToFloat64(providerPickerMetrics.readFilters)
		beforeError := testutil.ToFloat64(providerPickerMetrics.children["initial/error"])
		beforeEmpty := testutil.ToFloat64(providerPickerMetrics.children["initial/empty"])
		_, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if err == nil || testutil.ToFloat64(providerPickerMetrics.readFilters)-beforeRead != 1 || testutil.ToFloat64(providerPickerMetrics.children["initial/error"])-beforeError != 1 || testutil.ToFloat64(providerPickerMetrics.children["initial/empty"]) != beforeEmpty {
			t.Fatal("actual picker failure did not reach read and response counters exactly once")
		}
	})
}

func TestProviderPickerMetricsActualMissIsNotReadError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "missing")
		before := testutil.ToFloat64(providerPickerMetrics.readInitial) + testutil.ToFloat64(providerPickerMetrics.readFilters)
		empty := testutil.ToFloat64(providerPickerMetrics.children["initial/empty"])
		_, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if err != nil || testutil.ToFloat64(providerPickerMetrics.readInitial)+testutil.ToFloat64(providerPickerMetrics.readFilters) != before || testutil.ToFloat64(providerPickerMetrics.children["initial/empty"])-empty != 1 {
			t.Fatal("actual cache miss acquired error or lost empty outcome")
		}
	})
}

func TestProviderPickerMetricsActualDirectDoesNotReadCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "initial-failure")
		before := testutil.ToFloat64(providerPickerMetrics.children["direct/nonempty"])
		result, err := FindProviderLocations(&FindLocationsArgs{Query: server.NewId().String()}, &session.ClientSession{Ctx: ctx})
		if err != nil || result == nil || len(result.Devices) != 1 || testutil.ToFloat64(providerPickerMetrics.children["direct/nonempty"])-before != 1 {
			t.Fatal("direct syntactic device lookup changed or was misclassified")
		}
	})
}
