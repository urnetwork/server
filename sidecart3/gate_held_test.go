package sidecart3

// The held gate on server's side (TEST-M10-06; sidecar's gate_held.go): `sidecar-gate held` reports §23.6's later-of
// cases case by case, and each server test its rows and items name outside a case's convention entry is declared where
// the entry says, in a file that builds under the entry's tags, so a rename or a drifted tag fails here before the row
// or item reads missing at the gate. The hold cases' T3 runs are model's, against api's real hold/ writes (ABUSE-M10-01),
// since this package holds no conventional TestFs92OnT3, TestFs69OnT3 or TestFs66HoldOnT3: every T3 test those owners
// declare there is named, as is each mailbox case's T3 run MAIL-M10-04 declares here, so a T3 clause the gate would not
// run fails here; and no case the sweep names elsewhere has a conventional test in this package beside it, which the
// gate would never run. Reading source files opens no store, so this runs at every merge.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// The tests the files of directory matching glob declare whose names match name, in file and line order.
func gateHeldDeclaredTests(t *testing.T, directory string, glob string, name *regexp.Regexp) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, glob))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no file in %s matches %s", directory, glob)
	}
	names := []string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range gateItemTestDeclarationPattern.FindAllSubmatch(data, -1) {
			if name.Match(match[1]) {
				names = append(names, string(match[1]))
			}
		}
	}
	return names
}

// Every server test the held gate's rows name outside a case's convention entry is declared under its tags: model's
// hold cases with each clause their owners run on T3, the root package's FS-87 index and FS-99 card cases, and the
// mailbox cases' runs here. Every T3 test model's hold files declare and every mailbox case's T3 run here is among
// them.
func TestGateHeldRowsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	rows := gateRowServerTests(catalog, "held")
	for _, name := range gateItemServerTestsUndeclared(t, rows) {
		t.Errorf("a row of held names %s, which is not declared so", name)
	}
	named := map[string][]string{}
	for _, row := range rows {
		for _, test := range row.Tests {
			named[test.Package] = append(named[test.Package], test.Names...)
		}
	}
	for packagePath, names := range map[string][]string{
		"./model": {
			"TestFs66HoldPlacedByApiOnT3",
			"TestFs69HoldsPlacedByApiOnT3", "TestFs69BetweenSweepDeletesOnT3", "TestFs69AnEraseUnderTheHoldOnT3", "TestFs6ANowShredUnderAHoldOnT3",
			"TestFs92HoldsPlacedByApiOnT3", "TestFs92BetweenVerifyAndDeleteOnT3", "TestFs92BetweenDeletesOnT3", "TestFs92ADeleteInFlightOnT3",
			"TestFs92SealedHoldAnswersEqualANoHoldTwinOnT3",
		},
		".":           {"TestFs87IndexOnT3", "TestFs99CardsOnT3"},
		"./sidecart3": {"TestT3MailM10Fs76MailboxCase", "TestT3MailM10Fs78MailboxCase", "TestT3MailM10Fs111MailboxCase", "TestT3MailM10Fs112MailboxCase"},
	} {
		for _, name := range names {
			if !slices.Contains(named[packagePath], name) {
				t.Errorf("no row of held names %s in %s", name, packagePath)
			}
		}
	}
	// what the owners declare, so a T3 clause added there and not here fails
	for _, declared := range []struct {
		packagePath string
		directory   string
		glob        string
		name        *regexp.Regexp
	}{
		{packagePath: "./model", directory: filepath.Join("..", "model"), glob: "abuse_*_t3_test.go", name: regexp.MustCompile(`^TestFs[0-9]+[A-Za-z0-9]*OnT3$`)},
		{packagePath: "./sidecart3", directory: ".", glob: "mail_m10_t3_test.go", name: regexp.MustCompile(`^TestT3MailM10Fs[0-9]+MailboxCase$`)},
	} {
		names := gateHeldDeclaredTests(t, declared.directory, declared.glob, declared.name)
		if len(names) == 0 {
			t.Errorf("%s's %s declares no test the held gate runs", declared.directory, declared.glob)
		}
		for _, name := range names {
			if !slices.Contains(named[declared.packagePath], name) {
				t.Errorf("%s declares %s, which no row of held names", declared.packagePath, name)
			}
		}
	}
}

// A case the held gate names on T3 outside this package has no conventional TestFs<n><Case>OnT3 here: such a test
// would never run at the gate, whose row runs only the tests its sweep names. A case whose row keeps the convention's
// entry here is passed over, and its test reads missing until it is written.
func TestGateHeldNamesNoCaseElsewhereThatThisPackageHolds(t *testing.T) {
	catalog := fault.DefaultCatalog()
	declared := map[string]bool{}
	for _, name := range gateHeldDeclaredTests(t, ".", "*_test.go", regexp.MustCompile(`^TestFs[0-9]+`)) {
		declared[name] = true
	}
	elsewhere := 0
	for _, row := range fault.GateRows(catalog, "held") {
		if row.Row == "" {
			continue
		}
		testCase := catalog.Case(row.Row, row.Case)
		if !slices.Contains(testCase.TwinTiers(), fault.TierT3) {
			continue
		}
		convention := fault.GateT3TestName(testCase)
		kept := slices.ContainsFunc(row.Tests, func(test *fault.GateTest) bool {
			return test.Repo == "server" && test.Package == "./sidecart3" && slices.Equal(test.Names, []string{convention})
		})
		if kept {
			continue
		}
		elsewhere += 1
		if declared[convention] {
			t.Errorf("%s's T3 run is named outside this package, and this package declares %s, which the gate never runs", testCase, convention)
		}
	}
	if elsewhere != 5 {
		t.Errorf("held names %d cases' T3 runs outside this package, want 5: FS-66's hold, FS-69, FS-87's index, FS-92, FS-99's cards", elsewhere)
	}
}

// Every server test the held gate's items name is declared under its tags: the rules' and the store clock's here, and
// D-TEST-17's readings of FS-86's card case in the root package, the Team limit's on T3's layouts among them, as M10's
// exit carries them.
func TestGateHeldItemsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	for _, name := range gateItemServerTestsUndeclared(t, fault.GateRows(catalog, "held")) {
		t.Errorf("an item of held names %s, which is not declared so", name)
	}
	named := gateItemServerTestNames("held")
	for packagePath, names := range map[string][]string{
		"./sidecart3": {"TestGateT3RunsTheNightlySeeds", "TestT3StoreClockReadsAgesOnTheScheduleClock", "TestFs114RefusesTheStoreClock"},
		".":           {"TestFs86CardCommitsAtTheTeamLimitOnT3", "TestFs86CardCommitsOnTheLocalStackLogTheirP99MissesAndHoldTheRest", "TestFs86CardCommitsAtTheTeamLimitOnTheLayouts"},
	} {
		for _, name := range names {
			if !slices.Contains(named[packagePath], name) {
				t.Errorf("no item of held names %s in %s", name, packagePath)
			}
		}
	}
}
