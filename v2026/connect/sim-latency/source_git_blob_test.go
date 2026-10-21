// Remote-only pinned commits are read through private local bare fixtures;
// the product checkout's object database, refs and worktree remain unchanged.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Keeps publication separate from an intentionally unfetched control checkout.
type pinnedSourceBlobFixture struct {
	root          string
	publisherRoot string
	controlRoot   string
	branch        string
}

// All origins are bare repositories owned by this test's temporary directory.
func newPinnedSourceBlobFixture(t *testing.T, repository string) *pinnedSourceBlobFixture {
	t.Helper()
	root := t.TempDir()
	sourceTestRepository(t, root, repository)
	publisherRoot := filepath.Join(root, repository)
	branch := "sim-latency"
	if repository == "config" {
		branch = "main"
		sourceTestGit(t, publisherRoot, "branch", "-m", branch)
	}
	remoteRoot := filepath.Join(root, "origin.git")
	sourceTestGit(t, root, "init", "--quiet", "--bare", "--initial-branch="+branch, remoteRoot)
	sourceTestGit(t, publisherRoot, "remote", "add", "origin", remoteRoot)
	sourceTestGit(t, publisherRoot, "push", "--quiet", "origin", branch)
	controlRoot := filepath.Join(root, "control")
	sourceTestGit(t, root, "clone", "--quiet", "--no-hardlinks", remoteRoot, controlRoot)
	sourceTestGit(t, controlRoot, "branch", "-m", "synthetic-control-main")
	return &pinnedSourceBlobFixture{root: root, publisherRoot: publisherRoot, controlRoot: controlRoot, branch: branch}
}

