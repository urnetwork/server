package monitor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var numberedCatalogHeadingPattern = regexp.MustCompile(`(?m)^### ([0-9]+(?:\.[0-9]+[a-z]?)?) .+$`)
var numberedCoverageRowPattern = regexp.MustCompile(`(?m)^\| ([0-9]+(?:\.[0-9]+[a-z]?)?) \| (Runbook|Shared contract|Coverage gap) \| (.+) \|$`)
var backtickedSignalKeyPattern = regexp.MustCompile("`([a-z][a-z0-9]*(?:-[a-z0-9]+)*)`")

func TestEveryNumberedCatalogHeadingHasExecutableOrExplicitCoverage(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	coverage := catalogMarkerSection(t, catalog, "<!-- numbered-coverage-start -->", "<!-- numbered-coverage-end -->")

	registered := map[string]bool{}
	for _, signal := range NewSignals() {
		registered[signal.Key()] = true
	}
	rows := map[string]string{}
	kinds := map[string]string{}
	for _, match := range numberedCoverageRowPattern.FindAllStringSubmatch(coverage, -1) {
		number, kind, boundary := match[1], match[2], match[3]
		if _, duplicate := rows[number]; duplicate {
			t.Fatalf("coverage crosswalk repeats §%s", number)
		}
		rows[number] = boundary
		kinds[number] = kind

		keys := backtickedSignalKeyPattern.FindAllStringSubmatch(boundary, -1)
		if len(keys) == 0 {
			t.Errorf("coverage crosswalk §%s names no registered signal", number)
		}
		for _, key := range keys {
			if !registered[key[1]] {
				t.Errorf("coverage crosswalk §%s names unregistered signal %q", number, key[1])
			}
		}
		if kind == "Coverage gap" && !strings.Contains(boundary, "missing") {
			t.Errorf("coverage gap §%s does not name its missing capability", number)
		}
	}

	sections := numberedCatalogSections(catalog)
	seen := map[string]bool{}
	for _, section := range sections {
		number := section.number
		if seen[number] {
			t.Fatalf("SIGNALS.md repeats numbered heading §%s", number)
		}
		seen[number] = true
		_, hasCoverage := rows[number]
		switch numberedCatalogCoverageConflict(section.body, hasCoverage) {
		case "double":
			t.Errorf("implemented §%s also appears in the non-Probe coverage crosswalk", number)
		case "missing":
			t.Errorf("§%s has neither a Probe declaration nor an explicit coverage cross-reference", number)
		}
	}
	for number := range rows {
		if !seen[number] {
			t.Errorf("coverage crosswalk references missing heading §%s", number)
		}
	}

	for number, kind := range kinds {
		if strings.HasPrefix(number, "22.") && number != "22.0" && kind != "Coverage gap" {
			t.Errorf("unimplemented subnet family §%s is mislabeled %q", number, kind)
		}
	}
}

func catalogMarkerSection(t *testing.T, catalog, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(catalog, startMarker)
	end := strings.Index(catalog, endMarker)
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("SIGNALS.md is missing ordered markers %q and %q", startMarker, endMarker)
	}
	return catalog[start : end+len(endMarker)]
}

// Whole-number probe headings are real section boundaries, not part of the prior gap.
type numberedCatalogSection struct {
	number string
	body   string
}

// Preserve heading-local Probe ownership for both whole and dotted catalog numbers.
func numberedCatalogSections(catalog string) []numberedCatalogSection {
	headings := numberedCatalogHeadingPattern.FindAllStringSubmatchIndex(catalog, -1)
	sections := make([]numberedCatalogSection, 0, len(headings))
	for index, heading := range headings {
		end := len(catalog)
		if index+1 < len(headings) {
			end = headings[index+1][0]
		}
		sections = append(sections, numberedCatalogSection{number: catalog[heading[2]:heading[3]], body: catalog[heading[1]:end]})
	}
	return sections
}

// Exactly one executable declaration or explicit non-Probe coverage is required.
func numberedCatalogCoverageConflict(section string, hasCoverage bool) string {
	hasProbe := catalogProbePattern.MatchString(section)
	switch {
	case hasProbe && hasCoverage:
		return "double"
	case !hasProbe && !hasCoverage:
		return "missing"
	default:
		return ""
	}
}

// The whole-number26 probe must not make the genuinely unimplemented25.5 journey executable.
func TestCatalogCrosswalkWholeNumberProbeDoesNotConsumePriorGap(t *testing.T) {
	catalog := "### 25.5 Synthetic customer journey\nStatus: external probe missing.\n\n### 26 Current Proxy OTHER occupancy\nProbe: `contract-stage-inflight`\n"
	sections := numberedCatalogSections(catalog)
	if len(sections) != 2 || sections[0].number != "25.5" || sections[1].number != "26" {
		t.Fatal("whole-number heading was not a distinct catalog boundary")
	}
	if numberedCatalogCoverageConflict(sections[0].body, true) != "" || numberedCatalogCoverageConflict(sections[1].body, false) != "" {
		t.Fatal("probe ownership leaked across numbered section boundaries")
	}
	for _, row := range []string{"| 25.5 | Coverage gap | `proxy-path` leaves the external journey missing |", "| 26 | Shared contract | `contract-stage-inflight` owns current occupancy |"} {
		if len(numberedCoverageRowPattern.FindStringSubmatch(row)) != 4 {
			t.Fatal("whole or dotted numbered coverage row was not recognized")
		}
	}
}

// Whole-number support must retain rejection of double ownership and genuinely uncovered sections.
func TestCatalogCrosswalkWholeNumberCoverageStillFailsClosed(t *testing.T) {
	sections := numberedCatalogSections("### 25.5 Journey\nStatus: missing.\n### 26 Occupancy\nProbe: `contract-stage-inflight`\n")
	if len(sections) != 2 {
		t.Fatal("synthetic numbered catalog was not parsed")
	}
	if numberedCatalogCoverageConflict(sections[1].body, true) != "double" {
		t.Fatal("whole-number Probe plus non-Probe crosswalk was accepted")
	}
	if numberedCatalogCoverageConflict(sections[0].body, false) != "missing" {
		t.Fatal("unimplemented section without coverage was accepted")
	}
	if numberedCatalogCoverageConflict(sections[1].body, false) != "" || numberedCatalogCoverageConflict(sections[0].body, true) != "" {
		t.Fatal("valid executable and explicit-gap ownership was rejected")
	}
}
