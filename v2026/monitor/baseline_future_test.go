// Future wall-clock entries remain durable history, but cannot authorize a
// present learned band. Synthetic clocks keep the exact interval boundaries
// and consumer rates deterministic without wall-clock waits or live sources.
package monitor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Future values cannot supply either the minimum count or a median.
func TestBaselineFutureWindowCannotQualifyHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		metric := "synthetic/future-only"
		for index := 0; index < 30; index++ {
			store.record(metric, now.Add(time.Hour+time.Duration(index)*time.Minute), 12000)
		}
		before, err := os.ReadFile(store.path(metric))
		if err != nil {
			t.Fatal(err)
		}
		if values := store.trailingWindow(metric, time.Hour); len(values) != 0 {
			t.Fatalf("future-only history supplied %d current window values", len(values))
		}
		if median, count, ok := store.trailingMedian(metric, time.Hour, 30); ok || count != 0 || median != 0 {
			t.Fatalf("future-only history qualified: median=%v count=%d ok=%t", median, count, ok)
		}
		if median, count, ok := store.trailingMedianSpanning(metric, 24*time.Hour, 30, time.Hour); ok || count != 0 || median != 0 {
			t.Fatalf("future-only history supplied a span: median=%v count=%d ok=%t", median, count, ok)
		}
		after, err := os.ReadFile(store.path(metric))
		if err != nil {
			t.Fatal(err)
		}
		if len(store.metricSamples[metric]) != 30 || store.metricAppendCounts[metric] != 30 || !bytes.Equal(before, after) {
			t.Fatal("excluding future entries changed retained history")
		}
	})
}

// Both ends are inclusive; one nanosecond outside either end is ineligible.
func TestBaselineFutureWindowInclusiveBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		metric := "synthetic/window-boundaries"
		// Deliberately out of timestamp order, as after a clock rollback.
		store.record(metric, now.Add(time.Nanosecond), 90000)
		store.record(metric, now.Add(-time.Hour), 2000)
		store.record(metric, now.Add(-time.Hour-time.Nanosecond), 90000)
		store.record(metric, now, 4000)
		store.record(metric, now.Add(-30*time.Minute), 6000)
		if values := store.trailingWindow(metric, time.Hour); !reflect.DeepEqual(values, []float64{2000, 4000, 6000}) {
			t.Fatalf("inclusive window values = %v", values)
		}
		if median, count, ok := store.trailingMedian(metric, time.Hour, 3); !ok || count != 3 || median != 4000 {
			t.Fatalf("eligible median changed: median=%v count=%d ok=%t", median, count, ok)
		}
		if median, count, ok := store.trailingMedian(metric, time.Hour, 4); ok || count != 3 || median != 0 {
			t.Fatalf("out-of-window samples matured history: median=%v count=%d ok=%t", median, count, ok)
		}
		if median, count, ok := store.trailingMedianSpanning(metric, time.Hour, 3, time.Hour); !ok || count != 3 || median != 4000 {
			t.Fatalf("inclusive valid span changed: median=%v count=%d ok=%t", median, count, ok)
		}
	})
}

// A future entry cannot extend dense recent history into a daily anchor.
func TestBaselineFutureWindowCannotManufactureSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		metric := "synthetic/span"
		for index := 0; index < 120; index++ {
			store.record(metric, now.Add(-time.Duration(index)*time.Second), 5000)
		}
		store.record(metric, now.Add(12*time.Hour), 1000)
		if median, count, ok := store.trailingMedianSpanning(metric, 24*time.Hour, 120, 12*time.Hour); ok || count != 120 || median != 0 {
			t.Fatalf("future timestamp supplied the required span: median=%v count=%d ok=%t", median, count, ok)
		}
		store.record(metric, now.Add(-12*time.Hour), 5000)
		if median, count, ok := store.trailingMedianSpanning(metric, 24*time.Hour, 120, 12*time.Hour); !ok || count != 121 || median != 5000 {
			t.Fatalf("genuine daily anchor failed: median=%v count=%d ok=%t", median, count, ok)
		}
	})
}