// Publishes only into the fixture's local bare origin, never a configured repo.
func (self *pinnedSourceBlobFixture) publish(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(self.publisherRoot, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(self.publisherRoot, path), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, self.publisherRoot, "add", "--", path)
	sourceTestGit(t, self.publisherRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic published pin")
	sourceTestGit(t, self.publisherRoot, "push", "--quiet", "origin", self.branch)
	return sourceTestGit(t, self.publisherRoot, "rev-parse", "HEAD")
}

// A second promotion must not require pulling the first promotion into main.
// Auditing an older exact pin also must not read the remote's newer graph.
func TestSourceGraphAuditsRemotePinWithoutUpdatingControlCheckout(t *testing.T) {
	fixture := newPinnedSourceBlobFixture(t, "server")
	approvedModule := []byte("module module.example/server\n\ngo 1.23.0\n\nrequire module.example/connect v0.0.0\n\nreplace module.example/connect => ../connect\n")
	pin := fixture.publish(t, "go.mod", approvedModule)
	unlockedModule := []byte("module module.example/server\n\ngo 1.23.0\n\nrequire module.example/synthetic-new-dependency v0.0.0\n\nreplace module.example/synthetic-new-dependency => ../synthetic-new-dependency\n")
	newer := fixture.publish(t, "go.mod", unlockedModule)
	refsBefore := sourceTestGit(t, fixture.controlRoot, "for-each-ref", "--format=%(refname) %(objectname)")
	objectsBefore := sourceTestGit(t, fixture.controlRoot, "count-objects", "-v")
	headBefore := sourceTestGit(t, fixture.controlRoot, "rev-parse", "HEAD")
	moduleBefore, err := os.ReadFile(filepath.Join(fixture.controlRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, commit := range []string{pin, newer} {
		if _, err := sourceGitText(context.Background(), fixture.controlRoot, "cat-file", "-e", commit+"^{commit}"); err == nil {
			t.Fatal("fixture control checkout unexpectedly has a published commit")
		}
	}
	if err := verifyFrozenSourceGraph(context.Background(), fixture.controlRoot, pin); err != nil {
		t.Fatalf("remote-only reviewed pin could not be audited: %v", err)
	}
	if err := verifyFrozenSourceGraph(context.Background(), fixture.controlRoot, newer); err == nil || !strings.Contains(err.Error(), "synthetic-new-dependency is absent") {
		t.Fatalf("new remote pin bypassed complete graph coverage: %v", err)
	}
	for _, commit := range []string{pin, newer} {
		if _, err := sourceGitText(context.Background(), fixture.controlRoot, "cat-file", "-e", commit+"^{commit}"); err == nil {
			t.Fatal("private graph audit imported objects into the control checkout")
		}
	}
	if refs := sourceTestGit(t, fixture.controlRoot, "for-each-ref", "--format=%(refname) %(objectname)"); refs != refsBefore {
		t.Fatal("private graph audit changed control refs")
	}
	if objects := sourceTestGit(t, fixture.controlRoot, "count-objects", "-v"); objects != objectsBefore {
		t.Fatal("private graph audit changed the control object database")
	}
	if head := sourceTestGit(t, fixture.controlRoot, "rev-parse", "HEAD"); head != headBefore {
		t.Fatal("private graph audit changed the control head")
	}
	moduleAfter, err := os.ReadFile(filepath.Join(fixture.controlRoot, "go.mod"))
	if err != nil || !bytes.Equal(moduleAfter, moduleBefore) || sourceTestGit(t, fixture.controlRoot, "status", "--porcelain=v1") != "" {
		t.Fatal("private graph audit changed the control worktree")
	}
}

// Bootstrap may name an approved manifest absent from the local main checkout;
// byte identity includes trailing whitespace and does not follow newer main.
func TestPinnedSourceBlobReadsRemoteManifestExactly(t *testing.T) {
	fixture := newPinnedSourceBlobFixture(t, "config")
	manifest := []byte("schema: 2\nsynthetic_baseline: true\n\n")
	pin := fixture.publish(t, "main/sim-latency.yml", manifest)
	fixture.publish(t, "main/sim-latency.yml", []byte("schema: 2\nsynthetic_baseline: false\n"))
	refsBefore := sourceTestGit(t, fixture.controlRoot, "for-each-ref", "--format=%(refname) %(objectname)")
	scratch := filepath.Join(fixture.root, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	content, err := sourceGitBlobAtCommit(context.Background(), fixture.controlRoot, pin, "main/sim-latency.yml", maximumSourceConfigBytes)
	if err != nil || !bytes.Equal(content, manifest) {
		t.Fatalf("approved remote manifest changed: %q %v", content, err)
	}
	if _, err := sourceGitText(context.Background(), fixture.controlRoot, "cat-file", "-e", pin+"^{commit}"); err == nil {
		t.Fatal("manifest fetch wrote a control-checkout object")
	}
	if refs := sourceTestGit(t, fixture.controlRoot, "for-each-ref", "--format=%(refname) %(objectname)"); refs != refsBefore {
		t.Fatal("manifest fetch changed control refs")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private pinned repository survived the read: %v %v", entries, err)
	}
}

// An origin with a healthy head cannot substitute it for an unavailable pin.
func TestPinnedSourceBlobRejectsUnavailableCommit(t *testing.T) {
	fixture := newPinnedSourceBlobFixture(t, "server")
	refsBefore := sourceTestGit(t, fixture.controlRoot, "for-each-ref", "--format=%(refname) %(objectname)")
	if _, err := sourceGitBlobAtCommit(context.Background(), fixture.controlRoot, strings.Repeat("f", 40), "go.mod", maximumSourceConfigBytes); err == nil {
		t.Fatal("missing pinned commit was replaced by an origin head")
	}
	if refs := sourceTestGit(t, fixture.controlRoot, "for-each-ref", "--format=%(refname) %(objectname)"); refs != refsBefore {
		t.Fatal("failed pinned fetch changed control refs")
	}
}

// Cancellation remains inspectable through the bounded Git error boundary.
func TestPinnedSourceBlobPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sourceGitBlobAtCommit(ctx, t.TempDir(), strings.Repeat("f", 40), "go.mod", maximumSourceConfigBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pinned Git read lost cancellation identity: %v", err)
	}
}
