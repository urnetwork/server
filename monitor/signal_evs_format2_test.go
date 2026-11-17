package monitor

// SIGNALS.md §29.8 on fixture Mimir answers: the sweep's `deleted` share past 1% in a day warns and at 1% does not; a
// drop job refused {verify} or {mismatch} pages at once and one refused {locked} does not; a job executing with no
// scan or delete progress warns, and progress of either kind clears it; the purge's turn-on pass forgotten warns once
// purge has read on for a day while observe's keeps stand with no turn-on DELETE, and stays quiet after a pass and
// while purge is off, the day dated from this watcher's own reads of the file, which it reads through server's loader
// as the services and the turn-on command do; no series at all, format 2's state before it runs, raises nothing; no
// Mimir, or a sidecar.yml that does not read, is blind; and without the file_clusters block the signal is configured
// off.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
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

// The file read off, as every env's reads before the rollout's purge step.
var evsFormat2TestPurgeOff = &evsFormat2PurgeReading{}

func evaluateEvsFormat2(samples []*sidecarStorageSample, purge *evsFormat2PurgeReading) []finding {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	return newEvsFormat2Probe().evaluate(settings, samples, purge)
}

// Good samples with observe's keeps over the lookback, and no turn-on pass.
func keptEvsFormat2Samples() []*sidecarStorageSample {
	return append(goodEvsFormat2Samples(), sidecarStorageTestSample(evsFormat2CheckObserved, 4200))
}

func TestEvsFormat2GoodSourcesRaiseNothing(t *testing.T) {
	requireSidecarStorageRaised(t, evaluateEvsFormat2(goodEvsFormat2Samples(), evsFormat2TestPurgeOff))
	// before format 2 runs no process has counted anything
	requireSidecarStorageRaised(t, evaluateEvsFormat2(nil, evsFormat2TestPurgeOff))
	// purge on with nothing observe kept owes no pass
	requireSidecarStorageRaised(t, evaluateEvsFormat2(goodEvsFormat2Samples(), &evsFormat2PurgeReading{on: true, onFor: 30 * 24 * time.Hour}))
}

// Past 1% of a day's sweep DELETEs finding content warns; exactly 1% does not.
func TestEvsFormat2SweepDeletedSharePastOnePercentWarns(t *testing.T) {
	samples := goodEvsFormat2Samples()
	samples[0].value = 10
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples, evsFormat2TestPurgeOff))
	samples[0].value = 11
	findings := evaluateEvsFormat2(samples, evsFormat2TestPurgeOff)
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
		findings := evaluateEvsFormat2(samples, evsFormat2TestPurgeOff)
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
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples, evsFormat2TestPurgeOff))
}

// More drops moved into executing than out, with no scan or delete progress in the hour, warns; any progress clears.
func TestEvsFormat2StalledDropJobWarns(t *testing.T) {
	samples := goodEvsFormat2Samples()
	samples[2].value = 3
	findings := evaluateEvsFormat2(samples, evsFormat2TestPurgeOff)
	requireSidecarStorageRaised(t, findings, "evs-drop-stalled drop-jobs")
	for _, finding := range findings {
		if finding.class == evsFormat2ClassDropStalled && !finding.healthy && (finding.tier != tierWarn || !strings.Contains(finding.symptom, "young")) {
			t.Errorf("finding %+v", finding)
		}
	}
	samples[4].value = 4096
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples, evsFormat2TestPurgeOff))
	samples[4].value = 0
	samples = append(samples, sidecarStorageTestSample(evsFormat2CheckDropDeleted, 7))
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples, evsFormat2TestPurgeOff))
}