// Model a rollback by retaining samples stamped with the preceding clock,
// followed by a current sample. Loading and compaction must not discard them.
func TestBaselineFutureWindowRollbackHistorySurvivesReloadAndCatchUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		store, err := newBaselineStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		beforeRollback := now.Add(time.Hour)
		metric := "synthetic/rollback"
		for index := 0; index < 30; index++ {
			store.record(metric, beforeRollback.Add(-time.Duration(29-index)*time.Minute), 12000)
		}
		store.record(metric, now, 3000)
		before, err := os.ReadFile(store.path(metric))
		if err != nil {
			t.Fatal(err)
		}
		store, err = newBaselineStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if median, count, ok := store.trailingMedian(metric, time.Hour, 30); ok || count != 1 || median != 0 {
			t.Fatalf("rollback history qualified after reload: median=%v count=%d ok=%t", median, count, ok)
		}
		if len(store.metricSamples[metric]) != 31 || store.metricAppendCounts[metric] != 31 {
			t.Fatal("reload discarded future history")
		}
		store.stateLock.Lock()
		store.compactWithLock(metric, store.metricSamples[metric])
		store.stateLock.Unlock()
		after, err := os.ReadFile(store.path(metric))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("baseline qualification or compaction rewrote retained records")
		}
		// Inside synctest this advances only the synthetic clock.
		time.Sleep(time.Hour)
		if !time.Now().Equal(beforeRollback) {
			t.Fatal("synthetic clock did not reach the retained timestamp")
		}
		if median, count, ok := store.trailingMedian(metric, time.Hour, 30); !ok || count != 31 || median != 12000 {
			t.Fatalf("retained history did not become eligible: median=%v count=%d ok=%t", median, count, ok)
		}
		if median, count, ok := store.trailingMedianSpanning(metric, time.Hour, 30, time.Hour); !ok || count != 31 || median != 12000 {
			t.Fatalf("retained history did not recover its real span: median=%v count=%d ok=%t", median, count, ok)
		}
	})
}

// The original filename format retains the same query-time eligibility rule.
func TestBaselineFutureWindowLegacyFileRemainsLoadable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		now := time.Now()
		path := filepath.Join(dir, "synthetic_legacy.metric")
		data := []byte(fmt.Sprintf("%d 12000\n%d 3000\n", now.Add(time.Hour).Unix(), now.Unix()))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := newBaselineStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if median, count, ok := store.trailingMedian("synthetic/legacy", time.Hour, 1); !ok || count != 1 || median != 3000 {
			t.Fatalf("legacy file admitted future history: median=%v count=%d ok=%t", median, count, ok)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, after) || len(store.metricSamples["synthetic/legacy"]) != 2 {
			t.Fatal("legacy history was altered to exclude its future sample")
		}
	})
}

