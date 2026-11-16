// Raw-series fixtures exercise the exact production query in a real PromQL
// engine. Parser-only controls cannot certify PromQL matching or freshness.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const redisRatesPromqlEngineFixture = "testdata/redis_rates_promql_engine_test.go.txt"

// This separate gate requires a pinned Mimir checkout with its vendored
// Prometheus engine. A skipped gate is unqualified, never a successful proof.
// No Prometheus dependency is added to the Server module.
func TestRedisRatesPromqlEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("unqualified: MONITOR_PROMQL_ENGINE_SOURCE is required for the actual Redis cohort engine gate")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(redisRatesPromqlEngineFixture)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	engineTest := filepath.Join(directory, "redis_rates_promql_engine_test.go")
	if err := os.WriteFile(engineTest, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(directory, "redis_rates.responses.json")
	scriptPath := filepath.Join(directory, "redis_rates.test")
	if err := os.WriteFile(scriptPath, []byte(redisRatesPromqlScript()), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestRedisRatesConfiguredCohort$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "MONITOR_PROMQL_TEST_SCRIPT="+scriptPath, "MONITOR_PROMQL_TEST_RESPONSES="+responsePath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual Redis cohort engine gate: %v\n%s", err, output)
	}
	responseData, err := os.ReadFile(responsePath)
	if err != nil {
		t.Fatal(err)
	}
	responses := map[string]json.RawMessage{}
	if err := json.Unmarshal(responseData, &responses); err != nil {
		t.Fatal(err)
	}
	// These are real engine vectors serialized as Mimir envelopes, not rows
	// manufactured by the parser fixtures above.
	for _, test := range []struct {
		name        string
		raw         int
		oneMinute   int
		fiveMinutes int
		unknown     bool
	}{
		{name: "complete", raw: 3, oneMinute: 3, fiveMinutes: 3},
		{name: "substitution", raw: 2, oneMinute: 2, fiveMinutes: 2},
		{name: "duplicate-publisher", unknown: true},
		{name: "zero", raw: 3, oneMinute: 3, fiveMinutes: 3},
		{name: "reset", raw: 3, oneMinute: 3, fiveMinutes: 3},
		{name: "stale", raw: 2, oneMinute: 2, fiveMinutes: 2},
		{name: "fresh-boundary", raw: 3, oneMinute: 2, fiveMinutes: 3},
		{name: "single-sample", raw: 3, oneMinute: 2, fiveMinutes: 2},
		{name: "sparse", raw: 3, oneMinute: 0, fiveMinutes: 3},
		{name: "all-stale", raw: 0, oneMinute: 0, fiveMinutes: 0},
		{name: "wrong-job", raw: 2, oneMinute: 2, fiveMinutes: 2},
		{name: "wrong-environment", raw: 2, oneMinute: 2, fiveMinutes: 2},
		{name: "reversed-order", raw: 3, oneMinute: 3, fiveMinutes: 3},
	} {
		response, ok := responses[test.name]
		if !ok {
			t.Fatalf("native result omitted %s", test.name)
		}
		coverage, err := parseRedisRateCoverage(string(response), "synthetic", redisRatesExpectedTestSources())
		if test.unknown {
			if err == nil {
				t.Fatalf("native %s response was accepted", test.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("native %s response: %v", test.name, err)
		}
		for _, metric := range redisRateMetrics {
			if coverage.count(metric.short, "raw") != test.raw ||
				coverage.count(metric.short, "1m") != test.oneMinute ||
				coverage.count(metric.short, "5m") != test.fiveMinutes {
				t.Fatalf("native %s source reduction: %s", test.name, coverage.summary())
			}
		}
		if coverage.complete() != (test.raw == 3 && test.fiveMinutes == 3) {
			t.Fatalf("native %s completeness disagrees with the configured cohort", test.name)
		}
	}
	if len(responses) != 13 {
		t.Fatalf("native result count = %d, want 13", len(responses))
	}
	for _, name := range []string{"complete", "substitution"} {
		settings, observedQuery := redisRatesSyntheticSettings(t, string(responses[name]))
		settings.Grafana.AdminPassword = "test"
		server, _ := newRedisRatesGrafanaServer(t, "test", "5m", "5m", 200, "")
		defer server.Close()
		alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if *observedQuery != redisRateCoverageQuery("synthetic") {
			t.Fatal("signal executed a different query than the native fixture")
		}
		if name == "complete" && len(alerts) != 0 {
			t.Fatal("native complete cohort produced a signal finding")
		}
		if name == "substitution" {
			if len(alerts) != 1 {
				t.Fatal("native substituted cohort did not produce exactly one finding")
			}
			requireAlertClass(t, alerts, "redis-rate-source-coverage")
		}
	}
	t.Logf("gate_scope=exact-production-redis-query-and-reducer\n%s", strings.TrimSpace(string(output)))
}

// An independently described publisher series; a second publisher changes
// series multiplicity without inventing a new configured Redis node.
type redisRatesEngineSeries struct {
	source    redisRateSource
	job       string
	env       string
	publisher string
}

// Both the old count-only query and the owning query run against raw counters.
// The A/B/B_alt case explicitly reproduces all nine old counts equal to three.
func redisRatesPromqlScript() string {
	var script strings.Builder
	labelsText := func(labels map[string]string) string {
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+strconv.Quote(labels[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	legacyParts := []string{}
	for _, metric := range redisRateMetrics {
		selector := metric.name + `{env="synthetic"}`
		fresh := selector + ` and (timestamp(` + selector + `) >= time() - 180)`
		for _, window := range []string{"raw", "1m", "5m"} {
			expression := "count(" + fresh + ")"
			if window != "raw" {
				expression = "count(rate(" + selector + "[" + window + "]) and " + fresh + ")"
			}
			legacyParts = append(legacyParts, `label_replace((`+expression+` or vector(0)),"monitor_check",`+
				strconv.Quote(metric.short+"_"+window)+`,"__name__",".*")`)
		}
	}
	for _, mode := range []string{
		"complete", "substitution", "duplicate-publisher", "zero", "reset",
		"stale", "fresh-boundary", "single-sample", "sparse", "all-stale",
		"wrong-job", "wrong-environment", "reversed-order",
	} {
		fmt.Fprintf(&script, "# %s\nclear\nload 30s\n", mode)
		series := []redisRatesEngineSeries{}
		for _, source := range redisRatesTestSources() {
			series = append(series, redisRatesEngineSeries{source: source, job: "redis", env: "synthetic"})
		}
		if mode == "substitution" {
			series[2].source = redisRateSource{host: "redis-b-alt.example", node: series[1].source.node}
		}
		if mode == "duplicate-publisher" {
			series[2] = series[1]
			series[2].publisher = "synthetic-alternate"
		}
		if mode == "wrong-job" {
			series[2].job = "redis_exporter"
		}
		if mode == "wrong-environment" {
			series[2].env = "synthetic-other"
		}
		if mode == "reversed-order" {
			series[0], series[2] = series[2], series[0]
		}
		expected := map[string]int{}
		for _, metric := range redisRateMetrics {
			for _, window := range []string{"raw", "1m", "5m"} {
				expected[labelsText(map[string]string{"monitor_check": metric.short + "_" + window})] = 0
			}
			for index, publisher := range series {
				labels := map[string]string{
					"env": publisher.env, "job": publisher.job,
					"host": publisher.source.host, "node": publisher.source.node,
				}
				if publisher.publisher != "" {
					labels["publisher"] = publisher.publisher
				}
				points := make([]string, 21)
				for step := range points {
					value := 100 + step*3
					if mode == "zero" {
						value = 0
					}
					if mode == "reset" && index == 2 && step >= 18 {
						value = (step - 18) * 3
					}
					points[step] = strconv.Itoa(value)
					if (mode == "stale" || mode == "fresh-boundary") && index == 2 && step > 14 ||
						mode == "all-stale" && step > 14 ||
						mode == "single-sample" && index == 2 && step != 20 ||
						mode == "sparse" && step%3 != 0 {
						points[step] = "_"
					}
				}
				fmt.Fprintf(&script, "  %s%s %s\n", metric.name, labelsText(labels), strings.Join(points, " "))
				for _, window := range []string{"raw", "1m", "5m"} {
					present := true
					if publisher.job != "redis" || publisher.env != "synthetic" ||
						mode == "all-stale" || mode == "stale" && index == 2 ||
						mode == "fresh-boundary" && index == 2 && window == "1m" ||
						mode == "single-sample" && index == 2 && window != "raw" ||
						mode == "sparse" && window == "1m" {
						present = false
					}
					if present {
						key := labelsText(map[string]string{
							"monitor_check": metric.short + "_" + window,
							"env":           publisher.env, "job": publisher.job,
							"host": publisher.source.host, "node": publisher.source.node,
						})
						expected[key]++
					}
				}
			}
		}
		evaluation := "10m7s"
		if mode == "fresh-boundary" {
			evaluation = "10m"
		}
		fmt.Fprintf(&script, "\neval instant at %s %s\n", evaluation, redisRateCoverageQuery("synthetic"))
		keys := make([]string, 0, len(expected))
		for key := range expected {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&script, "  %s %d\n", key, expected[key])
		}
		script.WriteString("\n")
		if mode == "complete" || mode == "substitution" {
			fmt.Fprintf(&script, "eval instant at %s %s\n", evaluation, strings.Join(legacyParts, " or "))
			for _, metric := range redisRateMetrics {
				for _, window := range []string{"raw", "1m", "5m"} {
					fmt.Fprintf(&script, "  {monitor_check=%s} 3\n", strconv.Quote(metric.short+"_"+window))
				}
			}
			script.WriteString("\n")
		}
	}
	return script.String()
}
