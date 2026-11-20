package sidecart3

// M6's sweeps on server's side (TEST-M6-02; sidecar's gate_m6.go): the tests an item names in server are declared
// where the item says, in a file that builds under the item's tags. sidecar-gate's own
// TestGateSweepsNameServerTestsDeclaredUnderTheirTags holds a row's tests so and passes an item's over, and an item
// reads no tier from the catalog, so nothing else says a renamed scenario, revocation suite or benchmark test has left
// its item running nothing. Reading test files opens no store, so this runs at every merge.

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

var gateItemTestDeclarationPattern = regexp.MustCompile(`(?m)^func ((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]*)\(`)

// The tests rows's items name in server that no test file of their package declares in a file building under the
// item's tags, each as "<name> in <package> under <tags>".
func gateItemServerTestsUndeclared(t *testing.T, rows []*fault.GateRow) []string {
	t.Helper()
	declared := map[string]map[string][]string{}
	filesOf := func(directory string) map[string][]string {
		if nameFiles, ok := declared[directory]; ok {
			return nameFiles
		}
		paths, err := filepath.Glob(filepath.Join(directory, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		nameFiles := map[string][]string{}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range gateItemTestDeclarationPattern.FindAllSubmatch(data, -1) {
				nameFiles[string(match[1])] = append(nameFiles[string(match[1])], filepath.Base(path))
			}
		}
		declared[directory] = nameFiles
		return nameFiles
	}
	undeclared := []string{}
	for _, row := range rows {
		if row.Item == "" {
			continue
		}
		for _, test := range row.Tests {
			if test.Repo != "server" {
				continue
			}
			directory := filepath.Join("..", test.Dir, filepath.FromSlash(test.Package))
			buildContext := build.Default
			if test.Tags != "" {
				buildContext.BuildTags = strings.Split(test.Tags, ",")
			}
			for _, name := range test.Names {
				builds := false
				for _, fileName := range filesOf(directory)[name] {
					if match, err := buildContext.MatchFile(directory, fileName); err != nil {
						t.Fatal(err)
					} else if match {
						builds = true
					}
				}
				if !builds {
					undeclared = append(undeclared, fmt.Sprintf("%s in %s under %q", name, test.Package, test.Tags))
				}
			}
		}
	}
	return undeclared
}

// Every server test an item of M6's entry or exit names is declared under its tags: the benchmark's measurements in
// model, S3b's and S4b's T3 runs and the revocation suite's member form in this package, the member form on server's
// sequencer in api, Team billing in quotahost, and what is written of FS-43, FS-51 and the directory rows' T3 legs. A
// name nothing declares, and one declared only behind a tag its item lacks, are both caught.
func TestGateM6ItemsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	named := []string{}
	for _, gate := range []string{"M6-in", "M6"} {
		rows := fault.GateRows(catalog, gate)
		for _, name := range gateItemServerTestsUndeclared(t, rows) {
			t.Errorf("an item of %s names %s, which is not declared so", gate, name)
		}
		for _, row := range rows {
			if row.Item == "" {
				continue
			}
			for _, test := range row.Tests {
				if test.Repo == "server" {
					named = append(named, test.Names...)
				}
			}
		}
	}
	for _, name := range []string{
		"TestEscrowBenchmarkMeasurements", "TestScenarioS3bOnT3", "TestScenarioS4bOnT3", "TestRevocationSuiteMemberFormOnT3",
		"TestRevocationSuiteMemberFormOnServersSequencer", "TestTeamBillingFs46T3", "TestFs43StripeMeters", "TestFs99EvsScopeMutantExchangeMintingXAdminFailsOnT3",
	} {
		if !slices.Contains(named, name) {
			t.Errorf("no item of M6's gates names %s", name)
		}
	}
	wrong := []*fault.GateRow{{Item: "a fixture", Tests: []*fault.GateTest{
		{Repo: "server", Package: "./sidecart3", Names: []string{"TestGateNoSuchTestIsDeclaredAnywhere"}},
		// declared in revocation_suite_team_test.go, which builds under the local stack's tag alone
		{Repo: "server", Package: "./sidecart3", Names: []string{"TestRevocationSuiteMemberFormOnT3"}},
		{Repo: "server", Package: "./sidecart3", Tags: "sidecar_localstack", Names: []string{"TestRevocationSuiteMemberFormOnT3"}},
	}}}
	want := []string{
		`TestGateNoSuchTestIsDeclaredAnywhere in ./sidecart3 under ""`,
		`TestRevocationSuiteMemberFormOnT3 in ./sidecart3 under ""`,
	}
	if got := gateItemServerTestsUndeclared(t, wrong); !slices.Equal(got, want) {
		t.Errorf("the fixture's undeclared tests read %q, want %q", got, want)
	}
}
