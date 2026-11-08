// A gate's report (OPS-M1-07; owner decision 194): each milestone's local checks
// run and reported, with what needs hardware, production or people listed open,
// and FS-55 and the dump checks at every Gn-out. Run from a server checkout in
// the monoroot:
//
//	go run ./local/sidecar-gate [-base sidecar] [-out report.md] M2
//
// It reads gates.yml for the gate's §23.6 rows and ops-matrix.yml for §19's ids,
// runs each row's direct go test on its package, through the private-store
// wrapper SIDECAR_GATE_STORE_WRAPPER names when the test needs stores, and runs
// check-tables and each repo's commit-msg scan. It records the commit set the
// run saw, repo by repo. Each row reports pass, fail, missing or open: missing
// means no test exists yet, which is never a pass, so nothing looks finished
// before it is.
//
// It never takes tests/network-intensive-suite-lock.sh, never runs the
// whole-suite server/test.sh or connect/test.sh, never sets
// URNETWORK_TEST_REAL_VAULT, and reads nothing under vault: its git metadata
// included, since a commit there is the owner's to record.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The repos a commit set records, in §4.8's merge order. vault is not read.
var gateRepos = []string{"connect", "sidecar", "sdk", "sn", "server", "warp", "config", "xops", "mmm", "minio", "build"}

// Where the store wrapper's path comes from: the environment, never this repo.
const gateStoreWrapperVariable = "SIDECAR_GATE_STORE_WRAPPER"

// The tunables of a gate run.
type gateSettings struct {
	// The bound on one go test or command.
	TestTimeout time.Duration
}

func defaultGateSettings() *gateSettings {
	return &gateSettings{
		TestTimeout: 40 * time.Minute,
	}
}

// One go test on one package, as gates.yml and ops-matrix.yml write it.
type gateTest struct {
	Repo    string   `yaml:"repo"`
	Dir     string   `yaml:"dir"`
	Package string   `yaml:"package"`
	Names   []string `yaml:"names"`
	Tags    string   `yaml:"tags"`
	Stores  bool     `yaml:"stores"`
	Args    []string `yaml:"args"`
}

// One non-Go check run in a repo's directory.
type gateCommand struct {
	Repo    string   `yaml:"repo"`
	Dir     string   `yaml:"dir"`
	Command []string `yaml:"command"`
}

// One row of a gate: an FS row and case, or a named item, with what shows it.
type gateRow struct {
	Row      string     `yaml:"row"`
	Case     string     `yaml:"case"`
	Item     string     `yaml:"item"`
	Tests    []gateTest `yaml:"tests"`
	External string     `yaml:"external"`
}

func (self *gateRow) label() string {
	label := self.Row
	if self.Item != "" {
		label = self.Item
	}
	if self.Case != "" {
		label += " (" + self.Case + ")"
	}
	return label
}

type gatesFile struct {
	Gates map[string][]gateRow `yaml:"gates"`
}

// One id of ops-matrix.yml.
type matrixEntry struct {
	Title    string        `yaml:"title"`
	Rows     []string      `yaml:"rows"`
	Tests    []gateTest    `yaml:"tests"`
	Commands []gateCommand `yaml:"commands"`
	External string        `yaml:"external"`
}

type matrixFile struct {
	Ids map[string]*matrixEntry `yaml:"ids"`
}

// The outcomes a row or an id reports.
const (
	gatePass    = "pass"
	gateFail    = "fail"
	gateMissing = "missing"
	gateOpen    = "open"
)

// What one row or id reported, and why.
type gateResult struct {
	label   string
	outcome string
	details []string
}

// What a whole run found.
type gateReport struct {
	gate      string
	base      string
	group     string
	commits   []gateCommit
	rows      []*gateResult
	checks    []*gateResult
	matrix    []*gateResult
	startTime time.Time
}

// One repo's commit as the run saw it.
type gateCommit struct {
	repo   string
	branch string
	commit string
	note   string
}

