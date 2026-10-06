package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	servermonitor "github.com/urnetwork/server/monitor"
)

// All contact is replaced at the public transport seam. Every other source
// operation panics through the nil embedded interface if unexpectedly used.
type selectionOutputSource struct {
	servermonitor.SignalSource
	payload string
	calls   int
}

func (self *selectionOutputSource) Host(context.Context, servermonitor.HostSettings, string) (string, error) {
	self.calls++
	return self.payload, nil
}

func selectionOutputPayload(t *testing.T, now time.Time, reason string) string {
	t.Helper()
	rows := []map[string]any{}
	add := func(field string, value float64, outcome bool) {
		labels := map[string]string{"env": "synthetic", "job": "api", "host": "api-fixture", "block": "blue", "instance": "private-instance", "monitor_selection": field}
		if outcome {
			labels["target_kind"], labels["request_class"], labels["ip_family"], labels["rank_mode"] = "group", "default_minimum", "any", "quality"
			labels["outcome"], labels["reason"] = "zero", reason
			if reason == "returned_10_plus" {
				labels["outcome"] = "nonempty"
			}
		}
		rows = append(rows, map[string]any{"metric": labels, "value": []any{now.Unix(), fmt.Sprint(value)}})
	}
	for _, bound := range []string{"now", "prior"} {
		stamp := now.Add(-5 * time.Second)
		if bound == "prior" {
			stamp = stamp.Add(-5 * time.Minute)
		}
		add(bound+"_start", float64(now.Add(-time.Hour).Unix()), false)
		add(bound+"_schema", 2, false)
		add(bound+"_start_time", float64(stamp.Unix()), false)
		add(bound+"_schema_time", float64(stamp.Unix()), false)
	}
	if reason != "" {
		add("count", 38, true)
		add("count_time", float64(now.Add(-5*time.Second).Unix()), true)
		add("resets", 0, true)
		add("samples", 20, true)
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// The real CLI sink preserves bad, nonempty, quiet and unknown observations
// across runs without changing the probe count or suppressing actual alerts.
func TestMonitorProviderSelectionOutputAppendsEveryState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.jsonl")
	now := time.Date(2026, 10, 6, 21, 0, 55, 0, time.UTC)
	for _, reason := range []string{"cache_missing", "returned_10_plus", "", "unknown"} {
		source := &selectionOutputSource{payload: selectionOutputPayload(t, now, reason)}
		loader := func() (servermonitor.SignalSettings, error) {
			settings := servermonitor.SignalSettings{
				Environment: "synthetic", Source: source, Now: func() time.Time { return now },
				Hosts:       []servermonitor.HostSettings{{Name: "api-fixture", Roles: []string{"services"}}},
				LogServices: []string{"api"}, LogServiceHosts: map[string][]string{"api": {"api-fixture"}},
				LogServiceBlocks: map[string][]string{"api": {"blue"}},
			}
			if reason == "unknown" {
				settings.LogServiceBlocks = nil
			}
			return settings, nil
		}
		var alerts bytes.Buffer
		if err := runWithSettingsLoader([]string{"-once", "-include-signal", "provider-selection", "-provider-selection-output", path}, &alerts, loader); err != nil {
			t.Fatal(err)
		}
		wantCalls := 1
		if reason == "unknown" {
			wantCalls = 0
		}
		if source.calls != wantCalls {
			t.Fatalf("output changed probe contact count: got=%d want=%d", source.calls, wantCalls)
		}
		if reason == "cache_missing" && !strings.Contains(alerts.String(), "missing target cache metadata") {
			t.Fatal("observation output suppressed the actual bad symptom")
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 4 || strings.Contains(string(contents), "private-instance") {
		t.Fatal("states were lost or private identity was retained")
	}
	for index, line := range lines {
		var record servermonitor.ProviderSelectionObservation
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.SourceCoverageComplete != (index != 3) || (len(record.Outcomes) == 1) != (index < 2) {
			t.Fatalf("unknown/quiet/nonempty/bad state was conflated: index=%d record=%+v", index, record)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("selection observations were not created privately")
	}
}

func TestMonitorProviderSelectionOutputRequiresDistinctSelectedSink(t *testing.T) {
	for _, args := range [][]string{
		{"-list-signals", "-provider-selection-output", "unused.jsonl"},
		{"-once", "-output", "same.jsonl", "-provider-selection-output", "same.jsonl"},
		{"-url-probe-coverage-output", "same.jsonl", "-provider-selection-output", "same.jsonl"},
		{"-exclude-signal", "provider-selection", "-provider-selection-output", "unused.jsonl"},
	} {
		err := runWithSettingsLoader(args, &bytes.Buffer{}, func() (servermonitor.SignalSettings, error) {
			t.Fatal("invalid output configuration reached settings or contact")
			return servermonitor.SignalSettings{}, nil
		})
		if err == nil {
			t.Fatalf("invalid selection sink accepted: %v", args)
		}
	}
	if _, err := parseMonitorOptions([]string{"-provider-selection-output", filepath.Join(t.TempDir(), "continuous.jsonl")}); err != nil {
		t.Fatalf("standing observation sink cannot be armed: %v", err)
	}
}
