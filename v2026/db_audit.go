package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Schema audit: compare the schema a set of migrations is supposed to produce
// against the schema the live DB actually has.
//
// The "expected" schema is built by replaying the local SQL migrations into a
// throwaway temp database and introspecting it -- the same mechanism the test
// harness uses (see test_util.go). Building expected this way (rather than
// parsing the migration SQL by hand) means Postgres itself resolves generated
// columns, identifier truncation, canonical index definitions, etc., so the
// diff has no parser false positives.
//
// Code migrations are intentionally NOT replayed: all of them are data
// backfills with no effect on the catalog (and no-ops on an empty DB), so
// skipping them keeps the audit from needing redis or mutating shared state.

type auditColumn struct {
	name       string
	typ        string
	notNull    bool
	generated  bool
	defaultSql string
}

type auditIndex struct {
	table string
	def   string
}

type auditConstraint struct {
	name string
	typ  string // p (primary key), u (unique), c (check), f (foreign key)
	def  string
}

// SchemaSnapshot is the introspected shape of one database.
type SchemaSnapshot struct {
	tables      map[string]bool
	columns     map[string][]auditColumn
	indexes     map[string]auditIndex
	constraints map[string][]auditConstraint
}

// introspectSchema reads the public-schema shape of the database the global
// pool currently points at. Partition children (relispartition) are excluded --
// they are created at runtime, not by a migration, so they are not part of the
// expected schema. Constraint-backed indexes (PRIMARY KEY / UNIQUE / exclusion)
// are excluded from the index set too: they are reconciled through their
// constraint (a missing table's CREATE TABLE recreates them), so emitting them
// as standalone CREATE INDEX would double-create -- and a constraint's index
// cannot be dropped with DROP INDEX either.
func introspectSchema(ctx context.Context) *SchemaSnapshot {
	s := &SchemaSnapshot{
		tables:      map[string]bool{},
		columns:     map[string][]auditColumn{},
		indexes:     map[string]auditIndex{},
		constraints: map[string][]auditConstraint{},
	}

	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT c.relname
            FROM pg_class c
            JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
              AND c.relname <> 'migration_audit'
            `,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var table string
				Raise(result.Scan(&table))
				s.tables[table] = true
			}
		})
	})

	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT
                c.relname,
                a.attname,
                format_type(a.atttypid, a.atttypmod),
                a.attnotnull,
                a.attgenerated::text,
                COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
            FROM pg_class c
            JOIN pg_namespace n ON n.oid = c.relnamespace
            JOIN pg_attribute a ON a.attrelid = c.oid
            LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
            WHERE
                n.nspname = 'public' AND
                c.relkind IN ('r', 'p') AND NOT c.relispartition AND
                c.relname <> 'migration_audit' AND
                a.attnum > 0 AND NOT a.attisdropped
            ORDER BY c.relname, a.attnum
            `,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var table string
				var col auditColumn
				var generated string
				Raise(result.Scan(&table, &col.name, &col.typ, &col.notNull, &generated, &col.defaultSql))
				col.generated = generated == "s"
				s.columns[table] = append(s.columns[table], col)
			}
		})
	})

	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT i.relname, t.relname, pg_get_indexdef(ix.indexrelid)
            FROM pg_index ix
            JOIN pg_class i ON i.oid = ix.indexrelid
            JOIN pg_class t ON t.oid = ix.indrelid
            JOIN pg_namespace n ON n.oid = i.relnamespace
            WHERE n.nspname = 'public' AND t.relkind IN ('r', 'p') AND NOT t.relispartition
              AND t.relname <> 'migration_audit'
              AND NOT EXISTS (
                  SELECT 1 FROM pg_constraint con WHERE con.conindid = ix.indexrelid
              )
            `,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				var idx auditIndex
				Raise(result.Scan(&name, &idx.table, &idx.def))
				s.indexes[name] = idx
			}
		})
	})

	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT t.relname, con.conname, con.contype::text, pg_get_constraintdef(con.oid)
            FROM pg_constraint con
            JOIN pg_class t ON t.oid = con.conrelid
            JOIN pg_namespace n ON n.oid = t.relnamespace
            WHERE
                n.nspname = 'public' AND
                t.relkind IN ('r', 'p') AND NOT t.relispartition AND
                t.relname <> 'migration_audit' AND
                con.contype IN ('p', 'u', 'c', 'f')
            ORDER BY t.relname, con.contype, con.conname
            `,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var table string
				var con auditConstraint
				Raise(result.Scan(&table, &con.name, &con.typ, &con.def))
				s.constraints[table] = append(s.constraints[table], con)
			}
		})
	})

	return s
}

