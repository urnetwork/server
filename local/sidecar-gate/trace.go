package main

// The design trace (TEST-M1-12; R-ARCH-1, D-ARCH-2, D-CTX-1, D-CTX-2, D-TRUST-2, R-PHASE-3, R-PHASE-4,
// D-PHASE-1): §23.8's gates read the detail ids, and no other check reads the roll-ups above them, so nothing
// would notice one going unbuilt. trace.yml gives each row of §1.2 (R-CTX, whose Owner column D-CTX-2 makes the
// traceability root), §2.2 (R-TRUST), §2.9 (I-1 to I-20, binding every section by D-TRUST-2), §3.2 (R-ARCH),
// §23.2 (R-PHASE) and the X list its first gate and what shows it: tests, an FS row of gates.yml, a command, or
// open for hardware, production or people. `sidecar-gate trace` reads those tables from the sibling sidecar
// checkout's design sources, and the X list from its DESIGN.md's front matter, so:
//
//   - a table row with no trace entry, or a trace entry for an id no table holds, fails the run: the trace
//     and the design must list the same ids;
//   - a row whose entry names nothing reads missing, and so does one naming a test that does not exist;
//     an open row reads open; nothing reads pass that did not run and pass.
//
// It also holds D-ARCH-2's planes: every external port warp's sidecar blocks open and the subprotocol id the
// sidecar registers must be one §3.5's table carries, or a carriers entry citing an owner decision.
//
// stages.yml lists recURd's RS1 and RS2 and the burner launch BS1 (R-PHASE-4, §23.8's Releases), each with what
// it waits on, and `sidecar-gate RS1` reports a stage whole or names what is missing. A vertical slice counts
// only when its sidecar and server halves both pass (R-PHASE-3). A person signs every stage, so a stage holds a
// people row, which reads open: this reports, and never passes a person's row.
//
// This file carries the plan's COVERAGE.md into the repository, since .stack does not outlive the build.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Where the design's roll-up lists sit, relative to the sidecar checkout.
const (
	traceSectionsPath = "design/source/sections"
	traceDesignPath   = "DESIGN.md"
)

// A roll-up list: its section file, the heading that opens it, and the pattern its rows' ids take.
type traceList struct {
	name    string
	file    string
	heading string
	pattern *regexp.Regexp
}

// The lists trace.yml covers, in the design's order. The X list is the front matter's, which the assembly
// writes from asm/data_front*.py; it is read from DESIGN.md.
var traceLists = []*traceList{
	{name: "§1.2", file: "01-CTX.md", heading: "### 1.2 ", pattern: regexp.MustCompile(`^\| (R-CTX-[0-9]+) \|`)},
	{name: "§2.2", file: "02-TRUST.md", heading: "### 2.2 ", pattern: regexp.MustCompile(`^\| (R-TRUST-[0-9]+) \|`)},
	{name: "§2.9", file: "02-TRUST.md", heading: "### 2.9 ", pattern: regexp.MustCompile(`^- \*\*(I-[0-9]+)[ .(]`)},
	{name: "§3.2", file: "03-ARCH.md", heading: "### 3.2 ", pattern: regexp.MustCompile(`^\| (R-ARCH-[0-9]+) \|`)},
	{name: "§23.2", file: "23-PHASE.md", heading: "### 23.2 ", pattern: regexp.MustCompile(`^\| (R-PHASE-[0-9]+) \|`)},
}

// The X list in DESIGN.md: its header row, and its rows' ids.
var (
	traceXHeader  = "| X | Question | Decision | Owning § | Q id | Residual |"
	traceXPattern = regexp.MustCompile(`^\| (X[0-9]+) \|`)
)

// What shows a roll-up row or a stage item. Tests and commands run as a gate's do; schedules name FS rows, with
// a case as gates.yml writes it ("FS-19 (config-read)"), whose rows in gates.yml run; gates name whole gates of
// gates.yml; open says what outside the build it waits on; out_of_build says, in §1.7's words, that its work is
// out of this build (D-CTX-1). sidecar and server are a vertical slice's halves, each needed (R-PHASE-3).
type traceEvidence struct {
	Task       string         `yaml:"task"`
	Tests      []gateTest     `yaml:"tests"`
	Schedules  []string       `yaml:"schedules"`
	Commands   []gateCommand  `yaml:"commands"`
	Gates      []string       `yaml:"gates"`
	Open       string         `yaml:"open"`
	OutOfBuild string         `yaml:"out_of_build"`
	Sidecar    *traceEvidence `yaml:"sidecar"`
	Server     *traceEvidence `yaml:"server"`
}

