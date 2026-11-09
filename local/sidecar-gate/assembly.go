package main

// D-ARCH-4's assembly check, the row every Gn-in runs (TEST-M1-12; R-ARCH-1, R-ARCH-5, R-PHASE-1, D-PHASE-2,
// D-ARCH-1): the sibling sidecar checkout's design/source copied to a scratch directory and built there with its
// own build.sh, never in the checkout, since the scripts write into asm/v7/ and a track builds from that tree.
// The row passes when:
//
//   - the scratch DESIGN.md equals the checked-in one, byte for byte, so DESIGN.md is what its sources make;
//   - validate.json shows no id, link, owner-decision or section failure;
//   - a fresh run of v7/supersede_grep7.py, its readings carried forward by v7/carry_readings.py, finds no live
//     or gap hit; a hit no one has read reads missing, never pass, since its reading is a person's;
//   - `<me>` appears only where the sections quote PLAN (D-ARCH-1).
//
// It lists the Q- items the sections hold that main does not, the ones added since the last Gn-out merged
// sidecar into main, since a finding that changes a D- item returns as a Q- item (D-PHASE-2) and the review reads
// those. The scripts need python3 and nothing else; a host without it reads open.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The parts of the design the check reads, relative to the sidecar checkout.
const (
	assemblySourcePath   = "design/source"
	assemblyResearchPath = "design/design-research"
	assemblyBuiltName    = "DESIGN.md"
)

// The tunables of an assembly run.
type assemblySettings struct {
	// The bound on the build and on each script.
	StepTimeout time.Duration
	// Where the scratch directory goes; empty takes the system's.
	ScratchParent string
	// The ref the last Gn-out merged sidecar into, for the Q- items added since.
	LastGateRef string
}

func defaultAssemblySettings() *assemblySettings {
	return &assemblySettings{
		StepTimeout: 10 * time.Minute,
		LastGateRef: "main",
	}
}

// What validate.json says of each kind of failure the check reads, by the field paths validate10.py writes.
// section_refs' one unresolved reference is RFC 5246's §7.4.9 inside a code block, by design, so it is not read.
var assemblyValidateFailures = []struct {
	kind string
	path []string
}{
	{kind: "id", path: []string{"ids", "duplicates"}},
	{kind: "id", path: []string{"ids", "defined_outside_own_section"}},
	{kind: "id", path: []string{"ids", "undefined_references"}},
	{kind: "link", path: []string{"r_links", "broken"}},
	{kind: "link", path: []string{"r_links", "unconverted"}},
	{kind: "link", path: []string{"anchors", "toc_missing"}},
	{kind: "link", path: []string{"anchors", "internal_missing"}},
	{kind: "link", path: []string{"anchors", "relative_links_missing"}},
	{kind: "owner-decision", path: []string{"owner_decisions", "uncited_1_145"}},
	{kind: "owner-decision", path: []string{"owner_decisions", "uncited_not_superseded"}},
	{kind: "owner-decision", path: []string{"owner_decisions", "linked_targets_missing"}},
	{kind: "owner-decision", path: []string{"owner_record", "named_but_open"}},
	{kind: "owner-decision", path: []string{"owner_record", "body_named_open"}},
	{kind: "owner-decision", path: []string{"owner_record", "body_named_missing"}},
	{kind: "section", path: []string{"sections_verbatim", "mismatches"}},
	{kind: "section", path: []string{"tables", "bad_rows"}},
	{kind: "section", path: []string{"x_items", "missing"}},
}

// The value at path in a decoded JSON document, or nil.
func jsonAt(document any, path []string) any {
	for _, key := range path {
		object, ok := document.(map[string]any)
		if !ok {
			return nil
		}
		document = object[key]
	}
	return document
}

// The failures validate.json records, each as "<kind>: <field>: <count>".
func assemblyValidateFindings(data []byte) ([]string, error) {
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("validate.json: %w", err)
	}
	findings := []string{}
	for _, failure := range assemblyValidateFailures {
		value := jsonAt(document, failure.path)
		count := 0
		switch typed := value.(type) {
		case nil:
			findings = append(findings, fmt.Sprintf("%s: validate.json holds no %s, so the check cannot read it", failure.kind, strings.Join(failure.path, ".")))
			continue
		case []any:
			count = len(typed)
		case map[string]any:
			count = len(typed)
		case float64:
			count = int(typed)
		}
		if 0 < count {
			findings = append(findings, fmt.Sprintf("%s: %s: %d", failure.kind, strings.Join(failure.path, "."), count))
		}
	}
	return findings, nil
}

// Copies the tree at source to destination, files and directories, following no link.
func copyTree(source string, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case entry.Type().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil
	})
}

// The first line two texts differ on, from 1.
func firstDifferentLine(a []byte, b []byte) int {
	aLines := bytes.Split(a, []byte("\n"))
	bLines := bytes.Split(b, []byte("\n"))
	for index := 0; index < len(aLines) && index < len(bLines); index += 1 {
		if !bytes.Equal(aLines[index], bLines[index]) {
			return index + 1
		}
	}
	return 1 + min(len(aLines), len(bLines))
}

