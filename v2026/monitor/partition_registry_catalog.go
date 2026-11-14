// The catalog read shared by the partition registry signals (SIGNALS.md
// §2.4p-§2.4q, server/db_partition_registry.go): for every family the
// registry declares and no dedicated probe watches, each table's kind and
// every attached partition with its bound. The families, their tables, keys,
// ranges ahead and retention are the registry's static declarations, compiled
// into this binary. Catalog only; no row of a family is read.
package monitor

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The bounds of the shared read.
type PartitionRegistryCatalogSettings struct {
	// The whole-run budget of the read.
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// The partition census bound across the families' tables; a larger
	// census is unknown, never truncated into a verdict.
	MaxPartitions int
	// The longest text cell the frame may carry.
	MaxCellBytes int
}

func DefaultPartitionRegistryCatalogSettings() PartitionRegistryCatalogSettings {
	return PartitionRegistryCatalogSettings{
		RunBudget:      40 * time.Second,
		ClockTolerance: 30 * time.Second,
		MaxPartitions:  4096,
		MaxCellBytes:   256,
	}
}

const partitionRegistryCatalogColumns = 6

// One frame per read: the primary's identity and clock, one row per family
// table, one row per attached partition with a timestamp key's bounds cast
// back in the reading session, and an end row whose counts bind the rows.
// Table names are the registry's source-owned literals.
const partitionRegistryCatalogTemplate = `{hostname_guard}
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery
), family_table AS MATERIALIZED (
 SELECT expected.table_name, expected.timestamp_key,
        to_regclass('public.' || expected.table_name) AS table_oid,
        coalesce((SELECT relation.relkind::text FROM pg_class relation WHERE relation.oid = to_regclass('public.' || expected.table_name)), '') AS kind
 FROM (VALUES {tables}) AS expected(table_name, timestamp_key)
), attached AS MATERIALIZED (
 SELECT family_table.table_name, family_table.timestamp_key, child.relname::text AS name, coalesce(pg_get_expr(child.relpartbound, child.oid), '') AS bound
 FROM family_table
 JOIN pg_inherits inheritance ON inheritance.inhparent = family_table.table_oid
 JOIN pg_class child ON child.oid = inheritance.inhrelid
 ORDER BY 1, 3
 LIMIT {partition_limit}
), attached_bound AS MATERIALIZED (
 SELECT attached.table_name, attached.name, attached.bound,
        CASE WHEN attached.timestamp_key THEN to_char((regexp_match(attached.bound, '^FOR VALUES FROM \(''([^'']*)''\) TO'))[1]::timestamp, 'YYYY-MM-DD HH24:MI:SS.US') END AS lower_cast,
        CASE WHEN attached.timestamp_key THEN to_char((regexp_match(attached.bound, 'TO \(''([^'']*)''\)$'))[1]::timestamp, 'YYYY-MM-DD HH24:MI:SS.US') END AS upper_cast
 FROM attached
)
SELECT 'meta', s.hostname, s.observed_us::text, s.recovery, '', '' FROM source s
UNION ALL SELECT 'table', t.table_name, t.kind, '', '', '' FROM family_table t
UNION ALL SELECT 'partition', a.table_name, a.name, a.bound, coalesce(a.lower_cast, ''), coalesce(a.upper_cast, '') FROM attached_bound a
UNION ALL SELECT 'end', (SELECT count(*) FROM family_table)::text, (SELECT count(*) FROM attached)::text, '', '', '';
`

// The read of these families with this instance's partition bound.
func partitionRegistryCatalogQuery(settings PartitionRegistryCatalogSettings, families []*server.PartitionFamily) string {
	tables := []string{}
	for _, family := range families {
		for _, table := range family.Tables {
			tables = append(tables, "('"+table+"',"+strconv.FormatBool(family.KeyKind == server.PartitionKeyTimestamp)+")")
		}
	}
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{tables}", strings.Join(tables, ","),
		"{partition_limit}", strconv.Itoa(settings.MaxPartitions+1),
	).Replace(partitionRegistryCatalogTemplate)
}

// One family's tables as the catalog describes them.
type partitionRegistryFamilyCatalog struct {
	family *server.PartitionFamily
	// each table's kind, "" when missing
	kinds map[string]string
	// each table's attached partitions
	leaves map[string][]server.PartitionLeaf
}

// Whether the family is partitioned: its first table is a partitioned table.
func (self *partitionRegistryFamilyCatalog) partitioned() bool {
	return self.kinds[self.family.Tables[0]] == "p"
}

// Whether a table of a partitioned family is not partitioned, a layout no
// cutover leaves.
func (self *partitionRegistryFamilyCatalog) mixed() bool {
	if !self.partitioned() {
		return false
	}
	return slices.ContainsFunc(self.family.Tables, func(table string) bool {
		return self.kinds[table] != "p"
	})
}

// The validated catalog read.
type partitionRegistryCatalog struct {
	observed time.Time
	families []*partitionRegistryFamilyCatalog
}

