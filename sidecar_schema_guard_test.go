package server

// §22.7's schema guard, server's half (TEST-M1-03; I-18, R-TEST-3): "every server-visible column ... maps
// to a list row and back". The sidecar migration groups are applied to the test's scratch database, and
// every column of the tables they create, and every column they add to a table that exists today, must
// map to a live row of §2.8's list through local/sidecar-columns.yml, a row whose store names the table
// or the column; a mapped column that no longer exists fails too.
//
// A group reads only where this binary links it: under -tags sidecar_pending every group does, and
// without it the pending groups are invisible, so the guard reports each as not read, by a skip, and
// never as passed. The groups' own SQL, read under the tag, catches a table or added column that
// sidecar-columns.yml has not heard of. Run it on the private store only, through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar/sidecartest"
)

// The column map, relative to this repository.
const sidecarColumnsPath = "local/sidecar-columns.yml"

// sidecar-columns.yml: per table, its group, whether it existed before the sidecar, and each column's
// row of §2.8's list.
type sidecarColumnsFile struct {
	Tables map[string]*sidecarColumnsTable `yaml:"tables"`
}

type sidecarColumnsTable struct {
	// MG0 to MG9: the group that creates the table or adds its columns
	Group string `yaml:"group"`
	// a table that exists before the sidecar, of which only the mapped columns are the sidecar's
	Existing bool `yaml:"existing"`
	// each column's row id, then, for a column a row names in words rather than by name, those words:
	// "api-11 creator"
	Columns map[string]string `yaml:"columns"`
}

// Whether this binary links each group, which is when its tables exist.
var sidecarGroupLinkedFunctions = map[string]func() bool{
	"MG0": SidecarMg0Linked, "MG1": SidecarMg1Linked, "MG2": SidecarMg2Linked, "MG5": SidecarMg5Linked,
	"MG6": SidecarMg6Linked, "MG7": SidecarMg7Linked, "MG8": SidecarMg8Linked, "MG9": SidecarMg9Linked,
}

func readSidecarColumns(t testing.TB, path string) *sidecarColumnsFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if index := bytes.IndexByte(data, '\t'); 0 <= index {
		t.Fatalf("%s: line %d holds a tab", path, 1+bytes.Count(data[:index], []byte("\n")))
	}
	columns := &sidecarColumnsFile{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(columns); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return columns
}

// What the sidecar groups' SQL creates and adds: tables, and columns added to a table by name.
type sidecarSchemaTouch struct {
	tables       []string
	addedColumns map[string][]string
}

var (
	sidecarCreateTablePattern = regexp.MustCompile(`(?is)\bCREATE TABLE (?:IF NOT EXISTS )?([a-z_][a-z0-9_]*)`)
	sidecarAlterTablePattern  = regexp.MustCompile(`(?is)\bALTER TABLE (?:ONLY )?([a-z_][a-z0-9_]*)\s+(.*)`)
	sidecarAddColumnPattern   = regexp.MustCompile(`(?is)\bADD COLUMN (?:IF NOT EXISTS )?([a-z_][a-z0-9_]*)`)
)

// The tables and added columns in migrations' SQL.
func sidecarSchemaTouches(migrations []any) *sidecarSchemaTouch {
	touch := &sidecarSchemaTouch{addedColumns: map[string][]string{}}
	for _, migration := range migrations {
		sqlMigration, ok := migration.(*SqlMigration)
		if !ok {
			continue
		}
		for _, match := range sidecarCreateTablePattern.FindAllStringSubmatch(sqlMigration.sql, -1) {
			touch.tables = append(touch.tables, strings.ToLower(match[1]))
		}
		for _, statement := range strings.Split(sqlMigration.sql, ";") {
			alter := sidecarAlterTablePattern.FindStringSubmatch(statement)
			if alter == nil {
				continue
			}
			table := strings.ToLower(alter[1])
			for _, added := range sidecarAddColumnPattern.FindAllStringSubmatch(alter[2], -1) {
				touch.addedColumns[table] = append(touch.addedColumns[table], strings.ToLower(added[1]))
			}
		}
	}
	slices.Sort(touch.tables)
	touch.tables = slices.Compact(touch.tables)
	return touch
}