// Current source evidence keeps its static band; learned failures need only
// eligible history, including the sample-count guard on the six-hour fallback.
func TestBaselineFutureContractRateBands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			name          string
			rate          int
			recentSamples int
			recentValue   float64
			olderSamples  int
			futureSamples int
			futureValue   float64
			wantOutage    bool
			wantLearned   bool
		}{
			{name: "future cannot mature history", rate: 3000, futureSamples: 30, futureValue: 12000},
			{name: "insufficient eligible history", rate: 2500, recentSamples: 29, recentValue: 6000, futureSamples: 30, futureValue: 12000},
			{name: "healthy eligible history", rate: 3000, recentSamples: 30, recentValue: 3000, futureSamples: 60, futureValue: 12000},
			{name: "learned equality", rate: 3000, recentSamples: 30, recentValue: 6000, futureSamples: 60, futureValue: 12000},
			{name: "genuine drop", rate: 3000, recentSamples: 30, recentValue: 12000, futureSamples: 60, futureValue: 1000, wantOutage: true, wantLearned: true},
			{name: "six-hour count remains insufficient", rate: 3000, recentSamples: 30, recentValue: 12000, olderSamples: 88, futureSamples: 2, futureValue: 1000, wantOutage: true, wantLearned: true},
			{name: "static zero", rate: 0, futureSamples: 30, futureValue: 12000, wantOutage: true},
			{name: "static outage", rate: 999, futureSamples: 30, futureValue: 500, wantOutage: true},
			{name: "static equality", rate: 1000, futureSamples: 30, futureValue: 12000},
		} {
			store, err := newBaselineStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			for index := 1; index <= test.recentSamples; index++ {
				store.record(contractRateMetric, now.Add(-time.Duration(index)*time.Minute), test.recentValue)
			}
			for index := 0; index < test.olderSamples; index++ {
				store.record(contractRateMetric, now.Add(-2*time.Hour-time.Duration(index)*time.Minute), 1000)
			}
			for index := 0; index < test.futureSamples; index++ {
				store.record(contractRateMetric, now.Add(time.Hour+time.Duration(index)*time.Minute), test.futureValue)
			}
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return []Row{{fmt.Sprint(test.rate)}}, nil
			}}
			settings := syntheticSettings(source)
			settings.Now = time.Now
			settings.runtime = &signalRuntime{baseline: store}
			alerts, err := NewWithSignals(settings, NewContractRateSignal()).Run(context.Background())
			if err != nil {
				t.Fatalf("%s: current valid rate became unknown: %v", test.name, err)
			}
			if test.wantOutage {
				alert := requireAlertClass(t, alerts, "contracts-collapse")
				if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 3 ||
					alert.SignalNumber != "1.1" || alert.SignalID != "pg/contracts-collapse" || alert.Target != "pg-1" {
					t.Errorf("%s: contract outage identity changed", test.name)
				}
				if test.wantLearned {
					if !strings.Contains(alert.Observed, "trailing_median=12000 ratio=25%") {
						t.Errorf("%s: future values suppressed the real learned drop: %s", test.name, alert.Observed)
					}
				} else if !strings.Contains(alert.Baseline, "learned band pending history") {
					t.Errorf("%s: static outage acquired a future learned baseline", test.name)
				}
			} else if len(alerts) != 0 {
				t.Errorf("%s: future history manufactured %d alerts", test.name, len(alerts))
			}
			wantSamples := test.recentSamples + test.olderSamples + test.futureSamples + 1
			samples := store.metricSamples[contractRateMetric]
			if len(samples) != wantSamples || store.metricAppendCounts[contractRateMetric] != wantSamples ||
				samples[len(samples)-1].v != float64(test.rate) || !samples[len(samples)-1].at.Equal(now) {
				t.Errorf("%s: current valid rate was not appended once with retained history", test.name)
			}
		}
	})
}

// Invalid current evidence stays unknown and cannot mutate either eligible or
// future history, or disclose private source text while reporting the failure.
func TestBaselineFutureContractUnknownPreservesHistoryAndPrivacy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		store.record(contractRateMetric, now.Add(-time.Minute), 3000)
		store.record(contractRateMetric, now.Add(time.Hour), 12000)
		beforeSamples := append([]baselineSample(nil), store.metricSamples[contractRateMetric]...)
		before, err := os.ReadFile(store.path(contractRateMetric))
		if err != nil {
			t.Fatal(err)
		}
		privateValue := "synthetic-private-count person@example.invalid"
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{{privateValue}}, nil
		}}
		settings := syntheticSettings(source)
		settings.Now = time.Now
		settings.runtime = &signalRuntime{baseline: store}
		alerts, err := NewWithSignals(settings, NewContractRateSignal()).Run(context.Background())
		if err == nil || len(alerts) != 1 {
			t.Fatal("invalid current source must remain one visibility error")
		}
		alert := requireAlertClass(t, alerts, "cannot-observe")
		if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.Observed != "error_class="+observationErrorClassInvalidResponse {
			t.Fatal("unknown current source changed severity or fixed error classification")
		}
		requireAlertOmits(t, alert, privateValue, "person@example.invalid")
		if strings.Contains(err.Error(), privateValue) || strings.Contains(err.Error(), "person@example.invalid") {
			t.Fatal("invalid current source exposed private text")
		}
		after, readErr := os.ReadFile(store.path(contractRateMetric))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(before, after) || !reflect.DeepEqual(beforeSamples, store.metricSamples[contractRateMetric]) || store.metricAppendCounts[contractRateMetric] != 2 {
			t.Fatal("unknown current source changed retained history")
		}
	})
}

