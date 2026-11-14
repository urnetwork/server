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

// Evaluate panel 159's own expressions: the zero-carry judge sets its
// providers and verdicts gauges from each run, on whichever taskworker ran
// it, so the chart reads the process whose last-run timestamp is newest. An
// older process's values never survive a newer run, and the chart goes blank
// once no run has finished for fifteen minutes or a run time is ahead of the
// clock.
func TestZeroCarryProvidersPanelReadsTheNewestRun(t *testing.T) {
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

	panel := dashboardPanelById(readTestDashboard(t, "egress-probes.json"), 159)
	if panel == nil || panel.Title != "zero-carry providers by country" {
		t.Fatal("missing the zero-carry providers panel")
	}
	expressions := map[string]string{}
	for _, target := range panel.Targets {
		expressions[target.RefId] = strings.ReplaceAll(target.Expr, "$env", "synthetic")
	}
	series := func(metric, host, labels, value string) map[string]string {
		return map[string]string{
			"series": fmt.Sprintf(`%s{env="synthetic",service="taskworker",host=%q,block="g1",instance=%q%s}`, metric, host, host+":7100", labels),
			"values": value + "x20",
		}
	}
	ran := func(oldAt, newAt string) []map[string]string {
		return []map[string]string{
			series("urnetwork_provider_zero_carry_judge_last_run_timestamp_seconds", "old", "", oldAt),
			series("urnetwork_provider_zero_carry_judge_last_run_timestamp_seconds", "new", "", newAt),
		}
	}
	gauges := []map[string]string{
		series("urnetwork_provider_zero_carry_providers", "old", `,country_code="kz"`, "14"),
		series("urnetwork_provider_zero_carry_providers", "old", `,country_code="np"`, "1"),
		series("urnetwork_provider_zero_carry_providers", "new", `,country_code="kz"`, "13"),
		series("urnetwork_provider_zero_carry_verdicts", "old", "", "52"),
		series("urnetwork_provider_zero_carry_verdicts", "new", "", "50"),
	}
	// evaluated at 20m: a run is fresh after 300 s and no later than 1230 s
	cases := []struct {
		name      string
		ran       []map[string]string
		countries []map[string]any
		verdicts  []map[string]any
	}{
		{"the newest run replaces an older process's values", ran("1080", "1170"),
			[]map[string]any{{"labels": `{country_code="kz"}`, "value": 13}},
			[]map[string]any{{"labels": "{}", "value": 50}}},
		{"no run for fifteen minutes", ran("200", "250"), []map[string]any{}, []map[string]any{}},
		{"a run time ahead of the clock", ran("1300", "1300"), []map[string]any{}, []map[string]any{}},
	}
	tests := []map[string]any{}
	for _, c := range cases {
		checks := []map[string]any{
			{"expr": expressions["A"], "eval_time": "20m", "exp_samples": c.countries},
			{"expr": expressions["B"], "eval_time": "20m", "exp_samples": c.verdicts},
		}
		tests = append(tests, map[string]any{"name": c.name, "interval": "1m", "input_series": append(c.ran, gauges...), "promql_expr_test": checks})
	}
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "zero-carry-providers.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("zero-carry providers panel evaluation: %v\n%s", err, output)
	}
	// a country emptied by the verdicts is an owner's call: the panel says
	// how to read one, and the changes panel how verdicts end
	if !strings.Contains(panel.Description, "lost all its listed supply") {
		t.Error("the zero-carry providers panel does not say how to read an emptied country")
	}
	changes := dashboardPanelById(readTestDashboard(t, "egress-probes.json"), 160)
	if changes == nil || !strings.Contains(changes.Description, "72 hours") || !strings.Contains(changes.Description, "upgrade") {
		t.Error("the zero-carry changes panel does not say how verdicts end")
	}
}
