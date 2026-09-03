package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

func TestMaterializeReviewCandidateUsesPrivateFreshDirectory(t *testing.T) {
	patch := []byte("diff --git a/connect/example.go b/connect/example.go\n")
	digest := sha256.Sum256(patch)
	raw, normalized := 80.0, 125.0
	state := &controller.CandidateReviewState{
		CompetitionId: "sim-latency-season-1",
		RoundId:       server.NewId(),
		Epoch:         2,
		Status:        "pending_review",
		Candidate: &controller.CandidateReviewCandidate{
			Rank:        3,
			JobId:       server.NewId(),
			PatchSha256: hex.EncodeToString(digest[:]),
			SubmittedAt: time.Date(2026, 8, 28, 20, 0, 0, 0, time.UTC),
			Patch:       patch,
			Score: controller.ScoreResult{
				ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized,
			},
		},
	}
	directory := filepath.Join(t.TempDir(), "candidate")
	materialized, err := materializeReviewCandidate(state, directory)
	if err != nil {
		t.Fatal(err)
	}
	if materialized != directory {
		t.Fatalf("materialized directory = %q, want %q", materialized, directory)
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("candidate directory mode = %v, %v", info.Mode().Perm(), err)
	}
	for _, name := range []string{"candidate.json", "score.json", "canonical.patch"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != 0o400 {
			t.Errorf("%s mode = %v, %v", name, info.Mode().Perm(), err)
		}
	}
	gotPatch, err := os.ReadFile(filepath.Join(directory, "canonical.patch"))
	if err != nil || string(gotPatch) != string(patch) {
		t.Fatalf("materialized patch = %q, %v", gotPatch, err)
	}
	var candidate map[string]any
	candidateBytes, err := os.ReadFile(filepath.Join(directory, "candidate.json"))
	if err != nil || json.Unmarshal(candidateBytes, &candidate) != nil {
		t.Fatalf("candidate metadata is invalid: %v", err)
	}
	if _, exposed := candidate["Patch"]; exposed {
		t.Fatal("candidate metadata exposed patch bytes")
	}
	if _, exposed := candidate["patch"]; exposed {
		t.Fatal("candidate metadata exposed patch bytes")
	}
	if _, err := materializeReviewCandidate(state, directory); !os.IsExist(err) {
		t.Fatalf("existing review directory was reused: %v", err)
	}
}

func TestReviewCandidateMaterializationRequiresExplicitNextAction(t *testing.T) {
	state := &controller.CandidateReviewState{
		Status:    "pending_review",
		Candidate: &controller.CandidateReviewCandidate{JobId: server.NewId()},
	}
	if !reviewActionMaterializesCandidate("next", state) {
		t.Fatal("next action did not materialize its pending candidate")
	}
	for _, action := range []string{"reject", "approve", "export-winner"} {
		if reviewActionMaterializesCandidate(action, state) {
			t.Fatalf("%s action implicitly materialized another candidate directory", action)
		}
	}
	state.Status = "finalized"
	if reviewActionMaterializesCandidate("next", state) {
		t.Fatal("finalized state materialized a candidate directory")
	}
}

func TestReadHonestyEvidenceAuthenticatesRegularJson(t *testing.T) {
	path := filepath.Join(t.TempDir(), "honesty.json")
	evidence := []byte(`{"schema":1,"honest":true}`)
	if err := os.WriteFile(path, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	got, digest, err := readHonestyEvidence(path)
	if err != nil || string(got) != string(evidence) {
		t.Fatalf("evidence = %q, %v", got, err)
	}
	wantDigest := sha256.Sum256(evidence)
	if digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("evidence digest = %s", digest)
	}
	link := filepath.Join(filepath.Dir(path), "honesty-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readHonestyEvidence(link); err == nil {
		t.Fatal("symlinked honesty evidence was accepted")
	}
}