// Fixed framing, the primary's identity and role, a statement clock near the
// monitor's, exactly the families' tables, and every bound a range of its
// family's key. Any gap is one reason.
func parsePartitionRegistryCatalog(settings PartitionRegistryCatalogSettings, families []*server.PartitionFamily, rows []pgRow, primary string, now time.Time) (partitionRegistryCatalog, string) {
	catalog := partitionRegistryCatalog{}
	shortName, _, _ := strings.Cut(primary, ".")
	tableFamilies := map[string]*partitionRegistryFamilyCatalog{}
	for _, family := range families {
		familyCatalog := &partitionRegistryFamilyCatalog{family: family, kinds: map[string]string{}, leaves: map[string][]server.PartitionLeaf{}}
		catalog.families = append(catalog.families, familyCatalog)
		for _, table := range family.Tables {
			tableFamilies[table] = familyCatalog
		}
	}
	type attachedRow struct{ table, name, bound, lower, upper string }
	attachedRows := []attachedRow{}
	tables := map[string]bool{}
	metas, ends := 0, 0
	var end pgRow
	for _, row := range rows {
		if len(row) != partitionRegistryCatalogColumns {
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
			if row[1] != shortName || shortName == "" || row[3] != "0" {
				return catalog, "source-identity-or-role-unavailable"
			}
			observed, ok := retentionLivenessMicros(row[2])
			if !ok {
				return catalog, "invalid-source-clock"
			}
			if observed.Sub(now).Abs() > settings.ClockTolerance {
				return catalog, "source-clock-unavailable"
			}
			catalog.observed = observed
		case "table":
			switch row[2] {
			case "", "r", "p":
			default:
				return catalog, "invalid-table-row"
			}
			familyCatalog, ok := tableFamilies[row[1]]
			if !ok || tables[row[1]] {
				return catalog, "invalid-table-row"
			}
			tables[row[1]] = true
			familyCatalog.kinds[row[1]] = row[2]
		case "partition":
			if row[1] == "" || row[2] == "" {
				return catalog, "invalid-partition-row"
			}
			attachedRows = append(attachedRows, attachedRow{table: row[1], name: row[2], bound: row[3], lower: row[4], upper: row[5]})
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
	tableCount, okTables := pgFreezeAgeInteger(end[1], 0, math.MaxInt32)
	partitionCount, okPartitions := pgFreezeAgeInteger(end[2], 0, math.MaxInt32)
	if !okTables || !okPartitions || int(tableCount) != len(tables) || int(partitionCount) != len(attachedRows) {
		return catalog, "incomplete-frame"
	}
	if len(attachedRows) > settings.MaxPartitions {
		return catalog, "partition-census-overbound"
	}
	if len(tables) != len(tableFamilies) {
		return catalog, "incomplete-frame"
	}
	cast := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	for _, attached := range attachedRows {
		familyCatalog, ok := tableFamilies[attached.table]
		if !ok {
			return catalog, "invalid-partition-row"
		}
		leaf, ok := familyCatalog.family.ParseLeaf(attached.table, attached.name, attached.bound, cast(attached.lower), cast(attached.upper))
		if !ok {
			return catalog, "invalid-partition-bound"
		}
		familyCatalog.leaves[attached.table] = append(familyCatalog.leaves[attached.table], leaf)
	}
	return catalog, ""
}

// One guarded read on the single configured primary. reason is set when the
// read or its frame cannot be trusted; lifecycleErr when the run ended.
func readPartitionRegistryCatalog(ctx context.Context, env *probeEnv, settings PartitionRegistryCatalogSettings, families []*server.PartitionFamily) (catalog partitionRegistryCatalog, primary string, reason string, sourceErr error, lifecycleErr error) {
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return catalog, "primary", "primary-inventory-unavailable-or-ambiguous", nil, nil
	}
	primary = primaries[0].name
	if len(families) == 0 {
		catalog.observed = env.now().UTC()
		return catalog, primary, "", nil, nil
	}
	rows, err := env.runner.pg(ctx, partitionRegistryCatalogQuery(settings, families))
	if lifecycleErr = probeLifecycleError(ctx, err); lifecycleErr != nil {
		return catalog, primary, "", nil, lifecycleErr
	}
	if err != nil {
		return catalog, primary, "catalog-source-unavailable", err, nil
	}
	catalog, reason = parsePartitionRegistryCatalog(settings, families, rows, primary, env.now().UTC())
	return catalog, primary, reason, nil, nil
}

// The registered families whose runway the registry's probe watches: every
// family that names no dedicated probe (server.PartitionFamily.RunwaySignal).
func partitionRegistryRunwayFamilies() (families []*server.PartitionFamily) {
	for _, family := range server.PartitionFamilies() {
		if family.RunwaySignal == "" {
			families = append(families, family)
		}
	}
	return
}

// The registered families whose blocked retention the registry's probe
// watches: every family that drops ranges and names no dedicated probe.
func partitionRegistryRetentionFamilies() (families []*server.PartitionFamily) {
	for _, family := range server.PartitionFamilies() {
		if family.RetentionSignal == "" && 0 < family.Retention {
			families = append(families, family)
		}
	}
	return
}

// The covered ranges after the current one below which a family's runway
// pages: its declaration, else min(2, AheadRanges).
func partitionRegistryMinimumRunway(family *server.PartitionFamily) int {
	if 0 < family.MonitorRunwayRanges {
		return family.MonitorRunwayRanges
	}
	return min(2, family.AheadRanges)
}

// How long past its retention a range may survive before it counts as
// blocked: its declaration, else two ranges.
func partitionRegistryRetentionGrace(family *server.PartitionFamily) time.Duration {
	if 0 < family.MonitorRetentionGrace {
		return family.MonitorRetentionGrace
	}
	start := family.Granularity.RangeStart(time.Unix(0, 0))
	return family.Granularity.AddRanges(start, 2).Sub(start)
}

// The target of a family's findings.
func partitionRegistryTarget(primary string, family *server.PartitionFamily) string {
	return primary + "/" + family.Name + "-partitions"
}

// Observed text for a duration in days, one decimal.
func partitionRegistryDays(duration time.Duration) string {
	return strconv.FormatFloat(duration.Hours()/24, 'f', 1, 64)
}
