// The table swap's pure rules (db_table_swap.go): which relations are a
// swap's own, the identifier bounds of the names it gives, the index
// definitions it rebuilds, its settings and its profile registration, db
// audit's acceptance of a kept old table and the REINDEX rotation leaving it
// alone.
package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The synthetic queue the swap tests rotate (db_table_swap_db_test.go). Its
// preflight returns the refusal a test sets, and counts its calls.
const testTableSwapTable = "test_table_swap_queue"

type testTableSwapPreflightState struct {
	stateLock sync.Mutex
	// the refusal of the nth call (from 1), or of every call when keyed 0
	refusals map[int]error
	calls    int
	// the message the locked check raises, or none when empty
	lockedRefusal string
}

var testTableSwapPreflight = &testTableSwapPreflightState{refusals: map[int]error{}}

func (self *testTableSwapPreflightState) reset(refusals map[int]error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusals = refusals
	self.calls = 0
	self.lockedRefusal = ""
}

func (self *testTableSwapPreflightState) refuseLocked(message string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lockedRefusal = message
}

func (self *testTableSwapPreflightState) lockedCheckSql() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.lockedRefusal == "" {
		return "SELECT 1"
	}
	return tableSwapAssertSql("false", self.lockedRefusal)
}

func (self *testTableSwapPreflightState) callCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.calls
}

func (self *testTableSwapPreflightState) preflight(ctx context.Context, conn PgQuerier) ([]string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls += 1
	if err, ok := self.refusals[self.calls]; ok {
		return []string{"synthetic preflight refused"}, err
	}
	return []string{"synthetic preflight admitted"}, self.refusals[0]
}

var _ = RegisterTableSwapProfile(&TableSwapProfile{
	Table:          testTableSwapTable,
	Stop:           "nothing: a synthetic queue",
	Preflight:      testTableSwapPreflight.preflight,
	LockedCheckSql: testTableSwapPreflight.lockedCheckSql,
})

// A swap's twin and kept old tables of a registered table are its own; look
// alikes and other tables are not.
func TestTableSwapRelationRecognizesOnlyASwapsOwnTables(t *testing.T) {
	for _, test := range []struct {
		name  string
		owned bool
	}{
		{name: testTableSwapTable + "_old_20261118120000", owned: true},
		{name: testTableSwapTable + "_next", owned: true},
		{name: testTableSwapTable, owned: false},
		{name: testTableSwapTable + "_old_2026111812000", owned: false},
		{name: testTableSwapTable + "_old_202611181200000", owned: false},
		{name: testTableSwapTable + "_old_2026111812000x", owned: false},
		{name: testTableSwapTable + "_nexts", owned: false},
		{name: "synthetic_other_old_20261118120000", owned: false},
	} {
		table, owned := TableSwapRelation(test.name)
		if owned != test.owned || (owned && table != testTableSwapTable) {
			t.Errorf("%s: owned %t by %q", test.name, owned, table)
		}
	}
}

// Every name a swap gives must fit PostgreSQL's 63 bytes: a longer one would
// be truncated, and two truncated names could collide.
func TestTableSwapNameRefusalsBoundIdentifierLength(t *testing.T) {
	fits := &tableSwapShape{indexes: []tableSwapIndex{{name: strings.Repeat("i", pgMaxIdentifierLength-len("_old_20261118120000"))}}}
	if refusals := tableSwapNameRefusals("synthetic_queue", fits); len(refusals) != 0 {
		t.Fatalf("names within the bound refused: %v", refusals)
	}
	long := &tableSwapShape{indexes: []tableSwapIndex{{name: strings.Repeat("i", pgMaxIdentifierLength-len("_old_20261118120000")+1)}}}
	if refusals := tableSwapNameRefusals("synthetic_queue", long); len(refusals) != 1 || !strings.Contains(refusals[0], "longer than 63 bytes") {
		t.Fatalf("an old index name past the bound: %v", refusals)
	}
}

