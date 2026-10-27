// Authenticated duplicates are reconciled before either model transport;
// synthetic fixtures never claim performance execution or publication authority.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The score is synthetic link data only, not measured or gate-qualified output.
func winnerModelIdentityScoreFixture(t *testing.T) winnerReviewBrokerRequest {
	t.Helper()
	request, _ := winnerModelFixture()
	value := 10.0
	score := model.CompetitionScoreResult{ScoreSchema: 1, RawScore: &value,
		Diagnostics: map[string]any{"job_id": request.Request.JobId.String(), "round_id": request.Request.RoundId.String(), "baseline_sha256": request.Template.BaselineSha256}}
	content, err := json.Marshal(score)
	if err != nil {
		t.Fatal(err)
	}
	request.Packet.Score = content
	digest := sha256.Sum256(content)
	request.Template.ScoreSha256 = hex.EncodeToString(digest[:])
	request.Packet.Entry.ScoreSha256 = request.Template.ScoreSha256
	request.Packet.Entry.Score = &model.CompetitionScoreResult{}
	if err := json.Unmarshal(content, request.Packet.Entry.Score); err != nil {
		t.Fatal(err)
	}
	return request
}

// Every frozen provider policy consumes the same coherent authenticated input.
func TestWinnerReviewModelInputIdentityPreservesFrozenPolicies(t *testing.T) {
	for _, policyName := range []string{model.CompetitionWinnerReviewPolicy, model.CompetitionLegacyWinnerReviewPolicy, model.CompetitionSnowWinnerReviewPolicy} {
		request, _ := winnerModelFixture()
		modelName, reasoning, ok := model.CompetitionWinnerReviewProvider(policyName)
		if !ok {
			t.Fatal("synthetic policy is unsupported", policyName)
		}
		request.Request.Policy.WinnerReviewPolicy = policyName
		request.Request.ReviewerModel, request.Request.ReviewerReasoning = modelName, reasoning
		request.Template.ReviewerModel, request.Template.ReviewerReasoning = modelName, reasoning
		if _, err := winnerModelInput(request); err != nil {
			t.Fatalf("coherent frozen policy %s refused: %v", policyName, err)
		}
	}
}

// A valid byte digest cannot repair contradictory selected-entry metadata.
func TestWinnerReviewModelInputRejectsContradictoryIdentities(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*winnerReviewBrokerRequest)
	}{
		{name: "entry patch", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.PatchSha256 = strings.Repeat("0", 64) }},
		{name: "entry score", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.ScoreSha256 = strings.Repeat("0", 64) }},
		{name: "entry source lock", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.BaseSourceLockSha256 = strings.Repeat("0", 64) }},
		{name: "entry artifact", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.ArtifactManifestSha256 = strings.Repeat("0", 64) }},
		{name: "template baseline", mutate: func(r *winnerReviewBrokerRequest) { r.Template.BaselineSha256 = strings.Repeat("0", 64) }},
		{name: "entry job", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.JobId = server.Id{0: 99, 15: 1} }},
		{name: "template job", mutate: func(r *winnerReviewBrokerRequest) { r.Template.JobId = server.Id{0: 99, 15: 1} }},
		{name: "request job", mutate: func(r *winnerReviewBrokerRequest) { r.Request.JobId = server.Id{0: 99, 15: 1} }},
		{name: "seal round", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Seal.RoundId = server.Id{0: 99, 15: 2} }},
		{name: "request round", mutate: func(r *winnerReviewBrokerRequest) { r.Request.RoundId = server.Id{0: 99, 15: 2} }},
		{name: "seal digest", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Seal.SealSha256 = strings.Repeat("0", 64) }},
		{name: "packet revision", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.RewardRevision++ }},
		{name: "template revision", mutate: func(r *winnerReviewBrokerRequest) { r.Template.ExpectedRevision++ }},
		{name: "decision digest", mutate: func(r *winnerReviewBrokerRequest) { r.Request.RewardDecisionSha256 = strings.Repeat("0", 64) }},
		{name: "packet criteria", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.ReviewCriteriaVersion = "synthetic-other/v1" }},
		{name: "request criteria", mutate: func(r *winnerReviewBrokerRequest) { r.Request.CriteriaVersion = "synthetic-other/v1" }},
		{name: "template criteria", mutate: func(r *winnerReviewBrokerRequest) { r.Template.CriteriaVersion = "synthetic-other/v1" }},
		{name: "policy criteria", mutate: func(r *winnerReviewBrokerRequest) { r.Request.Policy.ReviewCriteriaVersion = "synthetic-other/v1" }},
		{name: "ineligible", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.Eligible = false }},
		{name: "failed entry", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.State = "failed" }},
		{name: "contradictory eligibility", mutate: func(r *winnerReviewBrokerRequest) { r.Packet.Entry.IneligibilityReason = "synthetic exclusion" }},
	}
	for _, testCase := range cases {
		request, _ := winnerModelFixture()
		if _, err := winnerModelInput(request); err != nil {
			t.Fatal("positive identity fixture is invalid", err)
		}
		testCase.mutate(&request)
		if _, err := winnerModelInput(request); err == nil {
			t.Fatal("contradictory identity reached readable input", testCase.name)
		}
	}
}

