// Precommitted synthetic examples exercise native representation, not model
// detection. Qualification expectations are never included in provider input.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Positive client/provider/cache/cancellation cases and no-op, benchmark-only,
// work-discard and missing-context controls all use the same favorable score.
func TestWinnerProductionBenefitReviewQualificationCasesKeepLabelsOutOfInput(t *testing.T) {
	content, err := os.ReadFile("testdata/production-benefit-review-qualification-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Schema          int    `json:"schema"`
		Kind            string `json:"kind"`
		AddendumVersion string `json:"addendum_version"`
		Cases           []struct {
			Name                          string `json:"name"`
			ExpectedDecision              string `json:"expected_decision"`
			ExpectedProductionDisposition string `json:"expected_production_disposition"`
			ExpectedWorkloadDisposition   string `json:"expected_workload_disposition"`
			SourceBefore                  string `json:"source_before"`
			SourceAfter                   string `json:"source_after"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(content, &corpus); err != nil || corpus.Schema != 1 || corpus.Kind != "synthetic-production-benefit-review-cases" || corpus.AddendumVersion != server.CompetitionProductionBenefitVersion || len(corpus.Cases) != 8 {
		t.Fatal("qualification corpus identity changed", err)
	}
	wantNames := []string{"production-packet-copy-elimination", "bounded-provider-concurrency-benefits-clients", "immutable-route-cache-preserves-caller-ownership", "ordinary-comment-has-no-production-effect", "benchmark-only-delay-removal", "discarding-useful-client-body-is-not-benefit", "missing-transitive-recovery-contract", "honor-real-client-cancellation"}
	wantDecisions := []string{"approve", "approve", "approve", "disqualify", "disqualify", "disqualify", "uncertain", "approve"}
	wantChecks := []string{"pass", "pass", "pass", "fail", "fail", "fail", "uncertain", "pass"}
	wantWorkloadChecks := []string{"pass", "pass", "pass", "pass", "pass", "fail", "uncertain", "pass"}
	for index, fixture := range corpus.Cases {
		if fixture.Name != wantNames[index] || fixture.ExpectedDecision != wantDecisions[index] || fixture.ExpectedProductionDisposition != wantChecks[index] || fixture.ExpectedWorkloadDisposition != wantWorkloadChecks[index] {
			t.Fatal("qualification control was relabeled", fixture.Name)
		}
		for _, source := range []string{fixture.SourceBefore, fixture.SourceAfter} {
			if _, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", source, parser.AllErrors); err != nil {
				t.Fatal("unparseable synthetic source", fixture.Name, err)
			}
		}
		request, judgment := winnerSnowJudgmentFixture()
		var patch bytes.Buffer
		before, after := strings.Split(strings.TrimSuffix(fixture.SourceBefore, "\n"), "\n"), strings.Split(strings.TrimSuffix(fixture.SourceAfter, "\n"), "\n")
		fmt.Fprintf(&patch, "diff --git a/connect/example.go b/connect/example.go\n--- a/connect/example.go\n+++ b/connect/example.go\n@@ -1,%d +1,%d @@\n", len(before), len(after))
		for _, line := range before {
			fmt.Fprintln(&patch, "-"+line)
		}
		for _, line := range after {
			fmt.Fprintln(&patch, "+"+line)
		}
		request.Packet.PatchPolicy = controller.PatchPolicy{MaxPatchBytes: 16384, AllowedPaths: []string{"connect/example.go"}}
		canonical, failure := controller.ValidateAndCanonicalizePatch(patch.String(), request.Packet.PatchPolicy)
		if failure != nil {
			t.Fatal("qualification patch refused", fixture.Name, failure)
		}
		request.Packet.Patch = canonical.Bytes
		request.Packet.Entry.PatchSha256, request.Template.PatchSha256 = canonical.Sha256, canonical.Sha256
		digest := sha256.Sum256([]byte(fixture.SourceBefore))
		request.SourceFiles = []winnerReviewSourceFile{{Path: "connect/example.go", Content: fixture.SourceBefore, Sha256: hex.EncodeToString(digest[:])}}
		input, err := winnerModelInput(request)
		if err != nil {
			t.Fatal("qualification input refused", fixture.Name, err)
		}
		if key, err := winnerQualificationAnswerKey(input); err != nil || key != "" || bytes.Contains(input, []byte(fixture.Name)) {
			t.Fatal("qualification answer leaked", fixture.Name, key, err)
		}
		judgment.Decision, judgment.ProductionBenefit.Disposition = fixture.ExpectedDecision, fixture.ExpectedProductionDisposition
		judgment.ProductionBenefit.ProductionPath = "The changed production/client-facing entry point in connect/example.go."
		judgment.ProductionBenefit.Mechanism = "Synthetic expected causal assessment of the supplied before/after function, not a provider finding."
		judgment.ProductionBenefit.ClientBenefit = "Synthetic qualification expectation only; the exact case's changed code must support any real benefit claim."
		judgment.ProductionBenefit.PreservedSemantics = "The real reviewer must check unchanged work, ownership, bounds, cancellation and joined lifecycle in the supplied case."
		judgment.ProductionBenefit.Explanation = "Contract representation control; no model detection or production experiment is claimed."
		judgment.WorkloadPreservation.Disposition = fixture.ExpectedWorkloadDisposition
		if fixture.ExpectedDecision == "disqualify" {
			judgment.ReasonCodes = []string{"NO_PRODUCTION_BENEFIT"}
			judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
			if fixture.ExpectedWorkloadDisposition == "fail" {
				judgment.ReasonCodes = append(judgment.ReasonCodes, "UNSAFE_BEHAVIOR")
				judgment.Findings[5].Disposition, judgment.Findings[5].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
				judgment.Findings[5].Explanation = "ForwardClientBody silently returns success without delivering a healthy large body."
			}
		} else if fixture.ExpectedDecision == "uncertain" {
			judgment.ReasonCodes = []string{"SOURCE_NOT_READY"}
			judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
		}
		if err := validateWinnerSnowMember(judgment, request); err != nil {
			t.Fatal("synthetic expected decision not representable", fixture.Name, err)
		}
	}
}
