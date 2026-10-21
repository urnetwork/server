package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProvidersDashboardShowsDeviceVersionsWithSourceFreshness(t *testing.T) {
	dashboard := readTestDashboard(t, "providers.json")
	for id, population := range map[int]string{923: "provider", 924: "client"} {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || panel.Type != "barchart" || len(panel.Targets) != 1 {
			t.Fatalf("missing %s version distribution", population)
		}
		target := panel.Targets[0]
		for _, required := range []string{`population="` + population + `"`, `env="$env"`, `service="taskworker"`, "max by (version)", "topk(1,", "time() - 900", "time() + 30", "and on (env, service, host, block, instance)"} {
			if !strings.Contains(target.Expr, required) {
				t.Errorf("%s chart lacks %s", population, required)
			}
		}
		if !target.Instant || target.Range == nil || *target.Range || !strings.Contains(panel.Description, "unknown") || !strings.Contains(panel.Description, "50") {
			t.Errorf("%s chart lost current snapshot or population documentation", population)
		}
	}
	for id, bucket := range map[int]string{926: "quality", 927: "speed"} {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || panel.FieldConfig.Defaults.Unit != "percentunit" || !strings.Contains(panel.Description, "not a percentage of all online providers") || !strings.Contains(panel.Targets[0].Expr, `bucket="`+bucket+`"`) {
			t.Fatalf("modern %s share lost its explicit candidate denominator", bucket)
		}
	}
}

// Evaluate the actual chart expressions: a disappearing version on an older
// replica must not survive the latest complete snapshot, nor be added twice.
func TestProvidersDeviceVersionPromqlUsesOneFreshSnapshot(t *testing.T) {
	promtool := os.Getenv("URNETWORK_PROMTOOL")
	if promtool == "" {
		var err error
		promtool, err = exec.LookPath("promtool")
		if err != nil {
			t.Fatal("official promtool 3.5.0 is required")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, promtool, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "promtool, version 3.5.0 ") {
		t.Fatal("official promtool 3.5.0 is required")
	}
	series := func(metric, instance, labels, value string) map[string]string {
		return map[string]string{"series": fmt.Sprintf(`%s{env="synthetic",service="taskworker",host="synthetic-host",block="a",instance=%q%s}`, metric, instance, labels), "values": value + "x20"}
	}
	tests := []map[string]any{}
	for id, population := range map[int]string{923: "provider", 924: "client"} {
		expression := strings.ReplaceAll(dashboardPanelById(readTestDashboard(t, "providers.json"), id).Targets[0].Expr, "$env", "synthetic")
		labels := `,population="` + population + `"`
		inputs := []map[string]string{
			series("urnetwork_stats_device_versions_observed_at_seconds", "old", "", "1180"),
			series("urnetwork_stats_device_versions_observed_at_seconds", "new", "", "1190"),
			series("urnetwork_stats_device_versions", "old", labels+`,version="2026.10.20-1"`, "9"),
			series("urnetwork_stats_device_versions", "old", labels+`,version="2026.10.19-1"`, "5"),
			series("urnetwork_stats_device_versions", "new", labels+`,version="2026.10.20-1"`, "3"),
			series("urnetwork_stats_device_versions", "new", labels+`,version="unknown"`, "7"),
		}
		for _, c := range []struct {
			name       string
			timestamps []map[string]string
			samples    []map[string]any
		}{
			{"new snapshot replaces every old bucket", nil, []map[string]any{{"labels": `{version="2026.10.20-1"}`, "value": 3}, {"labels": `{version="unknown"}`, "value": 7}}},
			{"expired snapshots disappear", []map[string]string{series("urnetwork_stats_device_versions_observed_at_seconds", "old", "", "200"), series("urnetwork_stats_device_versions_observed_at_seconds", "new", "", "200")}, []map[string]any{}},
			{"future snapshots disappear", []map[string]string{series("urnetwork_stats_device_versions_observed_at_seconds", "old", "", "1300"), series("urnetwork_stats_device_versions_observed_at_seconds", "new", "", "1300")}, []map[string]any{}},
		} {
			input := append([]map[string]string{}, inputs...)
			if c.timestamps != nil {
				input = append(c.timestamps, inputs[2:]...)
			}
			tests = append(tests, map[string]any{"name": population + ": " + c.name, "interval": "1m", "input_series": input, "promql_expr_test": []map[string]any{{"expr": expression, "eval_time": "20m", "exp_samples": c.samples}}})
		}
	}
	for id, bucket := range map[int]string{926: "quality", 927: "speed"} {
		expression := strings.ReplaceAll(dashboardPanelById(readTestDashboard(t, "providers.json"), id).Targets[0].Expr, "$env", "synthetic")
		labels := `,bucket="` + bucket + `"`
		inputs := []map[string]string{
			series("urnetwork_stats_provider_egress_source_started_seconds", "old", "", "1180"),
			series("urnetwork_stats_provider_egress_source_started_seconds", "new", "", "1190"),
			series("urnetwork_stats_provider_modern_gate_shadow", "old", labels+`,outcome="modern"`, "99"),
			series("urnetwork_stats_provider_modern_gate_shadow", "new", labels+`,outcome="modern"`, "6"),
			series("urnetwork_stats_provider_modern_gate_shadow", "new", labels+`,outcome="not_post_quantum"`, "1"),
			series("urnetwork_stats_provider_modern_gate_shadow", "new", labels+`,outcome="no_resizable_windows"`, "1"),
			series("urnetwork_stats_provider_modern_gate_shadow", "new", labels+`,outcome="no_evidence"`, "1"),
			series("urnetwork_stats_provider_modern_gate_shadow", "new", labels+`,outcome="stale_evidence"`, "1"),
		}
		tests = append(tests, map[string]any{"name": bucket + ": modern share includes unknown outcomes and excludes older replica", "interval": "1m", "input_series": inputs, "promql_expr_test": []map[string]any{{"expr": expression, "eval_time": "20m", "exp_samples": []map[string]any{{"labels": "{}", "value": 0.6}}}}})
	}
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "versions.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("version expression evaluation: %v\n%s", err, output)
	}
}
