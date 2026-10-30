// Synthetic local graphs exercise operator instrumentation without real build,
// review, publication, deployment, model execution or qualification claims.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Approval is sealed before the winner commit even exists.
func sourceTrustedInstrumentFixture(t *testing.T) (string, string, *model.CompetitionTrustedInstrumentApproval) {
	t.Helper()
	root := t.TempDir()
	sourceTestRepository(t, root, "server")
	repository := filepath.Join(root, "server")
	productPath := filepath.Join(repository, "connect", "synthetic_product.go")
	if err := os.WriteFile(productPath, []byte("package connect\nvar syntheticProduct = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/synthetic_product.go")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic product base")
	base := sourceTestGit(t, repository, "rev-parse", "HEAD")
	mainPath := filepath.Join(repository, "connect", "sim-latency", "main.go")
	if err := os.WriteFile(mainPath, []byte("package main\n// synthetic minimum-work instrument\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch, err := sourceGitBytes(context.Background(), repository, maximumSourceConfigBytes, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-renames", base, "--")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commits := sourceTestCommits(strings.Repeat("a", 40))
	commits["server"] = base
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	patchHash := sha256.Sum256(patch)
	approval := &model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-instrument-approval", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, InstrumentBaseCommits: commits, InstrumentBaseSourceLockSha256: lock,
		InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64), PatchSha256: hex.EncodeToString(patchHash[:]), Patch: patch,
		ProtectedPaths: []string{"connect/sim-latency/main.go"}}
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(approval); err != nil {
		t.Fatal(err)
	}
	return repository, base, approval
}

// The unknown winner remains the real parent, with its product edit intact;
// the original approved instrument base never becomes the product selection.
func TestTrustedInstrumentOverlayPreservesUnknownWinnerGraph(t *testing.T) {
	repository, base, approval := sourceTrustedInstrumentFixture(t)
	product := filepath.Join(repository, "connect", "synthetic_product.go")
	if err := os.WriteFile(product, []byte("package connect\nvar syntheticProduct = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/synthetic_product.go")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "previously unknown synthetic winner")
	winner := sourceTestGit(t, repository, "rev-parse", "HEAD")
	tree, err := trustedInstrumentTree(context.Background(), repository, winner, approval)
	if err != nil {
		t.Fatal(err)
	}
	child, err := trustedInstrumentCommit(context.Background(), repository, winner, tree, approval.ApprovalSha256)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceTestGit(t, repository, "rev-parse", "HEAD"); got != winner {
		t.Fatal("isolated instrumentation changed the selected checkout", got)
	}
	selected := sourceTestCommits(strings.Repeat("a", 40))
	selected["server"] = winner
	lock, _ := model.CompetitionSourceLockDigest(selected)
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: *approval, SelectedProductKind: "reviewed_source_promotion",
		SelectedProductCommits: selected, SelectedProductSourceLockSha256: lock, InstrumentedServerCommit: child}
	if err := verifyTrustedInstrumentChild(context.Background(), repository, proof); err != nil {
		t.Fatal(err)
	}
	actual, err := sourceGitBlobAtCommit(context.Background(), repository, child, "connect/synthetic_product.go", maximumSourceConfigBytes)
	if err != nil || string(actual) != "package connect\nvar syntheticProduct = 2\n" {
		t.Fatal("instrumentation replaced selected winner product bytes", err)
	}
	again, err := trustedInstrumentCommit(context.Background(), repository, winner, tree, approval.ApprovalSha256)
	if err != nil || child != again || child == base || child == winner {
		t.Fatal("instrument child is not deterministic and distinct", err)
	}
	parents := sourceTestGit(t, repository, "rev-list", "--parents", "-n", "1", child)
	if parents != child+" "+winner {
		t.Fatal("instrument child bypassed actual winner lineage", parents)
	}
}

