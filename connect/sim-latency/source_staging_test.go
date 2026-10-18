// Synthetic v2 staging regressions never read live policy, alter real refs,
// approve a source, or execute a deployment adapter.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Builds a new independent eleven-repository baseline, not relabeled history.
func stagingSourceTestManifest(t *testing.T) *sourceManifest {
	t.Helper()
	manifest := reviewedSourceTestManifest(t)
	manifest.Staging = true
	manifest.EvaluationSource.Branch = stagingEvaluationSourceBranch
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].SignificantImprovementPercent = 0
	manifest.EvaluationSource.Rounds[0].SignificantImprovementPercent = 0
	return manifest
}

// Six staging rounds carry the separately frozen zero-margin source unchanged.
func TestStagingSourceManifestCarriesIndependentBaseline(t *testing.T) {
	manifest := stagingSourceTestManifest(t)
	for epoch := 2; epoch <= maximumCompetitionEpoch; epoch++ {
		round := manifest.EvaluationSource.Rounds[0]
		round.Epoch, round.ActivationGeneration = epoch, int64(epoch)
		round.Reason, round.RewardDecisionDigest = "review_pending", strings.Repeat("d", 64)
		manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, round)
	}
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.EvaluationSource.Generations) != 1 || manifest.EvaluationSource.Rounds[5].SourceGeneration != 0 || manifest.EvaluationSource.Rounds[5].SignificantImprovementPercent != 0 {
		t.Fatal("staging carry-forward promoted an unreviewed winner or changed its margin")
	}
}

// Explicit mode cannot be inferred from margin, branch, or historical content.
func TestStagingSourceManifestRejectsCrossModeAndHistoricalRelabeling(t *testing.T) {
	mutations := []func(*sourceManifest){
		func(value *sourceManifest) { value.Staging = false },
		func(value *sourceManifest) { value.EvaluationSource.Branch = "sim-latency" },
		func(value *sourceManifest) { value.EvaluationSource.Generations[0].SignificantImprovementPercent = 0.1 },
		func(value *sourceManifest) { value.EvaluationSource.Rounds[0].SignificantImprovementPercent = 0.1 },
		func(value *sourceManifest) {
			value.EvaluationSource.Epochs = reviewedSourceTestManifest(t).EvaluationSource.Epochs
		},
		func(value *sourceManifest) { value.Schema = 1 },
	}
	for index, mutate := range mutations {
		manifest := stagingSourceTestManifest(t)
		mutate(manifest)
		if err := validateSourceManifest(manifest); err == nil {
			t.Fatalf("cross-mode staging manifest %d accepted", index)
		}
	}
	production := reviewedSourceTestManifest(t)
	production.EvaluationSource.Epochs = nil
	production.EvaluationSource.Generations[0].SignificantImprovementPercent = 0
	production.EvaluationSource.Rounds[0].SignificantImprovementPercent = 0
	if err := validateSourceManifest(production); err == nil {
		t.Fatal("zero production margin accepted")
	}
}

// Statistical recommendation stays authenticated evidence; it is not adopted
// as a nonzero staging threshold and never replaces separate source approval.
func TestStagingSourcePromotionRequiresApprovalAndKeepsZeroMargin(t *testing.T) {
	manifest := stagingSourceTestManifest(t)
	base := manifest.EvaluationSource.Generations[0]
	commits := base.Repositories.commits()
	commits["server"] = strings.Repeat("e", 40)
	lockDigest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	significance := validSourceSignificance()
	significance.TakeoverMarginPercent = 0
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedSourceGeneration{
		Generation: 1, Kind: "reviewed_source_promotion", Repositories: sourceRepositoriesFromCommits(commits), SourceLockSha256: lockDigest,
		SignificantImprovementPercent: 0, ThresholdEvidenceSha256: significance.ScoreSha256,
		SourceApprovalId: "synthetic-reviewed-stage", SourceApprovalDigest: strings.Repeat("f", 64),
		EvaluatedBaseSourceLockSha256: base.SourceLockSha256, PatchSha256: strings.Repeat("a", 64), ScoreSha256: significance.ScoreSha256, Significance: significance,
	})
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if significance.RecommendedNextEpochTakeoverMarginPercent == 0 {
		t.Fatal("test lost its positive statistical recommendation")
	}
	manifest.EvaluationSource.Generations[1].SourceApprovalId = ""
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("staging promoted source without genuine approval")
	}
	manifest.EvaluationSource.Generations[1].SourceApprovalId = "synthetic-reviewed-stage"
	significance.TakeoverMarginPercent = 16.1
	if err := validateSourceManifest(manifest); err == nil {
		t.Fatal("production score was relabeled as staging evidence")
	}
}

