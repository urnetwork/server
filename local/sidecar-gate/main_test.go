package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The gate report on a fixture monoroot, and gates.yml against §23.6.

// A fixture monoroot: one git repo holding a Go module whose tests pass, fail
// and skip, and the gate and matrix files that point at them.
func writeGateFixture(t *testing.T, gates string, matrix string) *gateOptions {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "fixture")
	if err := os.MkdirAll(filepath.Join(repo, "widget"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":           "module example.com/fixture\n\ngo 1.27.3\n",
		"widget/widget.go": "package widget\n\n// Answers two.\nfunc Two() int {\n\treturn 2\n}\n",
		"widget/widget_test.go": "package widget\n\nimport (\n\t\"flag\"\n\t\"testing\"\n)\n\n" +
			"var group = flag.String(\"fixture.group\", \"\", \"the gate's group\")\n\n" +
			"func TestGroup(t *testing.T) {\n\tif *group != \"MG1\" {\n\t\tt.Fatalf(\"group %q\", *group)\n\t}\n}\n\n" +
			"func TestPasses(t *testing.T) {\n\tif Two() != 2 {\n\t\tt.Fatal(\"two\")\n\t}\n}\n\n" +
			"func TestFails(t *testing.T) {\n\tt.Fatal(\"a planted failure\")\n}\n\n" +
			"func TestSkips(t *testing.T) {\n\tt.Skip(\"needs a host this machine is not\")\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "sidecar"},
		{"-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "add", "."},
		{"-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit", "-q", "-m", "Add the fixture"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s\n%s", args, err, output)
		}
	}
	gatesPath := filepath.Join(root, "gates.yml")
	matrixPath := filepath.Join(root, "ops-matrix.yml")
	if err := os.WriteFile(gatesPath, []byte(gates), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matrixPath, []byte(matrix), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, gateStoreWrapperVariable+"=") || strings.HasPrefix(variable, "GOFLAGS=")
	})
	return &gateOptions{
		gate:       "M1",
		base:       "main",
		root:       root,
		gatesPath:  gatesPath,
		matrixPath: matrixPath,
		repos:      []string{"fixture", "absent"},
		environ:    append(environ, "GOWORK=off", "GOFLAGS=-mod=mod"),
		settings:   defaultGateSettings(),
	}
}

const fixtureGates = `gates:
  M1:
    - row: FS-901
      tests:
        - {repo: fixture, package: ./widget, names: [TestPasses]}
    - row: FS-902
      tests:
        - {repo: fixture, package: ./widget, names: [TestPasses, TestFails]}
    - row: FS-903
      tests:
        - {repo: fixture, package: ./widget, names: [TestDoesNotExist]}
    - {row: FS-904}
    - {item: "a run on the production hosts", external: "the production hosts"}
    - row: FS-905
      tests:
        - {repo: fixture, package: ./widget, names: [TestSkips]}
    - row: FS-906
      tests:
        - {repo: fixture, package: ./widget, stores: true, names: [TestPasses]}
  every:
    - row: FS-907
      tests:
        - {repo: fixture, package: ./widget, names: [TestGroup], args: ["-fixture.group={group}"]}
`

const fixtureMatrix = `ids:
  D-OPS-6:
    title: pending groups
    tests:
      - {repo: fixture, package: ./widget, names: [TestPasses]}
  R-OPS-3:
    title: tiers
  R-OPS-12:
    title: the first metadata cluster
    external: the PM9A3 set
`

func gateOutcomes(results []*gateResult) map[string]string {
	outcomes := map[string]string{}
	for _, result := range results {
		outcomes[result.label] = result.outcome
	}
	return outcomes
}

