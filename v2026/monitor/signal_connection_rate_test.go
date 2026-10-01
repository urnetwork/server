package monitor

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func connectionRateAggregateFixture() requiredAggregateFixture {
	return requiredAggregateFixture{NewConnectionRateSignal, "SELECT COALESCE(sum(n_tup_ins), 0)", Row{"10000"}}
}

func TestConnectionRateAggregateShape(t *testing.T) {
	testRequiredAggregateShape(t, connectionRateAggregateFixture())
}

func TestConnectionRateAggregateRunLoop(t *testing.T) {
	testRequiredAggregateRunLoop(t, connectionRateAggregateFixture())
}

func TestConnectionRateAggregateUnknownPreservesCounterAndBaseline(t *testing.T) {
	for _, test := range requiredAggregateBadShapes(connectionRateAggregateFixture().healthyRow) {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				baseline, err := newBaselineStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				for i := 30; i > 0; i-- {
					baseline.record(connectRateMetric, start.Add(-time.Duration(i)*time.Minute), 1000)
				}
				beforeSamples := append([]baselineSample(nil), baseline.metricSamples[connectRateMetric]...)
				beforeAppends := baseline.metricAppendCounts[connectRateMetric]
				beforeBytes, err := os.ReadFile(baseline.path(connectRateMetric))
				if err != nil {
					t.Fatal(err)
				}
				rows := test.rows
				reads := 0
				source := &syntheticSource{postgresFn: func(string) ([]Row, error) { reads++; return rows, nil }}
				signal := NewConnectionRateSignal()
				probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
				probe.initialized, probe.lastCount, probe.lastTime = true, 10000, start
				settings := syntheticSettings(source)
				settings.Now = time.Now
				settings.runtime = &signalRuntime{baseline: baseline}
				m := NewWithSignals(settings, signal)
				time.Sleep(time.Minute)
				alerts, err, panicked := runRequiredAggregateSafely(m)
				if panicked {
					t.Error("unknown connection aggregate panicked")
				} else {
					if err == nil {
						t.Error("unknown connection aggregate was accepted")
					}
					requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
				}
				afterBytes, err := os.ReadFile(baseline.path(connectRateMetric))
				if err != nil {
					t.Fatal(err)
				}
				if !probe.initialized || probe.lastCount != 10000 || !probe.lastTime.Equal(start) || reads != 1 ||
					!reflect.DeepEqual(baseline.metricSamples[connectRateMetric], beforeSamples) ||
					baseline.metricAppendCounts[connectRateMetric] != beforeAppends || !bytes.Equal(afterBytes, beforeBytes) {
					t.Error("unknown aggregate changed counter time, learned samples or persisted baseline bytes")
				}
				// The valid continuation spans two minutes since the last real
				// counter, not one minute since an invented zero/accepted row.
				time.Sleep(time.Minute)
				rows = []Row{{"10200"}}
				alerts, err = m.Run(context.Background())
				if err != nil || reads != 2 || len(alerts) != 1 {
					t.Fatal("valid counter continuation did not retain the genuine low-rate finding")
				}
				alert := requireAlertClass(t, alerts, "connects-rate")
				if alert.Observed != "connects_last_min=100 median=1000" || alert.Severity != SeverityWarn || alert.Sustain != 5 ||
					probe.lastCount != 10200 || !probe.lastTime.Equal(start.Add(2*time.Minute)) {
					t.Error("valid rate did not use the full interval from the preceding valid counter")
				}
				samples := baseline.metricSamples[connectRateMetric]
				continuedBytes, err := os.ReadFile(baseline.path(connectRateMetric))
				if err != nil {
					t.Fatal(err)
				}
				if len(samples) != len(beforeSamples)+1 || samples[len(samples)-1].v != 100 ||
					!samples[len(samples)-1].at.Equal(start.Add(2*time.Minute)) ||
					baseline.metricAppendCounts[connectRateMetric] != beforeAppends+1 ||
					!bytes.HasPrefix(continuedBytes, beforeBytes) || bytes.Count(continuedBytes, []byte{'\n'}) != beforeAppends+1 {
					t.Error("only the valid continuation should append one rate sample")
				}
			})
		})
	}
}

