package monitor

// Signal evs-format2 implements SIGNALS.md §29.8 (EVS2-M10-34; §19.8's versionless and drop history rows, §19.10's
// drop rows, D-OPS-9, D-OPS-10, D-PHASE-10, X13; QD-635, QD-855). Every 5 min one Mimir query reads the series format
// 2's purge, sweep and drop jobs push, none with a network or repo label:
//
//   - evacuation's sweep DELETEs over the last day, urnetwork_evs_versionless_sweep_deletes_total{pass="evacuation"} by
//     what the HEAD before each found: the committing processes should delete nearly all replaced content, so a
//     `deleted` share past 1% in a day says the purge misses what it should take, and warns as
//     evs-sweep-deleted-share. A hold's end and the turn-on pass delete what a hold or observe mode kept on purpose,
//     so their passes stay out of the share;
//   - drop jobs refused {verify} or {mismatch}, urnetwork_evs_drop_jobs{state="refused_verify"|"refused_mismatch"},
//     over the last 7 days: storage integrity, paged as evs-drop-refused at any hour, the job deleting nothing;
//   - a drop job executing with no scan or delete progress for an hour: more drops moved into executing than out of
//     it over the last 7 days, while urnetwork_evs_drop_scan_bytes_total and urnetwork_evs_drop_deletes_total rose
//     by nothing in the last hour, warns as evs-drop-stalled. §19.8 exempts a job whose cluster is past 50% of
//     capacity or whose candidates are young, which no series shows yet: a job waiting for either by design may
//     warn, and the finding says so;
//   - §19.8's fourth format-2 row, the purge's turn-on pass not run (owner decision 206, QD-855): the env's
//     sidecar.yml has read format2 enabled with purge: on at every read of this watcher for a day,
//     urnetwork_evs_versionless_observed_total rose over the last two weeks (observe mode kept replaced content), and
//     urnetwork_evs_versionless_sweep_deletes_total{pass="turn_on"} did not, which warns as
//     evs-purge-turn-on-missing: `sidecarctl evs purge-turn-on <env>` was not run, or its pass did no work. The
//     turn-on's DELETEs are counted over the same two weeks as observe's keeps, not only the last day, so the row
//     stays quiet after a pass that ran: observe stops keeping once purge reads on, so its keeps leave the window no
//     later than the pass's DELETEs do. Its result label is summed, since a rerun finds what an earlier one deleted
//     absent. Both are counted per process series rather than by increase() (evsFormat2Query), so a pass short
//     enough for one push to carry whole still counts;
//   - evs-format2-unobservable reports a query Mimir did not answer, or a sidecar.yml whose read failed.
//
// The file is read as every service and `sidecarctl evs purge-turn-on` read it (server.CurrentSidecarConfig), and
// purge counts as on only while format2 is enabled too, as the command needs to schedule the pass. How long it has read
// on is this watcher's memory: a restarted watcher dates the turn-on from its first read, so it warns up to a day later
// than one that ran throughout, and a read that fails leaves the memory as it was. Like the other storage signals it
// runs only under monitor.yml's file_clusters block, and reports itself configured off without one.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

func NewEvsFormat2Signal() Signal {
	return &signalAdapter{
		number: "29.8", key: "evs-format2", name: "evs format 2's purge and drop jobs",
		probe: newEvsFormat2Probe(),
	}
}

const evsFormat2ProbeId = "evs/format2"

// SIGNALS.md §29.8's classes.
const (
	evsFormat2ClassSweepShare    = "evs-sweep-deleted-share"
	evsFormat2ClassDropRefused   = "evs-drop-refused"
	evsFormat2ClassDropStalled   = "evs-drop-stalled"
	evsFormat2ClassTurnOnMissing = "evs-purge-turn-on-missing"
	evsFormat2ClassUnobservable  = "evs-format2-unobservable"
)

// The target of a finding about the env's sidecar.yml.
const evsFormat2TargetSidecarConfig = "sidecar.yml"

// Holds how long the env's file has read purge on, across runs. Safe for concurrent use.
type evsFormat2Probe struct {
	// nil queries Mimir
	query sidecarStorageQuery
	// nil reads the file through server's loader (readSidecarFormat2PurgeOn)
	readPurgeOn func() (bool, error)

	stateLock sync.Mutex
	// when this watcher first read purge on since it last read it off; zero while it reads off
	purgeOnSince time.Time
}

func newEvsFormat2Probe() *evsFormat2Probe {
	return &evsFormat2Probe{}
}

