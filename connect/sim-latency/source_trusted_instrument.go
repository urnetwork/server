// Trusted instrumentation is an exact protected-file overlay on the actually
// selected product graph, never a replacement graph from a moving main branch.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Both creation and publication rederive the approved result independently.
// The index lives outside the clone and cannot replace its checkout or refs.
func trustedInstrumentTree(ctx context.Context, repositoryRoot, selectedCommit string, approval *model.CompetitionTrustedInstrumentApproval) (string, error) {
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(approval); err != nil {
		return "", err
	}
	if !sourceGitShaPattern.MatchString(selectedCommit) {
		return "", errors.New("trusted instrument requires the exact selected product commit")
	}
	base := approval.InstrumentBaseCommits["server"]
	for _, commit := range []string{base, selectedCommit} {
		resolved, err := sourceGitText(ctx, repositoryRoot, "rev-parse", "--verify", commit+"^{commit}")
		if err != nil || resolved != commit {
			return "", errors.New("trusted instrument original provenance or selected product object is unavailable")
		}
	}
	for _, path := range approval.ProtectedPaths {
		original, originalErr := sourceGitBytes(ctx, repositoryRoot, maximumSourceConfigBytes, "ls-tree", "-z", base, "--", path)
		selected, selectedErr := sourceGitBytes(ctx, repositoryRoot, maximumSourceConfigBytes, "ls-tree", "-z", selectedCommit, "--", path)
		if originalErr != nil || selectedErr != nil || string(original) != string(selected) {
			return "", fmt.Errorf("trusted instrument protected preimage conflict at %s; retain the actual selected product and obtain a new operator approval", path)
		}
		if len(original) != 0 {
			fields := strings.Fields(strings.SplitN(string(original), "\t", 2)[0])
			if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
				return "", errors.New("trusted instrument protected preimage is not a regular committed file")
			}
		}
	}
	root, err := os.MkdirTemp("", "sim-latency-instrument-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	patchPath := filepath.Join(root, "approved.patch")
	if err := writePrivateReviewFile(patchPath, approval.Patch); err != nil {
		return "", err
	}
	runIndex := func(args ...string) ([]byte, error) {
		command, err := sourceGitLocalCommand(ctx, repositoryRoot, args...)
		if err != nil {
			return nil, err
		}
		command.Env = append(command.Env, "GIT_INDEX_FILE="+filepath.Join(root, "index"))
		output := &sourceBoundedWriter{limit: maximumSourceConfigBytes}
		command.Stdout, command.Stderr = output, &sourceBoundedWriter{limit: 4096}
		if err := command.Run(); err != nil {
			return nil, fmt.Errorf("exact trusted instrument patch failed (no merge or conflict recovery is permitted): %w", err)
		}
		return output.content, nil
	}
	if _, err := runIndex("read-tree", selectedCommit); err != nil {
		return "", err
	}
	if _, err := runIndex("apply", "--cached", "--whitespace=error-all", "--", patchPath); err != nil {
		return "", err
	}
	tree, err := runIndex("write-tree")
	if err != nil {
		return "", err
	}
	// Full-index, binary-safe diff bytes are the approval format. Comparison
	// refuses extra paths, missing hunks, fuzz, offsets and unnoticed changes.
	actual, err := sourceGitBytes(ctx, repositoryRoot, maximumSourceConfigBytes, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-renames", selectedCommit, strings.TrimSpace(string(tree)), "--")
	if err != nil || string(actual) != string(approval.Patch) {
		return "", errors.New("trusted instrument result differs from the exact approved canonical patch bytes")
	}
	return strings.TrimSpace(string(tree)), nil
}

// Fixed author, committer, clock and message yield one deterministic child for
// one selected graph and approval. No identity is borrowed from a product review.
func trustedInstrumentCommit(ctx context.Context, repositoryRoot, selectedCommit, tree, approvalDigest string) (string, error) {
	if !sourceGitShaPattern.MatchString(selectedCommit) || !sourceGitShaPattern.MatchString(tree) || !validSha256(approvalDigest) {
		return "", errors.New("trusted instrument child requires exact parent, tree and approval")
	}
	command, err := sourceGitLocalCommand(ctx, repositoryRoot, "-c", "i18n.commitEncoding=UTF-8", "commit-tree", tree, "-p", selectedCommit)
	if err != nil {
		return "", err
	}
	command.Env = append(command.Env, "GIT_AUTHOR_NAME=Competition instrument", "GIT_AUTHOR_EMAIL=instrument@competition.example", "GIT_AUTHOR_DATE="+model.CompetitionTrustedInstrumentCommitTime,
		"GIT_COMMITTER_NAME=Competition instrument", "GIT_COMMITTER_EMAIL=instrument@competition.example", "GIT_COMMITTER_DATE="+model.CompetitionTrustedInstrumentCommitTime)
	command.Stdin = strings.NewReader("competition: trusted instrument " + approvalDigest + "\n")
	output := &sourceBoundedWriter{limit: 4096}
	command.Stdout, command.Stderr = output, &sourceBoundedWriter{limit: 4096}
	if err := command.Run(); err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(output.content))
	if !sourceGitShaPattern.MatchString(commit) {
		return "", errors.New("trusted instrument child has an invalid immutable identity")
	}
	return commit, nil
}

