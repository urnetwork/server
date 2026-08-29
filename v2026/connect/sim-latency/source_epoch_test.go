package main

// Deterministic source-epoch tests use isolated Git repositories so branch,
// head, and worktree mismatches are exercised without the operator checkout.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026/competition"
	"gopkg.in/yaml.v3"
)

// sourceTestGit runs Git with test-local author identity.
func sourceTestGit(t *testing.T, repositoryRoot string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repositoryRoot}, args...)
	command := exec.Command("git", commandArgs...)
	command.Env = append(
		os.Environ(),
		"GIT_AUTHOR_NAME=Source Test",
		"GIT_AUTHOR_EMAIL=source-test@invalid",
		"GIT_COMMITTER_NAME=Source Test",
		"GIT_COMMITTER_EMAIL=source-test@invalid",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// sourceTestRepository creates one clean sim-latency branch with one commit.
func sourceTestRepository(t *testing.T, repositoriesRoot string, name string) string {
	t.Helper()
	repositoryRoot := filepath.Join(repositoriesRoot, name)
	if err := os.MkdirAll(repositoryRoot, 0700); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repositoryRoot, "init", "--quiet", "--initial-branch=sim-latency")
	if err := os.WriteFile(filepath.Join(repositoryRoot, "source.txt"), []byte(name+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if name == "server" {
		protectedRoot := filepath.Join(repositoryRoot, "connect", "sim-latency")
		if err := os.MkdirAll(protectedRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(protectedRoot, "main.go"), []byte("package main\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sourceTestGit(t, repositoryRoot, "add", "--all")
	sourceTestGit(t, repositoryRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "source base")
	return sourceTestGit(t, repositoryRoot, "rev-parse", "HEAD")
}

// sourceTestManifest builds the complete baseline ledger required by validation.
func sourceTestManifest(repositoryCommits map[string]string) *sourceManifest {
	return &sourceManifest{
		Schema:   1,
		FrozenAt: "2026-08-28T00:00:00Z",
		EvaluationSource: evaluationSource{
			Branch: "sim-latency",
			Epochs: []sourceEpoch{{
				Epoch:                         0,
				Kind:                          "baseline",
				SignificantImprovementPercent: 16.1,
				Repositories: sourceRepositories{
					Connect: sourceRepository{Commit: repositoryCommits["connect"]},
					Sdk:     sourceRepository{Commit: repositoryCommits["sdk"]},
					Server:  sourceRepository{Commit: repositoryCommits["server"]},
					Proxy:   sourceRepository{Commit: repositoryCommits["proxy"]},
				},
			}},
		},
		ControlPlaneIdentity: controlPlaneIdentity{
			ApiBranch:                     "main",
			WorkerBranch:                  "main",
			RuntimeImageDigestEnvironment: "WARP_IMAGE_DIGEST",
			PersistPerEvaluation:          true,
			FreezeMainCommits:             false,
		},
	}
}

func validSourceSignificance() *sourceSignificance {
	return &sourceSignificance{
		ScoreSha256:                               strings.Repeat("b", 64),
		Method:                                    scoreSignificanceMethod,
		Alpha:                                     scoreSignificanceAlpha,
		ReplicateCount:                            9,
		BaselineMeanRawScore:                      100,
		CandidateMeanRawScore:                     70,
		BaselineSampleVariance:                    4,
		CandidateSampleVariance:                   4,
		ObservedImprovementPercent:                30,
		TakeoverMarginPercent:                     16.1,
		MinimumSignificantImprovementPercent:      2,
		RequiredImprovementPercent:                16.1,
		OneSidedPValue:                            0.001,
		NextEpochMinimumImprovementPercent:        2.5,
		RecommendedNextEpochTakeoverMarginPercent: 16.1,
	}
}

// writeSourceTestManifest serializes through the production strict decoder.
func writeSourceTestManifest(t *testing.T, path string, manifest *sourceManifest) {
	t.Helper()
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredSourceAcceptsExactEpochHeads(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
		repositoryCommits[repository] = sourceTestRepository(t, repositoriesRoot, repository)
	}
	manifestPath := filepath.Join(t.TempDir(), "sim-latency.yml")
	writeSourceTestManifest(t, manifestPath, sourceTestManifest(repositoryCommits))
	opts := docopt.Opts{
		"--epoch":         "0",
		"--source-config": manifestPath,
		"--repos-root":    repositoriesRoot,
	}
	epochNumber, sourceConfig, verifiedRoot, err := checkConfiguredSource(opts)
	if err != nil {
		t.Fatal(err)
	}
	if epochNumber != 0 || sourceConfig != manifestPath || verifiedRoot != repositoriesRoot {
		t.Fatalf("verified source = %d, %q, %q", epochNumber, sourceConfig, verifiedRoot)
	}
}

func TestConfiguredSourceRejectsRepositoryHeadMismatch(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
		repositoryCommits[repository] = sourceTestRepository(t, repositoriesRoot, repository)
	}
	serverRoot := filepath.Join(repositoriesRoot, "server")
	if err := os.WriteFile(filepath.Join(serverRoot, "source.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, serverRoot, "add", "source.txt")
	sourceTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "unexpected source")
	manifestPath := filepath.Join(t.TempDir(), "sim-latency.yml")
	writeSourceTestManifest(t, manifestPath, sourceTestManifest(repositoryCommits))
	opts := docopt.Opts{
		"--epoch":         "0",
		"--source-config": manifestPath,
		"--repos-root":    repositoriesRoot,
	}
	if _, _, _, err := checkConfiguredSource(opts); err == nil || !strings.Contains(err.Error(), "does not match epoch 0 commit") {
		t.Fatalf("repository head mismatch was accepted: %v", err)
	}
}

func TestConfiguredSourceRejectsDirtyRepository(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
		repositoryCommits[repository] = sourceTestRepository(t, repositoriesRoot, repository)
	}
	if err := os.WriteFile(filepath.Join(repositoriesRoot, "proxy", "untracked.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := sourceTestManifest(repositoryCommits)
	if err := verifySourceEpoch(manifest, 0, repositoriesRoot, ""); err == nil || !strings.Contains(err.Error(), "worktree is not clean") {
		t.Fatalf("dirty repository was accepted: %v", err)
	}
}

func TestConfiguredSourceRejectsMissingEpoch(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
		repositoryCommits[repository] = sourceTestRepository(t, repositoriesRoot, repository)
	}
	manifestPath := filepath.Join(t.TempDir(), "sim-latency.yml")
	writeSourceTestManifest(t, manifestPath, sourceTestManifest(repositoryCommits))
	opts := docopt.Opts{
		"--epoch":         "1",
		"--source-config": manifestPath,
		"--repos-root":    repositoriesRoot,
	}
	if _, _, _, err := checkConfiguredSource(opts); err == nil || !strings.Contains(err.Error(), "does not contain epoch 1") {
		t.Fatalf("missing epoch was accepted: %v", err)
	}
}

func TestSourceManifestRejectsMissingRepositoryCommit(t *testing.T) {
	repositoryCommit := strings.Repeat("a", 40)
	manifest := sourceTestManifest(map[string]string{
		"connect": repositoryCommit,
		"sdk":     repositoryCommit,
		"server":  "",
		"proxy":   repositoryCommit,
	})
	if err := validateSourceManifest(manifest); err == nil || !strings.Contains(err.Error(), "repository server has an invalid commit") {
		t.Fatalf("missing repository commit was accepted: %v", err)
	}
}

func TestSourceManifestCarriesCommitsForwardWhenEpochHasNoWinner(t *testing.T) {
	commit := strings.Repeat("a", 40)
	manifest := sourceTestManifest(map[string]string{
		"connect": commit,
		"sdk":     commit,
		"server":  commit,
		"proxy":   commit,
	})
	previousEpoch := 0
	manifest.EvaluationSource.Epochs = append(manifest.EvaluationSource.Epochs, sourceEpoch{
		Epoch:                         1,
		Kind:                          "no_winner_carry_forward",
		SignificantImprovementPercent: 16.1,
		PromotedFromEpoch:             &previousEpoch,
		PromotedAt:                    "2026-08-29T00:00:00Z",
		Repositories:                  manifest.EvaluationSource.Epochs[0].Repositories,
	})
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatalf("no-winner carry-forward rejected: %s", err)
	}
	manifest.EvaluationSource.Epochs[1].Repositories.Server.Commit = strings.Repeat("b", 40)
	if err := validateSourceManifest(manifest); err == nil || !strings.Contains(err.Error(), "carry repository commits forward unchanged") {
		t.Fatalf("changed no-winner commit was accepted: %v", err)
	}
	manifest.EvaluationSource.Epochs[1].Repositories = manifest.EvaluationSource.Epochs[0].Repositories
	manifest.EvaluationSource.Epochs[1].WinnerJobId = "invented-winner"
	if err := validateSourceManifest(manifest); err == nil || !strings.Contains(err.Error(), "cannot identify a winner") {
		t.Fatalf("winner metadata on no-winner epoch was accepted: %v", err)
	}
}

func TestSourceManifestRequiresWinnerSignificance(t *testing.T) {
	commit := strings.Repeat("a", 40)
	manifest := sourceTestManifest(map[string]string{
		"connect": commit,
		"sdk":     commit,
		"server":  commit,
		"proxy":   commit,
	})
	previousEpoch := 0
	manifest.EvaluationSource.Epochs = append(manifest.EvaluationSource.Epochs, sourceEpoch{
		Epoch:                         1,
		Kind:                          "winner_promotion",
		SignificantImprovementPercent: 16.1,
		WinnerJobId:                   "winner-1",
		WinnerSignificance:            validSourceSignificance(),
		PromotedFromEpoch:             &previousEpoch,
		PromotedAt:                    "2026-08-29T00:00:00Z",
		Repositories:                  manifest.EvaluationSource.Epochs[0].Repositories,
	})
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatalf("winner significance rejected: %s", err)
	}
	manifest.EvaluationSource.Epochs[1].WinnerSignificance.OneSidedPValue = 0.051
	if err := validateSourceManifest(manifest); err == nil ||
		!strings.Contains(err.Error(), "not statistically significant") {
		t.Fatalf("non-significant winner was accepted: %v", err)
	}
	manifest.EvaluationSource.Epochs[1].WinnerSignificance = nil
	if err := validateSourceManifest(manifest); err == nil ||
		!strings.Contains(err.Error(), "winning job and significance") {
		t.Fatalf("winner without significance was accepted: %v", err)
	}
}

func TestReadWinnerSignificanceRequiresEligibleR9Score(t *testing.T) {
	directory := t.TempDir()
	baselineVariance := 4.0
	candidateVariance := 4.0
	minimumPercent := 2.0
	requiredPercent := 16.1
	pValue := 0.001
	tStatistic := 10.0
	degreesOfFreedom := 16.0
	nextEpochMinimumPercent := 2.5
	recommendedPercent := 16.1
	gates := map[string]competition.Gate{}
	for _, name := range []string{
		"G1_success",
		"G2_volume",
		"G3_path_integrity",
		"G4_matchmaking",
		"G5_stability",
		"G6_resources",
	} {
		gates[name] = competition.Gate{Passed: true, Details: map[string]any{}}
	}
	rawScore := 70.0
	normalizedScore := 142.857
	result := &competition.ScoreResult{
		ScoreSchema:      competition.ScoreSchema,
		RawScore:         &rawScore,
		NormalizedScore:  &normalizedScore,
		Placeable:        true,
		TakeoverEligible: true,
		Gates:            gates,
		Significance: &competition.ScoreSignificance{
			Method:                                      scoreSignificanceMethod,
			Alpha:                                       scoreSignificanceAlpha,
			ReplicateCount:                              9,
			BaselineMeanRawScore:                        100,
			CandidateMeanRawScore:                       70,
			BaselineSampleVariance:                      &baselineVariance,
			CandidateSampleVariance:                     &candidateVariance,
			ObservedImprovementPercent:                  30,
			TakeoverMarginPercent:                       16.1,
			MinimumSignificantImprovementPercent:        &minimumPercent,
			RequiredImprovementPercent:                  &requiredPercent,
			OneSidedPValue:                              &pValue,
			WelchT:                                      &tStatistic,
			WelchDegreesOfFreedom:                       &degreesOfFreedom,
			StatisticallySignificant:                    true,
			NextEpochMinimumImprovementPercent:          &nextEpochMinimumPercent,
			RecommendedNextEpochTakeoverMarginPercent:   &recommendedPercent,
			RecommendedNextEpochTakeoverMarginSupported: true,
		},
		Diagnostics: map[string]any{"baseline_takeover_eligible": true},
	}
	writeScoreJSON(t, filepath.Join(directory, "score.json"), result)
	significance, err := readWinnerSignificance(directory)
	if err != nil {
		t.Fatal(err)
	}
	if significance.ReplicateCount != 9 || significance.OneSidedPValue != pValue ||
		!validSha256(significance.ScoreSha256) {
		t.Fatalf("winner significance = %+v", significance)
	}

	result.Significance.StatisticallySignificant = false
	writeScoreJSON(t, filepath.Join(directory, "score.json"), result)
	if _, err := readWinnerSignificance(directory); err == nil {
		t.Fatal("non-significant winner score was accepted")
	}
}

func TestEvaluatorSourceLockAcceptsOnlyOneServerCandidateCommit(t *testing.T) {
	repositoriesRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
		repositoryCommits[repository] = sourceTestRepository(t, repositoriesRoot, repository)
	}
	serverRoot := filepath.Join(repositoriesRoot, "server")
	if err := os.WriteFile(filepath.Join(serverRoot, "source.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, serverRoot, "add", "source.txt")
	sourceTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "candidate")
	lockBytes, err := json.Marshal(evaluatorSourceLock{
		Schema:              1,
		DevelopmentSnapshot: false,
		Repositories:        repositoryCommits,
	})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "source-lock.json")
	if err := os.WriteFile(lockPath, lockBytes, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := sourceTestManifest(repositoryCommits)
	if err := verifySourceEpoch(manifest, 0, repositoriesRoot, lockPath); err != nil {
		t.Fatal(err)
	}
	proxyRoot := filepath.Join(repositoriesRoot, "proxy")
	if err := os.WriteFile(filepath.Join(proxyRoot, "source.txt"), []byte("unapproved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, proxyRoot, "add", "source.txt")
	sourceTestGit(t, proxyRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "unapproved")
	if err := verifySourceEpoch(manifest, 0, repositoriesRoot, lockPath); err == nil || !strings.Contains(err.Error(), "does not match its authenticated source lock") {
		t.Fatalf("non-server candidate commit was accepted: %v", err)
	}
}

func TestStagePromotionRepositoryCommitsWinnerWithoutChangingCheckout(t *testing.T) {
	testRoot := t.TempDir()
	repositoriesRoot := filepath.Join(testRoot, "repositories")
	if err := os.Mkdir(repositoriesRoot, 0700); err != nil {
		t.Fatal(err)
	}
	previousCommit := sourceTestRepository(t, repositoriesRoot, "server")
	repositoryRoot := filepath.Join(repositoriesRoot, "server")
	remoteRoot := filepath.Join(testRoot, "server.git")
	sourceTestGit(t, testRoot, "init", "--quiet", "--bare", remoteRoot)
	sourceTestGit(t, repositoryRoot, "remote", "add", "origin", remoteRoot)
	sourceTestGit(t, repositoryRoot, "push", "--quiet", "--set-upstream", "origin", "sim-latency")
	patchClone := filepath.Join(testRoot, "patch-clone")
	sourceTestGit(t, testRoot, "clone", "--quiet", repositoryRoot, patchClone)
	if err := os.WriteFile(filepath.Join(patchClone, "source.txt"), []byte("winner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch := sourceTestGit(t, patchClone, "diff", "--binary") + "\n"
	patchPath := filepath.Join(testRoot, "server.patch")
	if err := os.WriteFile(patchPath, []byte(patch), 0600); err != nil {
		t.Fatal(err)
	}
	repository := &promotionRepository{
		Name:           "server",
		Branch:         "sim-latency",
		LocalRoot:      repositoryRoot,
		PreviousCommit: previousCommit,
		PatchPath:      patchPath,
	}
	stagingRoot := filepath.Join(testRoot, "staging")
	if err := os.Mkdir(stagingRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := stagePromotionRepository(repository, stagingRoot, "competition: test winner"); err != nil {
		t.Fatal(err)
	}
	if repository.NextCommit == previousCommit || !sourceGitShaPattern.MatchString(repository.NextCommit) {
		t.Fatalf("promotion commit = %q", repository.NextCommit)
	}
	if parent := sourceTestGit(t, repository.StagedRoot, "rev-parse", "HEAD^"); parent != previousCommit {
		t.Fatalf("promotion parent = %s, want %s", parent, previousCommit)
	}
	if branch := sourceTestGit(t, repository.StagedRoot, "symbolic-ref", "--short", "HEAD"); branch != "sim-latency" {
		t.Fatalf("promotion branch = %q, want sim-latency", branch)
	}
	stagedBytes, err := os.ReadFile(filepath.Join(repository.StagedRoot, "source.txt"))
	if err != nil || string(stagedBytes) != "winner\n" {
		t.Fatalf("staged winner content = %q, %v", stagedBytes, err)
	}
	localBytes, err := os.ReadFile(filepath.Join(repositoryRoot, "source.txt"))
	if err != nil || string(localBytes) != "server\n" {
		t.Fatalf("operator checkout changed during staging: %q, %v", localBytes, err)
	}
}

func TestStagePromotionRepositoryRejectsProtectedSimulatorPatch(t *testing.T) {
	testRoot := t.TempDir()
	repositoriesRoot := filepath.Join(testRoot, "repositories")
	if err := os.Mkdir(repositoriesRoot, 0700); err != nil {
		t.Fatal(err)
	}
	previousCommit := sourceTestRepository(t, repositoriesRoot, "server")
	repositoryRoot := filepath.Join(repositoriesRoot, "server")
	remoteRoot := filepath.Join(testRoot, "server.git")
	sourceTestGit(t, testRoot, "init", "--quiet", "--bare", remoteRoot)
	sourceTestGit(t, repositoryRoot, "remote", "add", "origin", remoteRoot)
	sourceTestGit(t, repositoryRoot, "push", "--quiet", "--set-upstream", "origin", "sim-latency")
	patchClone := filepath.Join(testRoot, "patch-clone")
	sourceTestGit(t, testRoot, "clone", "--quiet", repositoryRoot, patchClone)
	protectedPath := filepath.Join(patchClone, "connect", "sim-latency", "main.go")
	if err := os.WriteFile(protectedPath, []byte("package main\n\n// tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch := sourceTestGit(t, patchClone, "diff", "--binary") + "\n"
	patchPath := filepath.Join(testRoot, "server.patch")
	if err := os.WriteFile(patchPath, []byte(patch), 0600); err != nil {
		t.Fatal(err)
	}
	repository := &promotionRepository{
		Name: "server", Branch: "sim-latency", LocalRoot: repositoryRoot,
		PreviousCommit: previousCommit, PatchPath: patchPath,
	}
	stagingRoot := filepath.Join(testRoot, "staging")
	if err := os.Mkdir(stagingRoot, 0700); err != nil {
		t.Fatal(err)
	}
	err := stagePromotionRepository(repository, stagingRoot, "competition: malicious winner")
	if err == nil || !strings.Contains(err.Error(), "protected sim-latency source tree") {
		t.Fatalf("protected promotion error = %v", err)
	}
	if head := sourceTestGit(t, repositoryRoot, "rev-parse", "HEAD"); head != previousCommit {
		t.Fatalf("operator checkout changed to %s", head)
	}
}
