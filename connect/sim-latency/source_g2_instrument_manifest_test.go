// Draft-overlay regressions use only frozen source fixtures and pure new DTOs.
// Every approval/review identity below is synthetic test data, never authority.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A reviewed mono-root winner changes connect, not server. Its separately
// approved instrument child changes server and must retain that exact winner.
func g2DraftReviewedDependencyManifest(t *testing.T) *sourceManifest {
	t.Helper()
	manifest := reviewedSourceTestManifest(t)
	previous := manifest.EvaluationSource.Generations[0]
	selected := previous.Repositories.commits()
	selected["connect"] = strings.Repeat("c", 40)
	selectedLock, err := model.CompetitionSourceLockDigest(selected)
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/connect/sim-latency/score.go b/connect/sim-latency/score.go\n--- a/connect/sim-latency/score.go\n+++ b/connect/sim-latency/score.go\n@@ -1 +1 @@\n-// synthetic legacy instrument\n+// synthetic minimum work instrument\n")
	patchHash := sha256.Sum256(patch)
	approval := model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-draft-g2", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, InstrumentBaseCommits: previous.Repositories.commits(),
		InstrumentBaseSourceLockSha256: previous.SourceLockSha256, InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64),
		Patch: patch, PatchSha256: hex.EncodeToString(patchHash[:]), ProtectedPaths: []string{"connect/sim-latency/score.go"}}
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	reviewId := server.Id{15: 1}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: approval, SelectedProductKind: "reviewed_source_promotion",
		SelectedProductCommits: selected, SelectedProductSourceLockSha256: selectedLock, SelectedProductReviewId: &reviewId,
		SelectedProductReviewDigest: strings.Repeat("d", 64), SelectedProductPatchSha256: strings.Repeat("e", 64),
		InstrumentedServerCommit: strings.Repeat("f", 40)}
	commits := previous.Repositories.commits()
	for repository, commit := range selected {
		commits[repository] = commit
	}
	commits["server"] = proof.InstrumentedServerCommit
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	significance := validSourceSignificance()
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedSourceGeneration{
		Generation: 1, Kind: "reviewed_source_promotion", G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: proof,
		SourceLockSha256: lock, SignificantImprovementPercent: significance.RecommendedNextEpochTakeoverMarginPercent,
		ThresholdEvidenceSha256: significance.ScoreSha256, Repositories: sourceRepositoriesFromCommits(commits),
		SourceApprovalId: reviewId.String(), SourceApprovalDigest: proof.SelectedProductReviewDigest,
		EvaluatedBaseSourceLockSha256: previous.SourceLockSha256, PatchSha256: proof.SelectedProductPatchSha256,
		ScoreSha256: significance.ScoreSha256, Significance: significance})
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{
		Epoch: 2, SourceGeneration: 1, ActivationGeneration: 2, SourceDecisionDigest: strings.Repeat("1", 64),
		Kind: "reviewed_source_promotion", Reason: "synthetic_reviewed_dependency_winner", SourceLockSha256: lock,
		SignificantImprovementPercent: significance.RecommendedNextEpochTakeoverMarginPercent, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork})
	return manifest
}

// Only the explicit, fully bound new instrument may retain this already
// reviewed dependency change; a server-only synthetic winner is not required.
func TestG2DraftInstrumentManifestAcceptsReviewedNonserverProductGraph(t *testing.T) {
	manifest := g2DraftReviewedDependencyManifest(t)
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal("new trusted manifest discarded the genuine selected dependency graph", err)
	}
}