// A verifier requires exact deterministic identity as well as the parent/tree;
// arbitrary merge commits and extra product edits cannot satisfy this proof.
func verifyTrustedInstrumentChild(ctx context.Context, repositoryRoot string, proof *model.CompetitionTrustedInstrumentProof) error {
	if proof == nil || proof.Schema != 1 {
		return errors.New("trusted instrument child proof is missing")
	}
	selected := proof.SelectedProductCommits["server"]
	tree, err := trustedInstrumentTree(ctx, repositoryRoot, selected, &proof.Approval)
	if err != nil {
		return err
	}
	expected, err := trustedInstrumentCommit(ctx, repositoryRoot, selected, tree, proof.Approval.ApprovalSha256)
	if err != nil || expected != proof.InstrumentedServerCommit {
		return errors.New("trusted instrument child differs from its deterministic approved parent and tree")
	}
	parents, err := sourceGitText(ctx, repositoryRoot, "rev-list", "--parents", "-n", "1", proof.InstrumentedServerCommit)
	if err != nil || parents != proof.InstrumentedServerCommit+" "+selected {
		return errors.New("trusted instrument publication lost its one-child selected-product lineage")
	}
	return nil
}

// Journal/profile identity is operation-scoped, never a fabricated review id.
func sourceBuildAuthorizationIdentity(request reviewedSourceBuildRequest) string {
	if request.TrustedInstrument != nil {
		return "instrument-" + request.TrustedInstrument.Approval.ApprovalSha256 + "-" + request.Proposal.SourceLockSha256
	}
	return request.Proposal.ReviewId.String()
}

// Review and instrument operations share containment, but not authorization.
func validateSourceBuildReceipt(request reviewedSourceBuildRequest, receipt reviewedSourceBuildReceipt) error {
	if sourceBuildRequestMinimumWork(request) != (receipt.G2VolumePolicy == model.CompetitionG2VolumePolicyMinimumWork) {
		return errors.New("source build receipt differs from the prospectively frozen scoring policy")
	}
	if request.TrustedInstrument != nil {
		return controller.ValidateCompetitionTrustedInstrumentBuildReceipt(receipt, request.TrustedInstrument.Approval.ApprovalSha256, request.Proposal.PatchSha256,
			request.Proposal.SourceLockSha256, request.Proposal.Staging, request.Proposal.WinnerReviewPolicy)
	}
	return validateReviewedSourceBuildForPolicy(receipt, request.Proposal.ReviewId, request.Proposal.PatchSha256, request.Proposal.SourceLockSha256, request.Proposal.Staging, request.Proposal.WinnerReviewPolicy)
}

