package monitor

// Signal sidecar-dump implements SIGNALS.md §29.6 (EVSB-M3-01; D-OPS-18, D-TRUST-7, D-OPS-14, §19.6's sidecar dump
// and §19.8's warn rows). Every 5 min one Mimir query reads what the nightly dump of the sidecar tables and its
// off-site pull publish:
//
//   - edge-6's urnetwork_backup_onsite_*{archive="pg-sidecar"} (xops's bringyour-pg-sidecar-dump.sh through
//     db-archive.py): a last run that ended skipped, aborted, refused, failed or interrupted, or no run ended in
//     25 h, warns as sidecar-dump-run-failed, since each costs a day of RPO;
//   - planetoid's urnetwork_backup_archive_integrity_checked_timestamp_seconds{archive="pg-sidecar"}, which names
//     the newest generation it checked against the sha256 published beside it and the result: the newest
//     verified generation, by the time its name carries, older than 26 h warns as sidecar-dump-offsite-stale,
//     and so does a newest check that did not verify, since then the newest verified one is older still;
//   - sidecar-dump-unobservable reports what did not read: Mimir, or either side's series.
//
// It runs where monitor.yml has a sidecar_dump block: the dump runs from MG0's Gn-out, which the owner's
// sign-off brings to production.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"
)

func NewSidecarDumpSignal() Signal {
	return &signalAdapter{
		number: "29.6", key: "sidecar-dump", name: "Sidecar table dump and its off-site copy",
		probe: sidecarDumpProbe{},
	}
}

const sidecarDumpProbeId = "backup/sidecar-dump"

// SIGNALS.md §29.6's classes.
const (
	sidecarDumpClassRunFailed    = "sidecar-dump-run-failed"
	sidecarDumpClassOffsiteStale = "sidecar-dump-offsite-stale"
	sidecarDumpClassUnobservable = "sidecar-dump-unobservable"
)

// The query's partitions, and the archive's name in both hosts' series.
const (
	sidecarDumpCheckLastRun   = "last_run"
	sidecarDumpCheckRunning   = "in_progress"
	sidecarDumpCheckIntegrity = "integrity"
	sidecarDumpArchive        = "pg-sidecar"
	sidecarDumpResultVerified = "verified"
	sidecarDumpResultSuccess  = "success"
)

// A generation's name and the time it carries: main-pg-sidecar-YYYY-MM-DD-HH-MM-SS.dir.tar, UTC (db-archive.py's
// KINDS).
var sidecarDumpGenerationPattern = regexp.MustCompile(`^[a-z0-9]+-pg-sidecar-(\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2})\.dir\.tar$`)

// The time a generation's name carries.
func sidecarDumpGenerationTime(generation string) (time.Time, bool) {
	match := sidecarDumpGenerationPattern.FindStringSubmatch(generation)
	if match == nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02-15-04-05", match[1])
	return parsed.UTC(), err == nil
}

type sidecarDumpProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (sidecarDumpProbe) id() string             { return sidecarDumpProbeId }
func (sidecarDumpProbe) tier() string           { return tierWarn }
func (sidecarDumpProbe) cadence() time.Duration { return 5 * time.Minute }

// The one query: both sides' fresh samples.
func sidecarDumpQuery(environment string, freshness time.Duration) string {
	fresh := func(metric string, check string) string {
		return sidecarStorageTag(sidecarStorageFresh(sidecarStorageSelector(environment, metric, `archive="`+sidecarDumpArchive+`"`), freshness), check)
	}
	return sidecarStorageJoin(
		fresh("urnetwork_backup_onsite_last_run_timestamp_seconds", sidecarDumpCheckLastRun),
		fresh("urnetwork_backup_onsite_in_progress", sidecarDumpCheckRunning),
		fresh("urnetwork_backup_archive_integrity_checked_timestamp_seconds", sidecarDumpCheckIntegrity),
	)
}

func (self sidecarDumpProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if settings == nil || !settings.SidecarDump {
		return []finding{{
			probeId: sidecarDumpProbeId, tier: tierWarn, class: sidecarDumpClassUnobservable, target: "pg-sidecar",
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml has no sidecar_dump block",
		}}, nil
	}
	query := self.query
	if query == nil {
		query = sidecarStorageMimirQuery
	}
	samples, err := query(ctx, env, sidecarDumpQuery(env.cfg.env, settings.MetricFreshness))
	if err != nil {
		return []finding{self.dumpFinding(sidecarDumpClassUnobservable, "pg-sidecar", "mimir",
			"the sidecar dump's series did not read from Mimir", err.Error())}, nil
	}
	return self.evaluate(settings, env.now(), samples), nil
}

