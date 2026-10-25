// The finished_task cutover's pure rules: the days it stages, the shape it
// accepts, the bound it parses, the swap and rollback statements, the
// options it refuses before reading the database, and the schema audit's
// acceptance of a prepared cutover. No database is read.
package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// A cutover day ahead stages it and the days ahead of it; a day already past
// (a test's) stages through the days ahead of today, so today has a partition.
func TestFinishedTaskStagedDays(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC)
	starts := func(days []PartitionRange) (lowers []string) {
		for _, day := range days {
			if !day.Upper.Equal(day.Lower.AddDate(0, 0, 1)) {
				t.Fatalf("day %s is not one day", day.Lower)
			}
			lowers = append(lowers, day.Lower.Format(time.DateOnly))
		}
		return
	}
	ahead := finishedTaskStagedDays(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), now, 3)
	if got := starts(ahead); !slices.Equal(got, []string{"2026-10-26", "2026-10-27", "2026-10-28", "2026-10-29"}) {
		t.Errorf("a cutover day ahead stages %v", got)
	}
	past := finishedTaskStagedDays(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC), now, 2)
	if got := starts(past); !slices.Equal(got, []string{"2026-10-21", "2026-10-22", "2026-10-23", "2026-10-24", "2026-10-25", "2026-10-26"}) {
		t.Errorf("a cutover day past stages %v", got)
	}
	if day := TestingFinishedTaskCutoverDay(now); !day.Equal(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the test cutover day is %s", day)
	}
	// the staged days carry the registry's names, so the runway adopts them
	if name := finishedTaskDays.LeafName(finishedTaskPartitionTable, ahead[0]); name != "finished_task_p20261026" {
		t.Errorf("staged day %s", name)
	}
	if statements := finishedTaskStagedDaySql(ahead[0]); len(statements) != 2 ||
		!strings.Contains(statements[0], `CREATE TABLE public."finished_task_p20261026" (LIKE public."finished_task_staged"`) ||
		statements[1] != `ALTER TABLE public."finished_task_staged" ATTACH PARTITION public."finished_task_p20261026" FOR VALUES FROM ('2026-10-26 00:00:00') TO ('2026-10-27 00:00:00')` {
		t.Errorf("staged day statements %v", statements)
	}
}

// The table as the migrations leave it, with its three indexes.
func testFinishedTaskShape() *contractTableShape {
	return &contractTableShape{
		table:   "finished_task",
		relkind: "r",
		columns: []contractColumn{
			{name: "task_id", typeName: "uuid", notNull: true, statisticsTarget: -1},
			{name: "run_end_time", typeName: "timestamp without time zone", notNull: true, statisticsTarget: -1},
			{name: "post_error", typeName: "text", statisticsTarget: -1},
			{name: "post_completed", typeName: "boolean", notNull: true, expression: "true", statisticsTarget: -1},
		},
		indexes: []contractIndex{
			{
				name:                 "finished_task_pkey",
				definition:           "CREATE UNIQUE INDEX finished_task_pkey ON public.finished_task USING btree (task_id, run_end_time)",
				valid:                true,
				ready:                true,
				primary:              true,
				unique:               true,
				constraint:           "finished_task_pkey",
				constraintDefinition: finishedTaskPrimaryKeyDefinition,
			},
			{
				name:       "finished_task_run_end_time",
				definition: "CREATE INDEX finished_task_run_end_time ON public.finished_task USING btree (run_end_time, task_id)",
				valid:      true,
				ready:      true,
			},
			{
				name:       FinishedTaskPostRetryIndexName,
				definition: "CREATE INDEX finished_task_post_retry ON public.finished_task USING btree (task_id) WHERE ((post_error IS NOT NULL) AND (NOT post_completed))",
				valid:      true,
				ready:      true,
			},
		},
	}
}