// Whether the evidence names nothing that could show anything.
func (self *traceEvidence) empty() bool {
	return self == nil || (len(self.Tests) == 0 && len(self.Schedules) == 0 && len(self.Commands) == 0 && len(self.Gates) == 0 &&
		self.Open == "" && self.OutOfBuild == "" && self.Sidecar == nil && self.Server == nil)
}

// One roll-up id with its first gate and its evidence.
type traceRow struct {
	Id            string `yaml:"id"`
	Gate          string `yaml:"gate"`
	traceEvidence `yaml:",inline"`
}

// A carrier §3.5's table does not hold, allowed by an owner decision (D-ARCH-2): "tcp 2525", "udp 53",
// "subprotocol 21843".
type traceCarrier struct {
	Carrier  string `yaml:"carrier"`
	Decision string `yaml:"decision"`
}

type traceFile struct {
	Rows     []*traceRow     `yaml:"rows"`
	Carriers []*traceCarrier `yaml:"carriers"`
}

// One release stage (R-PHASE-4; §23.8's Releases).
type traceStage struct {
	Id         string       `yaml:"id"`
	Name       string       `yaml:"name"`
	Milestones []string     `yaml:"milestones"`
	Releases   string       `yaml:"releases"`
	Items      []*traceItem `yaml:"items"`
}

// One thing a stage waits on.
type traceItem struct {
	Item          string `yaml:"item"`
	traceEvidence `yaml:",inline"`
}

type stagesFile struct {
	Stages []*traceStage `yaml:"stages"`
}

var traceGatePattern = regexp.MustCompile(`^(G0|M([0-9]|10)(-in)?)$`)

// A YAML file strictly: a field nothing reads, a duplicate key or a second document fails.
func readStrictYamlFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if err := decoder.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s holds a second document", filepath.Base(path))
	}
	return nil
}

// The ids of a list, in the design's order.
func traceListIds(sidecarDirectory string, list *traceList) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(sidecarDirectory, filepath.FromSlash(traceSectionsPath), list.file))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	inside := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "### ") {
			inside = strings.HasPrefix(line, list.heading)
			continue
		}
		if !inside {
			continue
		}
		if match := list.pattern.FindStringSubmatch(line); match != nil {
			ids = append(ids, match[1])
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s holds no %s rows under %q", list.file, list.name, strings.TrimSpace(list.heading))
	}
	return ids, nil
}

// The X list's ids, from DESIGN.md's front matter.
func traceXIds(sidecarDirectory string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(sidecarDirectory, traceDesignPath))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	inside := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == traceXHeader {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		if match := traceXPattern.FindStringSubmatch(line); match != nil {
			ids = append(ids, match[1])
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("DESIGN.md holds no X list under its front matter's table")
	}
	return ids, nil
}

// Every roll-up id the design holds, in order, with the list each sits in.
func traceDesignIds(sidecarDirectory string) ([]string, map[string]string, error) {
	ids := []string{}
	lists := map[string]string{}
	for _, list := range traceLists {
		listIds, err := traceListIds(sidecarDirectory, list)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range listIds {
			ids = append(ids, id)
			lists[id] = list.name
		}
	}
	xIds, err := traceXIds(sidecarDirectory)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range xIds {
		ids = append(ids, id)
		lists[id] = "the X list"
	}
	return ids, lists, nil
}

// What one run of the trace found: each row, the planes, and what makes the trace and the design differ.
type traceReport struct {
	rows     []*gateResult
	planes   *gateResult
	findings []string
	list     bool
	commits  []gateCommit
}

func (self *traceReport) failed() bool {
	if 0 < len(self.findings) || (self.planes != nil && self.planes.outcome == gateFail) {
		return true
	}
	return slices.ContainsFunc(self.rows, func(result *gateResult) bool { return result.outcome == gateFail })
}

