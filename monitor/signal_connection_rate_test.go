package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestConnectionRateSignalSyntheticConnectionCollapse(t *testing.T) {
	stateDir := t.TempDir()
	values := make([]float64, 30)
	for i := range values {
		values[i] = 1000
	}
	populateMetric(t, stateDir, connectRateMetric, values...)

	count := int64(10_100)
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"10100"}}, nil
	}}
	signal := NewConnectionRateSignal()
	probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
	probe.initialized = true
	probe.lastCount = count - 100
	probe.lastTime = time.Now().Add(-time.Minute)
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "connects-rate")
}

func TestConnectionRateSignalSyntheticReconnectStorm(t *testing.T) {
	stateDir := t.TempDir()
	values := make([]float64, 30)
	for i := range values {
		values[i] = 1000
	}
	populateMetric(t, stateDir, connectRateMetric, values...)

	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"13000"}}, nil
	}}
	signal := NewConnectionRateSignal()
	probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
	probe.initialized = true
	probe.lastCount = 10_000
	probe.lastTime = time.Now().Add(-time.Minute)
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "connects-storm")
	if !strings.Contains(alert.Context, "matched disconnect_time cohorts") ||
		!strings.Contains(alert.Context, "right-censors") {
		t.Fatalf("storm alert omitted lifetime sampling guard: %+v", alert)
	}
}

func TestConnectionRateSignalSyntheticReliabilityWindowChurn(t *testing.T) {
	stateDir := t.TempDir()
	values := make([]float64, 30)
	for i := range values {
		values[i] = 4000
	}
	populateMetric(t, stateDir, connectRateMetric, values...)

	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "WITH cohorts(label, start_time, end_time)") {
			return []Row{{
				"25998", "24411", "1146", "978", "108", "8.76", "24.51",
				"14501", "6292", "1119", "992", "2249", "70.26", "5379.84",
				"0", "100737", "0", "0.666203",
			}}, nil
		}
		return []Row{{"25000"}}, nil
	}}
	signal := NewConnectionRateSignal()
	probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
	probe.initialized = true
	probe.lastCount = 10_000
	probe.lastTime = time.Now().Add(-time.Minute)
	settings := syntheticSettings(source)
	settings.StateDir = stateDir

	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "connects-storm")
	if alert.Frame != "reliability-window-churn" {
		t.Fatalf("frame = %q, want reliability-window-churn", alert.Frame)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"provider-window feedback loop",
		"classification_version=0",
		"score_passing_12h=0",
		"current_children_per_parent=21.30",
		"SIGNALS.md §2.7 and §2.15",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestConnectionRateSignalCumulativeCounterResetIsWarmup(t *testing.T) {
	probe := &pgConnectRateProbe{}
	start := time.Unix(1000, 0)
	if _, ok := probe.observe(10_000, start); ok {
		t.Fatal("first sample must warm up")
	}
	rate, ok := probe.observe(11_200, start.Add(2*time.Minute))
	if !ok || rate != 600 {
		t.Fatalf("two-minute rate = %d, ok=%t; want 600/min", rate, ok)
	}
	if _, ok := probe.observe(25, start.Add(3*time.Minute)); ok {
		t.Fatal("counter reset must warm up")
	}
}
