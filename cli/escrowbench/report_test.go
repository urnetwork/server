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
	verdict, err := CompareExit(baseline, entry, testingMeasurements("m6", 6_900_000, 99, 3, 2), DefaultCompareSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Pass {
		t.Fatalf("failed %v", testingFailed(verdict))
	}
	verdict, err = CompareExit(baseline, entry, testingMeasurements("m6", 7_300_000, 99, 3, 2), DefaultCompareSettings())
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
	pass, err = run([]string{"exit", "-baseline", baselinePath, "-entry", passingPath, "-candidate", passingPath}, &bytes.Buffer{}, now)
	if err != nil || !pass {
		t.Fatalf("exit: pass %v, %v", pass, err)
	}
	if _, err := run([]string{"exit", "-baseline", baselinePath, "-candidate", passingPath}, &bytes.Buffer{}, now); !errors.Is(err, errUsage) {
		t.Fatalf("exit without the entry run: %v", err)
	}
	if _, err := run([]string{"measure"}, &bytes.Buffer{}, now); !errors.Is(err, errUsage) {
		t.Fatalf("an unknown mode: %v", err)
	}
}
