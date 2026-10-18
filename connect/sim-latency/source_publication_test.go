// Publication tests force partial pushes, lost receipts and drift without
// contacting a remote, deploying code, or depending on scheduler timing.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
)

// Maintains fake remote heads independently of receipt persistence.
type sourcePublicationFixture struct {
	heads        map[string]string
	pushNames    []string
	receiptNames []string
	failPush     string
	failReceipt  string
	deny         bool
}

// Binds the fixture to a complete exact-ref intent.
func newSourcePublicationFixture() (*sourcePublicationIntent, *sourcePublicationFixture) {
	intent := &sourcePublicationIntent{Schema: 2, ActivationGeneration: 2, SourceDecisionDigest: strings.Repeat("c", 64), ManifestSha256: strings.Repeat("d", 64), ApprovedPatchSha256: strings.Repeat("e", 64), ApprovedPatchPath: "/synthetic/approved.patch"}
	fixture := &sourcePublicationFixture{heads: map[string]string{}}
	for _, name := range append(sourceRepositoryNames(), "config") {
		branch := "sim-latency"
		if name == "config" {
			branch = "main"
		}
		commit := strings.Repeat("a", 40)
		next := commit
		if name == "server" || name == "config" {
			next = strings.Repeat("b", 40)
		}
		intent.Refs = append(intent.Refs, sourcePublicationRef{Repository: name, Branch: branch, ExpectedCommit: commit, Commit: next, StagedRoot: "/synthetic/" + name})
		fixture.heads[name] = commit
	}
	return intent, fixture
}

// Models a separately revocable durable source approval.
func (self *sourcePublicationFixture) Authorize(context.Context, *sourcePublicationIntent) error {
	if self.deny {
		return errors.New("source approval revoked")
	}
	return nil
}

// Returns independently observable remote state.
func (self *sourcePublicationFixture) Head(_ context.Context, ref sourcePublicationRef) (string, error) {
	return self.heads[ref.Repository], nil
}

// Content authentication is exercised separately using real isolated Git.
func (self *sourcePublicationFixture) Verify(context.Context, sourcePublicationRef, *sourcePublicationIntent) error {
	return nil
}

// Injects failure before a particular external write.
func (self *sourcePublicationFixture) Push(_ context.Context, ref sourcePublicationRef) error {
	if self.failPush == ref.Repository {
		return errors.New("synthetic push failure")
	}
	self.heads[ref.Repository] = ref.Commit
	self.pushNames = append(self.pushNames, ref.Repository)
	return nil
}

// Injects a lost acknowledgment after a real successful write.
func (self *sourcePublicationFixture) Receipt(_ context.Context, ref sourcePublicationRef, _ *sourcePublicationIntent) error {
	if self.failReceipt == ref.Repository {
		return errors.New("synthetic lost receipt")
	}
	self.receiptNames = append(self.receiptNames, ref.Repository)
	return nil
}

// A failed config push leaves staged source inactive; retry never regenerates
// or republishes the already successful server commit.
func TestSourcePublicationResumesPartialPush(t *testing.T) {
	intent, fixture := newSourcePublicationFixture()
	fixture.failPush = "config"
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err == nil {
		t.Fatal("config push failure disappeared")
	}
	if len(fixture.pushNames) != 1 || fixture.pushNames[0] != "server" {
		t.Fatalf("unexpected partial publication %v", fixture.pushNames)
	}
	fixture.failPush = ""
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fixture.pushNames, ",") != "server,config" {
		t.Fatalf("retry changed source or config ordering: %v", fixture.pushNames)
	}
}

// Fresh-parent selection must never be used to rescue a partially published
// intent. Even an unrelated config advance leaves its original refs fixed.
func TestSourcePublicationDoesNotRebaseConfigAfterPartialPush(t *testing.T) {
	intent, fixture := newSourcePublicationFixture()
	original, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	fixture.failPush = "config"
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err == nil {
		t.Fatal("config push failure disappeared")
	}
	fixture.failPush = ""
	fixture.heads["config"] = strings.Repeat("f", 40)
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err == nil {
		t.Fatal("config drift after partial publication accepted")
	}
	if strings.Join(fixture.pushNames, ",") != "server" || fixture.heads["config"] != strings.Repeat("f", 40) {
		t.Fatal("partial retry rewrote a remote ref")
	}
	retried, err := json.Marshal(intent)
	if err != nil || string(retried) != string(original) {
		t.Fatal("partial retry changed the immutable expected/new ref plan")
	}
}

// No-op probes may authorize a safe carry; a lost server/config push reply is
// still publication and cannot be mistaken for an unpublished failed attempt.
func TestSourceFallbackRequiresEveryRefUnpublished(t *testing.T) {
	intent, backend := newSourcePublicationFixture()
	proof, err := probeUnpublishedSource(context.Background(), intent, backend)
	if err != nil || len(proof) == 0 || len(backend.pushNames) != 0 {
		t.Fatalf("read-only unpublished proof: %s %v", proof, err)
	}
	for _, repository := range []string{"server", "config", "connect"} {
		expected := backend.heads[repository]
		backend.heads[repository] = strings.Repeat("b", 40)
		if _, err := probeUnpublishedSource(context.Background(), intent, backend); err == nil {
			t.Fatalf("published/drifting %s permitted fallback", repository)
		}
		backend.heads[repository] = expected
	}
}

