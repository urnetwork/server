package sidecart3

// M9's sweeps on server's side (TEST-M9-01; sidecar's gate_m9.go): every server test M9's entry and exit name, for a
// row or an item, is declared where the entry says, in a file that builds under the entry's tags (gate_m6_test.go's
// check), so a renamed test or a tag its owner adds fails here rather than only reading missing at the gate: FS-31's,
// FS-123's and S6's T3 runs and the privacy row in cli/smtpedge, FS-42's M9 step and S10's records in model, S10's T3
// runs in this package, and the halt, complaint and RUA cases in controller. Reading test files opens no store, so
// this runs at every merge.

import (
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestGateM9NamesServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	rows := []*fault.GateRow{}
	for _, gate := range []string{"M9-in", "M9"} {
		for _, row := range fault.GateRows(catalog, gate) {
			// a row's tests are held as an item's: the sweep names every one of M9's cases, so none is a convention
			// entry nothing has written
			copied := *row
			if copied.Item == "" {
				copied.Item = row.Row + " " + row.Case
			}
			rows = append(rows, &copied)
		}
	}
	for _, name := range gateItemServerTestsUndeclared(t, rows) {
		t.Errorf("M9's sweeps name %s, which is not declared so", name)
	}
	packageNames := map[string][]string{}
	for _, row := range rows {
		for _, test := range row.Tests {
			if test.Repo != "server" {
				continue
			}
			packageNames[test.Package] = append(packageNames[test.Package], test.Names...)
			// a test on rows a pending group holds, MG9's smtp_* among them, skips without the group linked, and a
			// skip reads open at the gate
			if test.Stores && !slices.Contains(strings.Split(test.Tags, ","), "sidecar_pending") {
				t.Errorf("%s's %v run on the stores with tags %q", row.Item, test.Names, test.Tags)
			}
		}
	}
	for packagePath, names := range map[string][]string{
		"./cli/smtpedge": {"TestT3Fs31AnHonestSend", "TestT3Fs31AMessageRecordBeforeTheEnvelopesReplies", "TestT3Fs123QueuedFromAPhone", "TestT3S6NoUnauthenticatedMx", "TestT3SmtpSendCarriesNoCanaryPastTheDevice"},
		"./model":        {"TestSmtpDkimRevokedAfterTheDumpHasNoSelectorOnceTheReplayEnds", "TestDnsDkimDeleteInTheCallersTransaction", "TestDnsBurnerSelectorsKeepTheLabelsMail"},
		"./sidecart3":    {"TestScenarioS10ReceiveOnlyOnT3", "TestScenarioS10ReplyingOnT3", "TestGateT3RunsTheNightlySeeds"},
		"./controller":   {"TestAbuseMailRuaOpensACaseWithNoHalt", "TestAbuseMailHaltBecomesACaseAndAnAction", "TestSmtpReconcileOpensACaseOverTwentyOnePercentAndFive"},
	} {
		for _, name := range names {
			if !slices.Contains(packageNames[packagePath], name) {
				t.Errorf("M9's sweeps name no %s in %s", name, packagePath)
			}
		}
	}
	// the check catches what it is for: a row whose T3 test is renamed, and one named under a tag its file lacks
	wrong := []*fault.GateRow{{Item: "a fixture", Tests: []*fault.GateTest{
		{Repo: "server", Package: "./cli/smtpedge", Tags: "sidecar_pending,sidecar_testhooks", Names: []string{"TestT3Fs31NoSuchRowIsDeclared"}},
		// declared in fs31_test.go, which builds under sidecar_pending and sidecar_testhooks together
		{Repo: "server", Package: "./cli/smtpedge", Tags: "sidecar_pending", Names: []string{"TestT3Fs31AnHonestSend"}},
	}}}
	want := []string{
		`TestT3Fs31NoSuchRowIsDeclared in ./cli/smtpedge under "sidecar_pending,sidecar_testhooks"`,
		`TestT3Fs31AnHonestSend in ./cli/smtpedge under "sidecar_pending"`,
	}
	if got := gateItemServerTestsUndeclared(t, wrong); !slices.Equal(got, want) {
		t.Errorf("the fixture's undeclared tests read %q, want %q", got, want)
	}
}
