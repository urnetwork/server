// Retention that silently stops looks like growth. Three cheap read-only
// checks name the stopped reclaimers behind a runway alert: the payout
// frontier that pins reliability retention, contract reap stamping, and
// partition-drop liveness. A bounded list of large append-only relations
// rides on their findings; by catalog policy it has no alert of its own.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// The thresholds of SIGNALS.md §2.4g. The reliability retention rule mirrors
// the model's own (clientReliabilityRetentionCutoffBlock) so the monitor
// judges by the horizon the drop pass acts on.
type RetentionLivenessSettings struct {
	// Normal sampling cadence and the whole-run budget for both reads.
	Cadence   time.Duration
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// WARN when the newest subsidy_payment end_time is older than this.
	FrontierMaxAge  time.Duration
	FrontierSustain int
	// Contract reap stamping: TABLESAMPLE SYSTEM percent of transfer_contract
	// pages; WARN when, among at least ReapMinimumOldRows sampled rows older
	// than ReapMinimumAge, under ReapMinimumStampedShare have reap_time. A
	// sample under ReapMinimumSampledRows rows is unknown.
	ReapSamplePercent       float64
	ReapMinimumAge          time.Duration
	ReapMinimumStampedShare float64
	ReapMinimumOldRows      int64
	ReapMinimumSampledRows  int64
	ReapSustain             int
	// The rows wait for this straggler horizon when no payment stamps them.
	ReapStragglerHorizon time.Duration
	// client_reliability partitions: steady retention, the longest retention
	// a pinned payout frontier can hold, the margin kept before the
	// frontier, and how long past its cutoff the oldest non-empty partition
	// may survive before the drop pass counts as stuck.
	ReliabilityRetention      time.Duration
	ReliabilityMaxRetention   time.Duration
	ReliabilityFrontierMargin time.Duration
	PartitionStuckGrace       time.Duration
	PartitionSustain          int
	MaxPartitions             int
	// The append-only list: relations at least this large with no deletes
	// and some inserts over a statistics window at least this long.
	GrowthRelationMinimumBytes float64
	GrowthStatsWindow          time.Duration
	GrowthListLimit            int
}

// The §2.4g defaults, with the reliability horizon from the model's own
// constants and the margin it keeps before the payout frontier.
func DefaultRetentionLivenessSettings() *RetentionLivenessSettings {
	return &RetentionLivenessSettings{
		Cadence:                    time.Hour,
		RunBudget:                  70 * time.Second,
		ClockTolerance:             30 * time.Second,
		FrontierMaxAge:             10 * 24 * time.Hour,
		FrontierSustain:            1,
		ReapSamplePercent:          0.0002,
		ReapMinimumAge:             14 * 24 * time.Hour,
		ReapMinimumStampedShare:    0.05,
		ReapMinimumOldRows:         20,
		ReapMinimumSampledRows:     100,
		ReapSustain:                2,
		ReapStragglerHorizon:       model.StragglerContractExpiration,
		ReliabilityRetention:       model.ClientReliabilityRetention,
		ReliabilityMaxRetention:    model.ClientExpiration,
		ReliabilityFrontierMargin:  24 * time.Hour,
		PartitionStuckGrace:        2 * 24 * time.Hour,
		PartitionSustain:           1,
		MaxPartitions:              4096,
		GrowthRelationMinimumBytes: 100 * 1000 * 1000 * 1000,
		GrowthStatsWindow:          7 * 24 * time.Hour,
		GrowthListLimit:            16,
	}
}

// Stateless: each run owns its two bounded reads.
type retentionLivenessProbe struct {
	settings *RetentionLivenessSettings
}

func (self retentionLivenessProbe) id() string               { return retentionLivenessProbeId }
func (self retentionLivenessProbe) tier() string             { return tierWarn }
func (self retentionLivenessProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self retentionLivenessProbe) runBudget() time.Duration { return self.settings.RunBudget }

const retentionLivenessColumns = 10

