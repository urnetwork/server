// Builds a genuinely approved source proposal privately before the source
// deadline. Only a separately qualified isolation broker may run candidate code.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Independent build/test evidence binds the exact reviewed source and isolated
// execution boundary, not a caller's unsupported "build passed" assertion.
type reviewedSourceBuildReceipt struct {
	Schema                  int       `json:"schema"`
	Staging                 bool      `json:"staging,omitempty"`
	ReviewId                server.Id `json:"review_id"`
	PatchSha256             string    `json:"patch_sha256"`
	SourceLockSha256        string    `json:"source_lock_sha256"`
	EvaluatorImageDigest    string    `json:"evaluator_image_digest"`
	HostQualificationSha256 string    `json:"host_qualification_sha256"`
	ContainmentSha256       string    `json:"containment_sha256"`
	TestEvidenceSha256      string    `json:"test_evidence_sha256"`
	SandboxId               string    `json:"sandbox_id"`
	Offline                 bool      `json:"offline"`
	NoMainCredentials       bool      `json:"no_main_credentials"`
	ReadOnlySource          bool      `json:"read_only_source"`
	Passed                  bool      `json:"passed"`
	TestModel               string    `json:"test_model"`
	TestReasoning           string    `json:"test_reasoning"`
}

// Completed proposals do not occupy activation generations or alter refs.
// Choice later rechecks genuine approval and the unchanged evaluated base.
type reviewedSourceProposal struct {
	Schema               int                        `json:"schema"`
	Staging              bool                       `json:"staging,omitempty"`
	ReviewId             server.Id                  `json:"review_id"`
	JobId                server.Id                  `json:"job_id"`
	SubmittedAt          time.Time                  `json:"submitted_at"`
	ReviewDecisionDigest string                     `json:"review_decision_digest"`
	ReviewedAt           time.Time                  `json:"reviewed_at"`
	ReadyAt              time.Time                  `json:"ready_at"`
	BaseSourceLockSha256 string                     `json:"base_source_lock_sha256"`
	SourceLockSha256     string                     `json:"source_lock_sha256"`
	Commits              map[string]string          `json:"commits"`
	StagedRoots          map[string]string          `json:"staged_roots"`
	PatchPath            string                     `json:"patch_path"`
	PatchSha256          string                     `json:"patch_sha256"`
	Significance         *sourceSignificance        `json:"significance"`
	RawScore             float64                    `json:"raw_score"`
	EvaluatedImageDigest string                     `json:"evaluated_image_digest"`
	BuildReceipt         reviewedSourceBuildReceipt `json:"build_receipt"`
}

// Projects statistical fields while retaining the separately authenticated
// archived score-file identity supplied by the sealed review boundary.
func reviewedSourceSignificance(approved *controller.CompetitionReviewedSource) (*sourceSignificance, error) {
	return reviewedSourceSignificanceForMode(approved, false)
}

// Preserves the signed statistics and archived digest for the explicit mode.
// A staging score must itself have been evaluated at zero takeover margin.
func reviewedSourceSignificanceForMode(approved *controller.CompetitionReviewedSource, staging bool) (*sourceSignificance, error) {
	if approved == nil || !validSha256(approved.ScoreSha256) {
		return nil, errors.New("reviewed source lacks its archived score identity")
	}
	content, err := json.Marshal(approved.Score)
	if err != nil {
		return nil, err
	}
	significance, err := winnerSourceSignificanceForMode(&approved.Score, content, staging)
	if err != nil {
		return nil, err
	}
	significance.ScoreSha256 = approved.ScoreSha256
	return significance, nil
}

// A real sandbox receipt is required even for a reviewed candidate: review
// authority never authorizes candidate execution in the main host environment.
func validateReviewedSourceBuild(receipt reviewedSourceBuildReceipt, reviewId server.Id, patchDigest, lockDigest string) error {
	return validateReviewedSourceBuildForMode(receipt, reviewId, patchDigest, lockDigest, false)
}

// Binds independent image qualification to the explicit mode as well as pins.
func validateReviewedSourceBuildForMode(receipt reviewedSourceBuildReceipt, reviewId server.Id, patchDigest, lockDigest string, staging bool) error {
	if receipt.Schema != 1 || receipt.Staging != staging || receipt.ReviewId != reviewId || receipt.PatchSha256 != patchDigest || receipt.SourceLockSha256 != lockDigest ||
		!receipt.Passed || !receipt.Offline || !receipt.NoMainCredentials || !receipt.ReadOnlySource || receipt.SandboxId == "" ||
		receipt.TestModel != "gpt-6-sol" || receipt.TestReasoning != "medium" ||
		!strings.HasPrefix(receipt.EvaluatorImageDigest, "sha256:") || !validSha256(strings.TrimPrefix(receipt.EvaluatorImageDigest, "sha256:")) ||
		!validSha256(receipt.HostQualificationSha256) || !validSha256(receipt.ContainmentSha256) || !validSha256(receipt.TestEvidenceSha256) {
		return errors.New("reviewed source requires exact independent isolated build/test and containment evidence")
	}
	return nil
}

