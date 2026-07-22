package main

// tests for the statistical comparison tooling: the numerical kernel against
// known values, metric semantics on synthetic rows, artifact round-trips,
// the environment noise floor, and the compare verdict rules.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

func almostEqual(t *testing.T, name string, got float64, want float64, tolerance float64) {
	t.Helper()
	if math.IsNaN(got) || tolerance < math.Abs(got-want) {
		t.Fatalf("%s = %v, want %v ± %v", name, got, want, tolerance)
	}
}

// the survival function against textbook critical values, the exact Cauchy
// case, symmetry, and the normal limit.
func TestStudentTSf(t *testing.T) {
	almostEqual(t, "sf(0, 5)", studentTSf(0, 5), 0.5, 1e-12)
	// df=1 is Cauchy: P(T >= 1) = 1/4 exactly
	almostEqual(t, "sf(1, 1)", studentTSf(1, 1), 0.25, 1e-9)
	// one-sided 5% critical values from t tables
	almostEqual(t, "sf(2.132, 4)", studentTSf(2.132, 4), 0.05, 5e-4)
	almostEqual(t, "sf(1.812, 10)", studentTSf(1.812, 10), 0.05, 5e-4)
	almostEqual(t, "sf(2.764, 10)", studentTSf(2.764, 10), 0.01, 5e-4)
	// normal limit
	almostEqual(t, "sf(1.6449, 1e6)", studentTSf(1.6449, 1e6), 0.05, 1e-3)
	// symmetry
	for _, tv := range []float64{0.3, 1.7, 4.2} {
		almostEqual(t, "symmetry", studentTSf(-tv, 7)+studentTSf(tv, 7), 1.0, 1e-9)
	}
}

func TestStudentTCrit(t *testing.T) {
	almostEqual(t, "crit(0.05, 4)", studentTCrit(0.05, 4), 2.132, 5e-3)
	almostEqual(t, "crit(0.05, 10)", studentTCrit(0.05, 10), 1.812, 5e-3)
	// round trip
	crit := studentTCrit(0.025, 7)
	almostEqual(t, "sf(crit(0.025, 7))", studentTSf(crit, 7), 0.025, 1e-6)
}

func TestQuantile(t *testing.T) {
	// R-7 linear interpolation on 1..10
	values := func() []float64 {
		// deliberately unsorted: quantile must not depend on input order
		return []float64{7, 1, 9, 3, 10, 5, 2, 8, 6, 4}
	}
	almostEqual(t, "p50", quantile(values(), 0.50), 5.5, 1e-12)
	almostEqual(t, "p95", quantile(values(), 0.95), 9.55, 1e-12)
	almostEqual(t, "p05", quantile(values(), 0.05), 1.45, 1e-12)
	almostEqual(t, "p0", quantile(values(), 0), 1, 1e-12)
	almostEqual(t, "p100", quantile(values(), 1), 10, 1e-12)
	almostEqual(t, "single", quantile([]float64{3}, 0.95), 3, 1e-12)
	if !math.IsNaN(quantile([]float64{}, 0.5)) {
		t.Fatalf("empty quantile should be NaN")
	}
	// a larger shuffled input against a full-sort reference
	large := []float64{}
	for i := 0; i < 1000; i += 1 {
		large = append(large, float64((i*677)%1009))
	}
	almostEqual(t, "large p95", quantile(large, 0.95), quantileBySort(large, 0.95), 1e-9)
	almostEqual(t, "large p05", quantile(large, 0.05), quantileBySort(large, 0.05), 1e-9)
}

