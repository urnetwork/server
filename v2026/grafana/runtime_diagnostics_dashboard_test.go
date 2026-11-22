// Keeps runtime ownership, admission, and submission observations interpretable.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// Binds each diagnostic panel to its metric units, query semantics, and caveats.
type diagnosticDashboardPanel struct {
	id               int
	unit             string
	targets          []testTarget
	descriptionParts []string
}

// Requires authenticated time series without hiding absent telemetry or process identity.
func assertDiagnosticDashboardPanels(t *testing.T, name string, expectedPanels []diagnosticDashboardPanel) {
	t.Helper()
	dashboard := readTestDashboard(t, name)
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatalf("%s runtime diagnostics must remain authenticated", name)
	}
	for _, expected := range expectedPanels {
		panel := dashboardPanelById(dashboard, expected.id)
		if panel == nil {
			t.Errorf("%s diagnostic panel %d is missing", name, expected.id)
			continue
		}
		if panel.Type != "timeseries" || panel.FieldConfig.Defaults.Unit != expected.unit {
			t.Errorf("%s panel %d must be a %s time series", name, expected.id, expected.unit)
		}
		if len(panel.Targets) != len(expected.targets) {
			t.Errorf("%s panel %d has %d queries, want %d", name, expected.id, len(panel.Targets), len(expected.targets))
			continue
		}
		for index, target := range panel.Targets {
			want := expected.targets[index]
			if target.Expr != want.Expr || target.LegendFormat != want.LegendFormat {
				t.Errorf("%s panel %d target %d = %+v, want %+v", name, expected.id, index, target, want)
			}
			if target.Instant || target.Range != nil && !*target.Range {
				t.Errorf("%s panel %d target %d must remain a range query", name, expected.id, index)
			}
		}
		for _, part := range expected.descriptionParts {
			if !strings.Contains(panel.Description, part) {
				t.Errorf("%s panel %d omits observation boundary %q", name, expected.id, part)
			}
		}
	}
}

// Live resident goroutines and the executable capability are gauges, not work rates.
func TestConnectResidentDashboardPreservesOwnershipAndCapability(t *testing.T) {
	const selector = `{env="$env",service="connect",block=~"$block",host=~"$host",instance!=""}`
	const process = "{{host}} {{block}} {{instance}}"
	assertDiagnosticDashboardPanels(t, "connect.json", []diagnosticDashboardPanel{
		{
			id: 20, unit: "short",
			targets: []testTarget{
				{Expr: "urnetwork_connect_resident_callback_workers" + selector, LegendFormat: "callbacks " + process},
				{Expr: "urnetwork_connect_resident_forward_workers" + selector, LegendFormat: "destination forwards " + process},
				{Expr: "urnetwork_connect_resident_forward_idle_watchers" + selector, LegendFormat: "idle watchers " + process},
				{Expr: "go_goroutines" + selector, LegendFormat: "all goroutines " + process},
			},
			descriptionParts: []string{"instantaneous", "control and lazy forward-ingress", "destination-forward", "idle-timeout", "not completed work", "no-data"},
		},
		{
			id: 21, unit: "short",
			targets: []testTarget{
				{Expr: "urnetwork_connect_resident_lazy_forward_ingress_enabled" + selector, LegendFormat: process},
			},
			descriptionParts: []string{"first destination use", "1", "per process", "not traffic or readiness", "no-data"},
		},
	})
}