// What a run needs.
type gateOptions struct {
	gate           string
	base           string
	root           string
	serverRelative string
	gatesPath      string
	matrixPath     string
	repos          []string
	environ        []string
	settings       *gateSettings
	// runs the commit-msg scans and check-tables; off in a fixture
	repoChecks bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// 0 nothing failed, 1 something failed or could not run, 64 usage. Missing and
// open rows do not fail the run: owner decision 194 keeps the build moving past
// a gate, and the report says what stayed missing and open.
func run(args []string, out io.Writer, errOut io.Writer) int {
	flags := flag.NewFlagSet("sidecar-gate", flag.ContinueOnError)
	flags.SetOutput(errOut)
	base := flags.String("base", "sidecar", "N's ref for FS-55: built as a release leg it is N, and with the gate's groups promoted N+1")
	outPath := flags.String("out", "", "where the Markdown report goes; default standard output")
	serverDirectory := flags.String("server", ".", "the server checkout, inside the monoroot")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		fmt.Fprintf(errOut, "usage: sidecar-gate [-base <ref>] [-out <file>] [-server <dir>] <gate>\n")
		return 64
	}
	absoluteServer, err := filepath.Abs(*serverDirectory)
	if err != nil {
		fmt.Fprintf(errOut, "sidecar-gate: %s\n", err)
		return 1
	}
	report, err := runGate(context.Background(), &gateOptions{
		gate:           flags.Arg(0),
		base:           *base,
		root:           filepath.Dir(absoluteServer),
		serverRelative: filepath.Base(absoluteServer),
		gatesPath:      filepath.Join(absoluteServer, "local", "sidecar-gate", "gates.yml"),
		matrixPath:     filepath.Join(absoluteServer, "local", "sidecar-gate", "ops-matrix.yml"),
		repos:          gateRepos,
		environ:        os.Environ(),
		settings:       defaultGateSettings(),
		repoChecks:     true,
	})
	if err != nil {
		fmt.Fprintf(errOut, "sidecar-gate: %s\n", err)
		return 1
	}
	var buffer bytes.Buffer
	report.Write(&buffer)
	if *outPath == "" {
		out.Write(buffer.Bytes())
	} else if err := os.WriteFile(*outPath, buffer.Bytes(), 0o644); err != nil {
		fmt.Fprintf(errOut, "sidecar-gate: %s\n", err)
		return 1
	}
	if report.failed() {
		return 1
	}
	return 0
}

// The migration group a gate's Gn-out registers, "" for a gate with none.
func gateGroup(gate string) string {
	switch gate {
	case "M0", "M1", "M2", "M5", "M6", "M7", "M8", "M9":
		return "MG" + strings.TrimPrefix(gate, "M")
	}
	return ""
}

func readYamlFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// Runs a gate's rows, its every-Gn-out rows, the repo checks and the matrix.
func runGate(ctx context.Context, options *gateOptions) (*gateReport, error) {
	gates := &gatesFile{}
	if err := readYamlFile(options.gatesPath, gates); err != nil {
		return nil, err
	}
	rows, ok := gates.Gates[options.gate]
	if !ok {
		names := []string{}
		for name := range gates.Gates {
			names = append(names, name)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("gates.yml holds no gate %q; it holds %s", options.gate, strings.Join(names, ", "))
	}
	matrix := &matrixFile{}
	if err := readYamlFile(options.matrixPath, matrix); err != nil {
		return nil, err
	}
	report := &gateReport{gate: options.gate, base: options.base, group: gateGroup(options.gate), startTime: time.Now().UTC()}
	report.commits = readCommitSet(options.root, options.repos)

	// every Gn-out also runs FS-55, and FS-54 again, since a later group must
	// still leave a release leg with main's migrations alone (§23.6; OPS-M1-07)
	everyRows := []gateRow{}
	if options.gate != "every" && strings.HasPrefix(options.gate, "M") {
		everyRows = slices.Clone(gates.Gates["every"])
		if options.gate != "M1" {
			for _, row := range gates.Gates["M1"] {
				if row.Row == "FS-54" {
					everyRows = append(everyRows, row)
				}
			}
		}
	}
	for _, row := range append(slices.Clone(rows), everyRows...) {
		report.rows = append(report.rows, runGateRow(ctx, options, report, &row))
	}
	if options.repoChecks {
		report.checks = runRepoChecks(ctx, options)
	}
	ids := []string{}
	for id := range matrix.Ids {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, compareOpsIds)
	for _, id := range ids {
		report.matrix = append(report.matrix, runMatrixEntry(ctx, options, report, id, matrix.Ids[id]))
	}
	return report, nil
}

// R-OPS before D-OPS, each by its number.
func compareOpsIds(a string, b string) int {
	parse := func(id string) (string, int) {
		prefix, number, _ := strings.Cut(strings.TrimPrefix(id, "R-"), "-")
		value := 0
		fmt.Sscan(number, &value)
		if strings.HasPrefix(id, "R-") {
			return "0" + prefix, value
		}
		return "1" + prefix, value
	}
	prefixA, numberA := parse(a)
	prefixB, numberB := parse(b)
	if prefixA != prefixB {
		return strings.Compare(prefixA, prefixB)
	}
	return numberA - numberB
}

// Each repo's checked-out branch and commit; vault is never read.
func readCommitSet(root string, repos []string) []gateCommit {
	commits := []gateCommit{}
	for _, repo := range repos {
		commit := gateCommit{repo: repo}
		directory := filepath.Join(root, repo)
		if _, err := os.Stat(directory); err != nil {
			commit.note = "not checked out here"
			commits = append(commits, commit)
			continue
		}
		head, err := exec.Command("git", "-C", directory, "rev-parse", "HEAD").Output()
		if err != nil {
			commit.note = "not a git checkout"
			commits = append(commits, commit)
			continue
		}
		commit.commit = strings.TrimSpace(string(head))
		branch, err := exec.Command("git", "-C", directory, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err == nil {
			commit.branch = strings.TrimSpace(string(branch))
		}
		commits = append(commits, commit)
	}
	return commits
}

// A row's outcome from its tests: every test must pass; a row with none is
// missing; a row waiting on the world is open.
func runGateRow(ctx context.Context, options *gateOptions, report *gateReport, row *gateRow) *gateResult {
	result := &gateResult{label: row.label()}
	if row.External != "" && len(row.Tests) == 0 {
		result.outcome = gateOpen
		result.details = append(result.details, "waits on "+row.External)
		return result
	}
	if len(row.Tests) == 0 {
		result.outcome = gateMissing
		result.details = append(result.details, "no test shows it yet")
		return result
	}
	outcomes := []string{}
	for _, test := range row.Tests {
		outcome, details := runGateTest(ctx, options, report, test)
		outcomes = append(outcomes, outcome)
		result.details = append(result.details, details...)
	}
	result.outcome = combineOutcomes(outcomes)
	if row.External != "" {
		result.details = append(result.details, "also waits on "+row.External)
		if result.outcome == gatePass {
			result.outcome = gateOpen
		}
	}
	return result
}

// fail beats missing beats open beats pass: no part of a row passes for the rest.
func combineOutcomes(outcomes []string) string {
	for _, outcome := range []string{gateFail, gateMissing, gateOpen} {
		if slices.Contains(outcomes, outcome) {
			return outcome
		}
	}
	if len(outcomes) == 0 {
		return gateMissing
	}
	return gatePass
}

// Fills {base} and {group} in a test's arguments.
func expandGateArgs(args []string, report *gateReport) []string {
	expanded := []string{}
	for _, arg := range args {
		arg = strings.ReplaceAll(arg, "{base}", report.base)
		arg = strings.ReplaceAll(arg, "{group}", report.group)
		expanded = append(expanded, arg)
	}
	return expanded
}

// Runs one test entry and reads each name's verdict from go test -v: PASS
// passes, FAIL fails, SKIP is open, and a name with no verdict at all does not
// exist, which is missing.
func runGateTest(ctx context.Context, options *gateOptions, report *gateReport, test gateTest) (string, []string) {
	where := test.Repo + "/" + filepath.ToSlash(filepath.Join(test.Dir, test.Package))
	if len(test.Names) == 0 {
		return gateMissing, []string{where + " names no test"}
	}
	for _, arg := range test.Args {
		if strings.Contains(arg, "{group}") && report.group == "" {
			return gateOpen, []string{where + ": the gate registers no migration group, so there is no group to check"}
		}
	}
	quoted := []string{}
	for _, name := range test.Names {
		quoted = append(quoted, regexp.QuoteMeta(name))
	}
	goArgs := []string{"-count=1", "-v", "-timeout", options.settings.TestTimeout.String(), "-run", "^(" + strings.Join(quoted, "|") + ")$"}
	if test.Tags != "" {
		goArgs = append(goArgs, "-tags", test.Tags)
	}
	goArgs = append(goArgs, test.Package)
	goArgs = append(goArgs, expandGateArgs(test.Args, report)...)

	directory := filepath.Join(options.root, test.Repo, test.Dir)
	var command *exec.Cmd
	testCtx, cancel := context.WithTimeout(ctx, options.settings.TestTimeout+time.Minute)
	defer cancel()
	if test.Stores {
		wrapper := gateLookup(options.environ, gateStoreWrapperVariable)
		if wrapper == "" {
			return gateOpen, []string{where + ": needs the private stores, and " + gateStoreWrapperVariable + " names no wrapper"}
		}
		command = exec.CommandContext(testCtx, wrapper, append([]string{directory}, goArgs...)...)
	} else {
		command = exec.CommandContext(testCtx, "go", append([]string{"test"}, goArgs...)...)
	}
	command.Dir = directory
	// each repo's tests run as that repo runs them: a GOFLAGS the caller carries,
	// lowgo's -trimpath among them, would move the paths tests find their files by
	command.Env = gateChildEnv(options.environ)
	output, runErr := command.CombinedOutput()

	verdicts := map[string]string{}
	for _, match := range regexp.MustCompile(`(?m)^\s*--- (PASS|FAIL|SKIP): (\S+)`).FindAllStringSubmatch(string(output), -1) {
		verdicts[match[2]] = match[1]
	}
	outcomes := []string{}
	details := []string{}
	for _, name := range test.Names {
		switch verdicts[name] {
		case "PASS":
			outcomes = append(outcomes, gatePass)
			details = append(details, fmt.Sprintf("%s %s: pass", where, name))
		case "FAIL":
			outcomes = append(outcomes, gateFail)
			details = append(details, fmt.Sprintf("%s %s: fail", where, name))
		case "SKIP":
			outcomes = append(outcomes, gateOpen)
			details = append(details, fmt.Sprintf("%s %s: skipped, so not shown", where, name))
		default:
			if runErr != nil && !strings.Contains(string(output), "=== RUN") {
				outcomes = append(outcomes, gateFail)
				details = append(details, fmt.Sprintf("%s %s: did not build or run: %s", where, name, lastLines(string(output), 3)))
			} else {
				outcomes = append(outcomes, gateMissing)
				details = append(details, fmt.Sprintf("%s %s: no such test", where, name))
			}
		}
	}
	return combineOutcomes(outcomes), details
}

// The last lines of a command's output, for a finding.
func lastLines(output string, count int) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if count < len(lines) {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " / ")
}

// The environment a check runs in: the caller's, without GOFLAGS.
func gateChildEnv(environ []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(variable string) bool {
		return strings.HasPrefix(variable, "GOFLAGS=")
	})
}