func quantileBySort(values []float64, q float64) float64 {
	sorted := append([]float64{}, values...)
	for i := 1; i < len(sorted); i += 1 {
		for j := i; 0 < j && sorted[j] < sorted[j-1]; j -= 1 {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	h := q * float64(len(sorted)-1)
	k := int(math.Floor(h))
	if k+1 < len(sorted) {
		return sorted[k] + (h-float64(k))*(sorted[k+1]-sorted[k])
	}
	return sorted[k]
}

func TestHolmAdjust(t *testing.T) {
	adjusted := holmAdjust([]float64{0.01, 0.04, 0.03})
	almostEqual(t, "holm[0]", adjusted[0], 0.03, 1e-12)
	almostEqual(t, "holm[1]", adjusted[1], 0.06, 1e-12)
	almostEqual(t, "holm[2]", adjusted[2], 0.06, 1e-12)
	// NaN entries do not count toward the family
	adjusted = holmAdjust([]float64{0.02, math.NaN()})
	almostEqual(t, "holm single", adjusted[0], 0.02, 1e-12)
	if !math.IsNaN(adjusted[1]) {
		t.Fatalf("NaN input should stay NaN")
	}
}

// synthetic rows with known summaries: fail_rate over all rows, timing over
// successes only, throughput restricted to large transfers, goodput over the
// window.
func TestSummarizeRows(t *testing.T) {
	rows := []resultRow{}
	// 90 successes: ttfb 1..90 ms, total 2..180 ms, small bodies (excluded
	// from throughput)
	for i := 1; i <= 90; i += 1 {
		rows = append(rows, resultRow{
			tStartMs: int64(1000 + i),
			status:   200,
			bytes:    1024,
			ttfbMs:   float64(i),
			totalMs:  float64(2 * i),
		})
	}
	// 10 failures
	for i := 0; i < 10; i += 1 {
		rows = append(rows, resultRow{tStartMs: int64(2000 + i), status: 0, totalMs: 5000})
	}
	// 4 large transfers: 1 MiB in 1s, 2s, 4s, 8s
	for i, seconds := range []float64{1, 2, 4, 8} {
		rows = append(rows, resultRow{
			tStartMs: int64(3000 + i),
			status:   200,
			bytes:    1024 * 1024,
			ttfbMs:   10,
			totalMs:  seconds * 1000,
		})
	}
	// a row outside the window must not count anywhere
	rows = append(rows, resultRow{tStartMs: 999999, status: 0})

	stats := &RunStats{
		Schema:         runStatsSchema,
		Kind:           runStatsKind,
		MeasureStartMs: 0,
		MeasureEndMs:   10_000,
	}
	summarizeRows(rows, stats)

	if stats.Rows != 105 || stats.RowsInWindow != 104 {
		t.Fatalf("rows=%d inWindow=%d, want 105/104", stats.Rows, stats.RowsInWindow)
	}
	if stats.Failures != 10 {
		t.Fatalf("failures=%d, want 10", stats.Failures)
	}
	almostEqual(t, "fail_rate", stats.Metrics["fail_rate"].Value, 10.0/104, 1e-9)
	// ttfb over the 94 successes: 1..90 plus four 10s
	if n := stats.Metrics["ttfb_p50_ms"].N; n != 94 {
		t.Fatalf("ttfb n=%d, want 94", n)
	}
	// throughput only over the 4 large rows
	throughput := stats.Metrics["throughput_p50_bytes_per_s"]
	if throughput.N != 4 {
		t.Fatalf("throughput n=%d, want 4", throughput.N)
	}
	mib := 1024.0 * 1024
	almostEqual(t, "throughput_p50", throughput.Value, (mib/2+mib/4)/2, 1)
	// goodput = all delivered bytes / window
	wantGoodput := (90*1024.0 + 4*mib) / 10.0
	almostEqual(t, "goodput", stats.Metrics["goodput_bytes_per_s"].Value, wantGoodput, 1)
}

func TestBlockBootstrapDeterministic(t *testing.T) {
	rows := []resultRow{}
	for i := 0; i < 500; i += 1 {
		rows = append(rows, resultRow{
			tStartMs: int64(i * 20),
			status:   200,
			bytes:    1024,
			ttfbMs:   float64(50 + (i*37)%40),
			totalMs:  float64(100 + (i*53)%80),
		})
	}
	stats := func() *RunStats {
		s := &RunStats{Schema: runStatsSchema, Kind: runStatsKind, MeasureStartMs: 0, MeasureEndMs: 10_000}
		summarizeRows(rows, s)
		return s
	}
	a, b := stats(), stats()
	for name, summaryA := range a.Metrics {
		summaryB := b.Metrics[name]
		if summaryA.BlockSe != summaryB.BlockSe {
			t.Fatalf("%s block se not deterministic: %v vs %v", name, summaryA.BlockSe, summaryB.BlockSe)
		}
	}
	if a.Metrics["ttfb_p50_ms"].BlockSe <= 0 {
		t.Fatalf("expected a positive block se on synthetic rows")
	}
}

// the CSV emitted by the driver parses back, and loadRunArtifact ties rows to
// the side-car window (or the explicit override), erroring when neither
// exists.
func TestRunArtifactRoundTrip(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "a.csv")
	csvContent := `t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s
1000,c1,/,0,200,4096,10.000,20.000,204800
2000,c1,/x,1,200,4096,30.000,60.000,68267
3000,c1,/y,1,0,0,0.000,5000.000,0
999999,c1,/late,1,200,4096,10.000,20.000,204800
`
	if err := os.WriteFile(csvPath, []byte(csvContent), 0o644); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	// no side-car, no window: a clear error pointing at the convention
	if _, err := loadRunArtifact(csvPath, nil); err == nil || !strings.Contains(err.Error(), "run.json") {
		t.Fatalf("expected a side-car error, got %v", err)
	}

	// explicit window override
	stats, err := loadRunArtifact(csvPath, &measureWindow{startMs: 0, endMs: 10_000})
	if err != nil {
		t.Fatalf("load with window: %v", err)
	}
	if stats.Rows != 4 || stats.RowsInWindow != 3 || stats.Failures != 1 {
		t.Fatalf("rows=%d inWindow=%d failures=%d, want 4/3/1", stats.Rows, stats.RowsInWindow, stats.Failures)
	}
	almostEqual(t, "ttfb_p50", stats.Metrics["ttfb_p50_ms"].Value, 20, 1e-9)

	// side-car: written, then found by convention and used for the window
	stats.ConfigSha256 = "abc"
	sideCarPath := filepath.Join(dir, "a.run.json")
	if err := writeRunStats(sideCarPath, stats); err != nil {
		t.Fatalf("write side-car: %v", err)
	}
	loaded, err := loadRunArtifact(csvPath, nil)
	if err != nil {
		t.Fatalf("load with side-car: %v", err)
	}
	if loaded.ConfigSha256 != "abc" || loaded.MeasureEndMs != 10_000 {
		t.Fatalf("side-car meta not applied: %+v", loaded)
	}
	if loaded.RowsInWindow != 3 {
		t.Fatalf("rows not recomputed from csv")
	}

	// a run.json loads directly too
	direct, err := loadRunArtifact(sideCarPath, nil)
	if err != nil {
		t.Fatalf("load run.json: %v", err)
	}
	if direct.Metrics["ttfb_p50_ms"].Value != stats.Metrics["ttfb_p50_ms"].Value {
		t.Fatalf("run.json metrics differ")
	}
}

// mkRun builds a synthetic run for verdict tests.
func mkRun(label string, sha string, metrics map[string]float64, blockSe float64) *RunStats {
	stats := &RunStats{
		Schema:         runStatsSchema,
		Kind:           runStatsKind,
		Label:          label,
		ConfigSha256:   sha,
		MeasureStartMs: 0,
		MeasureEndMs:   600_000,
		BlockCount:     10,
		Metrics:        map[string]MetricSummary{},
	}
	for name, value := range metrics {
		stats.Metrics[name] = MetricSummary{Value: value, N: 1000, BlockSe: blockSe}
	}
	return stats
}

func mkBaseline(sds map[string]float64) *Baseline {
	baseline := &Baseline{
		Schema:     baselineSchema,
		Kind:       baselineKind,
		Alpha:      0.05,
		Replicates: 5,
		Metrics:    map[string]BaselineMetric{},
	}
	for name, sd := range sds {
		baseline.Metrics[name] = BaselineMetric{Mean: 0, Sd: sd}
	}
	return baseline
}

func referenceMetrics() map[string]float64 {
	return map[string]float64{
		"fail_rate":                  0.01,
		"ttfb_p50_ms":                100,
		"ttfb_p95_ms":                300,
		"total_p50_ms":               200,
		"total_p95_ms":               800,
		"throughput_p05_bytes_per_s": 50_000,
		"throughput_p50_bytes_per_s": 200_000,
		"throughput_p95_bytes_per_s": 900_000,
		"goodput_bytes_per_s":        1_000_000,
	}
}

func referenceSds() map[string]float64 {
	return map[string]float64{
		"fail_rate":                  0.002,
		"ttfb_p50_ms":                2,
		"ttfb_p95_ms":                6,
		"total_p50_ms":               4,
		"total_p95_ms":               20,
		"throughput_p05_bytes_per_s": 2_000,
		"throughput_p50_bytes_per_s": 5_000,
		"throughput_p95_bytes_per_s": 20_000,
		"goodput_bytes_per_s":        30_000,
	}
}

func TestCompareVerdictABetter(t *testing.T) {
	better := referenceMetrics()
	// ~10 env sds better on both primaries
	better["ttfb_p95_ms"] = 240
	better["throughput_p95_bytes_per_s"] = 1_100_000
	a := mkRun("a", "sha1", better, 0)
	b := mkRun("b", "sha1", referenceMetrics(), 0)

	result, err := CompareRuns([]*RunStats{a}, []*RunStats{b}, mkBaseline(referenceSds()), 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Verdict != "a_better" {
		t.Fatalf("verdict=%s (%s), want a_better", result.Verdict, result.Reason)
	}
	for _, comparison := range result.Metrics {
		if comparison.Name == "ttfb_p95_ms" {
			if !comparison.SignificantlyBetterA || comparison.Source != "baseline" {
				t.Fatalf("ttfb_p95: %+v", comparison)
			}
			if comparison.PHolmA <= 0 || 0.05 <= comparison.PHolmA {
				t.Fatalf("ttfb_p95 holm p = %v", comparison.PHolmA)
			}
		}
	}
}

func TestCompareVerdictIndistinguishable(t *testing.T) {
	near := referenceMetrics()
	// well inside the noise floor
	near["ttfb_p95_ms"] = 299
	a := mkRun("a", "sha1", near, 0)
	b := mkRun("b", "sha1", referenceMetrics(), 0)
	result, err := CompareRuns([]*RunStats{a}, []*RunStats{b}, mkBaseline(referenceSds()), 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Verdict != "indistinguishable" {
		t.Fatalf("verdict=%s (%s), want indistinguishable", result.Verdict, result.Reason)
	}
	// the min detectable delta is reported so under-powered != no difference
	for _, comparison := range result.Metrics {
		if comparison.Name == "ttfb_p95_ms" && comparison.MinDetectableDelta <= 0 {
			t.Fatalf("min detectable delta missing: %+v", comparison)
		}
	}
}

// the fail_rate guard: a big latency win cannot carry the verdict when the
// failure rate significantly regressed (dropping hard requests fakes speed).
func TestCompareFailRateGuard(t *testing.T) {
	cheat := referenceMetrics()
	cheat["ttfb_p95_ms"] = 240
	cheat["fail_rate"] = 0.05
	a := mkRun("a", "sha1", cheat, 0)
	b := mkRun("b", "sha1", referenceMetrics(), 0)
	result, err := CompareRuns([]*RunStats{a}, []*RunStats{b}, mkBaseline(referenceSds()), 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if result.Verdict != "mixed" {
		t.Fatalf("verdict=%s (%s), want mixed", result.Verdict, result.Reason)
	}
}

func TestCompareConfigMismatch(t *testing.T) {
	a := mkRun("a", "sha1", referenceMetrics(), 0)
	b := mkRun("b", "sha2", referenceMetrics(), 0)
	if _, err := CompareRuns([]*RunStats{a}, []*RunStats{b}, nil, 0.05); err == nil {
		t.Fatalf("expected a config mismatch error")
	}
}

// without environment.json a 1v1 comparison falls back to the within-run
// block SEs, flagged optimistic.
func TestCompareBlocksFallback(t *testing.T) {
	better := referenceMetrics()
	better["ttfb_p95_ms"] = 200
	a := mkRun("a", "sha1", better, 5)
	b := mkRun("b", "sha1", referenceMetrics(), 5)
	result, err := CompareRuns([]*RunStats{a}, []*RunStats{b}, nil, 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	foundOptimistic := false
	for _, comparison := range result.Metrics {
		if comparison.Name == "ttfb_p95_ms" {
			if !comparison.Testable || comparison.Source != "blocks" || !comparison.Optimistic {
				t.Fatalf("ttfb_p95: %+v", comparison)
			}
			foundOptimistic = true
		}
	}
	if !foundOptimistic {
		t.Fatalf("ttfb_p95 comparison missing")
	}
	warned := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "optimistic") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected an optimistic-fallback warning, got %v", result.Warnings)
	}
}

// with replicates on both sides and no env, welch on the run-level values
// carries the test.
func TestCompareWelch(t *testing.T) {
	mkSide := func(prefix string, ttfbP95 []float64) []*RunStats {
		runs := []*RunStats{}
		for i, v := range ttfbP95 {
			metrics := referenceMetrics()
			metrics["ttfb_p95_ms"] = v
			runs = append(runs, mkRun(prefix+string(rune('0'+i)), "sha1", metrics, 0))
		}
		return runs
	}
	a := mkSide("a", []float64{200, 202, 198})
	b := mkSide("b", []float64{300, 301, 299})
	result, err := CompareRuns(a, b, nil, 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	for _, comparison := range result.Metrics {
		if comparison.Name == "ttfb_p95_ms" {
			if comparison.Source != "welch" || !comparison.SignificantlyBetterA {
				t.Fatalf("ttfb_p95: %+v", comparison)
			}
		}
	}
}

func TestComputeBaseline(t *testing.T) {
	runs := []*RunStats{}
	for i, v := range []float64{100, 101, 99, 100.5, 99.5} {
		metrics := referenceMetrics()
		metrics["ttfb_p95_ms"] = v
		runs = append(runs, mkRun("r"+string(rune('0'+i)), "sha1", metrics, 0))
	}
	baseline, _, err := computeBaseline(runs, 0.05)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if baseline.Replicates != 5 {
		t.Fatalf("replicates=%d", baseline.Replicates)
	}
	metric := baseline.Metrics["ttfb_p95_ms"]
	almostEqual(t, "mean", metric.Mean, 100, 1e-9)
	almostEqual(t, "sd", metric.Sd, 0.7906, 1e-3)
	if metric.MinDetectableDelta <= metric.Sd {
		t.Fatalf("min detectable delta %v should exceed sd %v", metric.MinDetectableDelta, metric.Sd)
	}
	// sd sampling uncertainty at k=5: 1/sqrt(2*4)
	almostEqual(t, "sd_rel_error", metric.SdRelError, 0.3536, 1e-3)

	// estimate stability: sd from the first m=2..5 replicates, ending at the
	// full-k value
	if len(metric.SdByReplicates) != 4 {
		t.Fatalf("sd_by_replicates len=%d, want 4", len(metric.SdByReplicates))
	}
	almostEqual(t, "sd_by_replicates[0]", metric.SdByReplicates[0], 0.7071, 1e-3)
	almostEqual(t, "sd_by_replicates last", metric.SdByReplicates[3], metric.Sd, 1e-12)

	// measurement convergence: mdd for m=1..5 runs per side, sqrt(1/m)
	// scaling from the 1-run value
	if len(metric.MinDetectableDeltaByRunsPerSide) != 5 {
		t.Fatalf("mdd_by_runs len=%d, want 5", len(metric.MinDetectableDeltaByRunsPerSide))
	}
	almostEqual(t, "mdd[0]", metric.MinDetectableDeltaByRunsPerSide[0], metric.MinDetectableDelta, 1e-12)
	almostEqual(t, "mdd[3] = mdd[0]/2", metric.MinDetectableDeltaByRunsPerSide[3], metric.MinDetectableDelta/2, 1e-9)
	for m := 1; m < len(metric.MinDetectableDeltaByRunsPerSide); m += 1 {
		if metric.MinDetectableDeltaByRunsPerSide[m-1] <= metric.MinDetectableDeltaByRunsPerSide[m] {
			t.Fatalf("mdd_by_runs not decreasing at m=%d", m+1)
		}
	}

	// too few replicates
	if _, _, err := computeBaseline(runs[:2], 0.05); err == nil {
		t.Fatalf("expected an error for 2 replicates")
	}
	// mixed configs
	runs[1].ConfigSha256 = "other"
	if _, _, err := computeBaseline(runs, 0.05); err == nil {
		t.Fatalf("expected an error for mixed config shas")
	}
}

// baseline.json round-trips and rejects foreign json.
func TestBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	baseline := mkBaseline(referenceSds())
	baseline.Replicates = 5
	if err := writeBaseline(path, baseline); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := readBaseline(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if loaded.Metrics["ttfb_p95_ms"].Sd != baseline.Metrics["ttfb_p95_ms"].Sd {
		t.Fatalf("sd changed across round trip")
	}
	// a run.json is not a baseline.json
	runPath := filepath.Join(dir, "a.run.json")
	if err := writeRunStats(runPath, mkRun("a", "sha1", referenceMetrics(), 0)); err != nil {
		t.Fatalf("write run: %v", err)
	}
	if _, err := readBaseline(runPath); err == nil {
		t.Fatalf("expected a kind error")
	}
}

// the baseline measuring mode forwards the run-shaping flags to each
// replicate subprocess, always with --reset and the replicate's meta path.
func TestReplicateRunArgs(t *testing.T) {
	opts := docopt.Opts{
		"--providers": "p.yml",
		"--duration":  "5m",
		"--no-impair": true,
	}
	args := replicateRunArgs(opts, "dir/baseline-3.run.json")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run", "--reset", "--meta dir/baseline-3.run.json",
		"--providers=p.yml", "--duration=5m", "--no-impair",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	// flags the user did not pass are not forwarded
	if strings.Contains(joined, "--prewarm") || strings.Contains(joined, "--hosts") {
		t.Fatalf("args %q forward unset flags", joined)
	}
	if args[0] != "run" {
		t.Fatalf("first arg must be the run command, got %q", args[0])
	}
}

// max_connections: sampled per provider from the component's range, and 0
// (absent in older files) means unlimited.
func TestMaxConnectionsSampled(t *testing.T) {
	config := defaultConfig(11, 300, 10, 60)
	if err := generateFleet(config); err != nil {
		t.Fatalf("generate: %v", err)
	}
	componentRanges := map[string]Range{}
	for _, component := range config.Providers.Mixture {
		componentRanges[component.Name] = component.MaxConnections
	}
	for _, entry := range config.Fleet {
		r := componentRanges[entry.Component]
		if entry.MaxConnections < int(r.Min) || int(math.Ceil(r.Max)) < entry.MaxConnections {
			t.Fatalf("entry %d max_connections %d outside component range [%v, %v]",
				entry.Index, entry.MaxConnections, r.Min, r.Max)
		}
		if entry.MaxConnections <= 0 {
			t.Fatalf("default mixture should sample a positive cap")
		}
	}
}
