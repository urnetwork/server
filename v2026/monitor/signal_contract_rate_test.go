package monitor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestContractRateSignalSyntheticContractCollapse(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"700"}}, nil }}
	alerts, err := NewContractRateSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "contracts-collapse")
}

// Missing or malformed successful counts are unknown, not throughput samples.
func TestContractRateInvalidCountDoesNotMutateBaseline(t *testing.T) {
	for _, test := range []struct {
		name string
		rows []Row
	}{
		{name: "missing row"},
		{name: "missing column", rows: []Row{{}}},
		{name: "empty value", rows: []Row{{""}}},
		{name: "malformed value", rows: []Row{{"synthetic-private-count 192.0.2.32"}}},
		{name: "negative count", rows: []Row{{"-1"}}},
		{name: "fractional count", rows: []Row{{"9000.5"}}},
		{name: "overflowing count", rows: []Row{{"9223372036854775808"}}},
		{name: "extra row", rows: []Row{{"9000"}, {"9000"}}},
		{name: "extra column", rows: []Row{{"9000", "0"}}},
	} {
		baseline, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		baseline.record(contractRateMetric, time.Now(), 9000)
		beforeSamples := append([]baselineSample(nil), baseline.metricSamples[contractRateMetric]...)
		beforeAppendCount := baseline.metricAppendCounts[contractRateMetric]
		beforeBytes, err := os.ReadFile(baseline.path(contractRateMetric))
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			calls++
			return test.rows, nil
		}}
		settings := syntheticSettings(source)
		settings.runtime = &signalRuntime{baseline: baseline}
		var alerts Alerts
		var runErr error
		panicked := false
		func() {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			alerts, runErr = NewWithSignals(settings, NewContractRateSignal()).Run(context.Background())
		}()
		if panicked {
			t.Errorf("%s: invalid count panicked instead of returning unknown", test.name)
		}
		if runErr == nil || len(alerts) != 1 {
			t.Errorf("%s: invalid count did not become one visibility error: err=%v alerts=%d", test.name, runErr, len(alerts))
		} else {
			alert := alerts[0]
			if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
				alert.Target != "pg/contracts-collapse" || alert.Observed != "error_class="+observationErrorClassInvalidResponse {
				t.Errorf("%s: invalid count changed the shared unknown contract: class=%s severity=%s sustain=%d observed=%s",
					test.name, alert.Class, alert.Severity, alert.Sustain, alert.Observed)
			}
			for _, forbidden := range []string{"synthetic-private-count", "192.0.2.32"} {
				if strings.Contains(alert.Markdown(), forbidden) || strings.Contains(runErr.Error(), forbidden) {
					t.Errorf("%s: invalid count leaked synthetic private data", test.name)
				}
			}
		}
		afterBytes, err := os.ReadFile(baseline.path(contractRateMetric))
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !reflect.DeepEqual(baseline.metricSamples[contractRateMetric], beforeSamples) ||
			baseline.metricAppendCounts[contractRateMetric] != beforeAppendCount || !bytes.Equal(afterBytes, beforeBytes) {
			t.Errorf("%s: unknown count changed learned history or repeated its source", test.name)
		}
	}
}

// Numeric zero is affirmative outage evidence; valid rates still enter history.
func TestContractRateValidCountsPreserveZeroAndHealthyBands(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		count      int64
		wantOutage bool
	}{
		{name: "observed zero", value: "0", count: 0, wantOutage: true},
		{name: "below outage floor", value: "999", count: 999, wantOutage: true},
		{name: "outage floor boundary", value: "1000", count: 1000},
		{name: "ordinary rate", value: "9000", count: 9000},
		{name: "trimmed numeric rate", value: " 9000 ", count: 9000},
		{name: "maximum PostgreSQL count", value: "9223372036854775807", count: 9223372036854775807},
	} {
		baseline, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{{test.value}}, nil
		}}
		settings := syntheticSettings(source)
		settings.runtime = &signalRuntime{baseline: baseline}
		alerts, err := NewWithSignals(settings, NewContractRateSignal()).Run(context.Background())
		if err != nil {
			t.Fatalf("%s: valid count became unknown: %v", test.name, err)
		}
		if test.wantOutage {
			if len(alerts) != 1 || alerts[0].Class != "contracts-collapse" ||
				alerts[0].Severity != SeverityPage || alerts[0].Sustain != 3 ||
				alerts[0].Observed != fmt.Sprintf("contracts_last_min=%d", test.count) {
				t.Errorf("%s: affirmative outage evidence changed", test.name)
			}
		} else if len(alerts) != 0 {
			t.Errorf("%s: valid healthy count emitted %d alerts", test.name, len(alerts))
		}
		samples := baseline.metricSamples[contractRateMetric]
		if len(samples) != 1 || samples[0].v != float64(test.count) || baseline.metricAppendCounts[contractRateMetric] != 1 {
			t.Errorf("%s: valid count was not recorded exactly once", test.name)
		}
	}
}
