package sidecart3

// M10's exit sweep on server's side (TEST-M10-05; sidecar's gate_m10_out.go): each server test its rows name outside
// the convention's ./sidecart3, and each its items name, is declared where the entry says, in a file that builds under
// the entry's tags (gate_m6_test.go's check), so a rename or a drifted tag fails here before its row or item reads
// missing at the gate. And at every gate, a server test that runs in server's test env is named on the private stores,
// since off them its preflight fails the run before the test shows anything. Reading source files opens no store, so
// this runs at every merge.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// The server tests M10's rows name outside the convention, each as an item of its own so gate_m6_test.go's check reads
// it: a row's convention entry in this package is passed over, since a T3 test not written yet reads missing by design.
func gateM10RowServerTests(catalog *fault.Catalog) []*fault.GateRow {
	rows := []*fault.GateRow{}
	for _, row := range fault.GateRows(catalog, "M10") {
		if row.Row == "" {
			continue
		}
		testCase := catalog.Case(row.Row, row.Case)
		named := &fault.GateRow{Item: testCase.String()}
		for _, test := range row.Tests {
			if test.Repo != "server" {
				continue
			}
			if test.Package == "./sidecart3" && slices.Equal(test.Names, []string{fault.GateT3TestName(testCase)}) {
				continue
			}
			named.Tests = append(named.Tests, test)
		}
		if 0 < len(named.Tests) {
			rows = append(rows, named)
		}
	}
	return rows
}

// Every server test M10's rows name outside the convention is declared under its tags: evshost's T3 runs of format 2's
// commit side, model's hold cases against api's real hold/ writes, the root package's search, directory and remote
// search runs, and §22.9's file-cluster suite for FS-114's purge and drop cases. A name nothing declares, and one
// declared only behind a tag its entry lacks, are both caught.
func TestGateM10RowsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	rows := gateM10RowServerTests(catalog)
	for _, name := range gateItemServerTestsUndeclared(t, rows) {
		t.Errorf("a row of M10 names %s, which is not declared so", name)
	}
	named := map[string][]string{}
	for _, row := range rows {
		for _, test := range row.Tests {
			named[test.Package] = append(named[test.Package], test.Names...)
		}
	}
	for packagePath, names := range map[string][]string{
		"./evshost": {"TestFs68OnT3", "TestFs72MigrationOnT3", "TestFs72RollbackOnT3", "TestFs73OnT3", "TestFs89OnT3", "TestFs91OnT3", "TestFs94OnT3", "TestFs95OnT3"},
		"./model":   {"TestFs66HoldPlacedByApiOnT3", "TestFs69HoldsPlacedByApiOnT3", "TestFs92HoldsPlacedByApiOnT3"},
		".": {
			"TestFs74OnT3", "TestFs75OnT3", "TestFs83OnT3", "TestFs87IndexOnT3", "TestFs99CardsOnT3", "TestFs107OnT3", "TestFs108OnT3", "TestFs111OnT3",
			"TestFileClusterSuitePassesOnT3",
		},
	} {
		for _, name := range names {
			if !slices.Contains(named[packagePath], name) {
				t.Errorf("no row of M10 names %s in %s", name, packagePath)
			}
		}
	}
}

// Every server test M10's items name is declared under its tags: the rules' and the store clock's here, the leak leg,
// the CROSSSLOT lints and the inline paths, D-TEST-13's projection and the benchmark rows in cli/filecluster-bench,
// D-TEST-12's and D-TEST-18's soak counts in taskworker's work and sidecarctl's report, the format2 gate in evshost and
// the checked-in config in the root package, FS-72's migration and rollback runs, the git-lfs matrix, and the cards
// ref's commits on the local stack.
func TestGateM10ItemsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	for _, name := range gateItemServerTestsUndeclared(t, fault.GateRows(catalog, "M10")) {
		t.Errorf("an item of M10 names %s, which is not declared so", name)
	}
	named := gateItemServerTestNames("M10")
	for packagePath, names := range map[string][]string{
		"./sidecart3":             {"TestGateT3RunsTheNightlySeeds", "TestT3StoreClockReadsAgesOnTheScheduleClock", "TestT3EveryStoreClassCatchesItsLeak", "TestCrossslotKeysSpreadOverTheMasters", "TestInlinePathFailsAPlantedPostgresRead"},
		"./cli/filecluster-bench": {"TestProjectionOnT3CountsThatFitPasses", "TestProjectionReadsOpenWithoutM2sChoice", "TestDropFiguresComputeTheHistorysJobTime", "TestHddM10RunsTheMixWithPurgesBesideIt", "TestIndexModeRunsTheSearchNotesLoad"},
		"./taskworker/work":       {"TestEvsObserveSoaksMet", "TestEvsObserveDropSoakSharesJobsWithBothARenameAndACopy"},
		"./sidecarctl":            {"TestEvsObserveReportStatesEachUnmetThresholdAndExitsZero"},
		"./evshost":               {"TestHostFormat2GateReadsServersSidecarYml", "TestHostFormat2GateWithoutGatesIsClosed", "TestFs72RollbackOnThePinnedBuildOnT3", "TestLfsConformanceAcrossTheMatrix"},
		".":                       {"TestSidecarConfigFilesAreDark", "TestFs86CardCommitsAtTheTeamLimitOnT3"},
	} {
		for _, name := range names {
			if !slices.Contains(named[packagePath], name) {
				t.Errorf("no item of M10 names %s in %s", name, packagePath)
			}
		}
	}
}

