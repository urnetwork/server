// V2 carries reviewed implementation identity across fresh competition rounds
// without inheriting an unshipped winner's variance or changing legacy history.
package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// Constructs a synthetic approved baseline independent of all live repos.
func reviewedSourceTestManifest(t *testing.T) *sourceManifest {
	t.Helper()
	commits := sourceTestCommits(strings.Repeat("a", 40))
	manifest := sourceTestManifest(commits)
	manifest.Schema = 2
	lockDigest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Generations = []reviewedSourceGeneration{{
		Generation: 0, Kind: "baseline", SourceLockSha256: lockDigest, SignificantImprovementPercent: 16.1,
		ThresholdEvidenceSha256: strings.Repeat("b", 64), Repositories: sourceRepositoriesFromCommits(commits),
	}}
	manifest.EvaluationSource.Rounds = []reviewedSourceRound{{
		Epoch: 1, SourceGeneration: 0, ActivationGeneration: 1, SourceDecisionDigest: strings.Repeat("c", 64),
		Kind: "reviewed_source_carry_forward", Reason: "initial_reviewed_source", SourceLockSha256: lockDigest, SignificantImprovementPercent: 16.1,
	}}
	return manifest
}

// A genuine reward winner does not force its unreviewed code into the next
// source generation; all six mappings can keep the approved margin unchanged.
func TestReviewedSourceCarryForwardWithRewardWinner(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	for epoch := 2; epoch <= 6; epoch++ {
		previous := manifest.EvaluationSource.Rounds[len(manifest.EvaluationSource.Rounds)-1]
		previous.Epoch, previous.ActivationGeneration = epoch, int64(epoch)
		previous.RewardDecisionDigest = strings.Repeat("d", 64)
		previous.Reason = "review_pending"
		manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, previous)
	}
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal(err)
	}
	round, err := manifest.sourceRound(6)
	if err != nil || round.SourceGeneration != 0 || round.SignificantImprovementPercent != 16.1 {
		t.Fatalf("carry-forward mapping changed: %+v %v", round, err)
	}
	generation, err := manifest.epoch(0)
	if err != nil || generation.Repositories != manifest.EvaluationSource.Epochs[0].Repositories {
		t.Fatalf("source generation lookup changed legacy source: %+v %v", generation, err)
	}
	if _, err := manifest.epoch(5); err == nil {
		t.Fatal("round arithmetic invented a source generation")
	}
}

// Historical v1 entries remain untouched, while v2 maps exact generations.
func TestReviewedSourceCarryForwardRejectsUnshippedWinnerMargin(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Rounds[0].SignificantImprovementPercent = 20
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("unshipped winner changed incumbent margin")
	}
}

// A syntactically valid digest cannot disguise a changed dependency.
func TestReviewedSourceDigestBindsEntireRepositoryMap(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].Repositories.Warp.Commit = strings.Repeat("e", 40)
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("changed repository escaped source lock digest")
	}
}

// Approval must refer to the exact evaluated incumbent, not merely a reward
// identity. A late review cannot silently rebase after another source wins.
func TestReviewedSourcePromotionRejectsObsoleteBaseAndMissingApproval(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	base := manifest.EvaluationSource.Generations[0]
	commits := base.Repositories.commits()
	commits["server"] = strings.Repeat("e", 40)
	digest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	significance := validSourceSignificance()
	promotion := reviewedSourceGeneration{
		Generation: 1, Kind: "reviewed_source_promotion", Repositories: sourceRepositoriesFromCommits(commits), SourceLockSha256: digest,
		SignificantImprovementPercent: significance.RecommendedNextEpochTakeoverMarginPercent, ThresholdEvidenceSha256: significance.ScoreSha256,
		SourceApprovalId: "synthetic-source-approval", SourceApprovalDigest: strings.Repeat("f", 64),
		EvaluatedBaseSourceLockSha256: base.SourceLockSha256, PatchSha256: strings.Repeat("a", 64), ScoreSha256: significance.ScoreSha256, Significance: significance,
	}
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, promotion)
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Generations[1].EvaluatedBaseSourceLockSha256 = strings.Repeat("d", 64)
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("obsolete evaluated base was accepted")
	}
	manifest.EvaluationSource.Generations[1].EvaluatedBaseSourceLockSha256 = base.SourceLockSha256
	manifest.EvaluationSource.Generations[1].SourceApprovalId = ""
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("reward identity substituted for source approval")
	}
}

// A legacy parser path must never silently reinterpret a v2 mapping.
func TestLegacySourceManifestRejectsV2Fields(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	manifest.Schema = 1
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("legacy manifest accepted v2 source semantics")
	}
}
