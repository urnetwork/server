package main

// statistical comparison of simulation runs.
//
// The unit of replication is the run, not the request: requests within a run
// are heavily autocorrelated (shared market state, provider regime dwells,
// the warm client pool), so per-request tests wildly overstate certainty.
// Because providers.yml seeds the entire workload (arrivals, crawl trees,
// body sizes, churn), two runs of the same file replay the identical
// workload, and the difference between two runs of *unchanged* code is pure
// environment noise. `sim-latency baseline` measures exactly that from k
// replicates of unchanged code and exports it as baseline.json; `compare`
// then tests an observed A-vs-B difference against that noise floor:
//
//	t = (S_A - S_B) / (sd * sqrt(1/m_A + 1/m_B)),  df = k - 1
//
// With >= 2 runs per side a Welch t on the run-level statistics is also
// computed and the more conservative p wins. Without baseline.json the
// within-run block-bootstrap SEs are the fallback — flagged optimistic,
// because they cannot see between-run effects (warm-pool composition,
// scheduling, backing-store timing).
//
// Verdict rule: A beats B at level p iff some primary metric (ttfb_p95,
// throughput_p95) is significantly better after Holm correction, and no
// guard metric (the primaries + fail_rate) is significantly worse at raw
// alpha. fail_rate guards the success-only timing metrics: a build cannot
// win latency by dropping hard requests.

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/docopt/docopt-go"
)

// ---- comparison ----

// MetricComparison is the test result for one metric. T is signed toward
// "A better" (positive favors A) in the metric's better direction.
type MetricComparison struct {
	Name        string `json:"name"`
	LowerBetter bool   `json:"lower_better"`
	Primary     bool   `json:"primary"`
	Guard       bool   `json:"guard"`

	A        float64 `json:"a"`
	B        float64 `json:"b"`
	Delta    float64 `json:"delta"`
	DeltaPct float64 `json:"delta_pct"`
	// observed direction: which side looks better, ignoring significance
	BetterSide string `json:"better_side,omitempty"`

	// Testable=false when no error term is available (then the test fields
	// are zero and carry no meaning)
	Testable bool    `json:"testable"`
	T        float64 `json:"t,omitempty"`
	Df       float64 `json:"df,omitempty"`
	// one-sided p-values for each direction
	PA float64 `json:"p_a,omitempty"`
	PB float64 `json:"p_b,omitempty"`
	// Holm-adjusted across the primary set (primaries only)
	PHolmA float64 `json:"p_holm_a,omitempty"`
	PHolmB float64 `json:"p_holm_b,omitempty"`
	// which error term produced p: "baseline", "welch", "baseline+welch",
	// "blocks"
	Source string `json:"source,omitempty"`
	// blocks-only source: between-run noise not captured, p optimistic
	Optimistic bool `json:"optimistic,omitempty"`
	// smallest |delta| that would reach significance at the current runs
	// per side (baseline source only)
	MinDetectableDelta float64 `json:"min_detectable_delta,omitempty"`

	SignificantlyBetterA bool `json:"significantly_better_a,omitempty"`
	SignificantlyBetterB bool `json:"significantly_better_b,omitempty"`
}

type CompareResult struct {
	Alpha float64  `json:"alpha"`
	RunsA []string `json:"runs_a"`
	RunsB []string `json:"runs_b"`
	// "a_better" | "b_better" | "mixed" | "indistinguishable"
	Verdict string `json:"verdict"`
	// one-line human explanation of the verdict
	Reason   string             `json:"reason"`
	Metrics  []MetricComparison `json:"metrics"`
	Warnings []string           `json:"warnings,omitempty"`
}

