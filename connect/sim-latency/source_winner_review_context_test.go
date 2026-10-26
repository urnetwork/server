// Multi-repository reviews read real synthetic Git blobs from the exact frozen
// map, keeping same-named files and server-relative legacy paths separate.
package main

import (
	"context"
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
