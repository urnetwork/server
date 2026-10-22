// the capability probes dashboard reads the provider capability census
// (controller/stats_provider_capabilities.go) from one latest taskworker
// snapshot, and summarizes the probe's own series beside it
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func capabilityProbesPanel(t *testing.T, dashboard testDashboard, title string) *testPanel {
	t.Helper()
	for i := range dashboard.Panels {
		if dashboard.Panels[i].Title == title {
			return &dashboard.Panels[i]
		}
	}
	t.Fatalf("capability probes dashboard has no %q panel", title)
	return nil
}

func TestCapabilityProbesDashboardReadsOneFreshCensus(t *testing.T) {
	dashboard := readTestDashboard(t, "capability-probes.json")
	if dashboard.Uid != "urnetwork-capability-probes" || dashboard.Title != "urnetwork / capability probes" {
		t.Fatalf("capability probes dashboard identity = %q / %q", dashboard.Uid, dashboard.Title)
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("the capability probes dashboard must remain authenticated")
	}
	if len(dashboard.Templating.List) != 1 {
		t.Fatalf("capability probes dashboard has %d template variables, want only env", len(dashboard.Templating.List))
	}

	joined := strings.Join(dashboardExpressions(dashboard), "\n")
	for _, metric := range []string{
		"urnetwork_stats_provider_capabilities{",
		"urnetwork_stats_provider_capability_evidence_age{",
		"urnetwork_stats_provider_capability_key_exchanges{",
		"urnetwork_stats_provider_capabilities_observed_at_seconds{",
		"urnetwork_stats_provider_modern_gate_shadow{",
		"urnetwork_capability_probe_last_pass_timestamp_seconds{",
		"urnetwork_capability_probe_providers_total{",
		"urnetwork_capability_probe_readings_total{",
	} {
		if !strings.Contains(joined, metric) {
			t.Errorf("capability probes dashboard does not read %s", metric)
		}
	}

	// every census read is joined to the one latest fresh snapshot
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			for _, metric := range []string{"urnetwork_stats_provider_capabilities{", "urnetwork_stats_provider_capability_evidence_age{", "urnetwork_stats_provider_capability_key_exchanges{"} {
				count := strings.Count(target.Expr, metric)
				if count == 0 {
					continue
				}
				for _, required := range []string{`env="$env",service="taskworker"`, "topk(1,", "time() - 900", "time() + 30", "and on (env, service, host, block, instance)"} {
					if strings.Count(target.Expr, required) < count {
						t.Errorf("%q reads %s without %s", panel.Title, metric, required)
					}
				}
			}
		}
	}

	for _, title := range []string{"capabilities now", "evidence age now", "key exchange groups now"} {
		panel := capabilityProbesPanel(t, dashboard, title)
		if len(panel.Targets) != 1 || !panel.Targets[0].Instant || panel.Targets[0].Range == nil || *panel.Targets[0].Range {
			t.Errorf("%q is not an instant read of the current census", title)
		}
	}
	for _, title := range []string{"modern share", "post-quantum share", "resizable window share", "evidence coverage", "capability shares"} {
		if unit := capabilityProbesPanel(t, dashboard, title).FieldConfig.Defaults.Unit; unit != "percentunit" {
			t.Errorf("%q renders in %q, want percentunit", title, unit)
		}
	}
	if !strings.Contains(capabilityProbesPanel(t, dashboard, "modern share of the gated buckets").Description, "Not a share of the whole fleet") {
		t.Error("the gated bucket share lost its explicit candidate denominator")
	}
}