// CompareRuns tests whether the difference between run sets A and B exceeds
// what environment noise alone produces, at significance level alpha.
func CompareRuns(a []*RunStats, b []*RunStats, baseline *Baseline, alpha float64) (*CompareResult, error) {
	if len(a) == 0 || len(b) == 0 {
		return nil, fmt.Errorf("both sides need at least one run")
	}
	if alpha <= 0 || 0.5 < alpha {
		return nil, fmt.Errorf("p threshold %v out of range (0, 0.5]", alpha)
	}

	warnings := []string{}
	shaA, err := consistentConfigSha(a)
	if err != nil {
		return nil, fmt.Errorf("side A: %w", err)
	}
	shaB, err := consistentConfigSha(b)
	if err != nil {
		return nil, fmt.Errorf("side B: %w", err)
	}
	if shaA != "" && shaB != "" && shaA != shaB {
		return nil, fmt.Errorf(
			"A and B ran different providers.yml (sha %s vs %s): different workloads are not comparable",
			shortSha(shaA), shortSha(shaB),
		)
	}
	if shaA == "" || shaB == "" {
		warnings = append(warnings, "a side carries no config sha; cannot verify both ran the same providers.yml")
	}
	if baseline != nil {
		if baseline.ConfigSha256 != "" && shaA != "" && baseline.ConfigSha256 != shaA {
			warnings = append(warnings, "baseline.json was measured on a different providers.yml; its noise floor may not apply")
		}
		runDuration := a[0].windowSeconds()
		if 0 < baseline.DurationS && 0 < runDuration {
			ratio := runDuration / baseline.DurationS
			if ratio < 0.9 || 1.1 < ratio {
				warnings = append(warnings, fmt.Sprintf(
					"baseline.json was measured at %.0fs windows but the runs use %.0fs; variance depends on duration",
					baseline.DurationS, runDuration,
				))
			}
		}
		if baseline.Hostname != "" && a[0].Hostname != "" && baseline.Hostname != a[0].Hostname {
			warnings = append(warnings, "baseline.json was measured on a different host")
		}
	}
	if warmed := establishedImbalance(a, b); warmed != "" {
		warnings = append(warnings, warmed)
	}

	result := &CompareResult{Alpha: alpha}
	for _, run := range a {
		result.RunsA = append(result.RunsA, run.Label)
	}
	for _, run := range b {
		result.RunsB = append(result.RunsB, run.Label)
	}

	optimistic := false
	underpowered := []string{}
	for _, def := range metricDefs() {
		comparison := compareMetric(def, a, b, baseline, alpha)
		if comparison == nil {
			continue
		}
		if comparison.Optimistic {
			optimistic = true
		}
		if baseline != nil && comparison.Testable {
			if baselineMetric, ok := baseline.Metrics[def.name]; ok {
				maxBlockSe := 0.0
				for _, run := range append(append([]*RunStats{}, a...), b...) {
					if summary, ok := run.Metrics[def.name]; ok {
						maxBlockSe = math.Max(maxBlockSe, summary.BlockSe)
					}
				}
				if baselineMetric.Sd < maxBlockSe {
					underpowered = append(underpowered, def.name)
				}
			}
		}
		result.Metrics = append(result.Metrics, *comparison)
	}
	if optimistic {
		warnings = append(warnings, "no baseline.json: p-values use within-run block SEs only, which miss between-run noise (warm pool, scheduling); treat significance as optimistic and measure the noise floor with `sim-latency baseline`")
	}
	if 0 < len(underpowered) {
		warnings = append(warnings, fmt.Sprintf(
			"baseline sd is below a run's within-run block SE for %s: the noise floor is likely understated",
			strings.Join(underpowered, ", "),
		))
	}

	applyHolmAndVerdict(result, alpha)
	result.Warnings = warnings
	return result, nil
}

