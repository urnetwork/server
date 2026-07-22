package main

// `sim-latency baseline`: measure the environment noise floor.
//
// The workload is fully seeded by providers.yml, so the spread across
// replicate runs of unchanged code is pure environment noise — the error
// term `compare` tests observed A-vs-B differences against. Two modes:
//
//	baseline --runs a.csv,b.csv,...     compute from existing run artifacts
//	baseline --replicates n ...         orchestrate n sequential
//	                                    `run --reset` subprocesses first
//
// The output (baseline.json) encapsulates, per metric:
//
//   - the between-run mean/sd/cv over the k replicates — the noise floor
//   - sd_rel_error (~1/sqrt(2(k-1))): how uncertain the sd estimate itself
//     is at this k
//   - sd_by_replicates: the sd estimated from the first 2..k replicates —
//     whether the estimate has converged or more replicates are needed
//   - min_detectable_delta_by_runs_per_side: the smallest one-sided
//     significant |delta| at alpha for a future comparison with m = 1..k
//     runs per side (shrinks as sqrt(1/m)) — the convergence rate of the
//     measurement itself
//
// `compare --baseline baseline.json` uses the sd (with df = k-1) as the
// significance error term.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
)

const baselineSchema = 1
const baselineKind = "sim-latency-baseline"

// BaselineMetric is the between-run variability of one metric measured over
// baseline replicates, with its convergence diagnostics.
type BaselineMetric struct {
	Mean float64 `json:"mean"`
	Sd   float64 `json:"sd"`
	// sd / |mean|, the relative noise floor
	Cv float64 `json:"cv,omitempty"`
	// ~1/sqrt(2(k-1)): the relative sampling uncertainty of Sd itself
	SdRelError float64 `json:"sd_rel_error,omitempty"`
	// smallest one-sided-significant |delta| at Alpha with one run per side
	MinDetectableDelta float64 `json:"min_detectable_delta,omitempty"`
	// convergence of the estimate: sd from the first m replicates, m = 2..k
	SdByReplicates []float64 `json:"sd_by_replicates,omitempty"`
	// convergence of the measurement: min detectable delta with m runs per
	// side (final sd, df = k-1), m = 1..k
	MinDetectableDeltaByRunsPerSide []float64 `json:"min_detectable_delta_by_runs_per_side,omitempty"`
}