// The primary-guarded catalog read: frontier, partitions and the append-only
// candidates. Ten text cells per row; counts in the end row bind the rows.
func (self retentionLivenessProbe) catalogQuery() string {
	settings := self.settings
	return "\\set monitor_primary_hostname `hostname -s 2>/dev/null || printf ' unavailable'`\n" + `
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        floor(extract(epoch FROM pg_postmaster_start_time()) * 1000000)::bigint AS started_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery,
        coalesce((SELECT floor(extract(epoch FROM stats_reset) * 1000000)::bigint FROM pg_stat_database WHERE datname = current_database()), 0) AS stats_reset_us,
        coalesce((SELECT relkind::text FROM pg_class WHERE oid = to_regclass('public.client_reliability')), '') AS parent_kind,
        (SELECT floor(extract(epoch FROM max(end_time)) * 1000000)::bigint FROM subsidy_payment) AS frontier_us
), partitions AS MATERIALIZED (
 SELECT c.relname::text AS name, pg_relation_size(c.oid)::bigint AS heap_bytes
 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = to_regclass('public.client_reliability')
 ORDER BY c.relname LIMIT ` + strconv.Itoa(settings.MaxPartitions+1) + `
), growth AS MATERIALIZED (
 SELECT c.oid::bigint AS relation_oid, c.relname::text AS name, pg_total_relation_size(c.oid)::bigint AS total_bytes,
        coalesce(st.n_tup_ins, 0)::bigint AS inserted, coalesce(st.n_tup_del, 0)::bigint AS deleted
 FROM pg_class c JOIN pg_stat_all_tables st ON st.relid = c.oid
 WHERE c.relkind = 'r' AND c.relnamespace = 'public'::regnamespace
   AND pg_total_relation_size(c.oid) >= ` + strconv.FormatFloat(settings.GrowthRelationMinimumBytes, 'f', 0, 64) + `
 ORDER BY 3 DESC, 1 LIMIT ` + strconv.Itoa(settings.GrowthListLimit+1) + `
)
SELECT 'meta', s.hostname, s.observed_us::text, s.started_us::text, s.recovery, s.stats_reset_us::text, s.parent_kind,
       coalesce(s.frontier_us::text, 'none'), '', '' FROM source s
UNION ALL SELECT 'partition', p.name, p.heap_bytes::text, '', '', '', '', '', '', '' FROM partitions p
UNION ALL SELECT 'growth', g.name, g.relation_oid::text, g.total_bytes::text, g.inserted::text, g.deleted::text, '', '', '', '' FROM growth g
UNION ALL SELECT 'end', (SELECT count(*) FROM partitions)::text, (SELECT count(*) FROM growth)::text, '', '', '', '', '', '', '';
`
}

// One sampled-page read of transfer_contract; only counts leave SQL.
func (self retentionLivenessProbe) reapQuery() string {
	settings := self.settings
	age := strconv.FormatInt(int64(settings.ReapMinimumAge/time.Second), 10)
	old := "create_time < (statement_timestamp() AT TIME ZONE 'UTC') - " + age + " * interval '1 second'"
	return "\\set monitor_primary_hostname `hostname -s 2>/dev/null || printf ' unavailable'`\n" +
		"SELECT 'reap', :'monitor_primary_hostname'::text, floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint::text, " +
		"count(*)::text, (count(*) FILTER (WHERE " + old + "))::text, (count(*) FILTER (WHERE " + old + " AND reap_time IS NOT NULL))::text, " +
		"CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END " +
		"FROM transfer_contract TABLESAMPLE SYSTEM (" + strconv.FormatFloat(settings.ReapSamplePercent, 'f', -1, 64) + ");\n"
}

// The validated catalog read.
type retentionLivenessCatalog struct {
	observed    time.Time
	started     time.Time
	statsReset  time.Time
	parentKind  string
	frontier    time.Time
	hasFrontier bool
	partitions  []retentionLivenessPartition
	growth      []retentionLivenessRelation
	growthOver  bool
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
}

