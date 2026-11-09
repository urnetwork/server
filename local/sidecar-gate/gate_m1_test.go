package main

// M1's gate (TEST-M1-11): gates.yml is what sidecar's catalog and sweeps generate, the tool reads it whole,
// a row's commands run, an entry review runs no Gn-out rows, -list resolves without running, and M1's sweep
// lists on this tree.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
	"gopkg.in/yaml.v3"
)

// gates.yml is GateFile's output byte for byte, so catalog.yml stays the one source (C-18). With
// SIDECAR_REGENERATE_GATES=1 it writes the file instead.
func TestGatesYmlIsTheCatalogsGateMap(t *testing.T) {
	generated, err := fault.GateFile(fault.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SIDECAR_REGENERATE_GATES") == "1" {
		if err := os.WriteFile("gates.yml", generated, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote gates.yml: %d bytes", len(generated))
		return
	}
	committed, err := os.ReadFile("gates.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, generated) {
		committedLines := strings.Split(string(committed), "\n")
		generatedLines := strings.Split(string(generated), "\n")
		line := 0
		for line < len(committedLines) && line < len(generatedLines) && committedLines[line] == generatedLines[line] {
			line += 1
		}
		t.Fatalf("gates.yml is not what catalog.yml and the sweeps generate, from line %d; run SIDECAR_REGENERATE_GATES=1 go test ./local/sidecar-gate -run '^TestGatesYmlIsTheCatalogsGateMap$'", line+1)
	}
}

// Every field gates.yml holds is one the tool reads, so no row carries a value no report shows.
func TestGatesYmlHoldsOnlyWhatTheToolReads(t *testing.T) {
	data, err := os.ReadFile("gates.yml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	gates := &gatesFile{}
	if err := decoder.Decode(gates); err != nil {
		t.Fatal(err)
	}
	for _, gate := range fault.GateNames() {
		if _, ok := gates.Gates[gate]; !ok {
			t.Errorf("gates.yml has no gate %s", gate)
		}
	}
}

// A row's commands run beside its tests: exit 0 passes and anything else fails the row.
func TestGateRowRunsItsCommands(t *testing.T) {
	gates := `gates:
  M1:
    - item: "a check that passes"
      commands:
        - {repo: fixture, command: [./check.sh, pass]}
    - item: "a check that fails"
      commands:
        - {repo: fixture, command: [./check.sh, fail]}
      tests:
        - {repo: fixture, package: ./widget, names: [TestPasses]}
`
	options := writeGateFixture(t, gates, "ids: {}\n")
	if err := os.WriteFile(filepath.Join(options.root, "fixture", "check.sh"), []byte("#!/bin/sh\n[ \"$1\" = pass ]\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := gateOutcomes(report.rows)
	if outcomes["a check that passes"] != gatePass || outcomes["a check that fails"] != gateFail {
		t.Fatalf("outcomes %v", outcomes)
	}
}

// An entry review is no Gn-out: it runs its own rows and neither FS-55's every rows nor FS-54.
func TestAnEntryGateRunsNoGnOutRows(t *testing.T) {
	gates := strings.Replace(fixtureGates, "  every:", "  M2-in:\n    - row: FS-46\n      tests:\n        - {repo: fixture, package: ./widget, names: [TestPasses]}\n  every:", 1)
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M2-in"
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if outcomes := gateOutcomes(report.rows); len(outcomes) != 1 || outcomes["FS-46"] != gatePass {
		t.Fatalf("M2-in ran %v", outcomes)
	}
	if report.group != "" {
		t.Fatalf("an entry review registers group %q", report.group)
	}
}

// -list runs nothing: a declared test lists, one that fails lists too, since nothing ran, a removed one is
// missing, and a stores row lists with no wrapper.
func TestGateListResolvesWithoutRunning(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	options.list = true
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"FS-901":                        gateListed,
		"FS-902":                        gateListed,
		"FS-903":                        gateMissing,
		"FS-904":                        gateMissing,
		"a run on the production hosts": gateOpen,
		"FS-905":                        gateListed,
		"FS-906":                        gateListed,
		"FS-907":                        gateListed,
	}
	got := gateOutcomes(report.rows)
	for label, outcome := range want {
		if got[label] != outcome {
			t.Errorf("%s: %s, want %s", label, got[label], outcome)
		}
	}
	if report.failed() {
		t.Error("a list run failed")
	}
	var out bytes.Buffer
	report.Write(&out)
	for _, text := range []string{"This is -list: nothing ran.", "- rows: 5 listed, 0 fail, 2 missing, 1 open"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("the report lacks %q:\n%s", text, out.String())
		}
	}
}

// sidecar-gate -list M1 on this tree: the path from catalog.yml through gates.yml to the report runs, every
// row of M1's sweep names tests that are declared, FS-55 rides along as every Gn-out's, and the report names
// the commit set it read.
func TestGateM1ListsOnThisTree(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sidecar", "sidecartest", "fault", "catalog.yml")); err != nil {
		t.Skipf("not run: no sidecar checkout beside the server (%v)", err)
	}
	environ := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, gateStoreWrapperVariable+"=")
	})
	report, err := runGate(context.Background(), &gateOptions{
		gate:           "M1",
		base:           "sidecar",
		root:           root,
		serverRelative: "server",
		gatesPath:      "gates.yml",
		matrixPath:     "ops-matrix.yml",
		repos:          gateRepos,
		environ:        environ,
		settings:       defaultGateSettings(),
		list:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, result := range report.rows {
		labels[result.label] = true
		t.Logf("%s: %s", result.label, result.outcome)
		switch result.outcome {
		case gateListed, gateOpen:
		default:
			t.Errorf("%s reads %s on this tree: %s", result.label, result.outcome, strings.Join(result.details, "; "))
		}
	}
	if !labels["FS-54"] || !labels["FS-55"] || len(report.rows) < 10 {
		t.Fatalf("M1's rows: %v", labels)
	}
	for _, repo := range []string{"sidecar", "server"} {
		head, err := exec.Command("git", "-C", filepath.Join(root, repo), "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(report.commits, func(commit gateCommit) bool {
			return commit.repo == repo && commit.commit == strings.TrimSpace(string(head))
		}) {
			t.Errorf("the commit set lacks %s at %s: %+v", repo, head, report.commits)
		}
	}
}