func TestConnectionRateAggregateUnknownDoesNotInitializeCounter(t *testing.T) {
	for _, test := range requiredAggregateBadShapes(connectionRateAggregateFixture().healthyRow) {
		t.Run(test.name, func(t *testing.T) {
			rows := test.rows
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
			signal := NewConnectionRateSignal()
			probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
			m := NewWithSignals(syntheticSettings(source), signal)
			alerts, err, panicked := runRequiredAggregateSafely(m)
			if panicked {
				t.Error("unknown first connection aggregate panicked")
			} else {
				if err == nil {
					t.Error("unknown first connection aggregate was accepted")
				}
				requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
			}
			if probe.initialized || probe.lastCount != 0 || !probe.lastTime.IsZero() {
				t.Error("unknown aggregate consumed the initial counter warmup")
			}
			rows = []Row{{"0"}}
			alerts, err = m.Run(context.Background())
			if err != nil || len(alerts) != 0 || !probe.initialized || probe.lastCount != 0 || probe.lastTime.IsZero() {
				t.Error("valid zero must initialize a real counter without a throughput finding")
			}
		})
	}
}

func TestConnectionRateAggregateValidCounterBands(t *testing.T) {
	for _, test := range []struct {
		name        string
		count       string
		initialized bool
		elapsed     time.Duration
		class       string
		sustain     int
	}{
		{name: "first zero", count: "0"},
		{name: "counter reset", count: "0", initialized: true, elapsed: time.Minute},
		{name: "nonadvancing clock", count: "10200", initialized: true},
		{name: "low rate", count: "10100", initialized: true, elapsed: time.Minute, class: "connects-rate", sustain: 5},
		{name: "low equality", count: "10500", initialized: true, elapsed: time.Minute},
		{name: "storm equality", count: "12500", initialized: true, elapsed: time.Minute},
		{name: "storm", count: "13000", initialized: true, elapsed: time.Minute, class: "connects-storm", sustain: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stateDir := t.TempDir()
				values := make([]float64, 30)
				for i := range values {
					values[i] = 1000
				}
				populateMetric(t, stateDir, connectRateMetric, values...)
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if strings.Contains(query, connectionRateAggregateFixture().queryMarker) {
						return []Row{{test.count}}, nil
					}
					return nil, nil // Optional storm cohort does not change the scalar contract.
				}}
				signal := NewConnectionRateSignal()
				probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
				if test.initialized {
					probe.initialized, probe.lastCount, probe.lastTime = true, 10000, time.Now().Add(-test.elapsed)
				}
				settings := syntheticSettings(source)
				settings.StateDir = stateDir
				settings.Now = time.Now
				alerts, err := NewWithSignals(settings, signal).Run(context.Background())
				if err != nil || signal.Cadence() != time.Minute {
					t.Fatal("valid connection scalar changed its source or cadence contract")
				}
				if test.class == "" {
					if len(alerts) != 0 {
						t.Error("valid zero, warmup or exact rate boundary became an outage")
					}
					return
				}
				if len(alerts) != 1 {
					t.Fatal("genuine connection-rate violation lost its sole product finding")
				}
				alert := requireAlertClass(t, alerts, test.class)
				if alert.SignalID != signal.ID() || alert.Target != "pg-1" || alert.Severity != SeverityWarn || alert.Sustain != test.sustain {
					t.Error("valid connection-rate finding changed identity or escalation")
				}
			})
		})
	}
}

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
				"0", "101225", "1", "1.087622",
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
		"score_passing_12h=1",
		"fewer than one scored provider per 1,000 rows",
		"current_children_per_parent=21.30",
		"schema head 603",
		"SIGNALS.md §2.7 and §2.15",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestConnectionRateSignalSyntheticPersistentStormKeepsDailyAnchor(t *testing.T) {
	stateDir := t.TempDir()
	values := make([]float64, 24*60)
	for i := range values {
		values[i] = 4000
		if len(values)-6*60 <= i {
			values[i] = 13500
		}
	}
	populateMetric(t, stateDir, connectRateMetric, values...)

	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "WITH cohorts(label, start_time, end_time)") {
			return []Row{{
				"26772", "23347", "3564", "1272", "655", "9.31", "24.72",
				"9007", "6111", "2709", "1205", "9", "24.15", "285.06",
				"0", "101183", "0", "0.582754",
			}}, nil
		}
		return []Row{{"23000"}}, nil
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
	for _, want := range []string{"median=4000", "ratio=3.2x", "classification_version=0"} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("persistent-storm alert missing %q: %+v", want, alert)
		}
	}
}

func TestConnectionRateSignalDenseShortHistoryCannotManufactureDailyAnchor(t *testing.T) {
	store, err := newBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 240; i++ {
		store.record(connectRateMetric, now.Add(-time.Duration(i)*30*time.Second), 4000)
	}
	if median, samples, ok := store.trailingMedianSpanning(
		connectRateMetric,
		24*time.Hour,
		120,
		12*time.Hour,
	); ok {
		t.Fatalf("dense two-hour history qualified as daily anchor: median=%v samples=%d", median, samples)
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