// The actual Snow failure changed patch/template bytes while keeping the old
// sealed-entry hash. Both transport paths must refuse it before any journal/POST.
func TestWinnerReviewModelInputRejectsStalePatchBeforeProviders(t *testing.T) {
	for _, snow := range []bool{false, true} {
		request, _ := winnerModelFixture()
		if snow {
			request, _ = winnerSnowJudgmentFixture()
		}
		request.Packet.Patch = bytes.Replace(request.Packet.Patch, []byte("synthetic patch:"), []byte("synthetic replacement:"), 1)
		digest := sha256.Sum256(request.Packet.Patch)
		request.Template.PatchSha256 = hex.EncodeToString(digest[:])
		calls := 0
		client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("synthetic forbidden provider call")
		})}
		directory := t.TempDir()
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
		var result *winnerReviewBrokerResult
		var err error
		if snow {
			result, err = executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), defaultWinnerSnowProviderSettings(), strings.Repeat("a", 64), directory)
		} else {
			result, err = executeWinnerModel(context.Background(), client, "synthetic-model-key", request, defaultWinnerModelSettings())
		}
		entries, statErr := os.ReadDir(directory)
		if err == nil || !strings.Contains(err.Error(), "selected entry evidence identity") || result != nil || calls != 0 || statErr != nil || len(entries) != 0 {
			t.Fatalf("snow=%t stale patch was not refused before provider ownership: result=%t calls=%d files=%d err=%v stat=%v", snow, result != nil, calls, len(entries), err, statErr)
		}
	}
}

// If the full seal inventory is supplied, it cannot describe a different
// candidate, repeat the selected identity, or omit it in favor of another job.
func TestWinnerReviewModelInputBindsSuppliedSealInventory(t *testing.T) {
	request, _ := winnerModelFixture()
	request.Packet.Seal.Entries = []model.CompetitionSealedEntry{request.Packet.Entry}
	if _, err := winnerModelInput(request); err != nil {
		t.Fatal("coherent sealed entry refused", err)
	}
	for _, mutate := range []func(*winnerReviewBrokerRequest){
		func(r *winnerReviewBrokerRequest) { r.Packet.Seal.Entries[0].PatchSha256 = strings.Repeat("0", 64) },
		func(r *winnerReviewBrokerRequest) {
			r.Packet.Seal.Entries = append(r.Packet.Seal.Entries, r.Packet.Entry)
		},
		func(r *winnerReviewBrokerRequest) { r.Packet.Seal.Entries[0].JobId = server.Id{0: 99, 15: 1} },
	} {
		candidate, _ := winnerModelFixture()
		candidate.Packet.Seal.Entries = []model.CompetitionSealedEntry{candidate.Packet.Entry}
		mutate(&candidate)
		if _, err := winnerModelInput(candidate); err == nil {
			t.Fatal("contradictory selected seal inventory accepted")
		}
	}
}

