package main

// Winner promotion advances measured source one epoch. It creates one temporary
// root, clones all evaluator repositories there, checks out their sim-latency
// branch at the prior epoch, applies and commits the winner, pushes repository
// branches, and activates the config ledger last. The operator checkouts are
// preflight inputs only and are never patched.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/controller"
	"gopkg.in/yaml.v3"
)

const maximumPromotionPatchBytes = 64 * 1024 * 1024

var promotionJobIdPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// promotionRepository records one branch transition staged in an isolated clone.
type promotionRepository struct {
	Name           string
	Branch         string
	LocalRoot      string
	StagedRoot     string
	PreviousCommit string
	NextCommit     string
	PatchPath      string
}

// promotionResult is the operator-readable dry-run and completion record.
type promotionResult struct {
	Schema                        int                 `json:"schema"`
	Epoch                         int                 `json:"epoch"`
	Kind                          string              `json:"kind"`
	SignificantImprovementPercent float64             `json:"significant_improvement_percent"`
	WinnerJobId                   string              `json:"winner_job_id,omitempty"`
	WinnerSignificance            *sourceSignificance `json:"winner_significance,omitempty"`
	PromotedFromEpoch             int                 `json:"promoted_from_epoch"`
	RepositoryCommits             map[string]string   `json:"repository_commits"`
	ConfigCommit                  string              `json:"config_commit"`
	DryRun                        bool                `json:"dry_run"`
	LedgerActivated               bool                `json:"ledger_activated"`
}

// readWinnerScore authenticates the exact control-plane score record that was
// materialized for honesty review. The immutable evaluation archive remains
// the source of every underlying scorer artifact and replicate diagnostic.
func readWinnerScore(winnerRoot string) (*controller.ScoreResult, []byte, error) {
	path := filepath.Join(winnerRoot, "score.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("winner score must be a regular file")
	}
	content, err := readBoundedFile(path, "winner score")
	if err != nil {
		return nil, nil, err
	}
	result := &controller.ScoreResult{}
	if err := decodeStrictJSONBytes(content, result, "winner score"); err != nil {
		return nil, nil, err
	}
	if result.ScoreSchema != controller.ScoreSchema || result.RawScore == nil ||
		result.NormalizedScore == nil || !finitePositive(*result.RawScore) ||
		!finitePositive(*result.NormalizedScore) || *result.NormalizedScore < 1 ||
		200 < *result.NormalizedScore || !result.Placeable ||
		!result.TakeoverEligible || !allCompetitionScoreGatesPass(result.Gates) ||
		result.Significance == nil ||
		!result.Significance.StatisticallySignificant ||
		!result.Significance.RecommendedNextEpochTakeoverMarginSupported ||
		result.Significance.ReplicateCount != 9 {
		return nil, nil, errors.New("winner score is not a statistically eligible R=9 result")
	}
	takeoverEligible, ok := result.Diagnostics["baseline_takeover_eligible"].(bool)
	if !ok || !takeoverEligible {
		return nil, nil, errors.New("winner score is missing its takeover eligibility diagnostic")
	}
	return result, content, nil
}

func allCompetitionScoreGatesPass(gates map[string]controller.Gate) bool {
	if len(gates) == 0 {
		return false
	}
	for _, gate := range gates {
		if !gate.Passed || gate.Details == nil {
			return false
		}
	}
	return true
}

