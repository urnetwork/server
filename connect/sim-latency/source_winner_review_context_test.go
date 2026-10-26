// Multi-repository reviews read real synthetic Git blobs from the exact frozen
// map, keeping same-named files and server-relative legacy paths separate.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Same relative names have different contents in each repository. Using the
// server checkout for a sibling path cannot accidentally satisfy this proof.
func TestWinnerReviewReadsExactMonoRootSourceContext(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	lockDigest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	patchText := "diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-connect\n+synthetic-connect\n" +
		"diff --git a/server/source.txt b/server/source.txt\n--- a/server/source.txt\n+++ b/server/source.txt\n@@ -1 +1 @@\n-server\n+synthetic-server\n"
	policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**", "server/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	patch, patchError := controller.ValidateAndCanonicalizePatch(patchText, policy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	packet := &controller.CompetitionSourceReviewPacket{Patch: patch.Bytes, PatchPolicy: policy, SourceCommits: commits, BaseSha: commits["server"],
		Entry: model.CompetitionSealedEntry{PatchSha256: patch.Sha256, BaseSourceLockSha256: lockDigest}}
	files, err := loadWinnerReviewSourceFiles(context.Background(), root, packet, defaultWinnerReviewSourceSettings())
	if err != nil || len(files) != 2 || files[0].Path != "connect/source.txt" || files[0].Content != "connect\n" || files[1].Content != "server\n" {
		t.Fatalf("routed source context: %+v %v", files, err)
	}
	commits["connect"] = strings.Repeat("f", 40)
	if _, err := loadWinnerReviewSourceFiles(context.Background(), root, packet, defaultWinnerReviewSourceSettings()); err == nil || !strings.Contains(err.Error(), "frozen lock") {
		t.Fatal("changed sibling source escaped the authenticated review lock", err)
	}
}

// Legacy fixture bytes keep their original server-relative interpretation.
func TestWinnerReviewPreservesServerRelativeSourceContext(t *testing.T) {
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "server")
	policy := controller.PatchPolicy{MaxPatchBytes: 4096, AllowedPaths: []string{"source.txt"}}
	patch, patchError := controller.ValidateAndCanonicalizePatch("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-server\n+synthetic-server\n", policy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	packet := &controller.CompetitionSourceReviewPacket{Patch: patch.Bytes, PatchPolicy: policy, BaseSha: commit, Entry: model.CompetitionSealedEntry{PatchSha256: patch.Sha256}}
	files, err := loadWinnerReviewSourceFiles(context.Background(), root, packet, defaultWinnerReviewSourceSettings())
	if err != nil || len(files) != 1 || files[0].Path != "source.txt" || files[0].Content != "server\n" {
		t.Fatalf("legacy source context: %+v %v", files, err)
	}
}

// The same empty byte digest has two distinct meanings: an existing empty
// blob is real base context, whereas a validated creation must prove absence.
func TestWinnerReviewDistinguishesCreatedPathsFromEmptyBaseFiles(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	connectRoot := filepath.Join(root, "connect")
	if err := os.WriteFile(filepath.Join(connectRoot, "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, connectRoot, "add", "--", "empty.txt")
	sourceTestGit(t, connectRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic empty base")
	commits["connect"] = sourceTestGit(t, connectRoot, "rev-parse", "HEAD")
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	for _, fixture := range []struct {
		path   string
		patch  string
		absent bool
	}{
		{path: "empty.txt", patch: "--- a/connect/empty.txt\n+++ b/connect/empty.txt\n@@ -0,0 +1 @@\n+content\n"},
		{path: "new.txt", patch: "new file mode 100644\n--- /dev/null\n+++ b/connect/new.txt\n@@ -0,0 +1 @@\n+content\n", absent: true},
		{path: "new-empty.txt", patch: "new file mode 100644\nindex 0000000..e69de29\n", absent: true},
	} {
		raw := "diff --git a/connect/" + fixture.path + " b/connect/" + fixture.path + "\n" + fixture.patch
		patch, patchError := controller.ValidateAndCanonicalizePatch(raw, policy)
		if patchError != nil {
			t.Fatal(patchError)
		}
		packet := &controller.CompetitionSourceReviewPacket{Patch: patch.Bytes, PatchPolicy: policy, SourceCommits: commits, BaseSha: commits["server"],
			Entry: model.CompetitionSealedEntry{PatchSha256: patch.Sha256, BaseSourceLockSha256: lock}}
		files, err := loadWinnerReviewSourceFiles(context.Background(), root, packet, defaultWinnerReviewSourceSettings())
		emptyDigest := sha256.Sum256(nil)
		if err != nil || len(files) != 1 || files[0].AbsentAtBase != fixture.absent || files[0].Content != "" || files[0].Sha256 != hex.EncodeToString(emptyDigest[:]) {
			t.Fatalf("%s absence=%t: context=%+v error=%v", fixture.path, fixture.absent, files, err)
		}
	}
}

// A successful literal path query must find an existing similarly named file
// rather than interpreting wildcard metacharacters as an absence assertion.
func TestWinnerReviewCreationCannotRelabelExistingBasePath(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	connectRoot := filepath.Join(root, "connect")
	const name = "literal[1].txt"
	if err := os.WriteFile(filepath.Join(connectRoot, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, connectRoot, "--literal-pathspecs", "add", "--", name)
	sourceTestGit(t, connectRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic literal filename")
	commits["connect"] = sourceTestGit(t, connectRoot, "rev-parse", "HEAD")
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	raw := "diff --git a/connect/" + name + " b/connect/" + name + "\nnew file mode 100644\nindex 0000000..e69de29\n"
	patch, patchError := controller.ValidateAndCanonicalizePatch(raw, policy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	packet := &controller.CompetitionSourceReviewPacket{Patch: patch.Bytes, PatchPolicy: policy, SourceCommits: commits, BaseSha: commits["server"],
		Entry: model.CompetitionSealedEntry{PatchSha256: patch.Sha256, BaseSourceLockSha256: lock}}
	if _, err := loadWinnerReviewSourceFiles(context.Background(), root, packet, defaultWinnerReviewSourceSettings()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal("existing empty literal path was reported absent", err)
	}
}

// Inject a failure after exact commit resolution, at the absence query itself.
// Native Git still resolves the real synthetic object; no provider is called.
func TestWinnerReviewNeverTreatsFailedGitAbsenceQueryAsMissingFile(t *testing.T) {
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		commits[repository] = sourceTestRepository(t, root, repository)
	}
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	patch, patchError := controller.ValidateAndCanonicalizePatch("diff --git a/connect/new.txt b/connect/new.txt\nnew file mode 100644\nindex 0000000..e69de29\n", policy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	packet := &controller.CompetitionSourceReviewPacket{Patch: patch.Bytes, PatchPolicy: policy, SourceCommits: commits, BaseSha: commits["server"],
		Entry: model.CompetitionSealedEntry{PatchSha256: patch.Sha256, BaseSourceLockSha256: lock}}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	shim := `#!/usr/bin/env bash
set -Eeuo pipefail
for argument in "$@"; do
    if [[ "$argument" == ls-tree ]]; then printf 'synthetic Git refusal\n' >&2; exit 128; fi
done
exec "$SOURCE_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOURCE_TEST_REAL_GIT", gitPath)
	files, err := loadWinnerReviewSourceFiles(context.Background(), root, packet, defaultWinnerReviewSourceSettings())
	if err == nil || len(files) != 0 {
		t.Fatalf("failed Git query fabricated review context: files=%+v error=%v", files, err)
	}
}
