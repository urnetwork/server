// The catalog read shared by the contract partition signals (SIGNALS.md
// §2.4h-§2.4j, server/CONTRACT-PARTITION.md): each contract table's kind, its
// staged parent and prepared legacy bound, and every attached partition with
// its bound. Catalog only; no contract row is read.
package monitor

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The bounds of the shared read.
type ContractPartitionCatalogSettings struct {
	// The whole-run budget of the read.
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// The partition census bound across the six tables; a larger census is
	// unknown, never truncated into a verdict.
	MaxPartitions int
	// The longest text cell the frame may carry.
	MaxCellBytes int
}

func DefaultContractPartitionCatalogSettings() ContractPartitionCatalogSettings {
	return ContractPartitionCatalogSettings{
		RunBudget:      40 * time.Second,
		ClockTolerance: 30 * time.Second,
		MaxPartitions:  4096,
		MaxCellBytes:   256,
	}
}

const contractPartitionCatalogColumns = 7

// One frame per read: the primary's identity and clock, one row per contract
// table, one row per attached partition, and an end row whose counts bind the
// rows. Table names are source-owned literals.
const contractPartitionCatalogTemplate = `{hostname_guard}
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery
), contract_table AS MATERIALIZED (
 SELECT expected.table_name,
        to_regclass('public.' || expected.table_name) AS table_oid,
        coalesce((SELECT relation.relkind::text FROM pg_class relation WHERE relation.oid = to_regclass('public.' || expected.table_name)), '') AS kind,
        coalesce((SELECT relation.relkind::text FROM pg_class relation WHERE relation.oid = to_regclass('public.' || expected.table_name || '_p')), '') AS staged_kind,
        coalesce((
         SELECT pg_get_constraintdef(bound.oid)
         FROM pg_constraint bound
         WHERE bound.conrelid = to_regclass('public.' || expected.table_name)
           AND bound.conname = expected.table_name || '_legacy_bound'
           AND bound.contype = 'c'
        ), '') AS legacy_bound
 FROM (VALUES {tables}) AS expected(table_name)
), attached AS MATERIALIZED (
 SELECT contract_table.table_name, child.relname::text AS name, coalesce(pg_get_expr(child.relpartbound, child.oid), '') AS bound
 FROM contract_table
 JOIN pg_inherits inheritance ON inheritance.inhparent = contract_table.table_oid
 JOIN pg_class child ON child.oid = inheritance.inhrelid
 ORDER BY 1, 2
 LIMIT {partition_limit}
)
SELECT 'meta', s.hostname, s.observed_us::text, s.recovery, '', '', '' FROM source s
UNION ALL SELECT 'table', t.table_name, t.kind, t.staged_kind, t.legacy_bound, '', '' FROM contract_table t
UNION ALL SELECT 'partition', a.table_name, a.name, a.bound, '', '', '' FROM attached a
UNION ALL SELECT 'end', (SELECT count(*) FROM contract_table)::text, (SELECT count(*) FROM attached)::text, '', '', '', '';
`

// The read with this instance's partition bound.
func contractPartitionCatalogQuery(settings ContractPartitionCatalogSettings) string {
	tables := make([]string, 0, len(server.ContractPartitionTables))
	for _, table := range server.ContractPartitionTables {
		tables = append(tables, "('"+table+"')")
	}
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{tables}", strings.Join(tables, ","),
		"{partition_limit}", strconv.Itoa(settings.MaxPartitions+1),
	).Replace(contractPartitionCatalogTemplate)
}

// One attached partition and the contract id range it accepts. A nil bound
// is open (MINVALUE or MAXVALUE); a default partition accepts every id no
// other partition does.
type contractPartitionRange struct {
	name        string
	defaultPart bool
	lower       *server.Id
	upper       *server.Id
	// the partition name the maintenance task gives this range, if it is one
	scheme bool
}

// The contract id at which the range stops, as a time; zero when open.
func (self contractPartitionRange) upperTime() time.Time {
	if self.upper == nil {
		return time.Time{}
	}
	return server.ContractIdTime(*self.upper)
}

// Whether every id of the week starting at weekStart routes to this range.
func (self contractPartitionRange) coversWeek(weekStart time.Time) bool {
	if self.defaultPart {
		return true
	}
	floor := server.ContractIdFloor(weekStart)
	end := server.ContractIdFloor(weekStart.Add(contractPartitionWeek))
	return (self.lower == nil || bytes.Compare(self.lower[:], floor[:]) <= 0) &&
		(self.upper == nil || bytes.Compare(end[:], self.upper[:]) <= 0)
}

// The name rendered in alerts: the maintenance scheme's own name, else a
// fixed placeholder, since an arbitrary attached relation name is not
// source-owned.
func (self contractPartitionRange) label(table string) string {
	if self.scheme {
		return self.name
	}
	return "unnamed-partition-of-" + table
}

// One contract table as the catalog describes it.
type contractPartitionTable struct {
	name       string
	kind       string
	stagedKind string
	// the prepared legacy bound check definition, if any
	legacyBound string
	partitions  []contractPartitionRange
}

// The validated catalog read.
type contractPartitionCatalog struct {
	observed time.Time
	tables   []*contractPartitionTable
}

const contractPartitionWeek = 7 * 24 * time.Hour

// Whether the cutover has swapped the tables: transfer_contract leads, as in
// server.IsContractPartitioned.
func (self contractPartitionCatalog) partitioned() bool {
	return 0 < len(self.tables) && self.tables[0].kind == "p"
}

