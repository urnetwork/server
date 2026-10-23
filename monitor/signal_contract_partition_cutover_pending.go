// SIGNALS.md §2.4j (contract-partition-cutover-pending): a prepared contract
// partition cutover adds a CHECK that refuses contract ids from its cutover
// week on, so it must be finalized, or abandoned, before that week starts.
// Warns while a prepared, unfinalized cutover's week is less than a day away.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const contractPartitionCutoverProbeId = "pg/contract-partition-cutover"

func NewContractPartitionCutoverPendingSignal() Signal {
	return NewContractPartitionCutoverPendingSignalWithSettings(DefaultContractPartitionCutoverSettings())
}

// The signal with explicit thresholds.
func NewContractPartitionCutoverPendingSignalWithSettings(settings *ContractPartitionCutoverSettings) Signal {
	return &signalAdapter{
		number: "2.4j", key: "contract-partition-cutover-pending", name: "Contract partition cutover pending",
		probe: contractPartitionCutoverProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4j.
type ContractPartitionCutoverSettings struct {
	Catalog ContractPartitionCatalogSettings
	Cadence time.Duration
	// Warn when the prepared cutover week starts within this.
	Window time.Duration
	// The maintenance task abandons a prepared cutover this long before its
	// week (the server's contractCutoverAbandonMargin, 6 hours).
	AbandonMargin time.Duration
	Sustain       int
}

func DefaultContractPartitionCutoverSettings() *ContractPartitionCutoverSettings {
	return &ContractPartitionCutoverSettings{
		Catalog:       DefaultContractPartitionCatalogSettings(),
		Cadence:       15 * time.Minute,
		Window:        24 * time.Hour,
		AbandonMargin: 6 * time.Hour,
		Sustain:       1,
	}
}

// Stateless: each run owns its bounded catalog read.
type contractPartitionCutoverProbe struct {
	settings *ContractPartitionCutoverSettings
}

func (self contractPartitionCutoverProbe) id() string             { return contractPartitionCutoverProbeId }
func (self contractPartitionCutoverProbe) tier() string           { return tierWarn }
func (self contractPartitionCutoverProbe) cadence() time.Duration { return self.settings.Cadence }
func (self contractPartitionCutoverProbe) runBudget() time.Duration {
	return self.settings.Catalog.RunBudget
}

func (self contractPartitionCutoverProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	settings := self.settings
	catalog, primary, reason, sourceErr, lifecycleErr := readContractPartitionCatalog(ctx, env, settings.Catalog)
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	target := primary + "/contract-partitions"
	if reason != "" {
		return []finding{self.unknown(target, reason, sourceErr)}, nil
	}
	healthy := []finding{
		healthyFinding(contractPartitionCutoverProbeId, tierWarn, "contract-partition-cutover-pending", target),
		healthyFinding(contractPartitionCutoverProbeId, tierWarn, "contract-partition-cutover-unobservable", target),
	}
	if catalog.partitioned() {
		// Finalized: the bound moved to the legacy partitions with the swap.
		return healthy, nil
	}
	var cutoverWeek time.Time
	bounded, staged := []string{}, []string{}
	distinctWeeks := map[time.Time]bool{}
	for _, table := range catalog.tables {
		if table.stagedKind != "" {
			staged = append(staged, table.name)
		}
		if table.legacyBound == "" {
			continue
		}
		week, ok := parseContractLegacyBoundWeek(table.legacyBound)
		if !ok {
			return []finding{self.unknown(target, "invalid-legacy-bound", nil)}, nil
		}
		bounded = append(bounded, table.name)
		distinctWeeks[week] = true
		if cutoverWeek.IsZero() || week.Before(cutoverWeek) {
			cutoverWeek = week
		}
	}
	// A staged parent alone refuses nothing; only a bound check has a week.
	if cutoverWeek.IsZero() {
		return healthy, nil
	}
	untilCutover := cutoverWeek.Sub(catalog.observed)
	if settings.Window <= untilCutover {
		return healthy, nil
	}
	untilAbandon := untilCutover - settings.AbandonMargin
	symptom := fmt.Sprintf("A contract partition cutover is prepared and not finalized, and its week starts in %.1f hours", untilCutover.Hours())
	contextDetail := fmt.Sprintf("The maintenance task abandons a prepared cutover %.0f hours before its week (drops the staged parents and the bound checks); %.1f hours remain until then.", settings.AbandonMargin.Hours(), untilAbandon.Hours())
	switch {
	case untilCutover <= 0:
		symptom = fmt.Sprintf("A prepared contract partition cutover's week started %.1f hours ago and its bound check still refuses every contract id of that week", -untilCutover.Hours())
		contextDetail = "The abandonment margin has passed: the maintenance task is not running or its abandon failed, and new contracts are refused now. Contract creation (§1.1) shows the outage."
	case untilAbandon <= 0:
		contextDetail = fmt.Sprintf("The abandonment margin passed %.1f hours ago without the task abandoning the cutover: check that maintain_contract_partitions runs.", -untilAbandon.Hours())
	}
	return []finding{
		{
			probeId: contractPartitionCutoverProbeId, tier: tierWarn, class: "contract-partition-cutover-pending", target: target, sustain: settings.Sustain,
			symptom:   symptom,
			mechanism: "Preparing the cutover (bringyourctl model migrate contract-partition) stages the partitioned parents <table>_p and adds the check <table>_legacy_bound, contract_id < contract_id_floor(C), to each contract table. The check refuses every contract id of the cutover week C onward, so the swap (--finalize) must complete before C; until then the tables stay plain.",
			baseline:  fmt.Sprintf("No prepared cutover is unfinalized within %.0f hours of its week.", settings.Window.Hours()),
			observed: fmt.Sprintf("cutover_week=%s hours_until_cutover=%.1f hours_until_abandon=%.1f bounded_tables=[%s] staged_parents=[%s] distinct_bound_weeks=%d",
				cutoverWeek.Format(time.DateOnly), untilCutover.Hours(), untilAbandon.Hours(), strings.Join(bounded, " "), strings.Join(staged, " "), len(distinctWeeks)),
			evidence: "One primary-guarded read-only catalog query of the contract tables' kinds, their staged parents and the <table>_legacy_bound check definitions; the cutover week is the bound's contract id, whose first six bytes are its Unix millisecond.",
			context:  contextDetail,
			action:   "The owner decides now: run --finalize (it refuses within 10 minutes of the week), or abort with --abort and prepare a later week. Do not drop the bound checks or the staged parents by hand.",
			verify:   "The tables are partitioned (finalized) or the bound checks and staged parents are gone (aborted), on a complete observation.",
			playbook: "SIGNALS.md §2.4j",
		},
		healthyFinding(contractPartitionCutoverProbeId, tierWarn, "contract-partition-cutover-unobservable", target),
	}, nil
}

func (self contractPartitionCutoverProbe) unknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: contractPartitionCutoverProbeId, tier: tierWarn, class: "contract-partition-cutover-unobservable", target: target, sustain: 2,
		symptom:   "Whether a contract partition cutover is pending is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing or a prepared bound check could not be established, so a prepared cutover and its week are unknown.",
		baseline:  "One configured primary answers the guarded catalog read with a complete frame and parseable bound checks.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A cutover finding raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read; while it is unknown, check for <table>_legacy_bound checks on the contract tables directly."),
		verify:    "A complete read restores the check; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4j",
	}
}