// Matching content hashes do not excuse a typed score whose duplicate metadata
// disagrees, or whose repinned diagnostics identify another job/round/baseline.
func TestWinnerReviewModelInputBindsTypedScoreAndDiagnosticIdentity(t *testing.T) {
	request := winnerModelIdentityScoreFixture(t)
	if _, err := winnerModelInput(request); err != nil {
		t.Fatal("coherent synthetic typed score refused", err)
	}
	request.Packet.Entry.Score.ScoreSchema++
	if _, err := winnerModelInput(request); err == nil {
		t.Fatal("contradictory typed sealed score accepted")
	}
	for _, testCase := range []struct {
		key   string
		value any
	}{
		{key: "round_id", value: server.Id{0: 99, 15: 2}.String()},
		{key: "baseline_sha256", value: strings.Repeat("0", 64)},
		{key: "round_id", value: float64(1)},
	} {
		candidate := winnerModelIdentityScoreFixture(t)
		candidate.Packet.Entry.Score.Diagnostics[testCase.key] = testCase.value
		content, err := json.Marshal(candidate.Packet.Entry.Score)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Packet.Score = content
		digest := sha256.Sum256(content)
		candidate.Template.ScoreSha256 = hex.EncodeToString(digest[:])
		candidate.Packet.Entry.ScoreSha256 = candidate.Template.ScoreSha256
		if _, err := winnerModelInput(candidate); err == nil || !strings.Contains(err.Error(), "score diagnostics") {
			t.Fatalf("repinned score identity %s accepted or rejected for the wrong reason: %v", testCase.key, err)
		}
	}
}

// The real packet producer uses canonical score identities. Empty and absent
// omitempty diagnostics are equivalent even though Go reflection differs.
func TestWinnerReviewModelInputPreservesCanonicalScoreNormalization(t *testing.T) {
	request := winnerModelIdentityScoreFixture(t)
	request.Packet.Entry.Score.Diagnostics = map[string]any{}
	archived := *request.Packet.Entry.Score
	archived.Diagnostics = nil
	content, err := json.Marshal(archived)
	if err != nil {
		t.Fatal(err)
	}
	actual, actualErr := model.CompetitionAutomationDigest(archived)
	expected, expectedErr := model.CompetitionAutomationDigest(request.Packet.Entry.Score)
	if actualErr != nil || expectedErr != nil || actual != expected || reflect.DeepEqual(archived.Diagnostics, request.Packet.Entry.Score.Diagnostics) {
		t.Fatal("fixture does not reproduce the producer's canonical normalization", actualErr, expectedErr)
	}
	request.Packet.Score = content
	digest := sha256.Sum256(content)
	request.Template.ScoreSha256 = hex.EncodeToString(digest[:])
	request.Packet.Entry.ScoreSha256 = request.Template.ScoreSha256
	if _, err := winnerModelInput(request); err != nil {
		t.Fatal("producer-accepted canonical score normalization was refused", err)
	}
}

// Only the native schema's round/baseline fields have identity semantics.
// Other diagnostic keys remain evidence, not newly invented authentication.
func TestWinnerReviewModelInputKeepsOpaqueScoreDiagnostics(t *testing.T) {
	request := winnerModelIdentityScoreFixture(t)
	request.Packet.Entry.Score.Diagnostics["job_id"] = "synthetic opaque evaluator diagnostic"
	content, err := json.Marshal(request.Packet.Entry.Score)
	if err != nil {
		t.Fatal(err)
	}
	request.Packet.Score = content
	digest := sha256.Sum256(content)
	request.Template.ScoreSha256 = hex.EncodeToString(digest[:])
	request.Packet.Entry.ScoreSha256 = request.Template.ScoreSha256
	if _, err := winnerModelInput(request); err != nil {
		t.Fatal("undeclared diagnostic acquired an invented identity contract", err)
	}
}

// Models receive pristine before-patch context explicitly labeled as such;
// serializing readable input must not mutate or silently repair the request.
func TestWinnerReviewModelInputLabelsPristineBaseWithoutMutation(t *testing.T) {
	request, _ := winnerModelFixture()
	before, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	content, err := winnerModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	var readable map[string]json.RawMessage
	if err := json.Unmarshal(content, &readable); err != nil {
		t.Fatal(err)
	}
	var semantics, patch string
	var context winnerReviewBrokerRequest
	if json.Unmarshal(readable["source_files_semantics"], &semantics) != nil ||
		json.Unmarshal(readable["patch_text"], &patch) != nil ||
		json.Unmarshal(readable["context"], &context) != nil {
		t.Fatal("readable context lost its explicit source labels")
	}
	if !strings.Contains(semantics, "authenticated pristine source") || !strings.Contains(semantics, "before patch_text") ||
		!strings.Contains(semantics, "not post-patch candidate source") || patch != string(request.Packet.Patch) ||
		!reflect.DeepEqual(context.SourceFiles, request.SourceFiles) || strings.Contains(context.SourceFiles[0].Content, "synthetic patch:") {
		t.Fatal("base source was confused with the resulting patched candidate")
	}
	after, err := json.Marshal(request)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("model projection mutated authenticated input", err)
	}
}
