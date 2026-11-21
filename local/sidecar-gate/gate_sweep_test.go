package main

// gates.yml's server tests held to their packages' files (owner decision 548; QD-1081, QD-1085). A gate's sweep names
// an FS case's server tests in place of the convention's sidecart3 test where they run elsewhere: FS-54's and FS-55's
// OPS tests, the T3 tests in evshost, FS-5's, FS-73's, FS-72's rollback case's and M2's (gate_m2_out.go), M3's
// restores, FS-25's api legs and FS-50's monitor tests (gate_m3.go), M4's bridge rows in controller and FS-37's clauses
// in taskworker/work (gate_m4.go), and M10's T3 runs in evshost, model and the root package, FS-114's purge and drop
// cases by the file-cluster suite among them (gate_m10_out.go). The convention names every other case's T3 test in
// sidecart3 under its tags, the local stack's among them, since every sidecart3 FS test file is behind it (QD-1085).
//
// A row whose test is renamed, or whose file's build tags drift from the entry's, reads missing at the gate, never
// pass, while -list, which reads no tags, reads it listed. checkGateServerTestsDeclared fails first, on every entry whose
// package declares its test, the convention's included, and on every server test an item names, at every gate: each
// test must be declared in its package, in a file that go test builds on this host with the entry's tags. A convention
// entry whose test nobody has written yet is left out, since it reads missing by design. Fixtures show the check
// failing a convention test behind a tag the convention lacks and an item's test behind a tag its entry lacks, and a
// real run of the convention's entries on a stand-in for sidecart3 shows each test built in under their tags.

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// The convention's T3 tags before QD-1085, under which a real run built every sidecart3 FS test out.
const gateConventionT3TagsBeforeLocalStack = "sidecar_testhooks,sidecar_pending"

// Each top-level function the test files in directory declare, with the files that declare it. A directory that does
// not exist declares nothing.
func gateTestDeclaringFiles(directory string) (map[string][]string, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "*_test.go"))
	if err != nil {
		return nil, err
	}
	nameFiles := map[string][]string{}
	fileSet := token.NewFileSet()
	for _, path := range paths {
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
				nameFiles[function.Name.Name] = append(nameFiles[function.Name.Name], filepath.Base(path))
			}
		}
	}
	return nameFiles, nil
}

// Whether go test builds any of files, in directory, on this host with tags, a comma-separated list as gates.yml writes
// it.
func gateTestFilesBuild(directory string, files []string, tags string) bool {
	buildContext := build.Default
	if tags != "" {
		buildContext.BuildTags = strings.Split(tags, ",")
	}
	return slices.ContainsFunc(files, func(file string) bool {
		match, err := buildContext.MatchFile(directory, file)
		return err == nil && match
	})
}

// Holds each server test gates names, for an FS case or in an item, to the test files under serverDirectory: each must
// be declared in its package, in a file go test builds on this host with the entry's tags. A convention entry, the
// case's TestFs<n><Case>OnT3 in ./sidecart3 under whatever tags, whose test nobody has written yet is left out, since it
// reads missing by design; once its package declares the test, it is held like any other. An item names its tests
// outright, so none of them is left out. Returns each entry checked, as "<gate> <case> <package> <name>", an item's as
// "<gate> item <package> <name>", and each fault found; the error is a test file it could not read.
func checkGateServerTestsDeclared(gates *gatesFile, catalog *fault.Catalog, serverDirectory string) ([]string, []string, error) {
	directoryNameFiles := map[string]map[string][]string{}
	checked := []string{}
	faults := []string{}
	for _, gate := range fault.GateNames() {
		for _, row := range gates.Gates[gate] {
			// what checked names the row by, what a fault names it by, and its case's convention T3 test
			checkedLabel := "item"
			faultLabel := fmt.Sprintf("item %q", row.Item)
			conventionName := ""
			if row.Row != "" {
				testCase := catalog.Case(row.Row, row.Case)
				if testCase == nil {
					faults = append(faults, fmt.Sprintf("%s holds %s %q, which the catalog does not", gate, row.Row, row.Case))
					continue
				}
				checkedLabel = testCase.String()
				faultLabel = testCase.String()
				conventionName = fault.GateT3TestName(testCase)
			}
			for _, test := range row.Tests {
				if test.Repo != "server" {
					continue
				}
				// the convention's name in its package, whatever the tags: a test not written yet reads missing
				conventional := conventionName != "" && test.Package == "./sidecart3" && slices.Equal(test.Names, []string{conventionName})
				directory := filepath.Join(serverDirectory, test.Dir, filepath.FromSlash(test.Package))
				nameFiles, ok := directoryNameFiles[directory]
				if !ok {
					var err error
					if nameFiles, err = gateTestDeclaringFiles(directory); err != nil {
						return nil, nil, err
					}
					directoryNameFiles[directory] = nameFiles
				}
				for _, name := range test.Names {
					files := nameFiles[name]
					if len(files) == 0 && conventional {
						continue
					}
					if len(files) == 0 {
						faults = append(faults, fmt.Sprintf("%s's %s names server/%s's %s, which no test file there declares", gate, faultLabel, test.Package, name))
						continue
					}
					if !gateTestFilesBuild(directory, files, test.Tags) {
						faults = append(faults, fmt.Sprintf("%s's %s names server/%s's %s, declared in %v, which go test does not build here with tags %q", gate, faultLabel, test.Package, name, files, test.Tags))
					}
					checked = append(checked, gate+" "+checkedLabel+" "+test.Package+" "+name)
				}
			}
		}
	}
	return checked, faults, nil
}

func TestGateSweepsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	gates := &gatesFile{}
	if err := readYamlFile("gates.yml", gates); err != nil {
		t.Fatal(err)
	}
	serverDirectory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	checked, faults, err := checkGateServerTestsDeclared(gates, fault.DefaultCatalog(), serverDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, found := range faults {
		t.Error(found)
	}
	for _, want := range []string{
		"M1 FS-54 . TestSidecarPendingBuildTags",
		"M2 FS-1 ./evshost TestFs1OnT3",
		"M2 FS-5 ./evshost TestFs5OnT3",
		"M2 FS-10 ./evshost TestFs10TwinOnT3",
		"M2 FS-10 ./evshost TestFs10RollbackRefusedOnT3",
		"M2 FS-113 ./evshost TestFs113OnT3",
		"M2 FS-44 ./quotahost TestFs44T3Producer",
		"M2 FS-45 ./quotahost TestFs45T3RelayedPairs",
		"M2 FS-47 ./quotahost TestFs47T3QuotaClauses",
		"M2 FS-53 ./evshost TestFs53OnT3",
		"M2 FS-60 ./evshost TestFs60OnT3",
		"M2 FS-60 ./evshost TestFs60PendingRecordFencedThenDroppedOnT3",
		"M2 FS-14 ./sidecart3 TestFs14OnT3",
		"M2 FS-19's config-read ./sidecart3 TestFs19ConfigReadOnT3",
		"M2 FS-124's forged ./sidecart3 TestFs124ForgedOnT3",
		"M3 FS-25 ./api TestFs25GateResponseLostAfterCommit",
		"M3 FS-41 ./sidecarrestore TestFs41SiteLossThroughTheCloseOnT3",
		"M3 FS-41 . TestSiteCloseWithRestrictedUsersAcrossMetadataClusters",
		"M3 FS-41 . TestFileClusterReseedStorageLossesOnT3",
		"M3 FS-42 ./sidecarrestore TestFs42MainDumpOlder",
		"M3 FS-50 ./monitor TestPageClassAt0200PagesTheSecondPersonAt30MinutesNotBefore",
		"M3 FS-98's cli ./sidecart3 TestFs98CliOnT3",
		"M4 FS-19's grant ./sidecart3 TestFs19GrantOnT3",
		"M4 FS-21 ./sidecart3 TestFs21OnT3",
		"M4 FS-37 ./taskworker/work TestFs37PageKilledWithNoUnloadReapsOnceOnT3",
		"M4 FS-37 ./taskworker/work TestFs37ReapJobRunsTheReapOnT3",
		"M4 FS-38 ./controller TestFs38OnT3",
		"M4 FS-38's forged ./controller TestFs38ForgedOnT3",
		"M4 FS-56 ./controller TestFs56OnT3",
		"M10 FS-73 ./evshost TestFs73OnT3",
		"M10 FS-72's rollback ./evshost TestFs72RollbackOnT3",
		"M10 FS-72's migration ./evshost TestFs72MigrationOnT3",
		"M10 FS-68 ./evshost TestFs68OnT3",
		"M10 FS-89 ./evshost TestFs89OnT3",
		"M10 FS-95 ./evshost TestFs95OnT3",
		"M10 FS-66's hold ./model TestFs66HoldPlacedByApiOnT3",
		"M10 FS-69 ./model TestFs69HoldsPlacedByApiOnT3",
		"M10 FS-92 ./model TestFs92HoldsPlacedByApiOnT3",
		"M10 FS-74 . TestFs74OnT3",
		"M10 FS-75 . TestFs75OnT3",
		"M10 FS-83 . TestFs83OnT3",
		"M10 FS-87's index . TestFs87IndexOnT3",
		"M10 FS-111 . TestFs111OnT3",
		"M10 FS-114's purge . TestFileClusterSuitePassesOnT3",
		"M10 FS-114's drop . TestFileClusterSuitePassesOnT3",
		"every FS-55 ./local/fs55 TestFs55OnServerBinaries",
		// items, which no sidecart3 gate test holds before M4's
		"M2 item ./sidecart3 TestT3StoreClockReadsAgesOnTheScheduleClock",
		"M2 item ./sidecart3 TestFs1HistoriesLinearizeOnT3",
		"M2 item ./api TestAdmissionSignersOnT3",
		"M3 item ./sidecart3 TestFi12RunsAfterEveryStepOnT3",
		"next item . TestFileClustersConformOnTheLocalStack",
		"next item ./sidecart3 TestT3LayoutTakesRequestsOnAFakeClock",
	} {
		if !slices.Contains(checked, want) {
			t.Errorf("%q was not checked; checked %v", want, checked)
		}
	}
}