// Every column of the named tables in the current schema, as table.column.
func sidecarSchemaColumns(ctx context.Context, tables []string) map[string]bool {
	columns := map[string]bool{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT table_name || '.' || column_name
			FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ANY($1)
		`, tables)
		WithPgResult(result, err, func() {
			for result.Next() {
				var column string
				Raise(result.Scan(&column))
				columns[column] = true
			}
		})
	})
	return columns
}

// Whether a row of the list is live and stores the column: a table the sidecar creates must be one the
// row's store names, and a column added to an existing table must be named there with its table, as
// "auth_code (api_key_id)" or "network_client.acl_subject_client_id", or alone as a column. A column's
// name alone never maps a created table, since a row naming network_id would then cover any table.
func sidecarRowStores(row *sidecartest.MetadataRow, table string, column string, existing bool, words string) bool {
	if row == nil || !row.Live() {
		return false
	}
	if words != "" && !strings.Contains(strings.ToLower(row.Store+" "+row.Sees), strings.ToLower(words)) {
		return false
	}
	tableNamed := false
	columnNamed := false
	for _, family := range row.Families() {
		switch {
		case family.Kind == sidecartest.MetadataFamilyTable && family.Name == table:
			tableNamed = true
		case family.Kind == sidecartest.MetadataFamilyColumn && (family.Name == column || family.Name == table+"."+column):
			columnNamed = true
		}
	}
	if !existing {
		return tableNamed
	}
	return strings.Contains(row.Store, table+"."+column) || (tableNamed && (words != "" || strings.Contains(row.Store, column))) || columnNamed
}

// The guard over what the database holds: each column of a mapped or created table, and each added
// column, maps to a live row that stores it, and each mapped column exists. linked says whether a
// table's group is in this binary; a table whose group is not is not read, and is returned.
func sidecarSchemaGuardFindings(columns *sidecarColumnsFile, touch *sidecarSchemaTouch, held map[string]bool,
	list *sidecartest.MetadataList, linked func(group string) bool) (findings []string, notRead []string) {
	readTables := map[string]bool{}
	for _, table := range slices.Sorted(mapsKeys(columns.Tables)) {
		entry := columns.Tables[table]
		if _, known := sidecarGroupLinkedFunctions[entry.Group]; !known {
			findings = append(findings, fmt.Sprintf("%s names group %q", table, entry.Group))
			continue
		}
		if !linked(entry.Group) {
			notRead = append(notRead, table+" ("+entry.Group+")")
			continue
		}
		readTables[table] = true
		for column, mapping := range entry.Columns {
			// "api-11" or "api-11 creator": a row, and the words its store or sees names the column by
			rowId, words, _ := strings.Cut(mapping, " ")
			if !held[table+"."+column] {
				findings = append(findings, fmt.Sprintf("%s.%s is mapped to %s and does not exist", table, column, rowId))
			}
			if !sidecarRowStores(list.Row(rowId), table, column, entry.Existing, words) {
				findings = append(findings, fmt.Sprintf("%s.%s maps to %s, which is no live row of §2.8's list storing %s or %s", table, column, rowId, table, column))
			}
		}
	}
	// what the database holds of a table the groups create, or that sidecar-columns.yml maps whole
	for _, qualified := range slices.Sorted(mapsKeys(held)) {
		table, column, _ := strings.Cut(qualified, ".")
		entry, mapped := columns.Tables[table]
		created := slices.Contains(touch.tables, table)
		switch {
		case mapped && entry.Existing:
			// a table from before the sidecar: only its added columns are the sidecar's, which the groups'
			// SQL names below
		case mapped:
			if _, mappedColumn := entry.Columns[column]; !mappedColumn {
				findings = append(findings, fmt.Sprintf("%s maps to no row of §2.8's list", qualified))
			}
		case created:
			findings = append(findings, fmt.Sprintf("%s is a table the sidecar groups create, which %s does not map", table, sidecarColumnsPath))
		}
	}
	for table, added := range touch.addedColumns {
		entry, mapped := columns.Tables[table]
		for _, column := range added {
			if !mapped || entry.Columns[column] == "" {
				findings = append(findings, fmt.Sprintf("%s.%s is a column the sidecar groups add, which %s does not map", table, column, sidecarColumnsPath))
			}
		}
		if mapped && !entry.Existing && !slices.Contains(touch.tables, table) {
			findings = append(findings, fmt.Sprintf("%s gains columns and is not marked existing", table))
		}
	}
	for _, table := range touch.tables {
		if _, mapped := columns.Tables[table]; !mapped && !readTables[table] {
			findings = append(findings, fmt.Sprintf("%s is a table the sidecar groups create, which %s does not map", table, sidecarColumnsPath))
		}
	}
	sort.Strings(findings)
	return slices.Compact(findings), notRead
}

func mapsKeys[V any](values map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range values {
			if !yield(key) {
				return
			}
		}
	}
}

// The tables a guard reads: those the column map names and those the groups create.
func sidecarGuardTables(columns *sidecarColumnsFile, touch *sidecarSchemaTouch) []string {
	tables := slices.Collect(mapsKeys(columns.Tables))
	tables = append(tables, touch.tables...)
	slices.Sort(tables)
	return slices.Compact(tables)
}

func sidecarGuardGroupLinked(group string) bool {
	linked, known := sidecarGroupLinkedFunctions[group]
	return known && linked()
}

// The guard on this binary's database: every sidecar column maps to a row of §2.8's list and back.
func TestSidecarSchemaGuard(t *testing.T) {
	list, err := sidecartest.LoadMetadataList()
	if err != nil {
		t.Skipf("not read: §2.8's list is not beside this checkout: %v", err)
	}
	columns := readSidecarColumns(t, sidecarColumnsPath)
	touch := sidecarSchemaTouches(sidecarSchemaGuardMigrations())
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		held := sidecarSchemaColumns(ctx, sidecarGuardTables(columns, touch))
		findings, notRead := sidecarSchemaGuardFindings(columns, touch, held, list, sidecarGuardGroupLinked)
		for _, finding := range findings {
			t.Error(finding)
		}
		if 0 < len(notRead) {
			t.Logf("not read, their groups not linked in this binary (run with -tags sidecar_pending): %s", strings.Join(notRead, ", "))
		}
		if len(notRead) == len(columns.Tables) {
			t.Skip("not read: no sidecar group is linked in this binary, which is no pass; run with -tags sidecar_pending")
		}
		t.Logf("read %d columns of %d tables against %s", len(held), len(columns.Tables)-len(notRead), list.Version)
	})
}

// The values §22.7 names the guard must cover, once their groups exist: helo_name, network_client_lease
// and mail_connection_record.
func TestSidecarSchemaGuardNamesSection227sValues(t *testing.T) {
	columns := readSidecarColumns(t, sidecarColumnsPath)
	touch := sidecarSchemaTouches(sidecarSchemaGuardMigrations())
	for _, name := range []string{"helo_name", "network_client_lease", "mail_connection_record"} {
		mapped := false
		for table, entry := range columns.Tables {
			if table == name {
				mapped = true
			}
			if _, found := entry.Columns[name]; found {
				mapped = true
			}
		}
		created := slices.Contains(touch.tables, name)
		for _, added := range touch.addedColumns {
			created = created || slices.Contains(added, name)
		}
		switch {
		case mapped:
		case created:
			t.Errorf("%s is created by a group and not in %s", name, sidecarColumnsPath)
		default:
			t.Logf("%s: its group does not create it yet", name)
		}
	}
}

// A fixture group adding a column the map lacks fails, naming it; mapping it to a row that stores it
// passes, and a mapped column dropped fails. The fixture's tables are created in the scratch database,
// as a group's would be.
func TestSidecarSchemaGuardFailsOnAFixtureGroupsUnmappedColumn(t *testing.T) {
	if _, err := sidecartest.LoadMetadataList(); err != nil {
		t.Skipf("not read: §2.8's list is not beside this checkout: %v", err)
	}
	fixture := []any{
		newSqlMigration(`CREATE TABLE sidecar_guard_fixture (network_id uuid NOT NULL, planted_secret text NULL)`),
		newSqlMigration(`SET LOCAL lock_timeout = '2s'; ALTER TABLE sidecar_guard_existing ADD COLUMN planted_added uuid NULL`),
	}
	touch := sidecarSchemaTouches(fixture)
	if !slices.Equal(touch.tables, []string{"sidecar_guard_fixture"}) || !slices.Equal(touch.addedColumns["sidecar_guard_existing"], []string{"planted_added"}) {
		t.Fatalf("the fixture group's SQL read as %v and %v", touch.tables, touch.addedColumns)
	}
	linked := func(string) bool { return true }
	DefaultTestEnv().Run(t, func(t testing.TB) {
		// fresh on every attempt, since a test env may rerun this body
		list, err := sidecartest.LoadMetadataList()
		if err != nil {
			t.Fatal(err)
		}
		columns := &sidecarColumnsFile{Tables: map[string]*sidecarColumnsTable{
			"sidecar_guard_fixture":  {Group: "MG1", Columns: map[string]string{"network_id": "api-5"}},
			"sidecar_guard_existing": {Group: "MG1", Existing: true, Columns: map[string]string{}},
		}}
		ctx := context.Background()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE sidecar_guard_fixture (network_id uuid NOT NULL, planted_secret text NULL)`))
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE sidecar_guard_existing (id uuid NOT NULL, planted_added uuid NULL)`))
		})
		held := sidecarSchemaColumns(ctx, sidecarGuardTables(columns, touch))
		findings, _ := sidecarSchemaGuardFindings(columns, touch, held, list, linked)
		want := []string{
			"sidecar_guard_existing.planted_added is a column the sidecar groups add, which " + sidecarColumnsPath + " does not map",
			"sidecar_guard_fixture.network_id maps to api-5, which is no live row of §2.8's list storing sidecar_guard_fixture or network_id",
			"sidecar_guard_fixture.planted_secret maps to no row of §2.8's list",
		}
		if !slices.Equal(findings, want) {
			t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(findings, "\n"), strings.Join(want, "\n"))
		}

		// a row whose store names the table and the added column passes both
		columns.Tables["sidecar_guard_fixture"].Columns["planted_secret"] = "api-5"
		columns.Tables["sidecar_guard_existing"].Columns["planted_added"] = "api-5"
		list.Row("api-5").Store += "; sidecar_guard_fixture, sidecar_guard_existing.planted_added"
		findings, _ = sidecarSchemaGuardFindings(columns, touch, held, list, linked)
		if len(findings) != 0 {
			t.Errorf("a mapped fixture still fails: %v", findings)
		}
		columns.Tables["sidecar_guard_fixture"].Columns["dropped"] = "api-5"
		findings, _ = sidecarSchemaGuardFindings(columns, touch, held, list, linked)
		if !slices.Equal(findings, []string{"sidecar_guard_fixture.dropped is mapped to api-5 and does not exist"}) {
			t.Errorf("a mapped column that does not exist: %v", findings)
		}
	})
}

// Without a linked group, nothing reads as passed: each table is reported not read.
func TestSidecarSchemaGuardReportsUnlinkedGroupsAsNotRead(t *testing.T) {
	columns := readSidecarColumns(t, sidecarColumnsPath)
	findings, notRead := sidecarSchemaGuardFindings(columns, &sidecarSchemaTouch{addedColumns: map[string][]string{}},
		map[string]bool{}, &sidecartest.MetadataList{}, func(string) bool { return false })
	if len(findings) != 0 || len(notRead) != len(columns.Tables) {
		t.Errorf("an unlinked binary: %d findings, %d of %d tables not read", len(findings), len(notRead), len(columns.Tables))
	}
	if _, err := os.Stat(filepath.Join("..", "sidecar", "metadata_list.yml")); err != nil {
		t.Logf("§2.8's list is not beside this checkout: %v", err)
	}
}