// Runs a command in directory under the step bound, and returns its combined output.
func runAssemblyStep(ctx context.Context, settings *assemblySettings, directory string, name string, arguments ...string) ([]byte, error) {
	stepCtx, cancel := context.WithTimeout(ctx, settings.StepTimeout)
	defer cancel()
	command := exec.CommandContext(stepCtx, name, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err, lastLines(string(output), 4))
	}
	return output, nil
}

// One reading carry_readings.py writes.
type assemblyReading struct {
	Family  string `json:"family"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Reading string `json:"reading"`
	Live    bool   `json:"live"`
	Gap     bool   `json:"gap"`
}

// Lines of the sections holding `<me>` outside a quotation of PLAN: a line that names PLAN, or one under a
// "Departures from PLAN.md" heading, quotes it (D-ARCH-1).
func assemblySpellingFindings(sectionsDirectory string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(sectionsDirectory, "*.md"))
	if err != nil {
		return nil, err
	}
	findings := []string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		departures := false
		for index, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "#") {
				departures = strings.Contains(line, "Departures from PLAN.md")
			}
			if strings.Contains(line, "<me>") && !departures && !strings.Contains(line, "PLAN") {
				findings = append(findings, fmt.Sprintf("%s:%d spells <me> outside a PLAN quotation (D-ARCH-1)", filepath.Base(path), index+1))
			}
		}
	}
	return findings, nil
}

// Every Q- id the sections define, sorted.
func assemblyQuestionIds(sectionFiles map[string][]byte) []string {
	pattern := regexp.MustCompile(`(?m)^\*\*(Q-[A-Z]+-[0-9]+)`)
	ids := []string{}
	for _, data := range sectionFiles {
		for _, match := range pattern.FindAllSubmatch(data, -1) {
			ids = append(ids, string(match[1]))
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// The section files at HEAD and at ref, by name; the ref's are empty when git has no such ref.
func assemblySectionsAt(sidecarDirectory string, ref string) (map[string][]byte, map[string][]byte, error) {
	sectionsDirectory := filepath.Join(sidecarDirectory, filepath.FromSlash(traceSectionsPath))
	paths, err := filepath.Glob(filepath.Join(sectionsDirectory, "*.md"))
	if err != nil {
		return nil, nil, err
	}
	head := map[string][]byte{}
	before := map[string][]byte{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		head[filepath.Base(path)] = data
		output, err := exec.Command("git", "-C", sidecarDirectory, "show", ref+":"+traceSectionsPath+"/"+filepath.Base(path)).Output()
		if err == nil {
			before[filepath.Base(path)] = output
		}
	}
	return head, before, nil
}

// The assembly row's run over the sibling sidecar checkout.
func runAssembly(ctx context.Context, root string, settings *assemblySettings) *gateResult {
	result := &gateResult{label: "D-ARCH-4's assembly check"}
	fail := func(format string, arguments ...any) *gateResult {
		result.outcome = gateFail
		result.details = append(result.details, fmt.Sprintf(format, arguments...))
		return result
	}
	if _, err := exec.LookPath("python3"); err != nil {
		result.outcome = gateOpen
		result.details = append(result.details, "this host lacks python3, which the assembly scripts need, so it did not run")
		return result
	}
	sidecarDirectory := filepath.Join(root, "sidecar")
	source := filepath.Join(sidecarDirectory, filepath.FromSlash(assemblySourcePath))
	if _, err := os.Stat(filepath.Join(source, "build.sh")); err != nil {
		return fail("no design/source/build.sh in the sidecar checkout: %v", err)
	}
	scratch, err := os.MkdirTemp(settings.ScratchParent, "sidecar-gate-assembly-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(scratch)

	// the scratch tree: a copy of design/source, the research sheets and the checkout's other top-level files
	// beside it, as links, so relative links resolve as they do in the checkout
	scratchSource := filepath.Join(scratch, filepath.FromSlash(assemblySourcePath))
	if err := copyTree(source, scratchSource); err != nil {
		return fail("copying design/source: %v", err)
	}
	research := filepath.Join(sidecarDirectory, filepath.FromSlash(assemblyResearchPath))
	if _, err := os.Stat(research); err == nil {
		if err := os.Symlink(research, filepath.Join(scratch, filepath.FromSlash(assemblyResearchPath))); err != nil {
			return fail("%v", err)
		}
	}
	entries, err := os.ReadDir(sidecarDirectory)
	if err != nil {
		return fail("%v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "design" || entry.Name() == assemblyBuiltName || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := os.Symlink(filepath.Join(sidecarDirectory, entry.Name()), filepath.Join(scratch, entry.Name())); err != nil {
			return fail("%v", err)
		}
	}

	built := filepath.Join(scratch, assemblyBuiltName)
	if _, err := runAssemblyStep(ctx, settings, scratch, "sh", filepath.Join(scratchSource, "build.sh"), built); err != nil {
		return fail("the scratch build failed: %v", err)
	}
	outcomes := []string{}
	builtData, err := os.ReadFile(built)
	if err != nil {
		return fail("%v", err)
	}
	checkedIn, err := os.ReadFile(filepath.Join(sidecarDirectory, assemblyBuiltName))
	if err != nil {
		return fail("%v", err)
	}
	if bytes.Equal(builtData, checkedIn) {
		outcomes = append(outcomes, gatePass)
		result.details = append(result.details, "the scratch build equals the checked-in DESIGN.md")
	} else {
		outcomes = append(outcomes, gateFail)
		result.details = append(result.details, fmt.Sprintf("the scratch build differs from the checked-in DESIGN.md from line %d: DESIGN.md is not what its sources make", firstDifferentLine(builtData, checkedIn)))
	}

	validate, err := os.ReadFile(filepath.Join(scratchSource, "asm", "v7", "validate.json"))
	if err != nil {
		return fail("the build wrote no validate.json: %v", err)
	}
	validateFindings, err := assemblyValidateFindings(validate)
	if err != nil {
		return fail("%v", err)
	}
	if len(validateFindings) == 0 {
		outcomes = append(outcomes, gatePass)
		result.details = append(result.details, "validate.json shows no id, link, owner-decision or section failure")
	} else {
		outcomes = append(outcomes, gateFail)
		for _, finding := range validateFindings {
			result.details = append(result.details, "validate.json: "+finding)
		}
	}

	// the superseded-option grep, fresh, with its readings carried forward
	asm := filepath.Join(scratchSource, "asm")
	hits := filepath.Join(scratch, "hits.json")
	readings := filepath.Join(scratch, "readings.json")
	if _, err := runAssemblyStep(ctx, settings, asm, "python3", "-I", "v7/supersede_grep7.py", scratchSource, hits); err != nil {
		return fail("the superseded-option grep failed: %v", err)
	}
	if _, err := runAssemblyStep(ctx, settings, asm, "python3", "-I", "v7/carry_readings.py", hits, "v7/hits_class.json", readings); err != nil {
		return fail("carrying the readings failed: %v", err)
	}
	readingData, err := os.ReadFile(readings)
	if err != nil {
		return fail("%v", err)
	}
	carried := []*assemblyReading{}
	if err := json.Unmarshal(readingData, &carried); err != nil {
		return fail("the carried readings: %v", err)
	}
	unread, live := 0, 0
	for _, reading := range carried {
		switch {
		case reading.Live || reading.Gap:
			live += 1
			result.details = append(result.details, fmt.Sprintf("grep hit %s:%d (%s) reads live or gap: %s", reading.File, reading.Line, reading.Family, reading.Reading))
		case reading.Reading == "UNCLASSIFIED":
			unread += 1
			result.details = append(result.details, fmt.Sprintf("grep hit %s:%d (%s) has no reading", reading.File, reading.Line, reading.Family))
		}
	}
	switch {
	case 0 < live:
		outcomes = append(outcomes, gateFail)
	case 0 < unread:
		outcomes = append(outcomes, gateMissing)
		result.details = append(result.details, fmt.Sprintf("%d grep hits have no reading: a person reads each against owner/DECISIONS.md and adds it to v7/hits_class.json", unread))
	default:
		outcomes = append(outcomes, gatePass)
		result.details = append(result.details, fmt.Sprintf("the superseded-option grep's %d hits each have a reading, none live", len(carried)))
	}

	spellings, err := assemblySpellingFindings(filepath.Join(sidecarDirectory, filepath.FromSlash(traceSectionsPath)))
	if err != nil {
		return fail("%v", err)
	}
	if len(spellings) == 0 {
		outcomes = append(outcomes, gatePass)
	} else {
		outcomes = append(outcomes, gateFail)
		result.details = append(result.details, spellings...)
	}

	head, before, err := assemblySectionsAt(sidecarDirectory, settings.LastGateRef)
	if err != nil {
		return fail("%v", err)
	}
	if len(before) == 0 {
		result.details = append(result.details, fmt.Sprintf("%s holds no sections, so no Q- items are listed as added since the last gate", settings.LastGateRef))
	} else {
		earlier := assemblyQuestionIds(before)
		added := slices.DeleteFunc(assemblyQuestionIds(head), func(id string) bool { return slices.Contains(earlier, id) })
		if len(added) == 0 {
			result.details = append(result.details, fmt.Sprintf("no Q- item added since %s, the last gate's merge", settings.LastGateRef))
		} else {
			result.details = append(result.details, fmt.Sprintf("Q- items added since %s, the last gate's merge, for the review (D-PHASE-2): %s", settings.LastGateRef, strings.Join(added, ", ")))
		}
	}
	result.outcome = combineOutcomes(outcomes)
	return result
}

// `sidecar-gate assembly`: the row's report on its own, exit 0 when it passed.
func writeAssembly(out io.Writer, result *gateResult) {
	fmt.Fprintf(out, "# %s: %s\n\n", result.label, result.outcome)
	for _, detail := range result.details {
		fmt.Fprintf(out, "- %s\n", detail)
	}
}
