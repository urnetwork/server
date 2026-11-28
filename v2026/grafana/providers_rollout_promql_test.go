// The rollout panel's embedded expressions run against synthetic process
// populations in the official Prometheus expression evaluator.
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

// The full grafana test suite requires official promtool 3.5.0 on the path or
// at URNETWORK_PROMTOOL. This isolated evaluator opens no metrics connection.
func TestProvidersRankMeasuredFirstRolloutPromql(t *testing.T) {
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
		t.Fatalf("require official promtool 3.5.0: %v\n%s", err, version)
	}
	panel := dashboardPanelById(readTestDashboard(t, "providers.json"), 77)
	if panel == nil || panel.Title != "rank measured first: on" || len(panel.Targets) != 2 {
		t.Fatal("rank measured first rollout panel is missing its two expressions")
	}
	expressions := map[string]string{}
	for _, target := range panel.Targets {
		expressions[target.RefId] = strings.ReplaceAll(target.Expr, "$env", "synthetic")
	}
	if expressions["A"] == "" || expressions["B"] == "" {
		t.Fatal("rollout panel must retain the every-process and any-process targets")
	}
	series := func(metric, service, host, instance, values string) map[string]string {
		return map[string]string{
			"series": fmt.Sprintf(`%s{env="synthetic",service=%q,host=%q,instance=%q}`, metric, service, host, instance),
			"values": values,
		}
	}
	const processMetric = "process_start_time_seconds"
	const enabledMetric = "urnetwork_findproviders2_rank_measured_first_enabled"
	firstProcess := series(processMetric, "api", "synthetic-host", "first", "1 1")
	secondProcess := series(processMetric, "api", "synthetic-host", "second", "1 1")
	firstOn := series(enabledMetric, "api", "synthetic-host", "first", "1 1")
	secondOn := series(enabledMetric, "api", "synthetic-host", "second", "1 1")
	secondOff := series(enabledMetric, "api", "synthetic-host", "second", "0 0")
	proxyProcess := series(processMetric, "proxy", "synthetic-host", "first", "1 1")
	proxyOn := series(enabledMetric, "proxy", "synthetic-host", "first", "1 1")
	samples := func(values map[string]float64) []map[string]any {
		result := []map[string]any{}
		for _, service := range []string{"api", "proxy"} {
			if value, ok := values[service]; ok {
				result = append(result, map[string]any{"labels": fmt.Sprintf(`{service=%q}`, service), "value": value})
			}
		}
		return result
	}
	tests := []map[string]any{}
	for _, c := range []struct {
		name   string
		inputs []map[string]string
		every  map[string]float64
		any    map[string]float64
	}{
		{name: "old binary has no gauge", inputs: []map[string]string{firstProcess, secondProcess, firstOn}, every: map[string]float64{"api": 0}, any: map[string]float64{"api": 1}},
		{name: "second process is explicitly off", inputs: []map[string]string{firstProcess, secondProcess, firstOn, secondOff}, every: map[string]float64{"api": 0}, any: map[string]float64{"api": 1}},
		{name: "every process is on", inputs: []map[string]string{firstProcess, secondProcess, firstOn, secondOn}, every: map[string]float64{"api": 1}, any: map[string]float64{"api": 1}},
		{name: "every binary lacks the gauge", inputs: []map[string]string{firstProcess, secondProcess}, every: map[string]float64{"api": 0}},
		{name: "inventory is unavailable", inputs: []map[string]string{firstOn}, any: map[string]float64{"api": 1}},
		{name: "replacement instance cannot inherit the old gauge", inputs: []map[string]string{secondProcess, firstOn}, every: map[string]float64{"api": 0}, any: map[string]float64{"api": 1}},
		{name: "services have independent inventories", inputs: []map[string]string{firstProcess, secondProcess, firstOn, proxyProcess, proxyOn}, every: map[string]float64{"api": 0, "proxy": 1}, any: map[string]float64{"api": 1, "proxy": 1}},
		{name: "same instance on another host still needs a gauge", inputs: []map[string]string{firstProcess, firstOn, series(processMetric, "api", "synthetic-other-host", "first", "1 1")}, every: map[string]float64{"api": 0}, any: map[string]float64{"api": 1}},
		{name: "drained process no longer blocks after staleness", inputs: []map[string]string{firstProcess, firstOn, series(processMetric, "api", "synthetic-host", "second", "1 stale")}, every: map[string]float64{"api": 1}, any: map[string]float64{"api": 1}},
		{name: "unrelated service is excluded", inputs: []map[string]string{firstProcess, firstOn, series(processMetric, "taskworker", "synthetic-host", "second", "1 1")}, every: map[string]float64{"api": 1}, any: map[string]float64{"api": 1}},
		{name: "empty inventory remains unknown"},
	} {
		tests = append(tests, map[string]any{
			"name": c.name, "interval": "1m", "input_series": c.inputs,
			"promql_expr_test": []map[string]any{
				{"expr": expressions["A"], "eval_time": "1m", "exp_samples": samples(c.every)},
				{"expr": expressions["B"], "eval_time": "1m", "exp_samples": samples(c.any)},
			},
		})
	}
	// Json is also valid yaml; its quoting preserves the exact embedded query.
	fixture, err := json.MarshalIndent(map[string]any{"evaluation_interval": "1m", "tests": tests}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(t.TempDir(), "providers-rollout.yml")
	if err := os.WriteFile(fixturePath, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, promtool, "test", "rules", fixturePath).CombinedOutput()
	if err != nil {
		t.Fatalf("rollout expression evaluation failed: %v\n%s", err, output)
	}
}