var retentionLivenessPartitionPattern = regexp.MustCompile(`^client_reliability_p([0-9]{8})$`)
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
			if len(cell) > 256 {
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
			if row[7] != "none" {
				frontier, ok := retentionLivenessMicros(row[7])
				if !ok {
					return catalog, "invalid-frontier"
				}
				catalog.frontier, catalog.hasFrontier = frontier, true
			}
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
			catalog.growth = append(catalog.growth, retentionLivenessRelation{name: row[1], relationId: relationId, totalBytes: total, inserted: inserted, deleted: deleted})
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
		parts = append(parts, fmt.Sprintf("%s total_gb=%.1f inserted_rows=%d", name, gb(float64(relation.totalBytes)), relation.inserted))
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

// The reliability retention cutoff the drop pass acts on: steady retention,
// moved back to cover the payout frontier, never past the longest retention.
func (self retentionLivenessProbe) reliabilityCutoff(catalog retentionLivenessCatalog) time.Time {
	settings := self.settings
	cutoff := catalog.observed.Add(-settings.ReliabilityRetention)
	if catalog.hasFrontier {
		if frontier := catalog.frontier.Add(-settings.ReliabilityFrontierMargin); frontier.Before(cutoff) {
			cutoff = frontier
		}
	}
	if longest := catalog.observed.Add(-settings.ReliabilityMaxRetention); cutoff.Before(longest) {
		cutoff = longest
	}
	return cutoff
}

// Two independent reads: the catalog read feeds the frontier and partition
// checks, the sampled read feeds reap stamping. A failure in one leaves only
// its checks unknown.
func (self retentionLivenessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		findings := []finding{}
		for _, check := range []string{"subsidy-frontier", "contract-reap", "client-reliability-partitions"} {
			findings = append(findings, self.unknown("primary/"+check, check, "primary-inventory-unavailable-or-ambiguous", nil, "append_only_relations=unknown"))
		}
		return findings, nil
	}
	primary := primaries[0]
	read := func(query string) ([]pgRow, error, error) {
		rows, err := env.runner.pg(ctx, query)
		if ctx.Err() != nil {
			if err != nil {
				return nil, nil, &signalContextError{contextErr: ctx.Err(), sourceErr: err}
			}
			return nil, nil, ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return nil, nil, &signalContextError{contextErr: context.Canceled, sourceErr: err}
		}
		return rows, err, nil
	}
	now := env.now().UTC()
	catalogRows, catalogErr, lifecycleErr := read(self.catalogQuery())
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	catalog, catalogReason := retentionLivenessCatalog{}, ""
	if catalogErr != nil {
		catalogReason = "catalog-source-unavailable"
	} else {
		catalog, catalogReason = self.parseCatalog(catalogRows, primary.name, now)
	}
	list := self.appendOnly(catalog, catalogReason)

	findings := []finding{}
	frontierTarget := primary.name + "/subsidy-frontier"
	partitionTarget := primary.name + "/client-reliability-partitions"
	if catalogReason != "" {
		findings = append(findings,
			self.unknown(frontierTarget, "subsidy-frontier", catalogReason, catalogErr, list),
			self.unknown(partitionTarget, "client-reliability-partitions", catalogReason, catalogErr, list))
	} else {
		findings = append(findings, self.frontier(frontierTarget, catalog, list)...)
		findings = append(findings, self.partitions(partitionTarget, catalog, list)...)
	}

	reapRows, reapErr, lifecycleErr := read(self.reapQuery())
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	findings = append(findings, self.reap(primary.name, reapRows, reapErr, now, list)...)
	return findings, nil
}

