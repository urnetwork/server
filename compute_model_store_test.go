package server

// The model mirror's objects in the blob store (CSRV-M11-01; compute-plan-model-mirror-store, QD-1409): each file's
// index, chunks and state under compute-models/<sha256>/, read back as the routes read them; absent objects; a served
// file's objects final; and keys built from a lowercase hex SHA-256 and a chunk number alone. On a local blob store, no
// database.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/handler"
)

// A file the size of a few chunks, its SHA-256, its chunk files and an index file standing in for the tooling's.
type testComputeModelObjects struct {
	sha256      string
	chunkPaths  []string
	chunks      [][]byte
	indexPath   string
	index       []byte
	indexSha256 string
}

func newTestComputeModelObjects(t *testing.T, chunks ...string) *testComputeModelObjects {
	t.Helper()
	directory := t.TempDir()
	objects := &testComputeModelObjects{}
	whole := []byte{}
	for n, chunk := range chunks {
		path := filepath.Join(directory, "chunk-"+string(rune('a'+n)))
		if err := os.WriteFile(path, []byte(chunk), 0o600); err != nil {
			t.Fatal(err)
		}
		objects.chunkPaths = append(objects.chunkPaths, path)
		objects.chunks = append(objects.chunks, []byte(chunk))
		whole = append(whole, chunk...)
	}
	digest := sha256.Sum256(whole)
	objects.sha256 = hex.EncodeToString(digest[:])
	objects.index = []byte(`{"sha256":"` + objects.sha256 + `","version":1}`)
	indexDigest := sha256.Sum256(objects.index)
	objects.indexSha256 = hex.EncodeToString(indexDigest[:])
	objects.indexPath = filepath.Join(directory, "index")
	if err := os.WriteFile(objects.indexPath, objects.index, 0o600); err != nil {
		t.Fatal(err)
	}
	return objects
}

