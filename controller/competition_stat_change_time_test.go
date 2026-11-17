// The custody clock comparison sees an inode change on every host that builds
// the controller: the linux services and an operator's Mac.
package controller

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The raw stat of one path, as the expired bridge reads it.
func lstatForChangeTime(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("lstat returned no syscall.Stat_t")
	}
	return stat
}

// An unchanged file keeps its change time; a chmod moves it.
func TestStatChangeTimeEqualSeesAnInodeChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	if err := os.WriteFile(path, []byte("record"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := lstatForChangeTime(t, path)
	if !statChangeTimeEqual(first, lstatForChangeTime(t, path)) {
		t.Fatal("an unchanged file reported a new change time")
	}
	// Some filesystems keep coarse timestamps: retry the chmod until the clock moves, within a second.
	modes := []os.FileMode{0o640, 0o600}
	for i := 0; i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		if err := os.Chmod(path, modes[i%2]); err != nil {
			t.Fatal(err)
		}
		if !statChangeTimeEqual(first, lstatForChangeTime(t, path)) {
			return
		}
	}
	t.Fatal("a chmod never moved the change time")
}
