package sidecart3

// M7's and M8's sweeps on server's side (TEST-M7-01, TEST-M8-01; sidecar's gate_m7.go and gate_m8.go): the tests their
// items name in server are declared where each item says, under its tags (gate_m6_test.go's check), and the rows the
// catalog puts on T3 that no owner has written a T3 test for keep the convention's entry in this package, so each
// reads missing here rather than passing on its T2 run.

import (
	"reflect"
	"slices"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// The server tests the items of gate name, by package.
func gateItemServerTestNames(gate string) map[string][]string {
	packageNames := map[string][]string{}
	for _, row := range fault.GateRows(fault.DefaultCatalog(), gate) {
		if row.Item == "" {
			continue
		}
		for _, test := range row.Tests {
			if test.Repo == "server" {
				packageNames[test.Package] = append(packageNames[test.Package], test.Names...)
			}
		}
	}
	return packageNames
}

// M7's items name server tests in this package alone, the rules' and the store clock's, and M8's name the forwarding
// cases in controller and model and the indexer paths' T3 run here; each is declared under its tags.
func TestGateM7AndM8ItemsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	for _, gate := range []string{"M7", "M8"} {
		for _, name := range gateItemServerTestsUndeclared(t, fault.GateRows(catalog, gate)) {
			t.Errorf("an item of %s names %s, which is not declared so", gate, name)
		}
	}
	m7 := gateItemServerTestNames("M7")
	if len(m7) != 1 || !slices.Contains(m7["./sidecart3"], "TestGateT3RunsTheNightlySeeds") {
		t.Errorf("M7's items name %v in server", m7)
	}
	m8 := gateItemServerTestNames("M8")
	for packagePath, names := range map[string][]string{
		"./sidecart3":  {"TestGateT3RunsTheNightlySeeds", "TestT3MailM8IndexerPathsTakeMailAndEndThePrompt"},
		"./controller": {"TestFs38ForwardingOnT3", "TestFwdAppRollbackOnT3"},
		"./model":      {"TestForwardingClientMint", "TestForwardingClientMintRefusals"},
	} {
		for _, name := range names {
			if !slices.Contains(m8[packagePath], name) {
				t.Errorf("no item of M8 names %s in %s", name, packagePath)
			}
		}
	}
}

// The T3 runs nothing has written stay the convention's entries in this package under its tags: FS-47's and FS-82's
// mail cases and FS-121 at M7, whose T2 runs M7's sweep names, and FS-23's mailbox case, which the sweep leaves alone.
func TestGateM7KeepsTheConventionsT3EntryWhereNoT3TestIsWritten(t *testing.T) {
	catalog := fault.DefaultCatalog()
	rows := fault.GateRows(catalog, "M7")
	for _, want := range []struct {
		rowId    string
		caseName string
	}{
		{rowId: "FS-23", caseName: "mailbox"},
		{rowId: "FS-47", caseName: "mail"},
		{rowId: "FS-82", caseName: "mail"},
		{rowId: "FS-121"},
	} {
		testCase := catalog.Case(want.rowId, want.caseName)
		if testCase == nil {
			t.Errorf("the catalog holds no %s %q", want.rowId, want.caseName)
			continue
		}
		convention := &fault.GateTest{Repo: "server", Package: "./sidecart3", Tags: "sidecar_testhooks,sidecar_pending,sidecar_localstack", Stores: true, Names: []string{fault.GateT3TestName(testCase)}}
		serverTests := []*fault.GateTest{}
		for _, row := range rows {
			if row.Row != want.rowId || row.Case != want.caseName {
				continue
			}
			for _, test := range row.Tests {
				if test.Repo == "server" {
					serverTests = append(serverTests, test)
				}
			}
		}
		if len(serverTests) != 1 || !reflect.DeepEqual(serverTests[0], convention) {
			t.Errorf("%s at M7 runs %d server entries, not the convention's alone", testCase, len(serverTests))
		}
	}
}