// A passing, a failing and a missing row each report as such; a row with no
// test is missing; an external one is open; a skipped test is open, never pass;
// a stores row with no wrapper named in the environment is open.
func TestGateReportsEachRowAsItIs(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	got := gateOutcomes(report.rows)
	want := map[string]string{
		"FS-901":                        gatePass,
		"FS-902":                        gateFail,
		"FS-903":                        gateMissing,
		"FS-904":                        gateMissing,
		"a run on the production hosts": gateOpen,
		"FS-905":                        gateOpen,
		"FS-906":                        gateOpen,
		// M1 registers MG1, so the every-Gn-out row runs with its group filled in
		"FS-907": gatePass,
	}
	for label, outcome := range want {
		if got[label] != outcome {
			t.Errorf("%s: %s, want %s", label, got[label], outcome)
		}
	}
	matrix := gateOutcomes(report.matrix)
	for label, outcome := range map[string]string{"D-OPS-6 pending groups": gatePass, "R-OPS-3 tiers": gateMissing, "R-OPS-12 the first metadata cluster": gateOpen} {
		if matrix[label] != outcome {
			t.Errorf("%s: %s, want %s", label, matrix[label], outcome)
		}
	}
	if !report.failed() {
		t.Error("a failing row did not fail the run")
	}
}

// A test that does not exist never reads as a pass, alone or beside one that
// passes.
func TestGateNeverReadsAMissingTestAsAPass(t *testing.T) {
	gates := `gates:
  M3:
    - row: FS-910
      tests:
        - {repo: fixture, package: ./widget, names: [TestPasses, TestRenamedAway]}
`
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M3"
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := gateOutcomes(report.rows)["FS-910"]; outcome != gateMissing {
		t.Fatalf("a row with a test that does not exist reads %s", outcome)
	}
}

// The report names the commit set the run saw, says what it did not read, and
// states every outcome.
func TestGateReportNamesTheCommitSet(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "-C", filepath.Join(options.root, "fixture"), "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report.Write(&out)
	text := out.String()
	for _, want := range []string{
		"# Gate report: M1",
		"| fixture | sidecar | " + strings.TrimSpace(string(head)) + " |",
		"| absent | | not checked out here |",
		"| vault | | not read",
		"| FS-902 | fail |",
		"| FS-903 | missing |",
		"widget TestDoesNotExist: no such test",
		"- rows: 2 pass, 1 fail, 2 missing, 3 open",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %q:\n%s", want, text)
		}
	}
}