// buildExpectedSchema creates a throwaway temp database, replays the local SQL
// migrations up to version upTo, introspects the result, then drops the temp
// database and restores the pools. This mirrors the temp-DB dance in
// DefaultTestEnv().Run (test_util.go) but touches only Postgres (no redis).
//
// The whole lifecycle runs over the DIRECT maintenance connection
// (pg_maintenance.yml, falling back to pg.yml), NEVER the main pool: in
// production the main pool goes through PgBouncer, which cannot CREATE DATABASE
// and does not know a brand-new temp DB. Both vault resources (pg.yml and
// pg_maintenance.yml) are repointed at the temp DB with the direct authority, so
// every pool the migrations and introspection touch lands on the temp DB and the
// audited database is only ever read, never written.
func buildExpectedSchema(ctx context.Context, upTo int) *SchemaSnapshot {
	// resolve the direct connection the same way safeMaintenancePool does:
	// pg_maintenance.yml if present, otherwise pg.yml
	pgResource := Vault.RequireSimpleResource(DefaultPgVaultResourceName)
	if r, err := Vault.SimpleResource(MaintenancePgVaultResourceName); err == nil {
		pgResource = r
	}
	pg := pgResource.Parse()

	randBytes := make([]byte, 16)
	_, err := rand.Read(randBytes)
	Raise(err)
	tempDbName := fmt.Sprintf("audit_%d_%s", NowUtc().UnixMilli(), hex.EncodeToString(randBytes))

	// create the temp DB over the direct maintenance connection (connected to the
	// real DB; CREATE DATABASE runs outside a transaction)
	MaintenanceDb(ctx, func(conn PgConn) {
		_, err := conn.Exec(
			ctx,
			fmt.Sprintf(
				`
                CREATE DATABASE %s
                WITH
                    OWNER=%s
                    ENCODING=UTF8
                    LC_COLLATE='en_US.UTF-8'
                    LC_CTYPE='en_US.UTF-8'
                    TEMPLATE='template0'
                `,
				tempDbName,
				pg["user"],
			),
		)
		Raise(err)
	}, OptReadWrite())

	// repoint BOTH pools at the temp DB using the direct authority
	tempConfig := []byte(fmt.Sprintf(
		`
authority: "%s"
user: "%s"
password: "%s"
db: "%s"`,
		pg["authority"],
		pg["user"],
		pg["password"],
		tempDbName,
	))
	popPg := Vault.PushSimpleResource(DefaultPgVaultResourceName, tempConfig)
	popMaintenance := Vault.PushSimpleResource(MaintenancePgVaultResourceName, tempConfig)
	PgReset()

	// always restore the real pools and drop the temp database, even on panic
	defer func() {
		popMaintenance()
		popPg()
		PgReset()
		MaintenanceDb(ctx, func(conn PgConn) {
			_, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE %s`, tempDbName))
			Raise(err)
		}, OptReadWrite())
	}()

	applyAuditMigrations(ctx, upTo)

	return introspectSchema(ctx)
}

// applyAuditMigrations replays the SQL migrations at slice indices [0, upTo)
// onto the database the pool currently points at. Code migrations are skipped
// (see the package comment above).
func applyAuditMigrations(ctx context.Context, upTo int) {
	if upTo > len(migrations) {
		upTo = len(migrations)
	}
	for i := 0; i < upTo; i += 1 {
		if m, ok := migrations[i].(*SqlMigration); ok {
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, m.sql))
			})
		}
	}
}

type indexChange struct {
	name        string
	expectedDef string
	actualDef   string
}

// SchemaDiff is the difference of an actual database against an expected one.
// "missing" = expected but absent from the DB; "extra" = present in the DB but
// not expected.
type SchemaDiff struct {
	missingTables      []string
	extraTables        []string
	missingCols        map[string][]auditColumn
	extraCols          map[string][]string
	missingConstraints map[string][]auditConstraint
	extraConstraints   map[string][]string
	missingIdx         []string
	extraIdx           []string
	changedIdx         []indexChange

	// adoptConstraintIndex[name] means a missing PK/UNIQUE constraint should be
	// added with ADD CONSTRAINT ... USING INDEX name, because a bare index of that
	// name already exists (e.g. left by an earlier partial fix). Such indexes are
	// kept out of extraIdx -- they are adopted, not dropped.
	adoptConstraintIndex map[string]bool

	expected *SchemaSnapshot
	actual   *SchemaSnapshot
}

func diffSchemas(expected, actual *SchemaSnapshot) *SchemaDiff {
	d := &SchemaDiff{
		missingCols:        map[string][]auditColumn{},
		extraCols:          map[string][]string{},
		missingConstraints: map[string][]auditConstraint{},
		extraConstraints:   map[string][]string{},
		expected:           expected,
		actual:             actual,
	}

	for table := range expected.tables {
		if !actual.tables[table] {
			d.missingTables = append(d.missingTables, table)
		}
	}
	for table := range actual.tables {
		if !expected.tables[table] {
			d.extraTables = append(d.extraTables, table)
		}
	}

	for table := range expected.tables {
		if !actual.tables[table] {
			continue
		}
		actualCols := map[string]bool{}
		for _, c := range actual.columns[table] {
			actualCols[c.name] = true
		}
		expectedCols := map[string]bool{}
		for _, c := range expected.columns[table] {
			expectedCols[c.name] = true
			if !actualCols[c.name] {
				d.missingCols[table] = append(d.missingCols[table], c)
			}
		}
		for _, c := range actual.columns[table] {
			if !expectedCols[c.name] {
				d.extraCols[table] = append(d.extraCols[table], c.name)
			}
		}

		// constraints (missing-table constraints come with its CREATE TABLE, so
		// only compare tables present in both)
		actualCons := map[string]bool{}
		for _, con := range actual.constraints[table] {
			actualCons[con.name] = true
		}
		expectedCons := map[string]bool{}
		for _, con := range expected.constraints[table] {
			expectedCons[con.name] = true
			if !actualCons[con.name] {
				d.missingConstraints[table] = append(d.missingConstraints[table], con)
			}
		}
		for _, con := range actual.constraints[table] {
			if !expectedCons[con.name] {
				d.extraConstraints[table] = append(d.extraConstraints[table], con.name)
			}
		}
	}

	for name, idx := range expected.indexes {
		actualIdx, ok := actual.indexes[name]
		if !ok {
			d.missingIdx = append(d.missingIdx, name)
		} else if normalizeIndexDef(actualIdx.def) != normalizeIndexDef(idx.def) {
			d.changedIdx = append(d.changedIdx, indexChange{
				name:        name,
				expectedDef: idx.def,
				actualDef:   actualIdx.def,
			})
		}
	}
	for name := range actual.indexes {
		if _, ok := expected.indexes[name]; !ok {
			d.extraIdx = append(d.extraIdx, name)
		}
	}

	// pair an extra bare index with a missing PK/UNIQUE constraint of the same
	// name (a partial fix left a CREATE UNIQUE INDEX where a constraint belongs):
	// adopt the index into the constraint via ADD CONSTRAINT ... USING INDEX
	// rather than dropping+recreating it, and keep it out of extraIdx.
	d.adoptConstraintIndex = map[string]bool{}
	for _, cons := range d.missingConstraints {
		for _, con := range cons {
			if con.typ != "p" && con.typ != "u" {
				continue
			}
			if _, ok := actual.indexes[con.name]; ok {
				d.adoptConstraintIndex[con.name] = true
			}
		}
	}
	if 0 < len(d.adoptConstraintIndex) {
		kept := d.extraIdx[:0]
		for _, name := range d.extraIdx {
			if !d.adoptConstraintIndex[name] {
				kept = append(kept, name)
			}
		}
		d.extraIdx = kept
	}

	sort.Strings(d.missingTables)
	sort.Strings(d.extraTables)
	sort.Strings(d.missingIdx)
	sort.Strings(d.extraIdx)
	sort.Slice(d.changedIdx, func(i, j int) bool { return d.changedIdx[i].name < d.changedIdx[j].name })

	return d
}

// normalizeIndexDef canonicalizes an index definition for comparison: it
// collapses whitespace and drops the ONLY marker that pg_get_indexdef emits for
// a partitioned table's parent index ("CREATE INDEX ... ON ONLY tbl ..."). A
// table that is partitioned at runtime (e.g. client_reliability, whose daily
// partitions are created by the maintenance task, not a migration) but built as
// a plain table by the migrations would otherwise report every one of its
// indexes as changed purely because of that marker -- the indexed columns are
// identical. Only the first " ON ONLY " (the table clause, which precedes the
// column list and any WHERE) is stripped.
func normalizeIndexDef(def string) string {
	def = strings.Join(strings.Fields(def), " ")
	return strings.Replace(def, " ON ONLY ", " ON ", 1)
}

// HasDifferences reports whether the DB diverges from the expected schema.
func (d *SchemaDiff) HasDifferences() bool {
	return len(d.missingTables) > 0 ||
		len(d.extraTables) > 0 ||
		len(d.missingCols) > 0 ||
		len(d.extraCols) > 0 ||
		len(d.missingConstraints) > 0 ||
		len(d.extraConstraints) > 0 ||
		len(d.missingIdx) > 0 ||
		len(d.extraIdx) > 0 ||
		len(d.changedIdx) > 0
}

// Report renders the diff as a human-readable audit.
func (d *SchemaDiff) Report() string {
	var b strings.Builder

	if len(d.missingTables) > 0 {
		b.WriteString("\nTABLES missing from DB (expected at this version, absent):\n")
		for _, t := range d.missingTables {
			fmt.Fprintf(&b, "  - %s\n", t)
		}
	}
	if len(d.extraTables) > 0 {
		b.WriteString("\nTABLES extra in DB (present, not expected at this version):\n")
		for _, t := range d.extraTables {
			fmt.Fprintf(&b, "  + %s\n", t)
		}
	}

	colTables := unionKeys(d.missingCols, d.extraCols)
	if len(colTables) > 0 {
		b.WriteString("\nCOLUMNS (tables present in both):\n")
		for _, t := range colTables {
			fmt.Fprintf(&b, "  %s:\n", t)
			for _, c := range d.missingCols[t] {
				fmt.Fprintf(&b, "    - missing: %s %s\n", c.name, c.typ)
			}
			for _, c := range d.extraCols[t] {
				fmt.Fprintf(&b, "    + extra:   %s\n", c)
			}
		}
	}

	conTables := unionKeys(d.missingConstraints, d.extraConstraints)
	if len(conTables) > 0 {
		b.WriteString("\nCONSTRAINTS (tables present in both):\n")
		for _, t := range conTables {
			fmt.Fprintf(&b, "  %s:\n", t)
			for _, con := range d.missingConstraints[t] {
				fmt.Fprintf(&b, "    - missing: %s %s\n", con.name, con.def)
			}
			for _, name := range d.extraConstraints[t] {
				fmt.Fprintf(&b, "    + extra:   %s\n", name)
			}
		}
	}

	if len(d.missingIdx) > 0 {
		b.WriteString("\nINDEXES missing from DB (expected at this version, absent):\n")
		for _, name := range d.missingIdx {
			fmt.Fprintf(&b, "  - %s  ON %s\n", name, d.expected.indexes[name].table)
		}
	}
	if len(d.extraIdx) > 0 {
		b.WriteString("\nINDEXES extra in DB (present, not expected at this version):\n")
		for _, name := range d.extraIdx {
			fmt.Fprintf(&b, "  + %s  ON %s\n", name, d.actual.indexes[name].table)
		}
	}
	if len(d.changedIdx) > 0 {
		b.WriteString("\nINDEXES with a definition that differs from expected:\n")
		for _, ic := range d.changedIdx {
			fmt.Fprintf(&b, "  ~ %s\n      expected: %s\n      actual:   %s\n", ic.name, ic.expectedDef, ic.actualDef)
		}
	}

	if !d.HasDifferences() {
		b.WriteString("\nNo differences: the DB matches the expected schema at this version.\n")
	}

	return b.String()
}

// FixSql renders the full reconciliation as SQL in three tiers, all shown for a
// dry run: the additive changes `db audit --fix` applies (uncommented), the
// index drops/recreations `db audit --fix --force-drop-indexes` also applies
// (commented), and the table/column drops that are never applied automatically
// (commented). `db audit` (no --fix) prints this after the report.
func (d *SchemaDiff) FixSql() string {
	var b strings.Builder

	b.WriteString("-- Reconciliation SQL (dry run). `db audit --fix` applies the additive changes\n")
	b.WriteString("-- below. Large-table indexes should be created with CREATE INDEX CONCURRENTLY out\n")
	b.WriteString("-- of band; the plain CREATE INDEX statements match db_migrations.go.\n")
	d.writeAdditiveSql(&b)

	if d.hasIndexDrops() {
		b.WriteString("\n\n-- Index changes. `db audit --fix --force-drop-indexes` also applies these\n")
		b.WriteString("-- (an index is safe to drop and recreate):")
		d.writeIndexDropSql(&b)
	}

	if d.hasDataDrops() {
		b.WriteString("\n\n-- Extra objects in the DB, not defined by the migrations. NEVER dropped\n")
		b.WriteString("-- automatically -- review and apply by hand (a table/column drop loses data;\n")
		b.WriteString("-- a constraint drop removes an integrity guarantee):")
		d.writeDataDropSql(&b)
	}

	b.WriteString("\n")
	return b.String()
}

// NotAppliedSql renders the destructive changes `db audit --fix` did NOT apply,
// commented, for manual review. When forceDropIndexes is false the index
// drops/recreations are included (they were not applied); the table/column drops
// are always included. Empty when there is nothing left to review.
func (d *SchemaDiff) NotAppliedSql(forceDropIndexes bool) string {
	var b strings.Builder

	if !forceDropIndexes && d.hasIndexDrops() {
		b.WriteString("\n-- Index changes not applied (re-run --fix with --force-drop-indexes to apply):")
		d.writeIndexDropSql(&b)
	}
	if d.hasDataDrops() {
		b.WriteString("\n-- Data changes not applied (review and apply by hand -- data loss):")
		d.writeDataDropSql(&b)
	}

	if strings.TrimSpace(b.String()) == "" {
		return ""
	}
	return b.String() + "\n"
}

func (d *SchemaDiff) hasIndexDrops() bool {
	return 0 < len(d.changedIdx) || 0 < len(d.extraIdx)
}

func (d *SchemaDiff) hasDataDrops() bool {
	return 0 < len(d.extraTables) || 0 < len(d.extraCols) || 0 < len(d.extraConstraints)
}

func (d *SchemaDiff) writeAdditiveSql(b *strings.Builder) {
	for _, t := range d.missingTables {
		b.WriteString("\n")
		b.WriteString(d.createTableSql(t))
		b.WriteString("\n")
	}

	for _, t := range sortedMapKeys(d.missingCols) {
		for _, c := range d.missingCols[t] {
			fmt.Fprintf(b, "\nALTER TABLE %s ADD COLUMN %s;", t, columnDefSql(c))
		}
	}

	for _, t := range sortedMapKeys(d.missingConstraints) {
		for _, con := range d.missingConstraints[t] {
			fmt.Fprintf(b, "\n%s;", addConstraintSql(t, con, d.adoptConstraintIndex[con.name]))
		}
	}

	for _, name := range d.missingIdx {
		fmt.Fprintf(b, "\n%s;", d.expected.indexes[name].def)
	}
}

// writeIndexDropSql emits the index reconciliation as commented SQL: each
// changed index dropped and recreated with its expected definition, then each
// extra index dropped. These are what --force-drop-indexes applies.
func (d *SchemaDiff) writeIndexDropSql(b *strings.Builder) {
	for _, ic := range d.changedIdx {
		fmt.Fprintf(b, "\n--   DROP INDEX %s;", ic.name)
		fmt.Fprintf(b, "\n--   %s;", ic.expectedDef)
	}
	for _, name := range d.extraIdx {
		fmt.Fprintf(b, "\n--   DROP INDEX %s;", name)
	}
}

// writeDataDropSql emits the table/column/constraint drops as commented SQL.
// These are never applied automatically.
func (d *SchemaDiff) writeDataDropSql(b *strings.Builder) {
	for _, t := range sortedMapKeys(d.extraConstraints) {
		for _, name := range d.extraConstraints[t] {
			fmt.Fprintf(b, "\n--   ALTER TABLE %s DROP CONSTRAINT %s;", t, name)
		}
	}
	for _, t := range sortedMapKeys(d.extraCols) {
		for _, c := range d.extraCols[t] {
			fmt.Fprintf(b, "\n--   ALTER TABLE %s DROP COLUMN %s;", t, c)
		}
	}
	for _, t := range d.extraTables {
		fmt.Fprintf(b, "\n--   DROP TABLE %s;", t)
	}
}

// FixStatements returns the additive reconciliation statements -- CREATE TABLE
// for missing tables, ALTER TABLE ADD COLUMN for missing columns, CREATE INDEX
// for missing indexes -- in dependency-friendly order (tables, then columns,
// then indexes). These are exactly what ApplySchemaFix runs. Destructive changes
// (DROP, changed-index replacement, DROP TABLE/COLUMN) are excluded -- index
// drops are handled by IndexDropStatements under --force-drop-indexes, and
// table/column drops are never applied.
func (d *SchemaDiff) FixStatements() []string {
	statements := []string{}
	for _, t := range d.missingTables {
		statements = append(statements, d.createTableSql(t))
	}
	for _, t := range sortedMapKeys(d.missingCols) {
		for _, c := range d.missingCols[t] {
			statements = append(statements, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", t, columnDefSql(c)))
		}
	}
	for _, t := range sortedMapKeys(d.missingConstraints) {
		for _, con := range d.missingConstraints[t] {
			statements = append(statements, addConstraintSql(t, con, d.adoptConstraintIndex[con.name]))
		}
	}
	for _, name := range d.missingIdx {
		statements = append(statements, d.expected.indexes[name].def)
	}
	return statements
}

// addConstraintSql renders an ALTER TABLE ADD CONSTRAINT for a missing
// constraint. When adopt is set (a PK/UNIQUE constraint whose backing-index name
// already exists as a bare index), it promotes that index with ADD CONSTRAINT
// ... USING INDEX instead of building a new one -- which would collide on the
// index name (SQLSTATE 42P07).
func addConstraintSql(table string, con auditConstraint, adopt bool) string {
	if adopt {
		kind := "UNIQUE"
		if con.typ == "p" {
			kind = "PRIMARY KEY"
		}
		return fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s USING INDEX %s", table, con.name, kind, con.name)
	}
	return fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s", table, con.name, con.def)
}

// IndexDropStatements returns the index reconciliation statements applied under
// --force-drop-indexes: each changed index dropped and recreated with its
// expected definition, then each extra index dropped. An index is safe to drop
// and recreate (no data loss), unlike a table or column. DROP INDEX IF EXISTS is
// used so a re-run is idempotent.
func (d *SchemaDiff) IndexDropStatements() []string {
	statements := []string{}
	for _, ic := range d.changedIdx {
		statements = append(statements, "DROP INDEX IF EXISTS "+ic.name)
		statements = append(statements, ic.expectedDef)
	}
	for _, name := range d.extraIdx {
		statements = append(statements, "DROP INDEX IF EXISTS "+name)
	}
	return statements
}

// IndexDropCount is the number of index reconciliation changes (extra indexes to
// drop plus indexes whose definition differs). DataDropCount is the number of
// table/column drops, which are never applied.
func (d *SchemaDiff) IndexDropCount() int {
	return len(d.extraIdx) + len(d.changedIdx)
}

func (d *SchemaDiff) DataDropCount() int {
	n := len(d.extraTables)
	for _, cols := range d.extraCols {
		n += len(cols)
	}
	for _, cons := range d.extraConstraints {
		n += len(cons)
	}
	return n
}

// createTableSql reconstructs a CREATE TABLE for a table missing from the DB,
// using the expected snapshot. Primary key / unique / check constraints are
// inlined; foreign keys are emitted as trailing ALTERs to sidestep table
// ordering. It is review-quality DDL, not a substitute for the migration.
func (d *SchemaDiff) createTableSql(table string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", table)

	lines := []string{}
	for _, c := range d.expected.columns[table] {
		lines = append(lines, "    "+columnDefSql(c))
	}
	fks := []auditConstraint{}
	for _, con := range d.expected.constraints[table] {
		if con.typ == "f" {
			fks = append(fks, con)
			continue
		}
		lines = append(lines, "    "+con.def)
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n);")

	for _, fk := range fks {
		fmt.Fprintf(&b, "\nALTER TABLE %s ADD CONSTRAINT %s %s;", table, fk.name, fk.def)
	}
	return b.String()
}

func columnDefSql(c auditColumn) string {
	s := c.name + " " + c.typ
	if c.generated {
		s += " GENERATED ALWAYS AS (" + c.defaultSql + ") STORED"
		if c.notNull {
			s += " NOT NULL"
		}
		return s
	}
	if c.defaultSql != "" {
		s += " DEFAULT " + c.defaultSql
	}
	if c.notNull {
		s += " NOT NULL"
	}
	return s
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unionKeys[A any, B any](a map[string]A, b map[string]B) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SchemaAuditResult is the outcome of AuditSchema.
type SchemaAuditResult struct {
	// DbVersion is the migration version the live DB currently records.
	DbVersion int
	// LocalVersion is the head version of the local db_migrations.go -- the
	// version the expected schema is always built to.
	LocalVersion int
	Diff         *SchemaDiff
}

// AuditSchema diffs the live DB against the schema the full local
// db_migrations.go head should produce. `db audit` reports this diff (and prints
// the reconciling SQL as a dry run); `db audit --fix` applies the additive part
// of it. Building to the head means the report/fix cover both genuine drift and
// migrations the DB has not caught up to yet.
func AuditSchema(ctx context.Context) *SchemaAuditResult {
	dbVersion := DbVersion(ctx)
	localVersion := MigrationCount()

	// introspect the real DB first, before buildExpectedSchema repoints the pools
	actual := introspectSchema(ctx)
	expected := buildExpectedSchema(ctx, localVersion)

	return &SchemaAuditResult{
		DbVersion:    dbVersion,
		LocalVersion: localVersion,
		Diff:         diffSchemas(expected, actual),
	}
}

// ApplySchemaFix executes the reconciliation statements against the database
// the maintenance pool currently points at -- the audited (real) database, since
// AuditSchema has already restored the pools by the time this is called. It runs
// the additive statements (FixStatements) always, and the index drops/recreations
// (IndexDropStatements) when forceDropIndexes is true. It never drops a table or
// column. Each statement runs in its own transaction, so progress survives a
// mid-run failure and index builds do not share one long-held lock. onStatement,
// if non-nil, is called just before each statement runs (for progress reporting).
//
// Note: missing indexes are created exactly as the migrations define them (plain
// CREATE INDEX, which takes a write lock for the build). For a large table,
// prefer creating that index out of band with CREATE INDEX CONCURRENTLY and
// re-running the audit -- inspect the statements first with plain `db audit`.
func ApplySchemaFix(ctx context.Context, diff *SchemaDiff, forceDropIndexes bool, onStatement func(index int, total int, statement string)) {
	statements := diff.FixStatements()
	if forceDropIndexes {
		statements = append(statements, diff.IndexDropStatements()...)
	}
	for i, statement := range statements {
		if onStatement != nil {
			onStatement(i, len(statements), statement)
		}
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, statement))
		})
	}
}