// Selects only a completed, applicable proposal at the frozen deadline. Exact
// raw score, admission time, and job id retain the original sealed ranking.
func selectReviewedSourceProposal(proposals []reviewedSourceProposal, current *model.CompetitionSourceActivation, deadline time.Time) *reviewedSourceProposal {
	if current == nil || !validSourceMargin(current.Threshold*100, current.Staging) {
		return nil
	}
	eligible := []reviewedSourceProposal{}
	for _, proposal := range proposals {
		if proposal.Schema != 2 || proposal.Staging != current.Staging || proposal.ReviewId == (server.Id{}) || !validSha256(proposal.ReviewDecisionDigest) ||
			proposal.JobId == (server.Id{}) || proposal.SubmittedAt.IsZero() || proposal.BaseSourceLockSha256 != current.SourceLockSha256 || proposal.ReadyAt.IsZero() || proposal.ReviewedAt.IsZero() ||
			proposal.ReadyAt.After(deadline) || proposal.ReviewedAt.After(deadline) || !finitePositive(proposal.RawScore) || validateSourceSignificanceForMode(proposal.Significance, current.Staging) != nil ||
			proposal.Significance.RecommendedNextEpochTakeoverMarginPercent < current.Threshold*100 {
			continue
		}
		lockDigest, err := model.CompetitionSourceLockDigest(proposal.Commits)
		if err != nil || lockDigest != proposal.SourceLockSha256 || validateReviewedSourceBuildForMode(proposal.BuildReceipt, proposal.ReviewId, proposal.PatchSha256, lockDigest, current.Staging) != nil {
			continue
		}
		applicable := proposal.Commits["server"] != current.Commits["server"]
		for repository, commit := range current.Commits {
			if repository != "server" && proposal.Commits[repository] != commit {
				applicable = false
			}
		}
		if applicable {
			eligible = append(eligible, proposal)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].RawScore != eligible[j].RawScore {
			return eligible[i].RawScore < eligible[j].RawScore
		}
		if !eligible[i].SubmittedAt.Equal(eligible[j].SubmittedAt) {
			return eligible[i].SubmittedAt.Before(eligible[j].SubmittedAt)
		}
		if eligible[i].JobId != eligible[j].JobId {
			return eligible[i].JobId.String() < eligible[j].JobId.String()
		}
		return eligible[i].ReviewId.String() < eligible[j].ReviewId.String()
	})
	if len(eligible) == 0 {
		return nil
	}
	return &eligible[0]
}

// Ignores unavailable proposals rather than putting discretionary review on
// the critical path. Every selected proposal is reauthenticated from the store.
func (self *sourceSupervisorRuntime) readySourceProposal(ctx context.Context, current *model.CompetitionSourceActivation, deadline time.Time) (*reviewedSourceProposal, error) {
	entries, err := os.ReadDir(self.stateDirectory)
	if err != nil {
		return nil, err
	}
	proposals := []reviewedSourceProposal{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "reviewed-source-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			continue
		}
		content, err := readSourceFile(filepath.Join(self.stateDirectory, entry.Name()))
		if err != nil {
			continue
		}
		var proposal reviewedSourceProposal
		if decodeStrictJSONBytes(content, &proposal, "reviewed source proposal") != nil {
			continue
		}
		approved, err := self.store.ReviewedSource(ctx, self.settings, proposal.ReviewId)
		if errors.Is(err, controller.ErrConflict) || errors.Is(err, controller.ErrNotFound) || errors.Is(err, controller.ErrEvaluationInvalidated) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if approved.Review.RequestSha256 != proposal.ReviewDecisionDigest || approved.Review.Request.BaseSourceLockSha256 != proposal.BaseSourceLockSha256 ||
			approved.Review.Request.JobId != proposal.JobId || !approved.SubmittedAt.Equal(proposal.SubmittedAt) ||
			approved.Review.Request.PatchSha256 != proposal.PatchSha256 || proposal.Significance == nil || approved.Review.Request.ScoreSha256 != proposal.Significance.ScoreSha256 ||
			!approved.Review.CreatedAt.Equal(proposal.ReviewedAt) || approved.EvaluatedImageDigest != proposal.EvaluatedImageDigest || approved.Score.RawScore == nil || *approved.Score.RawScore != proposal.RawScore {
			continue
		}
		authenticatedSignificance, err := reviewedSourceSignificanceForMode(approved, current.Staging)
		if err != nil || *authenticatedSignificance != *proposal.Significance {
			continue
		}
		proposals = append(proposals, proposal)
	}
	return selectReviewedSourceProposal(proposals, current, deadline), nil
}

