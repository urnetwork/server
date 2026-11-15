package main

// M2's entry review (TEST-M2-18): what sidecar's M2-in sweep prints for an item is what this tool reads, so a written
// row, an empty builder, a missing row, an open review and a model TLC has not run each report as they are; and every
// item of the real M2-in names tests that exist and commands this tool runs.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// sidecar's outcome line and outcomes are this tool's.
func TestGateM2InSpeaksTheToolsOutcomes(t *testing.T) {
	if fault.GateOutcomeLinePrefix != gateOutcomeLinePrefix {
		t.Errorf("sidecar prints %q, the tool reads %q", fault.GateOutcomeLinePrefix, gateOutcomeLinePrefix)
	}
	for _, pair := range [][2]string{
		{fault.GateOutcomePass, gatePass}, {fault.GateOutcomeFail, gateFail}, {fault.GateOutcomeMissing, gateMissing}, {fault.GateOutcomeOpen, gateOpen},
	} {
		if pair[0] != pair[1] {
			t.Errorf("sidecar's %q is the tool's %q", pair[0], pair[1])
		}
	}
}

// On a fixture, each kind of M2-in item reports as it is: a command printing a written row's lines passes, an empty
// builder's fails, a missing row's reads missing, a model TLC has not run reads open, and a review with nothing to run
// reads open, each with the reason the command printed last.
func TestGateM2InReportsEachKindOfItem(t *testing.T) {
	lines := map[string]*fault.GateWrittenResult{
		"written": {Outcome: fault.GateOutcomePass, Reason: "FS-901: written on T2, its schedule 3 steps checking FI-1"},
		"empty":   {Outcome: fault.GateOutcomeFail, Reason: "FS-902's T2 builder: an empty builder: its schedule has no step"},
		"missing": {Outcome: fault.GateOutcomeMissing, Reason: "FS-903: no T2 builder is registered (fault.Register)"},
		"tlc":     {Outcome: fault.GateOutcomeOpen, Reason: "TLC has not run at these sources for TestEvsCommitPassesTlcAtItsBounds"},
	}
	gates := `gates:
  M2-in:
    - item: "FS-901 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, written]}
    - item: "FS-902 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, empty]}
    - item: "FS-903 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, missing]}
    - item: "EvsCommit.tla passing TLC at its bounds"
      commands:
        - {repo: fixture, command: [./written.sh, tlc]}
    - item: "RECORD.md reviewed"
      external: "people: a reviewer reads RECORD.md and signs it"
`
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M2-in"
	// as go test -v prints the written check: its run line, the lines, then its verdict; an empty builder's run fails
	script := "#!/bin/sh\necho '=== RUN   TestGateM2InRowWritten'\ncase \"$1\" in\n"
	for name, result := range lines {
		status := 0
		verdict := "PASS"
		if result.Outcome == fault.GateOutcomeFail {
			status = 1
			verdict = "FAIL"
		}
		script += fmt.Sprintf("%s) printf '%%s' '%s'; echo '--- %s: TestGateM2InRowWritten (0.00s)'; exit %d;;\n", name, strings.ReplaceAll(result.Lines(), "'", "'\\''"), verdict, status)
	}
	script += "esac\nexit 2\n"
	if err := os.WriteFile(filepath.Join(options.root, "fixture", "written.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]*gateResult{}
	for _, result := range report.rows {
		results[result.label] = result
	}
	for label, want := range map[string]struct {
		outcome string
		reason  string
	}{
		"FS-901 written as a test against the fakes": {outcome: gatePass, reason: lines["written"].Reason},
		"FS-902 written as a test against the fakes": {outcome: gateFail, reason: lines["empty"].Reason},
		"FS-903 written as a test against the fakes": {outcome: gateMissing, reason: lines["missing"].Reason},
		"EvsCommit.tla passing TLC at its bounds":    {outcome: gateOpen, reason: lines["tlc"].Reason},
		"RECORD.md reviewed":                         {outcome: gateOpen, reason: "people: a reviewer reads RECORD.md"},
	} {
		result := results[label]
		if result == nil {
			t.Errorf("no row %q", label)
			continue
		}
		if result.outcome != want.outcome || !strings.Contains(strings.Join(result.details, "; "), want.reason) {
			t.Errorf("%q reads %s: %v; want %s with %q", label, result.outcome, result.details, want.outcome, want.reason)
		}
	}
	if !report.failed() {
		t.Error("an empty builder failed nothing")
	}
}

// sidecar-gate -list M2-in on this tree: D-ARCH-4's assembly first, every FS case of M2's exit an item of its own,
// every test an item names declared, each command listed and nothing run, and each review open.
func TestGateM2InListsOnThisTree(t *testing.T) {
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
		gate:           "M2-in",
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
	if len(report.rows) == 0 || !strings.HasPrefix(report.rows[0].label, "D-ARCH-4's assembly check") {
		t.Fatalf("M2-in opens with %+v", report.rows)
	}
	labels := map[string]string{}
	for _, result := range report.rows {
		labels[result.label] = result.outcome
		switch result.outcome {
		case gateListed, gateOpen:
		default:
			t.Errorf("%s reads %s on this tree: %s", result.label, result.outcome, strings.Join(result.details, "; "))
		}
	}
	for _, testCase := range fault.DefaultCatalog().GateCases("M2-out") {
		label := testCase.String() + " written as a test against the fakes (§22.10's M2 entry)"
		if labels[label] != gateListed {
			t.Errorf("%q reads %q", label, labels[label])
		}
	}
	reviews := 0
	for label, outcome := range labels {
		if strings.Contains(label, "reviewed") || strings.HasPrefix(label, "peer review") || strings.Contains(label, "design closure") {
			reviews += 1
			if outcome != gateOpen {
				t.Errorf("the review %q reads %s", label, outcome)
			}
		}
	}
	if reviews < 4 {
		t.Errorf("M2-in holds %d reviews: %v", reviews, labels)
	}
}
