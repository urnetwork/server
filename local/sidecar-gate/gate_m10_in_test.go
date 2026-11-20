package main

// M10's entry review (TEST-M10-04): what sidecar's M10-in sweep prints for an item is what this tool reads, so a written
// row, an empty builder, a missing row, a case written as the T3 test M10's exit sweep names, a model TLC has not run,
// the purger's stories not standing, an open review and a vector no test shows yet each report as they are; and every
// item of the real M10-in names tests that exist and commands this tool runs.

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

// On a fixture, each kind of M10-in item reports as it is, with the reason the command printed last: a written row
// passes, an empty builder fails, a missing row reads missing, FS-73's declared T3 test passes, TLC not run and the
// purger's stories not standing read open, a review with nothing to run but people reads open, and the cards' vectors,
// with nothing to run and nothing waited on, read missing.
func TestGateM10InReportsEachKindOfItem(t *testing.T) {
	lines := map[string]*fault.GateWrittenResult{
		"written": {Outcome: fault.GateOutcomePass, Reason: "FS-901: written on T2, its schedule 3 steps checking FI-16"},
		"empty":   {Outcome: fault.GateOutcomeFail, Reason: "FS-902's T2 builder: its schedule checks nothing: it names no invariant (the contract's rule 4)"},
		"missing": {Outcome: fault.GateOutcomeMissing, Reason: "FS-903: no T2 builder is registered (fault.Register)"},
		"t3":      {Outcome: fault.GateOutcomePass, Reason: "FS-904: written on T3 as the tests M10's exit sweep names in place of a builder"},
		"tlc":     {Outcome: fault.GateOutcomeOpen, Reason: "TLC has not run at these sources for TestIndexRollupPassesTlcAtItsBounds"},
		"stories": {Outcome: fault.GateOutcomeOpen, Reason: "no passing stories run of EvsCommit, which holds the purger's stories, stands at its sources"},
	}
	gates := `gates:
  M10-in:
    - item: "FS-901 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, written]}
    - item: "FS-902 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, empty]}
    - item: "FS-903 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, missing]}
    - item: "FS-904 written as a test against the fakes"
      commands:
        - {repo: fixture, command: [./written.sh, t3]}
    - item: "IndexRollup.tla passing TLC at its bounds"
      commands:
        - {repo: fixture, command: [./written.sh, tlc]}
    - item: "EvsCommit.tla with the purger passing TLC"
      commands:
        - {repo: fixture, command: [./written.sh, stories]}
    - item: "peer review of format 2's schemas"
      external: "people: the owning sections' reviewers read and sign the schemas"
    - item: "the vector schemas fixed, directory.proto's cards and repo files"
`
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M10-in"
	// as go test -v prints the written check: its run line, the lines, then its verdict; an empty builder's run fails
	script := "#!/bin/sh\necho '=== RUN   TestGateM10InRowWritten'\ncase \"$1\" in\n"
	for name, result := range lines {
		status := 0
		verdict := "PASS"
		if result.Outcome == fault.GateOutcomeFail {
			status = 1
			verdict = "FAIL"
		}
		script += fmt.Sprintf("%s) printf '%%s' '%s'; echo '--- %s: TestGateM10InRowWritten (0.00s)'; exit %d;;\n", name, strings.ReplaceAll(result.Lines(), "'", "'\\''"), verdict, status)
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
		"FS-901 written as a test against the fakes":                       {outcome: gatePass, reason: lines["written"].Reason},
		"FS-902 written as a test against the fakes":                       {outcome: gateFail, reason: lines["empty"].Reason},
		"FS-903 written as a test against the fakes":                       {outcome: gateMissing, reason: lines["missing"].Reason},
		"FS-904 written as a test against the fakes":                       {outcome: gatePass, reason: lines["t3"].Reason},
		"IndexRollup.tla passing TLC at its bounds":                        {outcome: gateOpen, reason: lines["tlc"].Reason},
		"EvsCommit.tla with the purger passing TLC":                        {outcome: gateOpen, reason: lines["stories"].Reason},
		"peer review of format 2's schemas":                                {outcome: gateOpen, reason: "people: the owning sections' reviewers"},
		"the vector schemas fixed, directory.proto's cards and repo files": {outcome: gateMissing, reason: "no test shows it yet"},
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

// sidecar-gate -list M10-in on this tree: D-ARCH-4's assembly first, every case of FS-64 to FS-81 and FS-83 to FS-96
// that M10's exit holds an item of its own, every test an item names declared, each command listed and nothing run,
// each review open, and the cards' vectors missing until a vector file pins their bytes.
func TestGateM10InListsOnThisTree(t *testing.T) {
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
		gate:           "M10-in",
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
		t.Fatalf("M10-in opens with %+v", report.rows)
	}
	cardsVectors := "the vector schemas fixed, directory.proto's cards and repo files"
	labels := map[string]string{}
	for _, result := range report.rows {
		labels[result.label] = result.outcome
		switch {
		case strings.HasPrefix(result.label, cardsVectors):
			if result.outcome != gateMissing {
				t.Errorf("%s reads %s on this tree: %s", result.label, result.outcome, strings.Join(result.details, "; "))
			}
		case result.outcome == gateListed, result.outcome == gateOpen:
		default:
			t.Errorf("%s reads %s on this tree: %s", result.label, result.outcome, strings.Join(result.details, "; "))
		}
	}
	cases := fault.GateM10InCases(fault.DefaultCatalog())
	if len(cases) == 0 {
		t.Fatal("M10's entry looks for no case")
	}
	for _, testCase := range cases {
		label := testCase.String() + " written as a test against the fakes (§22.10's M10 entry)"
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
	if reviews != 3 {
		t.Errorf("M10-in holds %d reviews: %v", reviews, labels)
	}
}
