package work

// The observe soaks' report on recorded fixtures (EVS2-M10-33): each threshold of D-TEST-12 and D-TEST-18 met, and
// each one unmet stated, a retained candidate failing the purge soak whatever else holds, a counter's restart read as
// a new start, and an oracle refusal failing the drop soak.

import (
	"strings"
	"testing"
	"time"
)

var evsObserveTestStart = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// Samples a day apart from the start, each day's clean and retained added.
func evsObserveTestSamples(days int, cleanPerDay int64, retainedPerDay int64) []*EvsObserveSample {
	samples := []*EvsObserveSample{}
	for day := 0; day <= days; day += 1 {
		samples = append(samples, &EvsObserveSample{
			At: evsObserveTestStart.Add(time.Duration(day) * 24 * time.Hour), Clean: int64(day) * cleanPerDay, Retained: int64(day) * retainedPerDay,
		})
	}
	return samples
}

// count jobs ended a day apart from the start, the first renamed of them with renames and copies.
func evsObserveTestJobs(count int, renamed int) []*EvsObserveDropJob {
	jobs := []*EvsObserveDropJob{}
	for i := range count {
		jobs = append(jobs, &EvsObserveDropJob{EndedAt: evsObserveTestStart.Add(time.Duration(i) * time.Hour), Observed: true, RenamesAndCopies: i < renamed})
	}
	return jobs
}

func TestEvsObserveSoaksMet(t *testing.T) {
	now := evsObserveTestStart.Add(31 * 24 * time.Hour)
	report := EvsObserveSoakAt(evsObserveTestSamples(15, 1000, 0), evsObserveTestJobs(200, 100), now, nil)
	if !report.Purge.Met || !report.Drop.Met {
		t.Fatalf("met soaks reported unmet: purge %v, drop %v", report.Purge.Unmet, report.Drop.Unmet)
	}
	if report.Purge.Candidates != 15000 || report.Purge.Retained != 0 {
		t.Fatalf("the purge soak counted %d candidates, %d retained", report.Purge.Candidates, report.Purge.Retained)
	}
	for _, line := range report.Lines() {
		if strings.Contains(line, "not met") {
			t.Fatalf("a met report printed %q", line)
		}
	}
}

func TestEvsObserveSoaksStateEachUnmetThreshold(t *testing.T) {
	now := evsObserveTestStart.Add(10 * 24 * time.Hour)
	report := EvsObserveSoakAt(evsObserveTestSamples(10, 500, 0), evsObserveTestJobs(50, 10), now, nil)
	if report.Purge.Met || len(report.Purge.Unmet) != 2 {
		t.Fatalf("a short, thin purge soak reported %v", report.Purge.Unmet)
	}
	if report.Drop.Met || len(report.Drop.Unmet) != 3 {
		t.Fatalf("a short drop soak of few jobs, few renamed, reported %v", report.Drop.Unmet)
	}
	printed := strings.Join(report.Lines(), "\n")
	for _, want := range []string{"10.0 of 14 days", "5000 of 10000 candidates", "50 of 200 jobs", "10 jobs on repos with renames"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the report does not say %q:\n%s", want, printed)
		}
	}
}

// A retained candidate fails the purge soak however long and large; an oracle refusal fails the drop soak.
func TestEvsObserveSoaksFailOnAFinding(t *testing.T) {
	now := evsObserveTestStart.Add(40 * 24 * time.Hour)
	samples := evsObserveTestSamples(20, 1000, 0)
	samples[len(samples)-1].Retained = 1
	jobs := evsObserveTestJobs(300, 200)
	jobs = append(jobs, &EvsObserveDropJob{EndedAt: evsObserveTestStart, RefusedVerify: true})
	report := EvsObserveSoakAt(samples, jobs, now, nil)
	if report.Purge.Met || report.Purge.Retained != 1 {
		t.Fatalf("a retained candidate left the purge soak met: %v", report.Purge.Unmet)
	}
	if report.Drop.Met || report.Drop.RefusedVerify != 1 {
		t.Fatalf("an oracle refusal left the drop soak met: %v", report.Drop.Unmet)
	}
}

// A counter that fell between samples restarted there: its new reading counts from zero, never as a negative.
func TestEvsObserveSoakReadsARestartAsANewStart(t *testing.T) {
	samples := []*EvsObserveSample{
		{At: evsObserveTestStart, Clean: 0},
		{At: evsObserveTestStart.Add(24 * time.Hour), Clean: 4000},
		{At: evsObserveTestStart.Add(48 * time.Hour), Clean: 1000},
		{At: evsObserveTestStart.Add(72 * time.Hour), Clean: 3000},
	}
	report := EvsObserveSoakAt(samples, nil, evsObserveTestStart.Add(72*time.Hour), nil)
	if report.Purge.Candidates != 7000 {
		t.Fatalf("a restarted counter counted %d candidates, want 7000", report.Purge.Candidates)
	}
}