// Evaluate the chart expressions: an older replica's census must not be
// added to the latest one, expired and future snapshots disappear, and the
// modern share counts providers without evidence in its denominator.
func TestCapabilityProbesCensusPromqlUsesOneFreshSnapshot(t *testing.T) {
	promtool := os.Getenv("URNETWORK_PROMTOOL")
	if promtool == "" {
		var err error
		promtool, err = exec.LookPath("promtool")
		if err != nil {
			t.Fatal("official promtool 3.5.0 is required; run bash ./grafana/promtool.sh from the server root and export its path as URNETWORK_PROMTOOL")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, promtool, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "promtool, version 3.5.0 ") {
		t.Fatal("official promtool 3.5.0 is required")
	}

	dashboard := readTestDashboard(t, "capability-probes.json")
	expression := func(title string, ref int) string {
		return strings.ReplaceAll(capabilityProbesPanel(t, dashboard, title).Targets[ref].Expr, "$env", "synthetic")
	}
	series := func(metric, instance, labels, value string) map[string]string {
		return map[string]string{"series": fmt.Sprintf(`%s{env="synthetic",service="taskworker",host="synthetic-host",block="a",instance=%q%s}`, metric, instance, labels), "values": value + "x20"}
	}
	const observed = "urnetwork_stats_provider_capabilities_observed_at_seconds"
	const pairs = "urnetwork_stats_provider_capabilities"
	inputs := []map[string]string{
		series(observed, "old", "", "1180"),
		series(observed, "new", "", "1190"),
		series(pairs, "old", `,e2e="post_quantum",window="resizable"`, "99"),
		series(pairs, "new", `,e2e="post_quantum",window="resizable"`, "6"),
		series(pairs, "new", `,e2e="post_quantum",window="fixed"`, "1"),
		series(pairs, "new", `,e2e="no_responder",window="fixed"`, "1"),
		series(pairs, "new", `,e2e="no_evidence",window="unknown"`, "2"),
	}
	expired := []map[string]string{series(observed, "old", "", "200"), series(observed, "new", "", "200")}
	future := []map[string]string{series(observed, "old", "", "1300"), series(observed, "new", "", "1300")}
	evaluate := func(name string, timestamps []map[string]string, expr string, samples []map[string]any) map[string]any {
		input := slices.Clone(inputs)
		if timestamps != nil {
			input = append(slices.Clone(timestamps), inputs[2:]...)
		}
		return map[string]any{"name": name, "interval": "1m", "input_series": input, "promql_expr_test": []map[string]any{{"expr": expr, "eval_time": "20m", "exp_samples": samples}}}
	}
	current := []map[string]any{
		{"labels": `{e2e="post_quantum",window="resizable"}`, "value": 6},
		{"labels": `{e2e="post_quantum",window="fixed"}`, "value": 1},
		{"labels": `{e2e="no_responder",window="fixed"}`, "value": 1},
		{"labels": `{e2e="no_evidence",window="unknown"}`, "value": 2},
	}
	tests := []map[string]any{
		evaluate("new snapshot replaces the old one", nil, expression("capabilities now", 0), current),
		evaluate("expired snapshots disappear", expired, expression("capabilities now", 0), []map[string]any{}),
		evaluate("future snapshots disappear", future, expression("capabilities now", 0), []map[string]any{}),
		evaluate("providers counts the latest census once", nil, expression("providers", 0), []map[string]any{{"labels": "{}", "value": 10}}),
		evaluate("modern share counts providers without evidence", nil, expression("modern share", 0), []map[string]any{{"labels": "{}", "value": 0.6}}),
		evaluate("post-quantum share", nil, expression("post-quantum share", 0), []map[string]any{{"labels": "{}", "value": 0.7}}),
		evaluate("evidence coverage", nil, expression("evidence coverage", 0), []map[string]any{{"labels": "{}", "value": 0.8}}),
		evaluate("trend by e2e reading", nil, expression("providers by end-to-end reading", 0), []map[string]any{
			{"labels": `{e2e="post_quantum"}`, "value": 7},
			{"labels": `{e2e="no_responder"}`, "value": 1},
			{"labels": `{e2e="no_evidence"}`, "value": 2},
		}),
	}
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capabilities.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("capability census expression evaluation: %v\n%s", err, output)
	}
}
