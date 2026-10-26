// Source-mode validation is independent of optional index metadata. Synthetic
// Git records exercise the shared predicate used by all real Git entry points.
package controller

import (
	"errors"
	"strings"
	"testing"
)

// Executable regular text is not a mode change or permission to run candidate
// code. Both regular modes retain canonical create/modify/delete semantics.
func TestCompetitionRepositoryPatchPreservesRegularExecutableTextModes(t *testing.T) {
	policy := competitionRepositoryTestPolicy()
	for _, raw := range []string{
		strings.Replace(competitionFileOperationFixture("connect/script.sh", true, "0000000..1111111"), "100644", "100755", 1),
		strings.Replace(competitionFileOperationFixture("connect/script.sh", false, "1111111..0000000"), "100644", "100755", 1),
		"diff --git a/connect/script.sh b/connect/script.sh\nindex 1111111..2222222 100755\n--- a/connect/script.sh\n+++ b/connect/script.sh\n@@ -1 +1 @@\n-old\n+new\n",
		"diff --git a/connect/script.sh b/connect/script.sh\nnew file mode 100755\nindex 0000000..e69de29\n",
		"diff --git a/connect/script.sh b/connect/script.sh\ndeleted file mode 100755\nindex e69de29..0000000\n",
	} {
		patch, evalError := ValidateAndCanonicalizePatch(raw, policy)
		if evalError != nil || patch.FileOperations["connect/script.sh"].Mode != "100755" {
			t.Fatalf("regular executable text was refused or lost its mode: %v", evalError)
		}
	}
	for _, raw := range []string{
		"diff --git a/connect/script.sh b/connect/script.sh\nold mode 100644\nnew mode 100755\n",
		"diff --git a/connect/script.sh b/connect/script.sh\nnew file mode 100755\nindex 0000000..1111111 100644\n--- /dev/null\n+++ b/connect/script.sh\n@@ -0,0 +1 @@\n+new\n",
	} {
		if _, evalError := ValidateAndCanonicalizePatch(raw, policy); evalError == nil {
			t.Fatalf("contradictory mode metadata accepted: %q", raw)
		}
	}
	legacy := PatchPolicy{MaxPatchBytes: 4096, AllowedPaths: []string{"script.sh"}, ForbiddenPaths: []string{protectedSimulatorTreePattern}}
	if _, evalError := ValidateAndCanonicalizePatch("diff --git a/script.sh b/script.sh\nindex 1111111..2222222 100755\n--- a/script.sh\n+++ b/script.sh\n@@ -1 +1 @@\n-old\n+new\n", legacy); evalError == nil {
		t.Fatal("legacy index-mode authority was broadened")
	}
}

// The absent side is determined by a successful immutable tree read, never by
// omitted diff mode metadata, an empty base blob, or a failed Git invocation.
func TestCompetitionRepositoryPatchBaseRequiresPinnedRegularMode(t *testing.T) {
	policy := competitionRepositoryTestPolicy()
	for _, fixture := range []struct {
		operation string
		declared  string
		actual    string
		kind      string
		missing   bool
		want      bool
	}{
		{operation: "modify", actual: "100644", kind: "blob", want: true},
		{operation: "modify", actual: "100755", kind: "blob", want: true},
		{operation: "modify", actual: "120000", kind: "blob"},
		{operation: "modify", actual: "160000", kind: "commit"},
		{operation: "modify", actual: "040000", kind: "tree"},
		{operation: "modify", declared: "100644", actual: "100755", kind: "blob"},
		{operation: "modify", missing: true},
		{operation: "create", declared: "100755", missing: true, want: true},
		{operation: "create", declared: "100644", actual: "100644", kind: "blob"},
		{operation: "delete", declared: "100755", actual: "100755", kind: "blob", want: true},
		{operation: "delete", declared: "100755", actual: "100644", kind: "blob"},
		{operation: "delete", declared: "100644", missing: true},
	} {
		patch := &CanonicalPatch{Paths: []string{"connect/script.sh"}, FileOperations: map[string]CompetitionPatchFileOperation{"connect/script.sh": {Operation: fixture.operation, Mode: fixture.declared}}}
		err := ValidateCompetitionRepositoryPatchBase(patch, policy, "connect", func(relative string) ([]byte, error) {
			if relative != "script.sh" {
				t.Fatal("tree lookup did not select the exact routed path", relative)
			}
			if fixture.missing {
				return nil, nil
			}
			return []byte(fixture.actual + " " + fixture.kind + " " + strings.Repeat("a", 40) + "\t" + relative + "\x00"), nil
		})
		if (err == nil) != fixture.want {
			t.Fatalf("operation=%s declared=%s actual=%s missing=%t: %v", fixture.operation, fixture.declared, fixture.actual, fixture.missing, err)
		}
	}
	patch := &CanonicalPatch{Paths: []string{"connect/new.sh"}, FileOperations: map[string]CompetitionPatchFileOperation{"connect/new.sh": {Operation: "create", Mode: "100755"}}}
	failure := errors.New("synthetic Git failure")
	if err := ValidateCompetitionRepositoryPatchBase(patch, policy, "connect", func(string) ([]byte, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal("Git failure was translated into creation absence", err)
	}
}
