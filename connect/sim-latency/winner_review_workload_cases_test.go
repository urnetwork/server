// Fixed positive/negative qualification inputs are synthetic, never executed by
// a model here. Desired labels stay outside the readable provider request.
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

	"github.com/urnetwork/server/controller"
)

// Real provider qualification must use this independently committed census,
// including the honest-loss control, rather than selecting favorable examples.
func TestWinnerWorkloadReviewQualificationCasesKeepLabelsOutOfInput(t *testing.T) {
	content, err := os.ReadFile("testdata/workload-review-qualification-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Schema          int    `json:"schema"`
		Kind            string `json:"kind"`
		AddendumVersion string `json:"addendum_version"`
		Cases           []struct {
			Name                        string `json:"name"`
			ExpectedDecision            string `json:"expected_decision"`
			ExpectedWorkloadDisposition string `json:"expected_workload_disposition"`
			SourceBefore                string `json:"source_before"`
			SourceAfter                 string `json:"source_after"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(content, &corpus); err != nil || corpus.Schema != 1 || corpus.Kind != "synthetic-traffic-preservation-review-cases" || corpus.AddendumVersion != winnerWorkloadReviewVersion || len(corpus.Cases) != 5 {
		t.Fatal("qualification corpus identity or census changed", err)
	}
	wantNames := []string{"generic-large-transfer-abort", "healthy-parent-body-abort", "honest-ready-batch-with-incidental-loss", "legitimate-caller-cancellation", "unavailable-recovery-contract"}
	wantDecisions := []string{"disqualify", "disqualify", "approve", "approve", "uncertain"}
	wantChecks := []string{"fail", "fail", "pass", "pass", "uncertain"}
	for index, fixture := range corpus.Cases {
		if fixture.Name != wantNames[index] || fixture.ExpectedDecision != wantDecisions[index] || fixture.ExpectedWorkloadDisposition != wantChecks[index] {
			t.Fatal("qualification controls were relabeled", fixture.Name)
		}
		for _, source := range []string{fixture.SourceBefore, fixture.SourceAfter} {
			if _, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", source, parser.AllErrors); err != nil {
				t.Fatal("synthetic source is not parseable Go", fixture.Name, err)
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
			t.Fatal("synthetic qualification patch is not canonical", fixture.Name, failure)
		}
		request.Packet.Patch = canonical.Bytes
		request.Packet.Entry.PatchSha256, request.Template.PatchSha256 = canonical.Sha256, canonical.Sha256
		digest := sha256.Sum256([]byte(fixture.SourceBefore))
		request.SourceFiles = []winnerReviewSourceFile{{Path: "connect/example.go", Content: fixture.SourceBefore, Sha256: hex.EncodeToString(digest[:])}}
		// All controls intentionally have the same suspicious but G1/G2-passing
		// diagnostics. A reviewer must reason about code, not label from metrics.
		request.Packet.Score = []byte(`{"synthetic_only":true,"gates":{"G1_success":{"passed":true},"G2_volume":{"passed":true}},"diagnostics":{"baseline":{"raw_score":1000,"success_rate":1,"request_count":1000,"received_bytes":1000000},"candidate":{"raw_score":100,"success_rate":0.9916666666666667,"request_count":960,"received_bytes":952800}}}`)
		digest = sha256.Sum256(request.Packet.Score)
		request.Packet.Entry.ScoreSha256, request.Template.ScoreSha256 = hex.EncodeToString(digest[:]), hex.EncodeToString(digest[:])
		input, err := winnerModelInput(request)
		if err != nil || !bytes.Contains(input, []byte(`"workload_review"`)) || !bytes.Contains(input, []byte(`"instrument"`)) {
			t.Fatal("qualification case lost required evidence", fixture.Name, err)
		}
		if key, err := winnerQualificationAnswerKey(input); err != nil || key != "" || bytes.Contains(input, []byte(fixture.Name)) {
			t.Fatal("qualification answer label leaked into provider input", key, err)
		}
		judgment.Decision = fixture.ExpectedDecision
		judgment.WorkloadPreservation.Disposition = fixture.ExpectedWorkloadDisposition
		judgment.WorkloadPreservation.Explanation = "Synthetic contract expectation for the changed Forward function; this fixture does not claim a provider detected the behavior."
		if fixture.ExpectedDecision == "disqualify" {
			judgment.ReasonCodes = []string{"WORKLOAD_SPECIAL_CASING"}
			judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "fail", "WORKLOAD_SPECIAL_CASING"
		} else if fixture.ExpectedDecision == "uncertain" {
			judgment.ReasonCodes = []string{"SOURCE_NOT_READY"}
			judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
		}
		if err := validateWinnerSnowMember(judgment, request); err != nil {
			t.Fatal("supported synthetic disposition was not representable", fixture.Name, err)
		}
	}
}

// Exact decoded keys distinguish external answer labels from authenticated
// expected_decision_sha256 identities, including inside nested array objects.
func winnerQualificationAnswerKey(input []byte) (string, error) {
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return "", err
	}
	var find func(any) string
	find = func(value any) string {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "expected_decision" || key == "expected_workload_disposition" {
					return key
				}
				if found := find(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range value {
				if found := find(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return find(value), nil
}

// The legitimate authenticated identity is not an answer label, while actual
// keys remain forbidden at every object/array depth and after JSON unescaping.
func TestWinnerWorkloadReviewQualificationAnswerKeysAreStructural(t *testing.T) {
	for _, fixture := range []struct {
		input string
		want  string
	}{
		{input: `{"context":{"template":{"expected_decision_sha256":"synthetic-digest"}}}`},
		{input: `{"explanation":"expected_decision is a label name, not a JSON key here"}`},
		{input: `{"expected_decision":"approve"}`, want: "expected_decision"},
		{input: `{"context":{"labels":{"expected_workload_disposition":"pass"}}}`, want: "expected_workload_disposition"},
		{input: `{"context":[null,[{"expected_decision":"approve"}]]}`, want: "expected_decision"},
		{input: `[{"expected_\u0064ecision":"approve"}]`, want: "expected_decision"},
	} {
		if got, err := winnerQualificationAnswerKey([]byte(fixture.input)); err != nil || got != fixture.want {
			t.Fatalf("answer-key check for %s: got %q, want %q: %v", fixture.input, got, fixture.want, err)
		}
	}
}