// The trace over the monoroot options names: each design row against trace.yml, each row's evidence run, or
// read in -list, and the planes.
func runTrace(ctx context.Context, options *gateOptions, tracePath string) (*traceReport, error) {
	trace := &traceFile{}
	if err := readStrictYamlFile(tracePath, trace); err != nil {
		return nil, err
	}
	gates := &gatesFile{}
	if err := readYamlFile(options.gatesPath, gates); err != nil {
		return nil, err
	}
	sidecarDirectory := filepath.Join(options.root, "sidecar")
	ids, lists, err := traceDesignIds(sidecarDirectory)
	if err != nil {
		return nil, err
	}
	report := &traceReport{list: options.list, commits: readCommitSet(options.root, options.repos)}
	entries := map[string]*traceRow{}
	for _, row := range trace.Rows {
		if _, ok := entries[row.Id]; ok {
			report.findings = append(report.findings, fmt.Sprintf("trace.yml lists %s twice", row.Id))
			continue
		}
		entries[row.Id] = row
		if _, ok := lists[row.Id]; !ok {
			report.findings = append(report.findings, fmt.Sprintf("trace.yml lists %s, which no roll-up list of the design holds", row.Id))
		}
		if row.Gate != "" && !traceGatePattern.MatchString(row.Gate) {
			report.findings = append(report.findings, fmt.Sprintf("%s's first gate %q is no gate", row.Id, row.Gate))
		}
	}
	gateRun := &gateReport{gate: "trace", base: options.base, list: options.list}
	for _, id := range ids {
		row := entries[id]
		if row == nil {
			report.findings = append(report.findings, fmt.Sprintf("%s's %s has no entry in trace.yml: a row the design gains fails until the trace lists it", lists[id], id))
			report.rows = append(report.rows, &gateResult{label: id, outcome: gateMissing, details: []string{"no entry in trace.yml"}})
			continue
		}
		result := runEvidence(ctx, options, gateRun, gates, &row.traceEvidence)
		result.label = id
		if row.Gate != "" {
			result.label += " (first at " + row.Gate + ")"
		}
		report.rows = append(report.rows, result)
	}
	report.planes = runPlanes(options, trace.Carriers)
	return report, nil
}

// One piece of evidence's outcome: every test, schedule, gate and command run, each half of a slice needed,
// open and out-of-build reading open, and nothing named reading missing.
func runEvidence(ctx context.Context, options *gateOptions, report *gateReport, gates *gatesFile, evidence *traceEvidence) *gateResult {
	result := &gateResult{}
	if evidence.empty() {
		result.outcome = gateMissing
		result.details = append(result.details, "its entry names nothing that shows it")
		return result
	}
	if evidence.Task != "" {
		result.details = append(result.details, "shown by "+evidence.Task+"'s evidence")
	}
	outcomes := []string{}
	if 0 < len(evidence.Tests) || 0 < len(evidence.Commands) {
		row := &gateRow{Tests: evidence.Tests, Commands: evidence.Commands}
		rowResult := runGateRow(ctx, options, report, row)
		outcomes = append(outcomes, rowResult.outcome)
		result.details = append(result.details, rowResult.details...)
	}
	for _, schedule := range evidence.Schedules {
		row := traceScheduleRow(gates, schedule)
		if row == nil {
			outcomes = append(outcomes, gateMissing)
			result.details = append(result.details, schedule+": no gate of gates.yml holds it")
			continue
		}
		rowResult := runGateRow(ctx, options, report, row)
		outcomes = append(outcomes, rowResult.outcome)
		result.details = append(result.details, fmt.Sprintf("%s: %s", schedule, rowResult.outcome))
	}
	for _, gate := range evidence.Gates {
		rows, ok := gates.Gates[gate]
		if !ok {
			outcomes = append(outcomes, gateMissing)
			result.details = append(result.details, "gate "+gate+": gates.yml holds no such gate")
			continue
		}
		gateOutcomes := []string{}
		for index := range rows {
			gateOutcomes = append(gateOutcomes, runGateRow(ctx, options, report, &rows[index]).outcome)
		}
		outcome := combineOutcomes(gateOutcomes)
		outcomes = append(outcomes, outcome)
		counts := map[string]int{}
		for _, each := range gateOutcomes {
			counts[each] += 1
		}
		result.details = append(result.details, fmt.Sprintf("gate %s: %s (%d rows: %d pass, %d listed, %d fail, %d missing, %d open)",
			gate, outcome, len(gateOutcomes), counts[gatePass], counts[gateListed], counts[gateFail], counts[gateMissing], counts[gateOpen]))
	}
	for _, half := range []struct {
		name     string
		evidence *traceEvidence
	}{{name: "sidecar", evidence: evidence.Sidecar}, {name: "server", evidence: evidence.Server}} {
		if evidence.Sidecar == nil && evidence.Server == nil {
			break
		}
		halfResult := runEvidence(ctx, options, report, gates, half.evidence)
		if half.evidence == nil {
			halfResult = &gateResult{outcome: gateMissing, details: []string{"no " + half.name + " half: a vertical slice needs both (R-PHASE-3)"}}
		}
		outcomes = append(outcomes, halfResult.outcome)
		result.details = append(result.details, half.name+" half: "+halfResult.outcome)
		for _, detail := range halfResult.details {
			result.details = append(result.details, "  "+detail)
		}
	}
	if evidence.OutOfBuild != "" {
		outcomes = append(outcomes, gateOpen)
		result.details = append(result.details, "out of this build: "+evidence.OutOfBuild)
	}
	if evidence.Open != "" {
		outcomes = append(outcomes, gateOpen)
		result.details = append(result.details, "waits on "+evidence.Open)
	}
	result.outcome = combineOutcomes(outcomes)
	return result
}

