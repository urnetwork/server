package stats

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// stubBlobStore is an in-memory server.BlobStore for tests: objects live in a
// map, Put reads the local file, and per-key/listing failures are injectable.
type stubBlobStore struct {
	lock    sync.Mutex
	prefix  string
	objects map[string][]byte
	listErr error
	putErr  func(key string) error
}

func newStubBlobStore(prefix string) *stubBlobStore {
	return &stubBlobStore{prefix: prefix, objects: map[string][]byte{}}
}

func (self *stubBlobStore) Put(ctx context.Context, key string, localPath string, contentType string) error {
	if self.putErr != nil {
		if err := self.putErr(key); err != nil {
			return err
		}
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	self.objects[key] = content
	return nil
}

func (self *stubBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	content, ok := self.objects[key]
	if !ok {
		return nil, fmt.Errorf("no object %s", key)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (self *stubBlobStore) List(ctx context.Context, keyPrefix string) ([]server.BlobObject, error) {
	if self.listErr != nil {
		return nil, self.listErr
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	objects := []server.BlobObject{}
	for key, content := range self.objects {
		if strings.HasPrefix(key, keyPrefix) {
			objects = append(objects, server.BlobObject{Key: key, Size: int64(len(content))})
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (self *stubBlobStore) SetLifecycle(ctx context.Context, rules []server.BlobLifecycleRule) error {
	return nil
}
func (self *stubBlobStore) Bucket() string    { return "stub-bucket" }
func (self *stubBlobStore) Prefix() string    { return self.prefix }
func (self *stubBlobStore) Authority() string { return "stub" }

// happy path: the in-window segments round-trip through the .tgz along with
// the manifest, schema, and README; out-of-window segments are excluded.
func TestExportSamplesRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newStubBlobStore("stats")
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	oldDay := now.AddDate(0, 0, -30).Format("2006-01-02")

	keyA := "stats/main/findproviders2/" + day + "/host1/1700000000000-1.pb.zst"
	keyB := "stats/main/findproviders2/" + day + "/host1/1700000000001-2.pb.zst"
	keyOld := "stats/main/findproviders2/" + oldDay + "/host1/1600000000000-1.pb.zst"
	store.objects[keyA] = []byte("segment-a-bytes")
	store.objects[keyB] = []byte("segment-b-bytes")
	store.objects[keyOld] = []byte("old-segment")

	outPath := filepath.Join(t.TempDir(), "export.tgz")
	if err := ExportSamples(ctx, store, "main", 7, now.UnixMilli(), outPath); err != nil {
		t.Fatalf("export: %v", err)
	}

	entries := readTgz(t, outPath)
	if !bytes.Equal(entries[keyA], []byte("segment-a-bytes")) {
		t.Fatalf("segment A did not round-trip: %q", entries[keyA])
	}
	if !bytes.Equal(entries[keyB], []byte("segment-b-bytes")) {
		t.Fatalf("segment B did not round-trip: %q", entries[keyB])
	}
	if _, ok := entries[keyOld]; ok {
		t.Fatal("out-of-window segment included")
	}
	for _, name := range []string{"manifest.json", "sample.proto", "README.md"} {
		if len(entries[name]) == 0 {
			t.Fatalf("missing archive entry %s", name)
		}
	}
}

func readTgz(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tarReader := tar.NewReader(gzipReader)
	entries := map[string][]byte{}
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("tar read %s: %v", header.Name, err)
		}
		entries[header.Name] = content
	}
}

// failingWriter rejects every write, the shape of a full disk at flush time.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("disk full")
}

// a write/flush failure in the gzip/tar close path must surface as an error —
// the deferred closes used to discard it and report a truncated .tgz as
// success.
func TestExportSamplesCloseErrorPropagates(t *testing.T) {
	ctx := context.Background()
	store := newStubBlobStore("stats") // empty: all content flushes at close
	err := writeExportArchive(ctx, store, "main", 7, time.Now().UnixMilli(), failingWriter{})
	if err == nil {
		t.Fatal("archive close/flush error was swallowed")
	}
}

// any export failure removes the partial output file instead of leaving a
// truncated archive behind.
func TestExportSamplesRemovesPartialOutputOnError(t *testing.T) {
	ctx := context.Background()
	store := newStubBlobStore("stats")
	store.listErr = fmt.Errorf("store unavailable")

	outPath := filepath.Join(t.TempDir(), "export.tgz")
	if err := ExportSamples(ctx, store, "main", 7, time.Now().UnixMilli(), outPath); err == nil {
		t.Fatal("export succeeded against a failing store")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("partial output left behind: stat err = %v", err)
	}

	flatPath := filepath.Join(t.TempDir(), "export.pb.xz")
	if _, err := ExportSamplesFlat(ctx, store, "main", 7, time.Now().UnixMilli(), flatPath); err == nil {
		t.Fatal("flat export succeeded against a failing store")
	}
	if _, err := os.Stat(flatPath); !os.IsNotExist(err) {
		t.Fatalf("partial flat output left behind: stat err = %v", err)
	}
}
