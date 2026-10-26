// Mono-root text operations retain canonical Git structure and explicit path
// identity. Historical policies cannot gain creation/deletion authority.
package controller

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// Creates or deletes one complete regular text file, with an optional index.
func competitionFileOperationFixture(path string, addition bool, index string) string {
	operation, oldPath, newPath, hunk, body := "new file mode 100644", "/dev/null", "b/"+path, "@@ -0,0 +1,2 @@", "+first\n+second\n"
	if !addition {
		operation, oldPath, newPath, hunk, body = "deleted file mode 100644", "a/"+path, "/dev/null", "@@ -1,2 +0,0 @@", "-first\n-second\n"
	}
	if index != "" {
		index = "index " + index + "\n"
	}
	return "diff --git a/" + path + " b/" + path + "\n" + operation + "\n" + index + "--- " + oldPath + "\n+++ " + newPath + "\n" + hunk + "\n" + body
}

// Routing must preserve /dev/null while stripping only the repository prefix
// from real paths. The added-path list comes from this same canonical parser.
func TestCompetitionRepositoryTextCreationDeletionRoutesExactly(t *testing.T) {
	addition := competitionFileOperationFixture("connect/new.txt", true, "0000000..1111111")
	deletion := competitionFileOperationFixture("sdk/old.txt", false, "2222222..0000000")
	raw := addition + deletion
	policy := competitionRepositoryTestPolicy()
	patch, evalError := ValidateAndCanonicalizePatch(raw, policy)
	if evalError != nil || !bytes.Equal(patch.Bytes, []byte(raw)) || !slices.Equal(patch.Paths, []string{"connect/new.txt", "sdk/old.txt"}) || !slices.Equal(patch.AddedPaths, []string{"connect/new.txt"}) {
		t.Fatalf("canonical operations changed: %+v %v", patch, evalError)
	}
	routed, evalError := SplitCompetitionRepositoryPatch(raw, policy)
	if evalError != nil || len(routed) != 2 || string(routed["connect"]) != strings.ReplaceAll(addition, "connect/new.txt", "new.txt") || string(routed["sdk"]) != strings.ReplaceAll(deletion, "sdk/old.txt", "old.txt") {
		t.Fatalf("misrouted file operations: %q %v", routed, evalError)
	}
}

// The exact empty SHA-1 blob index authenticates a standard no-hunk regular
// empty-file operation; arbitrary headerless metadata is never sufficient.
func TestCompetitionRepositoryEmptyFileOperationsRequireExactEmptyBlob(t *testing.T) {
	const emptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	for _, length := range []int{7, 12, 40} {
		addition := "diff --git a/connect/empty.txt b/connect/empty.txt\nnew file mode 100644\nindex " + strings.Repeat("0", length) + ".." + emptyBlob[:length] + "\n"
		deletion := "diff --git a/sdk/empty.txt b/sdk/empty.txt\ndeleted file mode 100644\nindex " + emptyBlob[:length] + ".." + strings.Repeat("0", length) + "\n"
		patch, evalError := ValidateAndCanonicalizePatch(addition+deletion, competitionRepositoryTestPolicy())
		if evalError != nil || !slices.Equal(patch.AddedPaths, []string{"connect/empty.txt"}) {
			t.Fatalf("standard empty-file operations refused at abbreviation %d: %v", length, evalError)
		}
		routed, evalError := SplitCompetitionRepositoryPatch(addition+deletion, competitionRepositoryTestPolicy())
		if evalError != nil || len(routed) != 2 || string(routed["connect"]) != strings.ReplaceAll(addition, "connect/empty.txt", "empty.txt") || string(routed["sdk"]) != strings.ReplaceAll(deletion, "sdk/empty.txt", "empty.txt") {
			t.Fatalf("empty-file metadata was changed by repository routing: %q %v", routed, evalError)
		}
		for _, raw := range []string{
			strings.Replace(addition, emptyBlob[:length], strings.Repeat("1", length), 1),
			strings.Replace(deletion, emptyBlob[:length], strings.Repeat("1", length), 1),
			strings.Replace(addition, "index "+strings.Repeat("0", length)+".."+emptyBlob[:length]+"\n", "", 1),
			addition + "--- /dev/null\n+++ b/connect/empty.txt\n",
			addition + "--- /dev/null\n+++ b/connect/empty.txt\n@@ -0,0 +1 @@\n+not empty\n",
		} {
			if _, evalError := ValidateAndCanonicalizePatch(raw, competitionRepositoryTestPolicy()); evalError == nil {
				t.Fatalf("contradictory empty-file operation accepted: %q", raw)
			}
		}
	}
}

