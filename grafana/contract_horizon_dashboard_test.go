// The contract horizon reaper's taskworker panels: deletions, protections and
// the pass position stay scoped to the taskworker, and the stats read the run
// that started last.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The reaper and retention panel ids on the taskworker dashboard.
const (
	contractHorizonModePanel      = 52
	contractHorizonHorizonPanel   = 53
	contractHorizonLagPanel       = 54
	contractHorizonCursorPanel    = 55
	contractHorizonPassStartPanel = 56
	contractHorizonNewestRunPanel = 57
	contractHorizonDeletedPanel   = 58
	contractHorizonKeptPanel      = 59
	contractHorizonPositionPanel  = 60
	contractHorizonBatchPanel     = 62
	contractHorizonRunsPanel      = 63
	contractHorizonRetentionPanel = 65
)

// The label matchers every reaper and retention selector starts with.
const contractHorizonTaskworkerFilter = `env="$env",service="taskworker",block=~"$block",host=~"$host",instance!=""`

// Every reaper and retention query reads taskworker series under the
// dashboard's filters, and reads a counter or histogram only through a rate or
// an increase. The three panels an operator watches while the reaper runs are
// rows deleted by table, contracts kept by reason, and the pass's lag beside
// its cursor and horizon. The runs panel also counts the transfer audit
// rollups that resumed at the first day the reaper cannot reach.
func TestContractHorizonReaperDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("reaper diagnostics must remain authenticated")
	}
	selectors := regexp.MustCompile(`\{[^}]*\}`)
	counters := regexp.MustCompile(`(\w*\()?(urnetwork_(?:contract_horizon|retention|transfer_audit_rollup)_\w*(?:_total|_bucket|_count))\{`)
	for id := contractHorizonModePanel; id <= contractHorizonRetentionPanel; id++ {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("reaper panel %d is missing", id)
		}
		for _, target := range panel.Targets {
			for _, selector := range selectors.FindAllString(target.Expr, -1) {
				if !strings.HasPrefix(selector, "{"+contractHorizonTaskworkerFilter) {
					t.Errorf("panel %q reads %s outside the taskworker filters", panel.Title, selector)
				}
			}
			for _, match := range counters.FindAllStringSubmatch(target.Expr, -1) {
				if match[1] != "rate(" && match[1] != "increase(" {
					t.Errorf("panel %q reads the counter %s without a rate or an increase", panel.Title, match[2])
				}
			}
			if panel.Type == "stat" && (!target.Instant || target.Range == nil || *target.Range) {
				t.Errorf("stat %q must be an instant query", panel.Title)
			}
		}
	}

	deleted := dashboardPanelById(dashboard, contractHorizonDeletedPanel)
	if deleted.Title != "reaper rows deleted / s by table" ||
		deleted.Targets[0].Expr != "sum by (table) (rate(urnetwork_contract_horizon_deleted_rows_total{"+contractHorizonTaskworkerFilter+"}[$__rate_interval]))" {
		t.Errorf("deleted rows panel lost its per-table rate: %+v", deleted.Targets)
	}
	kept := dashboardPanelById(dashboard, contractHorizonKeptPanel)
	if kept.Title != "contracts kept / s by reason" ||
		kept.Targets[0].Expr != "sum by (reason) (rate(urnetwork_contract_horizon_protected_contracts_total{"+contractHorizonTaskworkerFilter+"}[$__rate_interval]))" {
		t.Errorf("kept contracts panel lost its per-reason rate: %+v", kept.Targets)
	}
	for _, reason := range []string{"open", "unresolved", "recent_close", "busy", "unsettled", "payment_hold", "payment_incomplete", "payment_recent", "unpaid_legacy", "plan_staged", "block_points", "payment_review"} {
		if !strings.Contains(kept.Description, reason) {
			t.Errorf("kept contracts panel does not explain %s", reason)
		}
	}
	runs := dashboardPanelById(dashboard, contractHorizonRunsPanel)
	clamps := "sum(increase(urnetwork_transfer_audit_rollup_clamped_total{" + contractHorizonTaskworkerFilter + "}[1h]))"
	if len(runs.Targets) != 3 || runs.Targets[2].RefId != "C" || runs.Targets[2].Expr != clamps {
		t.Errorf("runs panel must count the transfer audit rollups clamped in the trailing hour: %+v", runs.Targets)
	}
	if !strings.Contains(runs.Description, "TransferAuditRollupFirstDay") {
		t.Error("runs panel does not explain the transfer audit rollup clamp")
	}

	position := dashboardPanelById(dashboard, contractHorizonPositionPanel)
	if position.Type != "timeseries" || position.FieldConfig.Defaults.Unit != "s" || len(position.Targets) != 3 ||
		!strings.Contains(position.Targets[0].Expr, "urnetwork_contract_horizon_lag_seconds") ||
		!strings.Contains(position.Targets[1].Expr, "urnetwork_contract_horizon_cursor_timestamp_seconds") ||
		!strings.Contains(position.Targets[2].Expr, "urnetwork_contract_horizon_horizon_timestamp_seconds") {
		t.Fatalf("pass position panel must chart the lag in seconds beside the cursor and the horizon: %+v", position.Targets)
	}
	dates := map[string]bool{}
	for _, override := range position.FieldConfig.Overrides {
		unit, right := "", false
		for _, property := range override.Properties {
			switch property.Id {
			case "unit":
				unit = strings.Trim(string(property.Value), `"`)
			case "custom.axisPlacement":
				right = string(property.Value) == `"right"`
			}
		}
		if unit == "dateTimeAsIso" && right {
			dates[override.Matcher.Options] = true
		}
	}
	if !dates["B"] || !dates["C"] || len(dates) != 2 {
		t.Errorf("the cursor and the horizon must share a right date axis, the lag stays on the left: %v", dates)
	}
}