// No-winner carry preserves the incumbent and calibrated margin without
// inventing a source review; only the independently approved child is new.
func TestG2DraftInstrumentManifestAcceptsUnreviewedIncumbentCarry(t *testing.T) {
	manifest := g2DraftReviewedDependencyManifest(t)
	previous := manifest.EvaluationSource.Generations[0]
	generation := &manifest.EvaluationSource.Generations[1]
	proof := generation.TrustedInstrument
	proof.SelectedProductKind = "reviewed_source_carry_forward"
	proof.SelectedProductCommits = previous.Repositories.commits()
	proof.SelectedProductSourceLockSha256 = previous.SourceLockSha256
	proof.SelectedProductReviewId, proof.SelectedProductReviewDigest, proof.SelectedProductPatchSha256 = nil, "", ""
	commits := previous.Repositories.commits()
	commits["server"] = proof.InstrumentedServerCommit
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	generation.Kind, generation.SourceLockSha256 = "trusted_instrument_transition", lock
	generation.Repositories = sourceRepositoriesFromCommits(commits)
	generation.SourceApprovalId, generation.SourceApprovalDigest, generation.EvaluatedBaseSourceLockSha256, generation.PatchSha256, generation.ScoreSha256 = "", "", "", "", ""
	generation.Significance = nil
	generation.SignificantImprovementPercent, generation.ThresholdEvidenceSha256 = previous.SignificantImprovementPercent, previous.ThresholdEvidenceSha256
	round := &manifest.EvaluationSource.Rounds[1]
	round.Kind, round.SourceLockSha256, round.SignificantImprovementPercent = generation.Kind, lock, previous.SignificantImprovementPercent
	round.Reason = "synthetic_no_reward_winner"
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal("instrument-only carry acquired a product review or changed its incumbent", err)
	}
}

// The old source contract remains server-only. New policy support must not
// remove its independent nonserver guard or reinterpret a legacy generation.
func TestG2DraftInstrumentManifestRetainsLegacyDependencyGuard(t *testing.T) {
	manifest := g2DraftReviewedDependencyManifest(t)
	manifest.EvaluationSource.Generations[1].G2VolumePolicy = model.CompetitionG2VolumePolicyLegacy
	manifest.EvaluationSource.Generations[1].TrustedInstrument = nil
	manifest.EvaluationSource.Rounds[1].G2VolumePolicy = model.CompetitionG2VolumePolicyLegacy
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("legacy dependency guard was broadened by the new instrument")
	}
}

// The adoption proof stays with generation one. A later real review may
// replace a server file, a mono-root dependency, or only the dependency.
func g2DraftMinimumWorkSuccessor(t *testing.T, changeServer bool) *sourceManifest {
	t.Helper()
	manifest := g2DraftReviewedDependencyManifest(t)
	previous := manifest.EvaluationSource.Generations[1]
	commits := previous.Repositories.commits()
	commits["connect"] = strings.Repeat("4", 40)
	if changeServer {
		commits["server"] = strings.Repeat("5", 40)
	}
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	reviewId := server.Id{14: 1}
	significance := validSourceSignificance()
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedSourceGeneration{
		Generation: 2, Kind: "reviewed_source_promotion", G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork,
		SourceLockSha256: lock, SignificantImprovementPercent: significance.RecommendedNextEpochTakeoverMarginPercent,
		ThresholdEvidenceSha256: significance.ScoreSha256, Repositories: sourceRepositoriesFromCommits(commits),
		SourceApprovalId: reviewId.String(), SourceApprovalDigest: strings.Repeat("6", 64),
		EvaluatedBaseSourceLockSha256: previous.SourceLockSha256, PatchSha256: strings.Repeat("7", 64),
		ScoreSha256: significance.ScoreSha256, Significance: significance})
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{
		Epoch: 3, SourceGeneration: 2, ActivationGeneration: 3, SourceDecisionDigest: strings.Repeat("8", 64),
		Kind: "reviewed_source_promotion", Reason: "synthetic_reviewed_minimum_work_successor", SourceLockSha256: lock,
		SignificantImprovementPercent: significance.RecommendedNextEpochTakeoverMarginPercent, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork})
	return manifest
}

// The first stale guard previously rejected a genuine reviewed dependency
// change even after a valid one-time minimum-work adoption was in its history.
func TestG2DraftMinimumWorkEpochThreeAcceptsReviewedServerAndDependencyWinner(t *testing.T) {
	manifest := g2DraftMinimumWorkSuccessor(t, true)
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal("inherited minimum-work rejected a reviewed server-and-dependency winner", err)
	}
}

// The second stale guard additionally demanded a server commit even when the
// actual approved mono-root winner changed only its selected dependency.
func TestG2DraftMinimumWorkEpochThreeAcceptsReviewedDependencyOnlyWinner(t *testing.T) {
	manifest := g2DraftMinimumWorkSuccessor(t, false)
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal("inherited minimum-work demanded a fabricated server change", err)
	}
}

