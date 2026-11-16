//go:build linux

// Kernel statfs word widths differ between native and compat executables.
package competitionagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Run on amd64 and386: an observed filesystem magic must pass the typed
// native reader, while a different magic cannot become evidence authority.
func TestManagementSshNativeFilesystemWordWidth(t *testing.T) {
	directory := t.TempDir()
	content := []byte("synthetic-property\n")
	if err := os.WriteFile(filepath.Join(directory, "property"), content, 0600); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	var filesystem syscall.Statfs_t
	if err := syscall.Fstatfs(int(parent.Fd()), &filesystem); err != nil {
		t.Fatal(err)
	}
	magic := int64(filesystem.Type)
	if magic == 0 {
		t.Fatal("test filesystem has no definite magic")
	}
	actual, err := readManagementSshFile(parent, "property", 1024, magic)
	if err != nil || string(actual) != string(content) {
		t.Fatal("kernel-width filesystem identity was rejected", err)
	}
	if _, err := readManagementSshFile(parent, "property", 1024, magic^1); err == nil {
		t.Fatal("different filesystem identity was accepted")
	}
}
