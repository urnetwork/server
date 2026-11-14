package monitor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The pinned real engine applies the actual raw-range selector to unaggregated
// producer samples. Returned matrices then enter the real public adapter with
// an independent synthetic native process census. No live source is contacted.
func TestProviderScoreReadErrorsPromqlEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("required score-reader qualification needs pinned MONITOR_PROMQL_ENGINE_SOURCE; skip is not a semantic PASS")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]*scoreReadFixture{}
	type engineCase struct {
		Name       string                            `json:"name"`
		Evaluation int64                             `json:"evaluation"`
		Series     []providerConnectionsEngineSeries `json:"series"`
	}
	cases := []engineCase{}
	for _, name := range []string{"healthy", "counts-error", "samples-error", "reset", "missing-live-process", "missing-phase", "stale-process", "foreign-env", "foreign-job", "foreign-service", "foreign-host", "foreign-block", "foreign-phase", "foreign-metric", "duplicate-labelset-owner", "left-boundary"} {
		f := newScoreReadFixture()
		switch name {
		case "counts-error", "missing-live-process", "missing-phase":
			f.row("api-a", "counts")["values"].([]any)[59].([]any)[1] = "11"
		case "samples-error":
			f.row("api-a", "samples")["values"].([]any)[59].([]any)[1] = "11"
		case "reset":
			f.row("api-b", "counts")["values"].([]any)[30].([]any)[1] = "0"
		}
		if name == "missing-live-process" {
			for _, field := range []string{"start", "counts", "samples", "success"} {
				f.remove("api-b", field)
			}
		}
		if name == "missing-phase" {
			f.remove("api-b", "samples")
		}
		if name == "stale-process" {
			for _, field := range []string{"start", "counts", "samples", "success"} {
				row := f.row("api-b", field)
				row["values"] = row["values"].([]any)[:50]
			}
		}
		c := engineCase{Name: name, Evaluation: f.now.Unix()}
		for _, row := range f.rows {
			series := providerConnectionsEngineSeries{Labels: row["metric"].(map[string]string)}
			if name == "left-boundary" {
				series.Points = append(series.Points, providerConnectionsEnginePoint{Seconds: f.now.Add(-15 * time.Minute).Unix(), Value: 0})
			}
			for _, raw := range row["values"].([]any) {
				point := raw.([]any)
				value, err := strconv.ParseFloat(point[1].(string), 64)
				if err != nil {
					t.Fatal(err)
				}
				series.Points = append(series.Points, providerConnectionsEnginePoint{Seconds: point[0].(int64), Value: value})
			}
			c.Series = append(c.Series, series)
		}
		for label, value := range map[string]string{"env": "other", "job": "connect", "service": "connect", "host": "foreign", "block": "other", "phase": "other", "metric": "urnetwork_findproviders2_outcomes_total"} {
			if name != "foreign-"+label {
				continue
			}
			foreign := c.Series[1]
			foreign.Labels = map[string]string{}
			for key, value := range c.Series[1].Labels {
				foreign.Labels[key] = value
			}
			if label == "metric" {
				label = "__name__"
			}
			foreign.Labels[label] = value
			foreign.Points = append([]providerConnectionsEnginePoint(nil), foreign.Points...)
			foreign.Points[len(foreign.Points)-1].Value = 1000
			c.Series = append(c.Series, foreign)
		}
		if name == "duplicate-labelset-owner" {
			duplicate := c.Series[1]
			duplicate.Labels = map[string]string{}
			for key, value := range c.Series[1].Labels {
				duplicate.Labels[key] = value
			}
			duplicate.Labels["private"] = "PRIVATE-ENGINE-CANARY"
			c.Series = append(c.Series, duplicate)
		}
		fixtures[name] = f
		cases = append(cases, c)
	}
	f := newScoreReadFixture()
	env, err := newProbeEnv(f.settings(t).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	query := providerScoreReadQuery("synthetic", providerScoreReadScope(env))
	request, err := json.Marshal(struct {
		Query string       `json:"query"`
		Cases []engineCase `json:"cases"`
	}{Query: query, Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	requestPath, outputPath := filepath.Join(directory, "request.json"), filepath.Join(directory, "matrices.json")
	if err := os.WriteFile(requestPath, request, 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/provider_score_read_errors_promql_engine_test.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	engineTest := filepath.Join(directory, "provider_score_read_errors_promql_engine_test.go")
	if err := os.WriteFile(engineTest, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestProviderScoreReadNativeMatrices$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "MONITOR_SCORE_READ_ENGINE_REQUEST="+requestPath, "MONITOR_SCORE_READ_ENGINE_OUTPUT="+outputPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("score-reader native engine failed: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	matrices := map[string]json.RawMessage{}
	if json.Unmarshal(encoded, &matrices) != nil || len(matrices) != len(cases) {
		t.Fatal("engine omitted raw-source controls")
	}
	for _, c := range cases {
		f := fixtures[c.Name]
		f.raw = string(matrices[c.Name])
		if f.raw == "" {
			t.Fatal("missing actual engine matrix")
		}
		alerts := f.run(t)
		positive := c.Name == "counts-error" || c.Name == "samples-error" || c.Name == "missing-live-process" || c.Name == "missing-phase"
		unknown := c.Name == "reset" || c.Name == "missing-live-process" || c.Name == "missing-phase" || c.Name == "stale-process" || c.Name == "duplicate-labelset-owner"
		if (requireAlertClassCount(alerts, "provider-score-read-errors") == 1) != positive || (requireAlertClassCount(alerts, "provider-score-read-errors-unobservable") == 1) != unknown {
			t.Fatalf("actual raw query/public authority mismatched %s", c.Name)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "PRIVATE-ENGINE-CANARY", "api-a", "api-b", "00000001")
		}
	}
}