func (self *evsFormat2Probe) id() string             { return evsFormat2ProbeId }
func (self *evsFormat2Probe) tier() string           { return tierPage }
func (self *evsFormat2Probe) cadence() time.Duration { return 5 * time.Minute }

// The query's partitions.
const (
	evsFormat2CheckSweepDeleted = "sweep_deleted"
	evsFormat2CheckSweepAll     = "sweep_all"
	evsFormat2CheckDropRefused  = "drop_refused"
	evsFormat2CheckDropEntered  = "drop_entered"
	evsFormat2CheckDropLeft     = "drop_left"
	evsFormat2CheckDropScanned  = "drop_scanned"
	evsFormat2CheckDropDeleted  = "drop_deleted"
	evsFormat2CheckObserved     = "observed"
	evsFormat2CheckTurnOn       = "turn_on"
)

// The drop states a job leaves executing for (evsservice's DropState labels).
const evsFormat2DropEndStates = "done|held|observed|cancelled|refused_locked|refused_mismatch|refused_verify"

// The one query: each partition's increase over its window, summed across processes. The turn-on row's two
// partitions are counted instead: each process pushes its own series (its instance label), which never resets, so
// a series' count in the window is its largest value there less its value at the window's start, or less zero for a
// series born inside it. increase() reads a series born at its total as flat, which a turn-on pass that one 15 s
// push carried whole is, so a pass that ran would read as missing.
func evsFormat2Query(environment string, settings *SidecarStorageSettings) string {
	increase := func(metric string, window time.Duration, check string, by string, matchers ...string) string {
		expression := "sum" + by + "(increase(" + sidecarStorageSelector(environment, metric, matchers...) + sidecarStorageRange(window) + "))"
		return sidecarStorageTag(expression, check)
	}
	counted := func(metric string, window time.Duration, check string, matchers ...string) string {
		selector := sidecarStorageSelector(environment, metric, matchers...)
		inWindow := "max_over_time(" + selector + sidecarStorageRange(window) + ")"
		atStart := selector + " offset " + strings.Trim(sidecarStorageRange(window), "[]")
		return sidecarStorageTag("sum("+inWindow+" - ("+atStart+" or "+inWindow+" * 0))", check)
	}
	return sidecarStorageJoin(
		increase("urnetwork_evs_versionless_sweep_deletes_total", settings.SweepShareWindow, evsFormat2CheckSweepDeleted, "", `pass="evacuation"`, `result="deleted"`),
		increase("urnetwork_evs_versionless_sweep_deletes_total", settings.SweepShareWindow, evsFormat2CheckSweepAll, "", `pass="evacuation"`),
		increase("urnetwork_evs_drop_jobs", settings.DropJobLookback, evsFormat2CheckDropRefused, " by (state)", `state=~"refused_verify|refused_mismatch"`),
		increase("urnetwork_evs_drop_jobs", settings.DropJobLookback, evsFormat2CheckDropEntered, "", `state="executing"`),
		increase("urnetwork_evs_drop_jobs", settings.DropJobLookback, evsFormat2CheckDropLeft, "", `state=~"`+evsFormat2DropEndStates+`"`),
		increase("urnetwork_evs_drop_scan_bytes_total", settings.DropProgressWindow, evsFormat2CheckDropScanned, ""),
		increase("urnetwork_evs_drop_deletes_total", settings.DropProgressWindow, evsFormat2CheckDropDeleted, ""),
		counted("urnetwork_evs_versionless_observed_total", settings.PurgeTurnOnLookback, evsFormat2CheckObserved),
		counted("urnetwork_evs_versionless_sweep_deletes_total", settings.PurgeTurnOnLookback, evsFormat2CheckTurnOn, `pass="turn_on"`),
	)
}

// The env's format2.purge as this watcher has read it.
type evsFormat2PurgeReading struct {
	// the file reads format2 enabled with purge: on
	on bool
	// how long it has read so at every read of this watcher that answered; zero while it reads off
	onFor time.Duration
	// why the file did not read: set, the turn-on row is unobservable this run
	err error
}

// Whether config/<env>/sidecar.yml reads format2 enabled with purge: on, as every service reads the file
// (server/sidecar_config.go) and `sidecarctl evs purge-turn-on` needs it to schedule the pass. A file that is absent,
// unreadable or does not parse reads off there, so the row never warns on a broken file; only a loader that panics is
// an error.
func readSidecarFormat2PurgeOn() (on bool, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			on, returnErr = false, fmt.Errorf("server's sidecar.yml loader failed: %v", recovered)
		}
	}()
	format2 := server.CurrentSidecarConfig().Format2()
	return format2.Gate().Enabled() && format2.Purge() == server.SidecarFormat2On, nil
}