// The migrated table passes; the shapes the cutover cannot carry, or that the
// migrations have not reached yet, are refused with their reason.
func TestFinishedTaskCutoverProblems(t *testing.T) {
	if problems := finishedTaskCutoverProblems(testFinishedTaskShape()); len(problems) != 0 {
		t.Fatalf("the migrated table has problems: %v", problems)
	}
	for _, test := range []struct {
		name   string
		modify func(shape *contractTableShape)
		want   string
	}{
		{"already partitioned", func(shape *contractTableShape) { shape.relkind = "p" }, "not a plain table"},
		{"the task id key", func(shape *contractTableShape) {
			shape.indexes[0].constraintDefinition = "PRIMARY KEY (task_id)"
		}, "apply the database migrations first"},
		{"no failed Post index", func(shape *contractTableShape) { shape.indexes = shape.indexes[:2] }, FinishedTaskPostRetryIndexName},
		{"an invalid index", func(shape *contractTableShape) { shape.indexes[1].valid = false }, "is invalid"},
		{"a unique index", func(shape *contractTableShape) {
			shape.indexes = append(shape.indexes, contractIndex{name: "finished_task_task_id", definition: "CREATE UNIQUE INDEX finished_task_task_id ON public.finished_task USING btree (task_id)", valid: true, ready: true, unique: true})
		}, "is not the primary key"},
		{"a nullable key", func(shape *contractTableShape) { shape.columns[1].notNull = false }, "run_end_time"},
		{"a trigger", func(shape *contractTableShape) {
			shape.triggers = []contractTrigger{{name: "synthetic_finished_task_trigger"}}
		}, "synthetic_finished_task_trigger"},
		{"a foreign key", func(shape *contractTableShape) {
			shape.referencedBy = []contractForeignKey{{name: "synthetic_finished_task_fkey"}}
		}, "synthetic_finished_task_fkey"},
		{"a NO INHERIT check", func(shape *contractTableShape) {
			shape.checks = []contractConstraint{{name: "synthetic_check", noInherit: true}}
		}, "NO INHERIT"},
	} {
		shape := testFinishedTaskShape()
		test.modify(shape)
		problems := finishedTaskCutoverProblems(shape)
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, test.want) }) {
			t.Errorf("%s: problems %v lack %q", test.name, problems, test.want)
		}
	}
}

// The bound check as pg_get_constraintdef prints it, validated or not; any
// other expression is not the cutover's.
func TestFinishedTaskLegacyBoundPattern(t *testing.T) {
	for _, definition := range []string{
		"CHECK ((run_end_time < '2026-10-26 00:00:00'::timestamp without time zone))",
		"CHECK ((run_end_time < '2026-10-26 00:00:00'::timestamp without time zone)) NOT VALID",
		"CHECK ((run_end_time < '10/26/2026 00:00:00'::timestamp without time zone))",
	} {
		if !finishedTaskLegacyBoundPattern.MatchString(definition) {
			t.Errorf("%s did not match", definition)
		}
	}
	for _, definition := range []string{
		"CHECK ((run_end_time <= '2026-10-26 00:00:00'::timestamp without time zone))",
		"CHECK ((run_start_time < '2026-10-26 00:00:00'::timestamp without time zone))",
		"CHECK (((run_end_time < '2026-10-26 00:00:00'::timestamp without time zone) OR true))",
	} {
		if finishedTaskLegacyBoundPattern.MatchString(definition) {
			t.Errorf("%s matched", definition)
		}
	}
	if finishedTaskLegacyBoundName != "finished_task_legacy_bound" {
		t.Errorf("bound name %s", finishedTaskLegacyBoundName)
	}
}

