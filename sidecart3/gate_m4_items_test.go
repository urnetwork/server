package sidecart3

// M4's sweep names server tests in its items as well as its rows (sidecar's gate_m4.go; TEST-M4-04): S8 and the flow
// path in controller, the escrow comparator and its benchmark harness in model and cli/escrowbench, and FI-11's run
// in this package. local/sidecar-gate's TestGateSweepsNameServerTestsDeclaredUnderTheirTags holds the rows' entries;
// this holds the items', so one that is renamed, or whose file's build tags drift from its entry's, fails here before
// its item reads missing at the gate. It reads source files and needs no store.

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestGateM4ItemsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	serverDirectory, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	checked := []string{}
	for _, row := range fault.GateRows(fault.DefaultCatalog(), "M4") {
		if row.Item == "" {
			continue
		}
		for _, test := range row.Tests {
			if test.Repo != "server" {
				continue
			}
			for _, err := range fault.GateTestDeclared(serverDirectory, test) {
				t.Errorf("M4's %q: %v", row.Item, err)
			}
			for _, name := range test.Names {
				checked = append(checked, test.Package+" "+name)
			}
		}
	}
	for _, want := range []string{
		"./sidecart3 TestFi11RunsAfterEveryStepOnT3",
		"./sidecart3 TestGateT3RunsTheNightlySeeds",
		"./controller TestScenarioS8OnT3",
		"./model TestEscrowBenchmarkMeasurements",
		"./model TestEscrowDrawLessIsAStrictTotalOrder",
		"./cli/escrowbench TestEntryFailsAnAddedRoundTrip",
	} {
		if !slices.Contains(checked, want) {
			t.Errorf("%q was not checked; checked %v", want, checked)
		}
	}
}
