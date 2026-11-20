// Covers the producer-to-artifact precision boundary: retained observations,
// persisted CSV, sidecar summaries, and the scorer must describe identical rows.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The historical CSV formatting is immutable, including near-zero values and
// floating-point rounding ties. Retained rows must round-trip those exact bytes.
func TestClientDriverRowsMatchCsvPrecision(t *testing.T) {
	var output bytes.Buffer
	var historical bytes.Buffer
	driver := &ClientDriver{out: bufio.NewWriter(&output)}
	driver.writeCsvHeader()
	fmt.Fprintln(&historical, resultCSVHeader)
	durations := []time.Duration{
		0,
		1 * time.Nanosecond,
		499 * time.Nanosecond,
		500 * time.Nanosecond,
		501 * time.Nanosecond,
		999 * time.Nanosecond,
		1 * time.Microsecond,
		1001 * time.Nanosecond,
		499499 * time.Nanosecond,
		499500 * time.Nanosecond,
		499501 * time.Nanosecond,
		500 * time.Microsecond,
		1000499 * time.Nanosecond,
		1000500 * time.Nanosecond,
		1000501 * time.Nanosecond,
		1499500 * time.Nanosecond,
		123456789 * time.Nanosecond,
	}
	for i, duration := range durations {
		start := time.UnixMilli(1000 + int64(i))
		driver.writeCsvRow(start, "synthetic-client", "/synthetic", 0, 200, throughputMinBytes, duration, duration)
		bytesPerSecond := float64(0)
		if 0 < duration {
			bytesPerSecond = float64(throughputMinBytes) / duration.Seconds()
		}
		fmt.Fprintf(&historical, "%d,%s,%s,%d,%d,%d,%.3f,%.3f,%.0f\n",
			start.UnixMilli(), "synthetic-client", "/synthetic", 0, 200, throughputMinBytes,
			float64(duration)/float64(time.Millisecond), float64(duration)/float64(time.Millisecond), bytesPerSecond)
	}
	if err := driver.flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), historical.Bytes()) {
		t.Fatalf("CSV representation changed:\n%s\nwant historical:\n%s", output.String(), historical.String())
	}
	csvPath := filepath.Join(t.TempDir(), "results.csv")
	if err := os.WriteFile(csvPath, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readResultsCsv(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	retained := driver.resultRows()
	if len(retained) != len(parsed) || len(parsed) != len(durations) {
		t.Fatalf("row counts retained/parsed/durations = %d/%d/%d", len(retained), len(parsed), len(durations))
	}
	for i, row := range parsed {
		if retained[i] != row {
			t.Errorf("duration %s retained %+v, but CSV contains %+v", durations[i], retained[i], row)
		}
	}
}

// Multiple occupied blocks give nonzero uncertainty, so parity cannot pass
// merely because a short scoring fixture has no bootstrap variation.
func TestClientDriverBootstrapMatchesCsvPrecision(t *testing.T) {
	var output bytes.Buffer
	driver := &ClientDriver{out: bufio.NewWriter(&output)}
	driver.writeCsvHeader()
	for i := range 64 {
		ttfb := time.Duration(2+i/8)*time.Millisecond + time.Duration(127*i+19)*time.Nanosecond
		total := time.Duration(50+11*(i/8))*time.Millisecond + time.Duration(337*i+1)*time.Nanosecond
		driver.writeCsvRow(time.UnixMilli(int64(i*250)), "synthetic-client", "/synthetic", 0, 200, throughputMinBytes, ttfb, total)
	}
	if err := driver.flush(); err != nil {
		t.Fatal(err)
	}
	csvPath := filepath.Join(t.TempDir(), "results.csv")
	if err := os.WriteFile(csvPath, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := readResultsCsv(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	retained := &RunStats{MeasureStartMs: 0, MeasureEndMs: 16_000}
	recomputed := &RunStats{MeasureStartMs: 0, MeasureEndMs: 16_000}
	summarizeRows(driver.resultRows(), retained)
	summarizeRows(rows, recomputed)
	if retained.BlockCount != 8 || recomputed.BlockCount != 8 {
		t.Fatalf("bootstrap block counts = %d/%d, want 8/8", retained.BlockCount, recomputed.BlockCount)
	}
	if len(retained.Metrics) != len(metricDefs()) || len(recomputed.Metrics) != len(metricDefs()) {
		t.Fatalf("incomplete multi-block metrics: retained=%+v recomputed=%+v", retained.Metrics, recomputed.Metrics)
	}
	for name, want := range recomputed.Metrics {
		if got := retained.Metrics[name]; got != want {
			t.Errorf("%s retained %+v, CSV recomputed %+v", name, got, want)
		}
	}
	for _, name := range []string{"ttfb_p50_ms", "total_p95_ms", "throughput_p50_bytes_per_s"} {
		if retained.Metrics[name].BlockSe <= 0 {
			t.Errorf("%s did not exercise nonzero bootstrap uncertainty", name)
		}
	}
}

// Exercises the real driver, manifest finalization, strict baseline builder,
// and candidate scorer with sub-microsecond timing precision in every metric.
func TestBuildScoreBaselineAuthenticatesDriverCsvPrecision(t *testing.T) {
	options := baselineScoreFixtureOptions()
	fixture := newScoreFixture(t, options)
	var output bytes.Buffer
	driver := &ClientDriver{out: bufio.NewWriter(&output)}
	driver.writeCsvHeader()
	for i := range options.rowCount {
		ttfb := time.Duration(1+i%7)*time.Millisecond + time.Duration((i*257)%1000)*time.Nanosecond
		total := time.Duration(80+i%23)*time.Millisecond + time.Duration((i*337)%1000)*time.Nanosecond
		driver.writeCsvRow(time.UnixMilli(fixture.runStats.MeasureStartMs+int64(i*10)),
			"synthetic-client", "/synthetic", 0, 200, options.bytesPerRow, ttfb, total)
	}
	if err := driver.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.run, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.runStats.ResultsCsvSha256, fixture.runStats.ResultsCsvBytes = driver.csvIdentity()
	summarizeRows(driver.resultRows(), fixture.runStats)
	manifestPath := scoreSidecarForCSV(fixture.run)
	if err := writeRunStats(manifestPath, fixture.runStats); err != nil {
		t.Fatal(err)
	}
	if err := writeFinalMarker(fixture.marker, manifestPath, fixture.runStats); err != nil {
		t.Fatal(err)
	}
	persisted, err := readRunStats(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, _, err := readScoreCSV(fixture.run)
	if err != nil {
		t.Fatal(err)
	}
	recomputed := &RunStats{MeasureStartMs: persisted.MeasureStartMs, MeasureEndMs: persisted.MeasureEndMs}
	summarizeRows(rows, recomputed)
	if len(persisted.Metrics) != len(metricDefs()) || len(recomputed.Metrics) != len(metricDefs()) {
		t.Fatalf("incomplete summary metrics: persisted=%+v recomputed=%+v", persisted.Metrics, recomputed.Metrics)
	}
	// Include total-duration percentiles, all throughput tails, and bootstrap
	// errors, not only the four telemetry values that exposed the mismatch.
	for name, want := range recomputed.Metrics {
		if got := persisted.Metrics[name]; got != want {
			t.Errorf("%s persisted %+v, CSV recomputed %+v", name, got, want)
		}
	}
	baseline, err := BuildScoreBaseline(baselineInputsFromFixture(fixture))
	if err != nil {
		t.Fatalf("driver artifacts rejected by baseline scorer: %v", err)
	}
	if baseline.Replicates[0].RawScore != recomputed.Metrics["total_p95_ms"].Value {
		t.Fatalf("baseline raw score = %v, want CSV total p95 %v", baseline.Replicates[0].RawScore, recomputed.Metrics["total_p95_ms"].Value)
	}
	for _, name := range scoreLiveMetricNames {
		if got, want := baseline.Replicates[0].LiveMetrics[name], recomputed.Metrics[name]; got != want {
			t.Errorf("baseline %s = %+v, want CSV summary %+v", name, got, want)
		}
	}
	writeScoreJSON(t, fixture.baseline, baseline)
	result := Score(fixture.inputs)
	if result.EvalError != nil || !result.Placeable || result.NormalizedScore != 100 {
		t.Fatalf("driver artifacts failed scorer round-trip: %+v", result)
	}
}

// Canonicalizing the producer does not create a tolerance: a single floating-
// point step or count change in any recorded live metric still fails closed.
func TestScoreLiveMetricsRejectsSubPrecisionForgery(t *testing.T) {
	rows := []resultRow{
		{tStartMs: 1, status: 200, bytes: throughputMinBytes, ttfbMs: 1.234, totalMs: 12.345},
		{tStartMs: 2, status: 200, bytes: 2 * throughputMinBytes, ttfbMs: 2.345, totalMs: 23.456},
	}
	runStats := &RunStats{MeasureStartMs: 0, MeasureEndMs: 1000}
	summarizeRows(rows, runStats)
	if _, err := scoreLiveMetricsFromRows(runStats, rows); err != nil {
		t.Fatalf("unaltered metrics rejected: %v", err)
	}
	for _, name := range scoreLiveMetricNames {
		original := runStats.Metrics[name]
		for _, forged := range []MetricSummary{
			{Value: math.Nextafter(original.Value, math.Inf(1)), N: original.N, BlockSe: original.BlockSe},
			{Value: original.Value, N: original.N + 1, BlockSe: original.BlockSe},
		} {
			runStats.Metrics[name] = forged
			_, err := scoreLiveMetricsFromRows(runStats, rows)
			var coded *scoreCodedError
			if !errors.As(err, &coded) || coded.code != "manifest_csv_mismatch" {
				t.Errorf("%s forged metric %+v was not rejected: %v", name, forged, err)
			}
		}
		runStats.Metrics[name] = original
	}
}