// The check fails a convention entry whose test sits behind a tag the entry lacks, the mismatch QD-1085 fixed. A
// fixture server holds FS-14's T3 test in sidecart3 behind the local stack, as the real file has it, and M2's row names
// it by convention. Under the convention's tags from before QD-1085, or with the test behind a tag today's convention
// lacks, the check finds it built out; under tags that build it, the check holds it and finds nothing. FS-16's
// convention test, which the fixture never wrote, is left out either way, while a sweep's test no file declares fails.
func TestGateSweepCheckFailsAConventionTestBehindATagTheConventionLacks(t *testing.T) {
	catalog := fault.DefaultCatalog()
	for _, c := range []struct {
		buildLine string
		tags      string
		builtOut  bool
	}{
		{buildLine: "//go:build sidecar_localstack", tags: gateConventionT3TagsBeforeLocalStack, builtOut: true},
		{buildLine: "//go:build sidecar_localstack && sidecar_disruption", tags: gateConventionT3Tags, builtOut: true},
		{buildLine: "//go:build sidecar_localstack", tags: gateConventionT3Tags, builtOut: false},
		{buildLine: "//go:build sidecar_localstack && sidecar_testhooks && sidecar_pending", tags: gateConventionT3Tags, builtOut: false},
	} {
		serverDirectory := t.TempDir()
		directory := filepath.Join(serverDirectory, "sidecart3")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		content := c.buildLine + "\n\npackage sidecart3\n\nimport \"testing\"\n\nfunc TestFs14OnT3(t *testing.T) {\n}\n"
		if err := os.WriteFile(filepath.Join(directory, "fs_team_test.go"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		gatesPath := filepath.Join(serverDirectory, "gates.yml")
		gatesText := "gates:\n  M2:\n" +
			"    - row: FS-14\n      tests:\n        - {repo: server, package: ./sidecart3, tags: '" + c.tags + "', stores: true, names: [TestFs14OnT3]}\n" +
			"    - row: FS-16\n      tests:\n        - {repo: server, package: ./sidecart3, tags: '" + c.tags + "', stores: true, names: [TestFs16OnT3]}\n"
		if err := os.WriteFile(gatesPath, []byte(gatesText), 0o600); err != nil {
			t.Fatal(err)
		}
		gates := &gatesFile{}
		if err := readYamlFile(gatesPath, gates); err != nil {
			t.Fatal(err)
		}
		checked, faults, err := checkGateServerTestsDeclared(gates, catalog, serverDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(checked, []string{"M2 FS-14 ./sidecart3 TestFs14OnT3"}) {
			t.Errorf("%q under tags %q: checked %v, want FS-14's test alone", c.buildLine, c.tags, checked)
		}
		builtOut := len(faults) == 1 && strings.Contains(faults[0], "TestFs14OnT3, declared in [fs_team_test.go], which go test does not build here with tags "+fmt.Sprintf("%q", c.tags))
		if c.builtOut != builtOut || (!c.builtOut && 0 < len(faults)) {
			t.Errorf("%q under tags %q: found %q, built out %t", c.buildLine, c.tags, faults, c.builtOut)
		}
	}

	// only a convention entry may name a test nobody wrote: a sweep's test that no file declares fails
	gates := &gatesFile{Gates: map[string][]gateRow{"M2": {{
		Row:   "FS-1",
		Tests: []gateTest{{Repo: "server", Package: "./evshost", Tags: gateConventionT3Tags, Stores: true, Names: []string{"TestFs1OnT3"}}},
	}}}}
	checked, faults, err := checkGateServerTestsDeclared(gates, catalog, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(checked) != 0 || len(faults) != 1 || !strings.Contains(faults[0], "TestFs1OnT3, which no test file there declares") {
		t.Errorf("a sweep's test no file declares: checked %v, found %q", checked, faults)
	}
}

// The check holds an item's server tests as it holds a row's, with nothing left out, since an item names its tests
// outright. A fixture server holds the store clock's T3 test in sidecart3 behind the local stack, as the real file has
// it, and an M2 item names it beside a test nobody wrote. Under the convention's tags from before QD-1085 the check
// finds the written test built out, under today's it finds that test clean, and the unwritten one fails either way.
func TestGateSweepCheckHoldsAnItemsServerTestsToTheirTags(t *testing.T) {
	catalog := fault.DefaultCatalog()
	for _, c := range []struct {
		tags     string
		builtOut bool
	}{
		{tags: gateConventionT3TagsBeforeLocalStack, builtOut: true},
		{tags: gateConventionT3Tags, builtOut: false},
	} {
		serverDirectory := t.TempDir()
		directory := filepath.Join(serverDirectory, "sidecart3")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		content := "//go:build sidecar_localstack\n\npackage sidecart3\n\nimport \"testing\"\n\nfunc TestT3StoreClockReadsAgesOnTheScheduleClock(t *testing.T) {\n}\n"
		if err := os.WriteFile(filepath.Join(directory, "clock_t3_test.go"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		gates := &gatesFile{Gates: map[string][]gateRow{"M2": {{
			Item:  "T3's store clock",
			Tests: []gateTest{{Repo: "server", Package: "./sidecart3", Tags: c.tags, Stores: true, Names: []string{"TestT3StoreClockReadsAgesOnTheScheduleClock", "TestT3StoreClockNobodyWrote"}}},
		}}}}
		checked, faults, err := checkGateServerTestsDeclared(gates, catalog, serverDirectory)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(checked, []string{"M2 item ./sidecart3 TestT3StoreClockReadsAgesOnTheScheduleClock"}) {
			t.Errorf("under tags %q: checked %v, want the written test alone", c.tags, checked)
		}
		wantFaults := []string{`M2's item "T3's store clock" names server/./sidecart3's TestT3StoreClockNobodyWrote, which no test file there declares`}
		if c.builtOut {
			wantFaults = append([]string{fmt.Sprintf(`M2's item "T3's store clock" names server/./sidecart3's TestT3StoreClockReadsAgesOnTheScheduleClock, declared in [clock_t3_test.go], which go test does not build here with tags %q`, c.tags)}, wantFaults...)
		}
		if !slices.Equal(faults, wantFaults) {
			t.Errorf("under tags %q: found %q, want %q", c.tags, faults, wantFaults)
		}
	}
}

// A sidecart3 case reads declared under a real run's tags (QD-1085): the gate runs the convention's T3 entries as
// gates.yml writes them, tags and stores, and each test runs, never "declared, but not in this build". The run is on a
// stand-in for server's sidecart3, whose tests need the private stores: each real file declaring a convention test
// gates.yml names gives the stand-in a file of its name, with its build line, declaring those tests, which pass. Under
// the convention's tags from before QD-1085 the run reads each test as checkGateServerTestsDeclared's reading of its
// real file says, FS-14's built out among them, as every real run read it then.
func TestGateRunBuildsTheConventionsSidecart3TestsUnderTheirTags(t *testing.T) {
	gates := &gatesFile{}
	if err := readYamlFile("gates.yml", gates); err != nil {
		t.Fatal(err)
	}
	catalog := fault.DefaultCatalog()
	realDirectory, err := filepath.Abs(filepath.Join("..", "..", "sidecart3"))
	if err != nil {
		t.Fatal(err)
	}
	nameFiles, err := gateTestDeclaringFiles(realDirectory)
	if err != nil {
		t.Fatal(err)
	}
	// the convention's sidecart3 tests that gates.yml names and sidecart3 declares, by their entries' tags and by the
	// files that declare them
	tagsNames := map[string][]string{}
	fileNames := map[string][]string{}
	for _, gate := range fault.GateNames() {
		for _, row := range gates.Gates[gate] {
			if row.Row == "" {
				continue
			}
			// a case the catalog does not hold is TestGateSweepsNameServerTestsDeclaredUnderTheirTags's to report
			testCase := catalog.Case(row.Row, row.Case)
			if testCase == nil {
				continue
			}
			name := fault.GateT3TestName(testCase)
			for _, test := range row.Tests {
				if test.Repo != "server" || test.Package != "./sidecart3" || !slices.Equal(test.Names, []string{name}) || len(nameFiles[name]) == 0 {
					continue
				}
				if !slices.Contains(tagsNames[test.Tags], name) {
					tagsNames[test.Tags] = append(tagsNames[test.Tags], name)
				}
				for _, file := range nameFiles[name] {
					if !slices.Contains(fileNames[file], name) {
						fileNames[file] = append(fileNames[file], name)
					}
				}
			}
		}
	}
	if len(tagsNames) == 0 {
		t.Fatal("gates.yml names no convention test that sidecart3 declares")
	}

	options := writeGateFixture(t, "gates: {}\n", "ids: {}\n")
	directory := filepath.Join(options.root, "fixture", "sidecart3")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// a file that is no test, as sidecart3 has, so tags that build every test file out still build the package
	if err := os.WriteFile(filepath.Join(directory, "sidecart3.go"), []byte("// Stands in for server's sidecart3.\npackage sidecart3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for file, names := range fileNames {
		data, err := os.ReadFile(filepath.Join(realDirectory, file))
		if err != nil {
			t.Fatal(err)
		}
		buildLine := ""
		for _, line := range strings.Split(string(data), "\n") {
			if constraint.IsGoBuild(line) {
				buildLine = line
				break
			}
			if strings.HasPrefix(line, "package ") {
				break
			}
		}
		var content strings.Builder
		content.WriteString(buildLine + "\n\npackage sidecart3\n\nimport \"testing\"\n")
		for _, name := range names {
			content.WriteString("\nfunc " + name + "(t *testing.T) {\n}\n")
		}
		if err := os.WriteFile(filepath.Join(directory, file), []byte(content.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// the private-store wrapper's contract, the directory and then go test's arguments, with no stores behind it
	wrapper := filepath.Join(options.root, "stores.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ncd \"$1\" || exit 1\nshift\nexec go test \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	options.environ = append(options.environ, gateStoreWrapperVariable+"="+wrapper)

	for _, tags := range slices.Sorted(maps.Keys(tagsNames)) {
		names := tagsNames[tags]
		outcome, details := runGateTest(context.Background(), options, &gateReport{}, gateTest{Repo: "fixture", Package: "./sidecart3", Tags: tags, Stores: true, Names: names})
		joined := strings.Join(details, "\n")
		if outcome != gatePass {
			t.Errorf("the convention's entries under tags %q read %s:\n%s", tags, outcome, joined)
		}
		for _, name := range names {
			if !strings.Contains(joined, "fixture/sidecart3 "+name+": pass") {
				t.Errorf("%s under tags %q did not run and pass:\n%s", name, tags, joined)
			}
		}
	}
	names := tagsNames[gateConventionT3Tags]
	if !slices.Contains(names, "TestFs14OnT3") {
		t.Errorf("no entry under the convention's tags %q names FS-14's sidecart3 test, which is written: %v", gateConventionT3Tags, tagsNames)
	}

	_, details := runGateTest(context.Background(), options, &gateReport{}, gateTest{Repo: "fixture", Package: "./sidecart3", Tags: gateConventionT3TagsBeforeLocalStack, Stores: true, Names: names})
	joined := strings.Join(details, "\n")
	for _, name := range names {
		want := "fixture/sidecart3 " + name + ": pass"
		if !gateTestFilesBuild(realDirectory, nameFiles[name], gateConventionT3TagsBeforeLocalStack) {
			want = "fixture/sidecart3 " + name + ": declared, but not in this build"
		}
		if !strings.Contains(joined, want) {
			t.Errorf("under the tags before QD-1085 the run lacks %q:\n%s", want, joined)
		}
	}
	if gateTestFilesBuild(realDirectory, nameFiles["TestFs14OnT3"], gateConventionT3TagsBeforeLocalStack) {
		t.Errorf("FS-14's sidecart3 test, in %v, builds under the convention's tags from before QD-1085, so it no longer needs the local stack, which the convention carries for it", nameFiles["TestFs14OnT3"])
	}
}
