package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func connectionRateAggregateFixture() requiredAggregateFixture {
	return requiredAggregateFixture{NewConnectionRateSignal, "SELECT COALESCE(sum(n_tup_ins), 0)", Row{"10000"}}
}

// Numeric authority and shape failures share the same public state-preservation control.
func connectionRateAuthorityBadRows() []struct {
	name string
	rows []Row
} {
	cases := requiredAggregateBadShapes(connectionRateAggregateFixture().healthyRow)
	for _, test := range []struct{ name, value string }{
		{"private text", aggregatePrivateCell}, {"null", ""}, {"blank", " "},
		{"fraction", "0.9"}, {"integral decimal", "10000.0"}, {"negative", "-1"},
		{"not a number", "NaN"}, {"infinity", "Inf"}, {"negative infinity", "-Inf"},
		{"integer overflow", "9223372036854775808"}, {"exponent", "1e4"},
		{"leading plus", "+10000"}, {"hexadecimal", "0x2710"},
	} {
		cases = append(cases, struct {
			name string
			rows []Row
		}{name: test.name, rows: []Row{{test.value}}})
	}
	return cases
}

func TestConnectionRateAggregateShape(t *testing.T) {
	testRequiredAggregateShape(t, connectionRateAggregateFixture())
}

func TestConnectionRateAggregateRunLoop(t *testing.T) {
	testRequiredAggregateRunLoop(t, connectionRateAggregateFixture())
}

func TestConnectionRateAggregateUnknownPreservesCounterAndBaseline(t *testing.T) {
	cases := connectionRateAuthorityBadRows()
	cases = append(cases, struct {
		name string
		rows []Row
	}{name: "derived rate overflow", rows: []Row{{"9223372036854775807"}}})
	for _, test := range cases {
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
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if strings.Contains(query, "WITH cohorts(label, start_time, end_time)") {
						return nil, nil
					}
					if !strings.Contains(query, connectionRateAggregateFixture().queryMarker) ||
						!strings.Contains(query, "schemaname = current_schema()") ||
						!strings.Contains(query, "relname = 'network_client_connection'") {
						t.Fatal("counter authority control did not reach its actual scalar query")
					}
					reads++
					return rows, nil
				}}
				signal := NewConnectionRateSignal()
				probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
				probe.initialized, probe.lastCount, probe.lastTime = true, 10000, start
				settings := syntheticSettings(source)
				settings.Now = time.Now
				settings.runtime = &signalRuntime{baseline: baseline}
				m := NewWithSignals(settings, signal)
				elapsed := time.Minute
				if test.name == "derived rate overflow" {
					elapsed = time.Nanosecond
				}
				time.Sleep(elapsed)
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
				// Continue two minutes after the last real counter, including
				// the rejected source's interval without inventing a zero.
				time.Sleep(2*time.Minute - elapsed)
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
	for _, test := range connectionRateAuthorityBadRows() {
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
	if _, ok, err := probe.observe(10_000, start); ok || err != nil {
		t.Fatal("first sample must warm up")
	}
	rate, ok, err := probe.observe(11_200, start.Add(2*time.Minute))
	if !ok || err != nil || rate != 600 {
		t.Fatalf("two-minute rate = %d, ok=%t; want 600/min", rate, ok)
	}
	if _, ok, err := probe.observe(25, start.Add(3*time.Minute)); ok || err != nil {
		t.Fatal("counter reset must warm up")
	}
}

// The cumulative source is bigint-sized even when only its small delta is a rate.
func TestConnectionRateAuthorityMaximumCounterRemainsValid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{{"9223372036854775807"}}, nil
		}}
		signal := NewConnectionRateSignal()
		settings := syntheticSettings(source)
		settings.Now = time.Now
		m := NewWithSignals(settings, signal)
		for i := 0; i < 2; i++ {
			alerts, err := m.Run(context.Background())
			if err != nil || len(alerts) != 0 {
				t.Fatal("valid maximum cumulative count or measured zero delta became unavailable")
			}
			time.Sleep(time.Minute)
		}
		probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
		if !probe.initialized || probe.lastCount != 9223372036854775807 {
			t.Fatal("valid maximum cumulative count lost its exact integer value")
		}
	})
}