// An every-Gn-out row that needs a group reads open at a gate that registers
// none, rather than running with an empty group, and FS-54's M1 rows run at the
// later gate too.
func TestGateWithoutAGroupLeavesFs55Open(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	options.gate = "M3"
	gates := strings.Replace(fixtureGates, "  every:", "  M3:\n    - {row: FS-930}\n  every:", 1)
	gates = strings.Replace(gates, "    - row: FS-901\n", "    - row: FS-54\n", 1)
	if err := os.WriteFile(options.gatesPath, []byte(gates), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := gateOutcomes(report.rows)
	if outcomes["FS-907"] != gateOpen {
		t.Fatalf("FS-907 at a gate with no group reads %s", outcomes["FS-907"])
	}
	if outcomes["FS-54"] != gatePass || outcomes["FS-930"] != gateMissing {
		t.Fatalf("M3's rows read %v", outcomes)
	}
	if _, ran := outcomes["FS-902"]; ran {
		t.Fatal("an M1 row other than FS-54 ran at M3")
	}
}

// A check this host cannot run, for want of a Python module, is open, never a
// failure of what it checks, and a failing command still fails.
func TestGateCommandsTellAMissingModuleFromAFailure(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	script := filepath.Join(options.root, "fixture", "check.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"ModuleNotFoundError: No module named 'yaml'\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if outcome, details := runGateCommand(context.Background(), options, gateCommand{Repo: "fixture", Command: []string{script}}); outcome != gateOpen {
		t.Fatalf("a missing module reads %s: %v", outcome, details)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'FAILED (failures=1)'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := runGateCommand(context.Background(), options, gateCommand{Repo: "fixture", Command: []string{script}}); outcome != gateFail {
		t.Fatalf("a failing check reads %s", outcome)
	}
}

// The checks run without the caller's GOFLAGS, so a -trimpath the caller
// carries cannot move the paths a repo's tests read their files by.
func TestGateChecksRunWithoutTheCallersGoflags(t *testing.T) {
	environ := gateChildEnv([]string{"GOFLAGS=-trimpath", "HOME=/nonexistent"})
	if slices.ContainsFunc(environ, func(variable string) bool { return strings.HasPrefix(variable, "GOFLAGS=") }) || !slices.Contains(environ, "HOME=/nonexistent") {
		t.Fatalf("environ %v", environ)
	}
}

// §19's ids report in order: requirements, then decisions, each by number.
func TestGateOrdersOpsIdsByKindAndNumber(t *testing.T) {
	ids := []string{"D-OPS-12", "R-OPS-10", "D-OPS-9", "R-OPS-2", "D-OPS-1"}
	slices.SortFunc(ids, compareOpsIds)
	if want := []string{"R-OPS-2", "R-OPS-10", "D-OPS-1", "D-OPS-9", "D-OPS-12"}; !slices.Equal(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}
}

func TestGateRefusesAGateItDoesNotHold(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	options.gate = "M42"
	if _, err := runGate(context.Background(), options); err == nil || !strings.Contains(err.Error(), "holds no gate") {
		t.Fatalf("an unknown gate: %v", err)
	}
}

// The FS ids §23.6 names at each gate, read from the design's own text: the
// table's right column, ranges expanded, gate names joined across their lines.
func section236Rows(t *testing.T) map[string][]string {
	t.Helper()
	path := "../../../sidecar/design/source/sections/23-PHASE.md"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no sidecar checkout beside the server (%s)", path)
	}
	text := string(data)
	start := strings.Index(text, "gate     design closure at Gn-in")
	if start < 0 {
		t.Fatal("§23.6's table is not where it was")
	}
	block := text[start : start+strings.Index(text[start:], "```")]
	lines := strings.Split(block, "\n")
	column := strings.Index(lines[0], "schedules passing at Gn-out")
	alias := map[string]string{"Gn-out": "every", "metadata": "next", "cluster": "next"}
	columns := map[string]string{}
	current := ""
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' {
			current = strings.TrimSpace(line[:min(9, len(line))])
			if name, ok := alias[current]; ok {
				current = name
			}
		}
		if column < len(line) {
			columns[current] += " " + line[column:]
		}
	}
	rows := map[string][]string{}
	for gate, right := range columns {
		ids := map[int]bool{}
		for _, match := range regexp.MustCompile(`FS-([0-9]+) to FS-([0-9]+)`).FindAllStringSubmatch(right, -1) {
			from, _ := strconv.Atoi(match[1])
			to, _ := strconv.Atoi(match[2])
			for id := from; id <= to; id += 1 {
				ids[id] = true
			}
		}
		for _, match := range regexp.MustCompile(`FS-([0-9]+)`).FindAllStringSubmatch(right, -1) {
			id, _ := strconv.Atoi(match[1])
			ids[id] = true
		}
		sorted := []int{}
		for id := range ids {
			sorted = append(sorted, id)
		}
		slices.Sort(sorted)
		for _, id := range sorted {
			rows[gate] = append(rows[gate], "FS-"+strconv.Itoa(id))
		}
	}
	return rows
}

// gates.yml lists every FS row §23.6 names at each gate and no other, so the
// report runs and shows what the design asks at that gate.
func TestGatesListEverySection236Row(t *testing.T) {
	want := section236Rows(t)
	gates := &gatesFile{}
	if err := readYamlFile("gates.yml", gates); err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{"G0", "M0", "M1", "M2", "M3", "M4", "M5", "M6", "M7", "M8", "M9", "M10", "every", "next"} {
		listed := map[string]bool{}
		for _, row := range gates.Gates[gate] {
			if row.Row != "" {
				listed[row.Row] = true
			}
		}
		got := []string{}
		for row := range listed {
			got = append(got, row)
		}
		slices.SortFunc(got, func(a string, b string) int {
			numberA, _ := strconv.Atoi(strings.TrimPrefix(a, "FS-"))
			numberB, _ := strconv.Atoi(strings.TrimPrefix(b, "FS-"))
			return numberA - numberB
		})
		if !slices.Equal(got, want[gate]) && (len(got) != 0 || len(want[gate]) != 0) {
			t.Errorf("%s lists %v, and §23.6 names %v", gate, got, want[gate])
		}
	}
	if len(want["M2"]) < 40 || !slices.Contains(want["M1"], "FS-54") || !slices.Contains(want["every"], "FS-55") {
		t.Fatalf("the table read wrong: %v", want)
	}
}

