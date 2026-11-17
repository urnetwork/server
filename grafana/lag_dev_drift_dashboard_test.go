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

// The lag.dev dashboard's drift test row (lag.dev/DRIFTTEST.md P7, E5, V4, V10,
// W8; lag.dev/ops/drift-test.md §7). The drift counters differ from lag.dev's
// own in two ways that lag.dev's rules (lag_dev_dashboard_test.go) do not fit,
// so the row has rules of its own:
//
//   - their counts are small, a few runs a day for a network during market
//     discovery, and they are the only record of usage (P7, Q9). Most of their
//     series start at their first count in a process, which an increase cannot
//     see, and a process that starts and stops within a day, as across a
//     redeploy, leaves no current series. So each is read per process as its
//     count over the window: its last value in the window less its last value
//     before it, or its whole value when its series began within the window.
//     A process's series is keyed by its random instance (server grafana.go),
//     so it never resets, and the difference is exact;
//   - runs and verdicts carry network_id, which names an Embed customer's
//     network (P7), never a visitor's. The row reads it only for the 10
//     networks with the most runs, so it stays bounded however many networks
//     enable Embed.

// What every drift test metric's name starts with, but the stats gauges'.
const lagDevDriftMetricPrefix = "urnetwork_lag_drift_"

const lagDevDriftStatsMetricPrefix = "urnetwork_stats_lag_drift_"

const lagDevDriftRowTitle = "drift test"

// The drift sweep's gauges, which each taskworker sets for the last pass it
// ran: the runs it left stuck, and when the pass ended.
const (
	lagDevDriftStuckRunsGauge      = "urnetwork_lag_drift_stuck_runs"
	lagDevDriftSweepTimestampGauge = "urnetwork_lag_drift_sweep_timestamp_seconds"
)

// The stuck runs of the taskworker whose pass ended last: another
// taskworker's count is as old as its own last pass.
const lagDevDriftStuckAtNewestPass = `max(urnetwork_lag_drift_stuck_runs{env="$env",service="taskworker"}` +
	` and on (env, service, block, host, instance)` +
	` topk(1, urnetwork_lag_drift_sweep_timestamp_seconds{env="$env",service="taskworker"}))`

// How long ago any taskworker's pass ended.
const lagDevDriftSweepAge = `time() - max(urnetwork_lag_drift_sweep_timestamp_seconds{env="$env",service="taskworker"})`

// The drift row's panels: every panel below its row, which is the dashboard's
// last.
func lagDevDriftPanels(t *testing.T, dashboard testDashboard) []testPanel {
	t.Helper()
	for index, panel := range dashboard.Panels {
		if panel.Type != "row" || panel.Title != lagDevDriftRowTitle {
			continue
		}
		panels := dashboard.Panels[index+1:]
		for _, other := range panels {
			if other.Type == "row" {
				t.Fatalf("row %q follows the drift test row, which must be last", other.Title)
			}
		}
		if len(panels) == 0 {
			t.Fatal("the drift test row has no panels")
		}
		return panels
	}
	t.Fatal("the lag.dev dashboard has no drift test row")
	return nil
}

func lagDevDriftExpressions(t *testing.T) []string {
	t.Helper()
	expressions := []string{}
	for _, panel := range lagDevDriftPanels(t, readTestDashboard(t, "lag-dev.json")) {
		for _, target := range panel.Targets {
			expressions = append(expressions, target.Expr)
		}
	}
	return expressions
}

var lagDevDriftMetricPattern = regexp.MustCompile(`urnetwork_(?:stats_)?lag_drift_[a-z0-9_]+`)

// A drift metric's registered name: a histogram is read by its series.
func lagDevDriftFamily(series string) string {
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if family, ok := strings.CutSuffix(series, suffix); ok {
			return family
		}
	}
	return series
}