func winnerSourceSignificance(
	result *controller.ScoreResult,
	content []byte,
) (*sourceSignificance, error) {
	significance := result.Significance
	if significance.BaselineSampleVariance == nil ||
		significance.CandidateSampleVariance == nil ||
		significance.MinimumSignificantImprovementPercent == nil ||
		significance.RequiredImprovementPercent == nil ||
		significance.OneSidedPValue == nil ||
		significance.NextEpochMinimumImprovementPercent == nil ||
		significance.RecommendedNextEpochTakeoverMarginPercent == nil {
		return nil, errors.New("winner score significance record is incomplete")
	}
	digest := sha256.Sum256(content)
	source := &sourceSignificance{
		ScoreSha256:                               fmt.Sprintf("%x", digest),
		Method:                                    significance.Method,
		Alpha:                                     significance.Alpha,
		ReplicateCount:                            significance.ReplicateCount,
		BaselineMeanRawScore:                      significance.BaselineMeanRawScore,
		CandidateMeanRawScore:                     significance.CandidateMeanRawScore,
		BaselineSampleVariance:                    *significance.BaselineSampleVariance,
		CandidateSampleVariance:                   *significance.CandidateSampleVariance,
		ObservedImprovementPercent:                significance.ObservedImprovementPercent,
		TakeoverMarginPercent:                     significance.TakeoverMarginPercent,
		MinimumSignificantImprovementPercent:      *significance.MinimumSignificantImprovementPercent,
		RequiredImprovementPercent:                *significance.RequiredImprovementPercent,
		OneSidedPValue:                            *significance.OneSidedPValue,
		NextEpochMinimumImprovementPercent:        *significance.NextEpochMinimumImprovementPercent,
		RecommendedNextEpochTakeoverMarginPercent: *significance.RecommendedNextEpochTakeoverMarginPercent,
	}
	if err := validateSourceSignificance(source); err != nil {
		return nil, err
	}
	return source, nil
}

// readWinnerSignificance retains the narrow helper used by source-ledger
// validation while requiring the review-time control-plane score schema.
func readWinnerSignificance(winnerRoot string) (*sourceSignificance, error) {
	result, content, err := readWinnerScore(winnerRoot)
	if err != nil {
		return nil, err
	}
	return winnerSourceSignificance(result, content)
}

func exactApprovedScoreMatches(
	approved controller.ScoreResult,
	winner *controller.ScoreResult,
) bool {
	if winner == nil {
		return false
	}
	approvedJSON, approvedErr := json.Marshal(approved)
	winnerJSON, winnerErr := json.Marshal(winner)
	return approvedErr == nil && winnerErr == nil && bytes.Equal(approvedJSON, winnerJSON)
}

// remoteBranchCommit reads one exact branch head without changing local refs.
func remoteBranchCommit(repositoryRoot string, branch string) (string, error) {
	output, err := gitOutput(repositoryRoot, "ls-remote", "--exit-code", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch || !sourceGitShaPattern.MatchString(fields[0]) {
		return "", fmt.Errorf("origin branch %s returned an invalid identity", branch)
	}
	return fields[0], nil
}

// promotionPatchPath accepts a per-repository patch and the evaluator's canonical server name.
func promotionPatchPath(winnerRoot string, repository string) (string, bool, error) {
	candidates := []string{filepath.Join(winnerRoot, repository+".patch")}
	if repository == "server" {
		candidates = append(candidates, filepath.Join(winnerRoot, "canonical.patch"))
	}
	foundPaths := []string{}
	for _, candidate := range candidates {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("winner patch must be a regular file: %s", candidate)
		}
		if info.Size() <= 0 || maximumPromotionPatchBytes < info.Size() {
			return "", false, fmt.Errorf("winner patch size is invalid: %s", candidate)
		}
		foundPaths = append(foundPaths, candidate)
	}
	if 1 < len(foundPaths) {
		return "", false, fmt.Errorf("winner contains both server.patch and canonical.patch")
	}
	if len(foundPaths) == 0 {
		return "", false, nil
	}
	path, err := filepath.Abs(foundPaths[0])
	return path, true, err
}