// A payout frontier older than the line holds reliability retention and
// leaves contract rows unstamped.
func (self retentionLivenessProbe) frontier(target string, catalog retentionLivenessCatalog, list string) []finding {
	settings := self.settings
	healthy := []finding{
		healthyFinding(retentionLivenessProbeId, tierWarn, "retention-frontier-pinned", target),
		healthyFinding(retentionLivenessProbeId, tierWarn, "retention-liveness-unobservable", target),
	}
	if !catalog.hasFrontier {
		// No subsidy payment has an end time, so none holds retention.
		return healthy
	}
	age := catalog.observed.Sub(catalog.frontier)
	if age <= settings.FrontierMaxAge {
		return healthy
	}
	steady := catalog.observed.Add(-settings.ReliabilityRetention)
	held := catalog.frontier.Add(-settings.ReliabilityFrontierMargin)
	holding := held.Before(steady)
	holdState := fmt.Sprintf("holding_reliability_retention=%t", holding)
	if !holding {
		holdState += fmt.Sprintf(" days_until_hold=%.1f", held.Sub(steady).Hours()/24)
	}
	return []finding{
		{
			probeId: retentionLivenessProbeId, tier: tierWarn, class: "retention-frontier-pinned", target: target, sustain: settings.FrontierSustain,
			symptom:   fmt.Sprintf("The newest subsidy payout epoch ended %.1f days ago; payout-held retention has stopped reclaiming", age.Hours()/24),
			mechanism: "client_reliability retention keeps every block from the last subsidy epoch's end minus a day for the next payout, so a payout frontier that stops advancing stops the nightly partition drops once it is older than the steady retention, until the longest retention forces them. Contract reap stamping also waits on completed payments, so the contract tables stop being reaped as well.",
			baseline:  fmt.Sprintf("max(subsidy_payment.end_time) is at most %.0f days old.", settings.FrontierMaxAge.Hours()/24),
			observed: fmt.Sprintf("frontier_age_days=%.1f %s effective_reliability_cutoff_age_days=%.1f",
				age.Hours()/24, holdState, catalog.observed.Sub(self.reliabilityCutoff(catalog)).Hours()/24),
			evidence: "One primary-guarded read-only catalog query: max(subsidy_payment.end_time) against the primary's statement clock. " + list,
			context:  "Retention horizons are owner decisions. The model's own comment notes that a missing reliability block silently cuts provider credit, so a held frontier is protecting the next payout, not a bug in the drop pass.",
			action:   "Have the payout owner decide whether the next payout will run. Never drop reliability data inside the payout frontier without the owner (§5.14 step 4); size the runway (§2.4f) against the date the longest retention forces the drops.",
			verify:   "A completed payout advances max(subsidy_payment.end_time) inside the line, or the owner records the retention decision and its runway.",
			playbook: "SIGNALS.md §2.4g and §5.14",
		},
		healthyFinding(retentionLivenessProbeId, tierWarn, "retention-liveness-unobservable", target),
	}
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
	upper := oldestDay.Add(24 * time.Hour)
	if !upper.Before(cutoff.Add(-settings.PartitionStuckGrace)) {
		return healthy
	}
	return []finding{
		{
			probeId: retentionLivenessProbeId, tier: tierWarn, class: "partition-retention-stuck", target: target, sustain: settings.PartitionSustain,
			symptom:   fmt.Sprintf("The oldest non-empty client_reliability partition ended %.1f days before its retention cutoff and was not dropped", cutoff.Sub(upper).Hours()/24),
			mechanism: "MaintainClientReliabilityPartitions drops every partition whose upper bound is at or before the retention cutoff, each in its own guarded transaction with a short lock timeout, and retries a lock-busy drop on its next run. A partition that outlives its cutoff by days means the maintenance task is not running or every drop keeps losing its lock.",
			baseline:  fmt.Sprintf("The oldest non-empty daily partition's upper bound is within %.0f days of the cutoff the drop pass computes (steady %.0f days, held back to the payout frontier minus %.0f hours, never past %.0f days).", settings.PartitionStuckGrace.Hours()/24, settings.ReliabilityRetention.Hours()/24, settings.ReliabilityFrontierMargin.Hours(), settings.ReliabilityMaxRetention.Hours()/24),
			observed: fmt.Sprintf("oldest_partition=%s upper_bound_age_days=%.1f cutoff_age_days=%.1f days_past_cutoff=%.1f non_empty_partitions=%d partitions=%d ignored_partitions=%d",
				oldestName, catalog.observed.Sub(upper).Hours()/24, catalog.observed.Sub(cutoff).Hours()/24, cutoff.Sub(upper).Hours()/24, nonEmpty, len(catalog.partitions), ignored),
			evidence: "One primary-guarded read-only catalog query of the partitions attached to client_reliability, their heap sizes, and the subsidy frontier the cutoff depends on. " + list,
			context:  "A partition with an empty heap holds no storage and is not counted. A frontier-held cutoff is retention-frontier-pinned, not a stuck drop.",
			action:   "Check the client reliability maintenance task's recent runs and errors (task canaries) and the lock waits behind its drops. Drop by hand only with the owner's go, one guarded transaction per partition with lock_timeout = '5s', and never inside the payout frontier (§5.14).",
			verify:   "The next maintenance runs drop the partition and a complete observation shows the oldest non-empty partition inside its cutoff plus grace.",
			playbook: "SIGNALS.md §2.4g and §5.14",
		},
		healthyFinding(retentionLivenessProbeId, tierWarn, "retention-liveness-unobservable", target),
	}
}