// A finite maximum integer rate is valid; floating conversion must neither
// reject it nor turn this true high-rate observation into a negative collapse.
func TestConnectionRateAuthorityLargestRepresentableRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := t.TempDir()
		values := make([]float64, 30)
		for i := range values {
			values[i] = 1000
		}
		populateMetric(t, stateDir, connectRateMetric, values...)
		maximum := int(^uint(0) >> 1)
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, connectionRateAggregateFixture().queryMarker) {
				return []Row{{strconv.Itoa(maximum)}}, nil
			}
			return nil, nil
		}}
		signal := NewConnectionRateSignal()
		probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
		probe.initialized, probe.lastCount, probe.lastTime = true, 0, time.Now().Add(-time.Minute)
		settings := syntheticSettings(source)
		settings.Now, settings.StateDir = time.Now, stateDir
		alerts, err := NewWithSignals(settings, signal).Run(context.Background())
		if err != nil || len(alerts) != 1 {
			t.Fatal("representable maximum rate became unknown or lost its product finding")
		}
		alert := requireAlertClass(t, alerts, "connects-storm")
		if !strings.Contains(alert.Observed, "connects_last_min="+strconv.Itoa(maximum)+" ") {
			t.Error("maximum valid rate lost its exact integer value or became a collapse")
		}
	})
}

// Current and mature comparison cohorts plus the actual twelve-hour score
// projection support a cause only after the independent rate is established.
func connectionStormAuthorityRow() Row {
	return Row{
		"25998", "24411", "1146", "978", "108", "8.76", "24.51",
		"14501", "6292", "1119", "992", "2249", "70.26", "5379.84",
		"0", "101225", "1", "1.087622",
	}
}

// Missing or invalid optional evidence retains the real storm and its one
// baseline append, while neither cause attribution nor private errors escape.
func TestConnectionRateStormAuthorityRejectsMalformedDiagnostic(t *testing.T) {
	valid := connectionStormAuthorityRow()
	cases := requiredAggregateBadShapes(valid)
	for _, column := range []int{0, 1, 2, 3, 4, 7, 8, 9, 10, 11, 14, 15, 16} {
		for _, invalid := range []struct{ name, value string }{
			{"private text", aggregatePrivateCell}, {"null", ""}, {"negative", "-1"},
			{"fraction", "0.9"}, {"not a number", "NaN"}, {"infinity", "Inf"},
			{"overflow", "9223372036854775808"}, {"exponent", "1e3"},
		} {
			row := append(Row(nil), valid...)
			row[column] = invalid.value
			cases = append(cases, struct {
				name string
				rows []Row
			}{"integer column " + strconv.Itoa(column) + " " + invalid.name, []Row{row}})
		}
	}
	for _, column := range []int{5, 6, 12, 13, 17} {
		for _, invalid := range []struct{ name, value string }{
			{"private text", aggregatePrivateCell}, {"null", ""}, {"negative", "-0.1"},
			{"not a number", "NaN"}, {"infinity", "Inf"}, {"overflow", "1e309"},
			{"underflow", "1e-9999"}, {"hexadecimal", "0x1p0"},
		} {
			row := append(Row(nil), valid...)
			row[column] = invalid.value
			cases = append(cases, struct {
				name string
				rows []Row
			}{"float column " + strconv.Itoa(column) + " " + invalid.name, []Row{row}})
		}
	}
	for _, test := range []struct {
		name  string
		cells map[int]string
	}{
		{"version int32 overflow", map[int]string{14: "2147483648"}},
		{"current children exceed total", map[int]string{1: "25999"}},
		{"current parents exceed total", map[int]string{2: "25999"}},
		{"current networks exceed total", map[int]string{3: "25999"}},
		{"current survivors exceed total", map[int]string{4: "25999"}},
		{"current children without parents", map[int]string{2: "0"}},
		{"current network without parents", map[int]string{1: "0", 2: "0"}},
		{"current percentile order", map[int]string{5: "25"}},
		{"current no closed lifetimes", map[int]string{4: valid[0]}},
		{"baseline children exceed total", map[int]string{8: "14502"}},
		{"baseline parents exceed total", map[int]string{9: "14502"}},
		{"baseline networks exceed total", map[int]string{10: "14502"}},
		{"baseline survivors exceed total", map[int]string{11: "14502"}},
		{"baseline children without parents", map[int]string{9: "0"}},
		{"baseline percentile order", map[int]string{12: "5380"}},
		{"baseline no closed lifetimes", map[int]string{11: valid[7]}},
		{"passing exceeds score rows", map[int]string{16: "101226"}},
		{"nonzero maximum without scores", map[int]string{15: "0", 16: "0"}},
		{"qualifying maximum without passing", map[int]string{16: "0"}},
		{"passing without qualifying maximum", map[int]string{17: "0.6"}},
	} {
		row := append(Row(nil), valid...)
		for column, value := range test.cells {
			row[column] = value
		}
		cases = append(cases, struct {
			name string
			rows []Row
		}{test.name, []Row{row}})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			testConnectionStormUnavailableDiagnostic(t, test.rows, nil, observationErrorClassInvalidResponse)
		})
	}
	for _, test := range []struct {
		name  string
		err   error
		class string
	}{
		{"private transport detail", errors.New(aggregatePrivateCell), observationErrorClassUnclassified},
		{"private parser detail", errors.New("invalid response " + aggregatePrivateCell), observationErrorClassInvalidResponse},
		{"independent timeout", context.DeadlineExceeded, observationErrorClassTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			testConnectionStormUnavailableDiagnostic(t, nil, test.err, test.class)
		})
	}
}