func TestAuthenticateApprovedWinnerBundleBindsPatchAndScore(t *testing.T) {
	root := t.TempDir()
	patch := []byte("diff --git a/connect/example.go b/connect/example.go\n")
	if err := os.WriteFile(filepath.Join(root, "canonical.patch"), patch, 0o600); err != nil {
		t.Fatal(err)
	}
	patchDigest := sha256.Sum256(patch)
	baselineVariance := 4.0
	candidateVariance := 2.0
	minimumPercent := 3.0
	requiredPercent := 16.1
	pValue := 0.01
	nextMinimumPercent := 2.5
	nextMarginPercent := 16.1
	rawScore := 80.0
	normalizedScore := 125.0
	approved := &controller.CandidateReviewCandidate{
		JobId:       server.NewId(),
		PatchSha256: hex.EncodeToString(patchDigest[:]),
		Score: controller.ScoreResult{
			ScoreSchema:      controller.ScoreSchema,
			RawScore:         &rawScore,
			NormalizedScore:  &normalizedScore,
			Placeable:        true,
			TakeoverEligible: true,
			Gates: map[string]controller.Gate{
				"G1_success": {Passed: true, Details: map[string]any{}},
			},
			Significance: &controller.ScoreSignificance{
				Method:                                      "one-sided-welch-t",
				Alpha:                                       0.05,
				ReplicateCount:                              9,
				BaselineMeanRawScore:                        100,
				CandidateMeanRawScore:                       80,
				BaselineSampleVariance:                      &baselineVariance,
				CandidateSampleVariance:                     &candidateVariance,
				ObservedImprovementPercent:                  20,
				TakeoverMarginPercent:                       16.1,
				MinimumSignificantImprovementPercent:        &minimumPercent,
				RequiredImprovementPercent:                  &requiredPercent,
				OneSidedPValue:                              &pValue,
				StatisticallySignificant:                    true,
				NextEpochMinimumImprovementPercent:          &nextMinimumPercent,
				RecommendedNextEpochTakeoverMarginPercent:   &nextMarginPercent,
				RecommendedNextEpochTakeoverMarginSupported: true,
			},
			Diagnostics: map[string]any{"baseline_takeover_eligible": true},
		},
	}
	scoreBytes, err := json.Marshal(approved.Score)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "score.json"), scoreBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	winnerScore, _, err := readWinnerScore(root)
	if err != nil {
		t.Fatal(err)
	}
	promotion := &sourceSignificance{
		Method:                                    "one-sided-welch-t",
		Alpha:                                     0.05,
		ReplicateCount:                            9,
		BaselineMeanRawScore:                      100,
		CandidateMeanRawScore:                     80,
		BaselineSampleVariance:                    baselineVariance,
		CandidateSampleVariance:                   candidateVariance,
		ObservedImprovementPercent:                20,
		TakeoverMarginPercent:                     16.1,
		MinimumSignificantImprovementPercent:      minimumPercent,
		RequiredImprovementPercent:                requiredPercent,
		OneSidedPValue:                            pValue,
		NextEpochMinimumImprovementPercent:        nextMinimumPercent,
		RecommendedNextEpochTakeoverMarginPercent: nextMarginPercent,
	}
	if err := authenticateApprovedWinnerBundle(root, approved, winnerScore, promotion); err != nil {
		t.Fatalf("approved bundle: %v", err)
	}
	tamperedScore := *winnerScore
	tamperedRawScore := *winnerScore.RawScore - 1
	tamperedScore.RawScore = &tamperedRawScore
	if err := authenticateApprovedWinnerBundle(root, approved, &tamperedScore, promotion); err == nil {
		t.Fatal("tampered approved score document was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "canonical.patch"), append(patch, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authenticateApprovedWinnerBundle(root, approved, winnerScore, promotion); err == nil {
		t.Fatal("tampered approved patch was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "canonical.patch"), patch, 0o600); err != nil {
		t.Fatal(err)
	}
	promotion.RecommendedNextEpochTakeoverMarginPercent = 17
	if err := authenticateApprovedWinnerBundle(root, approved, winnerScore, promotion); err == nil {
		t.Fatal("tampered approved score significance was accepted")
	}
	promotion.RecommendedNextEpochTakeoverMarginPercent = nextMarginPercent
	if err := os.WriteFile(filepath.Join(root, "userwireguard.patch"), patch, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authenticateApprovedWinnerBundle(root, approved, winnerScore, promotion); err == nil {
		t.Fatal("unevaluated repository patch was accepted")
	}
}