// The rebuild reuses everything after USING, which names neither the index
// nor its table.
func TestTableSwapIndexDefinitionParses(t *testing.T) {
	for _, test := range []struct {
		definition string
		unique     bool
		body       string
	}{
		{
			definition: "CREATE UNIQUE INDEX pending_task_pkey ON public.pending_task USING btree (task_id)",
			unique:     true,
			body:       "btree (task_id)",
		},
		{
			definition: "CREATE INDEX pending_task_function_poll_order ON public.pending_task USING btree (regexp_replace((function_name)::text, '/v[0-9]+'::text, ''::text, 'g'::text), available_block, run_priority DESC, run_max_time_seconds DESC, task_id)",
			body:       "btree (regexp_replace((function_name)::text, '/v[0-9]+'::text, ''::text, 'g'::text), available_block, run_priority DESC, run_max_time_seconds DESC, task_id)",
		},
		{
			definition: "CREATE INDEX synthetic_partial ON public.synthetic USING btree (a) INCLUDE (b) WITH (fillfactor='70') WHERE (c IS NULL)",
			body:       "btree (a) INCLUDE (b) WITH (fillfactor='70') WHERE (c IS NULL)",
		},
	} {
		match := tableSwapIndexDefinitionPattern.FindStringSubmatch(test.definition)
		if match == nil || (match[1] != "") != test.unique || match[5] != test.body {
			t.Errorf("%s: %q", test.definition, match)
		}
	}
}

func TestTableSwapSettingsValidate(t *testing.T) {
	if err := DefaultTableSwapSettings().validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*TableSwapSettings){
		func(settings *TableSwapSettings) { settings.LockTimeout = 0 },
		func(settings *TableSwapSettings) { settings.StatementTimeout = time.Microsecond },
		func(settings *TableSwapSettings) { settings.LockAttempts = 0 },
		func(settings *TableSwapSettings) { settings.MaxCopyRows = 0 },
		func(settings *TableSwapSettings) { settings.WorkMemBytes = 1024 },
		func(settings *TableSwapSettings) { settings.DropOldMinAge = 0 },
	} {
		settings := DefaultTableSwapSettings()
		change(settings)
		if err := settings.validate(); err == nil {
			t.Errorf("accepted %+v", settings)
		}
	}
	// the lock wait stays below a main-pool writer's own bound
	if PgLockTimeout <= DefaultTableSwapSettings().LockTimeout {
		t.Fatalf("the swap's lock wait %s reaches a writer's %s", DefaultTableSwapSettings().LockTimeout, PgLockTimeout)
	}
}

func TestRegisterTableSwapProfileRefusesInvalidTables(t *testing.T) {
	for _, profile := range []*TableSwapProfile{
		{Table: "Synthetic"},
		{Table: "synthetic-queue"},
		{Table: strings.Repeat("q", pgMaxIdentifierLength-len("_old_20261118120000")+1)},
		{Table: testTableSwapTable},
		{Table: "synthetic_bad_settings", Settings: &TableSwapSettings{}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registered %+v", profile)
				}
			}()
			RegisterTableSwapProfile(profile)
		}()
	}
	if err := SwapTable(context.Background(), TableSwapOptions{Table: "synthetic_unregistered"}); err == nil || !strings.Contains(err.Error(), "no table swap profile") {
		t.Fatal("an unregistered table was swapped", err)
	}
	if err := SwapTable(context.Background(), TableSwapOptions{Table: testTableSwapTable, Rollback: true, DropOld: true}); err == nil || !strings.Contains(err.Error(), "choose one") {
		t.Fatal("rollback and drop old together", err)
	}
}

