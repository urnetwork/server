// SIGNALS.md §2.4p (partition-runway): every family of the partition registry
// (server/db_partition_registry.go) that names no dedicated probe has no
// DEFAULT partition for its ranges, so an insert past its last partition
// fails. Pages while a partitioned family's table covers fewer ranges after
// the current one than the family declares, or does not cover the current
// range. The contract tables' runway is §2.4h's and network_ping's §2.19c's.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

const partitionRunwayProbeId = "pg/partition-runway"

func NewPartitionRunwaySignal() Signal {
	return NewPartitionRunwaySignalWithSettings(DefaultPartitionRunwaySettings())
}

// The signal with explicit thresholds.
func NewPartitionRunwaySignalWithSettings(settings *PartitionRunwaySettings) Signal {
	return &signalAdapter{
		number: "2.4p", key: "partition-runway", name: "Partition registry runway",
		probe: &partitionRunwayProbe{settings: settings, known: map[string]partitionRunwayMemory{}},
	}
}

// The thresholds of SIGNALS.md §2.4p. Each family's own line is its
// declaration (MonitorRunwayRanges, else min(2, AheadRanges)).
type PartitionRunwaySettings struct {
	Catalog PartitionRegistryCatalogSettings
	Cadence time.Duration
	Sustain int
	// An unknown runway pages when the last complete observation of the
	// family, at most UnknownRiskMemory old, was at most its line; otherwise
	// it warns (SIGNALS.md §7, 2026-10-22 rule 1).
	UnknownRiskMemory time.Duration
	// nil watches every family that names no dedicated probe; a test names
	// its own
	Families func() []*server.PartitionFamily
}

func DefaultPartitionRunwaySettings() *PartitionRunwaySettings {
	return &PartitionRunwaySettings{
		Catalog:           DefaultPartitionRegistryCatalogSettings(),
		Cadence:           15 * time.Minute,
		Sustain:           2,
		UnknownRiskMemory: 7 * 24 * time.Hour,
	}
}

// A family's last complete observation.
type partitionRunwayMemory struct {
	futureRanges int
	at           time.Time
}

// The memory belongs to this signal instance and survives between runs, so
// an unknown runway keeps the severity of a thin runway measured earlier. It
// is process-local and starts empty at every watcher start.
type partitionRunwayProbe struct {
	settings *PartitionRunwaySettings

	stateLock sync.Mutex
	known     map[string]partitionRunwayMemory
}

func (self *partitionRunwayProbe) id() string             { return partitionRunwayProbeId }
func (self *partitionRunwayProbe) tier() string           { return tierPage }
func (self *partitionRunwayProbe) cadence() time.Duration { return self.settings.Cadence }
func (self *partitionRunwayProbe) runBudget() time.Duration {
	return self.settings.Catalog.RunBudget
}

func (self *partitionRunwayProbe) families() []*server.PartitionFamily {
	if self.settings.Families != nil {
		return self.settings.Families()
	}
	return partitionRegistryRunwayFamilies()
}

func (self *partitionRunwayProbe) remember(family string, futureRanges int, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.known[family] = partitionRunwayMemory{futureRanges: futureRanges, at: at}
}

func (self *partitionRunwayProbe) forget(family string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.known, family)
}

// The tier an unknown runway of the family cannot rule out, and the basis.
func (self *partitionRunwayProbe) unknownTier(family *server.PartitionFamily, now time.Time) (string, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	memory, ok := self.known[family.Name]
	if !ok || self.settings.UnknownRiskMemory < now.Sub(memory.at) {
		return tierWarn, "last_complete_future_ranges=unknown"
	}
	basis := fmt.Sprintf("last_complete_future_ranges=%d last_complete_age_h=%.1f", memory.futureRanges, now.Sub(memory.at).Hours())
	if memory.futureRanges <= partitionRegistryMinimumRunway(family) {
		return tierPage, basis
	}
	return tierWarn, basis
}

// One table's covered ranges.
type partitionRunwayTable struct {
	table          string
	currentCovered bool
	futureRanges   int
}

