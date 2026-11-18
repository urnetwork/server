package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A build's four results: admission at p99Ns with acquires round trips an admission, settlement at perSecond.
func testingMeasurements(label string, p99Ns int64, perSecond float64, acquires float64, grantReads float64) *Measurements {
	measurements := &Measurements{Label: label, Time: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	for _, drawOrder := range []bool{false, true} {
		measurements.Results = append(measurements.Results,
			&Result{Name: ResultAdmission, DrawOrder: drawOrder, Count: 5000, P50Ns: p99Ns / 2, P99Ns: p99Ns, PerSecond: 200, PgAcquiresPerOp: acquires, GrantReadsPerOp: grantReads},
			&Result{Name: ResultSettlement, DrawOrder: drawOrder, Count: 5000, P50Ns: 9_000_000, P99Ns: 20_000_000, PerSecond: perSecond, PgAcquiresPerOp: 5},
		)
	}
	return measurements
}

// measurements as a rerun: measured two weeks on.
func testingRerun(measurements *Measurements) *Measurements {
	measurements.Time = measurements.Time.AddDate(0, 0, 14)
	return measurements
}

// measurements with every result paced at perSecond.
func testingPaced(measurements *Measurements, perSecond float64) *Measurements {
	for _, result := range measurements.Results {
		result.TargetPerSecond = perSecond
	}
	return measurements
}

func testingFailedAgainst(verdict *Verdict, against string, what string) bool {
	for _, check := range verdict.Checks {
		if !check.Pass && check.Against == against && check.What == what {
			return true
		}
	}
	return false
}

func testingWrite(t *testing.T, name string, measurements *Measurements) string {
	t.Helper()
	content, err := json.Marshal(measurements)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func testingFailed(verdict *Verdict) []string {
	failed := []string{}
	for _, check := range verdict.Checks {
		if !check.Pass {
			failed = append(failed, check.What)
		}
	}
	return failed
}

// Inside 5% on both measures and with no round trip more, the entry passes.
func TestEntryPassesWithinFivePercent(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	candidate := testingMeasurements("dark", 7_300_000, 96, 3.02, 2)
	verdict, err := CompareEntry(baseline, candidate, DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Pass {
		t.Fatalf("failed %v", testingFailed(verdict))
	}
}

// A round trip added to admission fails the entry, by the pool's acquires and by the grant reads alike.
func TestEntryFailsAnAddedRoundTrip(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	for _, candidate := range []*Measurements{
		testingMeasurements("dark", 7_000_000, 100, 4, 2),
		testingMeasurements("dark", 7_000_000, 100, 3, 3),
	} {
		verdict, err := CompareEntry(baseline, candidate, DefaultCompareSettings())
		if err != nil {
			t.Fatal(err)
		}
		if failed := testingFailed(verdict); verdict.Pass || len(failed) != 1 || !strings.Contains(failed[0], "per admission") {
			t.Fatalf("an added round trip: pass %v, failed %v", verdict.Pass, failed)
		}
	}
}

// Past 5% either measure fails, each by its own line.
func TestEntryFailsPastFivePercent(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	for what, candidate := range map[string]*Measurements{
		"p99 admission latency": testingMeasurements("dark", 7_400_000, 100, 3, 2),
		"settlement throughput": testingMeasurements("dark", 7_000_000, 94, 3, 2),
	} {
		verdict, err := CompareEntry(baseline, candidate, DefaultCompareSettings())
		if err != nil {
			t.Fatal(err)
		}
		if failed := testingFailed(verdict); verdict.Pass || len(failed) != 1 || failed[0] != what {
			t.Fatalf("%s: pass %v, failed %v", what, verdict.Pass, failed)
		}
	}
}

// Failed attempts and a run too short for a p99 fail the verdict, and are reported beside the percentiles.
func TestFailedAttemptsAndShortRunsFail(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	candidate := testingMeasurements("dark", 7_000_000, 100, 3, 2)
	candidate.Result(ResultAdmission, true).FailedCount = 3
	verdict, err := CompareEntry(baseline, candidate, DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass || !slicesContain(verdict.Notes, "5000 passed and 3 failed") {
		t.Fatalf("failed attempts: pass %v, notes %v", verdict.Pass, verdict.Notes)
	}
	candidate = testingMeasurements("dark", 7_000_000, 100, 3, 2)
	candidate.Result(ResultSettlement, true).Count = 40
	verdict, err = CompareEntry(baseline, candidate, DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass {
		t.Fatal("a 40-operation run carried a p99")
	}
}

func slicesContain(lines []string, part string) bool {
	for _, line := range lines {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

// The exit holds the rerun to the entry run as well as to main: a rerun inside 5% of main and past 5% of
// the entry run fails on the entry's line.
func TestExitHoldsTheRerunToTheEntryRun(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	entry := testingMeasurements("dark", 6_800_000, 100, 3, 2)
	verdict, err := CompareExit(baseline, entry, testingRerun(testingMeasurements("m6", 6_900_000, 99, 3, 2)), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Pass {
		t.Fatalf("failed %v", testingFailed(verdict))
	}
	verdict, err = CompareExit(baseline, entry, testingRerun(testingMeasurements("m6", 7_300_000, 99, 3, 2)), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass {
		t.Fatal("a rerun 7% slower than the entry run passed")
	}
	for _, check := range verdict.Checks {
		if !check.Pass && check.Against != "entry" {
			t.Fatalf("failed against %s: %s", check.Against, check.What)
		}
	}
}

// The entry run is the exit's yardstick only as a run the entry passed on: one that fails against main's, one
// with failed attempts, or one measured after the candidate fails the exit, whatever the rerun measured.
func TestExitFailsAnEntryRunThatIsNoYardstick(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	rerun := func() *Measurements {
		return testingRerun(testingMeasurements("m6", 7_000_000, 100, 3, 2))
	}

	// an entry run 20% slower than main's would let any rerun pass its line
	verdict, err := CompareExit(baseline, testingMeasurements("dark", 8_400_000, 100, 3, 2), rerun(), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass || !testingFailedAgainst(verdict, "baseline", "the entry run's p99 admission latency") {
		t.Fatalf("an entry run past 5%% of main's: pass %v, failed %v", verdict.Pass, testingFailed(verdict))
	}

	// an entry run with failed attempts
	failing := testingMeasurements("dark", 7_000_000, 100, 3, 2)
	failing.Result(ResultSettlement, true).FailedCount = 3
	verdict, err = CompareExit(baseline, failing, rerun(), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass || !testingFailedAgainst(verdict, "entry", "settlement failed attempts, draw_order on") {
		t.Fatalf("an entry run with failed attempts: pass %v, failed %v", verdict.Pass, testingFailed(verdict))
	}

	// the entry file given as its own rerun
	entry := testingMeasurements("dark", 7_000_000, 100, 3, 2)
	verdict, err = CompareExit(baseline, entry, entry, DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass || !testingFailedAgainst(verdict, "entry", "the rerun's time") {
		t.Fatalf("a candidate not measured after the entry run: pass %v, failed %v", verdict.Pass, testingFailed(verdict))
	}

	// an entry file without its gate-on results cannot be judged
	if _, err := CompareExit(baseline, &Measurements{Label: "dark", Results: []*Result{{Name: ResultAdmission, DrawOrder: false, Count: 5000}}}, rerun(), DefaultCompareSettings()); !errors.Is(err, ErrMissingResult) {
		t.Fatalf("%v", err)
	}
}

// A run is held only to one at the same pace: a rerun paced slower measures a lighter load, and fails on the
// pace's line in either mode, until the tolerance takes the difference in.
func TestRunsPacedDifferentlyFail(t *testing.T) {
	baseline := testingPaced(testingMeasurements("main", 7_000_000, 100, 3, 2), 200)
	entry := testingPaced(testingMeasurements("dark", 7_000_000, 100, 3, 2), 200)
	verdict, err := CompareExit(baseline, entry, testingRerun(testingPaced(testingMeasurements("m6", 7_000_000, 100, 3, 2), 200)), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Pass {
		t.Fatalf("runs at one pace failed %v", testingFailed(verdict))
	}

	slower := testingRerun(testingPaced(testingMeasurements("m6", 7_000_000, 100, 3, 2), 100))
	verdict, err = CompareExit(baseline, entry, slower, DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass {
		t.Fatal("a rerun at half the pace passed")
	}
	for _, against := range []string{"baseline", "entry"} {
		for _, what := range []string{"admission pace", "settlement pace"} {
			if !testingFailedAgainst(verdict, against, what) {
				t.Fatalf("no %s line against %s: failed %v", what, against, testingFailed(verdict))
			}
		}
	}
	if failed := testingFailed(verdict); len(failed) != 4 {
		t.Fatalf("failed %v", failed)
	}

	// an unpaced candidate against a paced baseline, at the entry
	verdict, err = CompareEntry(baseline, testingMeasurements("dark", 7_000_000, 100, 3, 2), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Pass || !testingFailedAgainst(verdict, "baseline", "admission pace") {
		t.Fatalf("an unpaced candidate: pass %v, failed %v", verdict.Pass, testingFailed(verdict))
	}

	// 1% apart, inside a 2% tolerance
	settings := DefaultCompareSettings()
	settings.PaceTolerance = 0.02
	verdict, err = CompareEntry(baseline, testingPaced(testingMeasurements("dark", 7_000_000, 100, 3, 2), 198), settings)
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Pass {
		t.Fatalf("paces 1%% apart failed %v", testingFailed(verdict))
	}
}

// A file without the results a mode compares cannot be judged.
func TestMissingResultIsAnError(t *testing.T) {
	baseline := testingMeasurements("main", 7_000_000, 100, 3, 2)
	candidate := &Measurements{Label: "dark", Results: []*Result{{Name: ResultAdmission, DrawOrder: false, Count: 5000}}}
	if _, err := CompareEntry(baseline, candidate, DefaultCompareSettings()); !errors.Is(err, ErrMissingResult) {
		t.Fatalf("%v", err)
	}
}

// The command end to end: files in, a report out, and the verdict as its answer.
func TestRunWritesTheReport(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	baselinePath := testingWrite(t, "main.json", testingMeasurements("main", 7_000_000, 100, 3, 2))
	passingPath := testingWrite(t, "dark.json", testingMeasurements("dark", 7_100_000, 99, 3, 2))
	addedPath := testingWrite(t, "added.json", testingMeasurements("dark+1", 7_100_000, 99, 4, 2))
	outPath := filepath.Join(t.TempDir(), "report.txt")
	pass, err := run([]string{"entry", "-baseline", baselinePath, "-candidate", passingPath, "-out", outPath}, &bytes.Buffer{}, now)
	if err != nil || !pass {
		t.Fatalf("pass %v, %v", pass, err)
	}
	report, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"escrow benchmark, M6 entry: PASS", "p99 admission latency against baseline", "settlement throughput against baseline", "Postgres acquires per admission", "5000 passed and 0 failed"} {
		if !strings.Contains(string(report), part) {
			t.Fatalf("the report lacks %q:\n%s", part, report)
		}
	}
	stdout := &bytes.Buffer{}
	pass, err = run([]string{"entry", "-baseline", baselinePath, "-candidate", addedPath}, stdout, now)
	if err != nil || pass {
		t.Fatalf("an added round trip: pass %v, %v", pass, err)
	}
	if !strings.Contains(stdout.String(), "M6 entry: FAIL") || !strings.Contains(stdout.String(), "FAIL  Postgres acquires per admission") {
		t.Fatalf("the failing report:\n%s", stdout.String())
	}
	rerunPath := testingWrite(t, "m6.json", testingRerun(testingMeasurements("m6", 7_100_000, 99, 3, 2)))
	stdout = &bytes.Buffer{}
	pass, err = run([]string{"exit", "-baseline", baselinePath, "-entry", passingPath, "-candidate", rerunPath}, stdout, now)
	if err != nil || !pass {
		t.Fatalf("exit: pass %v, %v\n%s", pass, err, stdout.String())
	}
	for _, part := range []string{"escrow benchmark, M6 exit: PASS", "p99 admission latency against entry", "admission pace against baseline", "entry, settlement, draw_order on: 5000 passed and 0 failed"} {
		if !strings.Contains(stdout.String(), part) {
			t.Fatalf("the exit report lacks %q:\n%s", part, stdout.String())
		}
	}
	// the entry file as its own rerun is no rerun
	stdout = &bytes.Buffer{}
	pass, err = run([]string{"exit", "-baseline", baselinePath, "-entry", passingPath, "-candidate", passingPath}, stdout, now)
	if err != nil || pass || !strings.Contains(stdout.String(), "M6 exit: FAIL") {
		t.Fatalf("exit on the entry file again: pass %v, %v\n%s", pass, err, stdout.String())
	}
	// paces a tolerance takes in
	pacedPath := testingWrite(t, "paced.json", testingPaced(testingMeasurements("dark", 7_100_000, 99, 3, 2), 1))
	pass, err = run([]string{"entry", "-baseline", baselinePath, "-candidate", pacedPath}, &bytes.Buffer{}, now)
	if err != nil || pass {
		t.Fatalf("a paced candidate against an unpaced baseline: pass %v, %v", pass, err)
	}
	pass, err = run([]string{"entry", "-baseline", baselinePath, "-candidate", pacedPath, "-pace_tolerance", "1"}, &bytes.Buffer{}, now)
	if err != nil || !pass {
		t.Fatalf("with -pace_tolerance 1: pass %v, %v", pass, err)
	}
	if _, err := run([]string{"exit", "-baseline", baselinePath, "-candidate", passingPath}, &bytes.Buffer{}, now); !errors.Is(err, errUsage) {
		t.Fatalf("exit without the entry run: %v", err)
	}
	if _, err := run([]string{"measure"}, &bytes.Buffer{}, now); !errors.Is(err, errUsage) {
		t.Fatalf("an unknown mode: %v", err)
	}
}