// The same approval automatically supports an incumbent-only selection, with
// a distinct new generation and no invented candidate review or score.
func TestTrustedInstrumentCarryBuildProtocolAndManifest(t *testing.T) {
	repository, base, approval := sourceTrustedInstrumentFixture(t)
	tree, err := trustedInstrumentTree(context.Background(), repository, base, approval)
	if err != nil {
		t.Fatal(err)
	}
	child, err := trustedInstrumentCommit(context.Background(), repository, base, tree, approval.ApprovalSha256)
	if err != nil {
		t.Fatal(err)
	}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: *approval, SelectedProductKind: "reviewed_source_carry_forward",
		SelectedProductCommits: approval.InstrumentBaseCommits, SelectedProductSourceLockSha256: approval.InstrumentBaseSourceLockSha256, InstrumentedServerCommit: child}
	commits := sourceTestCommits(strings.Repeat("a", 40))
	commits["server"] = child
	lock, _ := model.CompetitionSourceLockDigest(commits)
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	baseline := &manifest.EvaluationSource.Generations[0]
	baseline.Repositories, baseline.SourceLockSha256 = sourceRepositoriesFromCommits(approval.InstrumentBaseCommits), approval.InstrumentBaseSourceLockSha256
	manifest.EvaluationSource.Rounds[0].SourceLockSha256 = baseline.SourceLockSha256
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedSourceGeneration{
		Generation: 1, Kind: "trusted_instrument_transition", SourceLockSha256: lock, SignificantImprovementPercent: baseline.SignificantImprovementPercent,
		ThresholdEvidenceSha256: baseline.ThresholdEvidenceSha256, Repositories: sourceRepositoriesFromCommits(commits),
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, TrustedInstrument: proof})
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{Epoch: 2, SourceGeneration: 1, ActivationGeneration: 2,
		SourceDecisionDigest: strings.Repeat("c", 64), Kind: "trusted_instrument_transition", Reason: "no_reward_winner", SourceLockSha256: lock,
		SignificantImprovementPercent: baseline.SignificantImprovementPercent, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork})
	content, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{}
	for _, name := range sourceRepositoryNames() {
		roots[name] = filepath.Join(filepath.Dir(repository), name)
	}
	request := reviewedSourceBuildRequest{Operation: "isolated_trusted_instrument_build", TrustedInstrument: proof,
		Proposal: &reviewedSourceProposal{Schema: 2, BaseSourceLockSha256: proof.SelectedProductSourceLockSha256, SourceLockSha256: lock,
			Commits: commits, StagedRoots: roots, PatchSha256: approval.PatchSha256, EvaluatedImageDigest: approval.InstrumentBaseImageDigest},
		SourceManifest: content, SourceGeneration: 1, ConfigLocalDirectory: "/synthetic/config/local", VaultLocalDirectory: "/synthetic/vault/local", SandboxContract: "offline_evaluator_containment_v1"}
	if err := validateSourceBuildRequest(request); err != nil {
		t.Fatal(err)
	}
	plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("d", 64))
	if err != nil || !strings.Contains(string(plan.Dockerfile), "COPY --chmod=0555 source/server/connect/sim-latency/official-run.sh /usr/local/libexec/competition-official-run\n") ||
		!strings.Contains(string(plan.Dockerfile), "COPY --chmod=0555 source/server/connect/sim-latency/evaluator/container/entrypoint.sh /usr/local/libexec/competition-container\n") {
		t.Fatal("instrument image inherited epoch-one runner or entrypoint", err)
	}
	request.Operation = "isolated_reviewed_source_build"
	if err := validateSourceBuildRequest(request); err == nil {
		t.Fatal("instrument carry masqueraded as a genuinely reviewed candidate")
	}
	request.Operation = "isolated_trusted_instrument_build"
	request.Proposal.ReviewId = server.NewId()
	if err := validateSourceBuildRequest(request); err == nil {
		t.Fatal("instrument protocol accepted a fabricated review identity")
	}
}

// A winner touching any protected preimage conflicts even when its change is
// outside the patch hunk. Git fuzz/rebase cannot conceal instrument drift.
func TestTrustedInstrumentRejectsProtectedPreimageConflict(t *testing.T) {
	repository, _, approval := sourceTrustedInstrumentFixture(t)
	if err := os.WriteFile(filepath.Join(repository, "connect", "sim-latency", "main.go"), []byte("package main\n// independently changed protected source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/sim-latency/main.go")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic protected preimage conflict")
	winner := sourceTestGit(t, repository, "rev-parse", "HEAD")
	if _, err := trustedInstrumentTree(context.Background(), repository, winner, approval); err == nil || !strings.Contains(err.Error(), "protected preimage conflict") {
		t.Fatal("protected conflict did not fail closed at the preimage boundary", err)
	}
}

// Legacy publication remains unable to alter the evaluator; only the explicit
// protected-child proof extends that rule for the operator-approved operation.
func TestTrustedInstrumentPublicationRequiresExplicitProof(t *testing.T) {
	repository, base, approval := sourceTrustedInstrumentFixture(t)
	tree, err := trustedInstrumentTree(context.Background(), repository, base, approval)
	if err != nil {
		t.Fatal(err)
	}
	child, err := trustedInstrumentCommit(context.Background(), repository, base, tree, approval.ApprovalSha256)
	if err != nil {
		t.Fatal(err)
	}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: *approval, SelectedProductKind: "reviewed_source_carry_forward",
		SelectedProductCommits: approval.InstrumentBaseCommits, SelectedProductSourceLockSha256: approval.InstrumentBaseSourceLockSha256, InstrumentedServerCommit: child}
	ref := sourcePublicationRef{Repository: "server", Branch: "sim-latency", ExpectedCommit: base, Commit: child, StagedRoot: repository}
	backend := &gitSourcePublicationBackend{}
	if err := backend.Verify(context.Background(), ref, &sourcePublicationIntent{}); err == nil {
		t.Fatal("legacy publication accepted protected evaluator change")
	}
	if err := backend.Verify(context.Background(), ref, &sourcePublicationIntent{TrustedInstrument: proof}); err != nil {
		t.Fatal(err)
	}
	proof.InstrumentedServerCommit = base
	if err := backend.Verify(context.Background(), ref, &sourcePublicationIntent{TrustedInstrument: proof}); err == nil {
		t.Fatal("publication accepted substituted child lineage")
	}
}

