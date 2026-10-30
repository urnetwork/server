// Private local repositories exercise the same immutable mode checks used
// before reviewed staging and publication replay, without executing scripts.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
)

// Executable text remains regular source. Omitting an index mode cannot turn
// an existing symlink into an approved regular-file modification.
func TestReviewedSourceChecksRealPinnedModesForStagingAndPublication(t *testing.T) {
	for _, fixture := range []struct{ executable, symlink bool }{
		{}, {executable: true}, {symlink: true},
	} {
		root := t.TempDir()
		sourceTestRepository(t, root, "connect")
		repository := filepath.Join(root, "connect")
		if err := os.Chmod(repository, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(repository, "script.sh")
		mode := os.FileMode(0o644)
		if fixture.executable {
			mode = 0o755
		}
		body := "-original\n+changed\n"
		if fixture.symlink {
			if err := os.Symlink("old-target.txt", target); err != nil {
				t.Fatal(err)
			}
			body = "-old-target.txt\n\\ No newline at end of file\n+new-target.txt\n\\ No newline at end of file\n"
		} else if err := os.WriteFile(target, []byte("original\n"), mode); err != nil {
			t.Fatal(err)
		}
		sourceTestGit(t, repository, "add", "--", "script.sh")
		sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic pinned mode")
		base := sourceTestGit(t, repository, "rev-parse", "HEAD")
		raw := "diff --git a/connect/script.sh b/connect/script.sh\n--- a/connect/script.sh\n+++ b/connect/script.sh\n@@ -1 +1 @@\n" + body
		policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096, AllowedPaths: []string{"connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
		canonical, patchError := controller.ValidateAndCanonicalizePatch(raw, policy)
		if patchError != nil {
			t.Fatal(patchError)
		}
		err := validateSourcePatchBase(context.Background(), repository, base, "connect", canonical, policy)
		if (err == nil) == fixture.symlink {
			t.Fatalf("staging executable=%t symlink=%t: %v", fixture.executable, fixture.symlink, err)
		}
		if fixture.symlink {
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("new-target.txt", target); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(target, []byte("changed\n"), mode); err != nil {
			t.Fatal(err)
		}
		sourceTestGit(t, repository, "add", "--", "script.sh")
		sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic staged change")
		candidate := sourceTestGit(t, repository, "rev-parse", "HEAD")
		patchPath := filepath.Join(root, "approved.patch")
		if err := os.WriteFile(patchPath, canonical.Bytes, 0o400); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(canonical.Bytes)
		intent := &sourcePublicationIntent{CandidatePathBase: policy.PathBase, ApprovedPatchPath: patchPath, ApprovedPatchSha256: hex.EncodeToString(digest[:])}
		ref := sourcePublicationRef{Repository: "connect", Branch: "sim-latency", ExpectedCommit: base, Commit: candidate, StagedRoot: repository}
		backend := &gitSourcePublicationBackend{patchPolicy: policy}
		err = backend.Verify(context.Background(), ref, intent)
		if (err == nil) == fixture.symlink {
			t.Fatalf("publication executable=%t symlink=%t: %v", fixture.executable, fixture.symlink, err)
		}
		if fixture.symlink && !strings.Contains(err.Error(), "pinned regular source blob") {
			t.Fatal("publication refusal came from an unrelated later replay failure", err)
		}
	}
}