// compareMetric builds the per-metric comparison, choosing the most
// conservative available error term. Returns nil when the metric is absent
// from every run on a side.
func compareMetric(def metricDef, a []*RunStats, b []*RunStats, baseline *Baseline, alpha float64) *MetricComparison {
	valuesA, blockVarA, blocksA := metricSide(def.name, a)
	valuesB, blockVarB, blocksB := metricSide(def.name, b)
	if len(valuesA) == 0 || len(valuesB) == 0 {
		return nil
	}
	meanA, _ := meanStd(valuesA)
	meanB, _ := meanStd(valuesB)

	comparison := &MetricComparison{
		Name:        def.name,
		LowerBetter: def.lowerBetter,
		Primary:     def.primary,
		Guard:       def.guard || def.primary,
		A:           meanA,
		B:           meanB,
		Delta:       meanA - meanB,
	}
	if meanB != 0 {
		comparison.DeltaPct = (meanA - meanB) / math.Abs(meanB) * 100
	}
	// advantage of A in the metric's better direction
	advantage := meanA - meanB
	if def.lowerBetter {
		advantage = -advantage
	}
	if 0 < advantage {
		comparison.BetterSide = "a"
	} else if advantage < 0 {
		comparison.BetterSide = "b"
	}

	// candidate error terms; p = the most conservative available
	type errorTerm struct {
		source string
		t      float64
		df     float64
	}
	terms := []errorTerm{}
	if baseline != nil {
		if baselineMetric, ok := baseline.Metrics[def.name]; ok && 0 < baselineMetric.Sd {
			se := baselineMetric.Sd * math.Sqrt(1/float64(len(valuesA))+1/float64(len(valuesB)))
			df := float64(baseline.Replicates - 1)
			terms = append(terms, errorTerm{source: "baseline", t: advantage / se, df: df})
			tCrit := studentTCrit(alpha, df)
			if !math.IsNaN(tCrit) {
				comparison.MinDetectableDelta = tCrit * se
			}
		}
	}
	if t, df, ok := welch(valuesA, valuesB); ok {
		// welch's t is signed (meanA - meanB); flip to the advantage sign
		if def.lowerBetter {
			t = -t
		}
		terms = append(terms, errorTerm{source: "welch", t: t, df: df})
	}
	if len(terms) == 0 && 0 < blockVarA && 0 < blockVarB && 2 < blocksA+blocksB {
		se := math.Sqrt(blockVarA/float64(len(valuesA)*len(valuesA)) + blockVarB/float64(len(valuesB)*len(valuesB)))
		if 0 < se {
			terms = append(terms, errorTerm{source: "blocks", t: advantage / se, df: float64(blocksA + blocksB - 2)})
			comparison.Optimistic = true
		}
	}
	if len(terms) == 0 {
		return comparison
	}

	sources := []string{}
	pA, pB := 0.0, 0.0
	for i, term := range terms {
		sources = append(sources, term.source)
		termPA := studentTSf(term.t, term.df)
		termPB := studentTSf(-term.t, term.df)
		if i == 0 || pA < termPA {
			pA = termPA
		}
		if i == 0 || pB < termPB {
			pB = termPB
		}
		// report the t/df of the first (preferred) term
		if i == 0 {
			comparison.T = term.t
			comparison.Df = term.df
		}
	}
	comparison.Testable = true
	comparison.PA = pA
	comparison.PB = pB
	comparison.Source = strings.Join(sources, "+")
	return comparison
}

// metricSide collects a metric's run-level values on one side, plus the sum
// of block-bootstrap variances and block counts (for the fallback error
// term).
func metricSide(name string, runs []*RunStats) ([]float64, float64, int) {
	values := []float64{}
	blockVarSum := 0.0
	blocks := 0
	blocksValid := true
	for _, run := range runs {
		summary, ok := run.Metrics[name]
		if !ok {
			continue
		}
		values = append(values, summary.Value)
		if 0 < summary.BlockSe {
			blockVarSum += summary.BlockSe * summary.BlockSe
			blocks += run.BlockCount
		} else {
			blocksValid = false
		}
	}
	if !blocksValid {
		// a run without a block SE invalidates the blocks fallback
		return values, 0, 0
	}
	return values, blockVarSum, blocks
}