// The public control verifies independent state, cause and serialized privacy
// together; no helper-only verdict can substitute for the real signal path.
func testConnectionStormUnavailableDiagnostic(t *testing.T, rows []Row, diagnosticErr error, errorClass string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		baseline, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for i := 30; i > 0; i-- {
			baseline.record(connectRateMetric, start.Add(-time.Duration(i)*time.Minute), 1000)
		}
		beforeBytes, err := os.ReadFile(baseline.path(connectRateMetric))
		if err != nil {
			t.Fatal(err)
		}
		requiredReads, diagnosticReads := 0, 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, connectionRateAggregateFixture().queryMarker) {
				requiredReads++
				return []Row{{"13000"}}, nil
			}
			diagnosticReads++
			for _, fragment := range []string{
				"WITH cohorts(label, start_time, end_time)", "COUNT(network_client_connection.*) AS total",
				"GROUP BY cohorts.label", "CROSS JOIN classification",
				"COUNT(*) FILTER (WHERE independent_reliability_weight >= 0.7)",
			} {
				if !strings.Contains(query, fragment) {
					t.Fatal("optional authority control did not reach the actual cohort query")
				}
			}
			return rows, diagnosticErr
		}}
		signal := NewConnectionRateSignal()
		probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
		probe.initialized, probe.lastCount, probe.lastTime = true, 10000, start.Add(-time.Minute)
		settings := syntheticSettings(source)
		settings.Now = time.Now
		settings.runtime = &signalRuntime{baseline: baseline}
		alerts, err := NewWithSignals(settings, signal).Run(context.Background())
		if err != nil || len(alerts) != 1 || requiredReads != 1 || diagnosticReads != 1 {
			t.Fatal("unavailable optional diagnostic erased the independent connection storm")
		}
		alert := alerts[0]
		if alert.Class != "connects-storm" || alert.Frame != "reconnect-storm" ||
			alert.Severity != SeverityWarn || alert.Sustain != 3 ||
			!strings.Contains(alert.Observed, "connects_last_min=3000 median=1000") ||
			!strings.Contains(alert.Observed, "diagnostic_error_class="+errorClass) ||
			strings.Contains(alert.Observed, "current_2m_total=") ||
			strings.Contains(alert.Mechanism, "provider-window feedback loop") ||
			strings.Contains(alert.Mechanism, "window replacement amplification") {
			t.Error("unavailable optional cohort acquired cause authority or lost the true rate finding")
		}
		afterBytes, err := os.ReadFile(baseline.path(connectRateMetric))
		if err != nil {
			t.Fatal(err)
		}
		samples := baseline.metricSamples[connectRateMetric]
		if !probe.initialized || probe.lastCount != 13000 || !probe.lastTime.Equal(start) ||
			len(samples) != 31 || samples[len(samples)-1].v != 3000 ||
			baseline.metricAppendCounts[connectRateMetric] != 31 ||
			!bytes.HasPrefix(afterBytes, beforeBytes) || bytes.Count(afterBytes, []byte{'\n'}) != 31 {
			t.Error("unavailable optional cohort discarded or corrupted the independently valid rate sample")
		}
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		var jsonl bytes.Buffer
		if err := alerts.WriteJSONL(&jsonl); err != nil {
			t.Fatal(err)
		}
		for _, rendered := range []string{alert.Markdown(), string(encoded), jsonl.String()} {
			for _, forbidden := range []string{"synthetic-private-aggregate", "fixture-only", "192.0.2.61"} {
				if strings.Contains(rendered, forbidden) {
					t.Error("optional cohort failure rendered rejected source or transport contents")
				}
			}
		}
	})
}

