// The prober shard retention's taskworker panels: passes, contracts and rows
// stay scoped to the taskworker under the model's fixed labels, and the stats
// read the run that started last.
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

// The prober shard retention row's panel ids on the taskworker dashboard.
const (
	proberRetentionRowPanel         = 69
	proberRetentionModePanel        = 70
	proberRetentionGenerationsPanel = 71
	proberRetentionCutoffPanel      = 72
	proberRetentionDuePanel         = 73
	proberRetentionLagPanel         = 74
	proberRetentionNewestRunPanel   = 75
	proberRetentionPassesPanel      = 76
	proberRetentionContractsPanel   = 77
	proberRetentionDeletedPanel     = 78
	proberRetentionBacklogPanel     = 79
	proberRetentionStepPanel        = 80
	proberRetentionRunsPanel        = 81
)

// The label matchers every retention selector starts with.
const proberRetentionTaskworkerFilter = `env="$env",service="taskworker",block=~"$block",host=~"$host",instance!=""`

// The process whose run started last, while it started within 30 minutes.
const proberRetentionNewestRun = `topk(1, urnetwork_prober_retention_run_timestamp_seconds{` + proberRetentionTaskworkerFilter + `} > time() - 1800)`

// Every retention query reads taskworker series under the dashboard's filters,
// groups only by the model's fixed labels, and reads a counter or histogram
// only through a rate or an increase. Every retention metric the model
// registers is charted in the row, each outcome, reason, stop, step and table
// is explained beside its series, and each gauge reads the newest run.
func TestProberRetentionDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("prober retention diagnostics must remain authenticated")
	}
	row := dashboardPanelById(dashboard, proberRetentionRowPanel)
	if row == nil || row.Type != "row" || row.Title != "prober shard retention (model/prober_shard_retention.go)" {
		t.Fatalf("taskworker dashboard lacks the prober shard retention row: %+v", row)
	}

	selectors := regexp.MustCompile(`\{[^}]*\}`)
	counters := regexp.MustCompile(`(\w*\()?(urnetwork_prober_retention_\w*(?:_total|_bucket|_count))\{`)
	groupings := regexp.MustCompile(`\b(by|on) \(([^)]*)\)`)
	fixedLabels := []string{"outcome", "reason", "table", "step", "stop", "le"}
	var queries strings.Builder
	for id := proberRetentionModePanel; id <= proberRetentionRunsPanel; id++ {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("prober retention panel %d is missing", id)
		}
		if panel.GridPos.Y <= row.GridPos.Y {
			t.Errorf("panel %q sits above its row", panel.Title)
		}
		if len(panel.Targets) == 0 {
			t.Errorf("panel %q has no query", panel.Title)
		}
		for _, target := range panel.Targets {
			queries.WriteString(target.Expr)
			queries.WriteByte('\n')
			for _, selector := range selectors.FindAllString(target.Expr, -1) {
				if !strings.HasPrefix(selector, "{"+proberRetentionTaskworkerFilter) {
					t.Errorf("panel %q reads %s outside the taskworker filters", panel.Title, selector)
				}
			}
			for _, match := range counters.FindAllStringSubmatch(target.Expr, -1) {
				if match[1] != "rate(" && match[1] != "increase(" {
					t.Errorf("panel %q reads the counter %s without a rate or an increase", panel.Title, match[2])
				}
			}
			for _, match := range groupings.FindAllStringSubmatch(target.Expr, -1) {
				if match[1] == "on" {
					if match[2] != "env, service, block, host, instance" {
						t.Errorf("panel %q matches on %q, not one taskworker process", panel.Title, match[2])
					}
					continue
				}
				for _, label := range strings.Split(match[2], ", ") {
					if !slices.Contains(fixedLabels, label) {
						t.Errorf("panel %q groups by %q, outside the model's fixed labels", panel.Title, label)
					}
				}
			}
			if panel.Type == "stat" && (!target.Instant || target.Range == nil || *target.Range) {
				t.Errorf("stat %q must be an instant query", panel.Title)
			}
		}
	}
	retentionMetrics := []string{}
	for _, metric := range registeredApplicationMetrics(t) {
		if strings.HasPrefix(metric, "urnetwork_prober_retention_") {
			retentionMetrics = append(retentionMetrics, metric)
		}
	}
	if !slices.Contains(retentionMetrics, "urnetwork_prober_retention_run_timestamp_seconds") {
		t.Fatalf("the metric inventory lacks the run clock the stats select by: %v", retentionMetrics)
	}
	for _, metric := range retentionMetrics {
		if !strings.Contains(queries.String(), metric) {
			t.Errorf("the prober retention row does not chart %s", metric)
		}
	}

	expressions := func(id int) []string {
		panel := dashboardPanelById(dashboard, id)
		values := []string{}
		for _, target := range panel.Targets {
			values = append(values, target.Expr)
		}
		return values
	}
	rate := func(grouping string, metric string) string {
		return grouping + "(rate(" + metric + "{" + proberRetentionTaskworkerFilter + "}[$__rate_interval]))"
	}
	for _, c := range []struct {
		id    int
		title string
		want  []string
	}{
		{proberRetentionPassesPanel, "passes / s by outcome and refusals by reason", []string{
			rate("sum by (outcome) ", "urnetwork_prober_retention_shards_total"),
			rate("sum by (reason) ", "urnetwork_prober_retention_blocked_shards_total"),
		}},
		{proberRetentionContractsPanel, "contracts deleted and kept / s by outcome", []string{
			rate("sum by (outcome) ", "urnetwork_prober_retention_contracts_total"),
			rate("sum", "urnetwork_prober_retention_archived_contracts_total"),
		}},
		{proberRetentionDeletedPanel, "rows deleted / s by table", []string{
			rate("sum by (table) ", "urnetwork_prober_retention_deleted_rows_total"),
		}},
		{proberRetentionRunsPanel, "runs and failed steps in the trailing hour", []string{
			"sum by (stop) (increase(urnetwork_prober_retention_runs_total{" + proberRetentionTaskworkerFilter + "}[1h]))",
			"sum by (step) (increase(urnetwork_prober_retention_batch_duration_seconds_count{" + proberRetentionTaskworkerFilter + `,outcome="error"}[1h]))`,
		}},
	} {
		panel := dashboardPanelById(dashboard, c.id)
		if panel.Title != c.title || panel.Type != "timeseries" || !slices.Equal(expressions(c.id), c.want) {
			t.Errorf("panel %d lost its per-label rates: %q %v", c.id, panel.Title, expressions(c.id))
		}
	}
	step := dashboardPanelById(dashboard, proberRetentionStepPanel)
	for index, quantile := range []string{"0.5", "0.99"} {
		want := "histogram_quantile(" + quantile + ", sum by (le, step) (rate(urnetwork_prober_retention_batch_duration_seconds_bucket{" + proberRetentionTaskworkerFilter + "}[$__rate_interval])))"
		if len(step.Targets) != 2 || step.Targets[index].Expr != want || step.FieldConfig.Defaults.Unit != "s" {
			t.Errorf("step duration panel lost its p%s by step: %+v", strings.TrimPrefix(quantile, "0."), step.Targets)
		}
	}

	// the model's fixed label values (model/prober_shard_retention.go,
	// model/prober_shard_model.go), each named where its series is charted
	for _, c := range []struct {
		id     int
		values []string
	}{
		{proberRetentionPassesPanel, []string{
			"dropped", "blocked", "error", "deferred", "gone", "dry_run",
			"active", "unresolved", "unsettled", "balance", "hosted", "connected",
			"maxShardsPerMinute",
		}},
		{proberRetentionContractsPanel, []string{"deleted", "kept_bytes", "kept_unsettled", "busy", "eligible_dry_run", "st_provider_usage_archive", "maxContractsPerSecond"}},
		{proberRetentionRunsPanel, []string{"caught_up", "budget", "off", "dry_run", "control_unavailable", "canceled", "error"}},
		{proberRetentionStepPanel, []string{"select", "contracts", "identity", "registry"}},
		{proberRetentionDeletedPanel, []string{
			"contract_close_report", "contract_close", "transfer_escrow", "transfer_escrow_sweep",
			"contract_extender", "transfer_contract", "client_tls_certificate", "network_client_role",
			"network_client_provider_intent", "network_client_connection",
			"network_client_location_reliability", "provide_key", "network_client", "device",
			"transfer_balance", "network", "network_user_auth_seedphrase", "network_user", "prober_shard_run",
		}},
		{proberRetentionModePanel, []string{"{prober_retention}control", "off", "dry run", "on"}},
	} {
		panel := dashboardPanelById(dashboard, c.id)
		for _, value := range c.values {
			if !strings.Contains(panel.Description, value) {
				t.Errorf("panel %q does not explain %s", panel.Title, value)
			}
		}
	}

	// the gauges read the newest run, the run start does not need to
	for _, c := range []struct {
		id   int
		unit string
	}{
		{proberRetentionModePanel, "none"},
		{proberRetentionGenerationsPanel, "none"},
		{proberRetentionCutoffPanel, "dateTimeAsIso"},
		{proberRetentionDuePanel, "none"},
		{proberRetentionLagPanel, "s"},
		{proberRetentionBacklogPanel, "short"},
	} {
		panel := dashboardPanelById(dashboard, c.id)
		if panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("panel %q unit = %q, want %q", panel.Title, panel.FieldConfig.Defaults.Unit, c.unit)
		}
		if !strings.Contains(panel.Description, "within the last 30 minutes") {
			t.Errorf("panel %q does not say it reads only a current run", panel.Title)
		}
		for _, target := range panel.Targets {
			if !strings.Contains(target.Expr, " and on (env, service, block, host, instance) "+proberRetentionNewestRun+")") {
				t.Errorf("panel %q does not read the newest run: %s", panel.Title, target.Expr)
			}
		}
	}
	newestRun := dashboardPanelById(dashboard, proberRetentionNewestRunPanel)
	if newestRun.FieldConfig.Defaults.Unit != "dateTimeAsIso" || len(newestRun.Targets) != 1 ||
		newestRun.Targets[0].Expr != "max(urnetwork_prober_retention_run_timestamp_seconds{"+proberRetentionTaskworkerFilter+"} > 0) * 1000" {
		t.Errorf("newest run panel must read the newest clock of any taskworker: %+v", newestRun.Targets)
	}
	backlog := dashboardPanelById(dashboard, proberRetentionBacklogPanel)
	lag := dashboardPanelById(dashboard, proberRetentionLagPanel)
	due := dashboardPanelById(dashboard, proberRetentionDuePanel)
	if len(backlog.Targets) != 2 || backlog.Targets[0].Expr != due.Targets[0].Expr || backlog.Targets[1].Expr != lag.Targets[0].Expr {
		t.Errorf("backlog panel must chart the due stat's and the lag stat's queries: %+v", backlog.Targets)
	}
	lagAxis := map[string]string{}
	for _, override := range backlog.FieldConfig.Overrides {
		if override.Matcher.Options != "B" {
			continue
		}
		for _, property := range override.Properties {
			lagAxis[property.Id] = strings.Trim(string(property.Value), `"`)
		}
	}
	if lagAxis["unit"] != "s" || lagAxis["custom.axisPlacement"] != "right" || len(backlog.FieldConfig.Overrides) != 1 {
		t.Errorf("the lag must take a right axis in seconds beside the passes due: %+v", backlog.FieldConfig.Overrides)
	}

	// the mode reads as its name
	documentBytes, err := dashboardsFs.ReadFile("dashboards/taskworker.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Panels []struct {
			Id          int `json:"id"`
			FieldConfig struct {
				Defaults struct {
					Mappings []struct {
						Type    string `json:"type"`
						Options map[string]struct {
							Text string `json:"text"`
						} `json:"options"`
					} `json:"mappings"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(documentBytes, &document); err != nil {
		t.Fatal(err)
	}
	modeNames := map[string]string{}
	for _, panel := range document.Panels {
		if panel.Id != proberRetentionModePanel {
			continue
		}
		for _, mapping := range panel.FieldConfig.Defaults.Mappings {
			if mapping.Type != "value" {
				continue
			}
			for value, option := range mapping.Options {
				modeNames[value] = option.Text
			}
		}
	}
	if modeNames["0"] != "off" || modeNames["1"] != "dry run" || modeNames["2"] != "on" || len(modeNames) != 3 {
		t.Errorf("mode stat must name model.ProberShardRetentionMode's values: %v", modeNames)
	}
}

// Each taskworker keeps the gauges of the last retention run it made, so the
// stats read the taskworker whose run started last, by the clock it published
// with its backlog. That holds when an older run reports a higher mode, more
// generations, a later cutoff, a longer backlog and a longer lag; a 0 mode
// still reads; nothing reads once the newest run is 30 minutes old, but the
// newest start does; a run whose oldest pass is after its cutoff reads no lag;
// and other services are ignored.
func TestProberRetentionPromqlReadsTheNewestRun(t *testing.T) {
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
			t.Fatalf("prober retention panel %d is missing", id)
		}
		for _, target := range panel.Targets {
			if target.RefId == refId {
				return variables.Replace(target.Expr)
			}
		}
		t.Fatalf("prober retention panel %d has no target %s", id, refId)
		return ""
	}

	// One process's retention gauges as its last run left them. start is the
	// run's clock.
	type retentionRun struct {
		service     string
		instance    string
		start       float64
		mode        float64
		generations float64
		cutoff      float64
		oldest      float64
		due         float64
	}
	inputs := func(runs ...retentionRun) []map[string]string {
		series := []map[string]string{}
		for _, run := range runs {
			gauge := func(name string, value float64) map[string]string {
				return map[string]string{
					"series": fmt.Sprintf(`urnetwork_prober_retention_%s{env="synthetic",service=%q,block="b1",host="synthetic-host",instance=%q}`, name, run.service, run.instance),
					"values": strconv.FormatFloat(value, 'f', -1, 64) + "x60",
				}
			}
			series = append(series,
				gauge("run_timestamp_seconds", run.start),
				gauge("mode", run.mode),
				gauge("retained_generations", run.generations),
				gauge("cutoff_timestamp_seconds", run.cutoff),
				gauge("oldest_shard_timestamp_seconds", run.oldest),
				gauge("due_shards", run.due),
			)
		}
		return series
	}
	const day = 86400
	// evaluated at 3000 s: a run is current when it started after 1200 s
	older := retentionRun{service: "taskworker", instance: "older", start: 1500, mode: 2, generations: 30, cutoff: 9 * day, oldest: 2 * day, due: 900}
	newest := retentionRun{service: "taskworker", instance: "newest", start: 2400, mode: 1, generations: 8, cutoff: 8 * day, oldest: 8*day - 7200, due: 12}
	idle := retentionRun{service: "taskworker", instance: "idle"}
	api := retentionRun{service: "api", instance: "api", start: 2900, mode: 2, generations: 30, cutoff: 20 * day, oldest: day, due: 5000}
	newestOff := newest
	newestOff.mode = 0
	staleOlder := older
	staleOlder.start = 600
	staleNewest := newest
	staleNewest.start = 1100
	// every pass is closed: the oldest is the run's own clock, after its cutoff
	caughtUp := newest
	caughtUp.due = 0
	caughtUp.oldest = 8*day + 3*3600

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
		runs        []retentionRun
		evaluations []evaluation
	}{
		{name: "the newest run wins over higher older values and other services", runs: []retentionRun{older, newest, idle, api}, evaluations: []evaluation{
			{proberRetentionModePanel, "A", value(1)},
			{proberRetentionGenerationsPanel, "A", value(8)},
			{proberRetentionCutoffPanel, "A", value(8 * day * 1000)},
			{proberRetentionDuePanel, "A", value(12)},
			{proberRetentionLagPanel, "A", value(7200)},
			{proberRetentionNewestRunPanel, "A", value(2400 * 1000)},
			{proberRetentionBacklogPanel, "A", value(12)},
			{proberRetentionBacklogPanel, "B", value(7200)},
		}},
		{name: "an off mode still reads", runs: []retentionRun{older, newestOff}, evaluations: []evaluation{
			{proberRetentionModePanel, "A", value(0)},
		}},
		{name: "a run 30 minutes old reads nothing", runs: []retentionRun{staleOlder, staleNewest}, evaluations: []evaluation{
			{proberRetentionModePanel, "A", none},
			{proberRetentionGenerationsPanel, "A", none},
			{proberRetentionCutoffPanel, "A", none},
			{proberRetentionDuePanel, "A", none},
			{proberRetentionLagPanel, "A", none},
			{proberRetentionBacklogPanel, "A", none},
			{proberRetentionBacklogPanel, "B", none},
			{proberRetentionNewestRunPanel, "A", value(1100 * 1000)},
		}},
		{name: "a caught-up run reads no lag over an older lagging one", runs: []retentionRun{older, caughtUp}, evaluations: []evaluation{
			{proberRetentionLagPanel, "A", value(0)},
			{proberRetentionDuePanel, "A", value(0)},
			{proberRetentionBacklogPanel, "B", value(0)},
		}},
		{name: "no run reads nothing", runs: []retentionRun{idle}, evaluations: []evaluation{
			{proberRetentionModePanel, "A", none},
			{proberRetentionNewestRunPanel, "A", none},
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
	path := filepath.Join(t.TempDir(), "prober-retention.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("prober retention expression evaluation: %v\n%s", err, output)
	}
}