func gateLookup(environ []string, name string) string {
	for _, variable := range environ {
		if value, ok := strings.CutPrefix(variable, name+"="); ok {
			return value
		}
	}
	return ""
}

// Runs one non-Go check: exit 0 passes, skipped tests read open.
func runGateCommand(ctx context.Context, options *gateOptions, command gateCommand) (string, []string) {
	where := command.Repo + "/" + command.Dir + ": " + strings.Join(command.Command, " ")
	if len(command.Command) == 0 {
		return gateMissing, []string{where + " names no command"}
	}
	commandCtx, cancel := context.WithTimeout(ctx, options.settings.TestTimeout)
	defer cancel()
	process := exec.CommandContext(commandCtx, command.Command[0], command.Command[1:]...)
	process.Dir = filepath.Join(options.root, command.Repo, command.Dir)
	process.Env = gateChildEnv(options.environ)
	output, err := process.CombinedOutput()
	// a host without a module the check imports cannot run it, which says nothing
	// of what it checks
	if match := regexp.MustCompile(`ModuleNotFoundError: No module named '([^']+)'`).FindStringSubmatch(string(output)); match != nil {
		return gateOpen, []string{where + ": this host lacks the Python module " + match[1] + ", so it did not run"}
	}
	if err != nil {
		return gateFail, []string{where + ": " + lastLines(string(output), 3)}
	}
	if match := regexp.MustCompile(`skipped=([0-9]+)`).FindStringSubmatch(string(output)); match != nil {
		return gateOpen, []string{where + ": passed with " + match[1] + " skipped, so not all shown"}
	}
	return gatePass, []string{where + ": pass"}
}

