//go:build linux

// Escaped native unit names remain literal Linux filenames, not artifact paths.
package competitionagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Exercise discovery and both descriptor snapshots with a real escaped name.
func TestManagementOrdinaryReadsLiteralEscapedMountCgroup(t *testing.T) {
	policy, settings, input := managementOrdinaryTestFacts(t)
	root := t.TempDir()
	writeManagementOrdinaryTestTree(t, root, input)
	parent := filepath.Join(root, managementPrivateCgroupPath)
	if err := os.Rename(filepath.Join(parent, "synthetic-control.service"), filepath.Join(parent, `synthetic\x2dworkspace.mount`)); err != nil {
		t.Fatal(err)
	}
	facts, err := readManagementOrdinaryFacts(t.Context(), policy, settings, root, false, nil)
	if err != nil {
		t.Fatal("literal systemd mount cgroup rejected", err)
	}
	if err := validateManagementOrdinaryFacts(facts, policy, settings); err != nil {
		t.Fatal("escaped descendant lost ordinary resource proof", err)
	}
}

// Kernel support does not widen submission/journal path spelling or aliases.
func TestManagementKernelEscapesRetainNoFollowAndArtifactPolicy(t *testing.T) {
	root := t.TempDir()
	name := `synthetic\x2dworkspace.mount`
	if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
		t.Fatal(err)
	}
	held, err := openDirectoryNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if file, err := openRelativeNoFollow(held, name, os.O_RDONLY|syscall.O_DIRECTORY, 0); err == nil {
		_ = file.Close()
		t.Fatal("artifact descriptor silently acquired kernel path grammar")
	}
	if err := os.Symlink(name, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../escape", "/absolute", "alias", "alias/child", "bad\nname"} {
		if file, err := openKernelRelativeNoFollow(held, path, os.O_RDONLY|syscall.O_DIRECTORY, 0); err == nil {
			_ = file.Close()
			t.Fatalf("kernel descriptor admitted alias/traversal %q", path)
		}
	}
}
