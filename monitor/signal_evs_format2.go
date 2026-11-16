package monitor

// Signal evs-format2 implements SIGNALS.md §29.8 (EVS2-M10-34; §19.8's versionless and drop history rows, §19.10's
// drop rows, D-OPS-9, D-OPS-10, D-PHASE-10, X13). Every 5 min one Mimir query reads the series format 2's purge,
// sweep and drop jobs push, none with a network or repo label:
//
//   - the versionless sweep's DELETEs over the last day, urnetwork_evs_versionless_sweep_deletes_total by what the
//     HEAD before each found: the committing processes should delete nearly all replaced content, so a `deleted`
//     share past 1% in a day says the purge misses what it should take, and warns as evs-sweep-deleted-share;
//   - drop jobs refused {verify} or {mismatch}, urnetwork_evs_drop_jobs{state="refused_verify"|"refused_mismatch"},
//     over the last 7 days: storage integrity, paged as evs-drop-refused at any hour, the job deleting nothing;
//   - a drop job executing with no scan or delete progress for an hour: more drops moved into executing than out of
//     it over the last 7 days, while urnetwork_evs_drop_scan_bytes_total and urnetwork_evs_drop_deletes_total rose
//     by nothing in the last hour, warns as evs-drop-stalled. §19.8 exempts a job whose cluster is past 50% of
//     capacity or whose candidates are young, which no series shows yet: a job waiting for either by design may
//     warn, and the finding says so;
//   - evs-format2-unobservable reports a query Mimir did not answer.
//
// §19.8's fourth format-2 row, observe's retained count staying flat for a day while purge is on (QD-635), is not
// built: no process exports a retained count (questions/evs2-m10-34-observe-retained-series). Like the other storage
// signals it runs only under monitor.yml's file_clusters block, and reports itself configured off without one.

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

func NewEvsFormat2Signal() Signal {
	return &signalAdapter{
		number: "29.8", key: "evs-format2", name: "evs format 2's purge and drop jobs",
		probe: evsFormat2Probe{},
	}
}

const evsFormat2ProbeId = "evs/format2"

// SIGNALS.md §29.8's classes.
const (
	evsFormat2ClassSweepShare   = "evs-sweep-deleted-share"
	evsFormat2ClassDropRefused  = "evs-drop-refused"
	evsFormat2ClassDropStalled  = "evs-drop-stalled"
	evsFormat2ClassUnobservable = "evs-format2-unobservable"
)

type evsFormat2Probe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (evsFormat2Probe) id() string             { return evsFormat2ProbeId }
func (evsFormat2Probe) tier() string           { return tierPage }
func (evsFormat2Probe) cadence() time.Duration { return 5 * time.Minute }

// The query's partitions.
const (
	evsFormat2CheckSweepDeleted = "sweep_deleted"
	evsFormat2CheckSweepAll     = "sweep_all"
	evsFormat2CheckDropRefused  = "drop_refused"
	evsFormat2CheckDropEntered  = "drop_entered"
	evsFormat2CheckDropLeft     = "drop_left"
	evsFormat2CheckDropScanned  = "drop_scanned"
	evsFormat2CheckDropDeleted  = "drop_deleted"
)

// The drop states a job leaves executing for (evsservice's DropState labels).
const evsFormat2DropEndStates = "done|held|observed|cancelled|refused_locked|refused_mismatch|refused_verify"

// The one query: each partition's increase over its window, summed across processes.
func evsFormat2Query(environment string, settings *SidecarStorageSettings) string {
	increase := func(metric string, window time.Duration, check string, by string, matchers ...string) string {
		expression := "sum" + by + "(increase(" + sidecarStorageSelector(environment, metric, matchers...) + sidecarStorageRange(window) + "))"
		return sidecarStorageTag(expression, check)
	}
	return sidecarStorageJoin(
		increase("urnetwork_evs_versionless_sweep_deletes_total", settings.SweepShareWindow, evsFormat2CheckSweepDeleted, "", `result="deleted"`),
		increase("urnetwork_evs_versionless_sweep_deletes_total", settings.SweepShareWindow, evsFormat2CheckSweepAll, ""),
		increase("urnetwork_evs_drop_jobs", settings.DropJobLookback, evsFormat2CheckDropRefused, " by (state)", `state=~"refused_verify|refused_mismatch"`),
		increase("urnetwork_evs_drop_jobs", settings.DropJobLookback, evsFormat2CheckDropEntered, "", `state="executing"`),
		increase("urnetwork_evs_drop_jobs", settings.DropJobLookback, evsFormat2CheckDropLeft, "", `state=~"`+evsFormat2DropEndStates+`"`),
		increase("urnetwork_evs_drop_scan_bytes_total", settings.DropProgressWindow, evsFormat2CheckDropScanned, ""),
		increase("urnetwork_evs_drop_deletes_total", settings.DropProgressWindow, evsFormat2CheckDropDeleted, ""),
	)
}

