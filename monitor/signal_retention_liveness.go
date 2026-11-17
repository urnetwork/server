// Retention that silently stops looks like growth. A cheap read-only check
// names a stopped reclaimer behind a runway alert: partition-drop liveness. A
// bounded list of large append-only relations rides on its findings; by
// catalog policy it has no alert of its own. The subsidy payout frontier no
// longer holds any retention (owner decision 2026-10-31), so it is not read.
// Nor is contract reap stamping: only a completed USDC payout stamped
// reap_time, USDC payouts ended (owner decision 2026-11-13), and the contract
// horizon reaper (§2.4r) reclaims contracts without it.
package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const retentionLivenessProbeId = "pg/retention-liveness"

// SIGNALS.md §2.4g (retention-liveness): tables whose retention stopped
// reclaiming.
func NewRetentionLivenessSignal() Signal {
	return NewRetentionLivenessSignalWithSettings(DefaultRetentionLivenessSettings())
}

// The signal with explicit thresholds.
func NewRetentionLivenessSignalWithSettings(settings *RetentionLivenessSettings) Signal {
	return &signalAdapter{
		number: "2.4g", key: "retention-liveness", name: "Retention liveness",
		probe: retentionLivenessProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4g. The reliability retention mirrors the
// model's own (model.ClientReliabilityRetention, the only horizon
// clientReliabilityRetentionCutoffBlock applies) so the monitor judges by the
// horizon the drop pass acts on.
type RetentionLivenessSettings struct {
	// Normal sampling cadence and the whole-run budget for the read.
	Cadence   time.Duration
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// client_reliability partitions: the retention, and how long past its
	// cutoff the oldest non-empty partition may survive before the drop pass
	// counts as stuck.
	ReliabilityRetention time.Duration
	PartitionStuckGrace  time.Duration
	PartitionSustain     int
	MaxPartitions        int
	// The append-only list: relations at least this large with no deletes
	// and some inserts over a statistics window at least this long.
	GrowthRelationMinimumBytes float64
	GrowthStatsWindow          time.Duration
	GrowthListLimit            int
	// The longest text cell either frame may carry.
	MaxCellBytes int
}

// The §2.4g defaults, with the reliability horizon from the model's own
// constant.
func DefaultRetentionLivenessSettings() *RetentionLivenessSettings {
	return &RetentionLivenessSettings{
		Cadence:                    time.Hour,
		RunBudget:                  70 * time.Second,
		ClockTolerance:             30 * time.Second,
		ReliabilityRetention:       model.ClientReliabilityRetention,
		PartitionStuckGrace:        2 * 24 * time.Hour,
		PartitionSustain:           1,
		MaxPartitions:              4096,
		GrowthRelationMinimumBytes: 100 * 1000 * 1000 * 1000,
		GrowthStatsWindow:          7 * 24 * time.Hour,
		GrowthListLimit:            16,
		MaxCellBytes:               256,
	}
}

// Stateless: each run owns its bounded read.
type retentionLivenessProbe struct {
	settings *RetentionLivenessSettings
}

func (self retentionLivenessProbe) id() string               { return retentionLivenessProbeId }
func (self retentionLivenessProbe) tier() string             { return tierWarn }
func (self retentionLivenessProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self retentionLivenessProbe) runBudget() time.Duration { return self.settings.RunBudget }

const retentionLivenessColumns = 10

// The primary-guarded catalog read: partitions and the append-only
// candidates. Ten text cells per row; counts in the end row bind the rows.
// The meta row's eighth cell, once the subsidy payout frontier, and its ninth,
// once transfer_contract's kind for the reap sample, stay empty.
const retentionLivenessCatalogTemplate = `{hostname_guard}
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        floor(extract(epoch FROM pg_postmaster_start_time()) * 1000000)::bigint AS started_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery,
        coalesce((SELECT floor(extract(epoch FROM stats_reset) * 1000000)::bigint FROM pg_stat_database WHERE datname = current_database()), 0) AS stats_reset_us,
        coalesce((SELECT relkind::text FROM pg_class WHERE oid = to_regclass('public.client_reliability')), '') AS parent_kind
), partitions AS MATERIALIZED (
 SELECT c.relname::text AS name, pg_relation_size(c.oid)::bigint AS heap_bytes
 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = to_regclass('public.client_reliability')
 ORDER BY c.relname LIMIT {partition_limit}
), growth_relation AS MATERIALIZED (
 -- a contract partition counts toward its partitioned contract table
 SELECT coalesce(contract_root.oid, c.oid)::bigint AS relation_oid, coalesce(contract_root.relname, c.relname)::text AS name,
        pg_total_relation_size(c.oid)::bigint AS total_bytes,
        coalesce(st.n_tup_ins, 0)::bigint AS inserted, coalesce(st.n_tup_del, 0)::bigint AS deleted,
        contract_root.oid IS NOT NULL AS partitioned
 FROM pg_class c JOIN pg_stat_all_tables st ON st.relid = c.oid
 LEFT JOIN pg_class contract_root
   ON c.relispartition
  AND contract_root.oid = pg_partition_root(c.oid)
  AND contract_root.relnamespace = c.relnamespace
  AND contract_root.relname IN ({contract_tables})
 WHERE c.relkind = 'r' AND c.relnamespace = 'public'::regnamespace
   AND (contract_root.oid IS NOT NULL OR pg_total_relation_size(c.oid) >= {growth_minimum_bytes})
), growth AS MATERIALIZED (
 SELECT relation_oid, name, sum(total_bytes)::bigint AS total_bytes,
        sum(inserted)::bigint AS inserted, sum(deleted)::bigint AS deleted, bool_or(partitioned) AS partitioned
 FROM growth_relation
 GROUP BY relation_oid, name
 HAVING sum(total_bytes) >= {growth_minimum_bytes}
 ORDER BY 3 DESC, 1 LIMIT {growth_limit}
)
SELECT 'meta', s.hostname, s.observed_us::text, s.started_us::text, s.recovery, s.stats_reset_us::text, s.parent_kind,
       '', '', '' FROM source s
UNION ALL SELECT 'partition', p.name, p.heap_bytes::text, '', '', '', '', '', '', '' FROM partitions p
UNION ALL SELECT 'growth', g.name, g.relation_oid::text, g.total_bytes::text, g.inserted::text, g.deleted::text,
       CASE WHEN g.partitioned THEN '1' ELSE '' END, '', '', '' FROM growth g
UNION ALL SELECT 'end', (SELECT count(*) FROM partitions)::text, (SELECT count(*) FROM growth)::text, '', '', '', '', '', '', '';
`

// The catalog read with this instance's bounds.
func (self retentionLivenessProbe) catalogQuery() string {
	settings := self.settings
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{partition_limit}", strconv.Itoa(settings.MaxPartitions+1),
		"{growth_minimum_bytes}", strconv.FormatFloat(settings.GrowthRelationMinimumBytes, 'f', 0, 64),
		"{growth_limit}", strconv.Itoa(settings.GrowthListLimit+1),
		"{contract_tables}", "'"+strings.Join(server.ContractPartitionTables, "','")+"'",
	).Replace(retentionLivenessCatalogTemplate)
}

// The validated catalog read.
type retentionLivenessCatalog struct {
	observed   time.Time
	started    time.Time
	statsReset time.Time
	parentKind string
	partitions []retentionLivenessPartition
	growth     []retentionLivenessRelation
	growthOver bool
	// the partition census exceeded its bound and was discarded
	partitionsOver bool
}

type retentionLivenessPartition struct {
	name      string
	heapBytes int64
}

type retentionLivenessRelation struct {
	name       string
	relationId int64
	totalBytes int64
	inserted   int64
	deleted    int64
	// a partitioned contract table, summed over its partitions
	partitioned bool
}

// The model's partition scheme: one client_reliability partition per UTC day
// named client_reliability_pYYYYMMDD.
var retentionLivenessPartitionPattern = regexp.MustCompile(`^client_reliability_p([0-9]{8})$`)

const retentionLivenessPartitionWidth = 24 * time.Hour

var retentionLivenessRelationNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Positive microsecond epoch cells.
func retentionLivenessMicros(raw string) (time.Time, bool) {
	value, valid := pgFreezeAgeInteger(raw, 1, math.MaxInt64)
	if !valid {
		return time.Time{}, false
	}
	return time.UnixMicro(value).UTC(), true
}

// Fixed framing, the primary's identity and role, and a source clock near
// the monitor's own. Any gap is one reason for every check this read feeds.
func (self retentionLivenessProbe) parseCatalog(rows []pgRow, primary string, now time.Time) (retentionLivenessCatalog, string) {
	settings := self.settings
	catalog := retentionLivenessCatalog{}
	shortName, _, _ := strings.Cut(primary, ".")
	metas, ends := 0, 0
	var end pgRow
	for _, row := range rows {
		if len(row) != retentionLivenessColumns {
			return catalog, "invalid-frame-width"
		}
		for _, cell := range row {
			if len(cell) > settings.MaxCellBytes {
				return catalog, "overbound-cell"
			}
		}
		switch row[0] {
		case "meta":
			metas++
			if row[1] != shortName || shortName == "" || row[4] != "0" {
				return catalog, "source-identity-or-role-unavailable"
			}
			observed, okObserved := retentionLivenessMicros(row[2])
			started, okStarted := retentionLivenessMicros(row[3])
			// Zero means the statistics were never reset.
			statsReset, okReset := time.Time{}, row[5] == "0"
			if !okReset {
				statsReset, okReset = retentionLivenessMicros(row[5])
			}
			if !okObserved || !okStarted || !okReset || started.After(observed) || statsReset.After(observed) {
				return catalog, "invalid-source-clock"
			}
			if observed.Sub(now).Abs() > settings.ClockTolerance {
				return catalog, "source-clock-unavailable"
			}
			catalog.observed, catalog.started, catalog.statsReset, catalog.parentKind = observed, started, statsReset, row[6]
		case "partition":
			heap, ok := pgFreezeAgeInteger(row[2], 0, math.MaxInt64)
			if !ok || row[1] == "" {
				return catalog, "invalid-partition-row"
			}
			catalog.partitions = append(catalog.partitions, retentionLivenessPartition{name: row[1], heapBytes: heap})
		case "growth":
			relationId, okId := pgFreezeAgeInteger(row[2], 1, math.MaxUint32)
			total, okTotal := pgFreezeAgeInteger(row[3], 0, math.MaxInt64)
			inserted, okInserted := pgFreezeAgeInteger(row[4], 0, math.MaxInt64)
			deleted, okDeleted := pgFreezeAgeInteger(row[5], 0, math.MaxInt64)
			if !okId || !okTotal || !okInserted || !okDeleted {
				return catalog, "invalid-growth-row"
			}
			catalog.growth = append(catalog.growth, retentionLivenessRelation{name: row[1], relationId: relationId, totalBytes: total, inserted: inserted, deleted: deleted, partitioned: row[6] == "1"})
		case "end":
			ends++
			end = row
		default:
			return catalog, "unexpected-frame-kind"
		}
	}
	if metas != 1 || ends != 1 {
		return catalog, "incomplete-frame"
	}
	partitionCount, okPartitions := pgFreezeAgeInteger(end[1], 0, math.MaxInt32)
	growthCount, okGrowth := pgFreezeAgeInteger(end[2], 0, math.MaxInt32)
	if !okPartitions || !okGrowth || int(partitionCount) != len(catalog.partitions) || int(growthCount) != len(catalog.growth) {
		return catalog, "incomplete-frame"
	}
	if len(catalog.partitions) > settings.MaxPartitions {
		// An over-cap census leaves only the partition check unknown.
		catalog.partitions, catalog.partitionsOver = nil, true
	}
	if len(catalog.growth) > settings.GrowthListLimit {
		catalog.growth, catalog.growthOver = catalog.growth[:settings.GrowthListLimit], true
	}
	return catalog, ""
}

// Large relations that only grew over a qualified statistics window. The
// window starts at the last statistics reset or, without one, the
// postmaster start; shorter windows cannot certify that nothing was deleted.
func (self retentionLivenessProbe) appendOnly(catalog retentionLivenessCatalog, reason string) string {
	settings := self.settings
	if reason != "" {
		return "append_only_relations=unknown"
	}
	since := catalog.statsReset
	if since.IsZero() {
		since = catalog.started
	}
	if catalog.observed.Sub(since) < settings.GrowthStatsWindow {
		return "append_only_relations=unqualified-stats-window"
	}
	parts := []string{}
	for _, relation := range catalog.growth {
		if relation.deleted != 0 || relation.inserted == 0 {
			continue
		}
		name := relation.name
		if !retentionLivenessRelationNamePattern.MatchString(name) {
			name = "relation-" + strconv.FormatInt(relation.relationId, 10)
		}
		part := fmt.Sprintf("%s total_gb=%.1f inserted_rows=%d", name, gb(float64(relation.totalBytes)), relation.inserted)
		if relation.partitioned {
			// a dropped week's deletes leave with its partition's statistics
			part += " partitioned=true reclaimed_by=partition-drops"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "append_only_relations=none"
	}
	listed := "append_only_relations=[" + strings.Join(parts, "; ") + "]"
	if catalog.growthOver {
		listed += " append_only_list=truncated"
	}
	return listed + fmt.Sprintf(" stats_window_days=%.1f", catalog.observed.Sub(since).Hours()/24)
}

// The reliability retention cutoff the drop pass acts on.
func (self retentionLivenessProbe) reliabilityCutoff(catalog retentionLivenessCatalog) time.Time {
	return catalog.observed.Add(-self.settings.ReliabilityRetention)
}

// One read: the catalog feeds the partition check and the append-only list.
func (self retentionLivenessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{self.unknown("primary/client-reliability-partitions", "client-reliability-partitions", "primary-inventory-unavailable-or-ambiguous", nil, "append_only_relations=unknown")}, nil
	}
	primary := primaries[0]
	now := env.now().UTC()
	catalogRows, catalogErr := env.runner.pg(ctx, self.catalogQuery())
	if lifecycleErr := probeLifecycleError(ctx, catalogErr); lifecycleErr != nil {
		return nil, lifecycleErr
	}
	catalog, catalogReason := retentionLivenessCatalog{}, ""
	if catalogErr != nil {
		catalogReason = "catalog-source-unavailable"
	} else {
		catalog, catalogReason = self.parseCatalog(catalogRows, primary.name, now)
	}
	list := self.appendOnly(catalog, catalogReason)

	partitionTarget := primary.name + "/client-reliability-partitions"
	if catalogReason != "" {
		return []finding{self.unknown(partitionTarget, "client-reliability-partitions", catalogReason, catalogErr, list)}, nil
	}
	return self.partitions(partitionTarget, catalog, list), nil
}

// The oldest storage-holding partition must be inside its cutoff plus grace.
func (self retentionLivenessProbe) partitions(target string, catalog retentionLivenessCatalog, list string) []finding {
	settings := self.settings
	if catalog.parentKind != "p" {
		reason := "parent-missing"
		if catalog.parentKind != "" {
			reason = "parent-not-partitioned"
		}
		return []finding{self.unknown(target, "client-reliability-partitions", reason, nil, list)}
	}
	if catalog.partitionsOver {
		return []finding{self.unknown(target, "client-reliability-partitions", "partition-census-overbound", nil, list)}
	}
	oldestDay, oldestName, ignored, nonEmpty := time.Time{}, "", 0, 0
	for _, partition := range catalog.partitions {
		match := retentionLivenessPartitionPattern.FindStringSubmatch(partition.name)
		if match == nil {
			// The drop pass leaves anything it did not create alone.
			ignored++
			continue
		}
		day, err := time.Parse("20060102", match[1])
		if err != nil {
			ignored++
			continue
		}
		if partition.heapBytes == 0 {
			continue
		}
		nonEmpty++
		if oldestDay.IsZero() || day.Before(oldestDay) {
			oldestDay, oldestName = day, partition.name
		}
	}
	healthy := []finding{
		healthyFinding(retentionLivenessProbeId, tierWarn, "partition-retention-stuck", target),
		healthyFinding(retentionLivenessProbeId, tierWarn, "retention-liveness-unobservable", target),
	}
	if oldestDay.IsZero() {
		return healthy
	}
	cutoff := self.reliabilityCutoff(catalog)
	upper := oldestDay.Add(retentionLivenessPartitionWidth)
	if !upper.Before(cutoff.Add(-settings.PartitionStuckGrace)) {
		return healthy
	}
	return []finding{
		{
			probeId: retentionLivenessProbeId, tier: tierWarn, class: "partition-retention-stuck", target: target, sustain: settings.PartitionSustain,
			symptom:   fmt.Sprintf("The oldest non-empty client_reliability partition ended %.1f days before its retention cutoff and was not dropped", cutoff.Sub(upper).Hours()/24),
			mechanism: "MaintainClientReliabilityPartitions drops every partition whose upper bound is at or before the retention cutoff, each in its own guarded transaction with a short lock timeout, and retries a lock-busy drop on its next run. A partition that outlives its cutoff by days means the maintenance task is not running or every drop keeps losing its lock.",
			baseline:  fmt.Sprintf("The oldest non-empty daily partition's upper bound is within %.0f days of the cutoff the drop pass computes (%.0f days before now).", settings.PartitionStuckGrace.Hours()/24, settings.ReliabilityRetention.Hours()/24),
			observed: fmt.Sprintf("oldest_partition=%s upper_bound_age_days=%.1f cutoff_age_days=%.1f days_past_cutoff=%.1f non_empty_partitions=%d partitions=%d ignored_partitions=%d",
				oldestName, catalog.observed.Sub(upper).Hours()/24, catalog.observed.Sub(cutoff).Hours()/24, cutoff.Sub(upper).Hours()/24, nonEmpty, len(catalog.partitions), ignored),
			evidence: "One primary-guarded read-only catalog query of the partitions attached to client_reliability and their heap sizes. " + list,
			context:  "A partition with an empty heap holds no storage and is not counted. A taskworker still running a build from before 2026-10-31 holds retention back to the last subsidy epoch end; this check judges by the current 21-day rule.",
			action:   "Check the client reliability maintenance task's recent runs and errors (task canaries), the deployed taskworker build, and the lock waits behind its drops. Drop by hand only with the owner's go, one guarded transaction per partition with lock_timeout = '5s', and never inside the retention (§5.14).",
			verify:   "The next maintenance runs drop the partition and a complete observation shows the oldest non-empty partition inside its cutoff plus grace.",
			playbook: "SIGNALS.md §2.4g and §5.14",
		},
		healthyFinding(retentionLivenessProbeId, tierWarn, "retention-liveness-unobservable", target),
	}
}

// An unavailable check is unknown for its own target only.
func (self retentionLivenessProbe) unknown(target, check, reason string, err error, list string) finding {
	observed := "check=" + check + " reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: retentionLivenessProbeId, tier: tierWarn, class: "retention-liveness-unobservable", target: target, sustain: 2,
		symptom:   "Retention liveness check " + check + " is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded read or its fixed framing could not be established, so whether this retention is reclaiming is unknown.",
		baseline:  "One configured primary answers the guarded catalog and sampled reads with complete fixed frames and a statement clock near the monitor's.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; SQL errors, relation contents and credentials stay private. " + list,
		context:   "Unknown is not healthy. A retention finding raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read; keep any open retention finding until the same check is observed completely."),
		verify:    "A complete read restores the check; only a complete observation inside its line is healthy.",
		playbook:  "SIGNALS.md §2.4g",
	}
}
