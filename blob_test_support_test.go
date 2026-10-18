package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Records the cleanups a helper registers, so a test can run them itself and
// observe what they put back.
type blobTestCleanups struct {
	testing.TB
	cleanups []func()
}

func (self *blobTestCleanups) Cleanup(cleanup func()) {
	self.cleanups = append(self.cleanups, cleanup)
}

// A test in another package reads a local store with its own root, prefix and
// allocation from LoadBlobStore, and the configured selection is back when
// that test ends.
func TestUseLocalBlobStoreForTestStandsInUntilTheTestEnds(t *testing.T) {
	connect.AssertEqual(t, blobStoreForTest.Load() == nil, true)
	root := t.TempDir()
	object := func(size int) string {
		path := filepath.Join(t.TempDir(), "object")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// the stand-in's cleanups run below, where the end of a test would run them
	cleanups := &blobTestCleanups{TB: t}
	UseLocalBlobStoreForTest(cleanups, root, "stand-in", 32)
	store, ok := LoadBlobStore()
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, store.Prefix(), "stand-in")
	// it writes below its root, and refuses what its allocation cannot hold
	if err := store.Put(t.Context(), "stand-in/small", object(8), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "stand-in", "small")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), "stand-in/large", object(64), "application/octet-stream"); err == nil {
		t.Fatal("a 64-byte object fit a 32-byte allocation")
	}

	for i := len(cleanups.cleanups) - 1; 0 <= i; i -= 1 {
		cleanups.cleanups[i]()
	}
	connect.AssertEqual(t, blobStoreForTest.Load() == nil, true)
}

// A later stand-in replaces an earlier one, and each end puts back the one
// before it.
func TestUseLocalBlobStoreForTestNestsInOrder(t *testing.T) {
	first := &blobTestCleanups{TB: t}
	UseLocalBlobStoreForTest(first, t.TempDir(), "first", 1024)
	second := &blobTestCleanups{TB: t}
	UseLocalBlobStoreForTest(second, t.TempDir(), "second", 1024)
	store, _ := LoadBlobStore()
	connect.AssertEqual(t, store.Prefix(), "second")

	for i := len(second.cleanups) - 1; 0 <= i; i -= 1 {
		second.cleanups[i]()
	}
	store, _ = LoadBlobStore()
	connect.AssertEqual(t, store.Prefix(), "first")

	for i := len(first.cleanups) - 1; 0 <= i; i -= 1 {
		first.cleanups[i]()
	}
	connect.AssertEqual(t, blobStoreForTest.Load() == nil, true)
}

// The selection belongs to the process, so a test that stands a store in
// cannot go on to run in parallel with the tests that load one.
func TestUseLocalBlobStoreForTestRefusesALaterParallel(t *testing.T) {
	UseLocalBlobStoreForTest(t, t.TempDir(), "stand-in", 1024)
	message := func() (message string) {
		defer func() { message = fmt.Sprint(recover()) }()
		t.Parallel()
		return ""
	}()
	if !strings.Contains(message, "t.Parallel") {
		t.Fatalf("t.Parallel after the stand-in: expected a refusal, got %q", message)
	}
}

// A parallel test cannot stand a store in, and the refused call leaves the
// selection as it was.
func TestUseLocalBlobStoreForTestRefusesAParallelTest(t *testing.T) {
	t.Parallel()
	before := blobStoreForTest.Load()
	message := func() (message string) {
		defer func() { message = fmt.Sprint(recover()) }()
		UseLocalBlobStoreForTest(t, t.TempDir(), "stand-in", 1024)
		return ""
	}()
	if !strings.Contains(message, "t.Parallel") {
		t.Fatalf("the stand-in in a parallel test: expected a refusal, got %q", message)
	}
	connect.AssertEqual(t, blobStoreForTest.Load() == before, true)
}
