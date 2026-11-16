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

// Evaluate panel 151's own expressions: the TLS judge sets its findings and
// undecided gauges from each judged run, on whichever taskworker ran it, so
// the chart reads the process whose last-judged timestamp is newest. An older
// process's values never survive a newer run, and the chart goes blank once
// no run has judged for fifteen minutes or a judged time is ahead of the
// clock.
func TestEgressTlsFindingsPanelReadsTheNewestJudgedRun(t *testing.T) {
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

	panel := dashboardPanelById(readTestDashboard(t, "egress-probes.json"), 151)
	if panel == nil {
		t.Fatal("missing the unresolved TLS findings panel")
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
	judged := func(oldAt, newAt string) []map[string]string {
		return []map[string]string{
			series("urnetwork_egress_tls_judge_last_judged_timestamp_seconds", "old", "", oldAt),
			series("urnetwork_egress_tls_judge_last_judged_timestamp_seconds", "new", "", newAt),
		}
	}
	gauges := []map[string]string{
		series("urnetwork_egress_tls_findings", "old", `,verdict="pending"`, "9"),
		series("urnetwork_egress_tls_findings", "old", `,verdict="provider"`, "4"),
		series("urnetwork_egress_tls_findings", "old", `,verdict="retired"`, "2"),
		series("urnetwork_egress_tls_findings", "new", `,verdict="pending"`, "1"),
		series("urnetwork_egress_tls_findings", "new", `,verdict="provider"`, "5"),
		series("urnetwork_egress_tls_findings", "new", `,verdict="retired"`, "0"),
		series("urnetwork_egress_tls_undecided_findings", "old", "", "7"),
		series("urnetwork_egress_tls_undecided_findings", "new", "", "1"),
		series("urnetwork_egress_tls_place_withheld_pairs", "old", `,reason="budget"`, "6"),
		series("urnetwork_egress_tls_place_withheld_pairs", "new", `,reason="budget"`, "2"),
		series("urnetwork_egress_tls_place_withheld_pairs", "new", `,reason="error"`, "0"),
	}
	// evaluated at 20m: a judged run is fresh after 300 s and no later than 1230 s
	cases := []struct {
		name      string
		judged    []map[string]string
		verdicts  []map[string]any
		undecided []map[string]any
		withheld  []map[string]any
	}{
		{"the newest judged run replaces an older process's values", judged("1080", "1170"),
			[]map[string]any{{"labels": `{verdict="pending"}`, "value": 1}, {"labels": `{verdict="provider"}`, "value": 5}, {"labels": `{verdict="retired"}`, "value": 0}},
			[]map[string]any{{"labels": "{}", "value": 1}},
			[]map[string]any{{"labels": `{reason="budget"}`, "value": 2}, {"labels": `{reason="error"}`, "value": 0}}},
		{"no run judged for fifteen minutes", judged("200", "250"), []map[string]any{}, []map[string]any{}, []map[string]any{}},
		{"a judged time ahead of the clock", judged("1300", "1300"), []map[string]any{}, []map[string]any{}, []map[string]any{}},
	}
	tests := []map[string]any{}
	for _, c := range cases {
		checks := []map[string]any{{"expr": expressions["A"], "eval_time": "20m", "exp_samples": c.verdicts}}
		if expression, ok := expressions["B"]; ok {
			checks = append(checks, map[string]any{"expr": expression, "eval_time": "20m", "exp_samples": c.undecided})
		}
		if expression, ok := expressions["C"]; ok {
			checks = append(checks, map[string]any{"expr": expression, "eval_time": "20m", "exp_samples": c.withheld})
		}
		tests = append(tests, map[string]any{"name": c.name, "interval": "1m", "input_series": append(c.judged, gauges...), "promql_expr_test": checks})
	}
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tls-findings.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("TLS findings panel evaluation: %v\n%s", err, output)
	}
	if _, ok := expressions["B"]; !ok {
		t.Fatal("the TLS findings panel does not chart the undecided findings")
	}
	if _, ok := expressions["C"]; !ok {
		t.Fatal("the TLS findings panel does not chart the pairs whose place evidence was withheld")
	}
	if !strings.Contains(panel.Description, "§2.31") || !strings.Contains(panel.Description, "undecided") {
		t.Fatal("the TLS findings panel does not point at the judge's monitor signal")
	}
}
