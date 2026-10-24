// Real local Git transports prove ignored data and credential-bearing config
// never enter a build context. No remote Git or candidate code executes.
package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

func TestSourceBuildSandboxCloneExcludesIgnoredSecretsAndOriginCredentials(t *testing.T) {
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "server")
	source := filepath.Join(root, "server")
	if err := os.WriteFile(filepath.Join(source, ".gitignore"), []byte("ignored-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, source, "add", ".gitignore")
	sourceTestGit(t, source, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic ignored data policy")
	commit = sourceTestGit(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "ignored-secret"), []byte("synthetic-secret-do-not-copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, source, "remote", "add", "origin", "https://synthetic-user:synthetic-secret@repository.example/project.git")
	destination := filepath.Join(root, "clean-server")
	if err := cloneSourceBuildRepository(context.Background(), "server", source, destination, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "ignored-secret")); !os.IsNotExist(err) {
		t.Fatal("ignored file entered context", err)
	}
	origin := sourceTestGit(t, destination, "remote", "get-url", "origin")
	if origin != sourceBuildPublicOrigin("server") || strings.Contains(origin, "synthetic-secret") || strings.Contains(origin, root) {
		t.Fatal("private origin entered context")
	}
	if got := sourceTestGit(t, source, "remote", "get-url", "origin"); !strings.Contains(got, "synthetic-secret") {
		t.Fatal("operator origin was modified")
	}
}

func TestSourceBuildSandboxCloneRejectsDirtyOrMismatchedSource(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		root := t.TempDir()
		commit := sourceTestRepository(t, root, "server")
		if dirty {
			if err := os.WriteFile(filepath.Join(root, "server", "source.txt"), []byte("uncommitted synthetic data\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			commit = strings.Repeat("e", 40)
		}
		destination := filepath.Join(root, "clean-server")
		if err := cloneSourceBuildRepository(context.Background(), "server", filepath.Join(root, "server"), destination, commit); err == nil {
			t.Fatal("unapproved source copied")
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("failed admission allocated clone", err)
		}
	}
}

// Build the complete real-shaped source map, then append exactly the reviewed
// server commit. Synthetic modules keep the test independent of moving main.
func sourceBuildContextFixture(t *testing.T) reviewedSourceBuildRequest {
	t.Helper()
	root := t.TempDir()
	commits := map[string]string{}
	roots := map[string]string{}
	for _, repository := range sourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
		roots[repository] = filepath.Join(root, repository)
	}
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].Repositories = sourceRepositoriesFromCommits(commits)
	baseLock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Generations[0].SourceLockSha256 = baseLock
	manifest.EvaluationSource.Rounds[0].SourceLockSha256 = baseLock
	current, proposal, _ := reviewedProposalFixture(t)
	current.Commits = commits
	current.SourceLockSha256 = baseLock
	current.Manifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	proposal.BaseSourceLockSha256 = baseLock
	proposal.Commits = map[string]string{}
	for repository, commit := range commits {
		proposal.Commits[repository] = commit
	}
	sourceTestGit(t, roots["server"], "checkout", "--quiet", "--detach", commits["server"])
	if err := os.WriteFile(filepath.Join(roots["server"], "source.txt"), []byte("reviewed synthetic change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, roots["server"], "add", "source.txt")
	sourceTestGit(t, roots["server"], "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic reviewed change")
	proposal.Commits["server"] = sourceTestGit(t, roots["server"], "rev-parse", "HEAD")
	proposal.SourceLockSha256, err = model.CompetitionSourceLockDigest(proposal.Commits)
	if err != nil {
		t.Fatal(err)
	}
	proposal.StagedRoots = roots
	proposal.EvaluatedImageDigest = "sha256:" + strings.Repeat("d", 64)
	content, err := reviewedSourceImageManifest(current, &proposal)
	if err != nil {
		t.Fatal(err)
	}
	return reviewedSourceBuildRequest{Operation: "isolated_reviewed_source_build", Proposal: &proposal, SourceManifest: content, SourceGeneration: 1,
		ConfigLocalDirectory: "/synthetic/config/local", VaultLocalDirectory: "/synthetic/vault/local", SandboxContract: "offline_evaluator_containment_v1"}
}

func TestSourceBuildSandboxContextAuthenticatesAndSealsCompleteCensus(t *testing.T) {
	request := sourceBuildContextFixture(t)
	settings := sourceSandboxTestSettings()
	settings.StateDirectory = t.TempDir()
	if err := os.Chmod(settings.StateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := strings.Repeat("a", 64)
	result, err := prepareSourceBuildContext(context.Background(), request, owner, settings)
	if err != nil {
		t.Fatal(err)
	}
	// Restore only this test-owned tree's directory write permission so t.TempDir
	// can remove it; production retains sealed success/failure evidence.
	t.Cleanup(func() {
		_ = filepath.WalkDir(result.SourceDirectory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	entries, err := os.ReadDir(result.SourceDirectory)
	if err != nil || len(entries) != len(sourceRepositoryNames()) {
		t.Fatal("incomplete context census", err)
	}
	for _, repository := range sourceRepositoryNames() {
		path := filepath.Join(result.SourceDirectory, repository)
		if got := sourceTestGit(t, path, "rev-parse", "HEAD"); got != request.Proposal.Commits[repository] {
			t.Fatal("wrong context commit", repository)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o555 {
			t.Fatal("source was not sealed", repository, err)
		}
	}
	lock, err := loadEvaluatorSourceLock(result.LockPath)
	if err != nil || len(lock.Repositories) != len(sourceRepositoryNames()) {
		t.Fatal("context lock failed", err)
	}
	if _, err := prepareSourceBuildContext(context.Background(), request, owner, settings); err == nil {
		t.Fatal("existing context was overwritten")
	}
}

func TestSourceBuildSandboxContextRejectsChangedStatisticsAndLocalScope(t *testing.T) {
	request := sourceBuildContextFixture(t)
	if err := validateSourceBuildRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Proposal.Significance.CandidateMeanRawScore++
	if err := validateSourceBuildRequest(request); err == nil {
		t.Fatal("different statistics reused manifest authority")
	}
	request.Proposal.Significance.CandidateMeanRawScore--
	request.ConfigLocalDirectory = "/synthetic/config/main"
	if err := validateSourceBuildRequest(request); err == nil {
		t.Fatal("main config entered build scope")
	}
}
