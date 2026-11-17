// SIGNALS.md §2.4h (contract-partition-runway): once the contract tables are
// partitioned there is no default partition, so an insert past the last
// partition fails and contract creation stops. Pages while any contract table
// has fewer than two covered days after the current one.
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
	// Page when a table has fewer covered days than this after the current
	// one. The maintenance task keeps server.ContractPartitionAheadDays.
	MinimumFutureDays int
	// Count covered days up to this many; a MAXVALUE or default partition
	// covers every day, and before the cutover day the legacy partition covers
	// every day up to it.
	CountedDays int
	Sustain     int
	// An unknown runway pages when the last complete observation of this
	// signal instance, at most UnknownRiskMemory old, had at most this many
	// future days; otherwise it warns (SIGNALS.md §7, 2026-10-22 rule 1).
	UnknownPageFutureDays int
	UnknownRiskMemory     time.Duration
}

func DefaultContractPartitionRunwaySettings() *ContractPartitionRunwaySettings {
	return &ContractPartitionRunwaySettings{
		Catalog:               DefaultContractPartitionCatalogSettings(),
		Cadence:               15 * time.Minute,
		MinimumFutureDays:     2,
		CountedDays:           90,
		Sustain:               2,
		UnknownPageFutureDays: 2,
		UnknownRiskMemory:     7 * 24 * time.Hour,
	}
}

// The memory belongs to this signal instance and survives between runs, so
// an unknown runway keeps the severity of a thin runway measured earlier. It
// is process-local and starts empty at every watcher start.
type contractPartitionRunwayProbe struct {
	settings *ContractPartitionRunwaySettings

	stateLock sync.Mutex
	known     bool
	knownDays int
	knownAt   time.Time
}

func (self *contractPartitionRunwayProbe) id() string             { return contractPartitionRunwayProbeId }
func (self *contractPartitionRunwayProbe) tier() string           { return tierPage }
func (self *contractPartitionRunwayProbe) cadence() time.Duration { return self.settings.Cadence }
func (self *contractPartitionRunwayProbe) runBudget() time.Duration {
	return self.settings.Catalog.RunBudget
}

func (self *contractPartitionRunwayProbe) remember(futureDays int, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.known, self.knownDays, self.knownAt = true, futureDays, at
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
		return tierWarn, "last_complete_future_days=unknown"
	}
	basis := fmt.Sprintf("last_complete_future_days=%d last_complete_age_h=%.1f", self.knownDays, now.Sub(self.knownAt).Hours())
	if self.knownDays <= settings.UnknownPageFutureDays {
		return tierPage, basis
	}
	return tierWarn, basis
}

// The days of one table after the current day that route to a partition.
type contractPartitionTableRunway struct {
	table          string
	currentCovered bool
	futureDays     int
	// the last consecutive covered day, zero when even the next is not
	lastCovered time.Time
}

