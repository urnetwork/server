// Counters whose series a process starts at their first count: the code
// labels no value in advance, so a series appears at 1 or more with no zero
// before it, and an increase cannot see that first count. Their panels read a
// series that began within the window whole, and the reset-aware increase of
// an older one.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The first-count panel of the competition automation dashboard.
const competitionReceiveCleanupPanel = 16

// Each panel, evaluated by promtool an hour into its series: a series that
// began within the hour reads its whole count, where a plain increase reads 0
// (the defect the form exists for); an older series reads its increase, 0
// without a count; a restart's reset is counted; two processes' new series
// add; another environment is ignored; and an hour later every series reads 0.
func TestCounterPanelsCountTheFirstCountOfANewSeries(t *testing.T) {
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

	panels := []struct {
		dashboard string
		id        int
		metric    string
		// the panel's grouping labels; the last one names each case
		labels []string
	}{
		{dashboard: "taskworker.json", id: ctfStepsPanel, metric: "urnetwork_st_ctf_steps_total", labels: []string{"step", "status"}},
		{dashboard: "taskworker.json", id: ctfAlertsPanel, metric: "urnetwork_st_ctf_alerts_total", labels: []string{"alert"}},
		{dashboard: "taskworker.json", id: ctfDrandPanel, metric: "urnetwork_st_ctf_drand_fetches_total", labels: []string{"result"}},
		{dashboard: "competition-automation.json", id: competitionReceiveCleanupPanel, metric: "urnetwork_competition_receive_cleanup_failures_total", labels: []string{"phase"}},
	}
	variables := strings.NewReplacer(`"$env"`, `"synthetic"`, `"$block"`, `".*"`, `"$host"`, `".*"`)
	windowIncrease := regexp.MustCompile(`increase\(([^\[]+)\[1h\]\)`)
	// one sample a minute from 0: the series that names each case, and when
	// it is read: 120m, an hour into the series, and 175m, after the window
	type series struct {
		env, instance, name, values string
	}
	inputs := []series{
		{env: "synthetic", instance: "w1", name: "new", values: "_x99 2x80"},
		{env: "synthetic", instance: "w1", name: "old", values: "5x180"},
		{env: "synthetic", instance: "w1", name: "grew", values: "0x99 1x80"},
		{env: "synthetic", instance: "w1", name: "restarted", values: "0x69 2x29 stale _x9 1x70"},
		{env: "synthetic", instance: "w1", name: "shared", values: "_x99 1x80"},
		{env: "synthetic", instance: "w2", name: "shared", values: "_x109 1x70"},
		{env: "other", instance: "w1", name: "new", values: "_x99 9x80"},
	}
	firstCounts := map[string]float64{"new": 2, "old": 0, "grew": 1, "restarted": 3, "shared": 2}
	increases := map[string]float64{"new": 0, "old": 0, "grew": 1, "restarted": 3, "shared": 0}
	afterWindow := map[string]float64{"new": 0, "old": 0, "grew": 0, "restarted": 0, "shared": 0}

	tests := []map[string]any{}
	for _, p := range panels {
		dashboard := readTestDashboard(t, p.dashboard)
		panel := dashboardPanelById(dashboard, p.id)
		if panel == nil || len(panel.Targets) != 1 {
			t.Fatalf("%s panel %d is missing its query", p.dashboard, p.id)
		}
		expression := variables.Replace(panel.Targets[0].Expr)
		match := windowIncrease.FindStringSubmatch(expression)
		if match == nil {
			t.Fatalf("%s panel %d has no trailing-hour increase: %s", p.dashboard, p.id, expression)
		}
		grouping := strings.Join(p.labels, ", ")
		plainIncrease := "sum by (" + grouping + ") (increase(" + match[1] + "[1h]))"
		caseLabels := func(name string) string {
			pairs := []string{}
			for index, label := range p.labels {
				labelValue := "x"
				if index == len(p.labels)-1 {
					labelValue = name
				}
				pairs = append(pairs, fmt.Sprintf("%s=%q", label, labelValue))
			}
			return strings.Join(pairs, ",")
		}
		samples := func(nameValues map[string]float64) []map[string]any {
			out := []map[string]any{}
			for _, name := range slices.Sorted(maps.Keys(nameValues)) {
				out = append(out, map[string]any{"labels": "{" + caseLabels(name) + "}", "value": nameValues[name]})
			}
			return out
		}
		inputSeries := []map[string]string{}
		for _, s := range inputs {
			inputSeries = append(inputSeries, map[string]string{
				"series": fmt.Sprintf(`%s{env=%q,service="taskworker",block="b1",host="synthetic-host",instance=%q,%s}`, p.metric, s.env, s.instance, caseLabels(s.name)),
				"values": s.values,
			})
		}
		tests = append(tests, map[string]any{
			"name":         fmt.Sprintf("%s panel %d", p.dashboard, p.id),
			"interval":     "1m",
			"input_series": inputSeries,
			"promql_expr_test": []map[string]any{
				{"expr": expression, "eval_time": "120m", "exp_samples": samples(firstCounts)},
				{"expr": plainIncrease, "eval_time": "120m", "exp_samples": samples(increases)},
				{"expr": expression, "eval_time": "175m", "exp_samples": samples(afterWindow)},
			},
		})
	}
	// Json is also valid yaml; its quoting preserves the exact embedded query.
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "first-count.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("first-count expression evaluation: %v\n%s", err, output)
	}
}