// Future baselines must neither invent nor hide a low rate or reconnect storm.
func TestBaselineFutureConnectionRateBands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			name          string
			rate          int
			recentSamples int
			recentValue   float64
			olderSamples  int
			futureSamples int
			futureValue   float64
			class         string
			sustain       int
		}{
			{name: "future high cannot mature history", rate: 3000, futureSamples: 30, futureValue: 12000},
			{name: "future low cannot mature history", rate: 3000, futureSamples: 30, futureValue: 1000},
			{name: "genuine low rate", rate: 3000, recentSamples: 30, recentValue: 12000, futureSamples: 60, futureValue: 1000, class: "connects-rate", sustain: 5},
			{name: "genuine reconnect storm", rate: 3000, recentSamples: 30, recentValue: 1000, futureSamples: 60, futureValue: 12000, class: "connects-storm", sustain: 3},
			{name: "low equality", rate: 3000, recentSamples: 30, recentValue: 6000, futureSamples: 60, futureValue: 12000},
			{name: "storm equality", rate: 3000, recentSamples: 30, recentValue: 1200, futureSamples: 60, futureValue: 1000},
			{name: "six-hour count remains insufficient", rate: 3000, recentSamples: 30, recentValue: 12000, olderSamples: 88, futureSamples: 2, futureValue: 1000, class: "connects-rate", sustain: 5},
		} {
			store, err := newBaselineStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			for index := 1; index <= test.recentSamples; index++ {
				store.record(connectRateMetric, now.Add(-time.Duration(index)*time.Minute), test.recentValue)
			}
			for index := 0; index < test.olderSamples; index++ {
				store.record(connectRateMetric, now.Add(-2*time.Hour-time.Duration(index)*time.Minute), 1000)
			}
			for index := 0; index < test.futureSamples; index++ {
				store.record(connectRateMetric, now.Add(time.Hour+time.Duration(index)*time.Minute), test.futureValue)
			}
			counterReads := 0
			diagnosticReads := 0
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "SELECT COALESCE(sum(n_tup_ins), 0)") {
					counterReads++
					return []Row{{fmt.Sprint(10000 + test.rate)}}, nil
				}
				diagnosticReads++
				return nil, nil // An unavailable optional cohort cannot hide a real storm.
			}}
			signal := NewConnectionRateSignal()
			probe := signal.(*signalAdapter).probe.(*pgConnectRateProbe)
			probe.initialized = true
			probe.lastCount = 10000
			probe.lastTime = now.Add(-time.Minute)
			settings := syntheticSettings(source)
			settings.Now = time.Now
			settings.runtime = &signalRuntime{baseline: store}
			alerts, err := NewWithSignals(settings, signal).Run(context.Background())
			if err != nil {
				t.Fatalf("%s: valid connection rate became unknown: %v", test.name, err)
			}
			if test.class == "" {
				if len(alerts) != 0 {
					t.Errorf("%s: future history manufactured %d alerts", test.name, len(alerts))
				}
			} else {
				alert := requireAlertClass(t, alerts, test.class)
				if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != test.sustain ||
					alert.SignalNumber != "2.7" || alert.SignalID != "pg/connects-rate" || alert.Target != "pg-1" ||
					!strings.Contains(alert.Observed, fmt.Sprintf("connects_last_min=%d median=%.0f", test.rate, test.recentValue)) {
					t.Errorf("%s: eligible baseline or connection alert identity changed", test.name)
				}
				if test.class == "connects-storm" && alert.Frame != "reconnect-storm" {
					t.Errorf("%s: missing cohort invented a storm initiator", test.name)
				}
			}
			wantDiagnostics := 0
			if test.class == "connects-storm" {
				wantDiagnostics = 1
			}
			wantSamples := test.recentSamples + test.olderSamples + test.futureSamples + 1
			if counterReads != 1 || diagnosticReads != wantDiagnostics || len(store.metricSamples[connectRateMetric]) != wantSamples ||
				store.metricAppendCounts[connectRateMetric] != wantSamples || probe.lastCount != int64(10000+test.rate) || !probe.lastTime.Equal(now) {
				t.Errorf("%s: baseline filtering changed observation or recording ownership", test.name)
			}
		}
	})
}

