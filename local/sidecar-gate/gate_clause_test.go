package main

// QD-1084's clause registry at the gate tool (TEST.md's rule 7): a case's row, as sidecar's convention writes it with
// the case's registered clauses' tests after its own, reads failed when one clause's test fails, passed only when
// every test passes, and missing when a clause's test is not there; and the sweep check holds a registered clause's
// T3 test to its package like any test a sweep names, where only a convention entry nobody wrote is left out.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