// Every test the real files name is spelled as a test, so a typo reads missing
// in a report rather than vanishing from it.
func TestTheRealFilesNameTestsAsTests(t *testing.T) {
	gates := &gatesFile{}
	if err := readYamlFile("gates.yml", gates); err != nil {
		t.Fatal(err)
	}
	matrix := &matrixFile{}
	if err := readYamlFile("ops-matrix.yml", matrix); err != nil {
		t.Fatal(err)
	}
	tests := []gateTest{}
	for _, rows := range gates.Gates {
		for _, row := range rows {
			tests = append(tests, row.Tests...)
		}
	}
	for _, entry := range matrix.Ids {
		tests = append(tests, entry.Tests...)
	}
	name := regexp.MustCompile(`^(Test|Fuzz)[A-Z][A-Za-z0-9]*$`)
	for _, test := range tests {
		if test.Repo == "" || test.Package == "" || len(test.Names) == 0 {
			t.Errorf("an entry names no repo, package or test: %+v", test)
		}
		for _, testName := range test.Names {
			if !name.MatchString(testName) {
				t.Errorf("%q is not a test's name", testName)
			}
		}
	}
}

// A fixture test that passes only when its binary runs under the -timeout its -fixture.timeout names, so a row's
// outcome says which bound the gate ran it under.
func writeTimeoutFixture(t *testing.T, options *gateOptions) {
	t.Helper()
	content := "package widget\n\nimport (\n\t\"flag\"\n\t\"testing\"\n)\n\n" +
		"var timeout = flag.String(\"fixture.timeout\", \"\", \"the -timeout the gate runs this binary under\")\n\n" +
		"func TestRunsUnderItsTimeout(t *testing.T) {\n" +
		"\tif got := flag.Lookup(\"test.timeout\").Value.String(); got != *timeout {\n" +
		"\t\tt.Fatalf(\"test.timeout %s, want %s\", got, *timeout)\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(options.root, "fixture", "widget", "timeout_test.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An entry's own timeout is the -timeout its go test runs under (QD-1058): the fixture's test passes only when its
// binary got the entry's 7m, not the settings' 40 minutes.
func TestGateEntryRunsUnderItsOwnTimeout(t *testing.T) {
	gates := `gates:
  M3:
    - row: FS-940
      tests:
        - {repo: fixture, package: ./widget, timeout: 7m, names: [TestRunsUnderItsTimeout], args: ["-fixture.timeout=7m0s"]}
`
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M3"
	writeTimeoutFixture(t, options)
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.rows) != 1 || report.rows[0].outcome != gatePass {
		for _, row := range report.rows {
			t.Logf("%s: %s: %s", row.label, row.outcome, strings.Join(row.details, "; "))
		}
		t.Fatal("an entry's own timeout is not the -timeout its test ran under")
	}
}

// An entry that names no timeout runs under the settings' TestTimeout, the 40 minutes every entry but the release
// fixture keeps (QD-1058).
func TestGateEntryWithoutATimeoutRunsUnderTheDefault(t *testing.T) {
	settings := defaultGateSettings()
	if settings.TestTimeout != 40*time.Minute {
		t.Fatalf("the default bound is %s, not 40 minutes", settings.TestTimeout)
	}
	gates := `gates:
  M3:
    - row: FS-941
      tests:
        - {repo: fixture, package: ./widget, names: [TestRunsUnderItsTimeout], args: ["-fixture.timeout=` + settings.TestTimeout.String() + `"]}
`
	options := writeGateFixture(t, gates, "ids: {}\n")
	options.gate = "M3"
	writeTimeoutFixture(t, options)
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.rows) != 1 || report.rows[0].outcome != gatePass {
		for _, row := range report.rows {
			t.Logf("%s: %s: %s", row.label, row.outcome, strings.Join(row.details, "; "))
		}
		t.Fatal("an entry with no timeout did not run under the settings' bound")
	}
}

