package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A plain directory, such as an encrypted dataset's mount point while the
// dataset is not mounted, is refused.
func TestRequireOwnMountRefusesAPlainDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage-archive")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := requireOwnMount(dir); err == nil {
		t.Fatal("a plain directory passed as a mount point")
	}
}

// The resource needs its user, authority and db; a missing file fails.
func TestConnectNeedsTheResourceFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg.yml")
	if err := os.WriteFile(path, []byte("user: u\npassword: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connect(t.Context(), path); err == nil {
		t.Fatal("a resource without authority and db connected")
	}
	if _, err := connect(t.Context(), filepath.Join(dir, "absent.yml")); err == nil {
		t.Fatal("a missing resource connected")
	}
}
