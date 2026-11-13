// SIGNALS.md §2.4q (partition-retention-blocked): the partition registry
// (server/db_partition_registry.go) drops a family's range once its upper
// bound is the family's declared retention past and nothing in it must stay.
// A partition that outlives that horizon by the family's grace means its
// range is blocked or the retention is not running. Every family that drops
// ranges and names no dedicated probe; the contract tables' retention is
// §2.4i's and network_ping's §2.19c's.
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const partitionRetentionProbeId = "pg/partition-retention"

func NewPartitionRetentionBlockedSignal() Signal {
	return NewPartitionRetentionBlockedSignalWithSettings(DefaultPartitionRetentionSettings())
}

// The signal with explicit thresholds.
func NewPartitionRetentionBlockedSignalWithSettings(settings *PartitionRetentionSettings) Signal {
	return &signalAdapter{
		number: "2.4q", key: "partition-retention-blocked", name: "Partition registry retention",
		probe: partitionRetentionProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4q. Each family's horizon and grace are its
// declaration (Retention, MonitorRetentionGrace, else two ranges).
type PartitionRetentionSettings struct {
	Catalog PartitionRegistryCatalogSettings
	Cadence time.Duration
	Sustain int
	// How many blocked partitions the observed text lists.
	ListLimit int
	// nil watches every family that drops ranges and names no dedicated
	// probe; a test names its own
	Families func() []*server.PartitionFamily
}

func DefaultPartitionRetentionSettings() *PartitionRetentionSettings {
	return &PartitionRetentionSettings{
		Catalog:   DefaultPartitionRegistryCatalogSettings(),
		Cadence:   time.Hour,
		Sustain:   1,
		ListLimit: 8,
	}
}

// Stateless: each run owns its bounded catalog read.
type partitionRetentionProbe struct {
	settings *PartitionRetentionSettings
}

func (self partitionRetentionProbe) id() string             { return partitionRetentionProbeId }
func (self partitionRetentionProbe) tier() string           { return tierWarn }
func (self partitionRetentionProbe) cadence() time.Duration { return self.settings.Cadence }
func (self partitionRetentionProbe) runBudget() time.Duration {
	return self.settings.Catalog.RunBudget
}

func (self partitionRetentionProbe) families() []*server.PartitionFamily {
	if self.settings.Families != nil {
		return self.settings.Families()
	}
	return partitionRegistryRetentionFamilies()
}

// One leaf past its family's line.
type partitionRetentionBlocked struct {
	table string
	name  string
	upper time.Time
}

func (self partitionRetentionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	settings := self.settings
	families := self.families()
	catalog, primary, reason, sourceErr, lifecycleErr := readPartitionRegistryCatalog(ctx, env, settings.Catalog, families)
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	findings := []finding{}
	if reason != "" {
		for _, family := range families {
			findings = append(findings, self.unknown(partitionRegistryTarget(primary, family), family, reason, sourceErr))
		}
		return findings, nil
	}
	for _, familyCatalog := range catalog.families {
		family := familyCatalog.family
		target := partitionRegistryTarget(primary, family)
		healthy := []finding{
			healthyFinding(partitionRetentionProbeId, tierWarn, "partition-retention-blocked", target),
			healthyFinding(partitionRetentionProbeId, tierWarn, "partition-retention-unobservable", target),
		}
		if !familyCatalog.partitioned() {
			// a plain table has no partition to drop
			findings = append(findings, healthy...)
			continue
		}
		grace := partitionRegistryRetentionGrace(family)
		blocked := []partitionRetentionBlocked{}
		leaves := 0
		for _, table := range family.Tables {
			for _, leaf := range familyCatalog.leaves[table] {
				leaves++
				// only the registry's own leaves are dropped by it; the
				// DEFAULT partition and a kept legacy partition never are
				if !leaf.Canonical || leaf.Range.Default || leaf.Range.Upper.IsZero() || (leaf.Range.Legacy && family.KeepLegacyPartition) {
					continue
				}
				if !leaf.Range.Upper.Add(family.Retention + grace).Before(catalog.observed) {
					continue
				}
				blocked = append(blocked, partitionRetentionBlocked{table: table, name: leaf.Name, upper: leaf.Range.Upper})
			}
		}
		if len(blocked) == 0 {
			findings = append(findings, healthy...)
			continue
		}
		slices.SortStableFunc(blocked, func(a, b partitionRetentionBlocked) int {
			return a.upper.Compare(b.upper)
		})
		oldest := blocked[0]
		listed := []string{}
		for _, partition := range blocked {
			if len(listed) == settings.ListLimit {
				break
			}
			listed = append(listed, partition.name)
		}
		list := strings.Join(listed, " ")
		if len(blocked) > len(listed) {
			list += fmt.Sprintf(" (+%d)", len(blocked)-len(listed))
		}
		findings = append(findings,
			finding{
				probeId: partitionRetentionProbeId, tier: tierWarn, class: "partition-retention-blocked", target: target, sustain: settings.Sustain,
				symptom: fmt.Sprintf("%d partition(s) of the %s family ended more than %s days plus %s days ago and were not dropped; the oldest, %s, ended %s days ago",
					len(blocked), family.Name, partitionRegistryDays(family.Retention), partitionRegistryDays(grace), oldest.name, partitionRegistryDays(catalog.observed.Sub(oldest.upper))),
				mechanism: "The registry drops a family's range once its upper bound is the family's retention past and its precondition, if it has one, passes; a family with its own task drops through that task. A range blocked by its precondition stays and is reported with the reason, so a partition this far past the horizon means a blocker persists or the retention is not running.",
				baseline:  fmt.Sprintf("No partition of the family whose upper bound is older than its %s-day retention plus %s days still exists.", partitionRegistryDays(family.Retention), partitionRegistryDays(grace)),
				observed: fmt.Sprintf("family=%s blocked_partitions=%d partitions=%d oldest_blocked=%s oldest_upper_bound=%s oldest_days_past_horizon=%s blocked=[%s]",
					family.Name, len(blocked), leaves, oldest.name, oldest.upper.Format(time.DateOnly), partitionRegistryDays(catalog.observed.Sub(oldest.upper.Add(family.Retention))), list),
				evidence: "One primary-guarded read-only catalog query of the attached partitions of the registry's families and their bounds.",
				context:  "Family: " + family.Description + ". The retention task logs each blocked range's reason in its [partitions] log lines; a family with its own task logs in that task's. Retention keeps growing storage while a range is blocked (§2.4f).",
				action:   "Read the retention's reason for the oldest blocked range and resolve that blocker at its owner. Check that the task runs at all (task canaries). Never drop or detach a family's partition by hand: a drop bypasses the family's guards and preconditions.",
				verify:   "The next runs drop the range and a complete observation shows no partition past the horizon plus grace.",
				playbook: "SIGNALS.md §2.4q and §5.14",
			},
			healthyFinding(partitionRetentionProbeId, tierWarn, "partition-retention-unobservable", target),
		)
	}
	return findings, nil
}

func (self partitionRetentionProbe) unknown(target string, family *server.PartitionFamily, reason string, err error) finding {
	observed := "family=" + family.Name + " reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: partitionRetentionProbeId, tier: tierWarn, class: "partition-retention-unobservable", target: target, sustain: 2,
		symptom:   "Partition retention of the " + family.Name + " family is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded catalog read, its fixed framing or a partition bound could not be established, so whether expired ranges are being dropped is unknown.",
		baseline:  "One configured primary answers the guarded catalog read with a complete frame and parseable bounds.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy. A retention finding raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read; keep any open retention finding until the partitions are observed completely."),
		verify:    "A complete read restores the check; only a complete observation inside the line is healthy.",
		playbook:  "SIGNALS.md §2.4q",
	}
}