// Valid zero cohorts, deleted-client denominators, uncapped weights and exact
// doubled-ratio boundaries retain their prior independent cause meanings.
func TestConnectionRateStormAuthorityValidDiagnosticBands(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, test := range []struct {
		name  string
		cells map[int]string
		frame string
	}{
		{name: "qualified reliability cause", frame: "reliability-window-churn"},
		{name: "qualified child cause", cells: map[int]string{14: "1"}, frame: "window-child-churn"},
		{name: "exact doubled ratio", cells: map[int]string{1: "2", 2: "1", 8: "1", 9: "1", 14: "1"}, frame: "reconnect-storm"},
		{name: "above doubled ratio", cells: map[int]string{1: "3", 2: "1", 8: "1", 9: "1", 14: "1"}, frame: "window-child-churn"},
		{name: "missing current durable clients", cells: map[int]string{1: "0", 2: "0", 3: "0", 14: "1"}, frame: "reconnect-storm"},
		{name: "missing baseline durable clients", cells: map[int]string{8: "0", 9: "0", 10: "0", 14: "1"}, frame: "reconnect-storm"},
		{name: "empty mature cohorts", cells: map[int]string{0: "0", 1: "0", 2: "0", 3: "0", 4: "0", 5: "0", 6: "0", 7: "0", 8: "0", 9: "0", 10: "0", 11: "0", 12: "0", 13: "0", 14: "1", 15: "0", 16: "0", 17: "0"}, frame: "reconnect-storm"},
		{name: "all current survive", cells: map[int]string{4: "25998", 5: "0", 6: "0", 14: "1"}, frame: "window-child-churn"},
		{name: "large exact child excess", cells: map[int]string{0: strconv.Itoa(maximum), 1: strconv.Itoa(maximum), 2: "1", 7: strconv.Itoa(maximum), 8: strconv.Itoa(maximum / 2), 9: "1", 14: "1"}, frame: "window-child-churn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				row := connectionStormAuthorityRow()
				for column, value := range test.cells {
					row[column] = value
				}
				stateDir := t.TempDir()
				values := make([]float64, 30)
				for i := range values {
					values[i] = 1000
				}
				populateMetric(t, stateDir, connectRateMetric, values...)
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					if strings.Contains(query, connectionRateAggregateFixture().queryMarker) {
						return []Row{{"13000"}}, nil
					}
					return []Row{row}, nil
				}}
				signal := NewConnectionRateSignal()
				probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
				probe.initialized, probe.lastCount, probe.lastTime = true, 10000, time.Now().Add(-time.Minute)
				settings := syntheticSettings(source)
				settings.Now, settings.StateDir = time.Now, stateDir
				alerts, err := NewWithSignals(settings, signal).Run(context.Background())
				if err != nil || len(alerts) != 1 {
					t.Fatal("valid matched cohort lost independent connection storm")
				}
				alert := alerts[0]
				if alert.Class != "connects-storm" || alert.Frame != test.frame ||
					strings.Contains(alert.Observed, "diagnostic_error") || !strings.Contains(alert.Observed, "current_2m_total=") {
					t.Error("valid cohort or exact doubled-ratio boundary changed its cause frame")
				}
			})
		})
	}
}
