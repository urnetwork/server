package grafana

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The session authority alerts (warp grafana/alerting/session-authority.yml)
// draw their levels where the session row charts their signals:
//   - the maintenance runs, with the stall level;
//   - the unfinished session work still aging, which is what the journal
//     alerts read: only the taskworker running maintenance shard 0 refreshes
//     the journal gauges, so the oldest-operation panel can hold a stale age;
//   - the oldest unfinished operation, with the journal levels;
//   - and the rule names on the clock, authority loss and record writes
//     panels.
//
// promtool evaluates the two new expressions over synthetic series.
func TestSessionAuthorityPanelsDrawTheAlertLevels(t *testing.T) {
	dashboard := readTestDashboard(t, "signals.json")
	index := func(title string) int {
		t.Helper()
		for i := range dashboard.Panels {
			if dashboard.Panels[i].Title == title {
				return i
			}
		}
		t.Fatalf("signals dashboard lacks the %q panel", title)
		return -1
	}
	levels := func(p *testPanel) []incidentLevel {
		found := []incidentLevel{}
		for _, step := range p.FieldConfig.Defaults.Thresholds.Steps {
			if step.Value != nil {
				found = append(found, incidentLevel{color: step.Color, value: *step.Value})
			}
		}
		return found
	}
	const maintenanceExpr = `sum by (outcome) (rate(urnetwork_taskworker_executions_total{env="$env",task="work.MaintainNetworkSessions"}[5m])) * 60`
	const agingExpr = `max by (state) ((urnetwork_session_journal_oldest_seconds{env="$env"} > 0) and (delta(urnetwork_session_journal_oldest_seconds{env="$env"}[5m]) > 0))`
	for _, want := range []struct {
		title  string
		unit   string
		expr   string
		legend string
		levels []incidentLevel
		words  []string
	}{
		{"session maintenance runs / min by outcome", "short", maintenanceExpr, "{{outcome}} / min", []incidentLevel{warnAt(32)},
			[]string{"SessionMaintenanceStalled", "SessionMaintenanceFailing", "32 successes a minute", "about 62", "Shard 0", "session-authority.yml"}},
		{"unfinished session work still aging", "s", agingExpr, "{{state}}", []incidentLevel{warnAt(60), pageAt(900)},
			[]string{"SessionJournalStuck", "SessionJournalStuckLong", "shard 0", "stale", "60 seconds", "15 minutes"}},
		{"oldest unfinished session operation", "s", `max by (state) (urnetwork_session_journal_oldest_seconds{env="$env"})`, "{{state}}", []incidentLevel{warnAt(60), pageAt(900)},
			[]string{"SessionJournalStuck", "shard 0", "stale"}},
	} {
		p := &dashboard.Panels[index(want.title)]
		if p.Type != "timeseries" || p.FieldConfig.Defaults.Unit != want.unit || len(p.Targets) != 1 ||
			p.Targets[0].Expr != want.expr || p.Targets[0].LegendFormat != want.legend {
			t.Errorf("%s does not chart %s in %s", want.title, want.expr, want.unit)
		}
		if got := levels(p); !slices.Equal(got, want.levels) || p.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode != "line" {
			t.Errorf("%s draws %v as %q, want lines at %v", want.title, got, p.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode, want.levels)
		}
		for _, word := range want.words {
			if !strings.Contains(p.Description, word) {
				t.Errorf("%s does not explain %q", want.title, word)
			}
		}
	}
	// the pending operations a recovery pass could not advance chart beside
	// the backlog they stay in
	const recoveryExpr = `sum(increase(urnetwork_session_operation_recovery_failures_total{env="$env"}[5m]))`
	backlog := dashboard.Panels[index("session journal backlog")]
	if backlog.Type != "timeseries" || backlog.FieldConfig.Defaults.Unit != "short" || len(backlog.Targets) != 2 ||
		backlog.Targets[0].Expr != `max by (state) (urnetwork_session_journal_pending{env="$env"})` ||
		backlog.Targets[1].Expr != recoveryExpr || backlog.Targets[1].LegendFormat != "recovery could not advance (last 5m)" {
		t.Errorf("the session journal backlog does not chart the backlog and %s", recoveryExpr)
	}
	for _, word := range []string{"recovery pass could not advance", "urnetwork_session_operation_recovery_failures_total", "shard 0", "behind the others", "keeps rising"} {
		if !strings.Contains(backlog.Description, word) {
			t.Errorf("the session journal backlog does not explain %q", word)
		}
	}
	clock := dashboard.Panels[index("session clock, authority loss and observation drops")]
	for _, word := range []string{"SessionClockDisagreement", "10 seconds", "SessionClockRefusals", "SessionAuthorityLoss", "reads 0 until minting is on"} {
		if !strings.Contains(clock.Description, word) {
			t.Errorf("the clock panel does not explain %q", word)
		}
	}
	// the two new panels sit after the record panels, inside the session row
	writes := index("revocation record writes / s by kind and outcome")
	if index("session maintenance runs / min by outcome") != writes+1 || index("unfinished session work still aging") != writes+2 ||
		dashboard.Panels[writes+3].Type != "row" {
		t.Error("the maintenance and aging panels are not the last of the sessions and credential state row")
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
	series := func(metric, env, instance, labels, values string) map[string]string {
		return map[string]string{"series": metric + `{env="` + env + `",service="taskworker",host="synthetic-host",block="a",instance="` + instance + `"` + labels + `}`, "values": values}
	}
	expression := func(expr string) string { return strings.ReplaceAll(expr, "$env", "synthetic") }
	inputs := []map[string]string{
		// t1 runs shard 0 and sees a prepared revoke aging since minute 0;
		// t2 last ran shard 0 long ago and keeps a stale 75 seconds
		series("urnetwork_session_journal_oldest_seconds", "synthetic", "t1", `,state="prepared"`, "0+60x20"),
		series("urnetwork_session_journal_oldest_seconds", "synthetic", "t2", `,state="prepared"`, "75x20"),
		series("urnetwork_session_journal_oldest_seconds", "synthetic", "t1", `,state="index"`, "0x20"),
		series("urnetwork_session_journal_oldest_seconds", "synthetic", "t2", `,state="index"`, "31x20"),
		// two taskworkers each run half the shards; one shard fails every run
		series("urnetwork_taskworker_executions_total", "synthetic", "t1", `,task="work.MaintainNetworkSessions",outcome="succeeded"`, "0+31x20"),
		series("urnetwork_taskworker_executions_total", "synthetic", "t2", `,task="work.MaintainNetworkSessions",outcome="succeeded"`, "0+29x20"),
		series("urnetwork_taskworker_executions_total", "synthetic", "t2", `,task="work.MaintainNetworkSessions",outcome="failed"`, "0+2x20"),
		// another task and another environment never show
		series("urnetwork_taskworker_executions_total", "synthetic", "t1", `,task="work.Other",outcome="succeeded"`, "0+600x20"),
		series("urnetwork_taskworker_executions_total", "other", "o1", `,task="work.MaintainNetworkSessions",outcome="succeeded"`, "0+600x20"),
		series("urnetwork_session_journal_oldest_seconds", "other", "o1", `,state="prepared"`, "0+600x20"),
		// shard 0's taskworker fails to advance one operation each pass, twice
		// a minute; another environment never shows
		series("urnetwork_session_operation_recovery_failures_total", "synthetic", "t1", "", "0+2x20"),
		series("urnetwork_session_operation_recovery_failures_total", "other", "o1", "", "0+600x20"),
	}
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": []map[string]any{{
		"name": "session authority panels", "interval": "1m", "input_series": inputs,
		"promql_expr_test": []map[string]any{
			{"expr": expression(agingExpr), "eval_time": "10m", "exp_samples": []map[string]any{
				{"labels": `{state="prepared"}`, "value": 600},
			}},
			{"expr": expression(maintenanceExpr), "eval_time": "10m", "exp_samples": []map[string]any{
				{"labels": `{outcome="succeeded"}`, "value": 60},
				{"labels": `{outcome="failed"}`, "value": 2},
			}},
			{"expr": expression(recoveryExpr), "eval_time": "10m", "exp_samples": []map[string]any{
				{"labels": `{}`, "value": 10},
			}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session_authority.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("session authority expression evaluation: %v\n%s", err, output)
	}
}
