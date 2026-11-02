// SIGNALS.md §2.4s (usage-archive-hash-budget): the epoch usage read
// (model stEpochProviderUsageSql) joins the usage archive's rows that closed
// in its window and hashes them in memory. Past work_mem * hash_mem_multiplier
// the hash spills in batches toward temp_file_limit, in the v1 close, the v2
// epoch step and the recovery census alike. A seven-day window's archived
// contracts sit in at most two of the archive's weekly partitions (contract
// weeks), so the probe reads each partition's estimated rows and the two
// settings from the catalog, and warns while twice the largest week passes a
// share of the rows the hash holds. Catalog only; no archive row is read.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const usageArchiveHashBudgetProbeId = "pg/usage-archive-hash-budget"

func NewUsageArchiveHashBudgetSignal() Signal {
	return NewUsageArchiveHashBudgetSignalWithSettings(DefaultUsageArchiveHashBudgetSettings())
}

// The signal with explicit thresholds.
func NewUsageArchiveHashBudgetSignalWithSettings(settings *UsageArchiveHashBudgetSettings) Signal {
	return &signalAdapter{
		number: "2.4s", key: "usage-archive-hash-budget", name: "Usage archive hash budget",
		probe: usageArchiveHashBudgetProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4s.
type UsageArchiveHashBudgetSettings struct {
	Cadence   time.Duration
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// The partition census bound; a larger census is unknown.
	MaxPartitions int
	MaxCellBytes  int
	// The bytes one archived row takes in the hash: its key and the tuple
	// overhead (Main, 2026-11-01: 3.66 GB of hash memory held about 60M).
	HashEntryBytes float64
	// Warn while the estimated window passes this share of the hash's rows.
	Share float64
}

// The defaults of SIGNALS.md §2.4s.
func DefaultUsageArchiveHashBudgetSettings() *UsageArchiveHashBudgetSettings {
	return &UsageArchiveHashBudgetSettings{
		Cadence:        time.Hour,
		RunBudget:      40 * time.Second,
		ClockTolerance: 30 * time.Second,
		MaxPartitions:  4096,
		MaxCellBytes:   256,
		HashEntryBytes: 64,
		Share:          0.5,
	}
}

func (self *UsageArchiveHashBudgetSettings) valid() bool {
	return self != nil && time.Minute <= self.Cadence && 0 < self.RunBudget && 0 < self.ClockTolerance &&
		0 < self.MaxPartitions && 0 < self.MaxCellBytes && 0 < self.HashEntryBytes && 0 < self.Share && self.Share <= 1
}

type usageArchiveHashBudgetProbe struct {
	settings *UsageArchiveHashBudgetSettings
}

func (usageArchiveHashBudgetProbe) id() string   { return usageArchiveHashBudgetProbeId }
func (usageArchiveHashBudgetProbe) tier() string { return tierWarn }

func (self usageArchiveHashBudgetProbe) cadence() time.Duration {
	return self.effectiveSettings().Cadence
}

func (self usageArchiveHashBudgetProbe) runBudget() time.Duration {
	return self.effectiveSettings().RunBudget
}

func (self usageArchiveHashBudgetProbe) effectiveSettings() *UsageArchiveHashBudgetSettings {
	if self.settings == nil {
		return DefaultUsageArchiveHashBudgetSettings()
	}
	return self.settings
}

const usageArchiveHashBudgetColumns = 5

// One frame per read: the primary's identity and clock, the two settings in
// pg_settings' own units (kB and a factor), the archive parent's kind and
// estimated rows, each attached partition's estimated rows, and an end row
// whose count binds the partitions.
const usageArchiveHashBudgetTemplate = `{hostname_guard}
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery
), archive AS MATERIALIZED (
 SELECT to_regclass('public.st_provider_usage_archive') AS oid
), parent AS MATERIALIZED (
 SELECT coalesce(relation.relkind::text, '') AS kind, coalesce(greatest(relation.reltuples, 0)::bigint, 0) AS rows
 FROM archive LEFT JOIN pg_class relation ON relation.oid = archive.oid
), part AS MATERIALIZED (
 SELECT child.relname::text AS name, greatest(child.reltuples, 0)::bigint AS rows
 FROM archive
 JOIN pg_inherits inheritance ON inheritance.inhparent = archive.oid
 JOIN pg_class child ON child.oid = inheritance.inhrelid
 ORDER BY 1
 LIMIT {partition_limit}
)
SELECT 'meta', s.hostname, s.observed_us::text, s.recovery, '' FROM source s
UNION ALL SELECT 'settings',
 coalesce((SELECT setting FROM pg_settings WHERE name = 'work_mem'), ''),
 coalesce((SELECT unit FROM pg_settings WHERE name = 'work_mem'), ''),
 coalesce((SELECT setting FROM pg_settings WHERE name = 'hash_mem_multiplier'), ''), ''
UNION ALL SELECT 'parent', p.kind, p.rows::text, '', '' FROM parent p
UNION ALL SELECT 'partition', t.name, t.rows::text, '', '' FROM part t
UNION ALL SELECT 'end', (SELECT count(*) FROM part)::text, '', '', '';
`

func usageArchiveHashBudgetQuery(settings *UsageArchiveHashBudgetSettings) string {
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{partition_limit}", strconv.Itoa(settings.MaxPartitions+1),
	).Replace(usageArchiveHashBudgetTemplate)
}

// The read's facts.
type usageArchiveHashBudget struct {
	workMemKb  float64
	multiplier float64
	// "p" partitioned, "r" a plain table
	kind       string
	parentRows float64
	partitions int
	totalRows  float64
	// the partition with the most estimated rows
	largestName string
	largestRows float64
}

// The rows the hash holds in one batch.
func (self usageArchiveHashBudget) hashRows(settings *UsageArchiveHashBudgetSettings) float64 {
	return self.workMemKb * 1024 * self.multiplier / settings.HashEntryBytes
}

// The most archived rows one seven-day window can join: two contract weeks of
// a partitioned archive, or all of a plain one.
func (self usageArchiveHashBudget) windowRows() float64 {
	if self.kind == "p" {
		return 2 * self.largestRows
	}
	return self.parentRows
}

func parseUsageArchiveHashBudget(settings *UsageArchiveHashBudgetSettings, rows []pgRow, primary string, now time.Time) (usageArchiveHashBudget, string) {
	budget := usageArchiveHashBudget{}
	shortName, _, _ := strings.Cut(primary, ".")
	metas, settingRows, parents, ends := 0, 0, 0, 0
	partitions := 0
	var end pgRow
	number := func(raw string) (float64, bool) {
		value, err := strconv.ParseFloat(raw, 64)
		return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && 0 <= value
	}
	for _, row := range rows {
		if len(row) != usageArchiveHashBudgetColumns {
			return budget, "invalid-frame-width"
		}
		for _, cell := range row {
			if len(cell) > settings.MaxCellBytes {
				return budget, "overbound-cell"
			}
		}
		switch row[0] {
		case "meta":
			metas++
			if row[1] != shortName || shortName == "" || row[3] != "0" {
				return budget, "source-identity-or-role-unavailable"
			}
			observed, ok := retentionLivenessMicros(row[2])
			if !ok {
				return budget, "invalid-source-clock"
			}
			if observed.Sub(now).Abs() > settings.ClockTolerance {
				return budget, "source-clock-unavailable"
			}
		case "settings":
			settingRows++
			workMem, okMem := number(row[1])
			multiplier, okMultiplier := number(row[3])
			if !okMem || row[2] != "kB" || !okMultiplier || workMem == 0 || multiplier == 0 {
				return budget, "invalid-settings"
			}
			budget.workMemKb, budget.multiplier = workMem, multiplier
		case "parent":
			parents++
			rowCount, ok := number(row[2])
			if !ok {
				return budget, "invalid-parent-row"
			}
			switch row[1] {
			case "p", "r":
			case "":
				return budget, "archive-missing"
			default:
				return budget, "invalid-parent-row"
			}
			budget.kind, budget.parentRows = row[1], rowCount
		case "partition":
			partitions++
			rowCount, ok := number(row[2])
			if row[1] == "" || !ok {
				return budget, "invalid-partition-row"
			}
			budget.totalRows += rowCount
			if budget.largestName == "" || budget.largestRows < rowCount {
				budget.largestName, budget.largestRows = row[1], rowCount
			}
		case "end":
			ends++
			end = row
		default:
			return budget, "invalid-row-kind"
		}
	}
	if metas != 1 || settingRows != 1 || parents != 1 || ends != 1 {
		return budget, "incomplete-frame"
	}
	count, err := strconv.Atoi(end[1])
	if err != nil || count != partitions {
		return budget, "incomplete-frame"
	}
	if settings.MaxPartitions < partitions {
		return budget, "overbound-partition-census"
	}
	if budget.kind == "p" && partitions == 0 {
		return budget, "partitioned-archive-without-partitions"
	}
	if budget.kind == "r" {
		budget.totalRows = budget.parentRows
	}
	budget.partitions = partitions
	return budget, ""
}

func (self usageArchiveHashBudgetProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	settings := self.effectiveSettings()
	primaries := env.cfg.hostsWithRole("pg-primary")
	primary := "primary"
	if len(primaries) == 1 {
		primary = primaries[0].name
	}
	target := primary + "/usage-archive"
	unobservable := func(reason string, sourceErr error) finding {
		observed := "reason=" + reason
		if sourceErr != nil {
			observed += " source_error=" + classifyObservationError(sourceErr)
		}
		return finding{
			probeId: usageArchiveHashBudgetProbeId, tier: tierWarn, class: "usage-archive-hash-budget-unobservable", target: target, sustain: 2,
			symptom:   "The usage archive's partition sizes or the hash settings could not be read.",
			mechanism: "Without the archive's estimated rows and work_mem and hash_mem_multiplier the read cannot say whether an epoch window's archive hash fits in one batch.",
			baseline:  "One bounded catalog frame from the single pg-primary, its clock within tolerance.",
			observed:  observed,
			evidence:  "Only a fixed structural reason leaves the probe.",
			context:   "The archive is st_provider_usage_archive, partitioned by contract week since 2026-10-24.",
			action:    "Check the primary inventory, its role and clock, and the read's permissions on pg_class, pg_inherits and pg_settings.",
			verify:    "The frame reads whole for two cadences.",
			playbook:  "SIGNALS.md §2.4s",
		}
	}
	if !settings.valid() {
		return []finding{unobservable("invalid-policy", nil)}, nil
	}
	if len(primaries) != 1 {
		return []finding{unobservable("primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	rows, err := env.runner.pg(ctx, usageArchiveHashBudgetQuery(settings))
	if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if err != nil {
		return []finding{unobservable("catalog-source-unavailable", err)}, nil
	}
	budget, reason := parseUsageArchiveHashBudget(settings, rows, primary, env.now().UTC())
	if reason != "" {
		return []finding{unobservable(reason, nil)}, nil
	}
	findings := []finding{healthyFinding(usageArchiveHashBudgetProbeId, tierWarn, "usage-archive-hash-budget-unobservable", target)}
	hashRows := budget.hashRows(settings)
	windowRows := budget.windowRows()
	if windowRows <= settings.Share*hashRows {
		return append(findings, healthyFinding(usageArchiveHashBudgetProbeId, tierWarn, "usage-archive-hash-budget", target)), nil
	}
	largest := budget.largestName
	if budget.kind != "p" {
		largest = "st_provider_usage_archive"
	}
	return append(findings, finding{
		probeId: usageArchiveHashBudgetProbeId, tier: tierWarn, class: "usage-archive-hash-budget", target: target, sustain: 1,
		symptom: fmt.Sprintf("An epoch window can join about %.1fM archived rows, %.0f%% of the %.1fM the usage read's hash holds in one batch",
			windowRows/1e6, 100*windowRows/hashRows, hashRows/1e6),
		mechanism: "The epoch usage read flags a live contract that is also archived by hashing the archive rows that closed in its window, about one or two contract weeks. Past work_mem * hash_mem_multiplier the hash runs in batches and spills, toward temp_file_limit, inside the v1 close, the v2 epoch step and the recovery census; an epoch step that fails misses its root. The contract horizon reaper archives every credit-bearing contract it deletes, so a week's partition grows as the reaper reaches it.",
		baseline:  fmt.Sprintf("Twice the largest contract week stays under %.0f%% of the hash's rows (work_mem * hash_mem_multiplier / %.0f bytes a row).", 100*settings.Share, settings.HashEntryBytes),
		observed: fmt.Sprintf("largest_week=%s largest_rows=%.0f window_estimate=%.0f hash_rows=%.0f work_mem_kb=%.0f hash_mem_multiplier=%g archive_rows=%.0f partitions=%d kind=%s",
			largest, budget.largestRows, windowRows, hashRows, budget.workMemKb, budget.multiplier, budget.totalRows, budget.partitions, budget.kind),
		evidence: "pg_class.reltuples of st_provider_usage_archive and its attached partitions, and pg_settings work_mem and hash_mem_multiplier, in one catalog frame. Estimates, refreshed by ANALYZE.",
		context:  "A build whose usage read joins the whole archive (before 669c57fe8, 2026-11-01) hashes every row instead; archive_rows is its build side. The read's own session may set work_mem differently from the catalog's.",
		action:   "Before the next epoch close, check EXPLAIN (ANALYZE) of the window's usage read on a replica or in a maintenance window, raise the read session's work_mem, or slow the reaper (maxRowsPerSecond) while the archive's weeks are this large. Never delete archive rows by hand: the subnet's epochs read them.",
		verify:   "Twice the largest week falls back under the line, or an EXPLAIN of the window's read shows one hash batch.",
		playbook: "SIGNALS.md §2.4s and §2.4r",
	}), nil
}
