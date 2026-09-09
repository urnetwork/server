package server

// Local usage scans ignore only private staged file names. Real independent
// writers commit at an explicit enumeration barrier; no timing assumption or
// replacement persistence implementation supplies the observed result.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Source files remain outside the store's independently charged namespace.
func localBlobUsageSource(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The marker sorts before the writer's hidden partial, so both directory-walk
// implementations enumerate the partial before this committed entry is visited.
func localBlobUsageMarker(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "blob", "!.usage-scan")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Both public write APIs retain exact committed bytes while another store
// instance removes its staged name after the scanner's directory enumeration.
func checkLocalBlobUsageVanishedPartial(t *testing.T, ifAbsent, cancelScan bool) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := localBlobUsageMarker(t, root)
	writer := NewLocalBlobStoreWithMaxBytes(root, "blob", 6).(*localBlobStore)
	scanner := NewLocalBlobStoreWithMaxBytes(root, "blob", 6).(*localBlobStore)
	first := localBlobUsageSource(t, "1234")
	second := localBlobUsageSource(t, "56")
	ctx, cancel := context.WithCancel(t.Context())
	entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	type writeResult struct {
		created bool
		err     error
	}
	results := make(chan writeResult, 1)
	writer.beforeCreateCommitForTest = func() {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(joined)
		created, err := writer.PutIfAbsent(ctx, "blob/first.json", first, "application/json")
		results <- writeResult{created: created, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		<-joined
	})
	select {
	case <-entered:
	case result := <-results:
		t.Fatalf("real writer ended before its staging barrier: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	partials, err := filepath.Glob(filepath.Join(root, "blob", ".first.json-*"+blobPartialSuffix))
	if err != nil || len(partials) != 1 {
		t.Fatalf("real writer did not retain exactly its private partial: %v/%v", partials, err)
	}
	visited := false
	scanner.afterUsageScanEntryForTest = func(path string) {
		if path != marker {
			return
		}
		if visited {
			t.Fatal("usage marker was visited more than once")
		}
		visited = true
		close(release)
		select {
		case result := <-results:
			if result.err != nil || !result.created {
				t.Fatalf("real first writer did not commit: %+v", result)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := os.Lstat(partials[0]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("writer did not remove its own enumerated partial: %v", err)
		}
		if cancelScan {
			cancel()
		}
	}
	// Preserve the historical scan race without holding the new mutation lock:
	// the independent writer must be able to commit while this census runs.
	if _, err := scanner.usageBytes(); err != nil {
		t.Fatalf("vanished private partial refused an otherwise valid write: %v", err)
	}
	scanner.afterUsageScanEntryForTest = nil
	var created bool
	if ifAbsent {
		created, err = scanner.PutIfAbsent(ctx, "blob/second.json", second, "application/json")
	} else {
		err = scanner.Put(ctx, "blob/second.json", second, "application/json")
	}
	if !visited {
		t.Fatal("real capacity scan did not reach the enumerated-entry barrier")
	}
	if cancelScan {
		if !errors.Is(err, context.Canceled) || created {
			t.Fatalf("partial exclusion masked canceled write: created=%t error=%v", created, err)
		}
		if _, err := os.Lstat(scanner.pathFor("blob/second.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled scan published a destination: %v", err)
		}
	} else if err != nil || ifAbsent && !created {
		t.Fatalf("vanished private partial refused an otherwise valid write: created=%t error=%v", created, err)
	}
	scanner.afterUsageScanEntryForTest = nil
	expected := []struct {
		key     string
		content string
	}{{key: "blob/first.json", content: "1234"}}
	wantUsage := int64(4)
	if !cancelScan {
		expected = append(expected, struct {
			key     string
			content string
		}{key: "blob/second.json", content: "56"})
		wantUsage = 6
	}
	for _, object := range expected {
		reader, err := scanner.Get(t.Context(), object.key)
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || string(raw) != object.content {
			t.Fatalf("committed winner changed: key=%s bytes=%q read=%v close=%v", object.key, raw, readErr, closeErr)
		}
	}
	usage, err := scanner.usageBytes()
	if err != nil || usage != wantUsage {
		t.Fatalf("committed namespace charge = %d/%v, want %d", usage, err, wantUsage)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, blobPartialSuffix) && path != filepath.Join(root, localBlobCapacityLockName) {
			t.Errorf("writer left private staged bytes: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Atomic create does not inspect an already excluded private partial.
func TestLocalBlobStorePutIfAbsentIgnoresVanishedWriterPartial(t *testing.T) {
	t.Parallel()
	checkLocalBlobUsageVanishedPartial(t, true, false)
}

// Ordinary replacement shares the same capacity scan and temporary ownership.
func TestLocalBlobStorePutIgnoresVanishedWriterPartial(t *testing.T) {
	t.Parallel()
	checkLocalBlobUsageVanishedPartial(t, false, false)
}

// Ignoring another writer's private file cannot authorize canceled publication.
func TestLocalBlobStoreUsagePartialCleanupPreservesCancellation(t *testing.T) {
	t.Parallel()
	checkLocalBlobUsageVanishedPartial(t, true, true)
}

// Only private files are excluded: an ordinary vanished entry still prevents
// a partial namespace census from authorizing a new immutable object.
func TestLocalBlobStoreUsageRetainsOrdinaryScanFailure(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := localBlobUsageMarker(t, root)
	ordinary := filepath.Join(root, "blob", "ordinary.partial-extra")
	if err := os.WriteFile(ordinary, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 6).(*localBlobStore)
	removed := false
	store.afterUsageScanEntryForTest = func(path string) {
		if path == marker {
			if err := os.Remove(ordinary); err != nil {
				t.Fatal(err)
			}
			removed = true
		}
	}
	created, err := store.PutIfAbsent(t.Context(), "blob/new.json", localBlobUsageSource(t, "4"), "application/json")
	if !removed || created || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary scan error was masked: removed=%t created=%t error=%v", removed, created, err)
	}
	if _, err := os.Lstat(store.pathFor("blob/new.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete ordinary census published a new key: %v", err)
	}
}

// A directory suffix cannot hide committed descendants from exact capacity;
// only staged files are excluded, and replacement retains its original credit.
func TestLocalBlobStoreUsageCountsPartialDirectoryAndExactCapacity(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "blob", "namespace.partial")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "committed.json"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob", ".pending.json-1.partial"), []byte("ignored-stage"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 6).(*localBlobStore)
	created, err := store.PutIfAbsent(t.Context(), "blob/second.json", localBlobUsageSource(t, "456"), "application/json")
	if err != nil || !created {
		t.Fatalf("exact committed capacity refused: %t/%v", created, err)
	}
	if created, err := store.PutIfAbsent(t.Context(), "blob/third.json", localBlobUsageSource(t, "7"), "application/json"); err == nil || created || !strings.Contains(err.Error(), "local blob capacity exceeded") {
		t.Fatalf("one-over committed capacity accepted: %t/%v", created, err)
	}
	if err := store.Put(t.Context(), "blob/second.json", localBlobUsageSource(t, "45"), "application/json"); err != nil {
		t.Fatal("replacement lost its own committed charge", err)
	}
	if created, err := store.PutIfAbsent(t.Context(), "blob/third.json", localBlobUsageSource(t, "7"), "application/json"); err != nil || !created {
		t.Fatalf("replacement credit did not preserve exact capacity: %t/%v", created, err)
	}
	if usage, err := store.usageBytes(); err != nil || usage != 6 {
		t.Fatalf("partial directory concealed committed bytes: %d/%v", usage, err)
	}
}