// Baseline is baseline.json: the measured noise floor of one
// (config, duration, machine) environment, from k replicate runs of
// unchanged code, used by `compare` to measure significance.
type Baseline struct {
	Schema     int     `json:"schema"`
	Kind       string  `json:"kind"`
	Alpha      float64 `json:"alpha"`
	Replicates int     `json:"replicates"`

	// fingerprint: comparisons using this noise floor should match these
	ConfigSha256 string   `json:"config_sha256,omitempty"`
	DurationS    float64  `json:"duration_s,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	Runs         []string `json:"runs,omitempty"`

	Metrics map[string]BaselineMetric `json:"metrics"`
}

// computeBaseline derives the noise floor and its convergence diagnostics
// from replicate runs of identical code + config.
func computeBaseline(runs []*RunStats, alpha float64) (*Baseline, []string, error) {
	warnings := []string{}
	k := len(runs)
	if k < 3 {
		return nil, nil, fmt.Errorf("need at least 3 replicate runs to estimate variance (got %d); 5+ recommended", k)
	}
	if k < 5 {
		warnings = append(warnings, fmt.Sprintf("only %d replicates: the variance estimate is coarse (df=%d); 5+ recommended", k, k-1))
	}
	if sha, err := consistentConfigSha(runs); err != nil {
		return nil, nil, err
	} else if sha == "" {
		warnings = append(warnings, "replicates carry no config sha; cannot verify they ran the same providers.yml")
	}

	durations := []float64{}
	for _, run := range runs {
		durations = append(durations, run.windowSeconds())
	}
	minDuration, maxDuration := durations[0], durations[0]
	for _, d := range durations {
		minDuration = math.Min(minDuration, d)
		maxDuration = math.Max(maxDuration, d)
	}
	if 0 < minDuration && 1.1 < maxDuration/minDuration {
		warnings = append(warnings, fmt.Sprintf("replicate window durations differ (%.0fs..%.0fs); variance depends on duration", minDuration, maxDuration))
	}

	baseline := &Baseline{
		Schema:       baselineSchema,
		Kind:         baselineKind,
		Alpha:        alpha,
		Replicates:   k,
		ConfigSha256: runs[0].ConfigSha256,
		DurationS:    durations[0],
		Hostname:     runs[0].Hostname,
		Metrics:      map[string]BaselineMetric{},
	}
	for _, run := range runs {
		baseline.Runs = append(baseline.Runs, run.Label)
	}

	underpowered := []string{}
	for _, def := range metricDefs() {
		values := []float64{}
		blockSes := []float64{}
		for _, run := range runs {
			if summary, ok := run.Metrics[def.name]; ok {
				values = append(values, summary.Value)
				if 0 < summary.BlockSe {
					blockSes = append(blockSes, summary.BlockSe)
				}
			}
		}
		if len(values) < 3 {
			warnings = append(warnings, fmt.Sprintf("%s: present in only %d/%d replicates; skipped", def.name, len(values), k))
			continue
		}
		if len(values) < k {
			warnings = append(warnings, fmt.Sprintf("%s: present in only %d/%d replicates", def.name, len(values), k))
		}
		mean, sd := meanStd(values)
		metric := BaselineMetric{Mean: mean, Sd: sd}
		if mean != 0 {
			metric.Cv = sd / math.Abs(mean)
		}
		metric.SdRelError = 1 / math.Sqrt(2*float64(len(values)-1))
		// estimate stability: the sd this baseline would have reported had it
		// stopped after the first m replicates
		for m := 2; m <= len(values); m += 1 {
			_, sdM := meanStd(values[:m])
			metric.SdByReplicates = append(metric.SdByReplicates, sdM)
		}
		// measurement convergence: with the final noise floor, what a
		// comparison with m runs per side can detect (sqrt(1/m) scaling)
		tCrit := studentTCrit(alpha, float64(len(values)-1))
		if !math.IsNaN(tCrit) {
			metric.MinDetectableDelta = tCrit * sd * math.Sqrt2
			for m := 1; m <= len(values); m += 1 {
				metric.MinDetectableDeltaByRunsPerSide = append(
					metric.MinDetectableDeltaByRunsPerSide,
					tCrit*sd*math.Sqrt(2/float64(m)),
				)
			}
		}
		baseline.Metrics[def.name] = metric

		// if the within-run SE exceeds the between-run sd, the replicates
		// likely got lucky and the noise floor is understated
		if 0 < len(blockSes) {
			meanBlockSe, _ := meanStd(blockSes)
			if sd < meanBlockSe {
				underpowered = append(underpowered, def.name)
			}
		}
	}
	if 0 < len(underpowered) {
		warnings = append(warnings, fmt.Sprintf(
			"between-run sd is below the within-run block SE for %s: the noise floor is likely understated; add replicates",
			strings.Join(underpowered, ", "),
		))
	}
	return baseline, warnings, nil
}

func writeBaseline(path string, baseline *Baseline) error {
	baselineBytes, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(baselineBytes, '\n'), 0o644)
}

func readBaseline(path string) (*Baseline, error) {
	baselineBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var baseline Baseline
	if err := json.Unmarshal(baselineBytes, &baseline); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if baseline.Kind != baselineKind {
		return nil, fmt.Errorf("%s: kind %q is not a %s (write it with `sim-latency baseline`)", path, baseline.Kind, baselineKind)
	}
	return &baseline, nil
}

// ---- replicate orchestration ----

// replicateRunArgs builds the argv for one baseline replicate subprocess:
// always `run --reset` (independent replicates) with the replicate's meta
// path, forwarding the run-shaping flags the user gave the baseline command.
func replicateRunArgs(opts docopt.Opts, metaPath string) []string {
	args := []string{"run", "--reset", "--meta", metaPath}
	forward := []string{
		"--providers", "--site-home",
		"--ramp", "--prewarm", "--settle", "--duration",
		"--fleet-shards", "--site-listen", "--hosts", "--api-port",
		"--pipeline-interval", "--test-timeout", "--announce-timeout",
	}
	for _, flag := range forward {
		if value := optString(opts, flag, ""); value != "" {
			args = append(args, flag+"="+value)
		}
	}
	if optBool(opts, "--no-impair") {
		args = append(args, "--no-impair")
	}
	return args
}

// runBaselineReplicates runs n sequential `sim-latency run --reset`
// subprocesses, writing baseline-<i>.csv + baseline-<i>.run.json into dir,
// and returns the csv paths. Sequential on purpose: replicates share the
// backing stores and the machine, and the noise floor being measured is the
// per-run environment, not co-scheduling artifacts.
func runBaselineReplicates(opts docopt.Opts, n int, dir string) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ctx, cancel := signalContext()
	defer cancel()

	paths := []string{}
	for i := 1; i <= n; i += 1 {
		csvPath := filepath.Join(dir, fmt.Sprintf("baseline-%d.csv", i))
		metaPath := filepath.Join(dir, fmt.Sprintf("baseline-%d.run.json", i))
		args := replicateRunArgs(opts, metaPath)
		logf("baseline replicate %d/%d: %s %s", i, n, filepath.Base(self), strings.Join(args, " "))

		out, err := os.Create(csvPath)
		if err != nil {
			return paths, err
		}
		cmd := exec.CommandContext(ctx, self, args...)
		cmd.Env = os.Environ()
		cmd.Stdout = out
		cmd.Stderr = os.Stderr
		// let the run drain on interrupt instead of a hard kill
		cmd.Cancel = func() error {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		cmd.WaitDelay = 15 * time.Second
		runErr := cmd.Run()
		out.Close()
		if runErr != nil {
			return paths, fmt.Errorf("replicate %d: %w", i, runErr)
		}
		if ctx.Err() != nil {
			return paths, ctx.Err()
		}
		paths = append(paths, csvPath)
	}
	return paths, nil
}

// ---- command ----

func runBaselineCmd(opts docopt.Opts) {
	alpha := optFloat(opts, "--alpha", 0.05)
	outPath := optString(opts, "--out", "baseline.json")

	paths := splitPaths(optString(opts, "--runs", ""))
	if len(paths) == 0 {
		replicates := optInt(opts, "--replicates", 5)
		dir := optString(opts, "--out-dir", "baseline-runs")
		measured, err := runBaselineReplicates(opts, replicates, dir)
		if err != nil {
			fatalf("baseline: %s", err)
		}
		paths = measured
	}

	runs := []*RunStats{}
	for _, path := range paths {
		stats, err := loadRunArtifact(path, nil)
		if err != nil {
			fatalf("baseline: %s", err)
		}
		runs = append(runs, stats)
	}
	baseline, warnings, err := computeBaseline(runs, alpha)
	if err != nil {
		fatalf("baseline: %s", err)
	}
	for _, warning := range warnings {
		logf("baseline: warning: %s", warning)
	}
	if err := writeBaseline(outPath, baseline); err != nil {
		fatalf("baseline: write %s: %s", outPath, err)
	}
	logf("wrote %s", outPath)
	printBaseline(baseline)
}

// ---- output ----

// printBaseline prints the noise floor plus the two convergence views (for
// the primary metrics; every metric is in the json).
func printBaseline(baseline *Baseline) {
	fmt.Printf("baseline noise floor: %d replicates, alpha %v\n", baseline.Replicates, baseline.Alpha)
	fmt.Printf("  %-30s %14s %14s %8s %8s %16s\n", "metric", "mean", "sd", "cv", "sd±", "min Δ (1/side)")
	for _, def := range metricDefs() {
		metric, ok := baseline.Metrics[def.name]
		if !ok {
			continue
		}
		fmt.Printf("  %-30s %14s %14s %7.1f%% %7.0f%% %16s\n",
			def.name, formatValue(metric.Mean), formatValue(metric.Sd),
			metric.Cv*100, metric.SdRelError*100, formatValue(metric.MinDetectableDelta))
	}

	const maxColumns = 8
	printIndexRow := func(offset int, count int) {
		fmt.Printf("  %-30s", "m")
		for i := 0; i < count; i += 1 {
			if maxColumns <= i {
				fmt.Printf("  ...")
				break
			}
			fmt.Printf(" %10d", offset+i)
		}
		fmt.Printf("\n")
	}
	printValueRow := func(label string, values []float64) {
		fmt.Printf("  %-30s", label)
		for i, v := range values {
			if maxColumns <= i {
				fmt.Printf("  ...")
				break
			}
			fmt.Printf(" %10s", formatValue(v))
		}
		fmt.Printf("\n")
	}

	fmt.Printf("\nmeasurement convergence — min detectable Δ with m runs per side (primaries):\n")
	for _, def := range metricDefs() {
		metric, ok := baseline.Metrics[def.name]
		if !ok || !def.primary || len(metric.MinDetectableDeltaByRunsPerSide) == 0 {
			continue
		}
		printIndexRow(1, len(metric.MinDetectableDeltaByRunsPerSide))
		printValueRow(def.name, metric.MinDetectableDeltaByRunsPerSide)
	}

	fmt.Printf("\nestimate stability — sd from the first m replicates (primaries):\n")
	for _, def := range metricDefs() {
		metric, ok := baseline.Metrics[def.name]
		if !ok || !def.primary || len(metric.SdByReplicates) == 0 {
			continue
		}
		printIndexRow(2, len(metric.SdByReplicates))
		printValueRow(def.name, metric.SdByReplicates)
	}
	fmt.Printf("\nthe sd estimate itself carries ~±%.0f%% sampling uncertainty at k=%d; if the\nstability series still drifts, add replicates before trusting tight verdicts\n",
		100/math.Sqrt(2*float64(baseline.Replicates-1)), baseline.Replicates)
}
