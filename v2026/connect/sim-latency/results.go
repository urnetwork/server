package main

// run measurement artifacts.
//
// The per-request CSV a run prints to stdout stays byte-identical to the
// historical format. Everything the statistical tooling needs beyond the rows
// lives in a side-car run.json (written by `run --meta`, regenerable from a
// CSV with `sim-latency analyze`): the measured window, the workload identity
// (providers.yml sha256 + seed), the build and host fingerprint, and the
// per-metric summaries with their within-run block-bootstrap standard errors.
//
// Metric semantics: `fail_rate` is computed over all in-window rows; timing
// and throughput metrics are computed over successful rows only, with
// `fail_rate` always a non-inferiority guard in the compare verdict so a
// build cannot win latency by dropping hard requests. Throughput quantiles
// are restricted to large transfers (>= throughputMinBytes) so they measure
// bandwidth rather than request latency; `goodput_bytes_per_s` (delivered
// bytes over the whole window) captures aggregate capacity including
// failures. The body-size distribution is seeded by providers.yml, so both
// sides of a comparison see the identical transfer mix.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

const runStatsSchema = 1
const runStatsKind = "sim-latency-run"

// throughput quantiles only count transfers at least this large, so they
// reflect sustained transfer rate rather than time-to-first-byte
const throughputMinBytes = 256 * 1024

// resultRow is one parsed CSV row (the fields the metrics need).
type resultRow struct {
	tStartMs int64
	status   int
	bytes    int64
	ttfbMs   float64
	totalMs  float64
}

func (self *resultRow) success() bool {
	return self.status == 200
}

// MetricSummary is one metric of one run. BlockSe is the within-run
// moving-block bootstrap standard error of Value (0 = unavailable); it is a
// diagnostic and fallback — the primary error term for comparisons is the
// between-run environment variance (see environment.json).
type MetricSummary struct {
	Value   float64 `json:"value"`
	N       int     `json:"n"`
	BlockSe float64 `json:"block_se,omitempty"`
}

