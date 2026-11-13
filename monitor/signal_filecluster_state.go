package monitor

// Signal filecluster-state implements SIGNALS.md §29.1 (EVSB-M3-01; §19.6's protection states, D-OPS-14, D-OPS-9,
// FS-50; g0-recheck-1-s11-degraded-write-debt, decided by the owner). Every 30 s it reads each file cluster's
// admin info and derives each erasure set's state by §19.6's table:
//
//   - protected: every drive online and no heal pending;
//   - degraded: a drive or host offline, or a heal pending: a drive rebuilding, MinIO's partial-write queue above
//     zero, entries it lost, or an outage no full heal has followed yet. A drive's rebuild alone leaves its set
//     degraded, never at_risk;
//   - at_risk: degraded and a further drive or host failing, or a host offline past 4 h (the RTO).
//
// A set leaves degraded once a full heal that began after its last failure ends, or, when no drive or host was
// offline, once the queue drains with nothing dropped and no unclean start since the last full heal began; it
// leaves at_risk once the failure is repaired and such a full heal ends. Lost queue entries with every drive
// online warn, and the warn says to start one full heal.
//
// The heal inputs are the minio fork's, read from Mimir (signal_filecluster_heal_debt.go): per pool and set each
// host's partial-write queue depth, the entries it lost, its unclean starts and its last loss, and when the
// cluster's last full heal began and ended. A set's heal debt is known only while every host of its cluster shows
// its queue and the full heal reads; otherwise it reads unknown, never zero (the owner's interim): the set does
// not read protected, with every drive online it reads unknown, a visibility finding, and a set that was degraded
// or at_risk stays so.
//
// It publishes urnetwork_filecluster_set_state{cluster,set,state} and the queues' sums,
// urnetwork_filecluster_mrf_depth{cluster,set}, urnetwork_filecluster_mrf_dropped_total{cluster,set} and
// urnetwork_filecluster_unclean_starts_total{cluster,set}. filecluster-set-at-risk pages §19.8's rota at any hour
// from M3; filecluster-set-degraded and filecluster-heal-debt-lost warn; filecluster-state-unobservable reports a
// cluster whose admin info did not read, or a set whose state hangs on an input it lacks.
//
// Its memory is the process's: each host's outage start and each set's episode. A restarted watcher dates an
// outage from its first sight, so a host already down at the restart pages as past the RTO up to 4 h late, and
// it forgets an episode that only a full heal ends: a set whose outage it did not see reads by its queue alone.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

func NewFileClusterStateSignal() Signal {
	return &signalAdapter{
		number: "29.1", key: "filecluster-state", name: "File-cluster protection states",
		probe: newFileClusterStateProbe(),
	}
}

const fileClusterStateProbeId = "filecluster/state"

// SIGNALS.md §29.1's classes.
const (
	fileClusterStateClassAtRisk       = "filecluster-set-at-risk"
	fileClusterStateClassDegraded     = "filecluster-set-degraded"
	fileClusterStateClassDebtLost     = "filecluster-heal-debt-lost"
	fileClusterStateClassUnobservable = "filecluster-state-unobservable"
)

// What admin info calls a host that answers and a drive that serves.
const (
	fileClusterHostOnline = "online"
	fileClusterDriveOk    = "ok"
)

// What MinIO says of one set's heal debt (see the file header). The zero value is unknown.
type fileClusterHealDebt struct {
	known bool
	// partial writes queued for heal
	depth float64
	// entries dropped, or an unclean start, that no full heal began after
	lost bool
	// when the cluster's last full heal began and ended; zero for none. The fork records one only when a heal
	// of the whole cluster ran to its end and left every item on every drive of its set.
	fullHealStartTime time.Time
	fullHealEndTime   time.Time
}

// Every set's heal debt in a cluster, by set, in place of the Mimir read. A set it does not name reads unknown.
type fileClusterHealDebtSource func(ctx context.Context, env *probeEnv, clusterId string) (map[string]*fileClusterHealDebt, error)

// What one set has been through since it last read clear.
type fileClusterSetMemory struct {
	// each host carrying the set's drives that is offline, and when it was first seen so
	hostOfflineTimes map[string]time.Time
	// every host, drive or debt that failed, rebuilt or queued since the set last read clear; two of them,
	// one failing now, is a further failure
	episodeUnits map[string]bool
	// when the episode began and when a unit last failed or rebuilt: a full heal must end after both
	episodeStartTime time.Time
	lastFailureTime  time.Time
	// a drive or host was offline in the episode, so only a full heal ends it
	episodeOffline bool
	// at_risk, held until the failure is repaired and a full heal ends
	atRisk bool
}

func newFileClusterSetMemory() *fileClusterSetMemory {
	return &fileClusterSetMemory{hostOfflineTimes: map[string]time.Time{}, episodeUnits: map[string]bool{}}
}