// The continuation exception cannot manufacture adoption, waive a genuine
// review, change its incumbent, bless an unbound graph, or downgrade policy.
func TestG2DraftMinimumWorkSuccessorRejectsForgedOrDowngradedLineage(t *testing.T) {
	for _, mutation := range []string{"missing adoption", "repeated adoption", "missing review", "bad review digest", "obsolete base", "unbound graph", "unchanged graph", "downgrade", "unknown policy"} {
		manifest := g2DraftMinimumWorkSuccessor(t, false)
		generation := &manifest.EvaluationSource.Generations[2]
		switch mutation {
		case "missing adoption":
			manifest.EvaluationSource.Generations[1].TrustedInstrument = nil
		case "repeated adoption":
			generation.TrustedInstrument = manifest.EvaluationSource.Generations[1].TrustedInstrument
		case "missing review":
			generation.SourceApprovalId = ""
		case "bad review digest":
			generation.SourceApprovalDigest = "not-a-review-digest"
		case "obsolete base":
			generation.EvaluatedBaseSourceLockSha256 = manifest.EvaluationSource.Generations[0].SourceLockSha256
		case "unbound graph":
			generation.Repositories.Connect.Commit = strings.Repeat("9", 40)
		case "unchanged graph":
			previous := manifest.EvaluationSource.Generations[1]
			generation.Repositories, generation.SourceLockSha256 = previous.Repositories, previous.SourceLockSha256
			manifest.EvaluationSource.Rounds[2].SourceLockSha256 = previous.SourceLockSha256
		case "downgrade":
			generation.G2VolumePolicy, manifest.EvaluationSource.Rounds[2].G2VolumePolicy = model.CompetitionG2VolumePolicyLegacy, model.CompetitionG2VolumePolicyLegacy
		case "unknown policy":
			generation.G2VolumePolicy, manifest.EvaluationSource.Rounds[2].G2VolumePolicy = "unknown/1", "unknown/1"
		}
		if err := validateSourceManifest(manifest); err == nil {
			t.Errorf("inherited minimum-work accepted %s", mutation)
		}
	}
}

// Self-consistent outer hashes cannot replace the selected reviewed graph,
// omit its review or change policy under the operator's instrument approval.
func TestG2DraftInstrumentManifestRejectsUnboundGraphAndReview(t *testing.T) {
	for _, mutation := range []string{"dependency", "review", "missing proof", "policy"} {
		manifest := g2DraftReviewedDependencyManifest(t)
		generation := &manifest.EvaluationSource.Generations[1]
		switch mutation {
		case "dependency":
			commits := generation.Repositories.commits()
			commits["connect"] = strings.Repeat("2", 40)
			generation.Repositories = sourceRepositoriesFromCommits(commits)
			lock, err := model.CompetitionSourceLockDigest(commits)
			if err != nil {
				t.Fatal(err)
			}
			generation.SourceLockSha256, manifest.EvaluationSource.Rounds[1].SourceLockSha256 = lock, lock
		case "review":
			generation.SourceApprovalDigest = strings.Repeat("3", 64)
		case "missing proof":
			generation.TrustedInstrument = nil
		case "policy":
			generation.G2VolumePolicy = "unknown/1"
			manifest.EvaluationSource.Rounds[1].G2VolumePolicy = "unknown/1"
		}
		if err := validateSourceManifest(manifest); err == nil {
			t.Errorf("new manifest accepted %s substitution", mutation)
		}
	}
}