// Each question ops/drift-test.md §7 asks of the panels, and the query parts
// one expression must hold to answer it.
func TestLagDevDriftRowAnswersEachQuestion(t *testing.T) {
	expressions := lagDevDriftExpressions(t)
	for _, question := range []struct {
		question string
		parts    []string
	}{
		{"runs per day", []string{"sum((last_over_time(urnetwork_lag_drift_runs_total{", "[1d]"}},
		{"verdicts per day", []string{"sum by (verdict) (", "urnetwork_lag_drift_verdicts_total{", "[1d]"}},
		{"runs per day by network", []string{"topk(10, sum by (network_id) (", "urnetwork_lag_drift_runs_total{", "[1d]"}},
		{"verdicts per day by network", []string{"sum by (network_id) (", "urnetwork_lag_drift_verdicts_total{", "and on (network_id) topk(10, "}},
		{"the verdict mix, inconclusive by reason", []string{"sum by (verdict, reason) (", "urnetwork_lag_drift_verdict_reasons_total{"}},
		{"stop reasons", []string{"sum by (stop) (", "urnetwork_lag_drift_run_outcomes_total{"}},
		{"runs ending reports_missing or stopped_early", []string{"urnetwork_lag_drift_run_outcomes_total{", `outcome=~"reports_missing|stopped_early"`}},
		{"processing latency, finish to complete", []string{"histogram_quantile(0.9, sum by (le) (", "urnetwork_lag_drift_processing_seconds_bucket{"}},
		{"runs stuck processing", []string{lagDevDriftStuckAtNewestPass}},
		{"whether the sweep runs", []string{lagDevDriftSweepAge}},
		{"tests failed", []string{"urnetwork_lag_drift_failures_total{"}},
		{"demo starts", []string{"urnetwork_lag_drift_starts_total{", `outcome="demo"`}},
		{"demo reports dropped", []string{"urnetwork_lag_drift_demo_reports_total{"}},
		{"start refusals by a rate limit", []string{"urnetwork_lag_drift_starts_total{", `outcome="refused_limit"`}},
		{"allocation refusals by a rate limit", []string{"urnetwork_lag_drift_allocations_total{", `outcome="refused_limit"`}},
		{"calibration freshness", []string{`max(urnetwork_stats_lag_drift_calibration_age_seconds{env="$env"})`}},
	} {
		answered := slices.ContainsFunc(expressions, func(expression string) bool {
			for _, part := range question.parts {
				if !strings.Contains(expression, part) {
					return false
				}
			}
			return true
		})
		if !answered {
			t.Errorf("no drift panel answers %q (wants %q)", question.question, question.parts)
		}
	}
}

// Every drift metric the code registers is on the drift row, every one the row
// reads is registered, and no other panel reads one, so lag.dev's rules and
// the drift row's each hold where they apply.
func TestLagDevDriftRowCoversTheDriftMetrics(t *testing.T) {
	dashboard := readTestDashboard(t, "lag-dev.json")
	driftPanels := lagDevDriftPanels(t, dashboard)
	read := map[string]bool{}
	for _, panel := range driftPanels {
		for _, target := range panel.Targets {
			for _, series := range lagDevDriftMetricPattern.FindAllString(target.Expr, -1) {
				read[lagDevDriftFamily(series)] = true
			}
		}
	}
	registered := map[string]bool{}
	for _, metric := range registeredApplicationMetrics(t) {
		if strings.HasPrefix(metric, lagDevDriftMetricPrefix) || strings.HasPrefix(metric, lagDevDriftStatsMetricPrefix) {
			registered[metric] = true
			if !read[metric] {
				t.Errorf("drift metric %s is not on the drift row", metric)
			}
		}
	}
	for _, metric := range slices.Sorted(maps.Keys(read)) {
		if !registered[metric] {
			t.Errorf("the drift row reads %s, which no code registers", metric)
		}
	}

	for _, panel := range dashboard.Panels[:len(dashboard.Panels)-len(driftPanels)] {
		for _, target := range panel.Targets {
			if lagDevDriftMetricPattern.MatchString(target.Expr) {
				t.Errorf("panel %q outside the drift row reads a drift metric: %s", panel.Title, target.Expr)
			}
		}
	}
}

// One counter's count over a window, per process: the last value in the
// window less the last value before it, or the whole value of a series that
// began within it. Groups: the three selectors and the four windows, which
// must agree.
var lagDevDriftCountPattern = regexp.MustCompile(`\(last_over_time\((urnetwork_lag_drift_[a-z0-9_]+\{[^{}\s]*\})\[(1h|1d)\]\)` +
	` - last_over_time\((urnetwork_lag_drift_[a-z0-9_]+\{[^{}\s]*\})\[(1h|1d)\] offset (1h|1d)\)\)` +
	` or last_over_time\((urnetwork_lag_drift_[a-z0-9_]+\{[^{}\s]*\})\[(1h|1d)\]\)`)