// Builds exact mode-bound recovery bytes with no real identifiers or policy.
func stagingSourceActivationFixture(t *testing.T) (*model.CompetitionSourceActivation, *controller.Settings) {
	t.Helper()
	manifest := stagingSourceTestManifest(t)
	content, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	intent := &model.CompetitionSourceActivation{Staging: true, StagingRoundWindowSeconds: 60, StagingPreparationSeconds: 0,
		Manifest: content, ManifestSha256: hex.EncodeToString(digest[:]), SourceLockSha256: manifest.EvaluationSource.Generations[0].SourceLockSha256,
		Commits: manifest.EvaluationSource.Generations[0].Repositories.commits()}
	settings := &controller.Settings{AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Staging: true, StagingRoundWindowSeconds: 60}}
	return intent, settings
}

// Recovery cannot reinterpret saved state using another mode or timing window.
func TestStagingSourceRecoveryBindsPolicyManifestAndTiming(t *testing.T) {
	intent, settings := stagingSourceActivationFixture(t)
	if err := validateSourceActivationMode(intent, settings); err != nil {
		t.Fatal(err)
	}
	for index, mutate := range []func(*model.CompetitionSourceActivation){
		func(value *model.CompetitionSourceActivation) { value.Staging = false },
		func(value *model.CompetitionSourceActivation) { value.StagingRoundWindowSeconds++ },
		func(value *model.CompetitionSourceActivation) { value.StagingPreparationSeconds++ },
		func(value *model.CompetitionSourceActivation) { value.Threshold = 0.161 },
		func(value *model.CompetitionSourceActivation) { value.ManifestSha256 = strings.Repeat("0", 64) },
		func(value *model.CompetitionSourceActivation) {
			value.Manifest = sourceConfigTestManifestBytes(t, false, false)
		},
	} {
		changed := *intent
		mutate(&changed)
		if err := validateSourceActivationMode(&changed, settings); err == nil {
			t.Fatalf("staging recovery mismatch %d accepted", index)
		}
	}
	settings.AutomationPolicy.Staging = false
	settings.AutomationPolicy.StagingRoundWindowSeconds = 0
	if err := validateSourceActivationMode(intent, settings); err == nil {
		t.Fatal("staging recovery accepted under production policy")
	}
}

// Changed server source still requires exact approval and isolated stage proof.
func TestStagingReviewedSourceChoiceRejectsCrossModeAndUnapprovedProof(t *testing.T) {
	current, proposal, deadline := reviewedProposalFixture(t)
	current.Staging, current.Threshold = true, 0
	proposal.Staging, proposal.BuildReceipt.Staging = true, true
	proposal.Significance.TakeoverMarginPercent = 0
	if chosen := selectReviewedSourceProposal([]reviewedSourceProposal{proposal}, current, deadline); chosen == nil {
		t.Fatal("genuinely approved synthetic staging proposal was rejected")
	}
	for index, mutate := range []func(*reviewedSourceProposal){
		func(value *reviewedSourceProposal) { value.Staging = false },
		func(value *reviewedSourceProposal) { value.BuildReceipt.Staging = false },
		func(value *reviewedSourceProposal) { value.BuildReceipt.NoMainCredentials = false },
		func(value *reviewedSourceProposal) { value.ReviewDecisionDigest = "" },
		func(value *reviewedSourceProposal) { value.ReadyAt = deadline.Add(1) },
	} {
		changed := proposal
		mutate(&changed)
		if selectReviewedSourceProposal([]reviewedSourceProposal{changed}, current, deadline) != nil {
			t.Fatalf("unsafe staging proposal %d accepted", index)
		}
	}
}

