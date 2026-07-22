package server

import (
	"context"
	"strings"
	"testing"
)

// TestSchemaDiffAndFix exercises the diff + fix-SQL generation with hand-built
// snapshots (no database). It checks that missing/extra tables, columns, and
// indexes are all detected, that additive fixes are emitted ready to run, and
// that every destructive fix is commented out.
func TestSchemaDiffAndFix(t *testing.T) {
	expected := &SchemaSnapshot{
		tables: map[string]bool{"keep": true, "added_table": true},
		columns: map[string][]auditColumn{
			"keep":        {{name: "id", typ: "bigint", notNull: true}, {name: "new_col", typ: "text"}},
			"added_table": {{name: "x", typ: "integer", notNull: true}},
		},
		indexes: map[string]auditIndex{
			"keep_pkey":    {table: "keep", def: "CREATE UNIQUE INDEX keep_pkey ON public.keep USING btree (id)"},
			"keep_new_idx": {table: "keep", def: "CREATE INDEX keep_new_idx ON public.keep USING btree (new_col)"},
		},
		constraints: map[string][]auditConstraint{
			"added_table": {{name: "added_table_pkey", typ: "p", def: "PRIMARY KEY (x)"}},
		},
	}
	actual := &SchemaSnapshot{
		tables: map[string]bool{"keep": true, "stale_table": true},
		columns: map[string][]auditColumn{
			"keep":        {{name: "id", typ: "bigint", notNull: true}, {name: "stale_col", typ: "text"}},
			"stale_table": {{name: "y", typ: "integer"}},
		},
		indexes: map[string]auditIndex{
			"keep_pkey": {table: "keep", def: "CREATE UNIQUE INDEX keep_pkey ON public.keep USING btree (id)"},
			"stale_idx": {table: "keep", def: "CREATE INDEX stale_idx ON public.keep USING btree (id)"},
		},
		constraints: map[string][]auditConstraint{},
	}

	d := diffSchemas(expected, actual)

	if !contains(d.missingTables, "added_table") {
		t.Fatalf("missingTables = %v, want it to contain added_table", d.missingTables)
	}
	if !contains(d.extraTables, "stale_table") {
		t.Fatalf("extraTables = %v, want it to contain stale_table", d.extraTables)
	}
	if !colContains(d.missingCols["keep"], "new_col") {
		t.Fatalf("missingCols[keep] = %v, want it to contain new_col", d.missingCols["keep"])
	}
	if !contains(d.extraCols["keep"], "stale_col") {
		t.Fatalf("extraCols[keep] = %v, want it to contain stale_col", d.extraCols["keep"])
	}
	if !contains(d.missingIdx, "keep_new_idx") {
		t.Fatalf("missingIdx = %v, want it to contain keep_new_idx", d.missingIdx)
	}
	if !contains(d.extraIdx, "stale_idx") {
		t.Fatalf("extraIdx = %v, want it to contain stale_idx", d.extraIdx)
	}
	if !d.HasDifferences() {
		t.Fatalf("HasDifferences() = false, want true")
	}

	fix := d.FixSql()
	for _, want := range []string{
		"CREATE TABLE added_table (",
		"PRIMARY KEY (x)",
		"ALTER TABLE keep ADD COLUMN new_col text;",
		"CREATE INDEX keep_new_idx ON public.keep USING btree (new_col);",
		"--   DROP TABLE stale_table;",
		"--   DROP INDEX stale_idx;",
		"--   ALTER TABLE keep DROP COLUMN stale_col;",
	} {
		if !strings.Contains(fix, want) {
			t.Fatalf("FixSql() missing %q\n---\n%s", want, fix)
		}
	}

	// no destructive statement may be emitted uncommented
	for _, line := range strings.Split(fix, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "DROP ") {
			t.Fatalf("FixSql() emitted an uncommented destructive statement: %q", line)
		}
	}

	// an identical schema must report clean
	same := diffSchemas(expected, expected)
	if same.HasDifferences() {
		t.Fatalf("diffing a schema against itself should be clean, got:\n%s", same.Report())
	}
}

// TestSchemaDiffGeneratedColumn checks that a generated STORED column missing
// from the DB is reconstructed with its generation expression, not a DEFAULT.
func TestSchemaDiffGeneratedColumn(t *testing.T) {
	expected := &SchemaSnapshot{
		tables: map[string]bool{"t": true},
		columns: map[string][]auditColumn{
			"t": {
				{name: "a", typ: "boolean", notNull: true},
				{name: "open", typ: "boolean", generated: true, defaultSql: "(a = false)"},
			},
		},
		indexes:     map[string]auditIndex{},
		constraints: map[string][]auditConstraint{},
	}
	actual := &SchemaSnapshot{
		tables:      map[string]bool{"t": true},
		columns:     map[string][]auditColumn{"t": {{name: "a", typ: "boolean", notNull: true}}},
		indexes:     map[string]auditIndex{},
		constraints: map[string][]auditConstraint{},
	}

	fix := diffSchemas(expected, actual).FixSql()
	want := "ALTER TABLE t ADD COLUMN open boolean GENERATED ALWAYS AS ((a = false)) STORED;"
	if !strings.Contains(fix, want) {
		t.Fatalf("FixSql() missing generated-column DDL %q\n---\n%s", want, fix)
	}
}