// §19.8's fourth row (QD-855): purge on in the env's file for a day, observe's keeps over the lookback and no turn-on
// DELETE in it warn that `sidecarctl evs purge-turn-on` was not run, as a sustained warn; exactly the day counts.
func TestEvsFormat2ForgottenTurnOnWarns(t *testing.T) {
	findings := evaluateEvsFormat2(keptEvsFormat2Samples(), &evsFormat2PurgeReading{on: true, onFor: 25 * time.Hour})
	requireSidecarStorageRaised(t, findings, "evs-purge-turn-on-missing purge")
	for _, finding := range findings {
		if finding.class == evsFormat2ClassTurnOnMissing && !finding.healthy && (finding.tier != tierWarn || finding.sustain != 2 ||
			!strings.Contains(finding.observed, "observed=4200") || !strings.Contains(finding.observed, "turn_on DELETEs=0") ||
			!strings.Contains(finding.action, "sidecarctl evs purge-turn-on")) {
			t.Errorf("finding %+v", finding)
		}
	}
	if kind, ok := alertClassKinds[evsFormat2ClassTurnOnMissing]; !ok || kind != incidentAlertClass {
		t.Errorf("the turn-on class is classified %v, %v", kind, ok)
	}
	if _, ok := pageClassOf(evsFormat2ClassTurnOnMissing); ok {
		t.Error("the turn-on class pages; §19.8 makes it a warn")
	}
	requireSidecarStorageRaised(t, evaluateEvsFormat2(keptEvsFormat2Samples(), &evsFormat2PurgeReading{on: true, onFor: 24 * time.Hour}), "evs-purge-turn-on-missing purge")
}

// A turn-on pass that ran keeps the row quiet: any of its DELETEs over the lookback, deleted or found absent by a rerun.
func TestEvsFormat2TurnOnQuietAfterAPass(t *testing.T) {
	onLong := &evsFormat2PurgeReading{on: true, onFor: 10 * 24 * time.Hour}
	samples := append(keptEvsFormat2Samples(), sidecarStorageTestSample(evsFormat2CheckTurnOn, 1))
	requireSidecarStorageRaised(t, evaluateEvsFormat2(samples, onLong))
}

// Purge off in the file, or on for less than a day, warns of nothing whatever observe kept.
func TestEvsFormat2TurnOnQuietWhilePurgeOff(t *testing.T) {
	requireSidecarStorageRaised(t, evaluateEvsFormat2(keptEvsFormat2Samples(), evsFormat2TestPurgeOff))
	requireSidecarStorageRaised(t, evaluateEvsFormat2(keptEvsFormat2Samples(), &evsFormat2PurgeReading{on: true, onFor: 24*time.Hour - time.Minute}))
}

// The day is this watcher's: dated from its first read of purge on, forgotten at a read off and kept across a read
// that fails, which is blind for the row, and across a Mimir that does not answer, which is blind for the signal.
func TestEvsFormat2DatesTheTurnOnFromItsReads(t *testing.T) {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	now := fileClusterTestNow
	purgeOn := false
	var readErr error
	var queryErr error
	probe := newEvsFormat2Probe()
	probe.query = func(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
		return keptEvsFormat2Samples(), queryErr
	}
	probe.readPurgeOn = func() (bool, error) {
		return purgeOn, readErr
	}
	env := sidecarStorageTestEnv(settings, func() time.Time { return now })
	run := func() []finding {
		t.Helper()
		findings, err := probe.check(t.Context(), env)
		if err != nil {
			t.Fatal(err)
		}
		return findings
	}
	missing := "evs-purge-turn-on-missing purge"

	requireSidecarStorageRaised(t, run())
	purgeOn = true
	requireSidecarStorageRaised(t, run())
	now = now.Add(23 * time.Hour)
	queryErr = errors.New("no services host")
	requireSidecarStorageRaised(t, run(), "evs-format2-unobservable mimir")
	queryErr = nil
	requireSidecarStorageRaised(t, run())
	now = now.Add(time.Hour)
	requireSidecarStorageRaised(t, run(), missing)

	readErr = errors.New("the loader panicked")
	findings := run()
	requireSidecarStorageRaised(t, findings, "evs-format2-unobservable sidecar.yml")
	for _, finding := range findings {
		if finding.class == evsFormat2ClassUnobservable && !finding.isUnobservable() {
			t.Errorf("finding %+v", finding)
		}
	}
	readErr = nil
	requireSidecarStorageRaised(t, run(), missing)

	purgeOn = false
	now = now.Add(time.Hour)
	requireSidecarStorageRaised(t, run())
	purgeOn = true
	requireSidecarStorageRaised(t, run())
	now = now.Add(24*time.Hour - time.Minute)
	requireSidecarStorageRaised(t, run())
	now = now.Add(time.Minute)
	requireSidecarStorageRaised(t, run(), missing)
}

