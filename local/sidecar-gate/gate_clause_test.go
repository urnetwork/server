package main

// QD-1084's clause registry at the gate tool (TEST.md's rule 7): a case's row, as sidecar's convention writes it with
// the case's registered clauses' tests after its own, reads failed when one clause's test fails, passed only when
// every test passes, and missing when a clause's test is not there; the sweep check holds a registered clause's T3 test
// to its package like any test a sweep names, where only a convention entry nobody wrote is left out; and every clause
// server's tests run through fault.RunClause by a literal row and name is registered, but those named here.

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// The case's row at gate as GateFile writes it, failing t when the gate holds none.
func gateCaseRowForTest(t *testing.T, gate string, rowId string, caseName string) *fault.GateRow {
	t.Helper()
	for _, row := range fault.GateRows(fault.DefaultCatalog(), gate) {
		if row.Row == rowId && row.Case == caseName {
			return row
		}
	}
	t.Fatalf("%s holds no %s %q", gate, rowId, caseName)
	return nil
}

// FS-9's row at M2, TestFs9 and its unfolded listing clause's test, on a fixture whose TestFs9 passes: with the clause's
// test failing the row reads fail, with one that passes in its place pass, and with one nobody wrote missing. Each entry
// keeps the real row's shape and names, moved onto the fixture's package.
func TestGateReadsACaseFailedWhenOneClauseFails(t *testing.T) {
	row := gateCaseRowForTest(t, "M2", "FS-9", "")
	names := []string{}
	for _, test := range row.Tests {
		names = append(names, test.Names...)
	}
	if !slices.Equal(names, []string{"TestFs9", "TestFs9UnfoldedListing"}) || len(row.Tests) != 2 {
		t.Fatalf("FS-9 at M2 names %v in %d entries: the convention no longer gives it its clause's test beside its own", names, len(row.Tests))
	}
	// the row's entries on the fixture, the clause's test renamed where a row needs another
	entries := func(clauseName string) string {
		var text strings.Builder
		for _, test := range row.Tests {
			entryNames := []string{}
			for _, name := range test.Names {
				if name == "TestFs9UnfoldedListing" {
					name = clauseName
				}
				entryNames = append(entryNames, name)
			}
			text.WriteString("        - {repo: fixture, package: ./widget, names: [" + strings.Join(entryNames, ", ") + "]}\n")
		}
		return text.String()
	}
	gates := "gates:\n  M2:\n" +
		"    - row: FS-9\n      tests:\n" + entries("TestFs9UnfoldedListing") +
		"    - row: FS-908\n      tests:\n" + entries("TestFs9UnfoldedListingPassing") +
		"    - row: FS-909\n      tests:\n" + entries("TestFs9UnfoldedListingNobodyWrote")
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M2"
	clauseTests := "package widget\n\nimport \"testing\"\n\n" +
		"func TestFs9(t *testing.T) {\n}\n\n" +
		"func TestFs9UnfoldedListing(t *testing.T) {\n\tt.Fatal(\"a planted failure of one clause\")\n}\n\n" +
		"func TestFs9UnfoldedListingPassing(t *testing.T) {\n}\n"
	if err := os.WriteFile(filepath.Join(options.root, "fixture", "widget", "clause_test.go"), []byte(clauseTests), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	got := gateOutcomes(report.rows)
	for label, want := range map[string]string{"FS-9": gateFail, "FS-908": gatePass, "FS-909": gateMissing} {
		if got[label] != want {
			t.Errorf("%s reads %s, want %s", label, got[label], want)
		}
	}
	if !report.failed() {
		t.Error("a case whose clause failed did not fail the run")
	}
}

// A registered clause's T3 test is held to its package like any test a sweep names: FS-62's row at M2 names its own T3
// test in evshost and its clauses' in evshost and sidecart3, and on a fixture server declaring evshost's two but not the
// ACL gate's in sidecart3, the check names that one test, though it sits in the convention's package.
func TestGateSweepCheckHoldsARegisteredClausesT3Test(t *testing.T) {
	registration := fault.LookupClause("FS-62", "acl gate")
	if registration == nil || registration.T3Package != "./sidecart3" {
		t.Fatalf("FS-62's acl gate clause: %+v", registration)
	}
	row := gateCaseRowForTest(t, "M2", "FS-62", "")
	tests := []gateTest{}
	for _, test := range row.Tests {
		tests = append(tests, gateTest{Repo: test.Repo, Dir: test.Dir, Package: test.Package, Tags: test.Tags, Stores: test.Stores, Names: slices.Clone(test.Names)})
	}
	serverDirectory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(serverDirectory, "evshost"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "//go:build sidecar_localstack && sidecar_testhooks\n\npackage evshost_test\n\nimport \"testing\"\n\n" +
		"func TestFs62OnT3(t *testing.T) {\n}\n\nfunc TestFs62LeaseHolderResyncOnT3(t *testing.T) {\n}\n"
	if err := os.WriteFile(filepath.Join(serverDirectory, "evshost", "fs_state_test.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gates := &gatesFile{Gates: map[string][]gateRow{"M2": {{Row: "FS-62", Tests: tests}}}}
	checked, faults, err := checkGateServerTestsDeclared(gates, fault.DefaultCatalog(), serverDirectory)
	if err != nil {
		t.Fatal(err)
	}
	wantChecked := []string{"M2 FS-62 ./evshost TestFs62OnT3", "M2 FS-62 ./evshost TestFs62LeaseHolderResyncOnT3"}
	if !slices.Equal(checked, wantChecked) {
		t.Errorf("checked %v, want %v", checked, wantChecked)
	}
	wantFaults := []string{"M2's FS-62 names server/./sidecart3's TestFs62AclGateOnT3, which no test file there declares"}
	if !slices.Equal(faults, wantFaults) {
		t.Errorf("found %q, want %q", faults, wantFaults)
	}
}

// What server's tests run through fault.RunClause and nobody has registered: FS-91's clause, whose T2 test file
// fi27-cached-head-clause holds (QD-1146), so QD-1084's sweep left its registration to its owner. One here that is
// registered fails, to come off.
var gateClauseUnregisteredRuns = map[string]string{
	"FS-91 within the window": "sidecar's fs91_test.go is fi27-cached-head-clause's, in the ops lane",
}

// The fault.RunClause and fault.CheckClause calls the test files under directory make with a literal row and clause,
// each as "<row> <clause>", and a fault for each that names no clause lookup finds, but the twin a forged case's clause
// runs and what unregistered names, and for each unregistered run lookup finds.
func gateServerClauseRuns(t *testing.T, directory string, lookup func(rowId string, clause string) *fault.ClauseRegistration, unregistered map[string]string) ([]string, []string) {
	t.Helper()
	literal := func(expression ast.Expr) (string, bool) {
		basic, ok := expression.(*ast.BasicLit)
		if !ok || basic.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(basic.Value)
		return value, err == nil
	}
	fileSet := token.NewFileSet()
	runs := []string{}
	faults := []string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != directory && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte("fault.RunClause(")) && !bytes.Contains(data, []byte("fault.CheckClause(")) {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, data, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "RunClause" && selector.Sel.Name != "CheckClause") || len(call.Args) < 3 {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "fault" {
				return true
			}
			rowId, rowOk := literal(call.Args[1])
			clause, clauseOk := literal(call.Args[2])
			if !rowOk || !clauseOk {
				return true
			}
			run := rowId + " " + clause
			runs = append(runs, run)
			_, isUnregistered := unregistered[run]
			switch registration := lookup(rowId, clause); {
			case clause == "twin" || strings.HasSuffix(clause, " twin"):
			case registration != nil && isUnregistered:
				faults = append(faults, fmt.Sprintf("%s: %s is registered: take it off the unregistered runs, and run it through fault.RunRegisteredClause", relative, registration))
			case registration == nil && !isUnregistered:
				faults = append(faults, fmt.Sprintf("%s: %s %q runs through fault.%s and is not registered (sidecar's clauses.go), so no gate names it", relative, rowId, clause, selector.Sel.Name))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return runs, faults
}

// Every fault.RunClause or fault.CheckClause call a server test makes with a literal row and clause names a registered
// clause (sidecar's clauses.go), but the twin a forged case's clause runs and what gateClauseUnregisteredRuns names: a
// clause run outside the registry is one no gate names, so a new T3 clause fails here until it registers, as a new T2
// one fails sidecar's TestEveryRunClauseCallIsRegistered.
func TestEveryServerRunClauseCallIsRegistered(t *testing.T) {
	serverDirectory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runs, faults := gateServerClauseRuns(t, serverDirectory, fault.LookupClause, gateClauseUnregisteredRuns)
	for _, found := range faults {
		t.Error(found)
	}
	for run := range gateClauseUnregisteredRuns {
		if !slices.Contains(runs, run) {
			t.Errorf("gateClauseUnregisteredRuns names %s, which no server test runs any more", run)
		}
	}
}

// The scan fails a run nobody registered and one listed unregistered that the registry finds, and passes a registered
// run, a forged case's twin and a listed unregistered one: a fixture server's calls, FS-1's concurrent retries the
// registered clause.
func TestTheServerRunClauseScanCatchesAnUnregisteredRun(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "evshost"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "package evshost_test\n\nimport (\n\t\"testing\"\n\n\t\"github.com/urnetwork/sidecar/sidecartest/fault\"\n)\n\n" +
		"func TestFixture(t *testing.T) {\n" +
		"\tfault.RunClause(t, \"FS-1\", \"concurrent retries\", nil, nil)\n" +
		"\tfault.RunClause(t, \"FS-1\", \"nobody registered\", nil, nil)\n" +
		"\tfault.RunClause(t, \"FS-10\", \"twin\", nil, nil)\n" +
		"\tfault.RunClause(t, \"FS-1\", \"listed\", nil, nil)\n" +
		"\tfault.RunClause(t, \"FS-1\", \"unknown deletes nothing\", nil, nil)\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(directory, "evshost", "fixture_test.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	unregistered := map[string]string{"FS-1 listed": "a fixture's", "FS-1 unknown deletes nothing": "a fixture's"}
	runs, faults := gateServerClauseRuns(t, directory, fault.LookupClause, unregistered)
	if len(runs) != 5 {
		t.Errorf("the scan found %v", runs)
	}
	want := []string{
		`evshost/fixture_test.go: FS-1 "nobody registered" runs through fault.RunClause and is not registered (sidecar's clauses.go), so no gate names it`,
		"evshost/fixture_test.go: FS-1's clause unknown deletes nothing on T2+T3 is registered: take it off the unregistered runs, and run it through fault.RunRegisteredClause",
	}
	if !slices.Equal(faults, want) {
		t.Errorf("the scan found %q, want %q", faults, want)
	}
}
