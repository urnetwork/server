package main

import (
	"fmt"
	"io"
	"strings"
)

// The Markdown report, one per gate: the commit set, the gate's rows, the checks
// every gate runs, and §19's ids, each with its outcome, then the counts.

// Counts outcomes by kind.
func countOutcomes(results []*gateResult) map[string]int {
	counts := map[string]int{}
	for _, result := range results {
		counts[result.outcome] += 1
	}
	return counts
}

func writeResults(out io.Writer, heading string, results []*gateResult) {
	fmt.Fprintf(out, "\n## %s\n\n", heading)
	if len(results) == 0 {
		fmt.Fprintf(out, "None.\n")
		return
	}
	fmt.Fprintf(out, "| item | outcome |\n|---|---|\n")
	for _, result := range results {
		fmt.Fprintf(out, "| %s | %s |\n", strings.ReplaceAll(result.label, "|", "\\|"), result.outcome)
	}
	for _, result := range results {
		if len(result.details) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n**%s** (%s)\n\n", result.label, result.outcome)
		for _, detail := range result.details {
			fmt.Fprintf(out, "- %s\n", detail)
		}
	}
}

func (self *gateReport) Write(out io.Writer) {
	fmt.Fprintf(out, "# Gate report: %s\n\n", self.gate)
	fmt.Fprintf(out, "Run %s. FS-55's N is %s", self.startTime.Format("2006-01-02 15:04 UTC"), self.base)
	if self.group != "" {
		fmt.Fprintf(out, ", and the gate's group %s", self.group)
	}
	fmt.Fprintf(out, ". Missing means no test shows the item yet, which is never a pass; open means it waits on hardware, production or people (owner decision 194).\n")

	fmt.Fprintf(out, "\n## Commit set\n\n| repo | branch | commit |\n|---|---|---|\n")
	for _, commit := range self.commits {
		if commit.note != "" {
			fmt.Fprintf(out, "| %s | | %s |\n", commit.repo, commit.note)
			continue
		}
		fmt.Fprintf(out, "| %s | %s | %s |\n", commit.repo, commit.branch, commit.commit)
	}
	fmt.Fprintf(out, "| vault | | not read: its commits are the owner's to record |\n")

	writeResults(out, "§23.6's rows, with every Gn-out's", self.rows)
	writeResults(out, "Checks at every gate", self.checks)
	writeResults(out, "§19's requirements and decisions", self.matrix)

	fmt.Fprintf(out, "\n## Counts\n\n")
	for _, section := range []struct {
		name    string
		results []*gateResult
	}{{name: "rows", results: self.rows}, {name: "checks", results: self.checks}, {name: "ids", results: self.matrix}} {
		counts := countOutcomes(section.results)
		fmt.Fprintf(out, "- %s: %d pass, %d fail, %d missing, %d open\n", section.name, counts[gatePass], counts[gateFail], counts[gateMissing], counts[gateOpen])
	}
	verdict := "nothing failed"
	if self.failed() {
		verdict = "something failed"
	}
	fmt.Fprintf(out, "\n%s; this report does not block the build (owner decision 194).\n", strings.ToUpper(verdict[:1])+verdict[1:])
}
