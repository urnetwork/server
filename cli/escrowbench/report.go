package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// One measurement, as server/model's escrow benchmark writes it: one run of admission or settlement in one
// state of escrow_draw_order.
type Result struct {
	Name            string  `json:"name"`
	DrawOrder       bool    `json:"draw_order"`
	Count           int     `json:"count"`
	FailedCount     int     `json:"failed_count"`
	P50Ns           int64   `json:"p50_ns"`
	P99Ns           int64   `json:"p99_ns"`
	PerSecond       float64 `json:"per_second"`
	TargetPerSecond float64 `json:"target_per_second"`
	PgAcquiresPerOp float64 `json:"pg_acquires_per_op"`
	GrantReadsPerOp float64 `json:"grant_reads_per_op"`
}

// One build's measurements.
type Measurements struct {
	Label   string    `json:"label"`
	Time    time.Time `json:"time"`
	Results []*Result `json:"results"`
}

const (
	ResultAdmission  = "admission"
	ResultSettlement = "settlement"
)

// The result named name in the gate state drawOrder, nil when the file has none.
func (self *Measurements) Result(name string, drawOrder bool) *Result {
	for _, result := range self.Results {
		if result.Name == name && result.DrawOrder == drawOrder {
			return result
		}
	}
	return nil
}

func ReadMeasurements(path string) (*Measurements, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	measurements := &Measurements{}
	if err := json.Unmarshal(content, measurements); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(measurements.Results) == 0 {
		return nil, fmt.Errorf("%s: no results", path)
	}
	return measurements, nil
}

// §20.9's pass rule: within 5% of main's baseline and no added Postgres round trip per admission.
type CompareSettings struct {
	// How far p99 admission latency may rise above the baseline's, as a fraction of it.
	LatencyTolerance float64
	// How far settlement throughput may fall below the baseline's, as a fraction of it.
	ThroughputTolerance float64
	// How far a round-trip count an operation may rise before it is an added round trip. The pool's
	// acquires take in whatever else the process read during a run, so the count is not exact.
	RoundTripTolerance float64
	// The fewest operations a result needs for its p99 to mean anything.
	MinCount int
}

func DefaultCompareSettings() *CompareSettings {
	return &CompareSettings{
		LatencyTolerance:    0.05,
		ThroughputTolerance: 0.05,
		RoundTripTolerance:  0.05,
		MinCount:            1000,
	}
}

// One line of a verdict.
type Check struct {
	// what was compared, and against which file
	What     string
	Against  string
	Baseline float64
	Measured float64
	// the bound Measured had to keep
	Bound float64
	Unit  string
	Pass  bool
	// why it failed, or what a reader should know of a pass
	Note string
}

// What a comparison found.
type Verdict struct {
	Mode   string
	Pass   bool
	Checks []*Check
	// each result's failed attempts and pace, reported beside the percentiles
	Notes []string
}

const (
	// M6's entry: the dark build against main's baseline (D-PHASE-7).
	ModeEntry = "entry"
	// M6's exit: the rerun, against main's baseline and against the entry run.
	ModeExit = "exit"
)

var ErrMissingResult = errors.New("a measurements file lacks a result the comparison needs")

// Compares candidate's results with the gate on against against's results with the gate off, the order
// main runs: p99 admission latency, settlement throughput, and admission's round trips.
func compareAgainst(verdict *Verdict, againstLabel string, against *Measurements, candidate *Measurements, settings *CompareSettings) error {
	baseAdmission := against.Result(ResultAdmission, false)
	baseSettlement := against.Result(ResultSettlement, false)
	admission := candidate.Result(ResultAdmission, true)
	settlement := candidate.Result(ResultSettlement, true)
	if baseAdmission == nil || baseSettlement == nil {
		return fmt.Errorf("%w: %s needs admission and settlement with draw_order off", ErrMissingResult, againstLabel)
	}
	if admission == nil || settlement == nil {
		return fmt.Errorf("%w: the candidate needs admission and settlement with draw_order on", ErrMissingResult)
	}
	add := func(check *Check) {
		check.Against = againstLabel
		if !check.Pass {
			verdict.Pass = false
		}
		verdict.Checks = append(verdict.Checks, check)
	}
	latencyBound := float64(baseAdmission.P99Ns) * (1 + settings.LatencyTolerance)
	add(&Check{
		What:     "p99 admission latency",
		Baseline: float64(baseAdmission.P99Ns),
		Measured: float64(admission.P99Ns),
		Bound:    latencyBound,
		Unit:     "ns",
		Pass:     float64(admission.P99Ns) <= latencyBound,
	})
	throughputBound := baseSettlement.PerSecond * (1 - settings.ThroughputTolerance)
	add(&Check{
		What:     "settlement throughput",
		Baseline: baseSettlement.PerSecond,
		Measured: settlement.PerSecond,
		Bound:    throughputBound,
		Unit:     "/s",
		Pass:     throughputBound <= settlement.PerSecond,
	})
	// the seam's count is exact: any more is an added round trip
	add(&Check{
		What:     "grant reads per admission",
		Baseline: baseAdmission.GrantReadsPerOp,
		Measured: admission.GrantReadsPerOp,
		Bound:    baseAdmission.GrantReadsPerOp,
		Unit:     "",
		Pass:     admission.GrantReadsPerOp <= baseAdmission.GrantReadsPerOp,
	})
	acquireBound := baseAdmission.PgAcquiresPerOp + settings.RoundTripTolerance
	add(&Check{
		What:     "Postgres acquires per admission",
		Baseline: baseAdmission.PgAcquiresPerOp,
		Measured: admission.PgAcquiresPerOp,
		Bound:    acquireBound,
		Unit:     "",
		Pass:     admission.PgAcquiresPerOp <= acquireBound,
	})
	return nil
}

