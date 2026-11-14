// Exercises the actual Dockerfile metadata step against synthetic source graphs.
// The base image stays immutable; only a private build layer gains branch refs.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Runs the production shell block with only its two absolute input locations
// redirected into the fixture. No Docker daemon or production data is required.
func repositoryApplyImageBranches(t *testing.T, root string, identity []byte, digest string) error {
	t.Helper()
	content, err := os.ReadFile("../../connect/sim-latency/evaluator/container/Dockerfile.submission")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(content), "# BEGIN authenticated image branch materialization\n")
	if !ok {
		t.Fatal("image source materialization block missing")
	}
	block, _, ok = strings.Cut(block, "# END authenticated image branch materialization")
	if !ok {
		t.Fatal("image source materialization block unterminated")
	}
	_, block, ok = strings.Cut(block, "RUN --network=none ")
	if !ok {
		t.Fatal("image metadata materialization must remain offline")
	}
	path := filepath.Join(t.TempDir(), "original-source.json")
	if err := os.WriteFile(path, identity, 0600); err != nil {
		t.Fatal(err)
	}
	block = strings.ReplaceAll(block, "\\\n", "\n")
	block = strings.ReplaceAll(block, "identity=/opt/urnetwork/submission/original-source.json;", fmt.Sprintf("identity=%q;", path))
	block = strings.ReplaceAll(block, "root=/workspace;", fmt.Sprintf("root=%q;", root))
	command := exec.Command("sh", "-ec", block)
	command.Env = append(os.Environ(), "ORIGINAL_SOURCE_IDENTITY_SHA256="+digest)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("synthetic image metadata: %w: %s", err, output)
	}
	return nil
}

// Frozen images legitimately retain a previous branch ref beneath detached
// current heads. The real patch applier must still accept the resulting graph.
func TestRepositoryImageDetachedHeadsAttachWithoutChangingFrozenCommits(t *testing.T) {
	root, identity, digest, original := repositoryApplyFixture(t)
	for repository := range original {
		path := filepath.Join(root, repository)
		repositoryApplyTestGit(t, path, "checkout", "--quiet", "--detach", "HEAD")
		stale := repositoryApplyTestGit(t, path, "commit-tree", "HEAD^{tree}", "-m", "synthetic stale image ref")
		repositoryApplyTestGit(t, path, "update-ref", "refs/heads/sim-latency", stale)
	}
	if err := repositoryApplyImageBranches(t, root, identity, digest); err != nil {
		t.Fatal(err)
	}
	for repository, expected := range original {
		path := filepath.Join(root, repository)
		if repositoryApplyTestGit(t, path, "rev-parse", "HEAD") != expected || repositoryApplyTestGit(t, path, "symbolic-ref", "--short", "HEAD") != "sim-latency" || repositoryApplyTestGit(t, path, "status", "--porcelain=v1") != "" {
			t.Fatal("image metadata materialization changed frozen source", repository)
		}
	}
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+improved\n")
	if _, err := applyRepositoryPatches(root, identity, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err != nil {
		t.Fatal("attached exact image graph refused by real applier", err)
	}
}

// Checking every head before writing any ref prevents an unrelated mismatched
// sibling from being disguised by the normalization of an earlier repository.
func TestRepositoryImageMismatchedHeadRefusesBeforeBranchMutation(t *testing.T) {
	root, identity, digest, original := repositoryApplyFixture(t)
	for repository := range original {
		repositoryApplyTestGit(t, filepath.Join(root, repository), "checkout", "--quiet", "--detach", "HEAD")
	}
	repositoryApplyTestGit(t, filepath.Join(root, "warp"), "commit", "--quiet", "--allow-empty", "-m", "synthetic unexpected image commit")
	if err := repositoryApplyImageBranches(t, root, identity, digest); err == nil {
		t.Fatal("mismatched image commit accepted")
	}
	for repository := range original {
		head, err := os.ReadFile(filepath.Join(root, repository, ".git", "HEAD"))
		if err != nil || strings.HasPrefix(string(head), "ref:") {
			t.Fatal("failed graph check changed branch metadata", repository, err)
		}
	}
}

// A detached head is not permission to discard uncommitted files. The unchanged
// application boundary independently refuses the dirty graph before patching.
func TestRepositoryImageDirtyWorktreeStillRejectedByPatchApplier(t *testing.T) {
	root, identity, digest, original := repositoryApplyFixture(t)
	for repository := range original {
		repositoryApplyTestGit(t, filepath.Join(root, repository), "checkout", "--quiet", "--detach", "HEAD")
	}
	if err := os.WriteFile(filepath.Join(root, "warp", "source.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repositoryApplyImageBranches(t, root, identity, digest); err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+improved\n")
	if _, err := applyRepositoryPatches(root, identity, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err == nil {
		t.Fatal("dirty frozen source was accepted")
	}
	if status := repositoryApplyTestGit(t, filepath.Join(root, "connect"), "status", "--porcelain=v1"); status != "" {
		t.Fatal("failed image admission applied a patch")
	}
}

// A named wrong branch is an identity error, not the detached-image case.
func TestRepositoryImageWrongAttachedBranchAndIdentityDigestRefused(t *testing.T) {
	root, identity, digest, _ := repositoryApplyFixture(t)
	repositoryApplyTestGit(t, filepath.Join(root, "warp"), "checkout", "--quiet", "-b", "synthetic-wrong-branch")
	if err := repositoryApplyImageBranches(t, root, identity, digest); err == nil {
		t.Fatal("wrong attached branch accepted")
	}
	if err := repositoryApplyImageBranches(t, root, identity, strings.Repeat("0", 64)); err == nil {
		t.Fatal("unauthenticated source identity accepted")
	}
}
