// Judges §22.8's escrow benchmark for M6 (§20.9's gate, owner decision 92, D-PHASE-7): admission and
// settlement in the draw order, with p99 admission latency and settlement throughput within 5% of main's
// baseline and no added Postgres round trip per admission. It reads the measurements server/model's
// TestEscrowBenchmarkMeasurements writes with ESCROWBENCH_OUT set, one file a build, each run on the same host
// at the same pace (twice the Y1 rates, given as ESCROWBENCH_ADMISSIONS_PER_S and
// ESCROWBENCH_SETTLEMENTS_PER_S), and writes the report an operator attaches to the review:
//
//	go run ./cli/escrowbench entry -baseline main.json -candidate dark.json -out entry-report.txt
//	go run ./cli/escrowbench exit -baseline main.json -entry dark.json -candidate m6.json -out exit-report.txt
//
// entry holds the dark build, gate on, to main's run. exit holds the rerun to main's run and to the entry
// run's gate-on results, the order then frozen. It exits 1 when the benchmark fails, 2 when it cannot judge.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

var errUsage = errors.New("usage: escrowbench entry|exit -baseline <file> [-entry <file>] -candidate <file> [-out <file>]")

// Runs one mode over args, writing the report to the -out file, or to stdout without one. pass is the
// benchmark's verdict.
func run(args []string, stdout io.Writer, now time.Time) (pass bool, returnErr error) {
	if len(args) == 0 {
		return false, errUsage
	}
	mode := args[0]
	if mode != ModeEntry && mode != ModeExit {
		return false, errUsage
	}
	defaults := DefaultCompareSettings()
	flags := flag.NewFlagSet("escrowbench "+mode, flag.ContinueOnError)
	baselinePath := flags.String("baseline", "", "main's measurements")
	entryPath := flags.String("entry", "", "the entry run's measurements (exit)")
	candidatePath := flags.String("candidate", "", "the measurements under judgment")
	outPath := flags.String("out", "", "where the report goes; stdout when empty")
	settings := &CompareSettings{}
	flags.Float64Var(&settings.LatencyTolerance, "latency_tolerance", defaults.LatencyTolerance, "how far p99 admission latency may rise, as a fraction")
	flags.Float64Var(&settings.ThroughputTolerance, "throughput_tolerance", defaults.ThroughputTolerance, "how far settlement throughput may fall, as a fraction")
	flags.Float64Var(&settings.RoundTripTolerance, "round_trip_tolerance", defaults.RoundTripTolerance, "how far pool acquires an admission may rise before a round trip counts as added")
	flags.IntVar(&settings.MinCount, "min_count", defaults.MinCount, "the fewest operations a result needs")
	if err := flags.Parse(args[1:]); err != nil {
		return false, err
	}
	if *baselinePath == "" || *candidatePath == "" || (mode == ModeExit) != (*entryPath != "") {
		return false, errUsage
	}
	baseline, err := ReadMeasurements(*baselinePath)
	if err != nil {
		return false, err
	}
	candidate, err := ReadMeasurements(*candidatePath)
	if err != nil {
		return false, err
	}
	labels := map[string]string{
		"baseline":  fmt.Sprintf("%s (%s, measured %s)", *baselinePath, baseline.Label, baseline.Time.UTC().Format(time.RFC3339)),
		"candidate": fmt.Sprintf("%s (%s, measured %s)", *candidatePath, candidate.Label, candidate.Time.UTC().Format(time.RFC3339)),
	}
	var verdict *Verdict
	if mode == ModeEntry {
		verdict, err = CompareEntry(baseline, candidate, settings)
	} else {
		var entry *Measurements
		entry, err = ReadMeasurements(*entryPath)
		if err != nil {
			return false, err
		}
		labels["entry"] = fmt.Sprintf("%s (%s, measured %s)", *entryPath, entry.Label, entry.Time.UTC().Format(time.RFC3339))
		verdict, err = CompareExit(baseline, entry, candidate, settings)
	}
	if err != nil {
		return false, err
	}
	out := stdout
	if *outPath != "" {
		file, err := os.Create(*outPath)
		if err != nil {
			return false, err
		}
		defer func() {
			if err := file.Close(); err != nil && returnErr == nil {
				returnErr = err
			}
		}()
		out = file
	}
	if err := WriteReport(out, verdict, labels, now); err != nil {
		return false, err
	}
	return verdict.Pass, nil
}

func main() {
	pass, err := run(os.Args[1:], os.Stdout, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if !pass {
		os.Exit(1)
	}
}