// The episode is over.
func (self *fileClusterSetMemory) clear() {
	self.episodeUnits = map[string]bool{}
	self.episodeStartTime = time.Time{}
	self.lastFailureTime = time.Time{}
	self.episodeOffline = false
	self.atRisk = false
}

// One set's state at one run, with why.
type fileClusterSetVerdict struct {
	state string
	// hosts and drives failing now
	failingUnits []string
	// drives rebuilding now
	rebuildingUnits []string
	// hosts offline past the RTO
	longOfflineHosts []string
	// what the episode holds
	episodeUnits []string
	debt         *fileClusterHealDebt
	// queue entries lost with every drive online
	debtLost bool
}

// Holds each set's memory across runs. Safe for concurrent use.
type fileClusterStateProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
	// nil reads the fork's series through query; set, it stands in for them
	healDebt fileClusterHealDebtSource

	stateLock sync.Mutex
	// by cluster, then set
	clusterSetMemories map[string]map[string]*fileClusterSetMemory
}

func newFileClusterStateProbe() *fileClusterStateProbe {
	return &fileClusterStateProbe{clusterSetMemories: map[string]map[string]*fileClusterSetMemory{}}
}

func (self *fileClusterStateProbe) id() string             { return fileClusterStateProbeId }
func (self *fileClusterStateProbe) tier() string           { return tierPage }
func (self *fileClusterStateProbe) cadence() time.Duration { return 30 * time.Second }

func (self *fileClusterStateProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, fileClusterStateProbeId, fileClusterStateClassUnobservable); ok {
		return []finding{gate}, nil
	}
	now := env.now()
	// one query reads every cluster's heal debt. A query that fails leaves every debt unknown, never zero.
	var debtSamples []*sidecarStorageSample
	var debtErr error
	if self.healDebt == nil {
		query := self.query
		if query == nil {
			query = sidecarStorageMimirQuery
		}
		debtSamples, debtErr = query(ctx, env, fileClusterHealDebtQuery(env.cfg.env, settings.MetricFreshness))
	}
	debtGauges, _ := settings.Gauges.(FileClusterHealDebtGauges)
	findings := []finding{}
	for _, clusterId := range settings.AdminSource.ClusterIds() {
		info, err := settings.AdminSource.ReadAdminInfo(ctx, clusterId)
		if err != nil {
			findings = append(findings, self.stateFinding(settings, fileClusterStateClassUnobservable, clusterId, "admin-info",
				"the cluster's admin info did not read, so its sets' states are unknown", err.Error()))
			if settings.Gauges != nil {
				settings.Gauges.PublishSetStates(clusterId, self.unknownSetStates(clusterId))
			}
			if debtGauges != nil {
				debtGauges.PublishQueueDebts(clusterId, nil)
			}
			continue
		}
		var debts map[string]*fileClusterHealDebt
		var queues map[string]FileClusterQueueDebt
		switch {
		case self.healDebt != nil:
			// an error leaves every debt unknown, never zero
			debts, _ = self.healDebt(ctx, env, clusterId)
		case debtErr == nil:
			debts, queues = fileClusterHealDebts(clusterId, info, debtSamples)
		}
		clusterFindings, setStates := self.evaluate(settings, now, clusterId, info, debts)
		findings = append(findings, clusterFindings...)
		if settings.Gauges != nil {
			settings.Gauges.PublishSetStates(clusterId, setStates)
		}
		if debtGauges != nil && self.healDebt == nil {
			debtGauges.PublishQueueDebts(clusterId, queues)
		}
	}
	return findings, nil
}

// The set's name in a series and a target: its pool and set index.
func fileClusterSetName(poolIndex int, setIndex int) string {
	return fmt.Sprintf("%d-%d", poolIndex, setIndex)
}

// A drive's unit name: its pool, set and disk index, which name no host.
func fileClusterDriveUnit(drive *server.FileClusterDriveInfo) string {
	return fmt.Sprintf("drive %d-%d-%d", drive.PoolIndex, drive.SetIndex, drive.DiskIndex)
}

// Every set the cluster's memory holds, unknown: a cluster whose admin info did not read.
func (self *fileClusterStateProbe) unknownSetStates(clusterId string) map[string]string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	setStates := map[string]string{}
	for set := range self.clusterSetMemories[clusterId] {
		setStates[set] = fileClusterStateUnknown
	}
	return setStates
}

