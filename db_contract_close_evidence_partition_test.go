// The close evidence partitions' pure rules (PARTITION-MAINTENANCE-PLAN.md C5):
// the family's declaration, the statements of a week's create with its move
// out of DEFAULT, the drop precondition's statement, the order of the row
// deletes' leaves, the cutover's checks and statements, the move guard's
// migration and the audit's acceptance of a prepared cutover. No database is
// read.
package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// The evidence is a registered weekly family on report_id: a DEFAULT and a
// legacy partition that are never dropped, a guarded drop under the evidence
// guard's opt-in, out of the REINDEX rotation, and its own hooks.
func TestCloseEvidencePartitionFamilyDeclaration(t *testing.T) {
	family := GetPartitionFamily("close_evidence")
	if family == nil || family != closeEvidencePartitionFamily {
		t.Fatal("the close evidence family is not registered")
	}
	if err := validatePartitionFamily(newCloseEvidencePartitionFamily()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(family.Tables, []string{"contract_close_report_evidence"}) || family.KeyColumn != "report_id" ||
		family.KeyKind != PartitionKeyId || family.Granularity != PartitionGranularityWeekly {
		t.Fatalf("family shape %+v", family)
	}
	if family.Retention != 91*24*time.Hour || family.AheadRanges != ContractPartitionAheadWeeks {
		t.Fatalf("retention %s, ahead %d", family.Retention, family.AheadRanges)
	}
	if !family.DefaultPartition || !family.KeepLegacyPartition || !family.ReindexExempt ||
		family.DropGuardSetting != "urnetwork.close_evidence_retention" {
		t.Fatalf("family rules %+v", family)
	}
	if family.CreateLeaf == nil || family.DropFence == nil || family.DropPrecondition == nil {
		t.Fatal("the family lacks its create, fence or precondition")
	}
	// leaves are the family's, and stay out of the rotation
	week := family.RangeAt(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	for _, name := range []string{
		"contract_close_report_evidence_w20261102",
		"contract_close_report_evidence_legacy",
		"contract_close_report_evidence_default",
	} {
		if PartitionFamilyOfTable(name) != family || !PartitionRegistryReindexExempt(name, false) {
			t.Errorf("%s is not a close evidence leaf out of the rotation", name)
		}
	}
	if name := family.LeafName("contract_close_report_evidence", week); name != "contract_close_report_evidence_w20261102" {
		t.Errorf("week leaf %s", name)
	}
	// the plain table keeps its place in the rotation until it is partitioned
	if PartitionRegistryReindexExempt("contract_close_report_evidence", false) || !PartitionRegistryReindexExempt("contract_close_report_evidence", true) {
		t.Error("the evidence parent's rotation exemption does not follow its layout")
	}
	settings := DefaultCloseEvidencePartitionSettings()
	if settings.LockTimeout <= 0 || settings.StatementTimeout <= 0 || settings.MaxAttempts <= 0 || settings.RetryInterval <= 0 ||
		settings.ValidateLockTimeout <= 0 || settings.MoveBatchSize <= 0 || settings.MaxAheadRows <= 0 || settings.RollbackMaxRows <= 0 ||
		settings.PrepareLeadTime <= settings.AbandonMargin || settings.AbandonMargin <= settings.FinalizeMargin || settings.FinalizeMargin <= 0 ||
		settings.LockSettings.MaxTransactionAge <= 0 {
		t.Fatalf("settings %+v", settings)
	}
}

// A week's create keeps the registry's statements, adds the key the move's
// guard looks copies up by and the index of late reports, moves the week's
// DEFAULT rows under the move opt-in, and attaches last.
func TestCloseEvidenceWeekLeafSql(t *testing.T) {
	week := PartitionGranularityWeekly.RangeOf(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	leaf := "contract_close_report_evidence_w20261102"
	create := []string{
		`CREATE TABLE public."contract_close_report_evidence_w20261102" (LIKE public."contract_close_report_evidence" INCLUDING DEFAULTS)`,
		`CREATE TRIGGER contract_close_report_evidence_truncate_guard BEFORE TRUNCATE ON public."contract_close_report_evidence_w20261102" FOR EACH STATEMENT EXECUTE FUNCTION contract_close_report_evidence_guard()`,
		`ALTER TABLE public."contract_close_report_evidence" ATTACH PARTITION public."contract_close_report_evidence_w20261102" FOR VALUES FROM ('a') TO ('b')`,
	}
	statements, move, attach, err := closeEvidenceWeekLeafSql("contract_close_report_evidence", leaf, week, create, "PRIMARY KEY (client_id, report_id)", `"client_id", "report_id"`, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(statements[:2], create[:2]) || attach != create[2] || len(statements) != 4 {
		t.Fatalf("create statements %q, attach %q", statements, attach)
	}
	if statements[2] != `ALTER TABLE public."contract_close_report_evidence_w20261102" ADD CONSTRAINT "contract_close_report_evidence_w20261102_pkey" PRIMARY KEY (client_id, report_id)` {
		t.Errorf("key %s", statements[2])
	}
	if statements[3] != `CREATE INDEX "contract_close_report_evidence_w20261102_late" ON public."contract_close_report_evidence_w20261102" USING btree (accepted_at) WHERE accepted_at >= '2026-11-10 00:00:00'::timestamp` {
		t.Errorf("late index %s", statements[3])
	}
	lower := contractIdLiteral(ContractIdFloor(week.Lower))
	upper := contractIdLiteral(ContractIdFloor(week.Upper))
	want := []string{
		`INSERT INTO public."contract_close_report_evidence_w20261102" ("client_id", "report_id") SELECT "client_id", "report_id" FROM public."contract_close_report_evidence_default" WHERE ` + lower + ` <= report_id AND report_id < ` + upper,
		`SET LOCAL urnetwork.close_evidence_retention = 'move'`,
		`DELETE FROM public."contract_close_report_evidence_default" WHERE ` + lower + ` <= report_id AND report_id < ` + upper,
		`SET LOCAL urnetwork.close_evidence_retention = ''`,
	}
	if !slices.Equal(move, want) {
		t.Fatalf("move %q\nwant %q", move, want)
	}
	// without a DEFAULT partition there is nothing to move
	_, move, _, err = closeEvidenceWeekLeafSql("contract_close_report_evidence", leaf, week, create, "PRIMARY KEY (client_id, report_id)", `"client_id"`, false)
	if err != nil || len(move) != 0 {
		t.Fatalf("move without a DEFAULT partition %q %v", move, err)
	}
	// a create that does not end with its attach is refused
	if _, _, _, err := closeEvidenceWeekLeafSql("contract_close_report_evidence", leaf, week, create[:2], "PRIMARY KEY (client_id, report_id)", `"client_id"`, true); err == nil {
		t.Fatal("a create without its attach was accepted")
	}
}

// The drop precondition is one statement over the week's own leaf: the day
// after retention, the late index's presence and newest entry, then the few
// old unresolved contracts looked up in the week.
func TestCloseEvidenceWeekBlockerSql(t *testing.T) {
	week := PartitionGranularityWeekly.RangeOf(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	sql := strings.Join(strings.Fields(closeEvidenceWeekBlockerSql("contract_close_report_evidence_w20261102", week)), " ")
	for _, want := range []string{
		`'2026-11-10 00:00:00'::timestamp THEN 'within a day of its retention'`,
		`to_regclass('public.contract_close_report_evidence_w20261102_late')`,
		`THEN 'no index of its late reports'`,
		`SELECT max(late.accepted_at) FROM public."contract_close_report_evidence_w20261102" late WHERE late.accepted_at >= '2026-11-10 00:00:00'::timestamp`,
		`THEN 'a report accepted within 91 days'`,
		`CROSS JOIN LATERAL ( SELECT 1 FROM public."contract_close_report_evidence_w20261102" evidence WHERE evidence.contract_id = transfer_contract.contract_id LIMIT 1 ) reported`,
		`transfer_contract.outcome IS NULL AND transfer_contract.contract_id < contract_id_floor(now() - interval '91 days' + interval '86400 seconds')`,
		`THEN 'a report of an unresolved contract'`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("precondition lacks %q:\n%s", want, sql)
		}
	}
	// the late index and the precondition share one bound
	if index := closeEvidenceLateIndexSql("contract_close_report_evidence_w20261102", week); !strings.HasSuffix(index, "WHERE accepted_at >= "+closeEvidenceLateBoundSql(week)) {
		t.Errorf("late index %s", index)
	}
	if blocker := closeEvidenceWeekBlocker(context.Background(), nil, PartitionRange{Legacy: true, Upper: week.Upper}); blocker == "" {
		t.Error("the legacy partition passed the precondition")
	}
	if blocker := closeEvidenceWeekBlocker(context.Background(), nil, PartitionRange{Default: true}); blocker == "" {
		t.Error("the DEFAULT partition passed the precondition")
	}
}

// The row deletes take the DEFAULT partition, then the expired weeks a
// straggler keeps, oldest first, then the legacy partition. A week the drop
// will take, one within its retention day, and one not expired are left
// alone.
func TestCloseEvidenceRemovalOrder(t *testing.T) {
	family := closeEvidencePartitionFamily
	weekLeaf := func(day time.Time) PartitionLeaf {
		r := family.RangeAt(day)
		return PartitionLeaf{Table: closeEvidenceTable, Name: family.LeafName(closeEvidenceTable, r), Range: r, Canonical: true}
	}
	oldest := weekLeaf(time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	older := weekLeaf(time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	dropping := weekLeaf(time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC))
	retentionDay := weekLeaf(time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC))
	young := weekLeaf(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	leaves := []PartitionLeaf{
		{Table: closeEvidenceTable, Name: closeEvidenceLegacyLeafName(), Range: PartitionRange{Upper: oldest.Range.Lower, Legacy: true}, Canonical: true},
		young,
		older,
		dropping,
		retentionDay,
		oldest,
		{Table: closeEvidenceTable, Name: closeEvidenceDefaultLeafName(), Range: PartitionRange{Default: true}, Canonical: true},
	}
	blockers := map[string]string{
		oldest.Name:       closeEvidenceBlockerUnresolved,
		older.Name:        closeEvidenceBlockerRecentReport,
		dropping.Name:     "",
		retentionDay.Name: closeEvidenceBlockerRetentionDay,
	}
	want := []string{closeEvidenceDefaultLeafName(), oldest.Name, older.Name, closeEvidenceLegacyLeafName()}
	if targets := closeEvidenceRemovalOrder(leaves, blockers); !slices.Equal(targets, want) {
		t.Fatalf("targets %v, want %v", targets, want)
	}
}

// A pass resumes at its cursor's leaf and contract id; a cursor from before
// leaves were recorded is the plain table's; a leaf no longer a target starts
// the pass over.
func TestCloseEvidenceRemovalCursorResume(t *testing.T) {
	contractId := ContractIdFloor(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	targets := []string{closeEvidenceDefaultLeafName(), "contract_close_report_evidence_w20260601", closeEvidenceLegacyLeafName()}
	for _, test := range []struct {
		cursor  CloseEvidenceRemovalCursor
		targets []string
		start   int
		from    Id
	}{
		{cursor: CloseEvidenceRemovalCursor{}, targets: targets, start: 0},
		{cursor: CloseEvidenceRemovalCursor{Leaf: closeEvidenceLegacyLeafName(), ContractId: &contractId}, targets: targets, start: 2, from: contractId},
		{cursor: CloseEvidenceRemovalCursor{Leaf: "contract_close_report_evidence_w20260601"}, targets: targets, start: 1},
		{cursor: CloseEvidenceRemovalCursor{Leaf: "contract_close_report_evidence_w20260608", ContractId: &contractId}, targets: targets, start: 0},
		// the plain table's cursor, before and after the leaf was recorded
		{cursor: CloseEvidenceRemovalCursor{ContractId: &contractId}, targets: []string{closeEvidenceTable}, start: 0, from: contractId},
		{cursor: CloseEvidenceRemovalCursor{Leaf: closeEvidenceTable, ContractId: &contractId}, targets: []string{closeEvidenceTable}, start: 0, from: contractId},
		// a plain table's cursor once the table is partitioned
		{cursor: CloseEvidenceRemovalCursor{ContractId: &contractId}, targets: targets, start: 0},
	} {
		start, from := test.cursor.resumeIn(test.targets)
		if start != test.start || from != test.from {
			t.Errorf("cursor %+v in %v resumes at %d from %s, want %d from %s", test.cursor, test.targets, start, from, test.start, test.from)
		}
	}
}

// The prepared bound check names its week by the floor of a Monday, on
// report_id; any other check is not a bound.
func TestCloseEvidenceLegacyBoundWeek(t *testing.T) {
	week := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	floor := contractIdLiteral(ContractIdFloor(week))
	for _, definition := range []string{"CHECK ((report_id < " + floor + "))", "CHECK ((report_id < " + floor + ")) NOT VALID"} {
		got, err := closeEvidenceLegacyBoundWeek(&contractConstraint{name: "contract_close_report_evidence_legacy_bound", definition: definition})
		if err != nil || !got.Equal(week) {
			t.Errorf("%s parsed %s %v", definition, got, err)
		}
	}
	for _, definition := range []string{
		"CHECK ((contract_id < " + floor + "))",
		"CHECK ((report_id < " + contractIdLiteral(ContractIdFloor(week.Add(24*time.Hour))) + "))",
		"CHECK ((report_id > " + floor + "))",
	} {
		if _, err := closeEvidenceLegacyBoundWeek(&contractConstraint{name: "contract_close_report_evidence_legacy_bound", definition: definition}); err == nil {
			t.Errorf("%s parsed as a bound", definition)
		}
	}
}

// The evidence table as migrated: two guards, the key and the census index.
func testCloseEvidenceShape() *contractTableShape {
	return &contractTableShape{
		table:   closeEvidenceTable,
		relkind: "r",
		columns: []contractColumn{
			{name: "client_id", typeName: "uuid", notNull: true},
			{name: "report_id", typeName: "uuid", notNull: true},
			{name: "contract_id", typeName: "uuid", notNull: true},
			{name: "accepted_at", typeName: "timestamp without time zone", notNull: true},
		},
		checks: []contractConstraint{
			{name: "contract_close_report_evidence_acked_byte_count_check", definition: "CHECK ((acked_byte_count >= 0))", validated: true},
		},
		indexes: []contractIndex{
			{
				name:                 "contract_close_report_evidence_pkey",
				definition:           "CREATE UNIQUE INDEX contract_close_report_evidence_pkey ON public.contract_close_report_evidence USING btree (client_id, report_id)",
				valid:                true,
				ready:                true,
				primary:              true,
				unique:               true,
				constraint:           "contract_close_report_evidence_pkey",
				constraintDefinition: "PRIMARY KEY (client_id, report_id)",
			},
			{
				name:       "contract_close_original_census",
				definition: "CREATE INDEX contract_close_original_census ON public.contract_close_report_evidence USING btree (contract_id, client_id, report_id)",
				valid:      true,
				ready:      true,
			},
		},
		triggers: []contractTrigger{
			{
				name:       "contract_close_report_evidence_guard",
				definition: "CREATE TRIGGER contract_close_report_evidence_guard BEFORE DELETE OR UPDATE ON public.contract_close_report_evidence FOR EACH ROW EXECUTE FUNCTION contract_close_report_evidence_guard()",
				enabled:    "O",
				row:        true,
			},
			{
				name:       "contract_close_report_evidence_truncate_guard",
				definition: "CREATE TRIGGER contract_close_report_evidence_truncate_guard BEFORE TRUNCATE ON public.contract_close_report_evidence FOR EACH STATEMENT EXECUTE FUNCTION contract_close_report_evidence_guard()",
				enabled:    "O",
				truncate:   true,
			},
		},
	}
}

// The table as migrated can become the legacy partition. Each shape the
// cutover cannot carry is refused with its reason.
func TestCloseEvidenceCutoverProblems(t *testing.T) {
	if problems := closeEvidenceCutoverProblems(testCloseEvidenceShape()); len(problems) != 0 {
		t.Fatalf("the migrated table has problems: %v", problems)
	}
	if problems := closeEvidenceCutoverProblems(nil); len(problems) != 1 {
		t.Fatalf("a missing table: %v", problems)
	}
	for _, test := range []struct {
		name   string
		change func(*contractTableShape)
		want   string
	}{
		{"partitioned", func(shape *contractTableShape) { shape.relkind = "p" }, "is not a plain table"},
		{"nullable key", func(shape *contractTableShape) { shape.columns[1].notNull = false }, "report_id is not a NOT NULL uuid"},
		{"no inherit check", func(shape *contractTableShape) { shape.checks[0].noInherit = true }, "is NO INHERIT"},
		{"invalid index", func(shape *contractTableShape) { shape.indexes[1].valid = false }, "is invalid"},
		{"key without report_id", func(shape *contractTableShape) {
			shape.indexes[0].constraintDefinition = "PRIMARY KEY (client_id)"
		}, "is not (client_id, report_id)"},
		{"another unique index", func(shape *contractTableShape) {
			shape.indexes = append(shape.indexes, contractIndex{
				name:       "synthetic_report_unique",
				definition: "CREATE UNIQUE INDEX synthetic_report_unique ON public.contract_close_report_evidence USING btree (report_id)",
				valid:      true,
				ready:      true,
				unique:     true,
			})
		}, "is not the primary key"},
		{"constraint trigger", func(shape *contractTableShape) { shape.triggers[0].constraint = true }, "is a constraint trigger"},
		{"foreign key", func(shape *contractTableShape) {
			shape.foreignKeys = append(shape.foreignKeys, contractForeignKey{name: "synthetic_contract_fk", table: closeEvidenceTable, referencedTable: "transfer_contract"})
		}, "foreign key synthetic_contract_fk"},
		{"referenced", func(shape *contractTableShape) {
			shape.referencedBy = append(shape.referencedBy, contractForeignKey{name: "synthetic_evidence_fk", table: "synthetic_child", referencedTable: closeEvidenceTable})
		}, "foreign key synthetic_evidence_fk"},
	} {
		shape := testCloseEvidenceShape()
		test.change(shape)
		problems := closeEvidenceCutoverProblems(shape)
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, test.want) }) {
			t.Errorf("%s: problems %v lack %q", test.name, problems, test.want)
		}
	}
}

// The staged parent is partitioned by report_id and carries both guards,
// while the contract tables' staged parents keep their contract_id key.
func TestCloseEvidenceStagedParentSql(t *testing.T) {
	statements, err := closeEvidenceStagedParentSql(testCloseEvidenceShape())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(statements[0], `CREATE TABLE public."contract_close_report_evidence_p" (LIKE public."contract_close_report_evidence" INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES`) ||
		!strings.HasSuffix(statements[0], "PARTITION BY RANGE (report_id)") {
		t.Fatalf("staged parent %s", statements[0])
	}
	if !slices.Contains(statements, `ALTER TABLE public."contract_close_report_evidence_p" ADD CONSTRAINT "contract_close_report_evidence_acked_byte_count_check" CHECK ((acked_byte_count >= 0))`) {
		t.Errorf("staged checks %q", statements)
	}
	for _, trigger := range []string{
		`CREATE TRIGGER contract_close_report_evidence_guard BEFORE DELETE OR UPDATE ON public."contract_close_report_evidence_p" FOR EACH ROW EXECUTE FUNCTION contract_close_report_evidence_guard()`,
		`CREATE TRIGGER contract_close_report_evidence_truncate_guard BEFORE TRUNCATE ON public."contract_close_report_evidence_p" FOR EACH STATEMENT EXECUTE FUNCTION contract_close_report_evidence_guard()`,
	} {
		if !slices.Contains(statements, trigger) {
			t.Errorf("staged parent lacks %s", trigger)
		}
	}
	contract := &contractTableShape{table: "transfer_contract"}
	if statement := contract.stagedParentSql()[0]; !strings.HasSuffix(statement, "PARTITION BY RANGE (contract_id)") {
		t.Errorf("contract staged parent %s", statement)
	}
	// the staged DEFAULT partition keeps its published name and the table's
	// TRUNCATE guard, and attaches last
	defaults, err := closeEvidenceStagedDefaultSql(testCloseEvidenceShape())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`CREATE TABLE public."contract_close_report_evidence_default" (LIKE public."contract_close_report_evidence_p" INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)`,
		`CREATE TRIGGER contract_close_report_evidence_truncate_guard BEFORE TRUNCATE ON public."contract_close_report_evidence_default" FOR EACH STATEMENT EXECUTE FUNCTION contract_close_report_evidence_guard()`,
		`ALTER TABLE public."contract_close_report_evidence_p" ATTACH PARTITION public."contract_close_report_evidence_default" DEFAULT`,
	}
	if !slices.Equal(defaults, want) {
		t.Fatalf("staged DEFAULT %q\nwant %q", defaults, want)
	}
}

// The swap locks both tables first, drops only the row guard (the parent's
// clones on), moves the table and its indexes to the legacy names, attaches
// it below the cutover week, and publishes the staged names.
func TestCloseEvidenceSwapSql(t *testing.T) {
	shape := testCloseEvidenceShape()
	staged := &contractTableShape{
		table: closeEvidenceStagedParentName(),
		indexes: []contractIndex{
			{name: "contract_close_report_evidence_pkey_p"},
			{name: "contract_close_original_census_p"},
		},
	}
	floor := ContractIdFloor(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	want := []string{
		`LOCK TABLE public."contract_close_report_evidence", public."contract_close_report_evidence_p" IN ACCESS EXCLUSIVE MODE`,
		`DROP TRIGGER "contract_close_report_evidence_guard" ON public."contract_close_report_evidence"`,
		`ALTER TABLE public."contract_close_report_evidence" RENAME TO "contract_close_report_evidence_legacy"`,
		`ALTER INDEX public."contract_close_report_evidence_pkey" RENAME TO "contract_close_report_evidence_pkey_l"`,
		`ALTER INDEX public."contract_close_original_census" RENAME TO "contract_close_original_census_l"`,
		`ALTER TABLE public."contract_close_report_evidence_p" ATTACH PARTITION public."contract_close_report_evidence_legacy" FOR VALUES FROM (MINVALUE) TO (` + contractIdLiteral(floor) + `)`,
		`ALTER TABLE public."contract_close_report_evidence_p" RENAME TO "contract_close_report_evidence"`,
		`ALTER INDEX public."contract_close_report_evidence_pkey_p" RENAME TO "contract_close_report_evidence_pkey"`,
		`ALTER INDEX public."contract_close_original_census_p" RENAME TO "contract_close_original_census"`,
	}
	if statements := closeEvidenceSwapSql(shape, staged, floor); !slices.Equal(statements, want) {
		t.Fatalf("swap %q\nwant %q", statements, want)
	}
}

// The command takes one action at a time, and prepare needs a week far
// enough ahead; both are refused before the database is read.
func TestCloseEvidenceCutoverRefusesBadOptions(t *testing.T) {
	ctx := context.Background()
	if err := MigrateCloseEvidenceToPartitions(ctx, CloseEvidencePartitionMigrateOptions{Finalize: true, Abort: true}); err == nil {
		t.Error("two actions were accepted")
	}
	if err := MigrateCloseEvidenceToPartitions(ctx, CloseEvidencePartitionMigrateOptions{}); err == nil {
		t.Error("prepare without a week was accepted")
	}
	soon := PartitionGranularityWeekly.RangeStart(NowUtc())
	if err := MigrateCloseEvidenceToPartitions(ctx, CloseEvidencePartitionMigrateOptions{CutoverWeek: soon}); err == nil {
		t.Error("prepare for the current week was accepted")
	}
}

// The move guard follows the retention guard (857), keeps its rule and adds
// the two moves; the version found by its SQL is its own.
func TestContractCloseEvidenceMoveGuardMigration(t *testing.T) {
	version := ContractCloseEvidenceMoveGuardMigrationVersion()
	if version == 0 || version != sqlMigrationIndex(t, "moved_row = $3")+1 {
		t.Fatalf("move guard version %d", version)
	}
	if retention := sqlMigrationVersion(contractCloseReportEvidenceRetentionGuardSql); retention == 0 || version <= retention {
		t.Fatalf("the move guard (%d) does not follow the retention guard (%d)", version, retention)
	}
	if sqlMigrationVersion("synthetic migration that is not in the list") != 0 {
		t.Fatal("a missing migration has a version")
	}
	if !strings.HasPrefix(strings.TrimSpace(contractCloseReportEvidenceMoveGuardSql), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the move guard does not bound its lock wait")
	}
	body := ContractCloseEvidenceMoveGuardBodySql()
	for _, want := range []string{
		// retention as 857
		"current_setting('urnetwork.close_evidence_retention', true) = 'on'",
		"OLD.accepted_at < (now() AT TIME ZONE 'UTC') - interval '91 days'",
		// moves out of the plain table and out of DEFAULT
		"current_setting('urnetwork.close_evidence_retention', true) = 'move'",
		"TG_TABLE_NAME = 'contract_close_report_evidence' THEN\n   destination := 'contract_close_report_evidence_p';",
		"TG_TABLE_NAME = 'contract_close_report_evidence_default' THEN\n   destination := 'contract_close_report_evidence_w' || to_char(date_trunc('week',",
		"moved_row = $3",
		"RAISE EXCEPTION 'original contract close reports are immutable';",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("move guard lacks %q", want)
		}
	}
	// the published 857 body is unchanged
	if retention := ContractCloseEvidenceRetentionGuardBodySql(); strings.Contains(retention, "move") {
		t.Error("the retention guard body changed")
	}
	// the weekly leaf names the guard derives are the registry's
	for _, at := range []time.Time{
		time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 8, 23, 59, 59, 999000000, time.UTC),
	} {
		if name := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, closeEvidencePartitionFamily.RangeAt(at)); name != "contract_close_report_evidence_w20261102" {
			t.Errorf("%s is in %s", at, name)
		}
	}
}

