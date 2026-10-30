// Real synthetic Git objects prove that omitted index modes cannot smuggle
// symlink edits, while ordinary executable text remains ordinary source data.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Native tree modes, not diff claims, decide whether source is regular. An
// injected ls-tree failure must stop before Git apply and return infrastructure.
func TestCompetitionPatchCliChecksPinnedModesBeforeApplying(t *testing.T) {
	for _, fixture := range []struct {
		executable bool
		symlink    bool
		gitFailure bool
	}{
		{}, {executable: true}, {symlink: true}, {gitFailure: true},
	} {
		root, identityBytes, identityDigest, original := repositoryApplyFixture(t)
		connectRoot := filepath.Join(root, "connect")
		path := filepath.Join(connectRoot, "source.txt")
		body := "-original\n+changed\n"
		if fixture.symlink {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("old-target.txt", path); err != nil {
				t.Fatal(err)
			}
			body = "-old-target.txt\n\\ No newline at end of file\n+new-target.txt\n\\ No newline at end of file\n"
		} else if fixture.executable {
			if err := os.Chmod(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if fixture.symlink || fixture.executable {
			repositoryApplyTestGit(t, connectRoot, "add", "--", "source.txt")
			repositoryApplyTestGit(t, connectRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic tracked mode")
			original["connect"] = repositoryApplyTestGit(t, connectRoot, "rev-parse", "HEAD")
			var identity repositorySourceIdentity
			if err := json.Unmarshal(identityBytes, &identity); err != nil {
				t.Fatal(err)
			}
			identity.Repositories = original
			var err error
			identityBytes, err = json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(identityBytes)
			identityDigest = hex.EncodeToString(digest[:])
		}
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n" + body)
		policy := []byte(`{"path_base":"mono_root_v1","max_patch_bytes":4096,"allowed_paths":["connect/**"],"forbidden_paths":["server/connect/sim-latency/**"]}`)
		policyDigest, patchDigest := sha256.Sum256(policy), sha256.Sum256(patch)
		inputs := t.TempDir()
		patchPath := writeTestFile(t, inputs, "canonical.patch", patch)
		policyPath := writeTestFile(t, inputs, "policy.json", policy)
		identityPath := writeTestFile(t, inputs, "original-source.json", identityBytes)
		if fixture.gitFailure {
			gitPath, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			shim := "#!/usr/bin/env bash\nset -Eeuo pipefail\nfor argument in \"$@\"; do\n  if [[ \"$argument\" == ls-tree ]]; then exit 128; fi\n  if [[ \"$argument\" == apply ]]; then printf 'unexpected apply\\n' > \"$COMPETITION_TEST_APPLY_MARKER\"; fi\ndone\nexec \"$COMPETITION_TEST_REAL_GIT\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("COMPETITION_TEST_REAL_GIT", gitPath)
			t.Setenv("COMPETITION_TEST_APPLY_MARKER", filepath.Join(inputs, "apply-called"))
		}
		var output bytes.Buffer
		err := run([]string{"--base-sha", original["server"], "--policy", policyPath,
			"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--patch", patchPath,
			"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--apply-source-root", root,
			"--source-identity", identityPath, "--expected-source-identity-sha256", identityDigest}, &output)
		if fixture.symlink || fixture.gitFailure {
			if err == nil || competitionPatchExitCode(err) != 2 || output.Len() != 0 {
				t.Fatalf("untrusted base mode/read produced a candidate receipt: %v %q", err, output.String())
			}
			if fixture.symlink && !strings.Contains(err.Error(), "pinned regular source blob") {
				t.Fatal("symlink refusal came from an unrelated later Git failure", err)
			}
			if _, err := os.Stat(filepath.Join(inputs, "apply-called")); !os.IsNotExist(err) {
				t.Fatal("Git failure crossed the before-apply gate", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil || (info.Mode().Perm()&0o111 != 0) != fixture.executable {
			t.Fatal("regular source modification changed executable mode", err)
		}
	}
}
