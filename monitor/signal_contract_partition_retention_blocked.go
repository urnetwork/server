// SIGNALS.md §2.4i (contract-partition-retention-blocked): the maintenance
// task drops a contract week's partitions once the week ended more than the
// retention horizon ago and nothing in it must stay. A partition that outlives
// that horizon by two weeks means its week is blocked or the task is not
// running.
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
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
	// How many blocked weeks the observed text lists.
	ListLimit int
}

func DefaultContractPartitionRetentionSettings() *ContractPartitionRetentionSettings {
	return &ContractPartitionRetentionSettings{
		Catalog:   DefaultContractPartitionCatalogSettings(),
		Cadence:   time.Hour,
		Retention: server.ContractRetention,
		Grace:     2 * contractPartitionWeek,
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
	weeks := []string{}
	for _, partition := range blocked {
		week := partition.upper.Format(time.DateOnly)
		if !slices.Contains(weeks, week) {
			weeks = append(weeks, week)
		}
	}
	listed := weeks
	if len(listed) > settings.ListLimit {
		listed = listed[:settings.ListLimit]
	}
	weekList := strings.Join(listed, " ")
	if len(weeks) > len(listed) {
		weekList += fmt.Sprintf(" (+%d)", len(weeks)-len(listed))
	}
	pastLine := catalog.observed.Sub(oldest.upper.Add(settings.Retention))
	return []finding{
		{
			probeId: contractPartitionRetentionProbeId, tier: tierWarn, class: "contract-partition-retention-blocked", target: target, sustain: settings.Sustain,
			symptom:   fmt.Sprintf("%d contract partition(s) ended more than %s days plus %s days ago and were not dropped; the oldest, %s, ended %s days ago", len(blocked), contractPartitionDays(settings.Retention), contractPartitionDays(settings.Grace), oldest.label, contractPartitionDays(catalog.observed.Sub(oldest.upper))),
			mechanism: "maintain_contract_partitions drops a week's six partitions together, children first, once the week ended longer ago than the retention horizon and every contract in it is resolved, every escrow settled, no sweep belongs to a pending payment and no legacy settlement intent refers to it; it first archives the week's usage proof. A blocked week stays and is reported by the task with its reason, so a partition this far past the horizon means a blocker persists or the task is not running.",
			baseline:  fmt.Sprintf("No contract partition (legacy or weekly) whose upper bound is older than the %s-day horizon plus %s days still exists.", contractPartitionDays(settings.Retention), contractPartitionDays(settings.Grace)),
			observed: fmt.Sprintf("blocked_partitions=%d partitions=%d oldest_blocked=%s oldest_upper_bound=%s oldest_days_past_horizon=%s blocked_by_table=[%s] blocked_week_ends=[%s]",
				len(blocked), partitions, oldest.label, oldest.upper.Format(time.DateOnly), contractPartitionDays(pastLine), strings.Join(byTable, " "), weekList),
			evidence: "One primary-guarded read-only catalog query of the attached partitions of " + contractPartitionTableList() + " and their bounds; the upper bound of the legacy partition is the cutover week.",
			context:  "The task reports each blocked week's reason (an unresolved contract, an unsettled escrow, a sweep of a pending payment, a legacy settlement intent) in its [cp] log lines and run report. Retention keeps growing storage while a week is blocked (§2.4f).",
			action:   "Read the task's reason for the oldest blocked week and resolve that blocker at its owner: close or settle the contracts, reconcile external payment attempts, finish payment retention stamping, or finish the legacy settlement. Cancellation alone does not release an external payment attempt's hold. Check that the task runs at all (task canaries). Never drop or detach a contract partition by hand: the drop bypasses the usage archive capture and the preconditions.",
			verify:   "The next maintenance runs drop the week and a complete observation shows no partition past the horizon plus grace.",
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
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing or a partition bound could not be established, so whether expired contract weeks are being dropped is unknown.",
		baseline:  "One configured primary answers the guarded catalog read with a complete frame and parseable bounds.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A retention finding raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read; keep any open retention finding until the partitions are observed completely."),
		verify:    "A complete read restores the check; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4i",
	}
}