var lagDevDriftSumPrefixPattern = regexp.MustCompile(`(?:sum\(|sum by \([a-z_, ]+\) \()$`)

// Every drift counter, and the latency histogram's buckets, is read as each
// process's count over the window, summed across processes and scoped to the
// env. The sweep's gauges are read only as the newest pass's stuck runs, and
// as the time since the newest pass.
func TestLagDevDriftRowCountsEveryCountOfEachProcess(t *testing.T) {
	for _, expression := range lagDevDriftExpressions(t) {
		occurrences := 0
		for _, series := range lagDevDriftMetricPattern.FindAllString(expression, -1) {
			if strings.HasPrefix(series, lagDevDriftMetricPrefix) && series != lagDevDriftStuckRunsGauge && series != lagDevDriftSweepTimestampGauge {
				occurrences += 1
			}
		}
		counts := lagDevDriftCountPattern.FindAllStringSubmatchIndex(expression, -1)
		if occurrences != 3*len(counts) {
			t.Errorf("a drift counter is read other than as each process's count over the window: %s", expression)
		}
		for _, count := range counts {
			group := func(index int) string {
				return expression[count[2*index]:count[2*index+1]]
			}
			if group(1) != group(3) || group(1) != group(6) {
				t.Errorf("a count reads different selectors: %s", expression[count[0]:count[1]])
			}
			if group(2) != group(4) || group(2) != group(5) || group(2) != group(7) {
				t.Errorf("a count reads different windows: %s", expression[count[0]:count[1]])
			}
			if !strings.Contains(group(1), `{env="$env"`) {
				t.Errorf("a count is not scoped to $env: %s", group(1))
			}
			if !lagDevDriftSumPrefixPattern.MatchString(expression[:count[0]]) {
				t.Errorf("a count is not summed across processes: %s", expression)
			}
		}
		stuckReads := strings.Count(expression, lagDevDriftStuckAtNewestPass)
		ageReads := strings.Count(expression, lagDevDriftSweepAge)
		if len(metricOccurrences(expression, lagDevDriftStuckRunsGauge)) != stuckReads {
			t.Errorf("the stuck runs gauge is read other than as the newest pass's: %s", expression)
		}
		if len(metricOccurrences(expression, lagDevDriftSweepTimestampGauge)) != stuckReads+ageReads {
			t.Errorf("the sweep timestamp is read other than to pick the newest pass or to age it: %s", expression)
		}
	}
}

// The drift row groups by the bounded labels of the drift metrics: outcome,
// verdict, reason, stop and le, and by network only for the 10 networks with
// the most runs. A process's labels only match the sweep's two gauges under a
// max, which keeps no process. Nothing names a visitor, an address, an
// extender, a test or a token (DRIFTTEST.md R1, ops/drift-test.md §10).
func TestLagDevDriftRowGroupsOnlyByBoundedLabels(t *testing.T) {
	bounded := map[string]bool{
		"outcome": true,
		"verdict": true,
		"reason":  true,
		"stop":    true,
		"le":      true,
	}
	byClause := regexp.MustCompile(`by \(([^)]*)\)`)
	topNetworks := "topk(10, sum by (network_id) ((last_over_time(urnetwork_lag_drift_runs_total{"
	for _, expression := range lagDevDriftExpressions(t) {
		for _, match := range byClause.FindAllStringSubmatch(expression, -1) {
			for _, label := range strings.Split(match[1], ",") {
				label = strings.TrimSpace(label)
				if label == "network_id" {
					continue
				}
				if !bounded[label] {
					t.Errorf("drift query groups by %q, which is not a bounded drift label: %s", label, expression)
				}
			}
		}
		if strings.Contains(expression, "network_id") {
			if !strings.HasPrefix(expression, topNetworks) && !strings.Contains(expression, " and on (network_id) "+topNetworks) {
				t.Errorf("drift query names the network without the top 10 bound: %s", expression)
			}
			for _, series := range lagDevDriftMetricPattern.FindAllString(expression, -1) {
				if series != "urnetwork_lag_drift_runs_total" && series != "urnetwork_lag_drift_verdicts_total" {
					t.Errorf("drift query groups %s by network, which it does not carry: %s", series, expression)
				}
			}
		}
		for _, unbounded := range []string{"extender_id", "client_id", "track_id", "drift_test_id", "token", "client_ip", "without ("} {
			if strings.Contains(expression, unbounded) {
				t.Errorf("drift query names %q: %s", unbounded, expression)
			}
		}
		if strings.Count(expression, "instance") != strings.Count(expression, lagDevDriftStuckAtNewestPass) {
			t.Errorf("drift query names the instance other than to match the newest pass's process: %s", expression)
		}
	}
}