// Every finding from one query's samples at now.
func (self sidecarDumpProbe) evaluate(settings *SidecarStorageSettings, now time.Time, samples []*sidecarStorageSample) []finding {
	findings := []finding{}
	var lastRunTime time.Time
	lastRunResult := ""
	running := false
	hasOnsite := false
	hasOffsite := false
	newestCheckedTime := time.Time{}
	newestChecked := ""
	newestCheckedResult := ""
	verifiedTimes := []time.Time{}
	for _, sample := range samples {
		switch sample.check {
		case sidecarDumpCheckLastRun:
			hasOnsite = true
			runTime := time.Unix(int64(sample.value), 0).UTC()
			if lastRunTime.IsZero() || runTime.After(lastRunTime) {
				lastRunTime, lastRunResult = runTime, sample.label("result")
			}
		case sidecarDumpCheckRunning:
			hasOnsite = true
			running = running || sample.value == 1
		case sidecarDumpCheckIntegrity:
			hasOffsite = true
			generationTime, ok := sidecarDumpGenerationTime(sample.label("generation"))
			if !ok {
				continue
			}
			if newestCheckedTime.IsZero() || generationTime.After(newestCheckedTime) {
				newestCheckedTime, newestChecked, newestCheckedResult = generationTime, sample.label("generation"), sample.label("result")
			}
			if sample.label("result") == sidecarDumpResultVerified {
				verifiedTimes = append(verifiedTimes, generationTime)
			}
		}
	}

	if !hasOnsite {
		findings = append(findings, self.dumpFinding(sidecarDumpClassUnobservable, "pg-sidecar", "onsite",
			"edge-6 publishes no run of the sidecar dump, so a skipped or aborted night cannot be seen", ""))
	} else {
		observed := fmt.Sprintf("last_result=%s last_run=%s in_progress=%t", lastRunResult, lastRunTime.Format(time.RFC3339), running)
		switch {
		case lastRunResult != "" && lastRunResult != sidecarDumpResultSuccess:
			findings = append(findings, self.dumpFinding(sidecarDumpClassRunFailed, "pg-sidecar", "",
				"the sidecar dump's last run did not complete: each skipped or aborted night costs a day of RPO", observed))
		case !running && (lastRunTime.IsZero() || settings.DumpRunMaxAge < now.Sub(lastRunTime)):
			findings = append(findings, self.dumpFinding(sidecarDumpClassRunFailed, "pg-sidecar", "",
				"no sidecar dump run has ended in 25 h: a night was skipped", observed))
		default:
			findings = append(findings, healthyFinding(sidecarDumpProbeId, tierWarn, sidecarDumpClassRunFailed, "pg-sidecar"))
		}
	}

	if !hasOffsite {
		findings = append(findings, self.dumpFinding(sidecarDumpClassUnobservable, "pg-sidecar", "offsite",
			"planetoid publishes no check of a sidecar dump generation, so the off-site copy's age cannot be seen", ""))
		return findings
	}
	slices.SortFunc(verifiedTimes, func(a time.Time, b time.Time) int { return b.Compare(a) })
	observed := fmt.Sprintf("newest_checked=%s result=%s", newestChecked, newestCheckedResult)
	switch {
	case newestCheckedResult != sidecarDumpResultVerified:
		findings = append(findings, self.dumpFinding(sidecarDumpClassOffsiteStale, "pg-sidecar", "",
			"the newest off-site generation planetoid checked did not verify against its sha256, so the newest verified one is older", observed))
	case len(verifiedTimes) == 0 || settings.DumpOffsiteMaxAge < now.Sub(verifiedTimes[0]):
		age := "none"
		if 0 < len(verifiedTimes) {
			age = now.Sub(verifiedTimes[0]).Round(time.Minute).String()
		}
		findings = append(findings, self.dumpFinding(sidecarDumpClassOffsiteStale, "pg-sidecar", "",
			"the newest verified off-site generation of the sidecar dump is older than 26 h",
			observed+" newest_verified_age="+age))
	default:
		findings = append(findings, healthyFinding(sidecarDumpProbeId, tierWarn, sidecarDumpClassOffsiteStale, "pg-sidecar"))
	}
	return findings
}

// A finding of class.
func (self sidecarDumpProbe) dumpFinding(class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  sidecarDumpProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		sustain:  1,
		playbook: "SIGNALS.md §29.6; §19.6's sidecar dump row (DESIGN.md); xops postgres-backup/restore.md",
	}
	switch class {
	case sidecarDumpClassRunFailed:
		finding.action = "Read edge-6's bringyour-pg-sidecar-dump journal for the run's cause (the slot guard's disk, CPU or lag, the 20 min bound, a lock wait), fix it, and let the next night run; a run may also be started by hand before planetoid's 04:00 UTC pull."
	case sidecarDumpClassOffsiteStale:
		finding.action = "Read planetoid's remote-backup-archive journal: the pull takes pg-sidecar first; a generation that fails its sha256 stays unverified and is not released from edge-6's final/."
	case sidecarDumpClassUnobservable:
		finding.unobservable = true
	}
	return finding
}