// While a cutover is prepared, the staged parent with its indexes and the
// bound check on the table are not drift; anything else still is.
func TestSchemaDiffAcceptsCloseEvidenceCutover(t *testing.T) {
	census := "CREATE INDEX contract_close_original_census ON public.contract_close_report_evidence USING btree (contract_id, client_id, report_id)"
	expected := &SchemaSnapshot{
		tables:      map[string]bool{"contract_close_report_evidence": true},
		columns:     map[string][]auditColumn{},
		indexes:     map[string]auditIndex{"contract_close_original_census": {table: "contract_close_report_evidence", def: census}},
		constraints: map[string][]auditConstraint{},
		partitioned: map[string]bool{},
	}
	prepared := &SchemaSnapshot{
		tables:  map[string]bool{"contract_close_report_evidence": true, "contract_close_report_evidence_p": true},
		columns: map[string][]auditColumn{},
		indexes: map[string]auditIndex{
			"contract_close_original_census":   {table: "contract_close_report_evidence", def: census},
			"contract_close_original_census_p": {table: "contract_close_report_evidence_p", def: "CREATE INDEX contract_close_original_census_p ON ONLY public.contract_close_report_evidence_p USING btree (contract_id, client_id, report_id)"},
		},
		constraints: map[string][]auditConstraint{
			"contract_close_report_evidence": {{name: "contract_close_report_evidence_legacy_bound", typ: "c", def: "CHECK ((report_id < '0192f0f0-0000-0000-0000-000000000000'::uuid)) NOT VALID"}},
		},
		partitioned: map[string]bool{"contract_close_report_evidence_p": true},
	}
	if diff := diffSchemas(expected, prepared); diff.HasDifferences() {
		t.Fatalf("a prepared cutover reads as drift:%s", diff.Report())
	}
	// after the swap the parent's index reads ON ONLY and still matches
	swapped := &SchemaSnapshot{
		tables:      map[string]bool{"contract_close_report_evidence": true},
		columns:     map[string][]auditColumn{},
		indexes:     map[string]auditIndex{"contract_close_original_census": {table: "contract_close_report_evidence", def: "CREATE INDEX contract_close_original_census ON ONLY public.contract_close_report_evidence USING btree (contract_id, client_id, report_id)"}},
		constraints: map[string][]auditConstraint{},
		partitioned: map[string]bool{"contract_close_report_evidence": true},
	}
	if diff := diffSchemas(expected, swapped); diff.HasDifferences() {
		t.Fatalf("the swapped table reads as drift:%s", diff.Report())
	}
	// a plain look-alike of the staged parent, or another check, is drift
	lookalike := &SchemaSnapshot{
		tables:  map[string]bool{"contract_close_report_evidence": true, "contract_close_report_evidence_p": true},
		columns: map[string][]auditColumn{},
		indexes: map[string]auditIndex{"contract_close_original_census": {table: "contract_close_report_evidence", def: census}},
		constraints: map[string][]auditConstraint{
			"contract_close_report_evidence": {{name: "synthetic_check", typ: "c", def: "CHECK (true)"}},
		},
		partitioned: map[string]bool{},
	}
	diff := diffSchemas(expected, lookalike)
	if !slices.Equal(diff.extraTables, []string{"contract_close_report_evidence_p"}) || !slices.Equal(diff.extraConstraints["contract_close_report_evidence"], []string{"synthetic_check"}) {
		t.Fatalf("drift was accepted: tables %v constraints %v", diff.extraTables, diff.extraConstraints)
	}
}