func (self evsFormat2Probe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, evsFormat2ProbeId, evsFormat2ClassUnobservable); ok {
		return []finding{gate}, nil
	}
	query := self.query
	if query == nil {
		query = sidecarStorageMimirQuery
	}
	samples, err := query(ctx, env, evsFormat2Query(env.cfg.env, settings))
	if err != nil {
		return []finding{self.format2Finding(settings, evsFormat2ClassUnobservable, "mimir", "mimir",
			"format 2's purge and drop series did not read from Mimir", err.Error())}, nil
	}
	return self.evaluate(settings, samples), nil
}

// Every finding from one query's samples. A partition with no sample read as zero: no process has counted it, which
// is every partition's state before format 2 runs.
func (self evsFormat2Probe) evaluate(settings *SidecarStorageSettings, samples []*sidecarStorageSample) []finding {
	values := map[string]float64{}
	refused := map[string]float64{}
	for _, sample := range samples {
		if !sidecarStorageCount(sample.value) {
			continue
		}
		if sample.check == evsFormat2CheckDropRefused {
			refused[sample.label("state")] += sample.value
			continue
		}
		values[sample.check] += sample.value
	}
	findings := []finding{}

	deleted, all := values[evsFormat2CheckSweepDeleted], values[evsFormat2CheckSweepAll]
	if 0 < all && settings.SweepDeletedShareWarn < deleted/all {
		findings = append(findings, self.format2Finding(settings, evsFormat2ClassSweepShare, "sweep", "",
			"the versionless sweep found more than 1% of a day's replaced content still stored: the committing processes' purge misses what it should delete",
			fmt.Sprintf("deleted=%g of %g sweep DELETEs over %s", deleted, all, settings.SweepShareWindow)))
	} else {
		findings = append(findings, healthyFinding(evsFormat2ProbeId, tierWarn, evsFormat2ClassSweepShare, "sweep"))
	}

	if verify, mismatch := refused["refused_verify"], refused["refused_mismatch"]; 0 < verify || 0 < mismatch {
		findings = append(findings, self.format2Finding(settings, evsFormat2ClassDropRefused, "drop-jobs", "",
			"a drop job ended refused {verify} or {mismatch}: storage integrity; it deletes nothing and stays refused",
			"refused_verify="+strconv.FormatFloat(verify, 'g', -1, 64)+" refused_mismatch="+strconv.FormatFloat(mismatch, 'g', -1, 64)+" over "+settings.DropJobLookback.String()))
	} else {
		findings = append(findings, healthyFinding(evsFormat2ProbeId, tierPage, evsFormat2ClassDropRefused, "drop-jobs"))
	}

	entered, left := values[evsFormat2CheckDropEntered], values[evsFormat2CheckDropLeft]
	progress := values[evsFormat2CheckDropScanned] + values[evsFormat2CheckDropDeleted]
	if left < entered && progress == 0 {
		findings = append(findings, self.format2Finding(settings, evsFormat2ClassDropStalled, "drop-jobs", "",
			"a drop job executes with no scan or delete progress for an hour, unless it waits by design on its cluster past 50% of capacity or on a young candidate, which no series shows",
			fmt.Sprintf("entered=%g left=%g over %s; no progress over %s", entered, left, settings.DropJobLookback, settings.DropProgressWindow)))
	} else {
		findings = append(findings, healthyFinding(evsFormat2ProbeId, tierWarn, evsFormat2ClassDropStalled, "drop-jobs"))
	}
	return findings
}

// A finding of class at target, sustained and tiered as the class is.
func (self evsFormat2Probe) format2Finding(settings *SidecarStorageSettings, class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  evsFormat2ProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		playbook: "SIGNALS.md §29.8; §19.10's drop rows (DESIGN.md); xops FILE-CLUSTERS.md's format 2 rows",
	}
	switch class {
	case evsFormat2ClassDropRefused:
		finding.tier = tierPage
		finding.sustain = 1
		finding.action = "{verify}: a bug; set format2's drop to observe (§19.11) and keep the job's dropscan/ files and drops/<repo>/_. {mismatch}: compare its signed list with resolve's, then likewise if resolve erred."
	case evsFormat2ClassSweepShare:
		finding.sustain = settings.WarnSustain
		finding.action = "Read the purge's alarms and the committing processes' logs: a purge that misses its candidates leaves them to the sweep; content is never lost by it."
	case evsFormat2ClassDropStalled:
		finding.sustain = settings.WarnSustain
		finding.action = "Read the job's lease, phase and cursor in drops/<repo>/_ (§19.10's stuck row): a wait for HDD load, a hold, a deleted repo, an unsure clock or a young candidate is by design."
	case evsFormat2ClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	}
	return finding
}
