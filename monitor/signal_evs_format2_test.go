package monitor

// SIGNALS.md §29.8 on fixture Mimir answers: the sweep's `deleted` share past 1% in a day warns and at 1% does not; a
// drop job refused {verify} or {mismatch} pages at once and one refused {locked} does not; a job executing with no
// scan or delete progress warns, and progress of either kind clears it; no series at all, format 2's state before it
// runs, raises nothing; no Mimir is blind; and without the file_clusters block the signal is configured off.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A day of healthy sweeping and a drop job that ran and ended.
func goodEvsFormat2Samples() []*sidecarStorageSample {
	return []*sidecarStorageSample{
		sidecarStorageTestSample(evsFormat2CheckSweepDeleted, 3),
		sidecarStorageTestSample(evsFormat2CheckSweepAll, 1000),
		sidecarStorageTestSample(evsFormat2CheckDropEntered, 2),
		sidecarStorageTestSample(evsFormat2CheckDropLeft, 2),
		sidecarStorageTestSample(evsFormat2CheckDropScanned, 0),
	}
}

func evaluateEvsFormat2(samples []*sidecarStorageSample) []finding {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	return evsFormat2Probe{}.evaluate(settings, samples)
}

func TestEvsFormat2GoodSourcesRaiseNothing(t *testing.T) {
	requireSidecarStorageRaised(t, evaluateEvsFormat2(goodEvsFormat2Samples()))
	// before format 2 runs no process has counted anything
	requireSidecarStorageRaised(t, evaluateEvsFormat2(nil))
}

// Past 1% of a day's sweep DELETEs finding content warns; exactly 1% does not.
func TestEvsFormat2SweepDeletedSharePastOnePercentWarns(t *testing.T) {
	samples := goodEvsFormat2Samples()
	samples[0].value = 10
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples))
	samples[0].value = 11
	findings := evaluateEvsFormat2(samples)
	requireSidecarStorageRaised(t, findings, "evs-sweep-deleted-share sweep")
	for _, finding := range findings {
		if finding.class == evsFormat2ClassSweepShare && !finding.healthy && (finding.tier != tierWarn || finding.sustain != 2 || !strings.Contains(finding.observed, "deleted=11")) {
			t.Errorf("finding %+v", finding)
		}
	}
}

// A job refused {verify} or {mismatch} pages at once, as storage integrity; {locked} is no integrity failure.
func TestEvsFormat2RefusedDropJobPages(t *testing.T) {
	for _, state := range []string{"refused_verify", "refused_mismatch"} {
		samples := append(goodEvsFormat2Samples(), sidecarStorageTestSample(evsFormat2CheckDropRefused, 1, "state", state))
		findings := evaluateEvsFormat2(samples)
		requireSidecarStorageRaised(t, findings, "evs-drop-refused drop-jobs")
		for _, finding := range findings {
			if finding.class == evsFormat2ClassDropRefused && !finding.healthy && (finding.tier != tierPage || finding.sustain != 1 || !strings.Contains(finding.observed, state+"=1")) {
				t.Errorf("finding %+v", finding)
			}
		}
		if _, ok := pageClassOf(evsFormat2ClassDropRefused); !ok {
			t.Error("the refused class has no §19.8 row")
		}
	}
	samples := append(goodEvsFormat2Samples(), sidecarStorageTestSample(evsFormat2CheckDropRefused, 1, "state", "refused_locked"))
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples))
}

// More drops moved into executing than out, with no scan or delete progress in the hour, warns; any progress clears.
func TestEvsFormat2StalledDropJobWarns(t *testing.T) {
	samples := goodEvsFormat2Samples()
	samples[2].value = 3
	findings := evaluateEvsFormat2(samples)
	requireSidecarStorageRaised(t, findings, "evs-drop-stalled drop-jobs")
	for _, finding := range findings {
		if finding.class == evsFormat2ClassDropStalled && !finding.healthy && (finding.tier != tierWarn || !strings.Contains(finding.symptom, "young")) {
			t.Errorf("finding %+v", finding)
		}
	}
	samples[4].value = 4096
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples))
	samples[4].value = 0
	samples = append(samples, sidecarStorageTestSample(evsFormat2CheckDropDeleted, 7))
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples))
}

// No Mimir is blind; no file_clusters block is configured off.
func TestEvsFormat2MissingSourcesAreBlindOrOff(t *testing.T) {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	probe := evsFormat2Probe{query: fixedSidecarStorageQuery(nil, errors.New("no services host"))}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "evs-format2-unobservable mimir")
	if !findings[0].isUnobservable() {
		t.Errorf("finding %+v", findings[0])
	}
	settings.FileClusters = false
	findings, err = probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil || len(findings) != 1 || !findings[0].configuredOff {
		t.Fatalf("no block: %+v %v", findings, err)
	}
}

// The query asks for each partition's increase over its window, in the environment.
func TestEvsFormat2QueryNamesItsSeries(t *testing.T) {
	query := evsFormat2Query("synthetic", DefaultSidecarStorageSettings())
	for _, part := range []string{
		`increase(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",result="deleted"}[86400s])`,
		`increase(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic"}[86400s])`,
		`sum by (state)(increase(urnetwork_evs_drop_jobs{env="synthetic",state=~"refused_verify|refused_mismatch"}[604800s]))`,
		`urnetwork_evs_drop_jobs{env="synthetic",state="executing"}[604800s]`,
		`urnetwork_evs_drop_scan_bytes_total{env="synthetic"}[3600s]`,
		`urnetwork_evs_drop_deletes_total{env="synthetic"}[3600s]`,
		`"monitor_check","drop_left"`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s: %s", part, query)
		}
	}
}