// applyHolmAndVerdict fills the Holm-adjusted p-values over the primary set,
// the per-metric significance flags, and the overall verdict.
func applyHolmAndVerdict(result *CompareResult, alpha float64) {
	primaryIndexes := []int{}
	primaryPA := []float64{}
	primaryPB := []float64{}
	for i, comparison := range result.Metrics {
		if comparison.Primary && comparison.Testable {
			primaryIndexes = append(primaryIndexes, i)
			primaryPA = append(primaryPA, comparison.PA)
			primaryPB = append(primaryPB, comparison.PB)
		}
	}
	holmA := holmAdjust(primaryPA)
	holmB := holmAdjust(primaryPB)

	superiorA, superiorB := []string{}, []string{}
	for j, i := range primaryIndexes {
		comparison := &result.Metrics[i]
		comparison.PHolmA = holmA[j]
		comparison.PHolmB = holmB[j]
		if holmA[j] < alpha {
			comparison.SignificantlyBetterA = true
			superiorA = append(superiorA, comparison.Name)
		}
		if holmB[j] < alpha {
			comparison.SignificantlyBetterB = true
			superiorB = append(superiorB, comparison.Name)
		}
	}

	// guards: raw one-sided alpha against the *other* side winning
	worseA, worseB := []string{}, []string{}
	for i := range result.Metrics {
		comparison := &result.Metrics[i]
		if !comparison.Guard || !comparison.Testable {
			continue
		}
		if comparison.PB < alpha {
			if !comparison.SignificantlyBetterB {
				comparison.SignificantlyBetterB = true
			}
			worseA = append(worseA, comparison.Name)
		}
		if comparison.PA < alpha {
			if !comparison.SignificantlyBetterA {
				comparison.SignificantlyBetterA = true
			}
			worseB = append(worseB, comparison.Name)
		}
	}

	switch {
	case 0 < len(superiorA) && len(worseA) == 0:
		result.Verdict = "a_better"
		result.Reason = fmt.Sprintf("A significantly better on %s with no guard regression", strings.Join(superiorA, ", "))
	case 0 < len(superiorB) && len(worseB) == 0:
		result.Verdict = "b_better"
		result.Reason = fmt.Sprintf("B significantly better on %s with no guard regression", strings.Join(superiorB, ", "))
	case 0 < len(superiorA) || 0 < len(superiorB):
		result.Verdict = "mixed"
		result.Reason = fmt.Sprintf(
			"significant effects in both directions (A better: %s; B better: %s)",
			strings.Join(append(superiorA, worseB...), ", "),
			strings.Join(append(superiorB, worseA...), ", "),
		)
	default:
		result.Verdict = "indistinguishable"
		result.Reason = fmt.Sprintf("no primary metric difference exceeds the noise floor at p<%v", alpha)
	}
}

// ---- shared helpers ----

func consistentConfigSha(runs []*RunStats) (string, error) {
	sha := ""
	for _, run := range runs {
		if run.ConfigSha256 == "" {
			continue
		}
		if sha == "" {
			sha = run.ConfigSha256
		} else if sha != run.ConfigSha256 {
			return "", fmt.Errorf(
				"runs mix different providers.yml (sha %s vs %s)",
				shortSha(sha), shortSha(run.ConfigSha256),
			)
		}
	}
	return sha, nil
}

func establishedImbalance(a []*RunStats, b []*RunStats) string {
	mean := func(runs []*RunStats) float64 {
		sum, n := 0, 0
		for _, run := range runs {
			if 0 < run.ClientsEstablished {
				sum += run.ClientsEstablished
				n += 1
			}
		}
		if n == 0 {
			return 0
		}
		return float64(sum) / float64(n)
	}
	meanA, meanB := mean(a), mean(b)
	if meanA <= 0 || meanB <= 0 {
		return ""
	}
	ratio := meanA / meanB
	if ratio < 0.95 || 1.05 < ratio {
		return fmt.Sprintf(
			"warm client pools differ (A established %.0f, B %.0f): load per client differs, which confounds the comparison",
			meanA, meanB,
		)
	}
	return ""
}

func shortSha(sha string) string {
	if 12 < len(sha) {
		return sha[:12]
	}
	return sha
}

// ---- commands ----

func runAnalyze(opts docopt.Opts) {
	runPath, _ := opts.String("--run")
	windowOverride := parseWindowOpt(opts)
	stats, err := loadRunArtifact(runPath, windowOverride)
	if err != nil {
		fatalf("analyze: %s", err)
	}
	if outPath := optString(opts, "--out", ""); outPath != "" {
		if err := writeRunStats(outPath, stats); err != nil {
			fatalf("analyze: write %s: %s", outPath, err)
		}
		logf("wrote %s", outPath)
	}
	if optBool(opts, "--json") {
		printJson(stats)
		return
	}
	printRunStats(stats)
}

func runCompare(opts docopt.Opts) {
	alpha := optFloat(opts, "--p", 0.05)
	windowOverride := parseWindowOpt(opts)

	loadSide := func(flag string) []*RunStats {
		pathsOpt, _ := opts.String(flag)
		runs := []*RunStats{}
		for _, path := range splitPaths(pathsOpt) {
			stats, err := loadRunArtifact(path, windowOverride)
			if err != nil {
				fatalf("compare: %s", err)
			}
			runs = append(runs, stats)
		}
		return runs
	}
	a := loadSide("--a")
	b := loadSide("--b")

	var baseline *Baseline
	if baselinePath := optString(opts, "--baseline", ""); baselinePath != "" {
		loaded, err := readBaseline(baselinePath)
		if err != nil {
			fatalf("compare: %s", err)
		}
		baseline = loaded
	}

	result, err := CompareRuns(a, b, baseline, alpha)
	if err != nil {
		fatalf("compare: %s", err)
	}
	if optBool(opts, "--json") {
		printJson(result)
		return
	}
	printCompareResult(result, baseline)
}

