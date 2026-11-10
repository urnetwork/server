package main

// The design trace, the planes, the stages and the assembly row (TEST-M1-12), on fixture trees and on this one.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A fixture design: two of §1.2's rows, one each of §2.2, §2.9, §3.2 and §23.2, two X rows, §3.5's plane table,
// the sidecar's subprotocol id and an empty sidecar-blocks directory, beside writeGateFixture's module.
func writeTraceFixture(t *testing.T, trace string) *gateOptions {
	t.Helper()
	options := writeGateFixture(t, "gates:\n  M1:\n    - row: FS-54\n      tests:\n        - {repo: fixture, package: ./widget, names: [TestPasses]}\n", "ids: {}\n")
	files := map[string]string{
		"sidecar/design/source/sections/01-CTX.md": "## 1. Context\n\n### 1.2 Requirements\n\n| Id | Requirement | Source | Owner |\n|---|---|---|---|\n" +
			"| R-CTX-1 | the first | PLAN.md:5 | §4 |\n| R-CTX-2 | the second | PLAN.md:14 | §5 |\n\n### 1.3 What exists today\n\n| R-CTX-99 | not in 1.2 | x | §1 |\n",
		"sidecar/design/source/sections/02-TRUST.md": "## 2. Trust\n\n### 2.2 Requirements\n\n| id | requirement | source |\n|---|---|---|\n| R-TRUST-1 | keys | PLAN.md:221 |\n\n" +
			"### 2.9 Security invariants\n\n- **I-1.** No server receives a key.\n- **I-2 (P0).** Ciphertext only.\n\n### 2.10 Threats\n",
		"sidecar/design/source/sections/03-ARCH.md": "## 3. Architecture\n\n### 3.2 Requirements\n\n| id | requirement | source |\n|---|---|---|\n| R-ARCH-1 | the map | OUTLINE §3 |\n\n" +
			"### 3.5 Repo graph and planes\n\n| Plane | Carrier | Endpoints | Protection (owner) |\n|---|---|---|---|\n" +
			"| Control | HTTPS | sidecar → api | jwt |\n| Peer data | id 21842 RPC, streams | sidecar ⇄ sidecar | Required |\n" +
			"| Public edges | TCP 443, 8443 and 25 in, 25 out; TCP and UDP 53 | visitors ⇄ edges | TLS ends in the sidecar |\n\n### 3.6 Runtime topologies\n",
		"sidecar/design/source/sections/23-PHASE.md": "## 23. Phasing\n\n### 23.2 Requirements\n\n| id | requirement | source |\n|---|---|---|\n| R-PHASE-1 | review first | CONVENTIONS |\n\n### 23.3 What exists today\n",
		"sidecar/DESIGN.md":                          "# DESIGN\n\n" + traceXHeader + "\n|---|---|---|---|---|---|\n| X1 | tls? | sidecar | §13 | Q-TLS-1 | None. |\n| X2 | headers? | tokens | §11 | Q-EVS-1 | None. |\n\nAfter the table.\n",
		"sidecar/envelope_constants.go":              "package sidecar\n\nconst (\n\tSubprotocolId = 0x5552\n)\n",
		"warp/services/sidecar-blocks/README.md":     "fragments\n",
		"server/local/sidecar-gate/trace.yml":        trace,
	}
	for name, content := range files {
		path := filepath.Join(options.root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return options
}

// Every fixture id listed: a passing test, a missing test, no evidence, an open row, a failing test, and the
// rest passing or open.
const fixtureTrace = `rows:
  - id: R-CTX-1
    gate: M0
    tests:
      - {repo: fixture, package: ./widget, names: [TestPasses]}
  - {id: R-CTX-2, gate: M1}
  - id: R-TRUST-1
    gate: M2
    open: "hardware: the PM9A3 set"
  - id: I-1
    gate: M2
    tests:
      - {repo: fixture, package: ./widget, names: [TestDoesNotExist]}
  - id: I-2
    gate: M1
    tests:
      - {repo: fixture, package: ./widget, names: [TestFails]}
  - id: R-ARCH-1
    gate: G0
    schedules: [FS-54]
  - id: R-PHASE-1
    gate: G0
    out_of_build: "app, console, or extension UI"
  - id: X1
    gate: M5
    tests:
      - {repo: fixture, package: ./widget, names: [TestPasses]}
  - id: X2
    gate: M2
    schedules: [FS-1]
carriers: []
`

// The row whose label starts with id.
func traceRowResult(report *traceReport, id string) *gateResult {
	for _, result := range report.rows {
		if result.label == id || strings.HasPrefix(result.label, id+" ") {
			return result
		}
	}
	return nil
}

// A passing test passes; no evidence, a missing test and a schedule no gate holds read missing; an open row and
// an out-of-build row read open, never pass; a failing test fails.
func TestTraceReadsEachRowAsItIs(t *testing.T) {
	options := writeTraceFixture(t, fixtureTrace)
	report, err := runTrace(context.Background(), options, filepath.Join(options.root, "server", "local", "sidecar-gate", "trace.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if 0 < len(report.findings) {
		t.Fatalf("findings on a trace that lists every row: %v", report.findings)
	}
	for id, want := range map[string]string{
		"R-CTX-1": gatePass, "R-CTX-2": gateMissing, "R-TRUST-1": gateOpen, "I-1": gateMissing, "I-2": gateFail,
		"R-ARCH-1": gatePass, "R-PHASE-1": gateOpen, "X1": gatePass, "X2": gateMissing,
	} {
		result := traceRowResult(report, id)
		if result == nil || result.outcome != want {
			t.Errorf("%s: %+v, want %s", id, result, want)
		}
	}
	if len(report.rows) != 9 {
		t.Errorf("%d rows, want the fixture's 9", len(report.rows))
	}
	if !report.failed() {
		t.Error("a failing row did not fail the trace")
	}
	var out bytes.Buffer
	report.Write(&out)
	for _, text := range []string{"| R-CTX-1 (first at M0) | pass |", "| X2 (first at M2) | missing |", "out of this build: app, console, or extension UI", "- rows: 3 pass, 0 listed, 1 fail, 3 missing, 2 open"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("the report lacks %q:\n%s", text, out.String())
		}
	}
}

// A section table gaining a row fails the trace until trace.yml lists it, and an entry no table holds fails too.
func TestTraceFailsUntilANewDesignRowIsListed(t *testing.T) {
	options := writeTraceFixture(t, fixtureTrace)
	tracePath := filepath.Join(options.root, "server", "local", "sidecar-gate", "trace.yml")
	section := filepath.Join(options.root, "sidecar", "design", "source", "sections", "01-CTX.md")
	data, err := os.ReadFile(section)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(section, bytes.Replace(data, []byte("| R-CTX-2 |"), []byte("| R-CTX-3 | the third | PLAN.md:17 | §16 |\n| R-CTX-2 |"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runTrace(context.Background(), options, tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(report.findings, func(finding string) bool { return strings.Contains(finding, "R-CTX-3 has no entry in trace.yml") }) || !report.failed() {
		t.Fatalf("a new row passed unlisted: %v", report.findings)
	}
	if result := traceRowResult(report, "R-CTX-3"); result == nil || result.outcome != gateMissing {
		t.Fatalf("the unlisted row: %+v", result)
	}

	listed := strings.Replace(fixtureTrace, "  - {id: R-CTX-2, gate: M1}\n", "  - {id: R-CTX-2, gate: M1}\n  - {id: R-CTX-3}\n  - {id: R-CTX-404, gate: M3}\n", 1)
	if err := os.WriteFile(tracePath, []byte(listed), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = runTrace(context.Background(), options, tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.findings) != 2 || !strings.Contains(report.findings[0], `R-CTX-3's first gate "" is no gate`) || !strings.Contains(report.findings[1], "R-CTX-404, which no roll-up list") {
		t.Fatalf("findings %v", report.findings)
	}
}

// A port outside §3.5's table fails the planes, and passes once its carriers entry cites an owner decision; an
// entry citing none allows nothing, and a subprotocol id other than §3.5's fails.
func TestPlanesHoldTheBlocksPortsToSection35(t *testing.T) {
	options := writeTraceFixture(t, fixtureTrace)
	fragment := filepath.Join(options.root, "warp", "services", "sidecar-blocks", "smtpedge.yml")
	if err := os.WriteFile(fragment, []byte("version:\n  lb:\n    tcp_stream_port_services:\n      25: mxedge\n      2525: smtpedge\n    udp_stream_port_services:\n      53: dns\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runPlanes(options, nil)
	if result.outcome != gateFail || !slices.ContainsFunc(result.details, func(detail string) bool { return strings.HasPrefix(detail, "tcp 2525: outside §3.5's table") }) {
		t.Fatalf("an outside port: %+v", result)
	}
	if result := runPlanes(options, []*traceCarrier{{Carrier: "tcp 2525", Decision: "a reviewer said so"}}); result.outcome != gateFail {
		t.Fatalf("a carrier citing no owner decision allowed the port: %+v", result)
	}
	result = runPlanes(options, []*traceCarrier{{Carrier: "tcp 2525", Decision: "owner decision 999, submission on 2525"}})
	if result.outcome != gatePass {
		t.Fatalf("a carrier citing an owner decision: %+v", result)
	}
	constants := filepath.Join(options.root, "sidecar", "envelope_constants.go")
	if err := os.WriteFile(constants, []byte("package sidecar\n\nconst SubprotocolId = 0x5553\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := runPlanes(options, []*traceCarrier{{Carrier: "tcp 2525", Decision: "owner decision 999"}}); result.outcome != gateFail {
		t.Fatalf("another subprotocol id: %+v", result)
	}
}

// §3.5's carriers as the table writes them: each clause's protocols take its numbers, HTTPS is tcp 443.
func TestSectionCarriersReadEachClause(t *testing.T) {
	options := writeTraceFixture(t, fixtureTrace)
	carriers, err := sectionCarriers(filepath.Join(options.root, "sidecar", "design", "source", "sections", "03-ARCH.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tcp 443", "tcp 8443", "tcp 25", "tcp 53", "udp 53", "subprotocol 21842"} {
		if !carriers[want] {
			t.Errorf("no %s in %v", want, carriers)
		}
	}
	if carriers["udp 443"] || carriers["udp 25"] {
		t.Errorf("a TCP clause's port read as UDP: %v", carriers)
	}
}

// A stage with one item failing is incomplete and names it; a vertical slice with no server half is missing;
// people rows read open; and a stage whose every item passes is whole.
func TestStageReportsWholeOrNamesWhatIsMissing(t *testing.T) {
	options := writeTraceFixture(t, fixtureTrace)
	stagesPath := filepath.Join(options.root, "server", "local", "sidecar-gate", "stages.yml")
	stages := `stages:
  - id: RS1
    name: "record and reach"
    milestones: [M1]
    releases: "the fixture's"
    items:
      - item: "a slice that passes"
        sidecar:
          tests:
            - {repo: fixture, package: ./widget, names: [TestPasses]}
        server:
          tests:
            - {repo: fixture, package: ./widget, names: [TestPasses]}
      - item: "a slice with one half"
        sidecar:
          tests:
            - {repo: fixture, package: ./widget, names: [TestPasses]}
      - item: "an item that fails"
        tests:
          - {repo: fixture, package: ./widget, names: [TestFails]}
      - item: "the gate"
        gates: [M1]
      - item: "a person signs"
        open: "people: the owner"
  - id: RS2
    name: "whole"
    milestones: [M1]
    releases: "the fixture's"
    items:
      - item: "a slice that passes"
        sidecar:
          tests:
            - {repo: fixture, package: ./widget, names: [TestPasses]}
        server:
          tests:
            - {repo: fixture, package: ./widget, names: [TestPasses]}
`
	if err := os.WriteFile(stagesPath, []byte(stages), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runStage(context.Background(), options, stagesPath, "RS1")
	if err != nil {
		t.Fatal(err)
	}
	outcomes := gateOutcomes(report.items)
	for item, want := range map[string]string{"a slice that passes": gatePass, "a slice with one half": gateMissing, "an item that fails": gateFail, "the gate": gatePass, "a person signs": gateOpen} {
		if outcomes[item] != want {
			t.Errorf("%s: %s, want %s", item, outcomes[item], want)
		}
	}
	var out bytes.Buffer
	report.Write(&out)
	if !strings.Contains(out.String(), "RS1 is incomplete: a slice with one half (missing); an item that fails (fail); a person signs (open).") {
		t.Fatalf("the stage's verdict:\n%s", out.String())
	}
	whole, err := runStage(context.Background(), options, stagesPath, "RS2")
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	whole.Write(&out)
	if !strings.Contains(out.String(), "RS2 is whole: every item passed.") {
		t.Fatalf("a whole stage:\n%s", out.String())
	}
	if _, err := runStage(context.Background(), options, stagesPath, "RS3"); err == nil {
		t.Fatal("a stage stages.yml does not hold ran")
	}
}

// A fixture design source whose build.sh writes DESIGN.md and validate.json from files the test controls, and
// whose grep and carry scripts write the hits and readings it controls, so each of the check's parts can be
// planted on its own.
func writeAssemblyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"sidecar/DESIGN.md":                               "# DESIGN\nbuilt\n",
		"sidecar/design/source/built.md":                  "# DESIGN\nbuilt\n",
		"sidecar/design/source/validate.json":             `{"ids": {"duplicates": {}, "defined_outside_own_section": [], "undefined_references": {}}, "r_links": {"broken": [], "unconverted": []}, "anchors": {"toc_missing": [], "internal_missing": [], "relative_links_missing": []}, "owner_decisions": {"uncited_1_145": [], "uncited_not_superseded": [], "linked_targets_missing": []}, "owner_record": {"named_but_open": [], "body_named_open": [], "body_named_missing": []}, "sections_verbatim": {"mismatches": []}, "tables": {"bad_rows": []}, "x_items": {"missing": []}}`,
		"sidecar/design/source/readings.json":             `[{"family": "pools", "file": "11-EVS.md", "line": 3, "reading": "ok: names no pool", "live": false, "gap": false}]`,
		"sidecar/design/source/build.sh":                  "#!/bin/sh\nset -eu\nsrc=$(cd \"$(dirname \"$0\")\" && pwd)\nmkdir -p \"$src/asm/v7\"\ncp \"$src/built.md\" \"$1\"\ncp \"$src/validate.json\" \"$src/asm/v7/validate.json\"\n",
		"sidecar/design/source/asm/v7/supersede_grep7.py": "import sys, json\nopen(sys.argv[2], 'w').write(json.dumps({'hits': []}))\n",
		"sidecar/design/source/asm/v7/carry_readings.py":  "import sys, shutil, os\nshutil.copy(os.path.join(os.path.dirname(os.path.abspath(sys.argv[1])), 'design', 'source', 'readings.json'), sys.argv[3])\n",
		"sidecar/design/source/asm/v7/hits_class.json":    "[]\n",
		"sidecar/design/source/sections/03-ARCH.md":       "## 3\n\n**D-ARCH-1.** `<root>` and `<net>`; PLAN.md:25's `<me>` appears only in PLAN quotations.\n\n### 3.12 Departures from PLAN.md\n\n| :104 `mail.<me>.ur.network` | `mx.<net>.<root>` |\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The assembly row passes on a fixture whose build reproduces DESIGN.md, and each planted part fails it or, for
// an unread grep hit, leaves it missing.
func TestAssemblyFailsOnEachPlantedPart(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("not run: this host lacks python3, which the assembly scripts need: %v", err)
	}
	settings := defaultAssemblySettings()
	settings.ScratchParent = t.TempDir()
	settings.LastGateRef = "no-such-ref"
	write := func(root string, name string, content string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	root := writeAssemblyFixture(t)
	if result := runAssembly(context.Background(), root, settings); result.outcome != gatePass {
		t.Fatalf("the clean fixture: %+v", result)
	}

	root = writeAssemblyFixture(t)
	write(root, "sidecar/DESIGN.md", "# DESIGN\nedited by hand\n")
	if result := runAssembly(context.Background(), root, settings); result.outcome != gateFail ||
		!slices.ContainsFunc(result.details, func(detail string) bool {
			return strings.Contains(detail, "differs from the checked-in DESIGN.md from line 2")
		}) {
		t.Errorf("a DESIGN.md its sources do not make: %+v", result)
	}

	root = writeAssemblyFixture(t)
	validate, err := os.ReadFile(filepath.Join(root, "sidecar", "design", "source", "validate.json"))
	if err != nil {
		t.Fatal(err)
	}
	write(root, "sidecar/design/source/validate.json", strings.Replace(string(validate), `"uncited_not_superseded": []`, `"uncited_not_superseded": [197]`, 1))
	if result := runAssembly(context.Background(), root, settings); result.outcome != gateFail ||
		!slices.ContainsFunc(result.details, func(detail string) bool {
			return strings.Contains(detail, "owner-decision: owner_decisions.uncited_not_superseded: 1")
		}) {
		t.Errorf("a validate.json failure: %+v", result)
	}

	root = writeAssemblyFixture(t)
	write(root, "sidecar/design/source/readings.json", `[{"family": "pools", "file": "11-EVS.md", "line": 3, "reading": "UNCLASSIFIED", "live": false, "gap": false}]`)
	if result := runAssembly(context.Background(), root, settings); result.outcome != gateMissing {
		t.Errorf("a grep hit with no reading: %+v", result)
	}

	root = writeAssemblyFixture(t)
	write(root, "sidecar/design/source/readings.json", `[{"family": "pools", "file": "11-EVS.md", "line": 3, "reading": "LIVE: a pool", "live": true, "gap": false}]`)
	if result := runAssembly(context.Background(), root, settings); result.outcome != gateFail {
		t.Errorf("a live grep hit: %+v", result)
	}

	root = writeAssemblyFixture(t)
	write(root, "sidecar/design/source/sections/03-ARCH.md", "## 3\n\nThe mail host is `mail.<me>.ur.network`.\n")
	if result := runAssembly(context.Background(), root, settings); result.outcome != gateFail ||
		!slices.ContainsFunc(result.details, func(detail string) bool {
			return strings.Contains(detail, "03-ARCH.md:3 spells <me> outside a PLAN quotation")
		}) {
		t.Errorf("<me> outside a PLAN quotation: %+v", result)
	}
}

// What the assembly row says beside its exit status: a command printing its outcome reads that outcome, and
// fails whatever it says once its exit status does.
func TestGateCommandReadsAnOutcomeLine(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	script := filepath.Join(options.root, "fixture", "outcome.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'report'\necho \""+gateOutcomeLinePrefix+"$1\"\nexit $2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		arguments []string
		want      string
	}{
		{arguments: []string{"missing", "0"}, want: gateMissing},
		{arguments: []string{"open", "0"}, want: gateOpen},
		{arguments: []string{"pass", "0"}, want: gatePass},
		{arguments: []string{"pass", "1"}, want: gateFail},
	} {
		if outcome, details := runGateCommand(context.Background(), options, gateCommand{Repo: "fixture", Command: append([]string{script}, check.arguments...)}); outcome != check.want {
			t.Errorf("%v: %s (%v), want %s", check.arguments, outcome, details, check.want)
		}
	}
}

// sidecar-gate -list trace on this tree: the trace and the design list the same ids, every row prints its state,
// none reads fail, and the planes hold.
func TestTraceListsOnThisTree(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sidecar", traceSectionsPath)); err != nil {
		t.Skipf("not run: no sidecar checkout beside the server (%v)", err)
	}
	report, err := runTrace(context.Background(), &gateOptions{
		gate: "trace", base: "sidecar", root: root, serverRelative: "server", gatesPath: "gates.yml",
		repos: gateRepos, environ: os.Environ(), settings: defaultGateSettings(), list: true,
	}, "trace.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.findings {
		t.Errorf("the trace and the design differ: %s", finding)
	}
	counts := countOutcomes(report.rows)
	for _, result := range report.rows {
		t.Logf("%s: %s", result.label, result.outcome)
		if result.outcome == gateFail {
			t.Errorf("%s fails on this tree: %s", result.label, strings.Join(result.details, "; "))
		}
	}
	if len(report.rows) < 100 {
		t.Errorf("%d rows: the design holds over 100", len(report.rows))
	}
	if report.planes == nil || report.planes.outcome != gatePass {
		t.Errorf("the planes on this tree: %+v", report.planes)
	}
	t.Logf("%d listed, %d missing, %d open", counts[gateListed], counts[gateMissing], counts[gateOpen])
}

// The assembly row on this tree: the sources build DESIGN.md as it is checked in and validate, and nothing reads
// live; an unread grep hit is a person's to read, so it may read missing, never fail.
func TestAssemblyOnThisTree(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sidecar", assemblySourcePath, "build.sh")); err != nil {
		t.Skipf("not run: no sidecar design sources beside the server (%v)", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("not run: this host lacks python3, which the assembly scripts need: %v", err)
	}
	settings := defaultAssemblySettings()
	settings.ScratchParent = t.TempDir()
	result := runAssembly(context.Background(), root, settings)
	for _, detail := range result.details {
		t.Log(detail)
	}
	if result.outcome != gatePass && result.outcome != gateMissing {
		t.Fatalf("the assembly on this tree reads %s", result.outcome)
	}
}
