package grafana

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Redis revocation record's reads and writes chart beside the other
// credential state signals, and each panel explains its outcomes: a read the
// record cannot answer refuses the credential, readers stay off until vault
// auth.yml sets revocation_record_enabled while writers always write, and a
// failed revocation write fails the revocation itself. promtool evaluates the
// charted expressions over synthetic counters.
func TestRevocationRecordPanelsChartReadAndWriteOutcomes(t *testing.T) {
	dashboard := readTestDashboard(t, "signals.json")
	panel := func(title string) *testPanel {
		for index := range dashboard.Panels {
			if dashboard.Panels[index].Title == title {
				return &dashboard.Panels[index]
			}
		}
		t.Fatalf("signals dashboard lacks the %q panel", title)
		return nil
	}
	checks := panel("revocation record checks / s by outcome")
	writes := panel("revocation record writes / s by kind and outcome")
	for _, chart := range []struct {
		panel  *testPanel
		expr   string
		legend string
		words  []string
	}{
		{checks, `sum by (service, outcome) (rate(urnetwork_jwt_state_record_checks_total{env="$env"}[$__rate_interval]))`, "{{service}} {{outcome}}",
			[]string{"record_valid", "record_refused", "record_miss", "record_unavailable", "refused the credential as unavailable",
				"Readers stay off", "auth.yml", "revocation_record_enabled"}},
		{writes, `sum by (service, kind, outcome) (rate(urnetwork_session_revocation_record_writes_total{env="$env"}[$__rate_interval]))`, "{{service}} {{kind}} {{outcome}}",
			[]string{"client", "session", "credential", "network", "projection", "failure", "stale",
				"fails the revocation itself", "because Redis was unavailable", "Writers always write", "revocation_record_enabled"}},
	} {
		if len(chart.panel.Targets) != 1 || chart.panel.Targets[0].Expr != chart.expr || chart.panel.Targets[0].LegendFormat != chart.legend {
			t.Errorf("%s does not chart %s", chart.panel.Title, chart.expr)
		}
		for _, word := range chart.words {
			if !strings.Contains(chart.panel.Description, word) {
				t.Errorf("%s does not explain %q", chart.panel.Title, word)
			}
		}
	}

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
	series := func(metric, env, labels, values string) map[string]string {
		return map[string]string{"series": metric + `{env="` + env + `",service="api",host="synthetic-host",block="a",instance="i"` + labels + `}`, "values": values}
	}
	inputs := []map[string]string{
		// the callers, operations and credential shapes of one outcome add up
		series("urnetwork_jwt_state_record_checks_total", "synthetic", `,caller="api",operation="validate",credential="client",outcome="record_unavailable"`, "0+60x20"),
		series("urnetwork_jwt_state_record_checks_total", "synthetic", `,caller="connect",operation="refresh",credential="account",outcome="record_unavailable"`, "0+120x20"),
		series("urnetwork_jwt_state_record_checks_total", "synthetic", `,caller="api",operation="validate",credential="client",outcome="record_valid"`, "0+30x20"),
		series("urnetwork_session_revocation_record_writes_total", "synthetic", `,kind="client",outcome="failure"`, "0+60x20"),
		series("urnetwork_session_revocation_record_writes_total", "synthetic", `,kind="projection",outcome="stale"`, "0+6x20"),
		// another environment never shows
		series("urnetwork_jwt_state_record_checks_total", "other", `,caller="api",operation="validate",credential="client",outcome="record_unavailable"`, "0+600x20"),
		series("urnetwork_session_revocation_record_writes_total", "other", `,kind="client",outcome="failure"`, "0+600x20"),
	}
	expression := func(panel *testPanel) string {
		return strings.NewReplacer("$env", "synthetic", "$__rate_interval", "5m").Replace(panel.Targets[0].Expr)
	}
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": []map[string]any{{
		"name": "revocation record outcomes per service", "interval": "1m", "input_series": inputs,
		"promql_expr_test": []map[string]any{
			{"expr": expression(checks), "eval_time": "10m", "exp_samples": []map[string]any{
				{"labels": `{outcome="record_unavailable",service="api"}`, "value": 3},
				{"labels": `{outcome="record_valid",service="api"}`, "value": 0.5},
			}},
			{"expr": expression(writes), "eval_time": "10m", "exp_samples": []map[string]any{
				{"labels": `{kind="client",outcome="failure",service="api"}`, "value": 1},
				{"labels": `{kind="projection",outcome="stale",service="api"}`, "value": 0.1},
			}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "revocation_record.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("revocation record expression evaluation: %v\n%s", err, output)
	}
}