// Product verification runs first with the genuine winner patch, and only its
// exact resulting commit may parent the independent protected instrument child.
func TestTrustedInstrumentPublicationVerifiesActualWinnerPatch(t *testing.T) {
	repository, base, approval := sourceTrustedInstrumentFixture(t)
	productPath := filepath.Join(repository, "connect", "synthetic_product.go")
	if err := os.WriteFile(productPath, []byte("package connect\nvar syntheticProduct = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/synthetic_product.go")
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic approved winner")
	winner := sourceTestGit(t, repository, "rev-parse", "HEAD")
	policy := controller.PatchPolicy{MaxPatchBytes: 4096, AllowedPaths: []string{"connect/synthetic_product.go"}, ForbiddenPaths: []string{"connect/sim-latency/**"}}
	rawPatch := "diff --git a/connect/synthetic_product.go b/connect/synthetic_product.go\n--- a/connect/synthetic_product.go\n+++ b/connect/synthetic_product.go\n@@ -1,2 +1,2 @@\n package connect\n-var syntheticProduct = 1\n+var syntheticProduct = 2\n"
	patch, patchError := controller.ValidateAndCanonicalizePatch(rawPatch, policy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	patchPath := filepath.Join(t.TempDir(), "selected-product.patch")
	if err := os.WriteFile(patchPath, patch.Bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := trustedInstrumentTree(context.Background(), repository, winner, approval)
	if err != nil {
		t.Fatal(err)
	}
	child, err := trustedInstrumentCommit(context.Background(), repository, winner, tree, approval.ApprovalSha256)
	if err != nil {
		t.Fatal(err)
	}
	selected := sourceTestCommits(strings.Repeat("a", 40))
	selected["server"] = winner
	lock, _ := model.CompetitionSourceLockDigest(selected)
	id := server.NewId()
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: *approval, SelectedProductKind: "reviewed_source_promotion", SelectedProductCommits: selected,
		SelectedProductSourceLockSha256: lock, SelectedProductReviewId: &id, SelectedProductReviewDigest: strings.Repeat("c", 64), SelectedProductPatchSha256: patch.Sha256, InstrumentedServerCommit: child}
	ref := sourcePublicationRef{Repository: "server", Branch: "sim-latency", ExpectedCommit: base, Commit: child, StagedRoot: repository}
	intent := &sourcePublicationIntent{TrustedInstrument: proof, ApprovedPatchPath: patchPath, ApprovedPatchSha256: patch.Sha256}
	backend := &gitSourcePublicationBackend{patchPolicy: policy}
	if err := backend.Verify(context.Background(), ref, intent); err != nil {
		t.Fatal(err)
	}
	intent.ApprovedPatchSha256 = strings.Repeat("f", 64)
	if err := backend.Verify(context.Background(), ref, intent); err == nil {
		t.Fatal("instrument approval concealed a substituted selected product patch")
	}
}

// Exact approved bytes, paths and original source provenance are independently
// bound, including paths that would otherwise be ordinary product files.
func TestTrustedInstrumentRejectsTamperedApproval(t *testing.T) {
	_, _, approval := sourceTrustedInstrumentFixture(t)
	for _, mutate := range []func(*model.CompetitionTrustedInstrumentApproval){
		func(value *model.CompetitionTrustedInstrumentApproval) {
			value.Patch = append([]byte("\n"), value.Patch...)
		},
		func(value *model.CompetitionTrustedInstrumentApproval) {
			value.ProtectedPaths = []string{"connect/synthetic_product.go"}
		},
		func(value *model.CompetitionTrustedInstrumentApproval) {
			value.InstrumentBaseSourceLockSha256 = strings.Repeat("f", 64)
		},
		func(value *model.CompetitionTrustedInstrumentApproval) {
			value.InstrumentBaseImageDigest = "sha256:" + strings.Repeat("f", 64)
		},
		func(value *model.CompetitionTrustedInstrumentApproval) { value.TargetEpoch = 1 },
	} {
		value := *approval
		mutate(&value)
		if err := controller.ValidateCompetitionTrustedInstrumentApproval(&value); err == nil {
			t.Fatal("tampered independently approved instrument content accepted")
		}
	}
}
