// Source application tests create only synthetic local Git repositories.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Existing private-applier fixtures describe only source.txt modifications.
// Full CLI tests exercise the real canonical parser and all operation modes.
func applyRepositoryPatches(root string, identityBytes []byte, expectedIdentitySha256 string, baseSha string, patchSha256 string, pathBase string, patches map[string][]byte, settings repositoryApplySettings) (map[string]string, error) {
	canonical := repositoryApplyTestMetadata(pathBase, patches, patchSha256)
	return applyRepositoryPatchesContext(context.Background(), root, identityBytes, expectedIdentitySha256, baseSha, patchSha256, controller.PatchPolicy{PathBase: pathBase}, canonical, patches, settings)
}

// Supplies trusted metadata for the Git-only source.txt fixtures above.
func repositoryApplyTestMetadata(pathBase string, patches map[string][]byte, digest string) *controller.CanonicalPatch {
	canonical := &controller.CanonicalPatch{Sha256: digest, FileOperations: map[string]controller.CompetitionPatchFileOperation{}}
	for repository := range patches {
		path := "source.txt"
		if pathBase != "" {
			path = repository + "/" + path
		}
		canonical.Paths = append(canonical.Paths, path)
		canonical.FileOperations[path] = controller.CompetitionPatchFileOperation{Operation: "modify"}
	}
	return canonical
}

// Explicit local identity avoids depending on the machine owner's Git settings.
func repositoryApplyTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Synthetic Source", "-c", "user.email=source@account.example", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic Git fixture: %v %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

// Returns exact trusted-preparer bytes and independently observable base heads.
func repositoryApplyFixture(t *testing.T) (string, []byte, string, map[string]string) {
	t.Helper()
	// TempDir's leaf is umask-filtered 0777, unlike the preparer's 0700 root.
	// Create our own private leaf and return its canonical absolute spelling.
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		path := filepath.Join(root, repository)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		repositoryApplyTestGit(t, path, "init", "--quiet", "--initial-branch=sim-latency")
		if err := os.WriteFile(filepath.Join(path, "source.txt"), []byte("original\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if repository == "server" {
			protected := filepath.Join(path, "connect", "sim-latency")
			if err := os.MkdirAll(protected, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(protected, "sim.go"), []byte("package main\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		repositoryApplyTestGit(t, path, "add", "--all")
		repositoryApplyTestGit(t, path, "commit", "--quiet", "-m", "synthetic base")
		commits[repository] = repositoryApplyTestGit(t, path, "rev-parse", "HEAD")
	}
	identity := repositorySourceIdentity{Schema: 1, Kind: "sim-latency-evaluation-source", Temporary: true, BaseImageId: "sha256:" + strings.Repeat("a", 64), BaseSha: commits["server"], Branch: "sim-latency", SourceLockSha256: strings.Repeat("b", 64), CandidatePathBase: "mono_root_v1", Repositories: commits}
	content, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return root, content, hex.EncodeToString(digest[:]), commits
}

// Shared routed bytes apply to their own repositories without moving server.
func TestRepositorySourceAppliesDependencyPatchesAndRetainsFrozenHeads(t *testing.T) {
	root, content, digest, original := repositoryApplyFixture(t)
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+improved\n")
	commits, err := applyRepositoryPatches(root, content, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch, "sdk": patch}, defaultRepositoryApplySettings())
	if err != nil {
		t.Fatal(err)
	}
	for repository, previous := range original {
		changed := repository == "connect" || repository == "sdk"
		if changed == (commits[repository] == previous) {
			t.Fatalf("wrong changed head: %s", repository)
		}
		if actual := repositoryApplyTestGit(t, filepath.Join(root, repository), "rev-parse", "HEAD"); actual != commits[repository] {
			t.Fatalf("unbound resulting head: %s", repository)
		}
	}
	if _, err := applyRepositoryPatches(root, content, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err == nil {
		t.Fatal("previously applied source accepted as pristine")
	}
}

// Validate the entire graph before changing even an unrelated healthy checkout.
func TestRepositorySourceDirtySiblingRefusesBeforeAnyMutation(t *testing.T) {
	root, content, digest, original := repositoryApplyFixture(t)
	if err := os.WriteFile(filepath.Join(root, "warp", "source.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+improved\n")
	if _, err := applyRepositoryPatches(root, content, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err == nil || err.Error() != "source repository differs from its pristine frozen identity" {
		t.Fatal("dirty sibling graph did not reach its frozen-identity refusal", err)
	}
	if status := repositoryApplyTestGit(t, filepath.Join(root, "connect"), "status", "--porcelain=v1"); status != "" {
		t.Fatal("earlier repository mutated before dirty sibling refusal")
	}
}

// Inherited index overrides must not redirect trusted source Git writes.
func TestRepositorySourceIgnoresInheritedGitIndexOverride(t *testing.T) {
	root, content, digest, original := repositoryApplyFixture(t)
	outside := filepath.Join(t.TempDir(), "unrelated-index")
	if err := os.WriteFile(outside, []byte("unrelated retained bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", outside)
	patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+improved\n")
	if _, err := applyRepositoryPatches(root, content, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings()); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(outside)
	if err != nil || string(actual) != "unrelated retained bytes\n" {
		t.Fatal("source application modified unrelated Git index")
	}
}