// Two drift panels evaluated by promtool on synthetic per-process series. The
// stops panel, an hour into its series: a series that began within the hour
// reads its whole count, which an increase misses; an older one its growth, 0
// without; a process that started and stopped within the hour its whole count;
// one whose pushes paused across the hour's start only its growth; two
// processes add; another environment is ignored; and an hour later every
// remaining series reads 0. The network table: the 10 networks with the most
// runs in the day, across processes, and the verdicts of those networks only.
// The stuck runs: the count of the taskworker whose pass ended last, however
// many another taskworker found at its own last pass.
func TestLagDevDriftRowPromqlCountsEachProcess(t *testing.T) {
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

	dashboard := readTestDashboard(t, "lag-dev.json")
	panelExpression := func(title string, refId string) string {
		for _, panel := range lagDevDriftPanels(t, dashboard) {
			if panel.Title != title {
				continue
			}
			for _, target := range panel.Targets {
				if target.RefId == refId {
					return strings.ReplaceAll(target.Expr, `"$env"`, `"synthetic"`)
				}
			}
		}
		t.Fatalf("no drift panel %q with query %s", title, refId)
		return ""
	}
	process := func(env string, instance string) string {
		return fmt.Sprintf(`env=%q,service="api",block="b1",host="synthetic-host",instance=%q`, env, instance)
	}
	samples := func(label string, values map[string]float64) []map[string]any {
		out := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(values)) {
			out = append(out, map[string]any{"labels": fmt.Sprintf("{%s=%q}", label, name), "value": values[name]})
		}
		return out
	}

	// one sample a minute from 0, each case its own stop
	stops := panelExpression("drift run stops per hour", "A")
	stopSeries := []map[string]string{}
	for _, s := range []struct{ env, instance, stop, values string }{
		{"synthetic", "p1", "new", "_x99 2x80"},
		{"synthetic", "p1", "old", "5x180"},
		{"synthetic", "p1", "grew", "0x99 1x80"},
		{"synthetic", "p1", "ended", "_x70 1x10 3x10 _x88"},
		{"synthetic", "p1", "paused", "4x50 _x15 5x114"},
		{"synthetic", "p1", "shared", "_x99 1x80"},
		{"synthetic", "p2", "shared", "_x109 1x70"},
		{"other", "p1", "new", "_x99 9x80"},
	} {
		stopSeries = append(stopSeries, map[string]string{
			"series": fmt.Sprintf(`urnetwork_lag_drift_run_outcomes_total{%s,outcome="counted",stop=%q}`, process(s.env, s.instance), s.stop),
			"values": s.values,
		})
	}

	// one sample an hour from 0: network nK has K runs from hour 30, and n12
	// one more in a second process; n0 has none in the day, and another
	// environment's network many
	runs := panelExpression("drift tests by network, last day", "A")
	agree := panelExpression("drift tests by network, last day", "B")
	networkSeries := []map[string]string{
		{"series": fmt.Sprintf(`urnetwork_lag_drift_runs_total{%s,network_id="n0"}`, process("synthetic", "p1")), "values": "100x50"},
		{"series": fmt.Sprintf(`urnetwork_lag_drift_runs_total{%s,network_id="n12"}`, process("synthetic", "p2")), "values": "_x40 1x10"},
		{"series": fmt.Sprintf(`urnetwork_lag_drift_runs_total{%s,network_id="nother"}`, process("other", "p1")), "values": "_x29 1000x21"},
		{"series": fmt.Sprintf(`urnetwork_lag_drift_verdicts_total{%s,network_id="n12",verdict="agree"}`, process("synthetic", "p1")), "values": "_x29 4x21"},
		{"series": fmt.Sprintf(`urnetwork_lag_drift_verdicts_total{%s,network_id="n12",verdict="disagree"}`, process("synthetic", "p1")), "values": "_x29 2x21"},
		{"series": fmt.Sprintf(`urnetwork_lag_drift_verdicts_total{%s,network_id="n1",verdict="agree"}`, process("synthetic", "p1")), "values": "_x29 1x21"},
	}
	topRuns := map[string]float64{}
	for network := 1; network <= 12; network++ {
		networkSeries = append(networkSeries, map[string]string{
			"series": fmt.Sprintf(`urnetwork_lag_drift_runs_total{%s,network_id="n%d"}`, process("synthetic", "p1"), network),
			"values": fmt.Sprintf("_x29 %dx21", network),
		})
		if 3 <= network {
			topRuns[fmt.Sprintf("n%d", network)] = float64(network)
		}
	}
	topRuns["n12"] += 1

	// one sample a minute from 0: t1 ran a pass each minute, half a minute
	// in, until minute 20, and found 2 runs stuck at each; t2 runs one each
	// minute throughout and finds none; the api pushes the gauges at 0
	stuck := panelExpression("drift runs stuck and tests failed", "A")
	taskworker := func(instance string) string {
		return fmt.Sprintf(`env="synthetic",service="taskworker",block="b1",host="synthetic-host",instance=%q`, instance)
	}
	sweepSeries := []map[string]string{
		{"series": "urnetwork_lag_drift_stuck_runs{" + taskworker("t1") + "}", "values": "2x60"},
		{"series": "urnetwork_lag_drift_sweep_timestamp_seconds{" + taskworker("t1") + "}", "values": "30+60x20 1230x40"},
		{"series": "urnetwork_lag_drift_stuck_runs{" + taskworker("t2") + "}", "values": "0x60"},
		{"series": "urnetwork_lag_drift_sweep_timestamp_seconds{" + taskworker("t2") + "}", "values": "0+60x60"},
		{"series": "urnetwork_lag_drift_stuck_runs{" + process("synthetic", "a1") + "}", "values": "0x60"},
		{"series": "urnetwork_lag_drift_sweep_timestamp_seconds{" + process("synthetic", "a1") + "}", "values": "0x60"},
	}
	newestPass := func(value float64) []map[string]any {
		return []map[string]any{{"labels": "{}", "value": value}}
	}

	fixture, err := json.Marshal(map[string]any{
		"evaluation_interval": "1m",
		"tests": []map[string]any{
			{
				"name":         "drift run stops per hour",
				"interval":     "1m",
				"input_series": stopSeries,
				"promql_expr_test": []map[string]any{
					{"expr": stops, "eval_time": "120m", "exp_samples": samples("stop", map[string]float64{"new": 2, "old": 0, "grew": 1, "ended": 3, "paused": 1, "shared": 2})},
					{"expr": stops, "eval_time": "175m", "exp_samples": samples("stop", map[string]float64{"new": 0, "old": 0, "grew": 0, "paused": 0, "shared": 0})},
				},
			},
			{
				"name":         "drift tests by network, last day",
				"interval":     "1h",
				"input_series": networkSeries,
				"promql_expr_test": []map[string]any{
					{"expr": runs, "eval_time": "48h", "exp_samples": samples("network_id", topRuns)},
					{"expr": agree, "eval_time": "48h", "exp_samples": samples("network_id", map[string]float64{"n12": 4})},
				},
			},
			{
				"name":         "drift runs stuck at the newest pass",
				"interval":     "1m",
				"input_series": sweepSeries,
				"promql_expr_test": []map[string]any{
					{"expr": stuck, "eval_time": "10m", "exp_samples": newestPass(2)},
					{"expr": stuck, "eval_time": "50m", "exp_samples": newestPass(0)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// json is also valid yaml, and its quoting keeps each query exact
	path := filepath.Join(t.TempDir(), "drift-row.yml")
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("drift row evaluation: %v\n%s", err, output)
	}
}
