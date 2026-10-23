// SIGNALS.md §2.4h (contract-partition-runway): once the contract tables are
// partitioned there is no default partition, so an insert past the last
// partition fails and contract creation stops. Pages while any contract table
// has fewer than two covered weeks after the current one.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

const contractPartitionRunwayProbeId = "pg/contract-partition-runway"

func NewContractPartitionRunwaySignal() Signal {
	return NewContractPartitionRunwaySignalWithSettings(DefaultContractPartitionRunwaySettings())
}

// The signal with explicit thresholds.
func NewContractPartitionRunwaySignalWithSettings(settings *ContractPartitionRunwaySettings) Signal {
	return &signalAdapter{
		number: "2.4h", key: "contract-partition-runway", name: "Contract partition runway",
		probe: &contractPartitionRunwayProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4h.
type ContractPartitionRunwaySettings struct {
	Catalog ContractPartitionCatalogSettings
	Cadence time.Duration
	// Page when a table has fewer covered weeks than this after the current
	// one. The maintenance task keeps server.ContractPartitionAheadWeeks.
	MinimumFutureWeeks int
	// Count covered weeks up to this many; a MAXVALUE or default partition
	// covers every week.
	CountedWeeks int
	Sustain      int
	// An unknown runway pages when the last complete observation of this
	// signal instance, at most UnknownRiskMemory old, had at most this many
	// future weeks; otherwise it warns (SIGNALS.md §7, 2026-10-22 rule 1).
	UnknownPageFutureWeeks int
	UnknownRiskMemory      time.Duration
}

func DefaultContractPartitionRunwaySettings() *ContractPartitionRunwaySettings {
	return &ContractPartitionRunwaySettings{
		Catalog:                DefaultContractPartitionCatalogSettings(),
		Cadence:                15 * time.Minute,
		MinimumFutureWeeks:     2,
		CountedWeeks:           26,
		Sustain:                2,
		UnknownPageFutureWeeks: 2,
		UnknownRiskMemory:      7 * 24 * time.Hour,
	}
}

// The memory belongs to this signal instance and survives between runs, so
// an unknown runway keeps the severity of a thin runway measured earlier. It
// is process-local and starts empty at every watcher start.
type contractPartitionRunwayProbe struct {
	settings *ContractPartitionRunwaySettings

	stateLock  sync.Mutex
	known      bool
	knownWeeks int
	knownAt    time.Time
}

func (self *contractPartitionRunwayProbe) id() string             { return contractPartitionRunwayProbeId }
func (self *contractPartitionRunwayProbe) tier() string           { return tierPage }
func (self *contractPartitionRunwayProbe) cadence() time.Duration { return self.settings.Cadence }
func (self *contractPartitionRunwayProbe) runBudget() time.Duration {
	return self.settings.Catalog.RunBudget
}

func (self *contractPartitionRunwayProbe) remember(futureWeeks int, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.known, self.knownWeeks, self.knownAt = true, futureWeeks, at
}

func (self *contractPartitionRunwayProbe) forget() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.known = false
}

// The tier an unknown runway cannot rule out, and the basis for it.
func (self *contractPartitionRunwayProbe) unknownTier(now time.Time) (string, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	settings := self.settings
	if !self.known || settings.UnknownRiskMemory < now.Sub(self.knownAt) {
		return tierWarn, "last_complete_future_weeks=unknown"
	}
	basis := fmt.Sprintf("last_complete_future_weeks=%d last_complete_age_h=%.1f", self.knownWeeks, now.Sub(self.knownAt).Hours())
	if self.knownWeeks <= settings.UnknownPageFutureWeeks {
		return tierPage, basis
	}
	return tierWarn, basis
}

// The weeks of one table after the current week that route to a partition.
type contractPartitionTableRunway struct {
	table          string
	currentCovered bool
	futureWeeks    int
	// the last consecutive covered week, zero when even the next is not
	lastCovered time.Time
}

func (self *contractPartitionRunwayProbe) tableRunway(table *contractPartitionTable, currentWeek time.Time) contractPartitionTableRunway {
	covered := func(weekStart time.Time) bool {
		for _, partition := range table.partitions {
			if partition.coversWeek(weekStart) {
				return true
			}
		}
		return false
	}
	runway := contractPartitionTableRunway{table: table.name, currentCovered: covered(currentWeek)}
	for week := 1; week <= self.settings.CountedWeeks; week++ {
		weekStart := currentWeek.Add(time.Duration(week) * contractPartitionWeek)
		if !covered(weekStart) {
			break
		}
		runway.futureWeeks, runway.lastCovered = week, weekStart
	}
	return runway
}

func (self *contractPartitionRunwayProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	settings := self.settings
	catalog, primary, reason, sourceErr, lifecycleErr := readContractPartitionCatalog(ctx, env, settings.Catalog)
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	now := env.now().UTC()
	target := primary + "/contract-partitions"
	if reason != "" {
		return []finding{self.unknown(target, reason, sourceErr, now)}, nil
	}
	healthy := []finding{
		healthyFinding(contractPartitionRunwayProbeId, tierPage, "contract-partition-runway", target),
		healthyFinding(contractPartitionRunwayProbeId, tierWarn, "contract-partition-runway-unobservable", target),
	}
	if !catalog.partitioned() {
		// Plain tables accept any contract id; there is no runway to keep.
		self.forget()
		return healthy, nil
	}
	for _, table := range catalog.tables {
		if table.kind != "p" {
			return []finding{self.unknown(target, "mixed-contract-layout", nil, now)}, nil
		}
	}
	currentWeek := server.ContractPartitionWeekStart(catalog.observed)
	runways := []contractPartitionTableRunway{}
	worst := -1
	for _, table := range catalog.tables {
		runway := self.tableRunway(table, currentWeek)
		runways = append(runways, runway)
		index := len(runways) - 1
		if worst < 0 || !runway.currentCovered && runways[worst].currentCovered ||
			runway.currentCovered == runways[worst].currentCovered && runway.futureWeeks < runways[worst].futureWeeks {
			worst = index
		}
	}
	thin := runways[worst]
	minimum := thin.futureWeeks
	if !thin.currentCovered {
		minimum = 0
	}
	self.remember(minimum, now)
	if thin.currentCovered && settings.MinimumFutureWeeks <= thin.futureWeeks {
		return healthy, nil
	}

	weeks := []string{}
	uncovered := []string{}
	for _, runway := range runways {
		weeks = append(weeks, fmt.Sprintf("%s=%d", runway.table, runway.futureWeeks))
		if !runway.currentCovered {
			uncovered = append(uncovered, runway.table)
		}
	}
	lastCovered := "none"
	if !thin.lastCovered.IsZero() {
		lastCovered = thin.lastCovered.Format(time.DateOnly)
	}
	symptom := fmt.Sprintf("Contract table %s has %d covered week(s) after the current week; inserts past its last partition fail and contract creation stops", thin.table, thin.futureWeeks)
	if !thin.currentCovered {
		symptom = fmt.Sprintf("No partition of contract table %s accepts contract ids of the current week; contract creation is failing now", thin.table)
	}
	return []finding{
		{
			probeId: contractPartitionRunwayProbeId, tier: tierPage, class: "contract-partition-runway", target: target, frame: thin.table, sustain: settings.Sustain,
			symptom:   symptom,
			mechanism: fmt.Sprintf("The contract tables are range-partitioned by contract_id with no default partition (server/CONTRACT-PARTITION.md), so an insert whose id lies past the last partition fails. The maintenance task (maintain_contract_partitions) creates each table's weekly partitions %d weeks ahead; a short runway means it has not run, or its creates keep failing (a lock timeout retries on the next run).", server.ContractPartitionAheadWeeks),
			baseline:  fmt.Sprintf("Every contract table accepts the current week and at least %d consecutive weeks after it, through weekly partitions or, before the cutover week, the legacy partition.", settings.MinimumFutureWeeks),
			observed: fmt.Sprintf("current_week=%s worst_table=%s worst_future_weeks=%d worst_last_covered_week=%s future_weeks=[%s] current_week_uncovered=[%s] minimum_future_weeks=%d",
				currentWeek.Format(time.DateOnly), thin.table, thin.futureWeeks, lastCovered, strings.Join(weeks, " "), strings.Join(uncovered, " "), settings.MinimumFutureWeeks),
			evidence: "One primary-guarded read-only catalog query of the attached partitions of " + contractPartitionTableList() + " and their bounds (pg_inherits, pg_get_expr(relpartbound)); a week counts as covered when one partition's range holds every id of it.",
			context:  "Weeks before the cutover week route to the legacy partition, so between the swap and the cutover week they count as covered without a weekly partition. The current week uncovered means contracts are already being refused.",
			action:   "Check the contract partition maintenance task's recent runs and errors (task canaries, [cp] log lines) and the lock waits behind its creates. If it cannot run in time, have the owner run its partition creation (server.MaintainContractPartitions) rather than writing DDL by hand: a hand-made partition must match its week's bounds, columns, storage settings and TRUNCATE guards exactly. Never add a default partition.",
			verify:   fmt.Sprintf("Two consecutive complete observations show every table covering the current week and at least %d weeks after it.", settings.MinimumFutureWeeks),
			playbook: "SIGNALS.md §2.4h",
		},
		healthyFinding(contractPartitionRunwayProbeId, tierWarn, "contract-partition-runway-unobservable", target),
	}, nil
}

// Unknown is not healthy for a safety property: the tier is the one a thin
// runway measured earlier cannot rule out.
func (self *contractPartitionRunwayProbe) unknown(target, reason string, err error, now time.Time) finding {
	tier, basis := self.unknownTier(now)
	observed := "reason=" + reason + " " + basis
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: contractPartitionRunwayProbeId, tier: tier, class: "contract-partition-runway-unobservable", target: target, sustain: 2,
		symptom:   "The contract partition runway is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing or a partition bound could not be established, so whether the next weeks' contract ids have partitions is unknown.",
		baseline:  fmt.Sprintf("One configured primary answers the guarded catalog read with a complete frame and parseable bounds. An unknown runway pages when the last complete observation, at most %.0f days old, had at most %d future weeks; otherwise it warns.", self.settings.UnknownRiskMemory.Hours()/24, self.settings.UnknownPageFutureWeeks),
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A runway alert raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read. While a paging unknown lasts, list the contract partitions on the primary directly (pg_inherits of transfer_contract) and treat the runway as its last measured value."),
		verify:    "A complete read restores the runway; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4h and §7",
	}
}
