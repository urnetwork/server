// Durable model intent prevents retries after ambiguous provider execution.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reuse authenticated synthetic source context without calling any provider.
func winnerSnowMemberFixture(t *testing.T) (winnerReviewBrokerRequest, []winnerSnowMemberEvidence) {
	t.Helper()
	request, judgment := winnerModelFixture()
	content, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	members := []winnerSnowMemberEvidence{}
	for index, member := range defaultWinnerSnowProviderSettings().Members {
		members = append(members, winnerSnowMemberEvidence{Name: member.Name, RequestedModel: member.Model, Model: member.Model, Revision: member.Revision, Reasoning: member.Reasoning, ProviderResponseId: fmt.Sprintf("synthetic-response-%d", index), RequestSha256: strings.Repeat("1", 64), ResponseSha256: strings.Repeat("2", 64), Usage: json.RawMessage(`{"input_tokens":10,"output_tokens":10}`), Judgment: judgment, ProviderOutput: string(content)})
	}
	return request, members
}

// Approval retains all eighteen attributed findings, not a selected model.
func TestWinnerSnowUnionRequiresAllThreeMembers(t *testing.T) {
	request, members := winnerSnowMemberFixture(t)
	combined := aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request)
	if combined.Decision != "approve" || len(combined.Findings) != 18 {
		t.Fatalf("complete union did not approve: %s (%d findings)", combined.Decision, len(combined.Findings))
	}
	combined = aggregateWinnerSnow(members[:2], defaultWinnerSnowProviderSettings(), request)
	if combined.Decision != "uncertain" {
		t.Fatal("missing third reviewer authorized a decision")
	}
}

// Parent replay must compare retained raw output, not trust a replaced struct.
func TestWinnerSnowUnionRejectsReplacedDecodedJudgment(t *testing.T) {
	request, members := winnerSnowMemberFixture(t)
	members[2].ProviderOutput = `{}`
	if aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request).Decision != "uncertain" {
		t.Fatal("replaced raw judgment authorized approval")
	}
}

// One provider receipt cannot stand in for two independent reviewers.
func TestWinnerSnowUnionRejectsDuplicateProviderReceipt(t *testing.T) {
	request, members := winnerSnowMemberFixture(t)
	members[2].ProviderResponseId = members[0].ProviderResponseId
	if aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request).Decision != "uncertain" {
		t.Fatal("reused provider receipt authorized approval")
	}
}

// The exact production journal operation must refuse a second intent even when
// the requested content changes; a crash is not a fresh review opportunity.
func TestWinnerSnowMemberJournalCannotReroll(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := persistWinnerSnowMember(directory, 1, "started", map[string]any{"schema": 1, "model": "synthetic-model"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(directory, "member-01.started.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := persistWinnerSnowMember(directory, 1, "started", map[string]any{"schema": 1, "model": "replacement-model"}); err == nil {
		t.Fatal("ambiguous model call was rerolled")
	}
	after, err := os.ReadFile(filepath.Join(directory, "member-01.started.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("original durable intent changed")
	}
}

// Journal directory permissions are an authority boundary, not a convenience.
func TestWinnerSnowMemberJournalRequiresPrivateDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := persistWinnerSnowMember(directory, 1, "started", struct{}{}); err == nil {
		t.Fatal("public state directory accepted")
	}
}

// A member cannot escape the fixed six-leaf journal census.
func TestWinnerSnowMemberJournalRejectsUnknownIdentity(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 4} {
		if err := persistWinnerSnowMember(directory, index, "started", struct{}{}); err == nil {
			t.Fatal("unknown member accepted")
		}
	}
	if err := persistWinnerSnowMember(directory, 1, "../escape", struct{}{}); err == nil {
		t.Fatal("unknown journal phase accepted")
	}
}