// The gates.yml row a schedule names, "FS-n" or "FS-n (case)", from the first gate that holds it.
func traceScheduleRow(gates *gatesFile, schedule string) *gateRow {
	rowId, caseName, _ := strings.Cut(schedule, " (")
	caseName = strings.TrimSuffix(caseName, ")")
	for _, gate := range traceGateOrder(gates) {
		for index := range gates.Gates[gate] {
			row := &gates.Gates[gate][index]
			if row.Row == rowId && row.Case == caseName {
				return row
			}
		}
	}
	return nil
}

// gates.yml's gates in review order, so a schedule reads at its first gate.
func traceGateOrder(gates *gatesFile) []string {
	order := func(gate string) int {
		switch gate {
		case "G0":
			return -1
		case "every":
			return 100
		case "next":
			return 101
		}
		number, inGate := strings.CutSuffix(strings.TrimPrefix(gate, "M"), "-in")
		n, err := strconv.Atoi(number)
		if err != nil {
			return 200
		}
		if inGate {
			return 2 * n
		}
		return 2*n + 1
	}
	names := []string{}
	for gate := range gates.Gates {
		names = append(names, gate)
	}
	slices.SortFunc(names, func(a string, b string) int {
		if order(a) != order(b) {
			return order(a) - order(b)
		}
		return strings.Compare(a, b)
	})
	return names
}

// D-ARCH-2's planes: §3.5's carriers against what the sidecar blocks open and the id the sidecar registers.
func runPlanes(options *gateOptions, carriers []*traceCarrier) *gateResult {
	result := &gateResult{label: "D-ARCH-2's planes"}
	sidecarDirectory := filepath.Join(options.root, "sidecar")
	allowed, err := sectionCarriers(filepath.Join(sidecarDirectory, filepath.FromSlash(traceSectionsPath), "03-ARCH.md"))
	if err != nil {
		result.outcome = gateFail
		result.details = append(result.details, err.Error())
		return result
	}
	decided := map[string]string{}
	decisionPattern := regexp.MustCompile(`\bowner decisions? [0-9]+`)
	for _, carrier := range carriers {
		if decisionPattern.MatchString(carrier.Decision) {
			decided[strings.ToLower(carrier.Carrier)] = carrier.Decision
		} else {
			result.details = append(result.details, fmt.Sprintf("trace.yml's carrier %q cites no owner decision, so it allows nothing", carrier.Carrier))
		}
	}
	found := []string{}
	ports, err := blockExternalPorts(filepath.Join(options.root, "warp", "services", "sidecar-blocks"))
	if err != nil {
		result.outcome = gateFail
		result.details = append(result.details, err.Error())
		return result
	}
	found = append(found, ports...)
	subprotocolId, err := sidecarSubprotocolId(sidecarDirectory)
	if err != nil {
		result.outcome = gateFail
		result.details = append(result.details, err.Error())
		return result
	}
	found = append(found, "subprotocol "+strconv.Itoa(subprotocolId))
	outcomes := []string{}
	for _, carrier := range found {
		switch {
		case allowed[carrier]:
			outcomes = append(outcomes, gatePass)
			result.details = append(result.details, carrier+": in §3.5's table")
		case decided[carrier] != "":
			outcomes = append(outcomes, gatePass)
			result.details = append(result.details, carrier+": outside §3.5's table, allowed by "+decided[carrier])
		default:
			outcomes = append(outcomes, gateFail)
			result.details = append(result.details, carrier+": outside §3.5's table, and no entry cites an owner decision for it (D-ARCH-2)")
		}
	}
	result.outcome = combineOutcomes(outcomes)
	return result
}