func testReadComputeModelObject(t *testing.T, reader io.ReadCloser, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The mirror writes a file's chunks, its index and its state under compute-models/<sha256>/ in the store's prefix,
// and the routes read each back as written: the state as its record, the index and each chunk byte for byte.
func TestComputeModelBlobStoreLayout(t *testing.T) {
	UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1024*1024)
	ctx := context.Background()
	mirror := NewComputeModelBlobStore(nil)
	objects := newTestComputeModelObjects(t, "first chunk.", "second chunk", "tail")

	for n, path := range objects.chunkPaths {
		if err := mirror.PutChunk(ctx, objects.sha256, n, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := mirror.PutFileRecord(ctx, objects.sha256, &handler.ComputeModelFileRecord{State: handler.ComputeModelFileStateCopied}); err != nil {
		t.Fatal(err)
	}
	if err := mirror.PutIndex(ctx, objects.sha256, objects.indexPath); err != nil {
		t.Fatal(err)
	}
	served := &handler.ComputeModelFileRecord{State: handler.ComputeModelFileStateServed, IndexSha256: objects.indexSha256}
	if err := mirror.PutFileRecord(ctx, objects.sha256, served); err != nil {
		t.Fatal(err)
	}

	store, ok := LoadBlobStore()
	if !ok {
		t.Fatal("no blob store")
	}
	listed, err := store.List(ctx, "blob/compute-models/")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, object := range listed {
		keys = append(keys, object.Key)
	}
	slices.Sort(keys)
	prefix := "blob/compute-models/" + objects.sha256 + "/"
	want := []string{prefix + "chunks/0", prefix + "chunks/1", prefix + "chunks/2", prefix + "index", prefix + "state"}
	if !slices.Equal(keys, want) {
		t.Fatalf("the mirror's keys = %v, want %v", keys, want)
	}

	record, err := mirror.FileRecord(ctx, objects.sha256)
	if err != nil || *record != *served {
		t.Fatalf("the state = %v %v, want %v", record, err, served)
	}
	reader, err := mirror.OpenIndex(ctx, objects.sha256)
	if index := testReadComputeModelObject(t, reader, err); string(index) != string(objects.index) {
		t.Fatalf("the index = %q", index)
	}
	for n, chunk := range objects.chunks {
		reader, err := mirror.OpenChunk(ctx, objects.sha256, n)
		if data := testReadComputeModelObject(t, reader, err); string(data) != string(chunk) {
			t.Fatalf("chunk %d = %q, want %q", n, data, chunk)
		}
	}
	stateReader, err := store.Get(ctx, prefix+"state")
	if state := testReadComputeModelObject(t, stateReader, err); string(state) != `{"index_sha256":"`+objects.indexSha256+`","state":"served"}` {
		t.Fatalf("the state object = %s", state)
	}
}

// A file the mirror has nothing of, or lacks an index or a chunk of, answers handler.ErrComputeModelObjectAbsent for
// that object, so the routes tell a file not served from a store that did not answer.
func TestComputeModelBlobStoreAbsentObjects(t *testing.T) {
	UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1024*1024)
	ctx := context.Background()
	mirror := NewComputeModelBlobStore(nil)
	objects := newTestComputeModelObjects(t, "only chunk")

	if _, err := mirror.FileRecord(ctx, objects.sha256); !errors.Is(err, handler.ErrComputeModelObjectAbsent) {
		t.Fatalf("no state: %v", err)
	}
	if _, err := mirror.OpenIndex(ctx, objects.sha256); !errors.Is(err, handler.ErrComputeModelObjectAbsent) {
		t.Fatalf("no index: %v", err)
	}
	if err := mirror.PutChunk(ctx, objects.sha256, 0, objects.chunkPaths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.OpenChunk(ctx, objects.sha256, 1); !errors.Is(err, handler.ErrComputeModelObjectAbsent) {
		t.Fatalf("no chunk 1: %v", err)
	}
	reader, err := mirror.OpenChunk(ctx, objects.sha256, 0)
	if data := testReadComputeModelObject(t, reader, err); string(data) != "only chunk" {
		t.Fatalf("chunk 0 = %q", data)
	}
}

// Once a file is served its objects are final: an index or a chunk written over it is refused, as is any state but the
// same one, which is answered as written; before it is served each may be written again.
func TestComputeModelBlobStoreServedFileIsFinal(t *testing.T) {
	UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1024*1024)
	ctx := context.Background()
	mirror := NewComputeModelBlobStore(nil)
	objects := newTestComputeModelObjects(t, "a chunk", "b chunk")

	for range 2 {
		if err := mirror.PutChunk(ctx, objects.sha256, 0, objects.chunkPaths[0]); err != nil {
			t.Fatal(err)
		}
	}
	indexed := &handler.ComputeModelFileRecord{State: handler.ComputeModelFileStateIndexed, IndexSha256: objects.indexSha256}
	if err := mirror.PutFileRecord(ctx, objects.sha256, indexed); err != nil {
		t.Fatal(err)
	}
	if err := mirror.PutIndex(ctx, objects.sha256, objects.indexPath); err != nil {
		t.Fatal(err)
	}
	served := &handler.ComputeModelFileRecord{State: handler.ComputeModelFileStateServed, IndexSha256: objects.indexSha256}
	if err := mirror.PutFileRecord(ctx, objects.sha256, served); err != nil {
		t.Fatal(err)
	}

	if err := mirror.PutChunk(ctx, objects.sha256, 1, objects.chunkPaths[1]); !errors.Is(err, ErrComputeModelFileServed) {
		t.Fatalf("a chunk over a served file: %v", err)
	}
	if err := mirror.PutIndex(ctx, objects.sha256, objects.indexPath); !errors.Is(err, ErrComputeModelFileServed) {
		t.Fatalf("an index over a served file: %v", err)
	}
	if err := mirror.PutFileRecord(ctx, objects.sha256, indexed); !errors.Is(err, ErrComputeModelFileServed) {
		t.Fatalf("a served file put back to indexed: %v", err)
	}
	other := &handler.ComputeModelFileRecord{State: handler.ComputeModelFileStateServed, IndexSha256: strings.Repeat("e", 64)}
	if err := mirror.PutFileRecord(ctx, objects.sha256, other); !errors.Is(err, ErrComputeModelFileServed) {
		t.Fatalf("a served file given another pin: %v", err)
	}
	if err := mirror.PutFileRecord(ctx, objects.sha256, served); err != nil {
		t.Fatalf("the same served state again: %v", err)
	}
	if _, err := mirror.OpenChunk(ctx, objects.sha256, 1); !errors.Is(err, handler.ErrComputeModelObjectAbsent) {
		t.Fatalf("the refused chunk was written: %v", err)
	}
}

// A key is built from a lowercase hex SHA-256 and a chunk number from 0 alone, so nothing else reaches the store; a
// state no record may hold is refused before it is written, and a stored state that does not read, of an unknown
// state, carrying more or past its bound is a fault, never a file the routes serve.
func TestComputeModelBlobStoreRefusesBadKeysAndStates(t *testing.T) {
	UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1024*1024)
	ctx := context.Background()
	mirror := NewComputeModelBlobStore(nil)
	objects := newTestComputeModelObjects(t, "chunk")

	for _, sha256Text := range []string{strings.ToUpper(objects.sha256), objects.sha256[:63], "../" + objects.sha256[3:], "", strings.Repeat("g", 64)} {
		if err := mirror.PutChunk(ctx, sha256Text, 0, objects.chunkPaths[0]); err == nil {
			t.Errorf("a chunk under %q was written", sha256Text)
		}
		if _, err := mirror.FileRecord(ctx, sha256Text); err == nil || errors.Is(err, handler.ErrComputeModelObjectAbsent) {
			t.Errorf("the state of %q: %v", sha256Text, err)
		}
	}
	if err := mirror.PutChunk(ctx, objects.sha256, -1, objects.chunkPaths[0]); err == nil {
		t.Error("chunk -1 was written")
	}
	for _, record := range []*handler.ComputeModelFileRecord{
		nil,
		{State: handler.ComputeModelFileStateCopied, IndexSha256: objects.indexSha256},
		{State: handler.ComputeModelFileStateIndexed},
		{State: handler.ComputeModelFileStateServed, IndexSha256: strings.ToUpper(objects.indexSha256)},
		{State: "removed", IndexSha256: objects.indexSha256},
	} {
		if err := mirror.PutFileRecord(ctx, objects.sha256, record); err == nil {
			t.Errorf("the state %v was written", record)
		}
	}
	store, ok := LoadBlobStore()
	if !ok {
		t.Fatal("no blob store")
	}
	if listed, err := store.List(ctx, "blob/"); err != nil || len(listed) != 0 {
		t.Fatalf("refused writes left %v %v", listed, err)
	}

	stateKey := "blob/compute-models/" + objects.sha256 + "/state"
	for _, state := range []string{
		`{"state":"served"}`,
		`{"index_sha256":"` + objects.indexSha256 + `","state":"removed"}`,
		`{"index_sha256":"` + objects.indexSha256 + `","state":"served","extra":1}`,
		`{"index_sha256":"` + objects.indexSha256 + `","state":"served"} {}`,
		`not json`,
		`{"state":"copied","note":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		path := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, stateKey, path, "application/json"); err != nil {
			t.Fatal(err)
		}
		if record, err := mirror.FileRecord(ctx, objects.sha256); err == nil || errors.Is(err, handler.ErrComputeModelObjectAbsent) {
			t.Errorf("the stored state %.60s: %v %v", state, record, err)
		}
	}
}
