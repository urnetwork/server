// These regressions force confined relative-link substitutions at the actual
// filesystem consumers. Every object is private test data, never host state.
package competitionagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A confined alias is not an exact signed file even when its bytes match.
func TestWriteFilesRejectsConfinedRelativeLeafAlias(t *testing.T) {
	root := t.TempDir()
	content := []byte("synthetic retained evidence")
	if err := os.WriteFile(filepath.Join(root, "original.json"), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.json", filepath.Join(root, "alias.json")); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	files := []File{{Path: "alias.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}}
	var output bytes.Buffer
	if err := WriteFiles(&output, root, files, DefaultLimits().ArtifactBytes, DefaultLimits()); err == nil || output.Len() != 0 {
		t.Fatal("relative leaf alias acquired authenticated transfer authority", err)
	}
}

// Input authentication and terminal hashing use the same exact-leaf boundary.
func TestAgentFileDigestRejectsConfinedRelativeLeafAlias(t *testing.T) {
	fixture := newAgentTestFixture(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "original.json"), []byte("synthetic input"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.json", filepath.Join(root, "alias.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.agent.fileDigest(root, "alias.json"); err == nil {
		t.Fatal("relative leaf alias acquired committed input authority")
	}
}

// Refusal must happen before consuming input, not after a partial private write.
type noFollowUnreadInput struct{ read bool }

// An unexpected read is counted and ends immediately, without timing dependence.
func (self *noFollowUnreadInput) Read([]byte) (int, error) {
	self.read = true
	return 0, io.ErrUnexpectedEOF
}

// A read-boundary hook forces an authority change between two received files,
// after the initially empty destination was already positively checked.
type noFollowTransferReader struct {
	reader *bytes.Reader
	before func()
}

// The callback occurs exactly once, without a goroutine or timing assumption.
func (self *noFollowTransferReader) Read(content []byte) (int, error) {
	if self.before != nil {
		callback := self.before
		self.before = nil
		callback()
	}
	return self.reader.Read(content)
}

// A pre-existing relative alias cannot cause mkdir inside another retained
// subtree before the later ancestor check rejects its signed target path.
func TestReadFilesRejectsRelativeAncestorBeforeAnyDirectoryCreation(t *testing.T) {
	root := privateProtocolTestDirectory(t)
	kept := filepath.Join(root, "kept")
	if err := os.Mkdir(kept, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("kept", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic"))
	files := []File{{Path: "alias/created/report.json", Sha256: hex.EncodeToString(digest[:]), Bytes: 9}}
	input := &noFollowUnreadInput{}
	if err := ReadFiles(input, root, files, DefaultLimits().ArtifactBytes, DefaultLimits()); err == nil || input.read {
		t.Fatal("reused aliased root reached a transfer body", err)
	}
	entries, err := os.ReadDir(kept)
	if err != nil || len(entries) != 0 {
		t.Fatal("relative alias caused a directory mutation before refusal", entries, err)
	}
}

// Empty-at-entry does not authorize a later alias planted before another file's
// parent creation. The actual receiver must refuse before a deeper mkdir.
func TestReadFilesRejectsRelativeAncestorInsertedDuringTransfer(t *testing.T) {
	root := privateProtocolTestDirectory(t)
	kept := filepath.Join(root, "kept")
	first, second := []byte("x"), []byte("synthetic")
	firstDigest, secondDigest := sha256.Sum256(first), sha256.Sum256(second)
	files := []File{{Path: "first.txt", Sha256: hex.EncodeToString(firstDigest[:]), Bytes: int64(len(first))},
		{Path: "alias/created/report.json", Sha256: hex.EncodeToString(secondDigest[:]), Bytes: int64(len(second))}}
	inserted := false
	reader := &noFollowTransferReader{reader: bytes.NewReader(append(first, second...)), before: func() {
		if err := os.Mkdir(kept, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("kept", filepath.Join(root, "alias")); err != nil {
			t.Fatal(err)
		}
		inserted = true
	}}
	if err := ReadFiles(reader, root, files, DefaultLimits().ArtifactBytes, DefaultLimits()); err == nil || !inserted {
		t.Fatal("inter-file alias was accepted or not exercised", err)
	}
	entries, err := os.ReadDir(kept)
	if err != nil || len(entries) != 0 {
		t.Fatal("inter-file relative alias caused a directory mutation before refusal", entries, err)
	}
}

// Fresh means empty as well as private; unrelated evidence is not ignored.
func TestReadFilesRejectsNonemptyRootBeforeBody(t *testing.T) {
	root := privateProtocolTestDirectory(t)
	if err := os.WriteFile(filepath.Join(root, "retained.json"), []byte("synthetic retained"), 0400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic"))
	input := &noFollowUnreadInput{}
	if err := ReadFiles(input, root, []File{{Path: "new.json", Sha256: hex.EncodeToString(digest[:]), Bytes: 9}}, DefaultLimits().ArtifactBytes, DefaultLimits()); err == nil || input.read {
		t.Fatal("nonempty root accepted another transfer", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "new.json")); !os.IsNotExist(err) {
		t.Fatal("reused root was modified before refusal", err)
	}
}

// The callback forces the race after a directory was discovered as ordinary
// but before its open. An empty alias target used to hide the changed inode.
func TestAgentArtifactInventoryRejectsDiscoveredDirectoryReplacement(t *testing.T) {
	fixture := newAgentTestFixture(t)
	root := t.TempDir()
	checked, target := filepath.Join(root, "checked"), filepath.Join(root, "target")
	for _, path := range []string{checked, target} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(checked, "original.json"), []byte("synthetic original"), 0400); err != nil {
		t.Fatal(err)
	}
	swapped := false
	fixture.agent.beforeDirectoryOpen = func(relative string) {
		if relative != "checked" {
			return
		}
		if err := os.Rename(checked, checked+"-captured"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", checked); err != nil {
			t.Fatal(err)
		}
		swapped = true
	}
	if _, err := fixture.agent.collectFiles(root); err == nil || !swapped {
		t.Fatal("directory generation substitution was accepted or not exercised", err)
	}
}

// Even a confined property alias cannot receive a privileged freeze write.
func TestHostCleanupCgroupFreezeRejectsConfinedRelativePropertyAlias(t *testing.T) {
	fixture := newHostCleanupTest(t)
	name := fixture.plan.CgroupNames[1]
	relative, err := cleanupSlicePath(name)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(directory, "kept")
	if err := os.WriteFile(kept, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("kept", filepath.Join(directory, "cgroup.freeze")); err != nil {
		t.Fatal(err)
	}
	groups := &hostCleanupCgroups{rootPath: root, maximumBytes: 128}
	if err := groups.Freeze(context.Background(), name); err == nil {
		t.Fatal("relative property alias received freeze authority")
	}
	content, err := os.ReadFile(kept)
	if err != nil || string(content) != "0\n" {
		t.Fatal("relative property target was mutated", err)
	}
}

// Read-only counters require the actual named property, not an aliased valid
// document that could falsely acknowledge emptiness or frozen completion.
func TestHostCleanupCgroupPopulationRejectsConfinedRelativePropertyAlias(t *testing.T) {
	fixture := newHostCleanupTest(t)
	name := fixture.plan.CgroupNames[1]
	relative, err := cleanupSlicePath(name)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "kept"), []byte("populated 0\nfrozen 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("kept", filepath.Join(directory, "cgroup.events")); err != nil {
		t.Fatal(err)
	}
	groups := &hostCleanupCgroups{rootPath: root, maximumBytes: 128}
	if empty, err := groups.Empty(context.Background(), name); err == nil || empty {
		t.Fatal("relative property alias falsely acknowledged cleanup", empty, err)
	}
}

// Parent capture must reject a relative alias inserted after inspection; a
// second directory on the same device is not the authenticated run parent.
func TestHostCleanupMountParentRejectsRelativeReplacementAfterInspection(t *testing.T) {
	fixture := newHostCleanupTest(t)
	root := t.TempDir()
	config := Config{WorkDirectory: root}
	parent := filepath.Join(root, fixture.plan.RunId)
	target := parent + "-target"
	for _, path := range []string{parent, target} {
		if err := os.MkdirAll(filepath.Join(path, ".evidence-runtime"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.plan.WorkDeviceId = uint64(info.Sys().(*syscall.Stat_t).Dev)
	fixture.plan.EvidenceDirectory = filepath.Join(parent, ".evidence-runtime")
	if err := validateCleanupPlan(fixture.plan, config); err != nil {
		t.Fatal("synthetic parent fixture is not reachable through cleanup admission", err)
	}
	mounts := newHostCleanupMounts(config, DefaultAgentSettings())
	swapped := false
	mounts.beforeParentOpen = func(cleanupPlan) {
		if err := os.Rename(parent, parent+"-captured"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Base(target), parent); err != nil {
			t.Fatal(err)
		}
		swapped = true
	}
	opened, err := mounts.openParent(fixture.plan)
	if opened != nil {
		_ = opened.Close()
	}
	if err == nil || !swapped {
		t.Fatal("replaced evidence parent was accepted or not exercised", err)
	}
}

// Direct snapshot calls exercise only private files, never a CLI or daemon.
func noFollowSnapshotDestination(t *testing.T, fixture *dockerProxyTest) string {
	t.Helper()
	if err := os.MkdirAll(fixture.proxy.buildDirectory(), 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(fixture.proxy.buildDirectory(), "synthetic-snapshot")
}

// Legitimate relative source symlinks remain links in the sealed build input.
func TestDockerProxyContextPreservesDeclaredRelativeSourceLinks(t *testing.T) {
	fixture := newDockerProxyTest(t)
	if err := os.Symlink("canonical.patch", filepath.Join(fixture.buildRequest.ContextDirectory, "declared-link")); err != nil {
		t.Fatal(err)
	}
	destination := noFollowSnapshotDestination(t, fixture)
	digest, size, err := fixture.proxy.snapshotContext(context.Background(), fixture.buildRequest.ContextDirectory, destination)
	if err != nil || len(digest) != 64 || size <= 0 {
		t.Fatal("declared safe source symlink was rejected", digest, size, err)
	}
	info, err := os.Lstat(filepath.Join(destination, "declared-link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("declared link was dereferenced into file bytes", err)
	}
	link, err := os.Readlink(filepath.Join(destination, "declared-link"))
	if err != nil || link != "canonical.patch" {
		t.Fatal("declared source link target changed", link, err)
	}
}

// Equal length and timestamp deliberately defeat the previous byte-count/time
// checks: only an exact descriptor open may close this leaf substitution.
func TestDockerProxyContextRejectsRelativeLeafSubstitutionAfterInspection(t *testing.T) {
	for _, replacement := range []string{"relative-link", "regular-inode"} {
		fixture := newDockerProxyTest(t)
		source := fixture.buildRequest.ContextDirectory
		checked := filepath.Join(source, "z-checked")
		stamp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		for name, content := range map[string]string{"z-checked": "aaa", "a-target": "bbb"} {
			path := filepath.Join(source, name)
			if err := os.WriteFile(path, []byte(content), 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
		swapped := false
		fixture.proxy.beforeContextOpen = func(role, relative string) {
			if role != "file" || relative != "z-checked" {
				return
			}
			if err := os.Rename(checked, checked+"-captured"); err != nil {
				t.Fatal(err)
			}
			var err error
			if replacement == "relative-link" {
				err = os.Symlink("a-target", checked)
			} else {
				err = os.Rename(filepath.Join(source, "a-target"), checked)
			}
			if err != nil {
				t.Fatal(err)
			}
			swapped = true
		}
		if _, _, err := fixture.proxy.snapshotContext(context.Background(), source, noFollowSnapshotDestination(t, fixture)); err == nil || !swapped {
			t.Fatal("leaf substitution was accepted or not exercised", replacement, err)
		}
	}
}

// The directory race is forced after its original stat, not by scheduler luck.
func TestDockerProxyContextRejectsRelativeDirectorySubstitutionAfterInspection(t *testing.T) {
	for _, replacement := range []string{"relative-link", "regular-inode"} {
		fixture := newDockerProxyTest(t)
		source := fixture.buildRequest.ContextDirectory
		checked := filepath.Join(source, "checked-directory")
		for _, path := range []string{checked, filepath.Join(source, "target-directory")} {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		swapped := false
		fixture.proxy.beforeContextOpen = func(role, relative string) {
			if role != "directory" || relative != "checked-directory" {
				return
			}
			if err := os.Rename(checked, checked+"-captured"); err != nil {
				t.Fatal(err)
			}
			var err error
			if replacement == "relative-link" {
				err = os.Symlink("target-directory", checked)
			} else {
				err = os.Rename(filepath.Join(source, "target-directory"), checked)
				if err == nil {
					err = os.Mkdir(filepath.Join(source, "target-directory"), 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			swapped = true
		}
		if _, _, err := fixture.proxy.snapshotContext(context.Background(), source, noFollowSnapshotDestination(t, fixture)); err == nil || !swapped {
			t.Fatal("directory substitution was accepted or not exercised", replacement, err)
		}
	}
}

// A root selected beneath the trusted workspace is not permission to follow a
// just-substituted alias to a different sealed-source directory.
func TestDockerProxyContextRejectsRelativeSourceRootSubstitution(t *testing.T) {
	for _, replacement := range []string{"relative-link", "regular-inode"} {
		fixture := newDockerProxyTest(t)
		source := fixture.buildRequest.ContextDirectory
		target := filepath.Join(fixture.plan.ContextRoot, "other-source")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		swapped := false
		fixture.proxy.beforeContextOpen = func(role, relative string) {
			if role != "source" || !strings.HasSuffix(relative, "build-input") {
				return
			}
			if err := os.Rename(source, source+"-captured"); err != nil {
				t.Fatal(err)
			}
			var err error
			if replacement == "relative-link" {
				err = os.Symlink("other-source", source)
			} else {
				err = os.Rename(target, source)
			}
			if err != nil {
				t.Fatal(err)
			}
			swapped = true
		}
		if _, _, err := fixture.proxy.snapshotContext(context.Background(), source, noFollowSnapshotDestination(t, fixture)); err == nil || !swapped {
			t.Fatal("source-root substitution was accepted or not exercised", replacement, err)
		}
	}
}