// Reads the file at now and moves this watcher's memory of how long it has read purge on: a read off forgets it, a
// read that fails leaves it, and a clock that went back dates the turn-on from now.
func (self *evsFormat2Probe) readPurge(now time.Time) *evsFormat2PurgeReading {
	readPurgeOn := self.readPurgeOn
	if readPurgeOn == nil {
		readPurgeOn = readSidecarFormat2PurgeOn
	}
	on, err := readPurgeOn()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case err != nil:
		return &evsFormat2PurgeReading{err: err}
	case !on:
		self.purgeOnSince = time.Time{}
		return &evsFormat2PurgeReading{}
	}
	if self.purgeOnSince.IsZero() || now.Before(self.purgeOnSince) {
		self.purgeOnSince = now
	}
	return &evsFormat2PurgeReading{on: true, onFor: now.Sub(self.purgeOnSince)}
}

func (self *evsFormat2Probe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, evsFormat2ProbeId, evsFormat2ClassUnobservable); ok {
		return []finding{gate}, nil
	}
	// read first, so the memory follows the file whether or not Mimir answers
	purge := self.readPurge(env.now())
	query := self.query
	if query == nil {
		query = sidecarStorageMimirQuery
	}
	samples, err := query(ctx, env, evsFormat2Query(env.cfg.env, settings))
	if err != nil {
		return []finding{self.format2Finding(settings, evsFormat2ClassUnobservable, "mimir", "mimir",
			"format 2's purge and drop series did not read from Mimir", err.Error())}, nil
	}
	return self.evaluate(settings, samples, purge), nil
}

// Every finding from one query's samples and the file's reading. A partition with no sample read as zero: no process
// has counted it, which is every partition's state before format 2 runs.
func (self *evsFormat2Probe) evaluate(settings *SidecarStorageSettings, samples []*sidecarStorageSample, purge *evsFormat2PurgeReading) []finding {
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
			fmt.Sprintf("deleted=%g of %g evacuation sweep DELETEs over %s", deleted, all, settings.SweepShareWindow)))
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

	observed, turnOn := values[evsFormat2CheckObserved], values[evsFormat2CheckTurnOn]
	switch {
	case purge == nil || purge.err != nil:
		reason := "no reading"
		if purge != nil {
			reason = purge.err.Error()
		}
		findings = append(findings, self.format2Finding(settings, evsFormat2ClassUnobservable, evsFormat2TargetSidecarConfig, evsFormat2TargetSidecarConfig,
			"the env's sidecar.yml did not read, so whether the purge's turn-on pass is owed is unknown", reason))
	case purge.on && settings.PurgeTurnOnGrace <= purge.onFor && 0 < observed && turnOn == 0:
		findings = append(findings, self.format2Finding(settings, evsFormat2ClassTurnOnMissing, "purge", "",
			"format2.purge has read on in the env's sidecar.yml for a day and observe mode kept replaced content, but no turn-on pass has deleted any of it: `sidecarctl evs purge-turn-on <env>` was not run, or its pass did no work",
			fmt.Sprintf("purge on for %s at this watcher's reads; observed=%g ids and turn_on DELETEs=%g over %s", purge.onFor.Round(time.Minute), observed, turnOn, settings.PurgeTurnOnLookback)))
	default:
		findings = append(findings, healthyFinding(evsFormat2ProbeId, tierWarn, evsFormat2ClassTurnOnMissing, "purge"))
	}
	return findings
}

// A finding of class at target, sustained and tiered as the class is.
func (self *evsFormat2Probe) format2Finding(settings *SidecarStorageSettings, class string, target string, frame string, symptom string, observed string) finding {
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
	case evsFormat2ClassTurnOnMissing:
		finding.sustain = settings.WarnSustain
		finding.action = "Run `sidecarctl evs purge-turn-on <env>` (xops FILE-CLUSTERS.md's format 2 rollout, step 2): the pass is idempotent, and what observe kept costs storage, never content. If it ran, read its evs.sweep jobs on taskworker: a pass on a host that still read observe kept everything, and one whose every candidate a hold keeps or the newest version names deletes and counts nothing; run it again once the hosts read on."
	case evsFormat2ClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	}
	return finding
}