// A kept old table and its indexes are not extra objects; another extra table
// and an index elsewhere still are.
func TestSchemaDiffAcceptsTableSwapTables(t *testing.T) {
	oldTable := testTableSwapTable + "_old_20261118120000"
	expected := &SchemaSnapshot{
		tables:      map[string]bool{testTableSwapTable: true},
		columns:     map[string][]auditColumn{},
		indexes:     map[string]auditIndex{},
		constraints: map[string][]auditConstraint{},
		partitioned: map[string]bool{},
	}
	actual := &SchemaSnapshot{
		tables:  map[string]bool{testTableSwapTable: true, oldTable: true, "synthetic_extra": true},
		columns: map[string][]auditColumn{},
		indexes: map[string]auditIndex{
			"test_table_swap_queue_poll_old_20261118120000": {table: oldTable, def: "CREATE INDEX test_table_swap_queue_poll_old_20261118120000 ON public." + oldTable + " USING btree (run_at_block)"},
			"test_table_swap_queue_stray":                   {table: testTableSwapTable, def: "CREATE INDEX test_table_swap_queue_stray ON public.test_table_swap_queue USING btree (payload)"},
		},
		constraints: map[string][]auditConstraint{},
		partitioned: map[string]bool{},
	}
	diff := diffSchemas(expected, actual)
	if !slices.Equal(diff.extraTables, []string{"synthetic_extra"}) {
		t.Fatalf("extra tables %v", diff.extraTables)
	}
	if !slices.Equal(diff.extraIdx, []string{"test_table_swap_queue_stray"}) {
		t.Fatalf("extra indexes %v", diff.extraIdx)
	}
}

// The REINDEX rotation leaves a swap's tables alone and keeps the table.
func TestDbMaintenanceSkipsTableSwapRelations(t *testing.T) {
	for _, name := range []string{testTableSwapTable + "_old_20261118120000", testTableSwapTable + "_next"} {
		if dbMaintenanceShouldReindexTable(name) || dbMaintenanceShouldReindexRelation(name, false) {
			t.Errorf("the rotation rebuilds %s", name)
		}
	}
	if !dbMaintenanceShouldReindexTable(testTableSwapTable) {
		t.Fatal("the rotation skips the table itself")
	}
}

// A column of a synthetic shape with the given attoptions, in the catalog's
// order.
func testTableSwapColumn(name string, options ...string) tableSwapColumn {
	sorted := slices.Clone(options)
	slices.Sort(sorted)
	return tableSwapColumn{
		name:           name,
		typeName:       "bigint",
		notNull:        true,
		storage:        "p",
		statistics:     -1,
		options:        sorted,
		orderedOptions: slices.Clone(options),
	}
}

func testTableSwapShapeOf(name string, columns ...tableSwapColumn) *tableSwapShape {
	return &tableSwapShape{
		name:            name,
		kind:            "r",
		persistence:     "p",
		replicaIdentity: "d",
		accessMethod:    "heap",
		owner:           "bringyour",
		columns:         columns,
		primaryKey:      []string{columns[0].name},
	}
}

// PostgreSQL 18.7 stamps every new column with an internal option,
// pg_attver, which no statement can set: the twin's columns get versions the
// table's never carry. A table from before 18.7 has none; one created since
// has older ones. On Main on 2026-11-17 the rehearsal of pending_task read
// pg_attver=2138604065658440 on every column of the twin and nothing on the
// table, and refused. The comparison holds only the options a user sets, so
// a missing or different user option still refuses.
func TestTableSwapDifferencesIgnorePostgresInternalColumnOptions(t *testing.T) {
	for _, test := range []struct {
		name        string
		table       *tableSwapShape
		twin        *tableSwapShape
		differences int
	}{
		{
			name:  "a table from before 18.7",
			table: testTableSwapShapeOf("pending_task", testTableSwapColumn("task_id"), testTableSwapColumn("run_at")),
			twin: testTableSwapShapeOf(
				"pending_task_next",
				testTableSwapColumn("task_id", "pg_attver=2138604065658440"),
				testTableSwapColumn("run_at", "pg_attver=2138604065658440"),
			),
		},
		{
			name: "a table created since, with a user option",
			table: testTableSwapShapeOf(
				"synthetic_queue",
				testTableSwapColumn("item_id", "pg_attver=2137466531237784"),
				testTableSwapColumn("queue_key", "pg_attver=2137466531401296", "n_distinct=-0.5"),
			),
			twin: testTableSwapShapeOf(
				"synthetic_queue_next",
				testTableSwapColumn("item_id", "pg_attver=2138604065658440"),
				testTableSwapColumn("queue_key", "n_distinct=-0.5", "pg_attver=2138604065658440"),
			),
		},
		{
			name: "a twin without the user option",
			table: testTableSwapShapeOf(
				"synthetic_queue",
				testTableSwapColumn("item_id"),
				testTableSwapColumn("queue_key", "n_distinct=-0.5"),
			),
			twin: testTableSwapShapeOf(
				"synthetic_queue_next",
				testTableSwapColumn("item_id", "pg_attver=2138604065658440"),
				testTableSwapColumn("queue_key", "pg_attver=2138604065658440"),
			),
			differences: 1,
		},
		{
			name: "a twin with another user option value",
			table: testTableSwapShapeOf(
				"synthetic_queue",
				testTableSwapColumn("item_id", "pg_attver=2137466531237784"),
				testTableSwapColumn("queue_key", "pg_attver=2137466531237784", "n_distinct=-0.5"),
			),
			twin: testTableSwapShapeOf(
				"synthetic_queue_next",
				testTableSwapColumn("item_id", "pg_attver=2138604065658440"),
				testTableSwapColumn("queue_key", "pg_attver=2138604065658440", "n_distinct=100"),
			),
			differences: 1,
		},
	} {
		if differences := test.table.differences(test.twin, tableSwapNextName); len(differences) != test.differences {
			t.Errorf("%s: %d difference(s), not %d:\n%s", test.name, len(differences), test.differences, strings.Join(differences, "\n"))
		}
	}
}