// stagePromotionRepository applies and commits one winner patch without touching the operator checkout.
func stagePromotionRepository(repository *promotionRepository, stagingRoot string, commitMessage string) error {
	originUrl, err := gitOutput(repository.LocalRoot, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	repository.StagedRoot = filepath.Join(stagingRoot, repository.Name)
	if _, err := gitOutput(stagingRoot, "clone", "--quiet", "--no-checkout", originUrl, repository.StagedRoot); err != nil {
		return err
	}
	if repository.Branch != "sim-latency" {
		return errors.New("promotion repository branch must be sim-latency")
	}
	if _, err := gitOutput(
		repository.StagedRoot,
		"checkout", "--quiet", "-B", repository.Branch, repository.PreviousCommit,
	); err != nil {
		return err
	}
	branch, err := gitOutput(repository.StagedRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != repository.Branch {
		return fmt.Errorf("repository %s did not check out branch %s", repository.Name, repository.Branch)
	}
	if repository.PatchPath == "" {
		repository.NextCommit = repository.PreviousCommit
		return nil
	}
	protectedSimulatorTree := ""
	if repository.Name == "server" {
		protectedSimulatorTree, err = gitOutput(
			repository.StagedRoot,
			"rev-parse", "HEAD:connect/sim-latency",
		)
		if err != nil || !sourceGitShaPattern.MatchString(protectedSimulatorTree) {
			return errors.New("server promotion is missing the protected sim-latency source tree")
		}
	}
	if _, err := gitOutput(repository.StagedRoot, "apply", "--check", "--whitespace=error-all", repository.PatchPath); err != nil {
		return fmt.Errorf("repository %s winner patch check: %w", repository.Name, err)
	}
	if _, err := gitOutput(repository.StagedRoot, "apply", "--index", "--whitespace=error-all", repository.PatchPath); err != nil {
		return fmt.Errorf("repository %s winner patch apply: %w", repository.Name, err)
	}
	changedPaths, err := gitOutput(repository.StagedRoot, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if changedPaths == "" {
		return fmt.Errorf("repository %s winner patch makes no change", repository.Name)
	}
	if repository.Name == "server" {
		for _, changedPath := range strings.Split(changedPaths, "\n") {
			if changedPath == "connect/sim-latency" || strings.HasPrefix(changedPath, "connect/sim-latency/") {
				return fmt.Errorf("repository server winner patch modifies the protected sim-latency source tree")
			}
		}
	}
	status, err := gitOutput(repository.StagedRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "??") {
			return fmt.Errorf("repository %s patch left untracked content", repository.Name)
		}
	}
	if _, err := gitOutput(
		repository.StagedRoot,
		"-c", "user.name=URnetwork Competition",
		"-c", "user.email=competition@invalid",
		"commit", "--quiet", "--no-gpg-sign", "-m", commitMessage,
	); err != nil {
		return err
	}
	repository.NextCommit, err = gitOutput(repository.StagedRoot, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	parent, err := gitOutput(repository.StagedRoot, "rev-parse", "HEAD^")
	if err != nil {
		return err
	}
	if parent != repository.PreviousCommit {
		return fmt.Errorf("repository %s promotion is not one commit above epoch base", repository.Name)
	}
	status, err = gitOutput(repository.StagedRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("repository %s staged promotion is not clean", repository.Name)
	}
	if repository.Name == "server" {
		candidateSimulatorTree, treeErr := gitOutput(
			repository.StagedRoot,
			"rev-parse", "HEAD:connect/sim-latency",
		)
		if treeErr != nil || candidateSimulatorTree != protectedSimulatorTree {
			return errors.New("server promotion changed the protected sim-latency source tree")
		}
	}
	return nil
}

// sourceRepositoriesFromCommits converts a complete promotion result back to the strict ledger shape.
func sourceRepositoriesFromCommits(repositoryCommits map[string]string) sourceRepositories {
	return sourceRepositories{
		Server:        sourceRepository{Commit: repositoryCommits["server"]},
		Connect:       sourceRepository{Commit: repositoryCommits["connect"]},
		Sdk:           sourceRepository{Commit: repositoryCommits["sdk"]},
		Proxy:         sourceRepository{Commit: repositoryCommits["proxy"]},
		Glog:          sourceRepository{Commit: repositoryCommits["glog"]},
		Goidenticons:  sourceRepository{Commit: repositoryCommits["goidenticons"]},
		Userwireguard: sourceRepository{Commit: repositoryCommits["userwireguard"]},
		Sn:            sourceRepository{Commit: repositoryCommits["sn"]},
	}
}

// stagePromotionConfig appends the next epoch in an isolated clone of config/main.
func stagePromotionConfig(
	manifest *sourceManifest,
	sourceConfig string,
	stagingRoot string,
	epochNumber int,
	transitionKind string,
	winnerJobId string,
	winnerSignificance *sourceSignificance,
	promotedAt string,
	repositoryCommits map[string]string,
) (string, string, error) {
	configRoot, err := gitOutput(filepath.Dir(sourceConfig), "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("source config is not in a git repository: %w", err)
	}
	configRoot, err = filepath.Abs(configRoot)
	if err != nil {
		return "", "", err
	}
	expectedSourceConfig := filepath.Join(configRoot, "main", "sim-latency.yml")
	if sourceConfig != expectedSourceConfig {
		return "", "", fmt.Errorf("source config must be %s", expectedSourceConfig)
	}
	branch, err := gitOutput(configRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != "main" {
		return "", "", errors.New("config repository must be on branch main")
	}
	status, err := gitOutput(configRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", "", err
	}
	if status != "" {
		return "", "", errors.New("config repository worktree is not clean")
	}
	if _, err := gitOutput(configRoot, "ls-files", "--error-unmatch", "main/sim-latency.yml"); err != nil {
		return "", "", errors.New("source config must be committed before promotion")
	}
	configHead, err := gitOutput(configRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	remoteHead, err := remoteBranchCommit(configRoot, "main")
	if err != nil {
		return "", "", err
	}
	if remoteHead != configHead {
		return "", "", fmt.Errorf("config origin/main %s does not match local head %s", remoteHead, configHead)
	}
	previousEpoch := epochNumber - 1
	significantImprovementPercent := manifest.EvaluationSource.Epochs[previousEpoch].SignificantImprovementPercent
	if winnerSignificance != nil {
		significantImprovementPercent = winnerSignificance.RecommendedNextEpochTakeoverMarginPercent
	}
	manifest.EvaluationSource.Epochs = append(manifest.EvaluationSource.Epochs, sourceEpoch{
		Epoch:                         epochNumber,
		Kind:                          transitionKind,
		SignificantImprovementPercent: significantImprovementPercent,
		WinnerJobId:                   winnerJobId,
		WinnerSignificance:            winnerSignificance,
		PromotedFromEpoch:             &previousEpoch,
		PromotedAt:                    promotedAt,
		Repositories:                  sourceRepositoriesFromCommits(repositoryCommits),
	})
	if err := validateSourceManifest(manifest); err != nil {
		return "", "", err
	}
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return "", "", err
	}
	configOriginUrl, err := gitOutput(configRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	stagedConfigRoot := filepath.Join(stagingRoot, "config")
	if _, err := gitOutput(stagingRoot, "clone", "--quiet", "--no-checkout", configOriginUrl, stagedConfigRoot); err != nil {
		return "", "", err
	}
	if _, err := gitOutput(stagedConfigRoot, "checkout", "--quiet", "--detach", configHead); err != nil {
		return "", "", err
	}
	stagedConfigPath := filepath.Join(stagedConfigRoot, "main", "sim-latency.yml")
	header := []byte("# Frozen measured-source commits by competition epoch. Control-plane services remain on main.\n")
	if err := os.WriteFile(stagedConfigPath, append(header, manifestBytes...), 0o644); err != nil {
		return "", "", err
	}
	if _, err := gitOutput(stagedConfigRoot, "add", "--", "main/sim-latency.yml"); err != nil {
		return "", "", err
	}
	if _, err := gitOutput(
		stagedConfigRoot,
		"-c", "user.name=URnetwork Competition",
		"-c", "user.email=competition@invalid",
		"commit", "--quiet", "--no-gpg-sign", "-m", fmt.Sprintf("competition: activate source epoch %d", epochNumber),
	); err != nil {
		return "", "", err
	}
	configCommit, err := gitOutput(stagedConfigRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return stagedConfigRoot, configCommit, nil
}

// fastForwardLocalBranch updates an already verified operator checkout after publication.
func fastForwardLocalBranch(repositoryRoot string, branch string, commit string) error {
	if _, err := gitOutput(repositoryRoot, "fetch", "--quiet", "origin", branch); err != nil {
		return err
	}
	if _, err := gitOutput(repositoryRoot, "merge", "--ff-only", commit); err != nil {
		return err
	}
	return nil
}

// runPromote validates, stages, publishes, and locally fast-forwards one winner transition.
func runPromote(opts docopt.Opts) {
	epochNumber, err := configuredEpoch(opts)
	if err != nil || epochNumber == 0 {
		fatalf("promotion epoch must be an integer in 1..%d", maximumCompetitionEpoch)
	}
	noWinner := optBool(opts, "--no-winner")
	winnerJobId := optString(opts, "--winner-job-id", "")
	winnerRoot := ""
	var winnerScore *controller.ScoreResult
	var winnerSignificance *sourceSignificance
	transitionKind := "winner_promotion"
	if noWinner {
		transitionKind = "no_winner_carry_forward"
		if winnerJobId != "" || optString(opts, "--winner", "") != "" {
			fatalf("--no-winner cannot be combined with --winner or --winner-job-id")
		}
	} else {
		if !promotionJobIdPattern.MatchString(winnerJobId) {
			fatalf("--winner-job-id must match [A-Za-z0-9._-]{1,128}")
		}
		winnerRoot, err = filepath.Abs(optString(opts, "--winner", ""))
		if err != nil {
			fatalf("winner path: %s", err)
		}
		winnerInfo, statErr := os.Stat(winnerRoot)
		if statErr != nil || !winnerInfo.IsDir() {
			fatalf("--winner must be an existing directory")
		}
		var winnerScoreBytes []byte
		winnerScore, winnerScoreBytes, err = readWinnerScore(winnerRoot)
		if err == nil {
			winnerSignificance, err = winnerSourceSignificance(winnerScore, winnerScoreBytes)
		}
		if err != nil {
			fatalf("winner score: %s", err)
		}
	}
	approvedCandidate, err := requireReviewedPromotion(epochNumber, winnerJobId, noWinner)
	if err != nil {
		fatalf("promotion honesty-review gate: %s", err)
	}
	if !noWinner {
		if err := authenticateApprovedWinnerBundle(
			winnerRoot,
			approvedCandidate,
			winnerScore,
			winnerSignificance,
		); err != nil {
			fatalf("promotion approved bundle: %s", err)
		}
	}
	messageSuffix := strings.TrimSpace(optString(opts, "--message", ""))
	if 120 < len(messageSuffix) || strings.ContainsAny(messageSuffix, "\r\n") {
		fatalf("--message must be one line of at most 120 bytes")
	}
	sourceConfig, repositoriesRoot, sourceLock, err := configuredSourcePaths(opts)
	if err != nil {
		fatalf("promotion source paths: %s", err)
	}
	if sourceLock != "" {
		fatalf("winner promotion is host-only and cannot use an evaluator source lock")
	}
	manifest, err := loadSourceManifest(sourceConfig)
	if err != nil {
		fatalf("promotion source config: %s", err)
	}
	if len(manifest.EvaluationSource.Epochs) != epochNumber {
		fatalf("epoch %d is not the next unconfigured epoch; ledger currently contains epochs 0..%d", epochNumber, len(manifest.EvaluationSource.Epochs)-1)
	}
	previousEpochNumber := epochNumber - 1
	if err := verifyRemoteSourceEpochHead(manifest, previousEpochNumber, repositoriesRoot); err != nil {
		fatalf("previous source epoch preflight: %s", err)
	}
	previousEpoch, err := manifest.epoch(previousEpochNumber)
	if err != nil {
		fatalf("previous source epoch: %s", err)
	}
	stagingRoot, err := os.MkdirTemp("", "sim-latency-promotion-")
	if err != nil {
		fatalf("create promotion staging directory: %s", err)
	}
	defer os.RemoveAll(stagingRoot)

	commitMessage := fmt.Sprintf("competition: promote epoch %d winner %s", epochNumber, winnerJobId)
	if noWinner {
		commitMessage = fmt.Sprintf("competition: carry source epoch %d after no winner", epochNumber)
	}
	if messageSuffix != "" {
		commitMessage += "\n\n" + messageSuffix
	}
	repositories := []*promotionRepository{}
	patchCount := 0
	for _, repositoryName := range sourceRepositoryNames() {
		patchPath := ""
		if !noWinner {
			var found bool
			var patchErr error
			patchPath, found, patchErr = promotionPatchPath(winnerRoot, repositoryName)
			if patchErr != nil {
				fatalf("winner bundle: %s", patchErr)
			}
			if found {
				patchCount += 1
			}
		}
		repository := &promotionRepository{
			Name:           repositoryName,
			Branch:         manifest.EvaluationSource.Branch,
			LocalRoot:      filepath.Join(repositoriesRoot, repositoryName),
			PreviousCommit: previousEpoch.Repositories.commits()[repositoryName],
			PatchPath:      patchPath,
		}
		remoteCommit, remoteErr := remoteBranchCommit(repository.LocalRoot, manifest.EvaluationSource.Branch)
		if remoteErr != nil {
			fatalf("repository %s remote preflight: %s", repositoryName, remoteErr)
		}
		if remoteCommit != repository.PreviousCommit {
			fatalf("repository %s origin/%s %s does not match epoch %d commit %s", repositoryName, manifest.EvaluationSource.Branch, remoteCommit, previousEpochNumber, repository.PreviousCommit)
		}
		if noWinner {
			repository.NextCommit = repository.PreviousCommit
		} else if stageErr := stagePromotionRepository(repository, stagingRoot, commitMessage); stageErr != nil {
			fatalf("stage repository %s: %s", repositoryName, stageErr)
		}
		repositories = append(repositories, repository)
	}
	if !noWinner && patchCount == 0 {
		fatalf("winner bundle must contain at least one repository patch")
	}
	repositoryCommits := map[string]string{}
	for _, repository := range repositories {
		repositoryCommits[repository.Name] = repository.NextCommit
	}
	promotedAt := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	stagedConfigRoot, configCommit, err := stagePromotionConfig(
		manifest,
		sourceConfig,
		stagingRoot,
		epochNumber,
		transitionKind,
		winnerJobId,
		winnerSignificance,
		promotedAt,
		repositoryCommits,
	)
	if err != nil {
		fatalf("stage source config: %s", err)
	}
	result := promotionResult{
		Schema: 1,
		Epoch:  epochNumber,
		Kind:   transitionKind,
		SignificantImprovementPercent: func() float64 {
			if winnerSignificance != nil {
				return winnerSignificance.RecommendedNextEpochTakeoverMarginPercent
			}
			return previousEpoch.SignificantImprovementPercent
		}(),
		WinnerJobId:        winnerJobId,
		WinnerSignificance: winnerSignificance,
		PromotedFromEpoch:  previousEpochNumber,
		RepositoryCommits:  repositoryCommits,
		ConfigCommit:       configCommit,
		DryRun:             optBool(opts, "--dry-run"),
		LedgerActivated:    false,
	}
	if !result.DryRun {
		for _, repository := range repositories {
			if repository.NextCommit == repository.PreviousCommit {
				continue
			}
			if _, err := gitOutput(repository.StagedRoot, "push", "--porcelain", "origin", repository.NextCommit+":refs/heads/"+manifest.EvaluationSource.Branch); err != nil {
				fatalf("push repository %s (source ledger remains inactive): %s", repository.Name, err)
			}
		}
		if _, err := gitOutput(stagedConfigRoot, "push", "--porcelain", "origin", configCommit+":refs/heads/main"); err != nil {
			fatalf("push config activation (repository branches may be staged ahead; no evaluation can select them yet): %s", err)
		}
		result.LedgerActivated = true
		// Product worktrees are runner inputs only for discovering their origins.
		// Winner patches are staged and pushed from private temporary clones, so
		// never change a developer or agent checkout after publication.
		configRoot, _ := gitOutput(filepath.Dir(sourceConfig), "rev-parse", "--show-toplevel")
		if err := fastForwardLocalBranch(configRoot, "main", configCommit); err != nil {
			fatalf("epoch activated, but local config repository did not fast-forward: %s", err)
		}
	}
	resultBytes, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("encode promotion result: %s", err)
	}
	fmt.Printf("%s\n", resultBytes)
}