// Each taskworker keeps the gauges of the last reaper run it made, so the
// stats and the pass position read the taskworker whose run started last. That
// holds when the newest run has the longer horizon, and so the older horizon
// time; a 0 mode still reads; nothing reads once the newest run is 30 minutes
// old, but the newest start does; a run without a committed cursor never
// borrows an older process's; and other services are ignored.
func TestContractHorizonReaperPromqlReadsTheNewestRun(t *testing.T) {
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

	dashboard := readTestDashboard(t, "taskworker.json")
	variables := strings.NewReplacer(`"$env"`, `"synthetic"`, `"$block"`, `".*"`, `"$host"`, `".*"`)
	expression := func(id int, refId string) string {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("reaper panel %d is missing", id)
		}
		for _, target := range panel.Targets {
			if target.RefId == refId {
				return variables.Replace(target.Expr)
			}
		}
		t.Fatalf("reaper panel %d has no target %s", id, refId)
		return ""
	}

	const day = 86400
	// One process's reaper gauges as its last run left them. start is the
	// run's clock: the published horizon time plus the horizon.
	type reaperRun struct {
		service     string
		instance    string
		start       float64
		horizonDays float64
		mode        float64
		lag         float64
		cursor      float64
		passStart   float64
		batchSize   float64
	}
	inputs := func(runs ...reaperRun) []map[string]string {
		series := []map[string]string{}
		for _, run := range runs {
			gauge := func(name string, value float64) map[string]string {
				return map[string]string{
					"series": fmt.Sprintf(`urnetwork_contract_horizon_%s{env="synthetic",service=%q,block="b1",host="synthetic-host",instance=%q}`, name, run.service, run.instance),
					"values": strconv.FormatFloat(value, 'f', -1, 64) + "x60",
				}
			}
			horizon := run.horizonDays * day
			series = append(series,
				gauge("horizon_seconds", horizon),
				gauge("horizon_timestamp_seconds", run.start-horizon),
				gauge("mode", run.mode),
				gauge("lag_seconds", run.lag),
				gauge("cursor_timestamp_seconds", run.cursor),
				gauge("pass_start_timestamp_seconds", run.passStart),
				gauge("batch_size", run.batchSize),
			)
		}
		return series
	}
	// evaluated at 3000 s: a run is current when it started after 1200 s
	older := reaperRun{service: "taskworker", instance: "older", start: 1500, horizonDays: 35, mode: 2, lag: 86400, cursor: 5000, passStart: 100, batchSize: 2000}
	newest := reaperRun{service: "taskworker", instance: "newest", start: 2400, horizonDays: 91, mode: 1, lag: 3600, cursor: 7000, passStart: 200, batchSize: 500}
	idle := reaperRun{service: "taskworker", instance: "idle"}
	api := reaperRun{service: "api", instance: "api", start: 2900, horizonDays: 50, mode: 2, lag: 1, cursor: 9999, passStart: 9999, batchSize: 9999}
	newestOff := newest
	newestOff.mode = 0
	staleOlder := older
	staleOlder.start = 600
	staleNewest := newest
	staleNewest.start = 1100
	uncommitted := newest
	uncommitted.mode = 2
	uncommitted.lag = 0
	uncommitted.cursor = 0
	uncommitted.batchSize = 0

	value := func(v float64) []map[string]any {
		return []map[string]any{{"labels": "{}", "value": v}}
	}
	none := []map[string]any{}
	type evaluation struct {
		id      int
		refId   string
		samples []map[string]any
	}
	cases := []struct {
		name        string
		runs        []reaperRun
		evaluations []evaluation
	}{
		{name: "the newest run wins with the longer horizon", runs: []reaperRun{older, newest, idle, api}, evaluations: []evaluation{
			{contractHorizonModePanel, "A", value(1)},
			{contractHorizonHorizonPanel, "A", value(91)},
			{contractHorizonLagPanel, "A", value(3600)},
			{contractHorizonCursorPanel, "A", value(7000 * 1000)},
			{contractHorizonPassStartPanel, "A", value(200 * 1000)},
			{contractHorizonNewestRunPanel, "A", value(2400 * 1000)},
			{contractHorizonPositionPanel, "A", value(3600)},
			{contractHorizonPositionPanel, "B", value(7000 * 1000)},
			{contractHorizonPositionPanel, "C", value((2400 - 91*day) * 1000)},
			{contractHorizonBatchPanel, "C", value(500)},
		}},
		{name: "an off mode still reads", runs: []reaperRun{older, newestOff}, evaluations: []evaluation{
			{contractHorizonModePanel, "A", value(0)},
		}},
		{name: "a run 30 minutes old reads nothing", runs: []reaperRun{staleOlder, staleNewest}, evaluations: []evaluation{
			{contractHorizonModePanel, "A", none},
			{contractHorizonHorizonPanel, "A", none},
			{contractHorizonLagPanel, "A", none},
			{contractHorizonCursorPanel, "A", none},
			{contractHorizonPositionPanel, "B", none},
			{contractHorizonNewestRunPanel, "A", value(1100 * 1000)},
		}},
		{name: "an uncommitted pass never borrows an older cursor", runs: []reaperRun{older, uncommitted}, evaluations: []evaluation{
			{contractHorizonModePanel, "A", value(2)},
			{contractHorizonLagPanel, "A", none},
			{contractHorizonCursorPanel, "A", none},
			{contractHorizonPositionPanel, "A", none},
			{contractHorizonBatchPanel, "C", none},
		}},
	}
	tests := []map[string]any{}
	for _, c := range cases {
		expressionTests := []map[string]any{}
		for _, e := range c.evaluations {
			expressionTests = append(expressionTests, map[string]any{"expr": expression(e.id, e.refId), "eval_time": "50m", "exp_samples": e.samples})
		}
		tests = append(tests, map[string]any{"name": c.name, "interval": "1m", "input_series": inputs(c.runs...), "promql_expr_test": expressionTests})
	}
	// Json is also valid yaml; its quoting preserves the exact embedded query.
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "contract-horizon.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("reaper expression evaluation: %v\n%s", err, output)
	}
}