// The swap moves the table and its indexes to the legacy names, attaches it
// below the cutover day, and gives the staged parent and its indexes the
// published names, in that order; the parent takes the swap's locks first.
func TestFinishedTaskSwapSql(t *testing.T) {
	shape := testFinishedTaskShape()
	staged := &contractTableShape{table: finishedTaskStagedParentName, relkind: "p"}
	for _, index := range shape.indexes {
		staged.indexes = append(staged.indexes, contractIndex{name: index.name + contractStagedIndexSuffix})
	}
	cutoverDay := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	want := []string{
		`ALTER TABLE public.finished_task RENAME TO "finished_task_legacy"`,
		`ALTER INDEX public."finished_task_pkey" RENAME TO "finished_task_pkey_l"`,
		`ALTER INDEX public."finished_task_run_end_time" RENAME TO "finished_task_run_end_time_l"`,
		`ALTER INDEX public."finished_task_post_retry" RENAME TO "finished_task_post_retry_l"`,
		`ALTER TABLE public."finished_task_staged" ATTACH PARTITION public."finished_task_legacy" FOR VALUES FROM (MINVALUE) TO ('2026-10-26 00:00:00')`,
		`ALTER TABLE public."finished_task_staged" RENAME TO finished_task`,
		`ALTER INDEX public."finished_task_pkey_p" RENAME TO "finished_task_pkey"`,
		`ALTER INDEX public."finished_task_run_end_time_p" RENAME TO "finished_task_run_end_time"`,
		`ALTER INDEX public."finished_task_post_retry_p" RENAME TO "finished_task_post_retry"`,
	}
	if got := finishedTaskSwapSql(shape, staged, cutoverDay); !slices.Equal(got, want) {
		t.Errorf("swap statements\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	settings := DefaultFinishedTaskPartitionSettings()
	locks := finishedTaskSwapLockSql(settings)
	if len(locks) != 3 || locks[0] != "SET LOCAL lock_timeout = '2000ms'" || locks[1] != "SET LOCAL statement_timeout = '30000ms'" ||
		locks[2] != `LOCK TABLE public.finished_task, public."finished_task_staged" IN ACCESS EXCLUSIVE MODE` {
		t.Errorf("swap locks %v", locks)
	}
	stagedSql := finishedTaskStagedParentSql(shape)
	if len(stagedSql) != 1 || !strings.HasPrefix(stagedSql[0], `CREATE TABLE public."finished_task_staged" (LIKE public."finished_task" INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES`) ||
		!strings.HasSuffix(stagedSql[0], "PARTITION BY RANGE (run_end_time)") {
		t.Errorf("staged parent statements %v", stagedSql)
	}
}

// The rollback copies every column but a generated one from the daily
// partitions, which the parent alone holds once the legacy table is detached.
func TestFinishedTaskRollbackCopySql(t *testing.T) {
	if got := finishedTaskRollbackCopySql([]string{"task_id", "run_end_time"}); got != `INSERT INTO public."finished_task_legacy" (task_id, run_end_time) SELECT task_id, run_end_time FROM public.finished_task` {
		t.Errorf("copy %s", got)
	}
	locks := finishedTaskRollbackLockSql(DefaultFinishedTaskPartitionSettings())
	if len(locks) != 3 || locks[1] != "SET LOCAL statement_timeout = '120000ms'" || locks[2] != "LOCK TABLE public.finished_task IN ACCESS EXCLUSIVE MODE" {
		t.Errorf("rollback locks %v", locks)
	}
}

// The command refuses two steps at once, a prepare without its day, and a
// cutover day already inside the margin where a prepare is abandoned, before
// any database read. A new prepare's lead is checked against the database's
// prepared bound, so that resuming one does not need it.
func TestFinishedTaskMigrateOptionsRefused(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		options FinishedTaskPartitionMigrateOptions
		want    string
	}{
		{"two steps", FinishedTaskPartitionMigrateOptions{Finalize: true, Rollback: true}, "choose one"},
		{"three steps", FinishedTaskPartitionMigrateOptions{Finalize: true, Rollback: true, Abort: true}, "choose one"},
		{"no day", FinishedTaskPartitionMigrateOptions{}, "cutover day is required"},
		{"a day already past", FinishedTaskPartitionMigrateOptions{CutoverDay: PartitionGranularityDaily.RangeStart(NowUtc())}, "prepare a later day"},
	} {
		err := MigrateFinishedTasksToPartitions(ctx, test.options)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v", test.name, err)
		}
	}
	defaults := DefaultFinishedTaskPartitionSettings()
	if defaults.CutoverAbandonMargin <= 0 || defaults.MinCutoverLead <= defaults.CutoverAbandonMargin {
		t.Errorf("a cutover day at the minimum lead would be abandoned before it could be finalized: %+v", defaults)
	}
}

// A prepared cutover's staged parent, its indexes and the bound check are not
// drift; another extra constraint, table or index still is.
func TestSchemaAuditAcceptsPreparedFinishedTaskCutover(t *testing.T) {
	actual := &SchemaSnapshot{
		tables:      map[string]bool{"finished_task": true, finishedTaskStagedParentName: true, "synthetic_extra": true},
		indexes:     map[string]auditIndex{"finished_task_pkey_p": {table: finishedTaskStagedParentName}, "synthetic_extra_index": {table: "finished_task"}},
		partitioned: map[string]bool{finishedTaskStagedParentName: true},
	}
	expected := &SchemaSnapshot{tables: map[string]bool{"finished_task": true}}
	d := &SchemaDiff{
		extraTables:      []string{finishedTaskStagedParentName, "synthetic_extra"},
		extraIdx:         []string{"finished_task_pkey_p", "synthetic_extra_index"},
		extraConstraints: map[string][]string{"finished_task": {finishedTaskLegacyBoundName, "synthetic_extra_check"}},
		expected:         expected,
		actual:           actual,
	}
	d.acceptFinishedTaskPartitionCutover()
	if !slices.Equal(d.extraTables, []string{"synthetic_extra"}) || !slices.Equal(d.extraIdx, []string{"synthetic_extra_index"}) ||
		!slices.Equal(d.extraConstraints["finished_task"], []string{"synthetic_extra_check"}) {
		t.Fatalf("accepted %v %v %v", d.extraTables, d.extraIdx, d.extraConstraints)
	}
	d.extraConstraints = map[string][]string{"finished_task": {finishedTaskLegacyBoundName}}
	d.acceptFinishedTaskPartitionCutover()
	if _, ok := d.extraConstraints["finished_task"]; ok {
		t.Fatal("the bound alone left an empty entry")
	}
	// a plain table of the staged name is drift
	actual.partitioned = map[string]bool{}
	d.extraTables = []string{finishedTaskStagedParentName}
	d.acceptFinishedTaskPartitionCutover()
	if !slices.Equal(d.extraTables, []string{finishedTaskStagedParentName}) {
		t.Fatal("a plain table under the staged name was accepted")
	}
}