func (self *partitionRunwayProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	settings := self.settings
	families := self.families()
	catalog, primary, reason, sourceErr, lifecycleErr := readPartitionRegistryCatalog(ctx, env, settings.Catalog, families)
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	now := env.now().UTC()
	findings := []finding{}
	if reason != "" {
		for _, family := range families {
			findings = append(findings, self.unknown(partitionRegistryTarget(primary, family), family, reason, sourceErr, now))
		}
		return findings, nil
	}
	for _, familyCatalog := range catalog.families {
		family := familyCatalog.family
		target := partitionRegistryTarget(primary, family)
		healthy := []finding{
			healthyFinding(partitionRunwayProbeId, tierPage, "partition-runway", target),
			healthyFinding(partitionRunwayProbeId, tierWarn, "partition-runway-unobservable", target),
		}
		if !familyCatalog.partitioned() {
			// a plain table accepts every key; there is no runway to keep
			self.forget(family.Name)
			findings = append(findings, healthy...)
			continue
		}
		if familyCatalog.mixed() {
			findings = append(findings, self.unknown(target, family, "mixed-family-layout", nil, now))
			continue
		}
		minimum := partitionRegistryMinimumRunway(family)
		tables := []partitionRunwayTable{}
		worst := -1
		for _, table := range family.Tables {
			currentCovered, futureRanges := family.Runway(familyCatalog.leaves[table], catalog.observed)
			tables = append(tables, partitionRunwayTable{table: table, currentCovered: currentCovered, futureRanges: futureRanges})
			index := len(tables) - 1
			if worst < 0 || !tables[index].currentCovered && tables[worst].currentCovered ||
				tables[index].currentCovered == tables[worst].currentCovered && tables[index].futureRanges < tables[worst].futureRanges {
				worst = index
			}
		}
		thin := tables[worst]
		measured := thin.futureRanges
		if !thin.currentCovered {
			measured = 0
		}
		self.remember(family.Name, measured, now)
		if thin.currentCovered && minimum <= thin.futureRanges {
			findings = append(findings, healthy...)
			continue
		}
		ranges := []string{}
		uncovered := []string{}
		for _, table := range tables {
			ranges = append(ranges, fmt.Sprintf("%s=%d", table.table, table.futureRanges))
			if !table.currentCovered {
				uncovered = append(uncovered, table.table)
			}
		}
		current := family.RangeAt(catalog.observed)
		symptom := fmt.Sprintf("Partitioned table %s of the %s family has %d covered %s range(s) after the current one; inserts past its last partition fail", thin.table, family.Name, thin.futureRanges, family.Granularity)
		if !thin.currentCovered {
			symptom = fmt.Sprintf("No partition of %s (the %s family) accepts keys of the current %s range; its inserts are failing now", thin.table, family.Name, family.Granularity)
		}
		findings = append(findings,
			finding{
				probeId: partitionRunwayProbeId, tier: tierPage, class: "partition-runway", target: target, frame: thin.table, sustain: settings.Sustain,
				symptom:   symptom,
				mechanism: fmt.Sprintf("The family is range-partitioned by %s with no DEFAULT partition for its ranges (server/db_partition_registry.go), so an insert past the last partition fails. The registry's runway task (maintain_partition_runway) creates each table's %s ranges %d ahead every 10 minutes; a short runway means it has not run, or its creates keep failing (a lock timeout retries on the next run).", family.KeyColumn, family.Granularity, family.AheadRanges),
				baseline:  fmt.Sprintf("Every table of the family covers the current range and at least %d consecutive ranges after it.", minimum),
				observed: fmt.Sprintf("family=%s current_range=%s worst_table=%s worst_future_ranges=%d future_ranges=[%s] current_range_uncovered=[%s] minimum_future_ranges=%d",
					family.Name, current.Lower.Format(time.DateOnly), thin.table, thin.futureRanges, strings.Join(ranges, " "), strings.Join(uncovered, " "), minimum),
				evidence: "One primary-guarded read-only catalog query of the attached partitions of the registry's families and their bounds (pg_inherits, pg_get_expr(relpartbound)); a range counts as covered when one partition other than DEFAULT holds every key of it.",
				context:  "Family: " + family.Description + ". The current range uncovered means its inserts are already being refused; a writer that creates its own range on demand (EnsureRangeFor) still fails when the create cannot take its lock.",
				action:   "Check the runway task's recent runs and errors (task canaries, [partitions] log lines) and the lock waits behind its creates. If it cannot run in time, have the owner run the registry's runway (server.MaintainPartitionRunways) rather than writing DDL by hand: a partition must have its family's canonical name and bounds. Never add a DEFAULT partition the family does not declare.",
				verify:   fmt.Sprintf("Two consecutive complete observations show every table of the family covering the current range and at least %d ranges after it.", minimum),
				playbook: "SIGNALS.md §2.4p",
			},
			healthyFinding(partitionRunwayProbeId, tierWarn, "partition-runway-unobservable", target),
		)
	}
	return findings, nil
}

// Unknown is not healthy for a safety property: the tier is the one a thin
// runway measured earlier cannot rule out.
func (self *partitionRunwayProbe) unknown(target string, family *server.PartitionFamily, reason string, err error, now time.Time) finding {
	tier, basis := self.unknownTier(family, now)
	observed := "family=" + family.Name + " reason=" + reason + " " + basis
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: partitionRunwayProbeId, tier: tier, class: "partition-runway-unobservable", target: target, sustain: 2,
		symptom:   "The partition runway of the " + family.Name + " family is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing, a partition bound or the family's layout could not be established, so whether the next ranges have partitions is unknown.",
		baseline:  fmt.Sprintf("One configured primary answers the guarded catalog read with a complete frame and parseable bounds. An unknown runway pages when the last complete observation, at most %.0f days old, was at the family's line; otherwise it warns.", self.settings.UnknownRiskMemory.Hours()/24),
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A runway alert raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read. While a paging unknown lasts, list the family's partitions on the primary directly (pg_inherits of its tables) and treat the runway as its last measured value."),
		verify:    "A complete read restores the runway; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4p and §7",
	}
}
