package monitor

// SIGNALS.md §29.6 on fixture Mimir answers: a run that did not complete or a night with no run warns, an
// off-site copy whose newest verified generation is past 26 h warns, and either side publishing nothing is
// blind.

import (
	"errors"
	"testing"
	"time"
)

// The dump at 02:30 today succeeded, and planetoid verified yesterday's 02:30 generation: 23 h 30 min old at
// 02:00 tomorrow is the worst a good night reaches before the next pull.
func sidecarDumpTestSamples(lastResult string, lastRun time.Time, checkedGeneration string, checkedResult string) []*sidecarStorageSample {
	return []*sidecarStorageSample{
		sidecarStorageTestSample(sidecarDumpCheckLastRun, float64(lastRun.Unix()), "archive", "pg-sidecar", "result", lastResult),
		sidecarStorageTestSample(sidecarDumpCheckRunning, 0, "archive", "pg-sidecar"),
		sidecarStorageTestSample(sidecarDumpCheckIntegrity, float64(lastRun.Unix()), "archive", "pg-sidecar",
			"format", "pg-sidecar-gpg-sha256", "generation", checkedGeneration, "result", checkedResult),
	}
}

var (
	sidecarDumpTestRun        = time.Date(2026, time.November, 6, 2, 30, 0, 0, time.UTC)
	sidecarDumpTestGeneration = "main-pg-sidecar-2026-11-06-02-30-00.dir.tar"
)

func evaluateSidecarDump(samples []*sidecarStorageSample, now time.Time) []finding {
	return sidecarDumpProbe{}.evaluate(sidecarStorageTestSettings(nil, nil), now, samples)
}

func TestSidecarDumpGoodNightRaisesNothing(t *testing.T) {
	samples := sidecarDumpTestSamples("success", sidecarDumpTestRun, sidecarDumpTestGeneration, "verified")
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(23*time.Hour+30*time.Minute)))
}

// Each result but success warns: each costs a day of RPO.
func TestSidecarDumpRunThatDidNotCompleteWarns(t *testing.T) {
	for _, result := range []string{"skipped", "aborted", "refused", "failed", "interrupted"} {
		samples := sidecarDumpTestSamples(result, sidecarDumpTestRun, sidecarDumpTestGeneration, "verified")
		requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(time.Hour)), "sidecar-dump-run-failed pg-sidecar")
	}
}

// No run ended in 25 h is a night skipped, unless one is running now.
func TestSidecarDumpNightWithNoRunWarns(t *testing.T) {
	samples := sidecarDumpTestSamples("success", sidecarDumpTestRun, sidecarDumpTestGeneration, "verified")
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(25*time.Hour)))
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(25*time.Hour+time.Minute)), "sidecar-dump-run-failed pg-sidecar")
	samples[1].value = 1
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(25*time.Hour+time.Minute)))
}

// The newest verified generation past 26 h warns, and a newest check that did not verify warns at once.
func TestSidecarDumpOffsiteStaleWarns(t *testing.T) {
	samples := sidecarDumpTestSamples("success", sidecarDumpTestRun.Add(24*time.Hour), sidecarDumpTestGeneration, "verified")
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(26*time.Hour)))
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(26*time.Hour+time.Minute)), "sidecar-dump-offsite-stale pg-sidecar")
	for _, result := range []string{"invalid", "missing"} {
		samples := sidecarDumpTestSamples("success", sidecarDumpTestRun, sidecarDumpTestGeneration, result)
		requireSidecarStorageRaised(t, evaluateSidecarDump(samples, sidecarDumpTestRun.Add(time.Hour)), "sidecar-dump-offsite-stale pg-sidecar")
	}
	if _, ok := sidecarDumpGenerationTime("main-pg-2026-11-06-02-30-00.dir.tar"); ok {
		t.Error("the main dump's generation read as the sidecar dump's")
	}
}

// Either side publishing nothing, or Mimir down: blind.
func TestSidecarDumpMissingSourcesAreBlind(t *testing.T) {
	samples := sidecarDumpTestSamples("success", sidecarDumpTestRun, sidecarDumpTestGeneration, "verified")
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples[2:], sidecarDumpTestRun.Add(time.Hour)), "sidecar-dump-unobservable pg-sidecar")
	requireSidecarStorageRaised(t, evaluateSidecarDump(samples[:2], sidecarDumpTestRun.Add(time.Hour)), "sidecar-dump-unobservable pg-sidecar")

	probe := sidecarDumpProbe{query: fixedSidecarStorageQuery(nil, errors.New("no services host"))}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(sidecarStorageTestSettings(nil, nil), func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "sidecar-dump-unobservable pg-sidecar")
	if !findings[0].isUnobservable() || findings[0].frame != "mimir" {
		t.Errorf("finding %+v", findings[0])
	}
}
