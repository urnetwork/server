// Range partitioning and retention of the contract tables (CONTRACT-PARTITION.md).
package server

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The smallest contract id of a millisecond. Contract ids lead with their
// 48-bit creation millisecond (connect.NewId), so an id range is a time range:
// the partition bounds and the legacy partition's bound check derive from this
// function. It is IMMUTABLE, which a CHECK constraint requires: an epoch does
// not depend on the session time zone.
const contractIdFloorSql = `
		CREATE OR REPLACE FUNCTION contract_id_floor(t timestamptz) RETURNS uuid
		LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
		AS $$
			SELECT (lpad(to_hex(floor(extract(epoch FROM t) * 1000)::bigint), 12, '0') || '00000000000000000000')::uuid
		$$
	`

// The contract tables, partitioned together by contract_id with the same
// weekly bounds: transfer_contract first, then the tables keyed by its contract
// id. A week's partitions are dropped together, children first.
var ContractPartitionTables = []string{
	"transfer_contract",
	"transfer_escrow",
	"contract_close",
	"transfer_escrow_sweep",
	"contract_close_report",
	"contract_extender",
}

// The retention horizon (owner decision 2026-10-23): a week's partitions are
// dropped once the week ended longer ago than this and it holds nothing that
// must stay. The close evidence guard (migration 857) uses the same 91 days.
const ContractRetention = 13 * 7 * 24 * time.Hour

// Weeks after the current one that always have partitions. An insert past the
// last partition fails, so the monitor alerts when fewer than 2 exist.
const ContractPartitionAheadWeeks = 4

const contractPartitionWeek = 7 * 24 * time.Hour

// The prepared legacy bound check refuses ids from the cutover week on, so a
// cutover that is not finalized this long before that week is abandoned
// (MaintainContractPartitions) before it can refuse a new contract.
const contractCutoverAbandonMargin = 6 * time.Hour

// The swap refuses to start this close to the cutover week.
const contractCutoverFinalizeMargin = 10 * time.Minute

// Outcomes whose provider usage the archive keeps, the predicate of
// ProviderUsageArchiveCaptureFunctionBodySql.
const contractCreditOutcomesSql = `('settled','dispute_resolved_to_source','dispute_resolved_to_destination')`

var contractPartitionNamePattern = regexp.MustCompile(`^(transfer_contract|transfer_escrow|contract_close|transfer_escrow_sweep|contract_close_report|contract_extender)_(w([0-9]{8})|legacy)$`)

