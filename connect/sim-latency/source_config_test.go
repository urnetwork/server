// Fresh-config regressions use only private local bare remotes. They exercise
// the real staging path without pushing to any configured or live repository.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Retains a published manifest while later commits independently advance main.
type sourceConfigFixture struct {
	root           string
	repositoryRoot string
	remoteRoot     string
	current        *model.CompetitionSourceActivation
}

// A different remote default branch makes main an explicit fetch requirement.
func newSourceConfigFixture(t *testing.T) *sourceConfigFixture {
	t.Helper()
	return newSourceConfigFixtureForMode(t, false)
}

// Separate paths exercise staging without reinterpreting production history.
func newSourceConfigFixtureForMode(t *testing.T, staging bool) *sourceConfigFixture {
	t.Helper()
	root := t.TempDir()
	sourceTestRepository(t, root, "config")
	repositoryRoot := filepath.Join(root, "config")
	sourceTestGit(t, repositoryRoot, "branch", "-m", "main")
	if err := os.Mkdir(filepath.Join(repositoryRoot, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := sourceConfigTestManifestBytes(t, staging, false)
	if err := os.WriteFile(filepath.Join(repositoryRoot, filepath.FromSlash(sourceManifestRepositoryPath(staging))), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if staging {
		if err := os.WriteFile(filepath.Join(repositoryRoot, "main", "sim-latency.yml"), []byte("synthetic_historical_production: unchanged\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repositoryRoot, "main", "unrelated.yml"), []byte("synthetic: previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repositoryRoot, "add", "--all")
	sourceTestGit(t, repositoryRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic approved manifest")
	manifestCommit := sourceTestGit(t, repositoryRoot, "rev-parse", "HEAD")
	remoteRoot := filepath.Join(root, "remote.git")
	sourceTestGit(t, root, "init", "--quiet", "--bare", "--initial-branch=synthetic-default", remoteRoot)
	sourceTestGit(t, repositoryRoot, "remote", "add", "origin", remoteRoot)
	sourceTestGit(t, repositoryRoot, "branch", "synthetic-default")
	sourceTestGit(t, repositoryRoot, "push", "--quiet", "origin", "main", "synthetic-default")
	digest := sha256.Sum256(manifest)
	return &sourceConfigFixture{
		root: root, repositoryRoot: repositoryRoot, remoteRoot: remoteRoot,
		current: &model.CompetitionSourceActivation{
			Staging:        staging,
			ManifestCommit: manifestCommit, ManifestSha256: hex.EncodeToString(digest[:]), Manifest: manifest,
			Commits: map[string]string{"server": strings.Repeat("a", 40)},
		},
	}
}

// Keeps synthetic config-byte regressions valid under the full ledger parser.
func sourceConfigTestManifestBytes(t *testing.T, staging, successor bool) []byte {
	t.Helper()
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	if staging {
		manifest = stagingSourceTestManifest(t)
	}
	if successor {
		round := manifest.EvaluationSource.Rounds[0]
		round.Epoch, round.ActivationGeneration = 2, 2
		round.Reason = "synthetic_carry_forward"
		manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, round)
	}
	content, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Only the fixture's private bare remote receives this synthetic main commit.
func (self *sourceConfigFixture) advance(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(self.repositoryRoot, path), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, self.repositoryRoot, "add", "--", path)
	sourceTestGit(t, self.repositoryRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic config advance")
	sourceTestGit(t, self.repositoryRoot, "push", "--quiet", "origin", "main")
	return sourceTestGit(t, self.repositoryRoot, "rev-parse", "HEAD")
}

// An unrelated main commit used to make every successor start on a stale
// parent. The real staging path must retain it without changing source pins.
func TestFreshSourceConfigPreservesUnrelatedMainAdvance(t *testing.T) {
	fixture := newSourceConfigFixture(t)
	unrelated := []byte("synthetic: updated\n\n")
	remoteHead := fixture.advance(t, "main/unrelated.yml", unrelated)
	stagedRoot := filepath.Join(fixture.root, "staged")
	nextManifest := sourceConfigTestManifestBytes(t, false, true)
	parent, commit, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, stagedRoot, fixture.current, nextManifest, "synthetic successor")
	if err != nil {
		t.Fatal(err)
	}
	if parent != remoteHead || parent == fixture.current.ManifestCommit {
		t.Fatalf("fresh parent = %s, want advanced main %s", parent, remoteHead)
	}
	if parents := sourceTestGit(t, stagedRoot, "rev-list", "--parents", "-n", "1", commit); parents != commit+" "+parent {
		t.Fatalf("successor did not preserve the exact fetched parent: %s", parents)
	}
	content, err := sourceGitBytes(context.Background(), stagedRoot, maximumSourceConfigBytes, "show", commit+":main/unrelated.yml")
	if err != nil || !bytes.Equal(content, unrelated) {
		t.Fatalf("unrelated config changed: %q, %v", content, err)
	}
	digest := sha256.Sum256(nextManifest)
	ref := sourcePublicationRef{Repository: "config", Branch: "main", ExpectedCommit: parent, Commit: commit, StagedRoot: stagedRoot}
	publication := &sourcePublicationIntent{ManifestSha256: hex.EncodeToString(digest[:])}
	if err := (&gitSourcePublicationBackend{}).Verify(context.Background(), ref, publication); err != nil {
		t.Fatal(err)
	}
	if head := sourceTestGit(t, fixture.remoteRoot, "rev-parse", "refs/heads/main"); head != remoteHead {
		t.Fatal("private staging changed the remote ref")
	}
	if len(fixture.current.Commits) != 1 || fixture.current.Commits["server"] != strings.Repeat("a", 40) {
		t.Fatal("config parent selection changed evaluated source pins")
	}
}

// Even semantically equivalent whitespace changes are unauthenticated manifest
// drift and cannot be hidden by preserving other config files.
func TestFreshSourceConfigRejectsManifestByteDrift(t *testing.T) {
	fixture := newSourceConfigFixture(t)
	drifted := append(append([]byte(nil), fixture.current.Manifest...), '\n')
	remoteHead := fixture.advance(t, "main/sim-latency.yml", drifted)
	parent, commit, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, filepath.Join(fixture.root, "staged"), fixture.current, sourceConfigTestManifestBytes(t, false, true), "synthetic successor")
	if err == nil || !strings.Contains(err.Error(), "differs from the active immutable manifest") || parent != "" || commit != "" {
		t.Fatalf("changed manifest was not refused: %s %s %v", parent, commit, err)
	}
	if head := sourceTestGit(t, fixture.remoteRoot, "rev-parse", "refs/heads/main"); head != remoteHead {
		t.Fatal("rejected staging changed the remote ref")
	}
}

// Equal Git blob bytes do not authorize a symlink checkout: staging must never
// follow a substituted manifest path when writing the next exact manifest.
func TestFreshSourceConfigRejectsManifestSymlink(t *testing.T) {
	fixture := newSourceConfigFixture(t)
	manifestPath := filepath.Join(fixture.repositoryRoot, "main", "sim-latency.yml")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(string(fixture.current.Manifest), manifestPath); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, fixture.repositoryRoot, "add", "--", "main/sim-latency.yml")
	sourceTestGit(t, fixture.repositoryRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic substituted manifest path")
	sourceTestGit(t, fixture.repositoryRoot, "push", "--quiet", "origin", "main")
	parent, commit, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, filepath.Join(fixture.root, "staged"), fixture.current, sourceConfigTestManifestBytes(t, false, true), "synthetic successor")
	if err == nil || !strings.Contains(err.Error(), "must remain a regular file") || parent != "" || commit != "" {
		t.Fatalf("manifest symlink was not refused: %s %s %v", parent, commit, err)
	}
}

// Matching bytes alone are insufficient when main has abandoned the active
// manifest history. The approved commit remains fetchable on another branch.
func TestFreshSourceConfigRejectsNonancestorMain(t *testing.T) {
	fixture := newSourceConfigFixture(t)
	sourceTestGit(t, fixture.repositoryRoot, "checkout", "--quiet", "--orphan", "synthetic-unrelated")
	sourceTestGit(t, fixture.repositoryRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic unrelated history")
	unrelated := sourceTestGit(t, fixture.repositoryRoot, "rev-parse", "HEAD")
	sourceTestGit(t, fixture.repositoryRoot, "push", "--quiet", "origin", "synthetic-unrelated")
	// This compare-and-swap changes only the fixture's local bare repository.
	sourceTestGit(t, fixture.remoteRoot, "update-ref", "refs/heads/main", unrelated, fixture.current.ManifestCommit)
	content, err := sourceGitBytes(context.Background(), fixture.remoteRoot, maximumSourceConfigBytes, "show", unrelated+":main/sim-latency.yml")
	if err != nil || !bytes.Equal(content, fixture.current.Manifest) {
		t.Fatalf("nonancestor fixture must retain exact manifest bytes: %q %v", content, err)
	}
	parent, commit, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, filepath.Join(fixture.root, "staged"), fixture.current, sourceConfigTestManifestBytes(t, false, true), "synthetic successor")
	if err == nil || !strings.Contains(err.Error(), "no longer descends") || parent != "" || commit != "" {
		t.Fatalf("unrelated history was not refused: %s %s %v", parent, commit, err)
	}
}

// A recorded private staging path is never reinterpreted as a fresh choice,
// even if a later remote advance would otherwise be safe for a new intent.
func TestFreshSourceConfigCannotRebaseExistingStaging(t *testing.T) {
	fixture := newSourceConfigFixture(t)
	stagedRoot := filepath.Join(fixture.root, "staged")
	nextManifest := sourceConfigTestManifestBytes(t, false, true)
	parent, commit, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, stagedRoot, fixture.current, nextManifest, "synthetic successor")
	if err != nil {
		t.Fatal(err)
	}
	fixture.advance(t, "main/unrelated.yml", []byte("synthetic: later\n"))
	if _, _, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, stagedRoot, fixture.current, nextManifest, "synthetic retry"); err == nil {
		t.Fatal("existing config staging was rebased")
	}
	if head := sourceTestGit(t, stagedRoot, "rev-parse", "HEAD"); head != commit {
		t.Fatal("failed restaging changed the recorded successor")
	}
	if parents := sourceTestGit(t, stagedRoot, "rev-list", "--parents", "-n", "1", commit); parents != commit+" "+parent {
		t.Fatal("failed restaging changed the recorded parent")
	}
}

// The expected digest authenticates current manifest bytes before any fetch;
// caller-provided bytes alone cannot replace the active predecessor evidence.
func TestFreshSourceConfigRejectsCorruptManifestEvidence(t *testing.T) {
	fixture := newSourceConfigFixture(t)
	fixture.current.ManifestSha256 = strings.Repeat("0", 64)
	stagedRoot := filepath.Join(fixture.root, "staged")
	if _, _, err := stageFreshSourceConfig(context.Background(), fixture.remoteRoot, stagedRoot, fixture.current, sourceConfigTestManifestBytes(t, false, true), "synthetic successor"); err == nil {
		t.Fatal("corrupt active manifest evidence accepted")
	}
	if _, err := os.Lstat(stagedRoot); !os.IsNotExist(err) {
		t.Fatalf("invalid predecessor reached config staging: %v", err)
	}
}
