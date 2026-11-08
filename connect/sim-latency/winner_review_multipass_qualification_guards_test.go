// These are synthetic offline protocol guards, not a real provider report.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All release paths/digests are visibly synthetic and are never dereferenced.
func winnerMultipassQualificationFixture(t *testing.T) *winnerMultipassQualificationPlan {
	t.Helper()
	original, _, _, _ := currentQgExecutionFixture(t)
	requests := []winnerReviewBrokerRequest{}
	for _, item := range original.Cases {
		requests = append(requests, item.Request)
	}
	pin := func(name string) currentQgQualificationPin {
		return currentQgQualificationPin{Path: "/synthetic/" + name, Sha256: winnerMultipassSha256([]byte("synthetic-" + name))}
	}
	settings := defaultWinnerMultipassSettings()
	for index, member := range defaultWinnerSnowProviderSettings().Members {
		settings.TokenizationProfiles[member.Name] = winnerMultipassTokenizationProfile{Member: member, MaximumContextTokens: []int{98304, 131072}[index], TokenizerContractSha256: winnerMultipassTokenizerContractSha256(), RuntimeEvidenceSha256: winnerMultipassSha256([]byte("synthetic-runtime-" + member.Name))}
	}
	release := winnerMultipassQualificationRelease{Protocol: winnerMultipassVersion, SourceCommit: strings.Repeat("a", 40), TrustedCli: pin("cli"), NativeDriver: pin("driver"), Wrapper: pin("wrapper"), Configuration: pin("configuration"), ProviderSettings: pin("provider"), Image: pin("image"), ImageId: "sha256:" + strings.Repeat("b", 64),
		OfflineQualification: pin("offline"), Preparation: pin("preparation"), NativeFixtureEvidence: pin("native-fixtures"), DeploymentDeclaration: pin("deployment"), CheckpointEvidenceKind: "trusted_deployment_declaration_not_weight_attestation", TokenizationProfiles: settings.TokenizationProfiles}
	started := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	plan, err := prepareWinnerMultipassQualification(requests, winnerMultipassOwner{Id: "synthetic-qualification-group", StartedAt: started, Deadline: started.Add(time.Hour)}, settings, release, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// Full native evidence is generated solely for offline reader tests; every
// original response ID and predecessor-bound payload is unique to its control.
func winnerMultipassQualificationEvidenceFixture(t *testing.T, plan *winnerMultipassQualificationPlan) map[int]winnerMultipassEvidence {
	t.Helper()
	all := map[int]winnerMultipassEvidence{}
	for _, item := range plan.Cases[1:] {
		mock := currentQgExecutionMockResponse(t, item.Bundle.Request, item.Expectation, nil)
		var result winnerReviewBrokerResult
		var snow winnerSnowEvidence
		if json.Unmarshal(mock, &result) != nil || json.Unmarshal([]byte(result.PrivateEvidence), &snow) != nil {
			t.Fatal("synthetic original native judgments unavailable")
		}
		evidence := winnerMultipassEvidenceFixture(t, item.Bundle, item.Plan, snow.Members[0].Judgment)
		completed := map[string]winnerMultipassCallEvidence{}
		for index, call := range item.Plan.Calls {
			value := evidence.Calls[index]
			payload, err := buildWinnerMultipassCall(item.Plan, item.Bundle, call.Id, completed)
			if err != nil {
				t.Fatal(err)
			}
			value.Request = payload
			value.Admission.RequestSha256 = winnerMultipassSha256(payload)
			var terminal map[string]any
			if err := json.Unmarshal(value.VisibleResponse, &terminal); err != nil {
				t.Fatal(err)
			}
			terminal["id"] = fmt.Sprintf("synthetic-control-%02d-response-%04d", item.Position, index)
			value.VisibleResponse, _ = json.Marshal(terminal)
			value.VisibleResponseSha256 = winnerMultipassSha256(value.VisibleResponse)
			value.TransportResponseSha256 = value.VisibleResponseSha256
			evidence.Calls[index] = value
			completed[call.Id] = value
		}
		all[item.Position] = evidence
	}
	return all
}

// All nineteen controls and every leaf preflight precede one fixed Qwen/Gemma batch.
func TestWinnerMultipassQualificationDeclaresCompleteModelBatchedCensus(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	if err := validateWinnerMultipassQualificationPlan(plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Cases) != 19 || plan.GenerationCalls != 72 || plan.TokenizerInvocations != 108 || len(plan.Invocations) != 181 {
		t.Fatalf("unexpected minimal complete protocol census: generations=%d tokenizers=%d invocations=%d", plan.GenerationCalls, plan.TokenizerInvocations, len(plan.Invocations))
	}
	seenGeneration, seenGemma, loads := false, false, 0
	for _, invocation := range plan.Invocations {
		if invocation.AllowModelLoad {
			loads++
			if invocation.Operation != "tokenize" {
				t.Fatal("inference became a model-load probe")
			}
		}
		if invocation.Phase == "frozen_leaf_preflight" && seenGeneration {
			t.Fatal("frozen leaf admission occurred after first draw")
		}
		if invocation.Operation != "generate" {
			continue
		}
		seenGeneration = true
		if strings.HasPrefix(invocation.CallId, "gemma-") {
			seenGemma = true
		} else if seenGemma {
			t.Fatal("qualification alternated member batches")
		}
	}
	if loads != 4 {
		t.Fatal("model-load transitions were not predeclared for two preflight and two execution batches")
	}
}

// Private desired outcomes cannot become hidden context inside a provider payload.
func TestWinnerMultipassQualificationKeepsExpectationsOutsideModelInput(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	state := newWinnerMultipassQualificationState(plan)
	for _, invocation := range plan.Invocations {
		if invocation.Phase != "frozen_leaf_preflight" {
			continue
		}
		envelope, err := winnerMultipassQualificationEnvelope(plan, invocation, state, plan.Owner.StartedAt.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		var payload winnerMultipassPayload
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if currentQgQualificationContainsExpectedLabels(envelope.Payload) || currentQgQualificationContainsExpectedLabels([]byte(payload.Input)) || strings.Contains(payload.Input, `"private_expectation"`) {
			t.Fatal("desired outcomes leaked into actual review input")
		}
	}
}

// An altered case label or omitted operation cannot be accepted after hashing.
func TestWinnerMultipassQualificationRejectsRetunedOrOmittedControl(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	plan.Cases[2].Expectation.Decision = "approve"
	if validateWinnerMultipassQualificationPlan(plan) == nil {
		t.Fatal("retuned control expectation accepted")
	}
	plan = winnerMultipassQualificationFixture(t)
	plan.Invocations = plan.Invocations[1:]
	if validateWinnerMultipassQualificationPlan(plan) == nil {
		t.Fatal("pre-input cancellation was omitted")
	}
}

// Every member and both required subchecks are reconstructed for all controls.
func TestWinnerMultipassQualificationReplaysAllOriginalOutcomes(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	evidence := winnerMultipassQualificationEvidenceFixture(t, plan)
	if err := validateWinnerMultipassQualificationOutcomes(plan, evidence); err != nil {
		t.Fatal(err)
	}
	delete(evidence, 19)
	if validateWinnerMultipassQualificationOutcomes(plan, evidence) == nil {
		t.Fatal("one mandatory control was dropped")
	}
}

// Whole-suite accounting includes both preflight and immediate per-call tokens.
func TestWinnerMultipassQualificationReportsConservativeWholeGroupCapacity(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 64 * 1024 * 1024 * 1024
	capacity, err := estimateWinnerMultipassQualificationCapacity(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(capacity.Cases) != 18 || capacity.RequiredDurableBytes <= capacity.Cases[2].RequiredDurableBytes || capacity.RequiredMemoryBytes > settings.MaximumMemoryBytes {
		t.Fatal("whole qualification resource census was not bounded")
	}
	t.Logf("Synthetic minimum complete-census estimate only: generations=%d tokenizers=%d durable_bytes=%d native_memory_bytes=%d", plan.GenerationCalls, plan.TokenizerInvocations, capacity.RequiredDurableBytes, capacity.RequiredMemoryBytes)
	settings.MaximumDurableBytes = 1
	if _, err := estimateWinnerMultipassQualificationCapacity(plan, settings); err == nil {
		t.Fatal("unreserved full group could start")
	}
}

// A copied qualified boolean cannot convert offline mocks into a genuine gate.
func TestWinnerMultipassQualificationGateRejectsOfflineAndHistoricalReports(t *testing.T) {
	plan := winnerMultipassQualificationFixture(t)
	for index, kind := range []string{"winner-review-multipass-qualification-report/v1", "winner-review-current-qg-qualification-report/v1"} {
		report := winnerMultipassQualificationReport{Schema: 1, Kind: kind, Protocol: winnerMultipassVersion, Mode: "offline", Release: plan.Release, Complete: true, Qualified: true}
		content, _ := json.Marshal(report)
		path := filepath.Join(t.TempDir(), fmt.Sprintf("synthetic-%d.json", index))
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := authenticateWinnerMultipassQualification(currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, plan.Release); err == nil {
			t.Fatal("offline/historical report unlocked native winner capture")
		}
	}
}