// Every set's findings and state from one admin info read.
func (self *fileClusterStateProbe) evaluate(
	settings *SidecarStorageSettings,
	now time.Time,
	clusterId string,
	info *server.FileClusterAdminInfo,
	debts map[string]*fileClusterHealDebt,
) ([]finding, map[string]string) {
	hostOnlines := map[string]bool{}
	for _, host := range info.Hosts {
		hostOnlines[host.Endpoint] = host.State == fileClusterHostOnline
	}
	setDrives := map[string][]*server.FileClusterDriveInfo{}
	for _, drive := range info.Drives {
		set := fileClusterSetName(drive.PoolIndex, drive.SetIndex)
		setDrives[set] = append(setDrives[set], drive)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	setMemories, ok := self.clusterSetMemories[clusterId]
	if !ok {
		setMemories = map[string]*fileClusterSetMemory{}
		self.clusterSetMemories[clusterId] = setMemories
	}
	for set := range setMemories {
		if _, listed := setDrives[set]; !listed {
			delete(setMemories, set)
		}
	}
	sets := []string{}
	for set := range setDrives {
		sets = append(sets, set)
	}
	slices.Sort(sets)

	findings := []finding{}
	setStates := map[string]string{}
	for _, set := range sets {
		memory, ok := setMemories[set]
		if !ok {
			memory = newFileClusterSetMemory()
			setMemories[set] = memory
		}
		verdict := evaluateFileClusterSet(settings, now, memory, setDrives[set], hostOnlines, debts[set])
		setStates[set] = verdict.state
		findings = append(findings, self.setFindings(settings, clusterId+"/"+set, verdict)...)
	}
	return findings, setStates
}

// One set's state by §19.6's table, moving its memory (see the file header). debt may be nil, which reads
// unknown.
func evaluateFileClusterSet(
	settings *SidecarStorageSettings,
	now time.Time,
	memory *fileClusterSetMemory,
	drives []*server.FileClusterDriveInfo,
	hostOnlines map[string]bool,
	debt *fileClusterHealDebt,
) *fileClusterSetVerdict {
	if debt == nil {
		debt = &fileClusterHealDebt{}
	}
	failingUnits := map[string]bool{}
	rebuildingUnits := map[string]bool{}
	offlineHosts := map[string]bool{}
	for _, drive := range drives {
		// a host that does not answer takes every drive of the set it carries with it, as one failure
		if online, listed := hostOnlines[drive.HostEndpoint]; listed && !online {
			failingUnits["host "+drive.HostEndpoint] = true
			offlineHosts[drive.HostEndpoint] = true
			continue
		}
		unit := fileClusterDriveUnit(drive)
		switch {
		case drive.State != fileClusterDriveOk:
			failingUnits[unit] = true
		case drive.Healing:
			rebuildingUnits[unit] = true
		}
	}

	for host := range memory.hostOfflineTimes {
		if !offlineHosts[host] {
			delete(memory.hostOfflineTimes, host)
		}
	}
	longOfflineHosts := []string{}
	for host := range offlineHosts {
		since, ok := memory.hostOfflineTimes[host]
		if !ok {
			since = now
			memory.hostOfflineTimes[host] = now
		}
		if settings.HostOfflineAtRisk <= now.Sub(since) {
			longOfflineHosts = append(longOfflineHosts, fmt.Sprintf("%s offline %s", host, now.Sub(since).Round(time.Minute)))
		}
	}
	slices.Sort(longOfflineHosts)

	// debt the queue holds before any unit failed is a cause of its own; queued writes during an outage are
	// that outage's
	queued := debt.known && (0 < debt.depth || debt.lost)
	if len(memory.episodeUnits) == 0 && (queued || 0 < len(failingUnits) || 0 < len(rebuildingUnits)) {
		memory.episodeStartTime = now
	}
	if queued && len(memory.episodeUnits) == 0 && len(failingUnits) == 0 && len(rebuildingUnits) == 0 {
		memory.episodeUnits["partial-write queue"] = true
	}
	for unit := range failingUnits {
		memory.episodeUnits[unit] = true
	}
	for unit := range rebuildingUnits {
		memory.episodeUnits[unit] = true
	}
	if 0 < len(failingUnits) || 0 < len(rebuildingUnits) {
		memory.lastFailureTime = now
	}
	if 0 < len(failingUnits) {
		memory.episodeOffline = true
	}
	if (0 < len(failingUnits) && 2 <= len(memory.episodeUnits)) || 0 < len(longOfflineHosts) {
		memory.atRisk = true
	}

	// the episode ends on a full heal that began after its last failure, or, with nothing offline in it, on a
	// drained queue that lost nothing. A heal that began before a drive returned or was replaced healed part
	// of the set without it, so its end ends nothing.
	if len(failingUnits) == 0 && len(rebuildingUnits) == 0 && 0 < len(memory.episodeUnits) && debt.known {
		healed := !debt.fullHealStartTime.IsZero() && !debt.fullHealEndTime.IsZero() &&
			debt.fullHealStartTime.After(memory.lastFailureTime) && debt.fullHealStartTime.After(memory.episodeStartTime)
		drained := !memory.episodeOffline && debt.depth == 0 && !debt.lost
		if healed || drained {
			memory.clear()
		}
	}

	verdict := &fileClusterSetVerdict{
		failingUnits:     fileClusterSortedUnits(failingUnits),
		rebuildingUnits:  fileClusterSortedUnits(rebuildingUnits),
		longOfflineHosts: longOfflineHosts,
		episodeUnits:     fileClusterSortedUnits(memory.episodeUnits),
		debt:             debt,
		debtLost:         debt.known && debt.lost && len(failingUnits) == 0,
	}
	switch {
	case memory.atRisk:
		verdict.state = fileClusterStateAtRisk
	case 0 < len(memory.episodeUnits):
		verdict.state = fileClusterStateDegraded
	case !debt.known:
		verdict.state = fileClusterStateUnknown
	default:
		verdict.state = fileClusterStateProtected
	}
	return verdict
}

// The map's keys, sorted.
func fileClusterSortedUnits(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// A finding of class at target, sustained and tiered as the class is.
func (self *fileClusterStateProbe) stateFinding(settings *SidecarStorageSettings, class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  fileClusterStateProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		playbook: "SIGNALS.md §29.1; §19.10's file cluster row (DESIGN.md)",
	}
	switch class {
	case fileClusterStateClassAtRisk:
		finding.tier = tierPage
		finding.sustain = settings.PageSustain
		finding.action = "Bring the failed host or drives back, or within 1 h of the page move a host's drives into the cold spare chassis or fit spare drives (§19.10); then run a full heal of the whole cluster (mc admin heal --recursive <alias>)."
	case fileClusterStateClassDegraded:
		finding.sustain = settings.WarnSustain
		finding.action = "Heal: once every drive is back run a full heal of the whole cluster (mc admin heal --recursive <alias>); the set stays degraded until one that began after the return ends with nothing left unhealed."
	case fileClusterStateClassDebtLost:
		finding.sustain = 1
		finding.action = "Start one full heal of the whole cluster (mc admin heal --recursive <alias>): MinIO's partial-write queue lost entries, so only a full heal restores the shards they named."
	case fileClusterStateClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	}
	return finding
}

// One set's findings from its verdict.
func (self *fileClusterStateProbe) setFindings(settings *SidecarStorageSettings, target string, verdict *fileClusterSetVerdict) []finding {
	list := func(units []string) string {
		if len(units) == 0 {
			return "-"
		}
		return strings.Join(units, ", ")
	}
	observed := fmt.Sprintf("state=%s failing=[%s] rebuilding=[%s] long_offline=[%s] episode=[%s] heal_debt_known=%t",
		verdict.state, list(verdict.failingUnits), list(verdict.rebuildingUnits), list(verdict.longOfflineHosts), list(verdict.episodeUnits), verdict.debt.known)
	if verdict.debt.known {
		fullHeal := "none"
		if !verdict.debt.fullHealEndTime.IsZero() {
			fullHeal = verdict.debt.fullHealStartTime.UTC().Format(time.RFC3339) + ".." + verdict.debt.fullHealEndTime.UTC().Format(time.RFC3339)
		}
		observed += fmt.Sprintf(" mrf_depth=%g mrf_lost=%t full_heal=%s", verdict.debt.depth, verdict.debt.lost, fullHeal)
	}
	findings := []finding{}
	switch verdict.state {
	case fileClusterStateAtRisk:
		findings = append(findings, self.stateFinding(settings, fileClusterStateClassAtRisk, target, "",
			"an erasure set is at_risk: degraded and a further drive or host failed, or a host is offline past the 4 h RTO", observed))
	case fileClusterStateDegraded:
		findings = append(findings,
			healthyFinding(fileClusterStateProbeId, tierPage, fileClusterStateClassAtRisk, target),
			self.stateFinding(settings, fileClusterStateClassDegraded, target, "",
				"an erasure set is degraded: a drive or host offline, a drive rebuilding, or a heal pending", observed))
	case fileClusterStateUnknown:
		findings = append(findings, self.stateFinding(settings, fileClusterStateClassUnobservable, target, "heal-debt",
			"every drive is online, and whether a heal is pending is unknown: a host's partial-write queue or the cluster's last full heal did not read from Mimir", observed))
	default:
		findings = append(findings,
			healthyFinding(fileClusterStateProbeId, tierPage, fileClusterStateClassAtRisk, target),
			healthyFinding(fileClusterStateProbeId, tierWarn, fileClusterStateClassDegraded, target))
	}
	if verdict.debtLost {
		findings = append(findings, self.stateFinding(settings, fileClusterStateClassDebtLost, target, "",
			"MinIO's partial-write queue lost entries, dropped when full or in an unclean start, with every drive online", observed))
	} else if verdict.debt.known {
		findings = append(findings, healthyFinding(fileClusterStateProbeId, tierWarn, fileClusterStateClassDebtLost, target))
	}
	return findings
}