// The carriers §3.5's plane table names: each TCP and UDP port of its rows, as "tcp N" and "udp N", HTTPS as
// tcp 443, and each subprotocol id, as "subprotocol N".
func sectionCarriers(sectionPath string) (map[string]bool, error) {
	data, err := os.ReadFile(sectionPath)
	if err != nil {
		return nil, err
	}
	carriers := map[string]bool{}
	inside := false
	numberPattern := regexp.MustCompile(`[0-9]+`)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "### ") {
			inside = strings.HasPrefix(line, "### 3.5 ")
			continue
		}
		if !inside || !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Plane") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		carrier := cells[2] + " " + cells[3]
		if strings.Contains(carrier, "HTTPS") {
			carriers["tcp 443"] = true
		}
		for _, match := range regexp.MustCompile(`\bid ([0-9]+)`).FindAllStringSubmatch(carrier, -1) {
			carriers["subprotocol "+match[1]] = true
		}
		// "TCP 443, 8443, 8883, 5671 and 25 in, 25 out; TCP and UDP 53": each clause's protocols take its numbers
		for _, clause := range strings.Split(cells[2], ";") {
			tcp := strings.Contains(clause, "TCP")
			udp := strings.Contains(clause, "UDP")
			for _, number := range numberPattern.FindAllString(clause, -1) {
				if tcp {
					carriers["tcp "+number] = true
				}
				if udp {
					carriers["udp "+number] = true
				}
			}
		}
	}
	if len(carriers) == 0 {
		return nil, errors.New("§3.5's plane table names no carrier")
	}
	return carriers, nil
}

// The external ports the sidecar's warp blocks open: each tcp_stream_port_services and udp_stream_port_services
// key, and each external_ports or public_ports entry, anywhere in a fragment.
func blockExternalPorts(directory string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "*.yml"))
	if err != nil {
		return nil, err
	}
	ports := []string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		document := &yaml.Node{}
		if err := yaml.Unmarshal(data, document); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		var walk func(node *yaml.Node)
		walk = func(node *yaml.Node) {
			if node.Kind == yaml.MappingNode {
				for index := 0; index+1 < len(node.Content); index += 2 {
					key, value := node.Content[index].Value, node.Content[index+1]
					protocol := ""
					switch key {
					case "tcp_stream_port_services", "external_ports", "public_ports":
						protocol = "tcp"
					case "udp_stream_port_services":
						protocol = "udp"
					}
					if protocol != "" {
						for _, port := range yamlPortNumbers(value) {
							ports = append(ports, protocol+" "+port)
						}
					}
					walk(value)
				}
				return
			}
			for _, child := range node.Content {
				walk(child)
			}
		}
		walk(document)
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

// The port numbers a value holds: a mapping's keys, a sequence's items, or a scalar.
func yamlPortNumbers(node *yaml.Node) []string {
	numbers := []string{}
	add := func(value string) {
		if _, err := strconv.Atoi(value); err == nil {
			numbers = append(numbers, value)
		}
	}
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index < len(node.Content); index += 2 {
			add(node.Content[index].Value)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			add(child.Value)
		}
	case yaml.ScalarNode:
		add(node.Value)
	}
	return numbers
}

// The id the sidecar registers its subprotocol on, read from its SubprotocolId constant.
func sidecarSubprotocolId(sidecarDirectory string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(sidecarDirectory, "*.go"))
	if err != nil {
		return 0, err
	}
	fileSet := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return 0, err
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if name.Name != "SubprotocolId" || len(value.Values) <= index {
						continue
					}
					literal, ok := value.Values[index].(*ast.BasicLit)
					if !ok || literal.Kind != token.INT {
						return 0, fmt.Errorf("%s: SubprotocolId is no integer literal", filepath.Base(path))
					}
					id, err := strconv.ParseInt(literal.Value, 0, 64)
					return int(id), err
				}
			}
		}
	}
	return 0, errors.New("the sidecar declares no SubprotocolId")
}

