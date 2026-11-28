// These deterministic preparation guards are not genuine provider evidence.
// Every fixture identity/source/model result below is explicitly synthetic.
package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Exact old census bytes and every classic category remain present. Current
// eligibility changes the no-op answer, not the policy to obtain an approval.
func TestCurrentQgQualificationPreparationClosedCensus(t *testing.T) {
	workload, production := currentQgQualificationReadCensuses(t)
	sources, err := currentQgQualificationSources(workload, production)
	if err != nil || len(sources) != 19 {
		t.Fatal("current QG case census is not complete", err)
	}
	groups := map[string]int{}
	seen := map[string]bool{}
	for index, source := range sources {
		if source.Expectation.Position != index+1 || source.Before == source.After || seen[source.Expectation.Name] ||
			!strings.Contains(source.Before, "func ServeProductionFixture(") || !strings.Contains(source.After, "func ServeProductionFixture(") {
			t.Fatal("closed QG fixture lost order, actual caller or exact changed source")
		}
		seen[source.Expectation.Name] = true
		groups[source.Expectation.Group]++
		for _, content := range []string{source.Before, source.After} {
			if _, err := parser.ParseFile(token.NewFileSet(), "synthetic_fixture.go", content, parser.AllErrors); err != nil {
				t.Fatalf("closed fixture source is syntactically incomplete: %s: %v", source.Expectation.Name, err)
			}
		}
	}
	if !reflect.DeepEqual(groups, map[string]int{"classic": 6, "workload": 5, "production": 8}) ||
		sources[0].Expectation.Decision != "canceled" || sources[1].Expectation.Decision != "disqualify" || sources[1].Expectation.ProductionDisposition != "fail" ||
		sources[4].Expectation.Decision != "approve" || !strings.Contains(sources[4].After, "return disqualification") || !strings.Contains(sources[4].Before, "append([]byte(nil), packet...)") ||
		strings.Contains(sources[4].After, "append([]byte(nil), packet...)") {
		t.Fatal("current QG no-op eligibility or prompt-injection positive lost its concrete code effect")
	}
	if _, err := currentQgQualificationSources(append(append([]byte{}, workload...), '\n'), production); err == nil {
		t.Fatal("unprecommitted workload census was accepted")
	}
	if _, err := currentQgQualificationSources(workload, bytes.Replace(production, []byte("\"approve\""), []byte("\"uncertain\""), 1)); err == nil {
		t.Fatal("unprecommitted production census was accepted")
	}
}

// The same deterministic request/source/instrument bytes are emitted twice.
// Neither expected labels nor real candidate authority enter provider input.
func TestCurrentQgQualificationPreparationBindsContextWithoutLabels(t *testing.T) {
	workload, production := currentQgQualificationReadCensuses(t)
	sources, err := currentQgQualificationSources(workload, production)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := winnerSnowJudgmentFixture()
	seed.Packet.SourceCommits = map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		seed.Packet.SourceCommits[repository] = strings.Repeat("a", 40)
	}
	seed.Packet.Entry.BaseSourceLockSha256, err = model.CompetitionSourceLockDigest(seed.Packet.SourceCommits)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := currentQgQualificationRequests(seed.Packet, seed.WorkloadReview, sources)
	if err != nil {
		t.Fatal(err)
	}
	second, err := currentQgQualificationRequests(seed.Packet, seed.WorkloadReview, sources)
	if err != nil || !reflect.DeepEqual(requests, second) {
		t.Fatal("prospective QG requests contain unfrozen clock/random evidence", err)
	}
	for index, request := range requests {
		if index == 0 {
			if request.Operation != "qualification_cancel_before_provider" {
				t.Fatal("first case lost pre-input cancellation operation")
			}
			request.Operation = "review_elected_winner"
		}
		input, err := winnerModelInput(request)
		if err != nil || currentQgQualificationContainsExpectedLabels(input) || !bytes.Contains(input, []byte("production-benefit/1")) || !bytes.Contains(input, []byte("traffic-preservation/1")) ||
			request.Packet.BaseSha != seed.Packet.BaseSha || request.Packet.Instrument.ImageDigest != seed.Packet.Instrument.ImageDigest || request.Packet.Seal.RoundId == seed.Packet.Seal.RoundId {
			t.Fatal("QG input lost authenticated context or exposed expected/real-candidate authority", err)
		}
		var score struct {
			OfficialMeasurement bool `json:"official_measurement"`
			PerformanceMeasured bool `json:"performance_measured"`
			TestsExecuted       bool `json:"tests_executed"`
		}
		if json.Unmarshal(request.Packet.Score, &score) != nil || score.OfficialMeasurement || score.PerformanceMeasured || score.TestsExecuted {
			t.Fatal("source preparation manufactured an execution or performance claim")
		}
	}
	seed.WorkloadReview.Addendum += " synthetic substitution"
	if _, err := currentQgQualificationRequests(seed.Packet, seed.WorkloadReview, sources); err == nil {
		t.Fatal("substituted addendum entered prospective QG preparation")
	}
}