var contractPartitionBoundPattern = regexp.MustCompile(`^FOR VALUES FROM \((MINVALUE|'([0-9a-f-]{36})')\) TO \((MAXVALUE|'([0-9a-f-]{36})')\)$`)

// The check the cutover's prepare step adds: contract_id < contract_id_floor(C).
var contractPartitionLegacyBoundPattern = regexp.MustCompile(`^CHECK \(\(contract_id < '([0-9a-f-]{36})'::uuid\)\)( NOT VALID)?$`)

// The range of one partition bound, or false when it is not a range of
// contract ids.
func parseContractPartitionRange(table, name, bound string) (contractPartitionRange, bool) {
	partition := contractPartitionRange{name: name}
	if bound == "DEFAULT" {
		partition.defaultPart = true
		return partition, true
	}
	match := contractPartitionBoundPattern.FindStringSubmatch(bound)
	if match == nil {
		return partition, false
	}
	if match[1] != "MINVALUE" {
		lower, err := server.ParseId(match[2])
		if err != nil {
			return partition, false
		}
		partition.lower = &lower
	}
	if match[3] != "MAXVALUE" {
		upper, err := server.ParseId(match[4])
		if err != nil {
			return partition, false
		}
		partition.upper = &upper
	}
	if partition.lower != nil && partition.upper != nil && bytes.Compare(partition.lower[:], partition.upper[:]) >= 0 {
		return partition, false
	}
	// The scheme's names: the legacy partition below the cutover week, and a
	// weekly partition whose bounds are its own Monday-to-Monday week.
	if parsedTable, weekStart, legacy, ok := server.ParseContractPartitionName(name); ok && parsedTable == table {
		if legacy {
			partition.scheme = partition.lower == nil && partition.upper != nil
		} else {
			floor := server.ContractIdFloor(weekStart)
			end := server.ContractIdFloor(weekStart.Add(contractPartitionWeek))
			partition.scheme = partition.lower != nil && partition.upper != nil && *partition.lower == floor && *partition.upper == end
		}
	}
	return partition, true
}

// The cutover week a prepared legacy bound refuses ids from.
func parseContractLegacyBoundWeek(definition string) (time.Time, bool) {
	match := contractPartitionLegacyBoundPattern.FindStringSubmatch(definition)
	if match == nil {
		return time.Time{}, false
	}
	floor, err := server.ParseId(match[1])
	if err != nil {
		return time.Time{}, false
	}
	return server.ContractIdTime(floor), true
}

// Fixed framing, the primary's identity and role, a statement clock near the
// monitor's, and exactly the six contract tables. Any gap is one reason.
func parseContractPartitionCatalog(settings ContractPartitionCatalogSettings, rows []pgRow, primary string, now time.Time) (contractPartitionCatalog, string) {
	catalog := contractPartitionCatalog{}
	shortName, _, _ := strings.Cut(primary, ".")
	tables := map[string]*contractPartitionTable{}
	type attachedRow struct{ table, name, bound string }
	attachedRows := []attachedRow{}
	metas, ends := 0, 0
	var end pgRow
	for _, row := range rows {
		if len(row) != contractPartitionCatalogColumns {
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
			switch row[3] {
			case "", "r", "p":
			default:
				return catalog, "invalid-table-row"
			}
			if _, duplicate := tables[row[1]]; duplicate {
				return catalog, "invalid-table-row"
			}
			tables[row[1]] = &contractPartitionTable{name: row[1], kind: row[2], stagedKind: row[3], legacyBound: row[4]}
		case "partition":
			if row[1] == "" || row[2] == "" {
				return catalog, "invalid-partition-row"
			}
			attachedRows = append(attachedRows, attachedRow{table: row[1], name: row[2], bound: row[3]})
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
	for _, name := range server.ContractPartitionTables {
		table, ok := tables[name]
		if !ok {
			return catalog, "incomplete-frame"
		}
		catalog.tables = append(catalog.tables, table)
	}
	if len(tables) != len(server.ContractPartitionTables) {
		return catalog, "invalid-table-row"
	}
	for _, attached := range attachedRows {
		table, ok := tables[attached.table]
		if !ok {
			return catalog, "invalid-partition-row"
		}
		partition, ok := parseContractPartitionRange(attached.table, attached.name, attached.bound)
		if !ok {
			return catalog, "invalid-partition-bound"
		}
		table.partitions = append(table.partitions, partition)
	}
	return catalog, ""
}

// One guarded read on the single configured primary. reason is set when the
// read or its frame cannot be trusted; lifecycleErr when the run ended.
func readContractPartitionCatalog(ctx context.Context, env *probeEnv, settings ContractPartitionCatalogSettings) (catalog contractPartitionCatalog, primary string, reason string, sourceErr error, lifecycleErr error) {
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return catalog, "primary", "primary-inventory-unavailable-or-ambiguous", nil, nil
	}
	primary = primaries[0].name
	rows, err := env.runner.pg(ctx, contractPartitionCatalogQuery(settings))
	if lifecycleErr = probeLifecycleError(ctx, err); lifecycleErr != nil {
		return catalog, primary, "", nil, lifecycleErr
	}
	if err != nil {
		return catalog, primary, "catalog-source-unavailable", err, nil
	}
	catalog, reason = parseContractPartitionCatalog(settings, rows, primary, env.now().UTC())
	return catalog, primary, reason, nil, nil
}

// The fixed names of the six tables, in their order.
func contractPartitionTableList() string {
	return strings.Join(server.ContractPartitionTables, ", ")
}

// Observed text for a duration in days, one decimal.
func contractPartitionDays(duration time.Duration) string {
	return fmt.Sprintf("%.1f", duration.Hours()/24)
}
