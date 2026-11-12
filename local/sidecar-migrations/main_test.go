package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// promote and check-tables, on copies of the real files and on small fixtures.
// Nothing here edits the checkout: every promotion runs on a copy in the test's
// own directory, and builds read it through an overlay.

// The server checkout this package sits in.
const serverDirectory = "../.."

// Copies the two files promote edits into a new directory.
func copyPromoteFiles(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{pendingFileName, migrationsFileName} {
		data, err := os.ReadFile(filepath.Join(serverDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// The top-level function names of a Go file.
func functionNames(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parseGoSource(path, data)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for name := range source.functions() {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Every string literal inside the named function, unquoted.
func functionStrings(t *testing.T, path string, name string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parseGoSource(path, data)
	if err != nil {
		t.Fatal(err)
	}
	function, ok := source.functions()[name]
	if !ok {
		t.Fatalf("%s declares no %s", path, name)
	}
	values := []string{}
	ast.Inspect(function, func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
		return true
	})
	return values
}

// Promoting MG0 takes exactly MG0's function out of the pending file and its
// call out of sidecarPendingMigrations, and adds its entries, marker last, to
// the end of the slice's literal, each SQL string byte for byte, so each entry's
// identity is the one a database recorded while the group was pending.
func TestPromoteMovesOnlyItsOwnGroup(t *testing.T) {
	directory := copyPromoteFiles(t)
	pendingPath := filepath.Join(directory, pendingFileName)
	migrationsPath := filepath.Join(directory, migrationsFileName)
	before := functionNames(t, pendingPath)
	movedStrings := functionStrings(t, pendingPath, "sidecarMg0Migrations")
	if len(movedStrings) == 0 {
		t.Fatal("MG0 holds no SQL to move")
	}

	result, err := promote(directory, "MG0")
	if err != nil {
		t.Fatal(err)
	}

	after := functionNames(t, pendingPath)
	want := slices.DeleteFunc(slices.Clone(before), func(name string) bool { return name == "sidecarMg0Migrations" })
	if !slices.Equal(after, want) {
		t.Fatalf("the pending file now declares %v, want %v", after, want)
	}
	pendingData, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pendingData, []byte("sidecarMg0Migrations")) {
		t.Fatal("the pending file still names sidecarMg0Migrations")
	}
	if !bytes.Contains(pendingData, []byte("sidecarMg1Migrations(),")) {
		t.Fatal("the pending file lost MG1's call")
	}

	migrationsData, err := os.ReadFile(migrationsPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parseGoSource(migrationsPath, migrationsData)
	if err != nil {
		t.Fatal(err)
	}
	literal, err := migrationsLiteral(source)
	if err != nil {
		t.Fatal(err)
	}
	// MG0's six entries: the owner table, the two column changes, the index, the
	// competition's permissive step and the marker
	if result.lastVersion != len(literal.Elts) || result.firstVersion != result.lastVersion-5 {
		t.Fatalf("versions %d-%d over a literal of %d", result.firstVersion, result.lastVersion, len(literal.Elts))
	}
	moved := literal.Elts[result.firstVersion-1:]
	last, ok := moved[len(moved)-1].(*ast.CallExpr)
	if !ok || !isIdentNamed(last.Fun, "newCodeMigration") || !isIdentNamed(last.Args[0], "sidecarMg0MigrationId") {
		t.Fatal("the slice does not end with MG0's marker")
	}
	landed := []string{}
	for _, element := range moved {
		ast.Inspect(element, func(node ast.Node) bool {
			if basic, ok := node.(*ast.BasicLit); ok && basic.Kind == token.STRING {
				value, err := strconv.Unquote(basic.Value)
				if err != nil {
					t.Fatal(err)
				}
				landed = append(landed, value)
			}
			return true
		})
	}
	if !slices.Equal(landed, movedStrings) {
		t.Fatal("the moved SQL is not byte for byte what the pending group held")
	}
}

// Groups register in order, and a refused promotion writes nothing.
func TestPromoteRefusesOutOfOrder(t *testing.T) {
	directory := copyPromoteFiles(t)
	read := func() [][]byte {
		datas := [][]byte{}
		for _, name := range []string{pendingFileName, migrationsFileName} {
			data, err := os.ReadFile(filepath.Join(directory, name))
			if err != nil {
				t.Fatal(err)
			}
			datas = append(datas, data)
		}
		return datas
	}
	before := read()
	for _, c := range []struct {
		groupName string
		want      string
	}{
		{groupName: "MG1", want: "MG0 is still pending"},
		{groupName: "MG9", want: "MG0 is still pending"},
		{groupName: "MG3", want: "creates nothing"},
		{groupName: "MG10", want: "names no group"},
		{groupName: "mg0", want: "names no group"},
	} {
		_, err := promote(directory, c.groupName)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("promote %s: %v, want %q", c.groupName, err, c.want)
		}
	}
	if !slices.EqualFunc(read(), before, bytes.Equal) {
		t.Fatal("a refused promotion changed a file")
	}

	if _, err := promote(directory, "MG0"); err != nil {
		t.Fatal(err)
	}
	if _, err := promote(directory, "MG0"); err == nil || !strings.Contains(err.Error(), "is not pending") {
		t.Fatalf("a second promotion of MG0: %v", err)
	}
	if _, err := promote(directory, "MG1"); err != nil {
		t.Fatalf("MG1 after MG0: %v", err)
	}
}

// What promote writes compiles on both legs: MG0 and then MG1 moved, built from
// the real package through an overlay of the two files.
func TestPromoteResultCompilesOnBothLegs(t *testing.T) {
	directory := copyPromoteFiles(t)
	for _, groupName := range []string{"MG0", "MG1"} {
		if _, err := promote(directory, groupName); err != nil {
			t.Fatal(err)
		}
	}
	absoluteServer, err := filepath.Abs(serverDirectory)
	if err != nil {
		t.Fatal(err)
	}
	overlay := map[string]map[string]string{"Replace": {}}
	for _, name := range []string{pendingFileName, migrationsFileName} {
		overlay["Replace"][filepath.Join(absoluteServer, name)] = filepath.Join(directory, name)
	}
	overlayData, err := json.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(directory, "overlay.json")
	if err := os.WriteFile(overlayPath, overlayData, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tags := range []string{"", "sidecar_pending"} {
		command := exec.CommandContext(t.Context(), "go", "build", "-tags", tags, "-overlay", overlayPath, ".")
		command.Dir = absoluteServer
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build with tags %q after promotion: %s\n%s", tags, err, output)
		}
	}
}

// A small server directory for promote: two groups, the second with parts.
const fixtureMigrations = `package server

import (
	"context"
)

var migrations = append([]any{
	newSqlMigration("CREATE TABLE existing (id uuid)"),
}, sidecarPendingMigrations()...)

func newSqlMigration(sql string) any                             { return sql }
func newCodeMigration(id string, f func(context.Context)) any    { return id }
func sidecarPendingMigrations() []any                            { return nil }

const sidecarMg0MigrationId = "sidecar_mg0"
const sidecarMg1MigrationId = "sidecar_mg1"
`

const fixturePending = `//go:build sidecar_pending

package server

import (
	"context"
	"slices"
)

func sidecarPendingMigrations() []any {
	return slices.Concat(
		sidecarMg1Migrations(),
	)
}

// MG1.
func sidecarMg1Migrations() []any {
	return slices.Concat(
		sidecarMg1CoreMigrations(),
		[]any{
			newCodeMigration(sidecarMg1MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG1's core part.
func sidecarMg1CoreMigrations() []any {
	return []any{newSqlMigration(PENDING_SQL)}
}
`

func writePromoteFixture(t *testing.T, pending string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, migrationsFileName), []byte(fixtureMigrations), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, pendingFileName), []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// A part may use only what the migrations slice can use too: a helper the
// pending file declares would not compile on a release leg once moved.
func TestPromoteRefusesAHelperOnlyThePendingFileDeclares(t *testing.T) {
	refused := strings.Replace(fixturePending, "PENDING_SQL", "pendingTableSql", 1) + "\nconst pendingTableSql = `CREATE TABLE t (id uuid)`\n"
	directory := writePromoteFixture(t, refused)
	if _, err := promote(directory, "MG1"); err == nil || !strings.Contains(err.Error(), "pendingTableSql") {
		t.Fatalf("a part using a pending-only constant: %v", err)
	}

	twin := strings.Replace(fixturePending, "PENDING_SQL", "`CREATE TABLE t (id uuid)`", 1)
	directory = writePromoteFixture(t, twin)
	if _, err := promote(directory, "MG1"); err != nil {
		t.Fatalf("the same part written inline: %v", err)
	}
}

// The last group's promotion leaves sidecarPendingMigrations returning nil, with
// imports nothing uses dropped, and the one-line literal gains its comma.
func TestPromoteOfTheLastGroupLeavesNil(t *testing.T) {
	directory := writePromoteFixture(t, strings.Replace(fixturePending, "PENDING_SQL", "`CREATE TABLE t (id uuid)`", 1))
	result, err := promote(directory, "MG1")
	if err != nil {
		t.Fatal(err)
	}
	if result.firstVersion != 2 || result.lastVersion != 3 {
		t.Fatalf("versions %d-%d, want 2-3", result.firstVersion, result.lastVersion)
	}
	pendingData, err := os.ReadFile(filepath.Join(directory, pendingFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pendingData, []byte("return nil")) || bytes.Contains(pendingData, []byte(`"context"`)) || bytes.Contains(pendingData, []byte(`"slices"`)) {
		t.Fatalf("the pending file after the last group:\n%s", pendingData)
	}
	migrationsData, err := os.ReadFile(filepath.Join(directory, migrationsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(migrationsData, []byte("newSqlMigration(`CREATE TABLE t (id uuid)`),")) {
		t.Fatalf("the migrations file after the last group:\n%s", migrationsData)
	}
}

// A check-tables fixture: a server directory whose pending groups create the
// tables given, a conf and a metadata list.
func writeCheckTablesFixture(t *testing.T, tables map[string][]string, conf string, metadata string) (string, string, string) {
	t.Helper()
	directory := t.TempDir()
	var pending strings.Builder
	pending.WriteString("//go:build sidecar_pending\n\npackage server\n")
	for _, groupNumber := range []string{"0", "1", "2"} {
		pending.WriteString("\nfunc sidecarMg" + groupNumber + "CoreMigrations() []any {\n\treturn []any{\n")
		for _, table := range tables["MG"+groupNumber] {
			pending.WriteString("\t\tnewSqlMigration(`\n\t\t\tCREATE TABLE " + table + " (id uuid NOT NULL)\n\t\t`),\n")
		}
		pending.WriteString("\t}\n}\n")
	}
	if err := os.WriteFile(filepath.Join(directory, pendingFileName), []byte(pending.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, migrationsFileName), []byte(fixtureMigrations), 0o600); err != nil {
		t.Fatal(err)
	}
	confPath := filepath.Join(directory, "sidecar-tables.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(directory, "metadata_list.yml")
	if err := os.WriteFile(metadataPath, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory, confPath, metadataPath
}

const fixtureMetadata = `api-1:
  service: api
  store: alpha_record, beta_record, later_record, network_dns_*
  ttl: alpha 13 months; beta 7 d
api-2:
  withdrawn: folded into api-1
`

func requireCheckTables(t *testing.T, tables map[string][]string, conf string, want string) {
	t.Helper()
	directory, confPath, metadataPath := writeCheckTablesFixture(t, tables, conf, fixtureMetadata)
	report, err := checkTables(directory, confPath, metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		if len(report.findings) != 0 {
			t.Fatalf("findings %q", report.findings)
		}
		return
	}
	if !slices.ContainsFunc(report.findings, func(finding string) bool { return strings.Contains(finding, want) }) {
		t.Fatalf("findings %q lack %q", report.findings, want)
	}
}

// The passing fixture each refusal below breaks one way.
const fixtureConf = `# table dump orphans group row ttl
alpha_record                 dump  drop:network_id   MG1   api-1   13 months
beta_record                  none  -                 MG2   api-1   7 d
network_dns_zone             dump  drop:network_id   MG2   api-1   while published
later_record                 dump  keep              MG6   api-1   13 months
family network_dns_*
`

var fixtureTables = map[string][]string{"MG1": {"alpha_record"}, "MG2": {"beta_record", "network_dns_zone"}}

func TestCheckTablesPassesAConfThatMatches(t *testing.T) {
	requireCheckTables(t, fixtureTables, fixtureConf, "")
}

func TestCheckTablesFailsOnATableWithNoLine(t *testing.T) {
	tables := map[string][]string{"MG1": {"alpha_record", "gamma_record"}, "MG2": {"beta_record", "network_dns_zone"}}
	requireCheckTables(t, tables, fixtureConf, "gamma_record, which MG1 creates, has no line")
	// a family line keeps it out of the main dump, and is no line of its own
	tables = map[string][]string{"MG1": {"alpha_record"}, "MG2": {"beta_record", "network_dns_zone", "network_dns_record"}}
	requireCheckTables(t, tables, fixtureConf, "network_dns_record, which MG2 creates, has no line")
}

func TestCheckTablesFailsOnASevenDayRowMarkedDump(t *testing.T) {
	requireCheckTables(t, fixtureTables,
		strings.Replace(fixtureConf, "beta_record                  none", "beta_record                  dump", 1),
		"beta_record is dumped, and its ttl, \"7 d\", is at most 7 d")
}

func TestCheckTablesFailsOnALongRowMarkedNone(t *testing.T) {
	requireCheckTables(t, fixtureTables,
		strings.Replace(fixtureConf, "alpha_record                 dump", "alpha_record                 none", 1),
		"alpha_record is left out of every dump")
}

// A ttl that counts from an event of its own bounds nothing, so dump stands.
func TestCheckTablesTakesATtlAfterAnEventAsUnbounded(t *testing.T) {
	requireCheckTables(t, fixtureTables,
		strings.Replace(fixtureConf, "MG1   api-1   13 months", "MG1   api-1   1 d after the decision's not_after", 1),
		"")
}

func TestCheckTablesFailsOnAGroupMismatch(t *testing.T) {
	requireCheckTables(t, fixtureTables,
		strings.Replace(fixtureConf, "dump  drop:network_id   MG1", "dump  drop:network_id   MG0", 1),
		"alpha_record is created by MG1 and its line names MG0")
}

func TestCheckTablesFailsOnARowThatDoesNotStoreTheTable(t *testing.T) {
	requireCheckTables(t, map[string][]string{"MG1": {"alpha_record", "delta_record"}, "MG2": {"beta_record", "network_dns_zone"}},
		fixtureConf+"delta_record                 dump  keep              MG1   api-1   13 months\n",
		"delta_record's line names api-1, whose store does not name the table")
	requireCheckTables(t, fixtureTables,
		strings.Replace(fixtureConf, "MG1   api-1", "MG1   api-2", 1),
		"which §2.8's list withdrew")
	requireCheckTables(t, fixtureTables,
		strings.Replace(fixtureConf, "MG1   api-1", "MG1   api-9", 1),
		"names api-9, which §2.8's list does not hold")
}

// The real groups against the real conf and list, where the monoroot holds them.
func TestCheckTablesOnThisCheckout(t *testing.T) {
	confPath := filepath.Join(serverDirectory, defaultTablesConfPath)
	metadataPath := filepath.Join(serverDirectory, defaultMetadataListPath)
	for _, path := range []string{confPath, metadataPath} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("no sibling checkout holds %s", path)
		}
	}
	report, err := checkTables(serverDirectory, confPath, metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.createdTableCount == 0 {
		t.Fatal("the groups create no table")
	}
	for _, finding := range report.findings {
		t.Error(finding)
	}
}

func TestRunNamesItsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"unknown"}, &out, &errOut); code != 64 || !strings.Contains(errOut.String(), "usage") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
}
