package stats

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestSegments(t *testing.T, root string, names []string, size int) string {
	t.Helper()
	dir := filepath.Join(root, "local", "host1", "findproviders2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		// deterministic mtime ordering for the cap's oldest-first drop
		mtime := time.Now().Add(time.Duration(i-len(names)) * time.Minute)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	return dir
}

// one poison segment must not starve the segments behind it: the sweep logs,
// counts, keeps it for retry, and CONTINUES — it used to return on the first
// failure, blocking every later segment on each 30s tick.
func TestUploadSweepContinuesPastFailingSegment(t *testing.T) {
	root := t.TempDir()
	names := []string{
		"1700000000000-1.pb.zst",
		"1700000000001-2.pb.zst", // the poison one (middle of walk order)
		"1700000000002-3.pb.zst",
	}
	dir := writeTestSegments(t, root, names, 8)

	store := newStubBlobStore("stats")
	poisoned := true
	store.putErr = func(key string) error {
		if poisoned && strings.Contains(key, "1700000000001-2") {
			return fmt.Errorf("poison segment")
		}
		return nil
	}

	uploader := &Uploader{
		ctx:       context.Background(),
		statsRoot: root,
		env:       "local",
		store:     store,
		settings:  DefaultUploaderSettings(),
	}

	uploader.sweep()

	if _, err := os.Stat(filepath.Join(dir, names[1])); err != nil {
		t.Fatalf("failed segment must be kept for retry: %v", err)
	}
	for _, name := range []string{names[0], names[2]} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("segment %s behind the poison one did not ship", name)
		}
	}
	if got := uploader.uploaded.Load(); got != 2 {
		t.Fatalf("uploaded = %d; want 2", got)
	}
	if got := uploader.failed.Load(); got != 1 {
		t.Fatalf("failed = %d; want 1", got)
	}

	// the kept segment ships on the next sweep once the store recovers
	poisoned = false
	uploader.sweep()
	if _, err := os.Stat(filepath.Join(dir, names[1])); !os.IsNotExist(err) {
		t.Fatalf("recovered segment did not ship on the next sweep")
	}
	if got := uploader.uploaded.Load(); got != 3 {
		t.Fatalf("uploaded after retry = %d; want 3", got)
	}
	// the shipped object keys carry the date-partitioned layout
	for key := range store.objects {
		if !strings.HasPrefix(key, "stats/local/findproviders2/") {
			t.Fatalf("unexpected object key %s", key)
		}
	}
}

// cap enforcement still runs when uploads fail: with an unreachable store the
// oldest finalized segments over the cap are dropped, not the whole sweep's
// worth kept forever.
func TestUploadSweepEnforcesCapOnFailure(t *testing.T) {
	root := t.TempDir()
	names := []string{
		"1700000000000-1.pb.zst",
		"1700000000001-2.pb.zst",
		"1700000000002-3.pb.zst",
	}
	dir := writeTestSegments(t, root, names, 100)

	store := newStubBlobStore("stats")
	store.putErr = func(key string) error { return fmt.Errorf("store unreachable") }

	uploader := &Uploader{
		ctx:       context.Background(),
		statsRoot: root,
		env:       "local",
		store:     store,
		settings:  &UploaderSettings{Interval: time.Second, LocalCapBytes: 150},
	}

	uploader.sweep()

	if got := uploader.failed.Load(); got != 3 {
		t.Fatalf("failed = %d; want 3", got)
	}
	// 300 bytes total > 150 cap: the two oldest drop, the newest survives
	if got := uploader.dropped.Load(); got != 2 {
		t.Fatalf("dropped = %d; want 2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, names[2])); err != nil {
		t.Fatalf("newest segment must survive the cap: %v", err)
	}
	for _, name := range []string{names[0], names[1]} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("oldest segment %s survived the cap", name)
		}
	}
}