// The appended raw-lock leaf preserves complete historical bytes, while the
// actual image loader refuses null, aliases and duplicate prospective leaves.
func TestWorkCoverageRawSourceLockKeepsLegacyBytesAndFullPolicy(t *testing.T) {
	commits := reviewedSourceTestManifest(t).EvaluationSource.Generations[0].Repositories.commits()
	for _, pathBase := range []string{"", "mono_root_v1"} {
		legacy := struct {
			Schema              int               `json:"schema"`
			DevelopmentSnapshot bool              `json:"development_snapshot"`
			CandidatePathBase   string            `json:"candidate_path_base,omitempty"`
			Repositories        map[string]string `json:"repositories"`
		}{Schema: 1, CandidatePathBase: pathBase, Repositories: commits}
		expected, err := json.MarshalIndent(legacy, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := json.MarshalIndent(evaluatorSourceLock{Schema: 1, CandidatePathBase: pathBase, Repositories: commits}, "", "  ")
		if err != nil || !bytes.Equal(expected, actual) {
			t.Fatal("raw-lock source leaf changed historical complete serialization", err)
		}
	}
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	valid, err := json.Marshal(map[string]any{"schema": 1, "development_snapshot": false, "repositories": commits, "work_coverage_policy": coverage})
	if err != nil {
		t.Fatal(err)
	}
	for index, content := range [][]byte{
		valid,
		bytes.Replace(valid, []byte(`"work_coverage_policy":`), []byte(`"Work_Coverage_Policy":`), 1),
		bytes.Replace(valid, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"work_coverage_policy":`), 1),
		bytes.Replace(valid, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"unexpected":`), 1),
	} {
		path := filepath.Join(t.TempDir(), "source-lock.json")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		lock, err := loadEvaluatorSourceLock(path)
		if index == 0 {
			if err != nil {
				t.Fatal("complete prospective raw policy was rejected", err)
			}
			content, err := json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(content, &fields); err != nil {
				t.Fatal(err)
			}
			var observed model.CompetitionWorkCoveragePolicy
			if err := json.Unmarshal(fields["work_coverage_policy"], &observed); err != nil || observed != coverage {
				t.Fatal("actual source loader erased its complete raw policy", err)
			}
		} else if err == nil {
			t.Fatal("actual source loader accepted an ambiguous raw policy", index)
		}
	}
}

// Fully valid synthetic source generations bind the raw policy before Git.
// Matching policy reaches the expected absent repository, never fake success.
func TestWorkCoverageRawSourceLockBindsGenerationBeforeGit(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	for _, fixture := range []struct {
		expected *model.CompetitionWorkCoveragePolicy
		stored   *model.CompetitionWorkCoveragePolicy
		matches  bool
	}{
		{expected: &coverage},
		{stored: &coverage},
		{expected: &coverage, stored: &coverage, matches: true},
	} {
		manifest := g2DraftReviewedDependencyManifest(t)
		generation := &manifest.EvaluationSource.Generations[1]
		generation.WorkCoveragePolicy = fixture.expected.Clone()
		manifest.EvaluationSource.Rounds[1].WorkCoveragePolicy = fixture.expected.Clone()
		approval := &generation.TrustedInstrument.Approval
		approval.WorkCoveragePolicy, approval.ApprovalSha256 = fixture.expected.Clone(), ""
		var err error
		approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSourceManifest(manifest); err != nil {
			t.Fatal("raw-policy boundary fixture is not a valid synthetic generation", err)
		}
		raw := map[string]any{"schema": 1, "development_snapshot": false, "repositories": generation.Repositories.commits()}
		if fixture.stored != nil {
			raw["work_coverage_policy"] = fixture.stored.Clone()
		}
		content, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "source-lock.json")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		err = verifySourceEpoch(manifest, generation.Generation, root, path)
		if fixture.matches {
			if err == nil || !strings.Contains(err.Error(), "repository server unavailable") {
				t.Fatal("matching policy did not reach genuine source inspection", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "coverage differs") {
			t.Fatal("raw-policy mismatch reached Git or bypassed its generation", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("source policy checks mutated the empty repository root", err)
		}
	}
}

// Source-leaf support must not import unrelated later staging tolerances.
// These exact rejections belong to the frozen instrument's significance rule.
func TestG2DraftSignificancePreservesFrozenStagingRejections(t *testing.T) {
	base := validSourceSignificance()
	base.TakeoverMarginPercent = 0
	if err := validateSourceSignificanceForMode(base, true); err != nil {
		t.Fatal("frozen valid staging significance was rejected", err)
	}
	for _, mutate := range []func(*sourceSignificance){
		func(value *sourceSignificance) { value.ObservedImprovementPercent = 0 },
		func(value *sourceSignificance) { value.OneSidedPValue = .5 },
		func(value *sourceSignificance) { value.RecommendedNextEpochTakeoverMarginPercent = 51 },
	} {
		changed := *base
		mutate(&changed)
		if err := validateSourceSignificanceForMode(&changed, true); err == nil {
			t.Fatal("future raw-policy port weakened frozen staging significance")
		}
	}
}
