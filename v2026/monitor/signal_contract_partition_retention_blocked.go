// SIGNALS.md §2.4i (contract-partition-retention-blocked): the maintenance
// task drops a contract day's partitions once the day ended more than the
// retention horizon ago and nothing in it must stay. A partition that outlives
// that horizon by two weeks means its day is blocked or the task is not
// running.
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const contractPartitionRetentionProbeId = "pg/contract-partition-retention"

func NewContractPartitionRetentionBlockedSignal() Signal {
	return NewContractPartitionRetentionBlockedSignalWithSettings(DefaultContractPartitionRetentionSettings())
}

// The signal with explicit thresholds.
func NewContractPartitionRetentionBlockedSignalWithSettings(settings *ContractPartitionRetentionSettings) Signal {
	return &signalAdapter{
		number: "2.4i", key: "contract-partition-retention-blocked", name: "Contract partition retention",
		probe: contractPartitionRetentionProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4i.
type ContractPartitionRetentionSettings struct {
	Catalog ContractPartitionCatalogSettings
	Cadence time.Duration
	// The horizon the maintenance task drops at (server.ContractRetention)
	// and how long past it a partition may survive before it counts as
	// blocked.
	Retention time.Duration
	Grace     time.Duration
	Sustain   int
	// How many blocked days the observed text lists.
	ListLimit int
}

func DefaultContractPartitionRetentionSettings() *ContractPartitionRetentionSettings {
	return &ContractPartitionRetentionSettings{
		Catalog:   DefaultContractPartitionCatalogSettings(),
		Cadence:   time.Hour,
		Retention: server.ContractRetention,
		Grace:     14 * contractPartitionDay,
		Sustain:   1,
		ListLimit: 8,
	}
}

// Stateless: each run owns its bounded catalog read.
type contractPartitionRetentionProbe struct {
	settings *ContractPartitionRetentionSettings
}

func (self contractPartitionRetentionProbe) id() string             { return contractPartitionRetentionProbeId }
func (self contractPartitionRetentionProbe) tier() string           { return tierWarn }
func (self contractPartitionRetentionProbe) cadence() time.Duration { return self.settings.Cadence }
func (self contractPartitionRetentionProbe) runBudget() time.Duration {
	return self.settings.Catalog.RunBudget
}

// One partition past the line.
type contractPartitionBlocked struct {
	table string
	label string
	upper time.Time
}

func (self contractPartitionRetentionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
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
		healthyFinding(contractPartitionRetentionProbeId, tierWarn, "contract-partition-retention-blocked", target),
		healthyFinding(contractPartitionRetentionProbeId, tierWarn, "contract-partition-retention-unobservable", target),
	}
	if !catalog.partitioned() {
		// Plain tables have no partition to drop; reaping is §2.4g's.
		return healthy, nil
	}
	blocked := []contractPartitionBlocked{}
	partitions := 0
	for _, table := range catalog.tables {
		for _, partition := range table.partitions {
			partitions++
			upper := partition.upperTime()
			if upper.IsZero() || !upper.Add(settings.Retention+settings.Grace).Before(catalog.observed) {
				continue
			}
			blocked = append(blocked, contractPartitionBlocked{table: table.name, label: partition.label(table.name), upper: upper})
		}
	}
	if len(blocked) == 0 {
		return healthy, nil
	}
	slices.SortStableFunc(blocked, func(a, b contractPartitionBlocked) int {
		return a.upper.Compare(b.upper)
	})
	oldest := blocked[0]
	byTable := []string{}
	for _, table := range catalog.tables {
		count := 0
		for _, partition := range blocked {
			if partition.table == table.name {
				count++
			}
		}
		if count > 0 {
			byTable = append(byTable, fmt.Sprintf("%s=%d", table.name, count))
		}
	}
	days := []string{}
	for _, partition := range blocked {
		day := partition.upper.Format(time.DateOnly)
		if !slices.Contains(days, day) {
			days = append(days, day)
		}
	}
	listed := days
	if len(listed) > settings.ListLimit {
		listed = listed[:settings.ListLimit]
	}
	dayList := strings.Join(listed, " ")
	if len(days) > len(listed) {
		dayList += fmt.Sprintf(" (+%d)", len(days)-len(listed))
	}
	pastLine := catalog.observed.Sub(oldest.upper.Add(settings.Retention))
	return []finding{
		{
			probeId: contractPartitionRetentionProbeId, tier: tierWarn, class: "contract-partition-retention-blocked", target: target, sustain: settings.Sustain,
			symptom:   fmt.Sprintf("%d contract partition(s) ended more than %s days plus %s days ago and were not dropped; the oldest, %s, ended %s days ago", len(blocked), contractPartitionDays(settings.Retention), contractPartitionDays(settings.Grace), oldest.label, contractPartitionDays(catalog.observed.Sub(oldest.upper))),
			mechanism: "maintain_contract_partitions retires a day 6 hours after its upper bound passes the contract horizon reaper's horizon (its control's horizonDays, 14 to 91 days, while the reaper's mode is on; 91 days otherwise; server/CONTRACT-PARTITION.md S1). It deletes no row: the reaper alone deletes contracts, by its keep rules, archiving their usage. Once the reaper has emptied a day, the task rewrites each leaf down to the rows the reaper keeps (CLUSTER) and drops the day's six partitions together, children first, once they hold no row at all. A day past this line still holds rows the reaper keeps, was never emptied by the reaper (its mode off or its pass behind), was left by a busy lock or a bound, or the task is not running.",
			baseline:  fmt.Sprintf("No contract partition (legacy or daily) whose upper bound is older than the %s-day horizon plus %s days still exists.", contractPartitionDays(settings.Retention), contractPartitionDays(settings.Grace)),
			observed: fmt.Sprintf("blocked_partitions=%d partitions=%d oldest_blocked=%s oldest_upper_bound=%s oldest_days_past_horizon=%s blocked_by_table=[%s] blocked_day_ends=[%s]",
				len(blocked), partitions, oldest.label, oldest.upper.Format(time.DateOnly), contractPartitionDays(pastLine), strings.Join(byTable, " "), dayList),
			evidence: "One primary-guarded read-only catalog query of the attached partitions of " + contractPartitionTableList() + " and their bounds; the upper bound of the legacy partition is the cutover day.",
			context:  "The task reports each expired day it compacted, the rows the reaper keeps in it, and why a day or leaf waits (the reaper has not emptied it, more live rows than a compaction moves, a vacuum or long transaction holding it, deleted rows an older snapshot can still see) in its [cp] log lines and run report; the reaper's kept contracts by reason are urnetwork_contract_horizon_pass_skipped_contracts. A compacted day holds only kept rows and is small; one that was never emptied holds its whole day. Retention keeps growing storage while a day waits (§2.4f).",
			action:   "Read the task's reason for the oldest day. If the reaper keeps its rows, resolve the reason at its owner (the reaper's protected reasons: an open or disputed contract, an unsettled escrow, a payment hold, review or missing payment row, block points still to be awarded). If the reaper has not emptied it, check the reaper's mode, horizon and lag (§2.4r). Check that the task runs at all (task canaries). Never drop, detach or rewrite a contract partition by hand: a drop bypasses the usage archive capture and would delete rows the reaper keeps.",
			verify:   "The next maintenance runs drop the day and a complete observation shows no partition past the horizon plus grace.",
			playbook: "SIGNALS.md §2.4i and §5.14",
		},
		healthyFinding(contractPartitionRetentionProbeId, tierWarn, "contract-partition-retention-unobservable", target),
	}, nil
}

func (self contractPartitionRetentionProbe) unknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: contractPartitionRetentionProbeId, tier: tierWarn, class: "contract-partition-retention-unobservable", target: target, sustain: 2,
		symptom:   "Contract partition retention is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing or a partition bound could not be established, so whether expired contract days are being dropped is unknown.",
		baseline:  "One configured primary answers the guarded catalog read with a complete frame and parseable bounds.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A retention finding raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read; keep any open retention finding until the partitions are observed completely."),
		verify:    "A complete read restores the check; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4i",
	}
}