// A partitioned table's parent index renders with "ON ONLY" via pg_get_indexdef,
// while the same index on a plain table (as the migrations build it, before a
// runtime partitioning cutover) renders without it. That marker alone must not
// flag the index as changed -- the indexed columns are identical -- but a real
// definition difference still must.
func TestSchemaDiffIgnoresPartitionedOnlyMarker(t *testing.T) {
	mk := func(def string) *SchemaSnapshot {
		return &SchemaSnapshot{
			tables:      map[string]bool{"client_reliability": true},
			columns:     map[string][]auditColumn{},
			indexes:     map[string]auditIndex{"client_reliability_pkey": {table: "client_reliability", def: def}},
			constraints: map[string][]auditConstraint{},
		}
	}

	// identical except the ON ONLY marker -> not a difference
	same := diffSchemas(
		mk("CREATE UNIQUE INDEX client_reliability_pkey ON public.client_reliability USING btree (block_number, client_address_hash, client_id)"),
		mk("CREATE UNIQUE INDEX client_reliability_pkey ON ONLY public.client_reliability USING btree (block_number, client_address_hash, client_id)"),
	)
	if same.HasDifferences() {
		t.Fatalf("ON ONLY marker alone should not be a difference, got:\n%s", same.Report())
	}

	// a genuine column difference is still flagged, ON ONLY notwithstanding
	changed := diffSchemas(
		mk("CREATE UNIQUE INDEX client_reliability_pkey ON public.client_reliability USING btree (block_number)"),
		mk("CREATE UNIQUE INDEX client_reliability_pkey ON ONLY public.client_reliability USING btree (block_number, client_id)"),
	)
	if len(changed.changedIdx) != 1 {
		t.Fatalf("a real column difference must still be flagged, changedIdx = %v", changed.changedIdx)
	}
}

// TestAuditSchemaCleanOnFreshDb is an integration check: a DB with every
// migration applied must match the schema rebuilt from those same migrations.
// This guards against false positives from the introspect/rebuild pipeline
// (identifier truncation, generated columns, canonical index defs, etc.) and
// confirms code migrations are schema-neutral (the audit build skips them).
func TestAuditSchemaCleanOnFreshDb(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		if v := DbVersion(ctx); v != MigrationCount() {
			t.Fatalf("fresh test DB version = %d, want head %d", v, MigrationCount())
		}

		result := AuditSchema(ctx)
		if result.Diff.HasDifferences() {
			t.Fatalf("fully-migrated DB should match expected schema, got diffs:\n%s", result.Diff.Report())
		}
	})
}

// TestApplySchemaFixCreatesMissingIndex drives the --fix apply path end to end:
// drop an index, confirm the audit reports it missing, apply the fix, and
// confirm the re-audit is clean.
func TestApplySchemaFixCreatesMissingIndex(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		const indexName = "transfer_contract_create_time"

		indexExists := func() bool {
			exists := false
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
					indexName,
				)
				WithPgResult(result, err, func() {
					exists = result.Next()
				})
			})
			return exists
		}

		if !indexExists() {
			t.Fatalf("precondition: %s should exist on a fully-migrated DB", indexName)
		}

		// drop it -> a missing-index drift against the local head
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, "DROP INDEX "+indexName))
		})
		if indexExists() {
			t.Fatalf("%s should be dropped", indexName)
		}

		// the audit diffs against the head; the dropped index is now missing
		result := AuditSchema(ctx)
		if !contains(result.Diff.missingIdx, indexName) {
			t.Fatalf("audit did not report %s as missing; missingIdx = %v", indexName, result.Diff.missingIdx)
		}
		if len(result.Diff.FixStatements()) == 0 {
			t.Fatalf("FixStatements() should include the CREATE INDEX")
		}

		// apply the fix
		ApplySchemaFix(ctx, result.Diff, false, nil)
		if !indexExists() {
			t.Fatalf("ApplySchemaFix did not recreate %s", indexName)
		}

		// re-audit against the head -> clean
		if again := AuditSchema(ctx); again.Diff.HasDifferences() {
			t.Fatalf("after apply, DB should match the head, got diffs:\n%s", again.Diff.Report())
		}
	})
}