// Sampled pages of transfer_contract: the share of old rows with reap_time.
func (self retentionLivenessProbe) reap(primary string, rows []pgRow, err error, now time.Time, list string) []finding {
	settings := self.settings
	target := primary + "/contract-reap"
	if err != nil {
		return []finding{self.unknown(target, "contract-reap", "reap-sample-unavailable", err, list)}
	}
	shortName, _, _ := strings.Cut(primary, ".")
	if len(rows) != 1 || len(rows[0]) != 7 || rows[0][0] != "reap" || rows[0][1] != shortName || shortName == "" || rows[0][6] != "0" {
		return []finding{self.unknown(target, "contract-reap", "reap-sample-frame-invalid", nil, list)}
	}
	observed, okObserved := retentionLivenessMicros(rows[0][2])
	sampled, okSampled := pgFreezeAgeInteger(rows[0][3], 0, math.MaxInt64)
	old, okOld := pgFreezeAgeInteger(rows[0][4], 0, math.MaxInt64)
	stamped, okStamped := pgFreezeAgeInteger(rows[0][5], 0, math.MaxInt64)
	switch {
	case !okObserved || !okSampled || !okOld || !okStamped || old > sampled || stamped > old:
		return []finding{self.unknown(target, "contract-reap", "reap-sample-frame-invalid", nil, list)}
	case observed.Sub(now).Abs() > settings.ClockTolerance:
		return []finding{self.unknown(target, "contract-reap", "source-clock-unavailable", nil, list)}
	case sampled < settings.ReapMinimumSampledRows:
		return []finding{self.unknown(target, "contract-reap", "insufficient-sample", nil, list)}
	}
	share := 0.0
	if old > 0 {
		share = float64(stamped) / float64(old)
	}
	if old < settings.ReapMinimumOldRows || share >= settings.ReapMinimumStampedShare {
		return []finding{
			healthyFinding(retentionLivenessProbeId, tierWarn, "contract-reap-stalled", target),
			healthyFinding(retentionLivenessProbeId, tierWarn, "retention-liveness-unobservable", target),
		}
	}
	return []finding{
		{
			probeId: retentionLivenessProbeId, tier: tierWarn, class: "contract-reap-stalled", target: target, sustain: settings.ReapSustain,
			symptom:   fmt.Sprintf("Only %.1f%% of sampled transfer_contract rows older than %.0f days have reap_time; contract retention is not reclaiming", 100*share, settings.ReapMinimumAge.Hours()/24),
			mechanism: fmt.Sprintf("RemoveCompletedContracts stamps reap_time only from a completed payment (completion plus seven days), and the reaper deletes by reap_time. When payouts stop, rows wait for the %.0f-day straggler horizon instead, so the contract tables grow without reaping.", settings.ReapStragglerHorizon.Hours()/24),
			baseline:  fmt.Sprintf("At least %.0f%% of sampled rows older than %.0f days have reap_time, judged on at least %d such rows.", 100*settings.ReapMinimumStampedShare, settings.ReapMinimumAge.Hours()/24, settings.ReapMinimumOldRows),
			observed:  fmt.Sprintf("sampled_rows=%d old_rows=%d old_rows_with_reap_time=%d stamped_share_pct=%.2f sample_percent=%s", sampled, old, stamped, 100*share, strconv.FormatFloat(settings.ReapSamplePercent, 'f', -1, 64)),
			evidence:  "One primary-guarded read-only TABLESAMPLE SYSTEM read of transfer_contract pages; only row counts leave SQL. " + list,
			context:   "False-positive qualifier: a page sample clusters rows of similar age, so one run can over- or under-state the share; the sustain covers that. The sample is random per run. Correlate retention-frontier-pinned: a stopped payout is the usual cause.",
			action:    "Retention horizons are owner decisions: have the payout owner confirm whether payments will resume, and size the contract tables' growth against the storage runway (§2.4f, server/CONTRACT-PARTITION.md). Do not stamp or delete contract rows by hand.",
			verify:    "Completed payments stamp reap_time again and two consecutive samples show the stamped share at or above the line.",
			playbook:  "SIGNALS.md §2.4g",
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