// The twin gets the options a user set on the table's columns, and never
// PostgreSQL's own: the server gives a new column its pg_attver and drops a
// value an ALTER COLUMN SET names.
func TestTableSwapTwinSqlSetsOnlyUserColumnOptions(t *testing.T) {
	table := testTableSwapShapeOf(
		"synthetic_queue",
		testTableSwapColumn("item_id", "pg_attver=2137466531237784"),
		testTableSwapColumn("queue_key", "pg_attver=2137466531237784", "n_distinct=-0.5"),
		testTableSwapColumn("payload"),
	)
	set := []string{}
	for _, statement := range table.twinSql("synthetic_queue_next") {
		if strings.Contains(statement, "pg_attver") {
			t.Errorf("a statement sets PostgreSQL's internal option: %s", statement)
		}
		if strings.Contains(statement, " SET (") {
			set = append(set, statement)
		}
	}
	expected := []string{"ALTER TABLE public." + quoteIdentifier("synthetic_queue_next") + " ALTER COLUMN " + quoteIdentifier("queue_key") + " SET (n_distinct=-0.5)"}
	if !slices.Equal(set, expected) {
		t.Fatalf("column option statements %q, not %q", set, expected)
	}
}

// A difference says which relation holds which value: on 2026-11-17 the
// twin's options were read as the table's.
func TestTableSwapDifferencesNameEachSide(t *testing.T) {
	table := testTableSwapShapeOf("synthetic_queue", testTableSwapColumn("item_id"), testTableSwapColumn("queue_key", "n_distinct=-0.5"))
	twin := testTableSwapShapeOf("synthetic_queue_next", testTableSwapColumn("item_id"), testTableSwapColumn("queue_key"))
	differences := table.differences(twin, tableSwapNextName)
	if len(differences) != 1 {
		t.Fatalf("differences %q", differences)
	}
	// what the difference says relation has, through the end of the column
	side := func(relation string) string {
		at := strings.Index(differences[0], relation+" has {")
		if at < 0 {
			return ""
		}
		rest := differences[0][at:]
		return rest[:strings.Index(rest, "}")+1]
	}
	if twinSide, tableSide := side("synthetic_queue_next"), side("synthetic_queue"); !strings.Contains(tableSide, "options:[n_distinct=-0.5]") || !strings.Contains(twinSide, "options:[]") {
		t.Fatalf("the difference does not say which relation has which options: %s", differences[0])
	}
}

// The preflight hook's refusal surfaces as the swap's.
func TestTableSwapPreflightStateRefusesByCall(t *testing.T) {
	refusal := errors.New("synthetic refusal")
	testTableSwapPreflight.reset(map[int]error{2: refusal})
	defer testTableSwapPreflight.reset(map[int]error{})
	if _, err := testTableSwapPreflight.preflight(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := testTableSwapPreflight.preflight(context.Background(), nil); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	if testTableSwapPreflight.callCount() != 2 {
		t.Fatal(testTableSwapPreflight.callCount())
	}
}