// Stages the exact approved patch without pushing or activating anything. The
// build broker gets only a fixed environment and explicit config/local mounts.
func runStageReviewedSource(opts docopt.Opts) {
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("source stage settings: %s", err)
	}
	if !settings.AutomationPolicy.Enabled {
		fatalf("source stage requires explicitly enabled v2 policy")
	}
	reviewId, err := server.ParseId(optString(opts, "--review-id", ""))
	if err != nil {
		fatalf("source review id: %s", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	current, err := store.CurrentSourceActivation(ctx, settings)
	if err != nil {
		fatalf("source incumbent: %s", err)
	}
	if err := validateSourceActivationMode(current, settings); err != nil {
		fatalf("source incumbent mode: %s", err)
	}
	approved, err := store.ReviewedSource(ctx, settings, reviewId)
	if err != nil {
		fatalf("genuine source approval: %s", err)
	}
	if approved.Review.Request.BaseSourceLockSha256 != current.SourceLockSha256 || approved.BaseSha != current.Commits["server"] {
		fatalf("source approval targets an obsolete incumbent; no implicit rebase is allowed")
	}
	patch, patchErr := controller.ValidateAndCanonicalizePatch(string(approved.Patch), approved.PatchPolicy)
	if patchErr != nil || patch.Sha256 != approved.Review.Request.PatchSha256 {
		fatalf("approved source patch is not canonical or allowed")
	}
	stateDirectory, err := filepath.Abs(optString(opts, "--state-dir", ""))
	if err != nil || optString(opts, "--state-dir", "") == "" {
		fatalf("source stage requires --state-dir")
	}
	info, err := os.Lstat(stateDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Mode()&os.ModeSymlink != 0 {
		fatalf("source stage directory must already be private")
	}
	proposalPath := filepath.Join(stateDirectory, "reviewed-source-"+reviewId.String()+".json")
	if _, err := os.Lstat(proposalPath); err == nil {
		fatalf("completed source proposal already exists; use it without regenerating commits")
	} else if !errors.Is(err, os.ErrNotExist) {
		fatalf("source proposal: %s", err)
	}
	_, repositoriesRoot, lock, err := configuredSourcePathsForMode(opts, settings.AutomationPolicy.Staging)
	if err != nil || lock != "" {
		fatalf("source stage requires host-only paths: %v", err)
	}
	root, err := os.MkdirTemp(stateDirectory, ".reviewed-source-")
	if err != nil {
		fatalf("private source stage: %s", err)
	}
	patchPath := filepath.Join(root, "canonical.patch")
	if err := writePrivateReviewFile(patchPath, approved.Patch); err != nil {
		fatalf("source patch: %s", err)
	}
	proposal := &reviewedSourceProposal{Schema: 2, Staging: current.Staging, ReviewId: reviewId, JobId: approved.Review.Request.JobId, SubmittedAt: approved.SubmittedAt, ReviewDecisionDigest: approved.Review.RequestSha256, ReviewedAt: approved.Review.CreatedAt,
		BaseSourceLockSha256: current.SourceLockSha256, Commits: map[string]string{}, StagedRoots: map[string]string{}, PatchPath: patchPath, PatchSha256: patch.Sha256, EvaluatedImageDigest: approved.EvaluatedImageDigest}
	for _, repository := range sourceRepositoryNames() {
		origin, err := sourceGitText(ctx, filepath.Join(repositoriesRoot, repository), "remote", "get-url", "origin")
		if err != nil {
			fatalf("source origin: %s", err)
		}
		stagedRoot := filepath.Join(root, repository)
		if _, err := sourceGitText(ctx, root, "-c", "core.fsync=all", "clone", "--quiet", "--no-checkout", origin, stagedRoot); err != nil {
			fatalf("source clone: %s", err)
		}
		if err := os.Chmod(stagedRoot, 0o700); err != nil {
			fatalf("source clone permissions: %s", err)
		}
		if _, err := sourceGitText(ctx, stagedRoot, "checkout", "--quiet", "--detach", current.Commits[repository]); err != nil {
			fatalf("source checkout: %s", err)
		}
		proposal.StagedRoots[repository], proposal.Commits[repository] = stagedRoot, current.Commits[repository]
		if repository == "server" {
			if err := verifyFrozenSourceGraph(ctx, stagedRoot, current.Commits[repository]); err != nil {
				fatalf("reviewed measured dependency graph: %s", err)
			}
			if _, err := sourceGitText(ctx, stagedRoot, "apply", "--index", "--whitespace=error-all", patchPath); err != nil {
				fatalf("source patch apply: %s", err)
			}
			if _, err := sourceGitText(ctx, stagedRoot, "-c", "core.fsync=all", "-c", "user.name=URnetwork Competition", "-c", "user.email=competition@invalid", "commit", "--quiet", "--no-gpg-sign", "-m", "competition: separately reviewed source "+reviewId.String()); err != nil {
				fatalf("source commit: %s", err)
			}
			proposal.Commits[repository], err = sourceGitText(ctx, stagedRoot, "rev-parse", "HEAD")
			if err != nil {
				fatalf("source commit identity: %s", err)
			}
			backend := &gitSourcePublicationBackend{patchPolicy: approved.PatchPolicy}
			if err := backend.Verify(ctx, sourcePublicationRef{Repository: "server", Branch: sourceBranch(current.Staging), ExpectedCommit: current.Commits[repository], Commit: proposal.Commits[repository], StagedRoot: stagedRoot}, &sourcePublicationIntent{Staging: current.Staging, ApprovedPatchPath: patchPath, ApprovedPatchSha256: patch.Sha256}); err != nil {
				fatalf("exact approved source tree: %s", err)
			}
		}
	}
	proposal.SourceLockSha256, err = model.CompetitionSourceLockDigest(proposal.Commits)
	if err != nil {
		fatalf("source lock: %s", err)
	}
	proposal.Significance, err = reviewedSourceSignificanceForMode(approved, current.Staging)
	if err != nil {
		fatalf("source significance: %s", err)
	}
	proposal.RawScore = *approved.Score.RawScore
	buildCtx, buildCancel := context.WithTimeout(ctx, 3*time.Hour)
	defer buildCancel()
	output, err := runSourceSupervisorProgram(buildCtx, optString(opts, "--build-command", ""), optString(opts, "--build-sha256", ""), struct {
		Operation            string                  `json:"operation"`
		Proposal             *reviewedSourceProposal `json:"proposal"`
		ConfigLocalDirectory string                  `json:"config_local_directory"`
		VaultLocalDirectory  string                  `json:"vault_local_directory"`
		SandboxContract      string                  `json:"sandbox_contract"`
	}{Operation: "isolated_reviewed_source_build", Proposal: proposal, ConfigLocalDirectory: settings.ConfigLocalDirectory, VaultLocalDirectory: settings.VaultLocalDirectory, SandboxContract: "offline_evaluator_containment_v1"}, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"})
	if err != nil {
		fatalf("isolated independent source build: %s", err)
	}
	if err := decodeStrictJSONBytes(output, &proposal.BuildReceipt, "isolated build receipt"); err != nil {
		fatalf("source build receipt: %s", err)
	}
	if err := validateReviewedSourceBuildForMode(proposal.BuildReceipt, reviewId, patch.Sha256, proposal.SourceLockSha256, current.Staging); err != nil {
		fatalf("source build readiness: %s", err)
	}
	// The trusted local completion time prevents an adapter from backdating a
	// late build into the frozen source-choice opportunity.
	proposal.ReadyAt = time.Now().UTC()
	encoded, err := json.Marshal(proposal)
	if err != nil {
		fatalf("source proposal encoding: %s", err)
	}
	if err := writePrivateReviewFile(proposalPath, encoded); err != nil {
		fatalf("source proposal persistence: %s", err)
	}
	directory, err := os.Open(stateDirectory)
	if err != nil {
		fatalf("source proposal directory: %s", err)
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		fatalf("source proposal durability: %s", err)
	}
	digest := sha256.Sum256(encoded)
	result, _ := json.Marshal(struct {
		ProposalPath string    `json:"proposal_path"`
		Sha256       string    `json:"sha256"`
		ReadyAt      time.Time `json:"ready_at"`
	}{ProposalPath: proposalPath, Sha256: hex.EncodeToString(digest[:]), ReadyAt: proposal.ReadyAt})
	fmt.Printf("%s\n", result)
}

// Copies authenticated patch bytes into the activation's private recovery root.
func copyReviewedSourcePatch(proposal *reviewedSourceProposal, root string) error {
	content, err := readSourceFile(proposal.PatchPath)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != proposal.PatchSha256 {
		return errors.New("reviewed source patch changed after build")
	}
	if bytes.Contains(content, []byte{0}) {
		return errors.New("reviewed source patch contains binary content")
	}
	return writePrivateReviewFile(filepath.Join(root, "canonical.patch"), content)
}