// The Monday 00:00 UTC that starts t's week.
func ContractPartitionWeekStart(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

// The smallest contract id of t's millisecond, the value of contract_id_floor.
func ContractIdFloor(t time.Time) Id {
	var id Id
	ms := uint64(t.UnixMilli())
	for i := range 6 {
		id[i] = byte(ms >> (8 * (5 - i)))
	}
	return id
}

// The creation millisecond a contract id leads with.
func ContractIdTime(id Id) time.Time {
	var ms uint64
	for i := range 6 {
		ms = ms<<8 | uint64(id[i])
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// Weekly partitions are named for their Monday: transfer_contract_w20261026.
func ContractPartitionName(table string, weekStart time.Time) string {
	return fmt.Sprintf("%s_w%s", table, weekStart.UTC().Format("20060102"))
}

// The partition that holds every row from before the cutover.
func ContractLegacyPartitionName(table string) string {
	return table + "_legacy"
}

// Parses a contract partition name. A weekly partition has its week start; the
// legacy partition has legacy set.
func ParseContractPartitionName(name string) (table string, weekStart time.Time, legacy bool, ok bool) {
	groups := contractPartitionNamePattern.FindStringSubmatch(name)
	if groups == nil {
		return
	}
	table = groups[1]
	if groups[2] == "legacy" {
		return table, time.Time{}, true, true
	}
	weekStart, err := time.Parse("20060102", groups[3])
	if err != nil || !ContractPartitionWeekStart(weekStart).Equal(weekStart) {
		return "", time.Time{}, false, false
	}
	return table, weekStart, false, true
}

// While a cutover is prepared, each table has a partitioned parent staged
// under this name, and its indexes carry stagedIndexSuffix until the swap.
func contractStagedParentName(table string) string {
	return table + "_p"
}

const contractStagedIndexSuffix = "_p"

// At the swap the legacy table's indexes move to these names, so the parent's
// indexes can take the published ones.
const contractLegacyIndexSuffix = "_l"

func contractLegacyBoundName(table string) string {
	return table + "_legacy_bound"
}

const pgMaxIdentifierLength = 63

func quoteIdentifier(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// The uuid literal of an id, for partition bounds and the legacy bound check.
func contractIdLiteral(id Id) string {
	return "'" + id.String() + "'::uuid"
}

type contractColumn struct {
	name             string
	typeName         string
	collation        string
	notNull          bool
	notNullName      string
	expression       string
	generated        string
	statisticsTarget int
}

type contractConstraint struct {
	name       string
	definition string
	validated  bool
	noInherit  bool
}

type contractIndex struct {
	name       string
	definition string
	valid      bool
	ready      bool
	primary    bool
	unique     bool
	// the primary key backed by the index, if any
	constraint           string
	constraintDefinition string
}

type contractTrigger struct {
	name       string
	definition string
	enabled    string
	row        bool
	truncate   bool
	constraint bool
}

type contractForeignKey struct {
	name            string
	table           string
	referencedTable string
	definition      string
}

// One contract table as the catalog describes it.
type contractTableShape struct {
	table      string
	relkind    string
	columns    []contractColumn
	checks     []contractConstraint
	indexes    []contractIndex
	triggers   []contractTrigger
	reloptions []string
	// outgoing and incoming foreign keys
	foreignKeys  []contractForeignKey
	referencedBy []contractForeignKey
	// the prepared legacy bound check, if any
	legacyBound *contractConstraint
}

// A connection or a transaction.
type contractCatalogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readContractTableShape(ctx context.Context, conn contractCatalogQuerier, table string) (shape *contractTableShape) {
	shape = &contractTableShape{table: table}
	exists := false
	result, err := conn.Query(ctx, `SELECT relkind::text, COALESCE(reloptions, ARRAY[]::text[]) FROM pg_class WHERE oid = to_regclass($1)`, "public."+table)
	WithPgResult(result, err, func() {
		if result.Next() {
			Raise(result.Scan(&shape.relkind, &shape.reloptions))
			exists = true
		}
	})
	if !exists {
		return nil
	}

	result, err = conn.Query(
		ctx,
		`
		SELECT
			a.attname,
			format_type(a.atttypid, a.atttypmod),
			CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation THEN quote_ident(n.nspname) || '.' || quote_ident(c.collname) ELSE '' END,
			a.attnotnull,
			COALESCE((
				SELECT nn.conname FROM pg_constraint nn
				WHERE nn.conrelid = a.attrelid AND nn.contype = 'n' AND nn.conkey = ARRAY[a.attnum]
			), ''),
			COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
			a.attgenerated::text,
			COALESCE(a.attstattarget::int, -1)
		FROM pg_attribute a
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_collation c ON c.oid = a.attcollation
		LEFT JOIN pg_namespace n ON n.oid = c.collnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = to_regclass($1) AND 0 < a.attnum AND NOT a.attisdropped
		ORDER BY a.attnum
		`,
		"public."+table,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var column contractColumn
			Raise(result.Scan(
				&column.name,
				&column.typeName,
				&column.collation,
				&column.notNull,
				&column.notNullName,
				&column.expression,
				&column.generated,
				&column.statisticsTarget,
			))
			shape.columns = append(shape.columns, column)
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT conname, pg_get_constraintdef(oid), convalidated, connoinherit
		FROM pg_constraint
		WHERE conrelid = to_regclass($1) AND contype = 'c'
		ORDER BY conname
		`,
		"public."+table,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var check contractConstraint
			Raise(result.Scan(&check.name, &check.definition, &check.validated, &check.noInherit))
			if check.name == contractLegacyBoundName(table) {
				shape.legacyBound = &check
			} else {
				shape.checks = append(shape.checks, check)
			}
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT
			c.relname,
			pg_get_indexdef(i.indexrelid),
			i.indisvalid,
			i.indisready,
			i.indisprimary,
			i.indisunique,
			COALESCE((SELECT con.conname FROM pg_constraint con WHERE con.conindid = i.indexrelid AND con.conrelid = i.indrelid), ''),
			COALESCE((SELECT pg_get_constraintdef(con.oid) FROM pg_constraint con WHERE con.conindid = i.indexrelid AND con.conrelid = i.indrelid), '')
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = to_regclass($1)
		ORDER BY c.relname
		`,
		"public."+table,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var index contractIndex
			Raise(result.Scan(
				&index.name,
				&index.definition,
				&index.valid,
				&index.ready,
				&index.primary,
				&index.unique,
				&index.constraint,
				&index.constraintDefinition,
			))
			shape.indexes = append(shape.indexes, index)
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT
			tgname,
			pg_get_triggerdef(oid),
			tgenabled::text,
			(tgtype & 1) <> 0,
			(tgtype & 32) <> 0,
			tgconstraint <> 0
		FROM pg_trigger
		WHERE tgrelid = to_regclass($1) AND NOT tgisinternal AND tgparentid = 0
		ORDER BY tgname
		`,
		"public."+table,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var trigger contractTrigger
			Raise(result.Scan(
				&trigger.name,
				&trigger.definition,
				&trigger.enabled,
				&trigger.row,
				&trigger.truncate,
				&trigger.constraint,
			))
			shape.triggers = append(shape.triggers, trigger)
		}
	})

	readForeignKeys := func(column string) (foreignKeys []contractForeignKey) {
		result, err := conn.Query(
			ctx,
			fmt.Sprintf(
				`
				SELECT conname, conrelid::regclass::text, confrelid::regclass::text, pg_get_constraintdef(oid)
				FROM pg_constraint
				WHERE %s = to_regclass($1) AND contype = 'f' AND conparentid = 0
				ORDER BY conname
				`,
				column,
			),
			"public."+table,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var foreignKey contractForeignKey
				Raise(result.Scan(&foreignKey.name, &foreignKey.table, &foreignKey.referencedTable, &foreignKey.definition))
				foreignKeys = append(foreignKeys, foreignKey)
			}
		})
		return
	}
	shape.foreignKeys = readForeignKeys("conrelid")
	shape.referencedBy = readForeignKeys("confrelid")
	return
}

var contractIndexDefinitionPattern = regexp.MustCompile(`^CREATE (UNIQUE )?INDEX (\S+) ON (ONLY )?(\S+) (USING .*)$`)

// The index definition on another table under another name, or false if the
// definition is not a plain CREATE INDEX.
func contractRetargetIndex(definition string, name string, table string) (string, bool) {
	groups := contractIndexDefinitionPattern.FindStringSubmatch(strings.Join(strings.Fields(definition), " "))
	if groups == nil {
		return "", false
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON public.%s %s", groups[1], quoteIdentifier(name), quoteIdentifier(table), groups[5]), true
}

// The part of an index definition that ignores its name, its table and ONLY.
func contractIndexShape(definition string) string {
	groups := contractIndexDefinitionPattern.FindStringSubmatch(strings.Join(strings.Fields(definition), " "))
	if groups == nil {
		return definition
	}
	return groups[1] + groups[5]
}

// The trigger definition on another table. pg_get_triggerdef names the table
// once, in the ON clause, before the timing of each row or statement.
func contractRetargetTrigger(definition string, fromTable string, toTable string) (string, bool) {
	from := " ON public." + fromTable + " "
	if strings.Count(definition, from) != 1 {
		return "", false
	}
	return strings.Replace(definition, from, " ON public."+quoteIdentifier(toTable)+" ", 1), true
}

func (self contractColumn) definition() string {
	var b strings.Builder
	b.WriteString(quoteIdentifier(self.name))
	b.WriteString(" ")
	b.WriteString(self.typeName)
	if self.collation != "" {
		b.WriteString(" COLLATE ")
		b.WriteString(self.collation)
	}
	switch self.generated {
	case "s":
		fmt.Fprintf(&b, " GENERATED ALWAYS AS (%s) STORED", self.expression)
	case "v":
		fmt.Fprintf(&b, " GENERATED ALWAYS AS (%s) VIRTUAL", self.expression)
	default:
		if self.expression != "" {
			fmt.Fprintf(&b, " DEFAULT %s", self.expression)
		}
	}
	if self.notNull {
		if self.notNullName != "" {
			fmt.Fprintf(&b, " CONSTRAINT %s NOT NULL", quoteIdentifier(self.notNullName))
		} else {
			b.WriteString(" NOT NULL")
		}
	}
	return b.String()
}

// The statements that create a table with shape's columns and checks. A
// partitioned parent keeps each check's validity; a new empty partition
// validates them all. Indexes, triggers and storage settings come separately.
func (self *contractTableShape) createTableSql(name string, partitioned bool) (statements []string) {
	columns := make([]string, 0, len(self.columns))
	for _, column := range self.columns {
		columns = append(columns, "\t"+column.definition())
	}
	create := fmt.Sprintf("CREATE TABLE public.%s (\n%s\n)", quoteIdentifier(name), strings.Join(columns, ",\n"))
	if partitioned {
		create += " PARTITION BY RANGE (contract_id)"
	}
	statements = append(statements, create)
	for _, check := range self.checks {
		definition := check.definition
		if !partitioned {
			definition = strings.TrimSuffix(definition, " NOT VALID")
		}
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s",
			quoteIdentifier(name),
			quoteIdentifier(check.name),
			definition,
		))
	}
	for _, column := range self.columns {
		if 0 <= column.statisticsTarget {
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s ALTER COLUMN %s SET STATISTICS %d",
				quoteIdentifier(name),
				quoteIdentifier(column.name),
				column.statisticsTarget,
			))
		}
	}
	return
}

// The leaf storage settings: autovacuum and the rest of reloptions. A
// partitioned parent has none.
func (self *contractTableShape) reloptionsSql(name string) (statements []string) {
	if len(self.reloptions) == 0 {
		return
	}
	return []string{fmt.Sprintf("ALTER TABLE public.%s SET (%s)", quoteIdentifier(name), strings.Join(self.reloptions, ", "))}
}

// The statement-level TRUNCATE guards, repeated on each leaf: a statement
// trigger on the parent does not fire for a statement that names a partition.
func (self *contractTableShape) truncateGuardsSql(fromTable string, leaf string) (statements []string, err error) {
	for _, trigger := range self.triggers {
		if !trigger.truncate {
			continue
		}
		definition, ok := contractRetargetTrigger(trigger.definition, fromTable, leaf)
		if !ok {
			return nil, fmt.Errorf("cannot retarget trigger %s of %s", trigger.name, fromTable)
		}
		statements = append(statements, definition)
	}
	return
}

func contractTableShapeIndex(shape *contractTableShape, name string) *contractIndex {
	for i := range shape.indexes {
		if shape.indexes[i].name == name {
			return &shape.indexes[i]
		}
	}
	return nil
}

func contractTableShapeColumnsEqual(a []contractColumn, b []contractColumn) bool {
	return slices.EqualFunc(a, b, func(x contractColumn, y contractColumn) bool {
		return x.name == y.name &&
			x.typeName == y.typeName &&
			x.collation == y.collation &&
			x.notNull == y.notNull &&
			x.expression == y.expression &&
			x.generated == y.generated
	})
}