// Whether each function of a package's files, test files among them, runs in server's test env: its body names
// server's TestEnv or DefaultTestEnv, or calls a function of the package that returns a *TestEnv or does either, to a
// fixed point. Build tags are not read, so a function declared under two tags counts if either body does.
func gateTestEnvFunctions(t *testing.T, directory string) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	// server's own names for the env, qualified outside its root package and bare inside it
	isEnvName := func(name string) bool {
		return name == "TestEnv" || name == "DefaultTestEnv"
	}
	fileSet := token.NewFileSet()
	calls := map[string][]string{}
	users := map[string]bool{}
	for _, path := range paths {
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		inServer := file.Name.Name == "server" || file.Name.Name == "server_test"
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil {
				continue
			}
			name := function.Name.Name
			// a constructor of the env: its results name *TestEnv
			if function.Type.Results != nil {
				for _, result := range function.Type.Results.List {
					star, ok := result.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					switch named := star.X.(type) {
					case *ast.SelectorExpr:
						if base, ok := named.X.(*ast.Ident); ok && base.Name == "server" && named.Sel.Name == "TestEnv" {
							users[name] = true
						}
					case *ast.Ident:
						if inServer && named.Name == "TestEnv" {
							users[name] = true
						}
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.SelectorExpr:
					if base, ok := node.X.(*ast.Ident); ok && base.Name == "server" && isEnvName(node.Sel.Name) {
						users[name] = true
					}
				case *ast.Ident:
					if inServer && isEnvName(node.Name) {
						users[name] = true
					}
				case *ast.CallExpr:
					if callee, ok := node.Fun.(*ast.Ident); ok {
						calls[name] = append(calls[name], callee.Name)
					}
				}
				return true
			})
		}
	}
	for changed := true; changed; {
		changed = false
		for name, callees := range calls {
			if users[name] {
				continue
			}
			if slices.ContainsFunc(callees, func(callee string) bool { return users[callee] }) {
				users[name] = true
				changed = true
			}
		}
	}
	return users
}

// Every server test a gate names off the private stores runs outside server's test env, whose preflight fails a run
// with no WARP_ENV: a test that needs the env, named without stores, reads fail at every gate run whatever it would
// show (as M2's and M10's CROSSSLOT items once named TestRunCrossslotRefusesOneNode). A fixture entry naming that
// test without stores is caught.
func TestGateServerTestsInTheTestEnvRunOnThePrivateStores(t *testing.T) {
	catalog := fault.DefaultCatalog()
	byDirectory := map[string]map[string]bool{}
	usesEnv := func(packagePath string, name string) bool {
		directory := filepath.Join("..", filepath.FromSlash(packagePath))
		if _, ok := byDirectory[directory]; !ok {
			byDirectory[directory] = gateTestEnvFunctions(t, directory)
		}
		return byDirectory[directory][name]
	}
	offStores := func(rows []*fault.GateRow) []string {
		found := []string{}
		for _, row := range rows {
			for _, test := range row.Tests {
				if test.Repo != "server" || test.Stores {
					continue
				}
				for _, name := range test.Names {
					if usesEnv(filepath.Join(test.Dir, test.Package), name) {
						found = append(found, fmt.Sprintf("%s in %s", name, test.Package))
					}
				}
			}
		}
		return found
	}
	for _, gate := range fault.GateNames() {
		for _, found := range offStores(fault.GateRows(catalog, gate)) {
			t.Errorf("%s names %s off the private stores, and it runs in server's test env", gate, found)
		}
	}
	fixture := []*fault.GateRow{{Item: "a fixture", Tests: []*fault.GateTest{
		{Repo: "server", Package: "./sidecart3", Names: []string{"TestRunCrossslotRefusesOneNode", "TestCrossslotKeysSpreadOverTheMasters"}},
		{Repo: "server", Package: "./sidecart3", Stores: true, Names: []string{"TestRunCrossslotRefusesOneNode"}},
	}}}
	if got := offStores(fixture); !slices.Equal(got, []string{"TestRunCrossslotRefusesOneNode in ./sidecart3"}) {
		t.Errorf("the fixture reads %v", got)
	}
}