// Full, redacted family observations retain genuine growth and their existing
// size/ratio boundaries without borrowing future samples to mature history.
func TestBaselineFutureKeyFamilyBandsAndPrivacy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			name          string
			count         int
			recentSamples int
			recentValue   float64
			futureSamples int
			futureValue   float64
			privateFamily bool
			wantGrowth    bool
		}{
			{name: "future cannot mature history", count: 30000, futureSamples: 3, futureValue: 1000},
			{name: "insufficient eligible history", count: 30000, recentSamples: 2, recentValue: 1000, futureSamples: 3, futureValue: 1000},
			{name: "genuine growth", count: 30000, recentSamples: 3, recentValue: 1000, futureSamples: 6, futureValue: 30000, wantGrowth: true},
			{name: "growth equality", count: 30000, recentSamples: 3, recentValue: 10000, futureSamples: 6, futureValue: 1000},
			{name: "minimum size equality", count: 20000, recentSamples: 3, recentValue: 1000, futureSamples: 6, futureValue: 1000},
			{name: "private family growth", count: 30000, recentSamples: 3, recentValue: 1000, futureSamples: 6, futureValue: 30000, privateFamily: true, wantGrowth: true},
		} {
			store, err := newBaselineStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			family := "ckey_<id>"
			observedFamily := family
			if test.privateFamily {
				family = "redacted-unnormalized-family"
				observedFamily = "synthetic-person@example.invalid"
			}
			metric := "redis/family/" + family
			for index := 1; index <= test.recentSamples; index++ {
				store.record(metric, now.Add(-time.Duration(index)*24*time.Hour), test.recentValue)
			}
			for index := 1; index <= test.futureSamples; index++ {
				store.record(metric, now.Add(time.Duration(index)*24*time.Hour), test.futureValue)
			}
			source := &syntheticSource{
				hostFn: func(_ HostSettings, command string) (string, error) {
					if strings.Contains(command, "INFO memory") {
						return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
					}
					return "", fmt.Errorf("unexpected synthetic family census request")
				},
				hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
					return fmt.Sprintf("%d %s\nfamily_source complete %d\n", test.count, observedFamily, test.count), nil
				},
			}
			settings := syntheticSettings(source)
			settings.Now = time.Now
			settings.runtime = &signalRuntime{baseline: store}
			alerts, err := NewWithSignals(settings, NewKeyFamiliesSignal()).Run(context.Background())
			if err != nil {
				t.Fatalf("%s: complete family observation became unknown: %v", test.name, err)
			}
			if test.wantGrowth {
				alert := requireAlertClass(t, alerts, "family-growth")
				if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 1 ||
					alert.SignalNumber != "3.3" || alert.SignalID != "redis/key-families" || alert.Frame != family ||
					alert.Observed != "count=30000 median=1000 node=6380" {
					t.Errorf("%s: future values hid growth or changed its identity", test.name)
				}
				requireAlertOmits(t, alert, "synthetic-person@example.invalid")
			} else if len(alerts) != 0 {
				t.Errorf("%s: future history manufactured %d alerts", test.name, len(alerts))
			}
			wantSamples := test.recentSamples + test.futureSamples + 1
			if len(store.metricSamples[metric]) != wantSamples || store.metricAppendCounts[metric] != wantSamples {
				t.Errorf("%s: complete family observation changed baseline recording", test.name)
			}
			for persistedMetric := range store.metricSamples {
				if strings.Contains(persistedMetric, "synthetic-person") || strings.Contains(persistedMetric, "@") {
					t.Errorf("%s: raw family entered persisted metric identity", test.name)
				}
			}
		}
	})
}