// Publication validation rejects either mode's refs before contacting a backend.
func TestStagingSourcePublicationCannotTouchProductionRefs(t *testing.T) {
	intent, backend := newSourcePublicationFixture()
	intent.Staging = true
	if err := reconcileSourcePublication(context.Background(), intent, backend); err == nil || len(backend.pushNames) != 0 {
		t.Fatal("staging intent accepted production source refs")
	}
	for index := range intent.Refs {
		if intent.Refs[index].Repository != "config" {
			intent.Refs[index].Branch = stagingEvaluationSourceBranch
		}
	}
	if err := reconcileSourcePublication(context.Background(), intent, backend); err != nil {
		t.Fatal(err)
	}
	if len(backend.pushNames) == 0 || backend.pushNames[len(backend.pushNames)-1] != "config" {
		t.Fatal("staging config did not publish last")
	}
	intent.Staging = false
	if err := validateSourcePublicationIntent(intent); err == nil {
		t.Fatal("staging journal was replayed as production")
	}
}

// Staging config CAS preserves production manifest bytes and only stages its
// dedicated path. No remote ref is updated by this private staging helper.
func TestStagingSourceConfigPreservesHistoricalProductionManifest(t *testing.T) {
	fixture := newSourceConfigFixtureForMode(t, true)
	production := []byte("synthetic_historical_production: independently_updated\n")
	remoteHead := fixture.advance(t, "main/sim-latency.yml", production)
	stagedRoot := filepath.Join(fixture.root, "stage-successor")
	next := sourceConfigTestManifestBytes(t, true, true)
	parent, commit, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, stagedRoot, fixture.current, next, "synthetic staging carry-forward")
	if err != nil || parent != remoteHead {
		t.Fatalf("staging config: %s %s %v", parent, commit, err)
	}
	content, err := sourceGitBytes(context.Background(), stagedRoot, maximumSourceConfigBytes, "show", commit+":main/sim-latency.yml")
	if err != nil || string(content) != string(production) {
		t.Fatal("staging changed historical production manifest")
	}
	digest := sha256.Sum256(next)
	journal := &sourcePublicationIntent{Staging: true, ManifestSha256: hex.EncodeToString(digest[:])}
	ref := sourcePublicationRef{Repository: "config", Branch: "main", ExpectedCommit: parent, Commit: commit, StagedRoot: stagedRoot}
	backend := &gitSourcePublicationBackend{}
	if err := backend.Verify(context.Background(), ref, journal); err != nil {
		t.Fatal(err)
	}
	journal.Staging = false
	if err := backend.Verify(context.Background(), ref, journal); err == nil {
		t.Fatal("staging config path was accepted by a production journal")
	}
	if head := sourceTestGit(t, fixture.remoteRoot, "rev-parse", "refs/heads/main"); head != remoteHead {
		t.Fatal("private staging mutated remote config")
	}
	wrongRoot := filepath.Join(fixture.root, "wrong-mode")
	if _, _, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, wrongRoot, fixture.current, sourceConfigTestManifestBytes(t, false, true), "synthetic cross-mode"); err == nil {
		t.Fatal("staging config accepted a production successor")
	}
	if _, err := os.Lstat(wrongRoot); !os.IsNotExist(err) {
		t.Fatal("cross-mode config reached Git staging")
	}
}

// New optional mode fields must not change historical production encodings.
func TestProductionSourceModeFieldsRemainOmitted(t *testing.T) {
	manifest, err := yaml.Marshal(reviewedSourceTestManifest(t))
	if err != nil || strings.Contains(string(manifest), "staging:") {
		t.Fatalf("production source encoding changed: %v", err)
	}
	journal, _ := newSourcePublicationFixture()
	content, err := json.Marshal(journal)
	if err != nil || strings.Contains(string(content), `"staging"`) {
		t.Fatalf("production journal encoding changed: %v", err)
	}
}
