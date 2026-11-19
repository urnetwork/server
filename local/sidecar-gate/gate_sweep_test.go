package main

// The server tests a gate's sweep names for an FS case in place of the convention's sidecart3 test (owner decision
// 548; QD-1081): FS-54's and FS-55's OPS tests, the T3 tests in evshost, FS-5's, FS-73's, FS-72's rollback case's
// and M2's (gate_m2_out.go), M3's restores, FS-25's api legs and FS-50's monitor tests (gate_m3.go), and M4's bridge
// rows in controller and FS-37's clauses in taskworker/work (gate_m4.go); and the convention's own sidecart3 entries
// wherever their package declares the test (QD-1085). A row whose test is renamed, or whose file's build tags drift
// from the entry's, reads missing at the gate, never pass. This test fails first: each test must be declared in its
// package, in a file that go test builds on this host with the entry's tags. A convention entry whose test nobody has
// written yet is left out, since it reads missing by design.

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestGateSweepsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	gates := &gatesFile{}
	if err := readYamlFile("gates.yml", gates); err != nil {
		t.Fatal(err)
	}
	serverDirectory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	catalog := fault.DefaultCatalog()

	// each top-level function of a directory's test files, with the files that declare it
	declaringFiles := func(directory string) map[string][]string {
		paths, err := filepath.Glob(filepath.Join(directory, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		nameFiles := map[string][]string{}
		fileSet := token.NewFileSet()
		for _, path := range paths {
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			for _, declaration := range file.Decls {
				if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
					nameFiles[function.Name.Name] = append(nameFiles[function.Name.Name], filepath.Base(path))
				}
			}
		}
		return nameFiles
	}

	checked := []string{}
	for _, gate := range fault.GateNames() {
		for _, row := range gates.Gates[gate] {
			if row.Row == "" {
				continue
			}
			testCase := catalog.Case(row.Row, row.Case)
			if testCase == nil {
				t.Errorf("%s holds %s %q, which the catalog does not", gate, row.Row, row.Case)
				continue
			}
			for _, test := range row.Tests {
				if test.Repo != "server" {
					continue
				}
				// the convention's name in its package, whatever the tags: a test not written yet reads missing
				conventional := test.Package == "./sidecart3" && slices.Equal(test.Names, []string{fault.GateT3TestName(testCase)})
				directory := filepath.Join(serverDirectory, test.Dir, filepath.FromSlash(test.Package))
				buildContext := build.Default
				if test.Tags != "" {
					buildContext.BuildTags = strings.Split(test.Tags, ",")
				}
				nameFiles := declaringFiles(directory)
				for _, name := range test.Names {
					files := nameFiles[name]
					if len(files) == 0 && conventional {
						continue
					}
					if len(files) == 0 {
						t.Errorf("%s's %s names server/%s's %s, which no test file there declares", gate, testCase, test.Package, name)
						continue
					}
					built := slices.ContainsFunc(files, func(file string) bool {
						match, err := buildContext.MatchFile(directory, file)
						return err == nil && match
					})
					if !built {
						t.Errorf("%s's %s names server/%s's %s, declared in %v, which go test does not build here with tags %q", gate, testCase, test.Package, name, files, test.Tags)
					}
					checked = append(checked, gate+" "+testCase.String()+" "+test.Package+" "+name)
				}
			}
		}
	}
	for _, want := range []string{
		"M1 FS-54 . TestSidecarPendingBuildTags",
		"M2 FS-1 ./evshost TestFs1OnT3",
		"M2 FS-5 ./evshost TestFs5OnT3",
		"M2 FS-10 ./evshost TestFs10TwinOnT3",
		"M2 FS-10 ./evshost TestFs10RollbackTwinOnT3",
		"M2 FS-113 ./evshost TestFs113OnT3",
		"M2 FS-14 ./sidecart3 TestFs14OnT3",
		"M2 FS-19's config-read ./sidecart3 TestFs19ConfigReadOnT3",
		"M2 FS-124's forged ./sidecart3 TestFs124ForgedOnT3",
		"M3 FS-25 ./api TestFs25GateResponseLostAfterCommit",
		"M3 FS-41 ./sidecarrestore TestFs41SiteLossThroughTheCloseOnT3",
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
		"every FS-55 ./local/fs55 TestFs55OnServerBinaries",
	} {
		if !slices.Contains(checked, want) {
			t.Errorf("%q was not checked; checked %v", want, checked)
		}
	}
}