// Each mutation breaks a structural or regular-file invariant while leaving
// the rest of an otherwise valid canonical operation intact.
func TestCompetitionRepositoryFileOperationsRejectAmbiguousOrUnsafeMetadata(t *testing.T) {
	addition := competitionFileOperationFixture("connect/new.txt", true, "0000000..1111111")
	deletion := competitionFileOperationFixture("sdk/old.txt", false, "2222222..0000000")
	for _, raw := range []string{
		strings.Replace(addition, "100644", "100755", 1),
		strings.Replace(addition, "100644", "120000", 1),
		strings.Replace(deletion, "100644", "160000", 1),
		strings.Replace(addition, "new file mode 100644\n", "new file mode 100644\nnew file mode 100644\n", 1),
		strings.Replace(addition, "new file mode 100644\n", "new file mode 100644\ndeleted file mode 100644\n", 1),
		strings.Replace(addition, "index 0000000..1111111\n", "index 0000000..1111111\nindex 0000000..1111111\n", 1),
		strings.Replace(addition, "0000000..1111111", "2222222..1111111", 1),
		strings.Replace(deletion, "2222222..0000000", "2222222..1111111", 1),
		strings.Replace(addition, "0000000..1111111", "0000000..0000000", 1),
		strings.Replace(addition, "0000000..1111111", "0000000..11111111", 1),
		strings.Replace(addition, "--- /dev/null", "--- a/connect/new.txt", 1),
		strings.Replace(deletion, "+++ /dev/null", "+++ b/sdk/old.txt", 1),
		strings.Replace(addition, "new file mode 100644\n", "", 1),
		strings.Replace(addition, "@@ -0,0 +1,2 @@", "@@ -0,1 +1,2 @@", 1),
		strings.Replace(deletion, "@@ -1,2 +0,0 @@", "@@ -1,2 +0,1 @@", 1),
		strings.Replace(addition, "@@ -0,0 +1,2 @@", "@@ -1,0 +1,2 @@", 1),
		strings.Replace(deletion, "@@ -1,2 +0,0 @@", "@@ -1,2 +1,0 @@", 1),
		strings.Replace(addition, "+second\n", " second\n", 1),
		strings.Replace(deletion, "-second\n", "+second\n", 1),
		addition + "@@ -0,0 +3 @@\n+third\n",
		strings.Replace(addition, "--- /dev/null\n", "old mode 100644\nnew mode 100755\n--- /dev/null\n", 1),
		strings.Replace(addition, "--- /dev/null\n", "rename from old.txt\n--- /dev/null\n", 1),
		strings.Replace(addition, "--- /dev/null\n", "GIT binary patch\n--- /dev/null\n", 1),
		strings.Replace(addition, "+first\n", "+Subproject commit 1111111\n", 1),
	} {
		if _, evalError := ValidateAndCanonicalizePatch(raw, competitionRepositoryTestPolicy()); evalError == nil {
			t.Fatalf("unsafe file operation accepted: %q", raw)
		}
	}
}

// Source scope cannot broaden a historical policy, even for a harmless file.
func TestCompetitionRepositoryFileOperationsPreserveLegacyRefusal(t *testing.T) {
	policy := PatchPolicy{MaxPatchBytes: 262144, AllowedPaths: []string{"product.txt"}, ForbiddenPaths: []string{protectedSimulatorTreePattern}}
	for _, raw := range []string{
		competitionFileOperationFixture("product.txt", true, "0000000..1111111"),
		competitionFileOperationFixture("product.txt", false, "1111111..0000000"),
		"diff --git a/product.txt b/product.txt\nnew file mode 100644\nindex 0000000..e69de29\n",
	} {
		if _, evalError := ValidateAndCanonicalizePatch(raw, policy); evalError == nil || evalError.Code != "unsupported_patch_operation" {
			t.Fatalf("legacy new/deleted-file policy changed: %v", evalError)
		}
	}
}
