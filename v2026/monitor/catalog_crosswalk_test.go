package monitor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var numberedCatalogHeadingPattern = regexp.MustCompile(`(?m)^### ([0-9]+(?:\.[0-9]+[a-z]?)) .+$`)
var numberedCoverageRowPattern = regexp.MustCompile(`(?m)^\| ([0-9]+(?:\.[0-9]+[a-z]?)) \| (Runbook|Shared contract|Coverage gap) \| (.+) \|$`)
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

	headings := numberedCatalogHeadingPattern.FindAllStringSubmatchIndex(catalog, -1)
	seen := map[string]bool{}
	for index, heading := range headings {
		number := catalog[heading[2]:heading[3]]
		if seen[number] {
			t.Fatalf("SIGNALS.md repeats numbered heading §%s", number)
		}
		seen[number] = true
		end := len(catalog)
		if index+1 < len(headings) {
			end = headings[index+1][0]
		}
		section := catalog[heading[1]:end]
		hasProbe := catalogProbePattern.MatchString(section)
		_, hasCoverage := rows[number]
		switch {
		case hasProbe && hasCoverage:
			t.Errorf("implemented §%s also appears in the non-Probe coverage crosswalk", number)
		case !hasProbe && !hasCoverage:
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