// The checks every gate runs beside its rows: check-tables, and each repo's
// commit-msg scan of main..sidecar.
func runRepoChecks(ctx context.Context, options *gateOptions) []*gateResult {
	results := []*gateResult{}
	tables := &gateResult{label: "check-tables"}
	outcome, details := runGateCommand(ctx, options, gateCommand{Repo: options.serverRelative, Command: []string{"go", "run", "./local/sidecar-migrations", "check-tables"}})
	tables.outcome, tables.details = outcome, details
	results = append(results, tables)
	for _, repo := range options.repos {
		result := &gateResult{label: "commit-msg --scan main..sidecar in " + repo}
		hook := filepath.Join(options.root, repo, ".githooks", "commit-msg")
		if _, err := os.Stat(hook); err != nil {
			result.outcome = gateOpen
			result.details = append(result.details, "no .githooks/commit-msg here")
			results = append(results, result)
			continue
		}
		directory := filepath.Join(options.root, repo)
		if exec.Command("git", "-C", directory, "rev-parse", "--verify", "--quiet", "main").Run() != nil ||
			exec.Command("git", "-C", directory, "rev-parse", "--verify", "--quiet", "sidecar").Run() != nil {
			result.outcome = gateOpen
			result.details = append(result.details, "no main or sidecar branch here")
			results = append(results, result)
			continue
		}
		outcome, details := runGateCommand(ctx, options, gateCommand{Repo: repo, Command: []string{hook, "--scan", "main..sidecar"}})
		result.outcome, result.details = outcome, details
		results = append(results, result)
	}
	return results
}

// One id of the matrix: its tests and commands, then what it waits on.
func runMatrixEntry(ctx context.Context, options *gateOptions, report *gateReport, id string, entry *matrixEntry) *gateResult {
	result := &gateResult{label: id + " " + entry.Title}
	outcomes := []string{}
	for _, test := range entry.Tests {
		outcome, details := runGateTest(ctx, options, report, test)
		outcomes = append(outcomes, outcome)
		result.details = append(result.details, details...)
	}
	for _, command := range entry.Commands {
		outcome, details := runGateCommand(ctx, options, command)
		outcomes = append(outcomes, outcome)
		result.details = append(result.details, details...)
	}
	if 0 < len(entry.Rows) {
		result.details = append(result.details, "shown also by "+strings.Join(entry.Rows, ", ")+", reported under their gates")
	}
	switch {
	case len(outcomes) == 0 && entry.External != "":
		result.outcome = gateOpen
	case len(outcomes) == 0:
		result.outcome = gateMissing
	default:
		result.outcome = combineOutcomes(outcomes)
		if result.outcome == gatePass && entry.External != "" {
			result.outcome = gateOpen
		}
	}
	if entry.External != "" {
		result.details = append(result.details, "waits on "+entry.External)
	}
	return result
}

// Whether any row, check or id failed.
func (self *gateReport) failed() bool {
	for _, results := range [][]*gateResult{self.rows, self.checks, self.matrix} {
		for _, result := range results {
			if result.outcome == gateFail {
				return true
			}
		}
	}
	return false
}