// Lost acknowledgments do not result in a second push or second commit.
func TestSourcePublicationReconcilesLostReceipt(t *testing.T) {
	intent, fixture := newSourcePublicationFixture()
	fixture.failReceipt = "server"
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err == nil {
		t.Fatal("lost receipt was ignored")
	}
	fixture.failReceipt = ""
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fixture.pushNames, ",") != "server,config" {
		t.Fatalf("lost receipt duplicated publication: %v", fixture.pushNames)
	}
}

// A changed ref or newly revoked approval prevents all subsequent effects.
func TestSourcePublicationRejectsDriftAndRevocation(t *testing.T) {
	intent, fixture := newSourcePublicationFixture()
	fixture.heads["server"] = strings.Repeat("f", 40)
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err == nil {
		t.Fatal("remote drift accepted")
	}
	if len(fixture.pushNames) != 0 {
		t.Fatal("publication continued after ref drift")
	}
	fixture.heads["server"] = intent.Refs[0].ExpectedCommit
	fixture.deny = true
	if err := reconcileSourcePublication(context.Background(), intent, fixture); err == nil {
		t.Fatal("revoked source approval accepted")
	}
	if len(fixture.pushNames) != 0 {
		t.Fatal("publication continued after revocation")
	}
}

// Authenticated blobs include every trailing byte. Whitespace normalization
// previously made two different immutable Git blobs appear to have one digest.
func TestSourcePublicationConfigDigestBindsExactGitBlob(t *testing.T) {
	root := t.TempDir()
	base := sourceTestRepository(t, root, "config")
	clone := filepath.Join(root, "config")
	if err := os.Chmod(clone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(clone, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := append(sourceConfigTestManifestBytes(t, false, false), '\n')
	if err := os.WriteFile(filepath.Join(clone, "main", "sim-latency.yml"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, clone, "add", "main/sim-latency.yml")
	sourceTestGit(t, clone, "commit", "--quiet", "-m", "manifest")
	commit := sourceTestGit(t, clone, "rev-parse", "HEAD")
	ref := sourcePublicationRef{Repository: "config", Branch: "main", ExpectedCommit: base, Commit: commit, StagedRoot: clone}
	digest := sha256.Sum256(content)
	intent := &sourcePublicationIntent{ManifestSha256: hex.EncodeToString(digest[:])}
	backend := &gitSourcePublicationBackend{}
	if err := backend.Verify(context.Background(), ref, intent); err != nil {
		t.Fatal(err)
	}
	trimmed := sha256.Sum256(content[:len(content)-1])
	intent.ManifestSha256 = hex.EncodeToString(trimmed[:])
	if err := backend.Verify(context.Background(), ref, intent); err == nil {
		t.Fatal("normalized blob digest concealed changed trailing bytes")
	}
	if err := os.Chmod(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	intent.ManifestSha256 = hex.EncodeToString(digest[:])
	if err := backend.Verify(context.Background(), ref, intent); err == nil {
		t.Fatal("nonprivate clone accepted")
	}
}

// Two patches can serialize different context yet produce the same tree.
// Source authority binds the submitted bytes, then verifies their applied tree;
// an additional allowed-file edit cannot hide behind an unchanged runner tree.
func TestSourcePublicationVerifiesExactApprovedPatchTree(t *testing.T) {
	root := t.TempDir()
	sourceTestRepository(t, root, "server")
	repository := filepath.Join(root, "server")
	if err := os.Chmod(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repository, "connect", "synthetic.go")
	if err := os.WriteFile(path, []byte("package connect\n\nvar synthetic = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/synthetic.go")
	sourceTestGit(t, repository, "commit", "--quiet", "-m", "synthetic base")
	base := sourceTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("package connect\n\nvar synthetic = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/connect/synthetic.go b/connect/synthetic.go\n--- a/connect/synthetic.go\n+++ b/connect/synthetic.go\n@@ -1,3 +1,3 @@\n package connect\n \n-var synthetic = 1\n+var synthetic = 2\n"
	patchPath := filepath.Join(root, "approved.patch")
	if err := os.WriteFile(patchPath, []byte(patch), 0o400); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/synthetic.go")
	sourceTestGit(t, repository, "commit", "--quiet", "-m", "approved source")
	commit := sourceTestGit(t, repository, "rev-parse", "HEAD")
	digest := sha256.Sum256([]byte(patch))
	intent := &sourcePublicationIntent{ApprovedPatchPath: patchPath, ApprovedPatchSha256: hex.EncodeToString(digest[:])}
	ref := sourcePublicationRef{Repository: "server", Branch: "sim-latency", ExpectedCommit: base, Commit: commit, StagedRoot: repository}
	backend := &gitSourcePublicationBackend{patchPolicy: controller.PatchPolicy{MaxPatchBytes: 4096, AllowedPaths: []string{"connect/*.go"}, ForbiddenPaths: []string{"connect/sim-latency/**"}}}
	if err := backend.Verify(context.Background(), ref, intent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package connect\n\nvar synthetic = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "add", "connect/synthetic.go")
	sourceTestGit(t, repository, "commit", "--amend", "--quiet", "--no-edit")
	ref.Commit = sourceTestGit(t, repository, "rev-parse", "HEAD")
	if err := backend.Verify(context.Background(), ref, intent); err == nil {
		t.Fatal("unreviewed changed source tree accepted")
	}
}