// ---- output ----

func printJson(v any) {
	jsonBytes, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatalf("marshal: %s", err)
	}
	fmt.Println(string(jsonBytes))
}

func printRunStats(stats *RunStats) {
	fmt.Printf("run %s\n", stats.Label)
	fmt.Printf("  window %.0fs  rows %d (%d in window)  failures %d\n",
		stats.windowSeconds(), stats.Rows, stats.RowsInWindow, stats.Failures)
	if 0 < stats.ClientsPool {
		fmt.Printf("  clients %d/%d established", stats.ClientsEstablished, stats.ClientsPool)
		if stats.ConfigSha256 != "" {
			fmt.Printf("  config %s", shortSha(stats.ConfigSha256))
		}
		fmt.Printf("\n")
	}
	fmt.Printf("  %-30s %14s %10s %12s\n", "metric", "value", "n", "block_se")
	for _, def := range metricDefs() {
		summary, ok := stats.Metrics[def.name]
		if !ok {
			continue
		}
		fmt.Printf("  %-30s %14s %10d %12s\n",
			def.name, formatValue(summary.Value), summary.N, formatValue(summary.BlockSe))
	}
}

func printCompareResult(result *CompareResult, baseline *Baseline) {
	baselineLabel := "none (within-run fallback)"
	if baseline != nil {
		baselineLabel = fmt.Sprintf("%d replicates", baseline.Replicates)
	}
	fmt.Printf("A: %s\nB: %s\nalpha %v  baseline %s\n\n",
		strings.Join(result.RunsA, ", "), strings.Join(result.RunsB, ", "), result.Alpha, baselineLabel)
	fmt.Printf("%-32s %14s %14s %9s %10s %10s %7s\n",
		"metric", "A", "B", "Δ%", "p(A wins)", "p(B wins)", "src")
	for _, comparison := range result.Metrics {
		name := comparison.Name
		if comparison.Primary {
			name += " *"
		} else if comparison.Guard {
			name += " †"
		}
		pa, pb := "-", "-"
		if comparison.Testable {
			pa = formatP(comparison.PA)
			pb = formatP(comparison.PB)
		}
		fmt.Printf("%-32s %14s %14s %8.1f%% %10s %10s %7s\n",
			name, formatValue(comparison.A), formatValue(comparison.B),
			comparison.DeltaPct, pa, pb, comparison.Source)
	}
	fmt.Printf("\n* primary (Holm-corrected verdict)   † guard (non-inferiority)\n")
	fmt.Printf("verdict: %s — %s\n", result.Verdict, result.Reason)
	for _, warning := range result.Warnings {
		fmt.Printf("warning: %s\n", warning)
	}
}

func formatValue(v float64) string {
	if v == 0 {
		return "0"
	}
	abs := math.Abs(v)
	switch {
	case abs < 0.001 || 1e9 <= abs:
		return fmt.Sprintf("%.3e", v)
	case abs < 1:
		return fmt.Sprintf("%.4f", v)
	case abs < 1000:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func formatP(p float64) string {
	if p < 0.0001 {
		return "<0.0001"
	}
	return fmt.Sprintf("%.4f", p)
}

func splitPaths(pathsOpt string) []string {
	paths := []string{}
	for _, path := range strings.Split(pathsOpt, ",") {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			paths = append(paths, trimmed)
		}
	}
	return paths
}

func parseWindowOpt(opts docopt.Opts) *measureWindow {
	windowOpt := optString(opts, "--window", "")
	if windowOpt == "" {
		return nil
	}
	window, err := parseMeasureWindow(windowOpt)
	if err != nil {
		fatalf("--window: %s", err)
	}
	return window
}

func optFloat(opts docopt.Opts, key string, fallback float64) float64 {
	s, err := opts.String(key)
	if err != nil || s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return v
}
