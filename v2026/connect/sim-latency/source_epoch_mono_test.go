// Broad source identities require an explicit newly qualified instrument flag;
// old immutable image locks keep their original server-only candidate boundary.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every repository is either the exact frozen base or one direct candidate
// commit above that base, with clean worktrees throughout the full graph.
func TestEvaluatorMonoSourceLockAcceptsDirectCandidateInEveryRepository(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range sourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	manifest := sourceTestManifest(commits)
	lock := evaluatorSourceLock{Schema: 1, CandidatePathBase: "mono_root_v1", Repositories: commits}
	content, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source-lock.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	for _, repository := range sourceRepositoryNames() {
		repositoryRoot := filepath.Join(root, repository)
		if err := os.WriteFile(filepath.Join(repositoryRoot, "source.txt"), []byte("candidate\n"), 0600); err != nil {
			t.Fatal(err)
		}
		sourceTestGit(t, repositoryRoot, "add", "source.txt")
		sourceTestGit(t, repositoryRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "candidate")
		if err := verifySourceEpoch(manifest, 0, root, path); err != nil {
			t.Fatalf("explicit broad candidate refused: %s %v", repository, err)
		}
	}
	dependency := filepath.Join(root, "connect")
	if err := os.WriteFile(filepath.Join(dependency, "source.txt"), []byte("second candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, dependency, "add", "source.txt")
	sourceTestGit(t, dependency, "commit", "--quiet", "--no-gpg-sign", "-m", "unbound second candidate")
	if err := verifySourceEpoch(manifest, 0, root, path); err == nil || !strings.Contains(err.Error(), "exactly one commit") {
		t.Fatalf("two commits above frozen dependency accepted: %v", err)
	}
}

// Unknown flags cannot create permissive candidate semantics by accident.
func TestEvaluatorSourceLockRejectsUnknownCandidatePathBase(t *testing.T) {
	lock := evaluatorSourceLock{Schema: 1, CandidatePathBase: "unknown_mode", Repositories: sourceTestCommits(strings.Repeat("a", 40))}
	content, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source-lock.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEvaluatorSourceLock(path); err == nil || !strings.Contains(err.Error(), "candidate path base") {
		t.Fatalf("unknown candidate semantics accepted: %v", err)
	}
}