// RunStats is the run.json side-car: the run's identity plus its metric
// summaries. Loaded either from the side-car directly or recomputed from a
// results CSV.
type RunStats struct {
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`
	// the artifact path this was loaded from (set at load, kept on save)
	Label string `json:"label,omitempty"`

	// workload identity: two runs are comparable only when these match
	ConfigSha256 string `json:"config_sha256,omitempty"`
	Seed         int64  `json:"seed,omitempty"`

	// build + host fingerprint
	BuildRevision string            `json:"build_revision,omitempty"`
	BuildModified bool              `json:"build_modified,omitempty"`
	Hostname      string            `json:"hostname,omitempty"`
	Os            string            `json:"os,omitempty"`
	Arch          string            `json:"arch,omitempty"`
	NumCpu        int               `json:"num_cpu,omitempty"`
	Flags         map[string]string `json:"flags,omitempty"`

	MeasureStartMs int64 `json:"measure_start_ms"`
	MeasureEndMs   int64 `json:"measure_end_ms"`

	ClientsPool        int `json:"clients_pool,omitempty"`
	ClientsEstablished int `json:"clients_established,omitempty"`

	// totals over the whole CSV / the measured window
	Rows         int `json:"rows"`
	RowsInWindow int `json:"rows_in_window"`
	Failures     int `json:"failures"`

	// block-bootstrap layout used for the BlockSe values
	BlockCount   int     `json:"block_count,omitempty"`
	BlockSeconds float64 `json:"block_seconds,omitempty"`

	Metrics map[string]MetricSummary `json:"metrics"`
}

func (self *RunStats) windowSeconds() float64 {
	return float64(self.MeasureEndMs-self.MeasureStartMs) / 1000
}

// ---- metric definitions ----

// metricDef is one run-level summary statistic. compute returns the value,
// the count of qualifying rows, and ok=false when no rows qualify.
type metricDef struct {
	name        string
	lowerBetter bool
	// primary metrics form the compare verdict (Holm-corrected superiority)
	primary bool
	// guard metrics gate the verdict on non-inferiority (primaries are
	// implicitly guards; this adds fail_rate)
	guard   bool
	compute func(rows []resultRow, windowSeconds float64) (float64, int, bool)
}

func quantileOf(values []float64, q float64) (float64, int, bool) {
	if len(values) == 0 {
		return 0, 0, false
	}
	return quantile(values, q), len(values), true
}

func ttfbValues(rows []resultRow) []float64 {
	values := []float64{}
	for _, row := range rows {
		if row.success() && 0 < row.ttfbMs {
			values = append(values, row.ttfbMs)
		}
	}
	return values
}

func totalValues(rows []resultRow) []float64 {
	values := []float64{}
	for _, row := range rows {
		if row.success() && 0 < row.totalMs {
			values = append(values, row.totalMs)
		}
	}
	return values
}

func throughputValues(rows []resultRow) []float64 {
	values := []float64{}
	for _, row := range rows {
		if row.success() && throughputMinBytes <= row.bytes && 0 < row.totalMs {
			values = append(values, float64(row.bytes)/(row.totalMs/1000))
		}
	}
	return values
}

func ttfbQuantileMetric(name string, q float64, primary bool) metricDef {
	return metricDef{
		name: name, lowerBetter: true, primary: primary, guard: primary,
		compute: func(rows []resultRow, windowSeconds float64) (float64, int, bool) {
			return quantileOf(ttfbValues(rows), q)
		},
	}
}

func totalQuantileMetric(name string, q float64) metricDef {
	return metricDef{
		name: name, lowerBetter: true,
		compute: func(rows []resultRow, windowSeconds float64) (float64, int, bool) {
			return quantileOf(totalValues(rows), q)
		},
	}
}

func throughputQuantileMetric(name string, q float64, primary bool) metricDef {
	return metricDef{
		name: name, lowerBetter: false, primary: primary, guard: primary,
		compute: func(rows []resultRow, windowSeconds float64) (float64, int, bool) {
			return quantileOf(throughputValues(rows), q)
		},
	}
}

// metricDefs is the canonical metric set. Primaries (the compare verdict) are
// the p95 tails of ttfb and throughput; note throughput_p95 is the fast tail
// (best transfers) while throughput_p05 is the struggling tail — both are
// reported.
func metricDefs() []metricDef {
	return []metricDef{
		{
			name: "fail_rate", lowerBetter: true, guard: true,
			compute: func(rows []resultRow, windowSeconds float64) (float64, int, bool) {
				if len(rows) == 0 {
					return 0, 0, false
				}
				failures := 0
				for _, row := range rows {
					if !row.success() {
						failures += 1
					}
				}
				return float64(failures) / float64(len(rows)), len(rows), true
			},
		},
		ttfbQuantileMetric("ttfb_p50_ms", 0.50, false),
		ttfbQuantileMetric("ttfb_p95_ms", 0.95, true),
		totalQuantileMetric("total_p50_ms", 0.50),
		totalQuantileMetric("total_p95_ms", 0.95),
		throughputQuantileMetric("throughput_p05_bytes_per_s", 0.05, false),
		throughputQuantileMetric("throughput_p50_bytes_per_s", 0.50, false),
		throughputQuantileMetric("throughput_p95_bytes_per_s", 0.95, true),
		{
			name: "goodput_bytes_per_s", lowerBetter: false,
			compute: func(rows []resultRow, windowSeconds float64) (float64, int, bool) {
				if len(rows) == 0 || windowSeconds <= 0 {
					return 0, 0, false
				}
				totalBytes := int64(0)
				for _, row := range rows {
					totalBytes += row.bytes
				}
				return float64(totalBytes) / windowSeconds, len(rows), true
			},
		},
	}
}

func metricDefByName(name string) (metricDef, bool) {
	for _, def := range metricDefs() {
		if def.name == name {
			return def, true
		}
	}
	return metricDef{}, false
}

// ---- summarization + block bootstrap ----

const bootstrapResamples = 200
const defaultBlockSeconds = 60.0
const minBlocksPerWindow = 8

// summarizeRows fills stats.Metrics and the totals from the CSV rows, using
// the measure window already set on stats. Rows outside the window are
// counted in Rows but excluded from every metric.
func summarizeRows(rows []resultRow, stats *RunStats) {
	inWindow := []resultRow{}
	for _, row := range rows {
		if stats.MeasureStartMs <= row.tStartMs && row.tStartMs < stats.MeasureEndMs {
			inWindow = append(inWindow, row)
		}
	}
	stats.Rows = len(rows)
	stats.RowsInWindow = len(inWindow)
	stats.Failures = 0
	for _, row := range inWindow {
		if !row.success() {
			stats.Failures += 1
		}
	}

	windowSeconds := stats.windowSeconds()
	blockMs, blockCount := blockLayout(stats.MeasureEndMs - stats.MeasureStartMs)
	stats.BlockCount = blockCount
	stats.BlockSeconds = float64(blockMs) / 1000

	stats.Metrics = map[string]MetricSummary{}
	for i, def := range metricDefs() {
		value, n, ok := def.compute(inWindow, windowSeconds)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		blockSe := blockBootstrapSe(inWindow, stats.MeasureStartMs, blockMs, blockCount, def, windowSeconds, int64(i))
		if math.IsNaN(blockSe) || math.IsInf(blockSe, 0) {
			blockSe = 0
		}
		stats.Metrics[def.name] = MetricSummary{Value: value, N: n, BlockSe: blockSe}
	}
}

// blockLayout picks the bootstrap block duration: 60s blocks (longer than the
// provider regime dwell, so within-block autocorrelation stays inside a
// block), shrunk when the window is short so there are at least
// minBlocksPerWindow blocks.
func blockLayout(windowMs int64) (int64, int) {
	if windowMs <= 0 {
		return 1000, 1
	}
	blockMs := int64(defaultBlockSeconds * 1000)
	if windowMs < int64(minBlocksPerWindow)*blockMs {
		blockMs = windowMs / minBlocksPerWindow
		if blockMs < 1000 {
			blockMs = 1000
		}
	}
	blockCount := int((windowMs + blockMs - 1) / blockMs)
	if blockCount < 1 {
		blockCount = 1
	}
	return blockMs, blockCount
}

// blockBootstrapSe estimates the within-run standard error of a metric by
// resampling time blocks with replacement. Deterministic (seeded from the
// metric index only), so re-analyzing a CSV reproduces the same values. This
// captures within-run sampling noise but not between-run environment noise —
// comparisons prefer the A/A replicate variance and fall back to this with a
// warning.
func blockBootstrapSe(
	rows []resultRow,
	windowStartMs int64,
	blockMs int64,
	blockCount int,
	def metricDef,
	windowSeconds float64,
	seed int64,
) float64 {
	if len(rows) == 0 || blockCount < 2 {
		return 0
	}
	blocks := make([][]resultRow, blockCount)
	for _, row := range rows {
		index := int((row.tStartMs - windowStartMs) / blockMs)
		if index < 0 {
			index = 0
		}
		if blockCount <= index {
			index = blockCount - 1
		}
		blocks[index] = append(blocks[index], row)
	}

	r := newRng(0x0b075e ^ seed)
	values := []float64{}
	resampled := make([]resultRow, 0, len(rows))
	for i := 0; i < bootstrapResamples; i += 1 {
		resampled = resampled[:0]
		for j := 0; j < blockCount; j += 1 {
			resampled = append(resampled, blocks[r.intn(blockCount)]...)
		}
		value, _, ok := def.compute(resampled, windowSeconds)
		if ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			values = append(values, value)
		}
	}
	if len(values) < bootstrapResamples/2 {
		return 0
	}
	_, sd := meanStd(values)
	return sd
}

// ---- artifact io ----

// readResultsCsv parses a run's results CSV (header + one row per request).
func readResultsCsv(path string) ([]resultRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	// log lines can carry unbalanced quotes; parse them as-is so the skip
	// logic below sees one line per record
	reader.LazyQuotes = true
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("%s: read header: %w", path, err)
	}
	column := map[string]int{}
	for i, name := range header {
		column[strings.TrimSpace(name)] = i
	}
	for _, name := range []string{"t_start_ms", "status", "bytes", "ttfb_ms", "total_ms"} {
		if _, ok := column[name]; !ok {
			return nil, fmt.Errorf("%s: not a sim-latency results csv (missing column %s)", path, name)
		}
	}

	// tolerate interleaved non-CSV lines (historically, sdk init redirected
	// stderr onto stdout, so older results files carry log lines): skip
	// anything whose t_start_ms does not parse, but fail loudly when nothing
	// parses at all
	rows := []resultRow{}
	skipped := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			skipped += 1
			continue
		}
		field := func(name string) string {
			index := column[name]
			if len(record) <= index {
				return ""
			}
			return record[index]
		}
		tStartMs, err := strconv.ParseInt(field("t_start_ms"), 10, 64)
		if err != nil {
			skipped += 1
			continue
		}
		status, _ := strconv.Atoi(field("status"))
		bytes, _ := strconv.ParseInt(field("bytes"), 10, 64)
		ttfbMs, _ := strconv.ParseFloat(field("ttfb_ms"), 64)
		totalMs, _ := strconv.ParseFloat(field("total_ms"), 64)
		rows = append(rows, resultRow{
			tStartMs: tStartMs,
			status:   status,
			bytes:    bytes,
			ttfbMs:   ttfbMs,
			totalMs:  totalMs,
		})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no parseable rows (%d lines skipped)", path, skipped)
	}
	if 0 < skipped {
		logf("%s: skipped %d non-CSV lines (logs interleaved with results)", path, skipped)
	}
	return rows, nil
}

func writeRunStats(path string, stats *RunStats) error {
	statsBytes, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(statsBytes, '\n'), 0o644)
}

func readRunStats(path string) (*RunStats, error) {
	statsBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var stats RunStats
	if err := json.Unmarshal(statsBytes, &stats); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if stats.Kind != runStatsKind {
		return nil, fmt.Errorf("%s: kind %q is not a %s", path, stats.Kind, runStatsKind)
	}
	return &stats, nil
}

// sideCarPath returns the run.json path convention for a results CSV:
// `x.csv -> x.run.json` (fallback `x.csv.run.json`).
func sideCarCandidates(csvPath string) []string {
	candidates := []string{}
	if trimmed := strings.TrimSuffix(csvPath, ".csv"); trimmed != csvPath {
		candidates = append(candidates, trimmed+".run.json")
	}
	return append(candidates, csvPath+".run.json")
}

// loadRunArtifact loads one run for analysis: either a run.json directly
// (metric summaries used as stored) or a results CSV, whose metrics are
// recomputed from the rows with the measure window taken from the side-car
// run.json (or the explicit override for CSVs without one).
func loadRunArtifact(path string, windowOverride *measureWindow) (*RunStats, error) {
	if strings.HasSuffix(path, ".json") {
		stats, err := readRunStats(path)
		if err != nil {
			return nil, err
		}
		stats.Label = path
		return stats, nil
	}

	rows, err := readResultsCsv(path)
	if err != nil {
		return nil, err
	}

	var stats *RunStats
	if windowOverride != nil {
		stats = &RunStats{
			Schema:         runStatsSchema,
			Kind:           runStatsKind,
			MeasureStartMs: windowOverride.startMs,
			MeasureEndMs:   windowOverride.endMs,
		}
	} else {
		for _, candidate := range sideCarCandidates(path) {
			if _, statErr := os.Stat(candidate); statErr == nil {
				stats, err = readRunStats(candidate)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		if stats == nil {
			return nil, fmt.Errorf(
				"%s: no side-car run metadata found (looked for %s); the measure window lives there. Re-run with `run --meta`, or pass --window=<startMs,endMs>",
				path,
				strings.Join(sideCarCandidates(path), ", "),
			)
		}
	}
	if stats.MeasureEndMs <= stats.MeasureStartMs {
		return nil, fmt.Errorf("%s: empty measure window [%d, %d]", path, stats.MeasureStartMs, stats.MeasureEndMs)
	}
	stats.Label = path
	// the rows are the source of truth; recompute the summaries
	summarizeRows(rows, stats)
	return stats, nil
}

type measureWindow struct {
	startMs int64
	endMs   int64
}

func parseMeasureWindow(s string) (*measureWindow, error) {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("window must be <startMs>,<endMs>")
	}
	startMs, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("window start: %w", err)
	}
	endMs, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("window end: %w", err)
	}
	if endMs <= startMs {
		return nil, fmt.Errorf("window end must be after start")
	}
	return &measureWindow{startMs: startMs, endMs: endMs}, nil
}
