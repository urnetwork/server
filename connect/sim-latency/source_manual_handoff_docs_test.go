// Operator-facing handoff instructions preserve the unchanged epoch-one
// contract and never advertise a prepared intake release as already live.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The help path exits before credential loading, compilation or lifecycle
// execution. It must not suggest a manual-only bypass of frozen model review.
func TestRunMainHelpPreservesPreparedManualHandoffBoundaries(t *testing.T) {
	content, err := exec.Command("bash", "run-main.sh", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("read-only help failed: %s, %v", content, err)
	}
	for _, required := range []string{"PREPARED / NOT LIVE", "one unchanged", "shared baseline and R=9", "Qwen/Gemma review remains mandatory", "persisted manual handoff stop", "source-first/config-last", "full seven-day", "immutable fresh same-round baseline"} {
		if !strings.Contains(string(content), required) {
			t.Errorf("operator help lost %q", required)
		}
	}
	if strings.Contains(string(content), "isolated three-model Snow review broker") {
		t.Fatal("operator help hard-coded obsolete Snow membership")
	}
}

// The dated prospective plan is additive. The default full-readiness policy,
// historical evidence and unanswered numerical choice cannot be rewritten.
func TestRunMainManualHandoffDocsKeepFrozenAndProspectiveAuthoritySeparate(t *testing.T) {
	content, err := os.ReadFile("RUN-MAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Status: `PREPARED / NOT LIVE`", "Finish epoch one unchanged", "Earlier code inspection and", "never select or approve a winner", "traffic-preservation/1", "production-benefit addendum", "twelve attributed findings", "persisted operator handoff disposition", "no-winner carry-forward", "actual opening after handoff", "INTAKE_OPEN-evaluator_pending", "immutable fresh same-round scoring baseline", "G2 choice is floors-only at 95%", "G7 rule is separately at least 95%", "at least 76%", "expected.minimum_page_coverage_ratio", "expected.minimum_byte_coverage_ratio", "protected, non-rewritable branches", "### Historical independent-source v2 policy"} {
		if !strings.Contains(string(content), required) {
			t.Errorf("manual handoff runbook lost %q", required)
		}
	}
}