// The expectation file is independently digested and never mounted or sent to
// a broker. The export is exclusive; partial evidence cannot be overwritten.
func TestCurrentQgQualificationPreparationExportIsPrivateAndNonAuthorizing(t *testing.T) {
	workload, production := currentQgQualificationReadCensuses(t)
	sources, err := currentQgQualificationSources(workload, production)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := winnerSnowJudgmentFixture()
	seed.Packet.SourceCommits = map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		seed.Packet.SourceCommits[repository] = strings.Repeat("a", 40)
	}
	seed.Packet.Entry.BaseSourceLockSha256, err = model.CompetitionSourceLockDigest(seed.Packet.SourceCommits)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := currentQgQualificationRequests(seed.Packet, seed.WorkloadReview, sources)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "prepared")
	if err := currentQgQualificationExport(directory, strings.Repeat("b", 64), requests, sources); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 40 {
		t.Fatal("private preparation artifact census changed", err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
			t.Fatal("preparation leaf lost private regular-file mode", err)
		}
		if strings.Contains(entry.Name(), "input.json") || strings.Contains(entry.Name(), "broker-request.json") {
			input, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil || currentQgQualificationContainsExpectedLabels(input) {
				t.Fatal("expected label leaked into broker/provider input", err)
			}
		}
	}
	content, err := os.ReadFile(filepath.Join(directory, "preparation.json"))
	if err != nil || !bytes.Contains(content, []byte("PREPARED_NOT_AUTHORIZED_NOT_QUALIFIED")) || !bytes.Contains(content, []byte("\"provider_calls_made\": 0")) {
		t.Fatal("preparation export manufactured genuine provider qualification", err)
	}
	if err := currentQgQualificationExport(directory, strings.Repeat("b", 64), requests, sources); err == nil {
		t.Fatal("preparation export overwrote existing attempt evidence")
	}
}

// Native validation still requires both unmodified originals even when the
// expected result is uncertainty; a fabricated or missing member is not a pass.
func TestCurrentQgQualificationPreparationChecksEveryNativeMember(t *testing.T) {
	request, members := winnerSnowMemberFixture(t)
	expected := currentQgQualificationExpectation{Decision: "approve", WorkloadDisposition: "pass", ProductionDisposition: "pass"}
	if err := validateCurrentQgQualificationMembers(request, members, expected); err != nil {
		t.Fatal("complete synthetic native shape failed the offline checker", err)
	}
	for _, mutate := range []func([]winnerSnowMemberEvidence){
		func(m []winnerSnowMemberEvidence) { m[1].ProviderOutput = "{}" },
		func(m []winnerSnowMemberEvidence) { m[1].ProviderResponseId = m[0].ProviderResponseId },
		func(m []winnerSnowMemberEvidence) { m[1].Revision = strings.Repeat("f", 40) },
		func(m []winnerSnowMemberEvidence) {
			m[1].Usage = json.RawMessage(`{"input_tokens":10,"output_tokens":0}`)
		},
		func(m []winnerSnowMemberEvidence) { m[1].Judgment.ProductionBenefit = nil },
		func(m []winnerSnowMemberEvidence) { m[1].Judgment.WorkloadPreservation = nil },
	} {
		request, members = winnerSnowMemberFixture(t)
		mutate(members)
		if validateCurrentQgQualificationMembers(request, members, expected) == nil {
			t.Fatal("offline checker accepted missing/substituted original member evidence")
		}
	}
	if validateCurrentQgQualificationMembers(request, members[:1], expected) == nil {
		t.Fatal("offline checker accepted a single member")
	}
	request, members = winnerSnowMemberFixture(t)
	for index := range members {
		judgment := &members[index].Judgment
		production := *judgment.ProductionBenefit
		workload := *judgment.WorkloadPreservation
		judgment.ProductionBenefit, judgment.WorkloadPreservation = &production, &workload
		judgment.Decision, judgment.ReasonCodes = "uncertain", []string{"SOURCE_NOT_READY"}
		judgment.ProductionBenefit.Disposition, judgment.WorkloadPreservation.Disposition = "uncertain", "uncertain"
		judgment.Findings = append([]model.CompetitionReviewFinding{}, judgment.Findings...)
		judgment.Findings[1].Disposition, judgment.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
		content, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		members[index].ProviderOutput = string(content)
	}
	expected = currentQgQualificationExpectation{Decision: "uncertain", WorkloadDisposition: "uncertain", ProductionDisposition: "uncertain"}
	if err := validateCurrentQgQualificationMembers(request, members, expected); err != nil {
		t.Fatal("two complete synthetic uncertain judgments were lost", err)
	}
	members[1].ProviderOutput = "{}"
	if validateCurrentQgQualificationMembers(request, members, expected) == nil {
		t.Fatal("malformed original member was mistaken for completed expected uncertainty")
	}
}

// The authenticated template's expected_decision_sha256 is an identity, not a
// desired verdict. Reject exact private label keys without that prefix bug.
func TestCurrentQgQualificationPreparationSeparatesLabelKeysAndIdentity(t *testing.T) {
	if currentQgQualificationContainsExpectedLabels([]byte(`{"expected_decision_sha256":"synthetic-authenticated-identity"}`)) {
		t.Fatal("authenticated expected decision identity was mistaken for a private label")
	}
	for _, content := range []string{`{"expected_decision":"approve"}`, `{"expected_workload_disposition":"pass"}`, `{"expected_production_disposition":"pass"}`} {
		if !currentQgQualificationContainsExpectedLabels([]byte(content)) {
			t.Fatal("private expected label entered provider input")
		}
	}
}