// The command running an entry lives the entry's own timeout and the grace, not the settings' bound: a stores
// entry whose wrapper ignores go test's -timeout and never returns on its own is killed at the entry's bound, and
// the report says the bound ended it.
func TestGateEntryTimeoutBoundsTheCommandRunningIt(t *testing.T) {
	options := writeGateFixture(t, "gates: {}\n", "ids: {}\n")
	// only a kill ends this wrapper before its sleep does, and that sleep outlasts the caller's bound below, so
	// nothing of it outlives the test by long even if the kill never comes
	wrapper := filepath.Join(options.root, "hangs.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec sleep 120\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	options.environ = append(options.environ, gateStoreWrapperVariable+"="+wrapper)
	options.settings.TestTimeoutGrace = 0
	// the caller's bound ends a run the entry's did not
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	entry := gateTest{Repo: "fixture", Package: "./widget", Stores: true, Timeout: gateTimeout(100 * time.Millisecond), Names: []string{"TestPasses"}}
	outcome, details := runGateTest(ctx, options, &gateReport{}, entry)
	if ctx.Err() != nil {
		t.Fatal("the run outlived its entry's bound, and only the caller's ended it")
	}
	joined := strings.Join(details, "\n")
	if outcome != gateFail || !strings.Contains(joined, "the run outlived its bound, 100ms and the 0s grace after it, and was killed") {
		t.Fatalf("a run its bound killed: %s:\n%s", outcome, joined)
	}
}

// A timeout that is no Go duration longer than zero, written with its unit, is refused when gates.yml loads, never
// run under the default or under some other bound; one that is loads as its duration.
func TestGateRefusesAMalformedTimeoutWhenGatesYmlLoads(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	writeGates := func(timeout string) {
		gates := "gates:\n  M1:\n    - row: FS-950\n      tests:\n        - {repo: fixture, package: ./widget, timeout: " + timeout + ", names: [TestPasses]}\n"
		if err := os.WriteFile(options.gatesPath, []byte(gates), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, timeout := range []string{"10800", "0", "1.5", "true", `"180"`, "3 hours", "3H", "3h junk", `""`, "0s", "-1h", "[3h]", "{hours: 3}"} {
		writeGates(timeout)
		if _, err := runGate(context.Background(), options); err == nil || !strings.Contains(err.Error(), "gates.yml") || !strings.Contains(err.Error(), "timeout") {
			t.Errorf("timeout %s: %v", timeout, err)
		}
	}
	for timeout, want := range map[string]time.Duration{"3h": 3 * time.Hour, "3h0m0s": 3 * time.Hour, "90m": 90 * time.Minute, `"1h30m"`: 90 * time.Minute} {
		writeGates(timeout)
		gates := &gatesFile{}
		if err := readYamlFile(options.gatesPath, gates); err != nil {
			t.Errorf("timeout %s: %v", timeout, err)
			continue
		}
		if got := time.Duration(gates.Gates["M1"][0].Tests[0].Timeout); got != want {
			t.Errorf("timeout %s loads as %s, want %s", timeout, got, want)
		}
	}
}

// The tool runs gates.yml's release fixture under 3h, as run.sh's BUILD_TEST runs it (QD-1056, QD-1058), and every
// other entry it reads, in gates.yml, ops-matrix.yml, trace.yml and stages.yml, under the settings' bound.
func TestGatesYmlGivesOnlyTheReleaseFixtureItsOwnTimeout(t *testing.T) {
	gates := &gatesFile{}
	if err := readYamlFile("gates.yml", gates); err != nil {
		t.Fatal(err)
	}
	matrix := &matrixFile{}
	if err := readYamlFile("ops-matrix.yml", matrix); err != nil {
		t.Fatal(err)
	}
	trace := &traceFile{}
	if err := readStrictYamlFile("trace.yml", trace); err != nil {
		t.Fatal(err)
	}
	stages := &stagesFile{}
	if err := readStrictYamlFile("stages.yml", stages); err != nil {
		t.Fatal(err)
	}
	tests := []gateTest{}
	for _, rows := range gates.Gates {
		for _, row := range rows {
			tests = append(tests, row.Tests...)
		}
	}
	for _, entry := range matrix.Ids {
		tests = append(tests, entry.Tests...)
	}
	var collect func(evidence *traceEvidence)
	collect = func(evidence *traceEvidence) {
		if evidence == nil {
			return
		}
		tests = append(tests, evidence.Tests...)
		collect(evidence.Sidecar)
		collect(evidence.Server)
	}
	for _, row := range trace.Rows {
		collect(&row.traceEvidence)
	}
	for _, stage := range stages.Stages {
		for _, item := range stage.Items {
			collect(&item.traceEvidence)
		}
	}
	settings := defaultGateSettings()
	fixtures := 0
	for _, test := range tests {
		got := test.testTimeout(settings)
		if slices.Contains(test.Names, "TestSidecarReleaseFixture") {
			fixtures += 1
			if got != 3*time.Hour || test.Tags != "sidecar_release_fixture" {
				t.Errorf("the release fixture runs under %s with tags %q", got, test.Tags)
			}
		} else if got != settings.TestTimeout {
			t.Errorf("%s %s's %v runs under %s", test.Repo, test.Package, test.Names, got)
		}
	}
	if fixtures != 1 || len(tests) < 100 {
		t.Fatalf("the files name the release fixture %d times among %d entries", fixtures, len(tests))
	}
}

// -list runs no matrix command either (QD-1082): an id whose command would fail, and would leave a file behind, if
// run reads listed with the file never written, as a row's command does, and a test beside it still resolves from
// its package's files.
func TestGateListRunsNoMatrixCommand(t *testing.T) {
	matrix := `ids:
  R-OPS-9:
    title: a command that must not run in a list
    commands:
      - {repo: fixture, command: [sh, -c, "touch ran-in-a-list && exit 1"]}
  D-OPS-6:
    title: a command beside a declared test
    tests:
      - {repo: fixture, package: ./widget, names: [TestPasses]}
    commands:
      - {repo: fixture, dir: widget, command: [sh, -c, "touch ran-in-a-list && exit 1"]}
`
	gates := `gates:
  M1:
    - item: "a row whose command must not run in a list"
      commands:
        - {repo: fixture, command: [sh, -c, "touch ran-in-a-list && exit 1"]}
`
	options := writeGateFixture(t, gates, matrix)
	options.list = true
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(options.root, "fixture"), filepath.Join(options.root, "fixture", "widget")} {
		if _, err := os.Stat(filepath.Join(directory, "ran-in-a-list")); err == nil {
			t.Errorf("a list ran a command in %s", directory)
		}
	}
	got := gateOutcomes(report.matrix)
	for label, outcome := range map[string]string{
		"R-OPS-9 a command that must not run in a list": gateListed,
		"D-OPS-6 a command beside a declared test":      gateListed,
	} {
		if got[label] != outcome {
			t.Errorf("%s: %s, want %s", label, got[label], outcome)
		}
	}
	for _, result := range report.matrix {
		if !strings.Contains(strings.Join(result.details, "\n"), "not run in list mode") {
			t.Errorf("%s does not say its command was not run: %v", result.label, result.details)
		}
	}
	if rows := gateOutcomes(report.rows); rows["a row whose command must not run in a list"] != gateListed {
		t.Errorf("the row's command: %v", rows)
	}
	if report.failed() {
		t.Error("a list run failed")
	}
}
