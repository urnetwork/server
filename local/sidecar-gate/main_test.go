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
// none, rather than running with an empty group.
func TestGateWithoutAGroupLeavesFs55Open(t *testing.T) {
	options := writeGateFixture(t, fixtureGates, fixtureMatrix)
	options.gate = "M3"
	gates := strings.Replace(fixtureGates, "  M1:", "  M3:", 1)
	if err := os.WriteFile(options.gatesPath, []byte(gates), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runGate(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if outcome := gateOutcomes(report.rows)["FS-907"]; outcome != gateOpen {
		t.Fatalf("FS-907 at a gate with no group reads %s", outcome)
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