func (self *contractPartitionRunwayProbe) tableRunway(table *contractPartitionTable, currentDay time.Time) contractPartitionTableRunway {
	covered := func(dayStart time.Time) bool {
		for _, partition := range table.partitions {
			if partition.coversDay(dayStart) {
				return true
			}
		}
		return false
	}
	runway := contractPartitionTableRunway{table: table.name, currentCovered: covered(currentDay)}
	for day := 1; day <= self.settings.CountedDays; day++ {
		dayStart := currentDay.Add(time.Duration(day) * contractPartitionDay)
		if !covered(dayStart) {
			break
		}
		runway.futureDays, runway.lastCovered = day, dayStart
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
	currentDay := server.ContractPartitionDayStart(catalog.observed)
	runways := []contractPartitionTableRunway{}
	worst := -1
	for _, table := range catalog.tables {
		runway := self.tableRunway(table, currentDay)
		runways = append(runways, runway)
		index := len(runways) - 1
		if worst < 0 || !runway.currentCovered && runways[worst].currentCovered ||
			runway.currentCovered == runways[worst].currentCovered && runway.futureDays < runways[worst].futureDays {
			worst = index
		}
	}
	thin := runways[worst]
	minimum := thin.futureDays
	if !thin.currentCovered {
		minimum = 0
	}
	self.remember(minimum, now)
	if thin.currentCovered && settings.MinimumFutureDays <= thin.futureDays {
		return healthy, nil
	}

	days := []string{}
	uncovered := []string{}
	for _, runway := range runways {
		days = append(days, fmt.Sprintf("%s=%d", runway.table, runway.futureDays))
		if !runway.currentCovered {
			uncovered = append(uncovered, runway.table)
		}
	}
	lastCovered := "none"
	if !thin.lastCovered.IsZero() {
		lastCovered = thin.lastCovered.Format(time.DateOnly)
	}
	symptom := fmt.Sprintf("Contract table %s has %d covered day(s) after the current day; inserts past its last partition fail and contract creation stops", thin.table, thin.futureDays)
	if !thin.currentCovered {
		symptom = fmt.Sprintf("No partition of contract table %s accepts contract ids of the current day; contract creation is failing now", thin.table)
	}
	return []finding{
		{
			probeId: contractPartitionRunwayProbeId, tier: tierPage, class: "contract-partition-runway", target: target, frame: thin.table, sustain: settings.Sustain,
			symptom:   symptom,
			mechanism: fmt.Sprintf("The contract tables are range-partitioned by contract_id with no default partition (server/CONTRACT-PARTITION.md), so an insert whose id lies past the last partition fails. The maintenance task (maintain_contract_partitions) creates each table's daily partitions %d days ahead; a short runway means it has not run, or its creates keep failing (a lock timeout retries on the next run).", server.ContractPartitionAheadDays),
			baseline:  fmt.Sprintf("Every contract table accepts the current day and at least %d consecutive days after it, through daily partitions or, before the cutover day, the legacy partition.", settings.MinimumFutureDays),
			observed: fmt.Sprintf("current_day=%s worst_table=%s worst_future_days=%d worst_last_covered_day=%s future_days=[%s] current_day_uncovered=[%s] minimum_future_days=%d",
				currentDay.Format(time.DateOnly), thin.table, thin.futureDays, lastCovered, strings.Join(days, " "), strings.Join(uncovered, " "), settings.MinimumFutureDays),
			evidence: "One primary-guarded read-only catalog query of the attached partitions of " + contractPartitionTableList() + " and their bounds (pg_inherits, pg_get_expr(relpartbound)); a day counts as covered when one partition's range holds every id of it.",
			context:  "Days before the cutover day route to the legacy partition, so between the swap and the cutover day they count as covered without a daily partition. The current day uncovered means contracts are already being refused; with the runway at one day, they are refused from the next 00:00 UTC.",
			action:   "Check the contract partition maintenance task's recent runs and errors (task canaries, [cp] log lines) and the lock waits behind its creates. If it cannot run in time, have the owner run its partition creation (server.MaintainContractPartitions) rather than writing DDL by hand: a hand-made partition must match its day's bounds, columns, storage settings and TRUNCATE guards exactly. Never add a default partition.",
			verify:   fmt.Sprintf("Two consecutive complete observations show every table covering the current day and at least %d days after it.", settings.MinimumFutureDays),
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
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing or a partition bound could not be established, so whether the next days' contract ids have partitions is unknown.",
		baseline:  fmt.Sprintf("One configured primary answers the guarded catalog read with a complete frame and parseable bounds. An unknown runway pages when the last complete observation, at most %.0f days old, had at most %d future days; otherwise it warns.", self.settings.UnknownRiskMemory.Hours()/24, self.settings.UnknownPageFutureDays),
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A runway alert raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read. While a paging unknown lasts, list the contract partitions on the primary directly (pg_inherits of transfer_contract) and treat the runway as its last measured value."),
		verify:    "A complete read restores the runway; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4h and §7",
	}
}