// TestApplySchemaFixForceDropsExtraIndex drives the --force-drop-indexes path:
// an index not defined by any migration is reported as extra, left in place by a
// plain fix, and dropped by a fix with forceDropIndexes.
func TestApplySchemaFixForceDropsExtraIndex(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		const indexName = "zz_audit_test_extra_idx"

		indexExists := func() bool {
			exists := false
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`,
					indexName,
				)
				WithPgResult(result, err, func() {
					exists = result.Next()
				})
			})
			return exists
		}

		// an index no migration defines -> reported as extra
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, "CREATE INDEX "+indexName+" ON transfer_contract (dispute)"))
		})

		result := AuditSchema(ctx)
		if !contains(result.Diff.extraIdx, indexName) {
			t.Fatalf("audit did not report %s as extra; extraIdx = %v", indexName, result.Diff.extraIdx)
		}

		// a plain fix leaves it in place (index drops are not applied)
		ApplySchemaFix(ctx, result.Diff, false, nil)
		if !indexExists() {
			t.Fatalf("plain fix should not drop the extra index")
		}

		// --force-drop-indexes drops it
		ApplySchemaFix(ctx, result.Diff, true, nil)
		if indexExists() {
			t.Fatalf("--force-drop-indexes did not drop %s", indexName)
		}

		// re-audit -> clean
		if again := AuditSchema(ctx); again.Diff.HasDifferences() {
			t.Fatalf("after force-drop, DB should match the head, got diffs:\n%s", again.Diff.Report())
		}
	})
}

// A missing table's constraint-backed indexes (PK / UNIQUE) are created by its
// CREATE TABLE, so they must NOT also appear as standalone CREATE INDEX in
// FixStatements -- otherwise the apply double-creates and fails (SQLSTATE 42P07,
// "relation already exists"). introspectSchema excludes constraint indexes, so a
// realistic snapshot carries them only as constraints; this guards that
// FixStatements emits just the CREATE TABLE.
func TestFixStatementsNoConstraintIndexDoubleCreate(t *testing.T) {
	expected := &SchemaSnapshot{
		tables: map[string]bool{"wac": true},
		columns: map[string][]auditColumn{
			"wac": {{name: "id", typ: "uuid", notNull: true}, {name: "val", typ: "text", notNull: true}},
		},
		indexes: map[string]auditIndex{},
		constraints: map[string][]auditConstraint{
			"wac": {
				{name: "wac_pkey", typ: "p", def: "PRIMARY KEY (id)"},
				{name: "wac_val_key", typ: "u", def: "UNIQUE (val)"},
			},
		},
	}
	actual := &SchemaSnapshot{
		tables:      map[string]bool{},
		columns:     map[string][]auditColumn{},
		indexes:     map[string]auditIndex{},
		constraints: map[string][]auditConstraint{},
	}

	stmts := diffSchemas(expected, actual).FixStatements()
	if len(stmts) != 1 {
		t.Fatalf("want 1 statement (just the CREATE TABLE), got %d: %v", len(stmts), stmts)
	}
	for _, want := range []string{"CREATE TABLE wac", "PRIMARY KEY (id)", "UNIQUE (val)"} {
		if !strings.Contains(stmts[0], want) {
			t.Fatalf("CREATE TABLE missing %q: %s", want, stmts[0])
		}
	}
	for _, s := range stmts {
		if strings.Contains(s, "CREATE UNIQUE INDEX") {
			t.Fatalf("constraint index emitted as a standalone statement: %s", s)
		}
	}
}

// TestIntrospectExcludesConstraintIndexes verifies the introspection query
// leaves constraint-backed indexes out of the index set while keeping plain
// migration indexes.
func TestIntrospectExcludesConstraintIndexes(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		snap := introspectSchema(context.Background())
		if _, ok := snap.indexes["transfer_contract_pkey"]; ok {
			t.Fatalf("constraint-backed transfer_contract_pkey should be excluded from the index set")
		}
		if _, ok := snap.indexes["transfer_contract_create_time"]; !ok {
			t.Fatalf("plain index transfer_contract_create_time should be in the index set")
		}
	})
}

// A constraint in the expected schema but missing from an existing table
// (present in both) is reconciled with ALTER TABLE ADD CONSTRAINT -- which
// recreates the constraint and its backing index together.
func TestFixStatementsAddsMissingConstraint(t *testing.T) {
	base := func(cons []auditConstraint) *SchemaSnapshot {
		return &SchemaSnapshot{
			tables:      map[string]bool{"t": true},
			columns:     map[string][]auditColumn{"t": {{name: "id", typ: "uuid", notNull: true}}},
			indexes:     map[string]auditIndex{},
			constraints: map[string][]auditConstraint{"t": cons},
		}
	}
	d := diffSchemas(
		base([]auditConstraint{{name: "t_pkey", typ: "p", def: "PRIMARY KEY (id)"}}),
		base(nil),
	)
	if len(d.missingConstraints["t"]) != 1 {
		t.Fatalf("want 1 missing constraint, got %v", d.missingConstraints["t"])
	}
	if !contains(d.FixStatements(), "ALTER TABLE t ADD CONSTRAINT t_pkey PRIMARY KEY (id)") {
		t.Fatalf("FixStatements missing the ADD CONSTRAINT; got %v", d.FixStatements())
	}
}

// TestApplySchemaFixAddsMissingConstraint drives the constraint path end to end
// on the real drift shape that crashed --fix: an existing table missing its
// primary key. The audit reports a missing constraint (not a missing index --
// constraint indexes are excluded), and --fix recreates it with ALTER TABLE ADD
// CONSTRAINT.
func TestApplySchemaFixAddsMissingConstraint(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		const table = "audit_contract_event"
		const constraintName = "audit_contract_event_pkey"

		constraintExists := func() bool {
			exists := false
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(ctx, `SELECT 1 FROM pg_constraint WHERE conname = $1`, constraintName)
				WithPgResult(result, err, func() {
					exists = result.Next()
				})
			})
			return exists
		}

		if !constraintExists() {
			t.Fatalf("precondition: %s should exist on a fully-migrated DB", constraintName)
		}

		// drop the PK constraint (also drops its backing index)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+constraintName))
		})
		if constraintExists() {
			t.Fatalf("%s should be dropped", constraintName)
		}

		result := AuditSchema(ctx)
		if len(result.Diff.missingConstraints[table]) == 0 {
			t.Fatalf("audit did not report the missing constraint on %s", table)
		}
		// the constraint-backed index must NOT show as a missing index
		if contains(result.Diff.missingIdx, constraintName) {
			t.Fatalf("%s should be a missing constraint, not a missing index", constraintName)
		}

		ApplySchemaFix(ctx, result.Diff, false, nil)
		if !constraintExists() {
			t.Fatalf("ApplySchemaFix did not recreate %s", constraintName)
		}

		if again := AuditSchema(ctx); again.Diff.HasDifferences() {
			t.Fatalf("after apply, DB should match the head, got diffs:\n%s", again.Diff.Report())
		}
	})
}

// TestApplySchemaFixAdoptsBareConstraintIndex reproduces the state an earlier
// partial fix left: a bare unique index named like a PK constraint, but no
// constraint. The audit must adopt the index into the constraint (ADD CONSTRAINT
// ... USING INDEX) rather than build a colliding new index (SQLSTATE 42P07) or
// drop the bare one -- even under --force-drop-indexes.
func TestApplySchemaFixAdoptsBareConstraintIndex(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		const table = "audit_contract_event"
		const name = "audit_contract_event_pkey"

		constraintExists := func() bool {
			exists := false
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(ctx, `SELECT 1 FROM pg_constraint WHERE conname = $1`, name)
				WithPgResult(result, err, func() {
					exists = result.Next()
				})
			})
			return exists
		}

		// drop the PK constraint (and its index), then leave a BARE unique index
		// of the same name -- exactly what the old CREATE-UNIQUE-INDEX fix produced
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+name))
			RaisePgResult(tx.Exec(ctx, "CREATE UNIQUE INDEX "+name+" ON "+table+" (event_id)"))
		})
		if constraintExists() {
			t.Fatalf("constraint should be gone")
		}

		result := AuditSchema(ctx)
		if !result.Diff.adoptConstraintIndex[name] {
			t.Fatalf("expected %s to be marked for adoption", name)
		}
		if contains(result.Diff.extraIdx, name) {
			t.Fatalf("bare index %s should be adopted, not reported/dropped as extra", name)
		}
		want := "ALTER TABLE " + table + " ADD CONSTRAINT " + name + " PRIMARY KEY USING INDEX " + name
		if !contains(result.Diff.FixStatements(), want) {
			t.Fatalf("FixStatements should adopt via USING INDEX; got %v", result.Diff.FixStatements())
		}

		// even with --force-drop-indexes the bare index is adopted, not dropped
		ApplySchemaFix(ctx, result.Diff, true, nil)
		if !constraintExists() {
			t.Fatalf("ApplySchemaFix did not create %s via USING INDEX", name)
		}

		if again := AuditSchema(ctx); again.Diff.HasDifferences() {
			t.Fatalf("after apply, DB should match the head, got diffs:\n%s", again.Diff.Report())
		}
	})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func colContains(cols []auditColumn, name string) bool {
	for _, c := range cols {
		if c.name == name {
			return true
		}
	}
	return false
}
