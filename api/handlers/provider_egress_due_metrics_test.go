package handlers

// Isolated collectors prove fixed label domains and idle capability visibility.

import (
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/model"
)

func TestProviderEgressDueMetricsFixedSeriesAndSelectedCounts(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	metrics := newProviderEgressDueCollectors(registry)
	if count := testutil.CollectAndCount(metrics.selected); count != 8 {
		t.Fatalf("idle selected series = %d, want all eight", count)
	}
	if testutil.ToFloat64(metrics.enabled) != 1 || testutil.ToFloat64(metrics.edf) != 1 || testutil.ToFloat64(metrics.requests) != 0 {
		t.Fatal("capabilities and request visibility must be separate from selected counts")
	}
	metrics.observe(model.ProviderEgressDueDiagnostics{})
	diagnostics := model.ProviderEgressDueDiagnostics{}
	diagnostics.Selected[model.ProviderEgressDueStaleHealth] = model.ProviderEgressDueCount{Current: 2, Expired: 1}
	diagnostics.Selected[model.ProviderEgressDueNoLocation] = model.ProviderEgressDueCount{Current: 4}
	metrics.observe(diagnostics)
	if testutil.ToFloat64(metrics.requests) != 2 {
		t.Fatal("empty and nonempty selections must each count one request")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	wantFamilies := map[string]bool{
		"urnetwork_egress_due_selected_total":      false,
		"urnetwork_egress_due_requests_total":      false,
		"urnetwork_egress_due_observation_enabled": false,
		"urnetwork_egress_due_edf_enabled":         false,
	}
	for _, family := range families {
		if _, ok := wantFamilies[family.GetName()]; !ok {
			t.Fatalf("unexpected provider-egress metric family %q", family.GetName())
		}
		wantFamilies[family.GetName()] = true
		for _, metric := range family.Metric {
			if family.GetName() != "urnetwork_egress_due_selected_total" {
				if len(metric.Label) != 0 {
					t.Fatal("capability or request metric gained a label")
				}
				continue
			}
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if len(labels) != 2 || (labels["expired"] != "false" && labels["expired"] != "true") {
				t.Fatal("selected metric escaped its fixed label schema")
			}
			laneIndex := -1
			for lane, name := range providerEgressDueLaneLabels {
				if name == labels["lane"] {
					laneIndex = lane
				}
			}
			if laneIndex < 0 {
				t.Fatal("unselected or caller-controlled lane escaped")
			}
			want := diagnostics.Selected[laneIndex].Current
			if labels["expired"] == "true" {
				want = diagnostics.Selected[laneIndex].Expired
			}
			if metric.Counter.GetValue() != float64(want) {
				t.Fatal("selected count includes an unselected lane or wrong expiry")
			}
		}
	}
	for name, seen := range wantFamilies {
		if !seen {
			t.Fatalf("missing provider-egress metric family %q", name)
		}
	}
}

func TestProviderEgressDueDiagnosticsCannotCarryCallerLabels(t *testing.T) {
	// The handler accepts a fixed array of numeric counts, not strings, maps,
	// provider identifiers, shard values, or labels supplied by the request.
	diagnosticsType := reflect.TypeOf(model.ProviderEgressDueDiagnostics{})
	if diagnosticsType.NumField() != 1 {
		t.Fatal("diagnostics gained an unchecked field")
	}
	selectedType := diagnosticsType.Field(0).Type
	if selectedType.Kind() != reflect.Array || selectedType.Len() != 4 {
		t.Fatal("diagnostics lane domain is no longer fixed")
	}
	countType := selectedType.Elem()
	if countType.NumField() != 2 || countType.Field(0).Type.Kind() != reflect.Int || countType.Field(1).Type.Kind() != reflect.Int {
		t.Fatal("diagnostics must contain only current/expired numeric counts")
	}
}
