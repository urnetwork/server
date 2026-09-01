package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mimirContinuityFixture(t *testing.T, timestamps []time.Time) string {
	t.Helper()
	values := make([][]any, 0, len(timestamps))
	for _, timestamp := range timestamps {
		values = append(values, []any{timestamp.Unix(), "1"})
	}
	payload := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result": []any{map[string]any{
				"metric": map[string]string{},
				"values": values,
			}},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func mimirContinuityTimes(start, end time.Time, omit map[time.Time]bool) []time.Time {
	timestamps := []time.Time{}
	for timestamp := start; !timestamp.After(end); timestamp = timestamp.Add(mimirContinuityStep) {
		if !omit[timestamp] {
			timestamps = append(timestamps, timestamp)
		}
	}
	return timestamps
}

func runMimirContinuitySynthetic(t *testing.T, timestamps []time.Time) ([]Alert, error, string) {
	t.Helper()
	var observedCommand string
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "edge-0" {
			return "", fmt.Errorf("unexpected Mimir gateway %s", host.Name)
		}
		observedCommand = command
		return mimirContinuityFixture(t, timestamps), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-0", Roles: []string{"services"}},
		{Name: "pg-only", Roles: []string{"pg-primary"}},
	}
	alerts, err := NewMimirContinuitySignal().Run(context.Background(), settings)
	return alerts, err, observedCommand
}

func TestMimirContinuitySignalSyntheticHealthyHistory(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now().UTC().Truncate(mimirContinuityStep)
	start := now.Add(-mimirContinuityWindow)
	alerts, err, command := runMimirContinuitySynthetic(
		t,
		mimirContinuityTimes(start, now, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("continuous Mimir history alerted: %+v", alerts)
	}
	for _, want := range []string{
		mimirContinuityMarker,
		"/prometheus/api/v1/query_range?",
		"urnetwork_build_info",
		"instance%21%3D%22%22",
		fmt.Sprintf("start=%d", start.Unix()),
		fmt.Sprintf("end=%d", now.Unix()),
		"step=300",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("continuity command lacks %q: %s", want, command)
		}
	}
}

func TestMimirContinuitySignalSyntheticRestartGap(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now().UTC().Truncate(mimirContinuityStep)
	start := now.Add(-mimirContinuityWindow)
	omit := map[time.Time]bool{}
	for index := 100; index <= 103; index++ {
		omit[start.Add(time.Duration(index)*mimirContinuityStep)] = true
	}
	alerts, err, _ := runMimirContinuitySynthetic(
		t,
		mimirContinuityTimes(start, now, omit),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-ingestion-gap")
	if alert.SignalNumber != "11.20" || alert.SignalKey != "mimir-continuity" {
		t.Fatalf("wrong continuity signal identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"4 missing 5-minute evaluations",
		"flush_blocks_on_shutdown",
		"ephemeral",
		"cannot infer whether each current child",
		"not zero throughput",
		"§11.21 exact-process signal is the current-state gate",
		"Run the §11.21 mimir-shutdown signal",
		"clean Warp commit 7176ccd",
		"do not redeploy solely for historical gaps",
		"Do not zero-fill",
		"controlled then full Grafana clean-shutdown rollout",
		"SIGNALS.md §11.20",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("continuity alert lacks %q: %s", want, markdown)
		}
	}
}

func TestMimirContinuitySignalIgnoresBoundedJitterAndRangeEdges(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now().UTC().Truncate(mimirContinuityStep)
	start := now.Add(-mimirContinuityWindow)
	interiorStart := start.Add(2 * time.Hour)
	interiorEnd := now.Add(-2 * time.Hour)
	omit := map[time.Time]bool{
		interiorStart.Add(10 * mimirContinuityStep): true,
		interiorStart.Add(11 * mimirContinuityStep): true,
	}
	alerts, err, _ := runMimirContinuitySynthetic(
		t,
		mimirContinuityTimes(interiorStart, interiorEnd, omit),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("two missing interior steps or absent range edges alerted: %+v", alerts)
	}
}

func TestMimirContinuitySignalRejectsAbsentControl(t *testing.T) {
	_, err, _ := runMimirContinuitySynthetic(t, nil)
	if err == nil || !strings.Contains(err.Error(), "no continuity control samples") {
		t.Fatalf("absent continuity control error = %v", err)
	}
}

func TestFindMimirContinuityGapsRejectsIrregularTimestamp(t *testing.T) {
	_, err := findMimirContinuityGaps([]time.Time{
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 7, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "irregular evaluation timestamps") {
		t.Fatalf("irregular timestamp error = %v", err)
	}
}