// Product selection has already completed. Only the independently configured
// epoch-two instrument change may now add its deterministic protected child.
func (self *sourceSupervisorRuntime) instrumentSelectedSource(ctx context.Context, current, intent *model.CompetitionSourceActivation, manifest *sourceManifest, stagedRoots map[string]string) error {
	future, err := self.settings.ForCompetitionEpoch(intent.TargetEpoch)
	if err != nil {
		return err
	}
	if future.EvaluationPolicy.G2VolumePolicy == current.G2VolumePolicy {
		return nil
	}
	if self.settings.FutureEpochPolicy == nil || self.settings.FutureEpochPolicy.TrustedInstrument == nil || current.Staging || intent.TargetEpoch != 2 || current.TargetEpoch != 1 ||
		future.EvaluationPolicy.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork || current.G2VolumePolicy != model.CompetitionG2VolumePolicyLegacy {
		return errors.New("selected product requires the exact prospectively approved epoch-two instrument transition")
	}
	approval := self.settings.FutureEpochPolicy.TrustedInstrument
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(approval); err != nil {
		return err
	}
	if approval.InstrumentBaseSourceLockSha256 != current.SourceLockSha256 || approval.InstrumentBaseImageDigest != current.EvaluatorImageDigest || !reflect.DeepEqual(approval.InstrumentBaseCommits, current.Commits) {
		return errors.New("operator instrument approval must preserve the original epoch-one source and image provenance")
	}
	proof := &model.CompetitionTrustedInstrumentProof{Schema: 1, Approval: *approval, SelectedProductKind: intent.Kind,
		SelectedProductCommits: intent.Commits, SelectedProductSourceLockSha256: intent.SourceLockSha256,
		SelectedProductReviewId: intent.ReviewId, SelectedProductReviewDigest: intent.ReviewDecisionDigest, SelectedProductPatchSha256: intent.ApprovedPatchSha256}
	tree, err := trustedInstrumentTree(ctx, stagedRoots["server"], proof.SelectedProductCommits["server"], approval)
	if err != nil {
		return err
	}
	proof.InstrumentedServerCommit, err = trustedInstrumentCommit(ctx, stagedRoots["server"], proof.SelectedProductCommits["server"], tree, approval.ApprovalSha256)
	if err != nil {
		return err
	}
	if _, err := sourceGitText(ctx, stagedRoots["server"], "checkout", "--quiet", "--detach", proof.InstrumentedServerCommit); err != nil {
		return err
	}
	commits := map[string]string{}
	for repository, commit := range proof.SelectedProductCommits {
		commits[repository] = commit
	}
	commits["server"] = proof.InstrumentedServerCommit
	intent.Commits = commits
	intent.SourceLockSha256, err = model.CompetitionSourceLockDigestForMode(commits, false)
	if err != nil {
		return err
	}
	intent.SourceGeneration = current.SourceGeneration + 1
	intent.G2VolumePolicy, intent.TrustedInstrument = model.CompetitionG2VolumePolicyMinimumWork, proof
	var generation reviewedSourceGeneration
	if proof.SelectedProductKind == "reviewed_source_carry_forward" {
		intent.Kind = "trusted_instrument_transition"
		generation = reviewedSourceGeneration{Generation: intent.SourceGeneration, Kind: intent.Kind,
			SignificantImprovementPercent: manifest.EvaluationSource.Generations[current.SourceGeneration].SignificantImprovementPercent, ThresholdEvidenceSha256: intent.ThresholdEvidenceSha256}
		manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, generation)
	} else if proof.SelectedProductKind == "reviewed_source_promotion" {
		generation = manifest.EvaluationSource.Generations[len(manifest.EvaluationSource.Generations)-1]
	} else {
		return errors.New("instrument transition cannot replace an unrecognized product selection")
	}
	generation.SourceLockSha256, generation.Repositories = intent.SourceLockSha256, sourceRepositoriesFromCommits(commits)
	generation.G2VolumePolicy, generation.TrustedInstrument = intent.G2VolumePolicy, proof
	manifest.EvaluationSource.Generations[len(manifest.EvaluationSource.Generations)-1] = generation
	if err := validateSourceManifest(manifest); err != nil {
		return err
	}
	if err := self.buildTrustedInstrument(ctx, current, intent, manifest, stagedRoots); err != nil {
		return err
	}
	// Product review/build times remain genuine. Instrument readiness has its
	// own receipt, and preparation starts only once that receipt exists.
	intent.PreparedAt = time.Now().UTC()
	return nil
}

// Builds the protected child through the existing offline pipeline and actual
// independent qualifier. No passed receipt or runtime pin is synthesized here.
func (self *sourceSupervisorRuntime) buildTrustedInstrument(ctx context.Context, current, intent *model.CompetitionSourceActivation, manifest *sourceManifest, stagedRoots map[string]string) error {
	proof := intent.TrustedInstrument
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	proposal := &reviewedSourceProposal{Schema: 2, Staging: false, BaseSourceLockSha256: proof.SelectedProductSourceLockSha256,
		SourceLockSha256: intent.SourceLockSha256, Commits: intent.Commits, StagedRoots: stagedRoots, PatchSha256: proof.Approval.PatchSha256,
		EvaluatedImageDigest: current.EvaluatorImageDigest, WinnerReviewPolicy: intent.WinnerReviewPolicy, CandidatePathBase: self.settings.PatchPolicy.PathBase}
	request := reviewedSourceBuildRequest{Operation: "isolated_trusted_instrument_build", Proposal: proposal, SourceManifest: manifestBytes, SourceGeneration: intent.SourceGeneration,
		ConfigLocalDirectory: filepath.Join(self.repositoriesRoot, "config", "local"), VaultLocalDirectory: filepath.Join(self.repositoriesRoot, "vault", "local"),
		SandboxContract: "offline_evaluator_containment_v1", TrustedInstrument: proof}
	content, err := runSourceSupervisorProgram(ctx, self.buildCommand, self.buildSha256, request, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"})
	if err != nil {
		return err
	}
	var receipt reviewedSourceBuildReceipt
	if err := decodeStrictJSONBytes(content, &receipt, "trusted instrument build receipt"); err != nil {
		return err
	}
	if err := controller.ValidateCompetitionTrustedInstrumentBuildReceipt(receipt, proof.Approval.ApprovalSha256, proof.Approval.PatchSha256, intent.SourceLockSha256, false, intent.WinnerReviewPolicy); err != nil {
		return err
	}
	proof.BuildReceipt = &receipt
	intent.EvaluatorImageDigest, intent.HostQualificationSha256 = receipt.EvaluatorImageDigest, receipt.HostQualificationSha256
	intent.SimulatorSha256, intent.ScorerSha256 = receipt.SimulatorSha256, receipt.SimulatorSha256
	return nil
}
