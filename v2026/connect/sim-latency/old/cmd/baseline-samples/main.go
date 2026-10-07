package main

// baseline-samples independently audits the FindProviders2 stream belonging
// to each completed sim-latency run. It is a post-campaign tool so compiling it
// cannot perturb the baseline being measured.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urnetwork/server/v2026/stats"
	"github.com/urnetwork/server/v2026/stats/sample"
)

const (
	outputSchema = 1
	outputKind   = "sim-latency-baseline-samples"
)

type runManifest struct {
	EvaluationId   string `json:"evaluation_id"`
	StatsRoot      string `json:"stats_root"`
	MeasureStartMs int64  `json:"measure_start_ms"`
	MeasureEndMs   int64  `json:"measure_end_ms"`
}

type runAudit struct {
	EvaluationId       string       `json:"evaluation_id"`
	StatsRoot          string       `json:"stats_root"`
	Samples            int64        `json:"samples"`
	FirstSampleMs      int64        `json:"first_sample_ms"`
	LastSampleMs       int64        `json:"last_sample_ms"`
	SampleSpanFraction float64      `json:"sample_span_fraction"`
	EmptyPools         int64        `json:"empty_pools"`
	LoadP95Ms          float64      `json:"load_p95_ms"`
	Report             stats.Report `json:"report"`
}

type output struct {
	Schema int                 `json:"schema"`
	Kind   string              `json:"kind"`
	Runs   map[string]runAudit `json:"runs"`
}

func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	h := q * float64(len(values)-1)
	lower := int(math.Floor(h))
	upper := int(math.Ceil(h))
	if lower == upper {
		return values[lower]
	}
	return values[lower] + (h-float64(lower))*(values[upper]-values[lower])
}

func tagForManifest(path string) string {
	name := filepath.Base(path)
	return strings.TrimSuffix(name, ".run.json")
}

func readManifest(path string) (*runManifest, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest runManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, err
	}
	if manifest.EvaluationId == "" || manifest.StatsRoot == "" ||
		manifest.MeasureEndMs <= manifest.MeasureStartMs {
		return nil, fmt.Errorf("manifest is missing its sample identity or measured window")
	}
	return &manifest, nil
}

func audit(path string) (runAudit, error) {
	manifest, err := readManifest(path)
	if err != nil {
		return runAudit{}, fmt.Errorf("%s: %w", path, err)
	}
	streamDir := filepath.Join(manifest.StatsRoot, "findproviders2")
	metrics := stats.NewMetrics()
	loads := []float64{}
	emptyPools := int64(0)
	firstSampleMs := int64(0)
	lastSampleMs := int64(0)
	err = stats.LoadStreamTyped(
		streamDir,
		func() *sample.FindProviders2Sample { return &sample.FindProviders2Sample{} },
		func(value *sample.FindProviders2Sample) error {
			if value == nil || uint64(math.MaxInt64) < value.TimeUnixMilli {
				return fmt.Errorf("invalid sample or timestamp")
			}
			timestamp := int64(value.TimeUnixMilli)
			if timestamp < manifest.MeasureStartMs || manifest.MeasureEndMs <= timestamp {
				return nil
			}
			loadMillis := float64(value.LoadMillis)
			if math.IsNaN(loadMillis) || math.IsInf(loadMillis, 0) || loadMillis < 0 {
				return fmt.Errorf("invalid load_millis")
			}
			for _, candidate := range value.Candidates {
				if candidate == nil {
					return fmt.Errorf("nil candidate")
				}
			}
			if value.PoolCount <= 0 || len(value.Candidates) == 0 {
				emptyPools += 1
			}
			if len(loads) == 0 || timestamp < firstSampleMs {
				firstSampleMs = timestamp
			}
			if len(loads) == 0 || lastSampleMs < timestamp {
				lastSampleMs = timestamp
			}
			loads = append(loads, loadMillis)
			metrics.Add(value)
			return nil
		},
	)
	if err != nil {
		return runAudit{}, fmt.Errorf("%s: %w", streamDir, err)
	}
	report := metrics.Report()
	if report.Samples == 0 {
		return runAudit{}, fmt.Errorf("%s: no in-window samples", streamDir)
	}
	return runAudit{
		EvaluationId:       manifest.EvaluationId,
		StatsRoot:          manifest.StatsRoot,
		Samples:            report.Samples,
		FirstSampleMs:      firstSampleMs,
		LastSampleMs:       lastSampleMs,
		SampleSpanFraction: float64(lastSampleMs-firstSampleMs) / float64(manifest.MeasureEndMs-manifest.MeasureStartMs),
		EmptyPools:         emptyPools,
		LoadP95Ms:          quantile(loads, 0.95),
		Report:             report,
	}, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: baseline-samples <run.json> [run.json ...]")
		os.Exit(2)
	}
	result := output{
		Schema: outputSchema,
		Kind:   outputKind,
		Runs:   map[string]runAudit{},
	}
	for _, path := range os.Args[1:] {
		tag := tagForManifest(path)
		if tag == "" {
			fmt.Fprintf(os.Stderr, "%s: empty run tag\n", path)
			os.Exit(1)
		}
		if _, exists := result.Runs[tag]; exists {
			fmt.Fprintf(os.Stderr, "%s: duplicate run tag %s\n", path, tag)
			os.Exit(1)
		}
		run, err := audit(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		result.Runs[tag] = run
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(&result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
