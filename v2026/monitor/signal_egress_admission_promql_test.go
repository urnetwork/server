package monitor

// The external pinned engine proves original scrape clocks for every fixed
// family. Before implementation, this is explicitly a candidate-shape gate.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const egressAdmissionPromqlEngineFixture = "testdata/egress_admission_promql_engine_test.go.txt"

// After the owning query changes, the same fixture executes its exact bytes.
// Candidate success must not be presented as qualification of unchanged code.
func egressAdmissionNativeWindowEngineQuery() (string, bool) {
	query := egressAdmissionQuery("synthetic")
	if strings.Contains(query, `"monitor_egress_window"`) {
		return query, true
	}
	selector := regexp.MustCompile(`\{[^{}]*\}`)
	previous := selector.ReplaceAllString(query, "$0 offset 1m")
	return `label_replace((` + query + `),"monitor_egress_window","current","job",".*") or ` +
		`label_replace((` + previous + `),"monitor_egress_window","previous","job",".*")`, false
}

// No engine dependency is added to Server. Sol supplies the already pinned
// Mimir checkout and records these changed owning inputs in a fresh gate.
func TestEgressAdmissionNativeWindowPromqlEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("set MONITOR_PROMQL_ENGINE_SOURCE for the required actual native-engine gate")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	query, productionBound := egressAdmissionNativeWindowEngineQuery()
	if productionBound {
		t.Log("gate_scope=exact-production-egress-query")
	} else {
		t.Log("gate_scope=prospective-offset-semantics-only production_query_qualified=false")
	}
	fixture, err := os.ReadFile(egressAdmissionPromqlEngineFixture)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	engineTest := filepath.Join(directory, "egress_admission_promql_engine_test.go")
	if err := os.WriteFile(engineTest, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(directory, "egress_admission.test")
	if err := os.WriteFile(scriptPath, []byte(egressAdmissionNativeWindowEngineScript(query)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestEgressAdmissionNativeWindow$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "MONITOR_PROMQL_TEST_SCRIPT="+scriptPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native original-timestamp semantics: %v\n%s", err, output)
	}
	t.Logf("native original-timestamp semantics: %s", strings.TrimSpace(string(output)))
}

// Independent source cells cover both process roles and every fixed zero.
// Distinct labels remain intact through the whole query, not selected sums.
type egressAdmissionEngineCell struct {
	metric string
	family string
	key    string
	labels map[string]string
}

func egressAdmissionNativeWindowEngineCells() []egressAdmissionEngineCell {
	cells := []egressAdmissionEngineCell{}
	add := func(metric, family, key, job string, extra map[string]string) {
		labels := map[string]string{
			"env": "synthetic", "job": job, "host": "synthetic-" + job + ".example",
			"block": "synthetic-block", "instance": "synthetic-" + job + "-process",
		}
		for name, value := range extra {
			labels[name] = value
		}
		cells = append(cells, egressAdmissionEngineCell{metric: metric, family: family, key: key, labels: labels})
	}
	for _, job := range []string{"api", "taskworker"} {
		add("process_resident_memory_bytes", "rss", "rss", job, nil)
		add("process_start_time_seconds", "start", "start", job, nil)
	}
	add("urnetwork_egress_due_observation_enabled", "enabled", "enabled", "api", nil)
	add("urnetwork_egress_probe_submission_observation_enabled", "enabled", "enabled", "taskworker", nil)
	add("urnetwork_egress_due_requests_total", "requests", "requests", "api", nil)
	for _, lane := range []string{"no-location", "stale-location", "stale-health", "missing-health"} {
		for _, expired := range []string{"false", "true"} {
			add("urnetwork_egress_due_selected_total", "selected", "selected/"+lane+"/"+expired, "api", map[string]string{"lane": lane, "expired": expired})
		}
	}
	for _, kind := range []string{"health", "attempt"} {
		for _, outcome := range []string{"acknowledged", "unsupported", "canceled", "error_or_unknown"} {
			add("urnetwork_egress_probe_submission_outcomes_total", "submission", "submission/"+kind+"/"+outcome, "taskworker", map[string]string{"kind": kind, "outcome": outcome})
		}
	}
	for _, result := range []string{"attempted", "submitted", "skipped", "failed"} {
		add("urnetwork_egress_probe_pass_providers_total", "full", "full/"+result, "taskworker", map[string]string{"schedule": "full", "result": result})
	}
	for _, step := range []string{"blackhole_due", "full_due", "pins", "blackhole_run", "blackhole_submit", "full_run", "canceled", "funding_unavailable", "funding_unknown"} {
		add("urnetwork_egress_probe_pass_errors_total", "pass_error", "pass_error/"+step, "taskworker", map[string]string{"step": step})
	}
	return cells
}

// The engine's evaluation is at 607s; ordinary source points are at 600s and
// 540s. Sparse/repeated/stale controls make substituting evaluation time fail.
func egressAdmissionNativeWindowEngineScript(query string) string {
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
	for _, mode := range []string{"complete", "sparse", "repeated", "stale", "reset", "restart", "mixed-source-clocks", "missing-enabled"} {
		script.WriteString("clear\nload 15s\n")
		cells := egressAdmissionNativeWindowEngineCells()
		pointValues := make([][]string, len(cells))
		for i, cell := range cells {
			values := make([]string, 41)
			for step := range values {
				value := 0
				switch cell.key {
				case "rss":
					value = 1024 * 1024
				case "enabled", "start":
					value = 1
				case "requests", "selected/stale-health/false", "submission/health/acknowledged", "submission/attempt/acknowledged":
					value = 10 + step
				}
				if mode == "reset" && cell.key == "requests" && step >= 38 {
					value = 1 + step - 38
				}
				if mode == "restart" && cell.key == "start" && step >= 38 {
					value = 570
				}
				values[step] = strconv.Itoa(value)
				if mode == "sparse" && step%8 != 0 ||
					mode == "repeated" && step != 36 ||
					mode == "stale" && step > 31 ||
					mode == "mixed-source-clocks" && cell.key == "requests" && step == 40 ||
					mode == "missing-enabled" && cell.key == "enabled" && cell.labels["job"] == "api" {
					values[step] = "_"
				}
			}
			pointValues[i] = values
			fmt.Fprintf(&script, "  %s%s %s\n", cell.metric, labelsText(cell.labels), strings.Join(values, " "))
		}
		fmt.Fprintf(&script, "\neval instant at 10m7s sum without (__name__) (%s)\n", query)
		for i, cell := range cells {
			for _, point := range []struct {
				window   string
				lastStep int
			}{
				{window: "current", lastStep: 40},
				{window: "previous", lastStep: 36},
			} {
				lastStep := point.lastStep
				for lastStep >= 0 && pointValues[i][lastStep] == "_" {
					lastStep--
				}
				if lastStep < 0 {
					continue
				}
				for _, part := range []string{"value", "timestamp"} {
					labels := map[string]string{}
					for name, value := range cell.labels {
						labels[name] = value
					}
					labels["monitor_egress_family"] = cell.family
					labels["monitor_egress_part"] = part
					labels["monitor_egress_window"] = point.window
					value := pointValues[i][lastStep]
					if part == "timestamp" {
						value = strconv.Itoa(15 * lastStep)
					}
					fmt.Fprintf(&script, "  %s %s\n", labelsText(labels), value)
				}
			}
		}
		script.WriteString("\n")
	}
	return script.String()
}