// The trace's Markdown report.
func (self *traceReport) Write(out io.Writer) {
	fmt.Fprintf(out, "# The design trace\n\n")
	fmt.Fprintf(out, "Each roll-up row of §1.2, §2.2, §2.9, §3.2, §23.2 and the X list, with its first gate and what shows it. Missing means nothing shows it yet, which is never a pass; open means it waits on hardware, production or people, or its work is out of this build.\n")
	if self.list {
		fmt.Fprintf(out, "\nThis is -list: nothing ran. Listed means every test the row names is declared, which is no pass.\n")
	}
	if 0 < len(self.findings) {
		fmt.Fprintf(out, "\n## Where the trace and the design differ\n\n")
		for _, finding := range self.findings {
			fmt.Fprintf(out, "- %s\n", finding)
		}
	}
	fmt.Fprintf(out, "\n## Commit set\n\n| repo | branch | commit |\n|---|---|---|\n")
	for _, commit := range self.commits {
		if commit.note != "" {
			fmt.Fprintf(out, "| %s | | %s |\n", commit.repo, commit.note)
			continue
		}
		fmt.Fprintf(out, "| %s | %s | %s |\n", commit.repo, commit.branch, commit.commit)
	}
	writeResults(out, "Roll-up rows", self.rows)
	if self.planes != nil {
		writeResults(out, "Planes", []*gateResult{self.planes})
	}
	counts := countOutcomes(self.rows)
	fmt.Fprintf(out, "\n## Counts\n\n- rows: %d pass, %d listed, %d fail, %d missing, %d open\n", counts[gatePass], counts[gateListed], counts[gateFail], counts[gateMissing], counts[gateOpen])
	verdict := "Nothing failed"
	if self.failed() {
		verdict = "Something failed"
	}
	fmt.Fprintf(out, "\n%s; this report does not block the build (owner decision 194).\n", verdict)
}

// One stage's report.
type stageReport struct {
	stage   *traceStage
	items   []*gateResult
	list    bool
	commits []gateCommit
}

// Whole only when every item passed; otherwise incomplete, naming each item that did not.
func (self *stageReport) verdict() (bool, []string) {
	short := []string{}
	for _, item := range self.items {
		if item.outcome != gatePass {
			short = append(short, item.label+" ("+item.outcome+")")
		}
	}
	return len(short) == 0, short
}

// A stage of stages.yml, each item run, or read in -list.
func runStage(ctx context.Context, options *gateOptions, stagesPath string, stageId string) (*stageReport, error) {
	stages := &stagesFile{}
	if err := readStrictYamlFile(stagesPath, stages); err != nil {
		return nil, err
	}
	gates := &gatesFile{}
	if err := readYamlFile(options.gatesPath, gates); err != nil {
		return nil, err
	}
	var stage *traceStage
	for _, each := range stages.Stages {
		if each.Id == stageId {
			stage = each
		}
	}
	if stage == nil {
		return nil, fmt.Errorf("stages.yml holds no stage %q", stageId)
	}
	report := &stageReport{stage: stage, list: options.list, commits: readCommitSet(options.root, options.repos)}
	gateRun := &gateReport{gate: stageId, base: options.base, list: options.list}
	for _, item := range stage.Items {
		result := runEvidence(ctx, options, gateRun, gates, &item.traceEvidence)
		result.label = item.Item
		report.items = append(report.items, result)
	}
	return report, nil
}

func (self *stageReport) Write(out io.Writer) {
	fmt.Fprintf(out, "# Stage report: %s, %s\n\n", self.stage.Id, self.stage.Name)
	fmt.Fprintf(out, "Milestones %s. %s\n", strings.Join(self.stage.Milestones, ", "), self.stage.Releases)
	if self.list {
		fmt.Fprintf(out, "\nThis is -list: nothing ran. Listed means every test the item names is declared, which is no pass.\n")
	}
	fmt.Fprintf(out, "\n## Commit set\n\n| repo | branch | commit |\n|---|---|---|\n")
	for _, commit := range self.commits {
		if commit.note != "" {
			fmt.Fprintf(out, "| %s | | %s |\n", commit.repo, commit.note)
			continue
		}
		fmt.Fprintf(out, "| %s | %s | %s |\n", commit.repo, commit.branch, commit.commit)
	}
	writeResults(out, "Items", self.items)
	whole, short := self.verdict()
	if whole {
		fmt.Fprintf(out, "\n%s is whole: every item passed.\n", self.stage.Id)
		return
	}
	fmt.Fprintf(out, "\n%s is incomplete: %s.\n", self.stage.Id, strings.Join(short, "; "))
}