// A result that cannot carry a verdict fails it: too few operations for a p99, or failed attempts, which
// are not in the percentiles.
func checkResults(verdict *Verdict, label string, measurements *Measurements, settings *CompareSettings) {
	for _, result := range measurements.Results {
		gate := "off"
		if result.DrawOrder {
			gate = "on"
		}
		pace := "unpaced"
		if 0 < result.TargetPerSecond {
			pace = fmt.Sprintf("paced at %.1f/s", result.TargetPerSecond)
		}
		verdict.Notes = append(verdict.Notes, fmt.Sprintf("%s, %s, draw_order %s: %d passed and %d failed, %s, p50 %s, p99 %s, %.1f/s",
			label, result.Name, gate, result.Count, result.FailedCount, pace,
			time.Duration(result.P50Ns), time.Duration(result.P99Ns), result.PerSecond))
		if result.Count < settings.MinCount {
			verdict.Pass = false
			verdict.Checks = append(verdict.Checks, &Check{
				What:     fmt.Sprintf("%s operations measured, draw_order %s", result.Name, gate),
				Against:  label,
				Measured: float64(result.Count),
				Bound:    float64(settings.MinCount),
				Note:     "too few operations for a p99",
			})
		}
		if 0 < result.FailedCount {
			verdict.Pass = false
			verdict.Checks = append(verdict.Checks, &Check{
				What:     fmt.Sprintf("%s failed attempts, draw_order %s", result.Name, gate),
				Against:  label,
				Measured: float64(result.FailedCount),
				Note:     "failed attempts are outside the percentiles",
			})
		}
	}
}

// M6's entry: candidate, the dark build, against baseline, main's run. The dark build with the gate off is
// main's order, so it is held to the baseline too.
func CompareEntry(baseline *Measurements, candidate *Measurements, settings *CompareSettings) (*Verdict, error) {
	verdict := &Verdict{Mode: ModeEntry, Pass: true}
	checkResults(verdict, "baseline", baseline, settings)
	checkResults(verdict, "candidate", candidate, settings)
	if err := compareAgainst(verdict, "baseline", baseline, candidate, settings); err != nil {
		return nil, err
	}
	return verdict, nil
}

// M6's exit: candidate, the rerun, against baseline, main's run, and against entry, the run M6's entry
// passed on, read with its gate on as the order then frozen.
func CompareExit(baseline *Measurements, entry *Measurements, candidate *Measurements, settings *CompareSettings) (*Verdict, error) {
	verdict := &Verdict{Mode: ModeExit, Pass: true}
	checkResults(verdict, "baseline", baseline, settings)
	checkResults(verdict, "candidate", candidate, settings)
	if err := compareAgainst(verdict, "baseline", baseline, candidate, settings); err != nil {
		return nil, err
	}
	entryAdmission := entry.Result(ResultAdmission, true)
	entrySettlement := entry.Result(ResultSettlement, true)
	if entryAdmission == nil || entrySettlement == nil {
		return nil, fmt.Errorf("%w: the entry run needs admission and settlement with draw_order on", ErrMissingResult)
	}
	// the entry run's gate-on results stand where a baseline's gate-off ones do
	frozen := &Measurements{Label: entry.Label, Results: []*Result{}}
	for _, result := range []*Result{entryAdmission, entrySettlement} {
		copied := *result
		copied.DrawOrder = false
		frozen.Results = append(frozen.Results, &copied)
	}
	if err := compareAgainst(verdict, "entry", frozen, candidate, settings); err != nil {
		return nil, err
	}
	return verdict, nil
}

// The report an operator attaches to M6's entry or exit review.
func WriteReport(out io.Writer, verdict *Verdict, labels map[string]string, now time.Time) error {
	lines := []string{}
	result := "FAIL"
	if verdict.Pass {
		result = "PASS"
	}
	lines = append(lines, fmt.Sprintf("escrow benchmark, M6 %s: %s", verdict.Mode, result))
	lines = append(lines, fmt.Sprintf("written %s", now.UTC().Format(time.RFC3339)))
	for _, name := range []string{"baseline", "entry", "candidate"} {
		if label, ok := labels[name]; ok {
			lines = append(lines, fmt.Sprintf("%s: %s", name, label))
		}
	}
	lines = append(lines, "")
	for _, check := range verdict.Checks {
		mark := "pass"
		if !check.Pass {
			mark = "FAIL"
		}
		line := fmt.Sprintf("%s  %s against %s: %.2f%s, %s's %.2f%s, bound %.2f%s", mark, check.What, check.Against, check.Measured, check.Unit, check.Against, check.Baseline, check.Unit, check.Bound, check.Unit)
		if check.Note != "" {
			line += " (" + check.Note + ")"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "")
	lines = append(lines, verdict.Notes...)
	_, err := io.WriteString(out, strings.Join(lines, "\n")+"\n")
	return err
}