// Purge reads on only where the services' loader reads format2 enabled with purge exactly on, the state in which
// `sidecarctl evs purge-turn-on` schedules the pass: a gate off, observe, a mode spelled otherwise, no format2 block
// and no file at all read off, so the row owes no pass there.
func TestEvsFormat2ReadsPurgeAsTheServicesDo(t *testing.T) {
	for _, c := range []struct {
		file string
		on   bool
	}{
		{file: "sidecar:\n  format2: {enabled: true, networks: [], percent: 0, purge: on, drop: observe}\n", on: true},
		{file: "sidecar:\n  format2: {enabled: true, purge: \"on\"}\n", on: true},
		{file: "sidecar:\n  format2: {enabled: false, purge: \"on\"}\n"},
		{file: "sidecar:\n  format2: {enabled: true, purge: observe}\n"},
		{file: "sidecar:\n  format2: {enabled: true, purge: \"On\"}\n"},
		{file: "sidecar:\n  evs: {enabled: true}\n"},
		{file: ""},
	} {
		pop := server.Testing_PushSidecarConfig(c.file)
		on, err := readSidecarFormat2PurgeOn()
		pop()
		if err != nil || on != c.on {
			t.Errorf("%q: on %v, %v; want %v", c.file, on, err, c.on)
		}
	}
}

// No Mimir is blind; no file_clusters block is configured off.
func TestEvsFormat2MissingSourcesAreBlindOrOff(t *testing.T) {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	probe := newEvsFormat2Probe()
	probe.query = fixedSidecarStorageQuery(nil, errors.New("no services host"))
	probe.readPurgeOn = func() (bool, error) { return false, nil }
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

// The query asks for each partition's increase over its window, in the environment: the share over evacuation's
// sweep alone, and observe's keeps and the turn-on pass's DELETEs over one lookback, each counted per process series
// from its value at the window's start, or from zero for a series born inside it, so a pass one push carried whole
// counts.
func TestEvsFormat2QueryNamesItsSeries(t *testing.T) {
	query := evsFormat2Query("synthetic", DefaultSidecarStorageSettings())
	for _, part := range []string{
		`increase(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",pass="evacuation",result="deleted"}[86400s])`,
		`increase(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",pass="evacuation"}[86400s])`,
		`sum by (state)(increase(urnetwork_evs_drop_jobs{env="synthetic",state=~"refused_verify|refused_mismatch"}[604800s]))`,
		`urnetwork_evs_drop_jobs{env="synthetic",state="executing"}[604800s]`,
		`urnetwork_evs_drop_scan_bytes_total{env="synthetic"}[3600s]`,
		`urnetwork_evs_drop_deletes_total{env="synthetic"}[3600s]`,
		`"monitor_check","drop_left"`,
		`label_replace(sum(max_over_time(urnetwork_evs_versionless_observed_total{env="synthetic"}[1209600s]) - ` +
			`(urnetwork_evs_versionless_observed_total{env="synthetic"} offset 1209600s or ` +
			`max_over_time(urnetwork_evs_versionless_observed_total{env="synthetic"}[1209600s]) * 0)),"monitor_check","observed"`,
		`label_replace(sum(max_over_time(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",pass="turn_on"}[1209600s]) - ` +
			`(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",pass="turn_on"} offset 1209600s or ` +
			`max_over_time(urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",pass="turn_on"}[1209600s]) * 0)),"monitor_check","turn_on"`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s: %s", part, query)
		}
	}
	// increase() reads a series born at its total as flat: the turn-on row's partitions never use it
	for _, selector := range []string{`urnetwork_evs_versionless_observed_total{`, `urnetwork_evs_versionless_sweep_deletes_total{env="synthetic",pass="turn_on"}`} {
		if strings.Contains(query, "increase("+selector) {
			t.Errorf("the query takes increase() of %s: %s", selector, query)
		}
	}
}