// Selection, reporter completion, and persisted provider history are separate boundaries.
func TestEgressAdmissionDashboardPreservesSelectionAndSubmissionBoundaries(t *testing.T) {
	const apiSelector = `{env="$env",service="api",instance!=""}`
	const workerSelector = `{env="$env",service="taskworker",instance!=""}`
	const process = "{{host}} {{block}} {{instance}}"
	assertDiagnosticDashboardPanels(t, "egress-probes.json", []diagnosticDashboardPanel{
		{
			id: 61, unit: "short",
			targets: []testTarget{
				{Expr: "urnetwork_egress_due_observation_enabled" + apiSelector, LegendFormat: "API selection observations " + process},
				{Expr: "urnetwork_egress_due_edf_enabled" + apiSelector, LegendFormat: "API earliest-deadline scheduling " + process},
				{Expr: "urnetwork_egress_probe_submission_observation_enabled" + workerSelector, LegendFormat: "taskworker submission observations " + process},
			},
			descriptionParts: []string{"per process", "1", "earliest absolute deadline", "no-data", "not proof of successful probes"},
		},
		{
			id: 62, unit: "reqps",
			targets: []testTarget{
				{Expr: "rate(urnetwork_egress_due_requests_total" + apiSelector + "[$__rate_interval])", LegendFormat: process},
			},
			descriptionParts: []string{"model selection returned", "empty selections", "not response delivery", "no-data"},
		},
		{
			id: 63, unit: "ops",
			targets: []testTarget{
				{Expr: "sum by (lane, expired) (rate(urnetwork_egress_due_selected_total" + apiSelector + "[$__rate_interval]))", LegendFormat: "{{lane}} expired={{expired}}"},
			},
			descriptionParts: []string{"no-location", "stale-location", "stale-health", "missing-health", "at selection", "not unique providers", "executed probes", "durable writes", "no-data"},
		},
		{
			id: 64, unit: "ops",
			targets: []testTarget{
				{Expr: "sum by (kind, outcome) (rate(urnetwork_egress_probe_submission_outcomes_total" + workerSelector + "[$__rate_interval]))", LegendFormat: "{{kind}} {{outcome}}"},
			},
			descriptionParts: []string{"health", "attempt", "acknowledged", "unsupported", "canceled", "error_or_unknown", "completed reporter calls", "not durable history", "no-data"},
		},
	})
}

// Temporary handoff allowance is neither steady capacity nor proof of unsatisfied demand.
func TestProxyHandoffDashboardPreservesPrivateBudgetObservationBoundaries(t *testing.T) {
	const selector = `{env="$env",service="proxy",block=~"$block",host=~"$host",instance!=""}`
	const process = "{{host}} {{block}} {{instance}}"
	assertDiagnosticDashboardPanels(t, "proxy.json", []diagnosticDashboardPanel{
		{
			id: 37, unit: "short",
			targets: []testTarget{
				{Expr: "urnetwork_proxy_platform_transport_active_handoff_transports" + selector, LegendFormat: process},
			},
			descriptionParts: []string{"temporary", "allowance", "not acquired carriers", "steady-state", "per process", "no-data"},
		},
		{
			id: 38, unit: "bytes",
			targets: []testTarget{
				{Expr: "urnetwork_proxy_platform_transport_active_handoff_bytes" + selector, LegendFormat: process},
			},
			descriptionParts: []string{"temporary", "allowance", "not tracked memory use", "steady-state", "per process", "no-data"},
		},
		{
			id: 39, unit: "short",
			targets: []testTarget{
				{Expr: "urnetwork_proxy_platform_transport_slot_full_pending_h1_devices" + selector, LegendFormat: "slot-full total " + process},
				{Expr: "urnetwork_proxy_platform_transport_slot_full_pending_h1_handoff_devices" + selector, LegendFormat: "handoff pending/active " + process},
				{Expr: "urnetwork_proxy_platform_transport_slot_full_pending_h1_handoff_unsatisfied_devices" + selector, LegendFormat: "handoff + unsatisfied " + process},
				{Expr: "urnetwork_proxy_platform_transport_slot_full_pending_h1_demand_unsatisfied_devices" + selector, LegendFormat: "no handoff + unsatisfied " + process},
				{Expr: "urnetwork_proxy_platform_transport_slot_full_pending_h1_window_unknown_devices" + selector, LegendFormat: "window unknown " + process},
			},
			descriptionParts: []string{"same private budget", "pending or active", "known unsatisfied", "without a handoff", "unknown", "overlap", "not a partition", "no-data"},
		},
	})
}
