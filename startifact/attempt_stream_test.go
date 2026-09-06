// Exact typed storage tests use local immutable stores and injected I/O edges,
// never shared MinIO, PostgreSQL, Redis or a testnet transaction.
package startifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/server"
)

// Homogeneous test limits remain small without changing the production codec.
func attemptObjectTestBounds() AttemptObjectBounds {
	return AttemptObjectBounds{MetadataBytes: 128, RecordBytes: 256, ProofBytes: 192}
}

// Derives the exact raw-byte identity, not an envelope payload hash.
func attemptObjectTestHash(data []byte) string {
	return fmt.Sprintf("0x%x", sha256.Sum256(data))
}

// Each call owns its reader and failure callbacks. Embedded storage retains
// real atomic behavior when only one boundary is being replaced.
type attemptObjectTestStore struct {
	server.BlobStore
	get func(context.Context, string) (io.ReadCloser, error)
	put func(context.Context, string, string, string) (bool, error)
}

// The owner selects one exact retrieval result without a process-global hook.
func (self *attemptObjectTestStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if self.get != nil {
		return self.get(ctx, key)
	}
	return self.BlobStore.Get(ctx, key)
}

// Writes still use atomic create, including the losing concurrent publisher.
func (self *attemptObjectTestStore) PutIfAbsent(ctx context.Context, key, filePath, contentType string) (bool, error) {
	if self.put != nil {
		return self.put(ctx, key, filePath, contentType)
	}
	return self.BlobStore.PutIfAbsent(ctx, key, filePath, contentType)
}

// The real local backend persists independent typed keys across new clients.
func TestAttemptObjectTypedLocalRoundTrip(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store := server.NewLocalBlobStore(directory, "test-attempt")
	data := []byte("{\"sequence\":1}\n")
	contentHash := attemptObjectTestHash(data)
	keys := map[string]bool{}
	for _, kind := range []string{"metadata", "records", "proofs"} {
		for range 2 {
			if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), kind, contentHash, data); err != nil {
				t.Fatal(err)
			}
		}
		key, err := AttemptObjectKey(store, kind, contentHash)
		if err != nil || keys[key] {
			t.Fatalf("typed namespace alias: %s %v", key, err)
		}
		keys[key] = true
		reopened := server.NewLocalBlobStore(directory, "test-attempt")
		var result bytes.Buffer
		count, err := ReadAttemptObjectTo(t.Context(), reopened, attemptObjectTestBounds(), kind, contentHash, &result)
		if err != nil || count != uint64(len(data)) || !bytes.Equal(data, result.Bytes()) {
			t.Fatalf("typed object %s changed after restart: count=%d error=%v", kind, count, err)
		}
	}
}

// Alias rejection precedes any object-store lookup or temporary upload.
func TestAttemptObjectCanonicalAdmissionHasNoStorageEffects(t *testing.T) {
	t.Parallel()
	base := newImmutableRaceBlobStore("")
	calls := 0
	store := &attemptObjectTestStore{BlobStore: base, get: func(context.Context, string) (io.ReadCloser, error) {
		calls++
		return nil, errors.New("unexpected storage call")
	}, put: func(context.Context, string, string, string) (bool, error) {
		calls++
		return false, errors.New("unexpected storage call")
	}}
	data := []byte("original")
	contentHash := attemptObjectTestHash(data)
	for _, hash := range []string{"", contentHash[2:], "sha256:" + contentHash[2:], " " + contentHash, strings.ToUpper(contentHash), "0x" + strings.Repeat("0", 64), contentHash + "/.."} {
		if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash, data); err == nil {
			t.Fatalf("noncanonical upload admitted: %q", hash)
		}
		if _, err := ReadAttemptObjectTo(t.Context(), store, attemptObjectTestBounds(), "records", hash, io.Discard); err == nil {
			t.Fatalf("noncanonical read admitted: %q", hash)
		}
	}
	for _, kind := range []string{"", "Records", "../records", "metadata/../proofs"} {
		if _, err := ReadAttemptObjectTo(t.Context(), store, attemptObjectTestBounds(), kind, contentHash, io.Discard); err == nil {
			t.Fatalf("untyped read admitted: %q", kind)
		}
	}
	if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", contentHash, []byte("modified")); err == nil || calls != 0 {
		t.Fatalf("invalid bytes reached storage: calls=%d error=%v", calls, err)
	}
}

// Configured roots cannot normalize into another object namespace.
func TestAttemptObjectRejectsNoncanonicalStorePrefix(t *testing.T) {
	t.Parallel()
	store := newImmutableRaceBlobStore("")
	for _, prefix := range []string{"", "/blob", "blob/", "blob//other", "blob/../other", "blob/./other", "blob\\other"} {
		store.prefix = prefix
		if key, err := AttemptObjectKey(store, "records", attemptObjectTestHash([]byte("one"))); err == nil || key != "" {
			t.Fatalf("aliased prefix admitted: %q", prefix)
		}
	}
}

// Integer limits and each separate type are checked before backend effects.
func TestAttemptObjectRejectsMissingOverflowAndTypedBounds(t *testing.T) {
	t.Parallel()
	for _, bounds := range []AttemptObjectBounds{{}, {MetadataBytes: 1, RecordBytes: math.MaxInt64, ProofBytes: 1}, {MetadataBytes: 1, RecordBytes: 1, ProofBytes: math.MaxUint64}} {
		if bounds.Validate() == nil {
			t.Fatalf("invalid bounds accepted: %+v", bounds)
		}
	}
	store := newImmutableRaceBlobStore("")
	data := bytes.Repeat([]byte("x"), 193)
	hash := attemptObjectTestHash(data)
	if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash, data); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"metadata", "proofs"} {
		if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), kind, hash, data); err == nil {
			t.Fatalf("%s borrowed the record budget", kind)
		}
	}
	if len(store.objects) != 1 {
		t.Fatalf("oversized typed uploads created objects: %d", len(store.objects))
	}
}

// Two simultaneous valid publishers converge through real PutIfAbsent.
func TestAttemptObjectConcurrentPublicationUsesAtomicWinner(t *testing.T) {
	t.Parallel()
	data := []byte("same\n")
	hash := attemptObjectTestHash(data)
	store := newImmutableRaceBlobStore("")
	key, err := AttemptObjectKey(store, "records", hash)
	if err != nil {
		t.Fatal(err)
	}
	store.raceKey, store.raceReady = key, make(chan struct{})
	results := make(chan error, 2)
	var owners sync.WaitGroup
	for range 2 {
		owners.Add(1)
		go func() {
			defer owners.Done()
			results <- PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash, data)
		}()
	}
	owners.Wait()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if store.raceArrivals != 2 || len(store.objects) != 1 || !bytes.Equal(store.objects[key], data) {
		t.Fatal("atomic publication failed to retain one exact winner")
	}
}

// Both a conflicting prior winner and a corrupt newly created object fail
// full fetch-back. Neither failure permits replacing the stored object.
func TestAttemptObjectPublicationRejectsConflictingAndCorruptWinner(t *testing.T) {
	t.Parallel()
	data, changed := []byte("original"), []byte("modified")
	hash := attemptObjectTestHash(data)
	for _, existing := range []bool{false, true} {
		store := newImmutableRaceBlobStore("")
		key, err := AttemptObjectKey(store, "records", hash)
		if err != nil {
			t.Fatal(err)
		}
		if existing {
			store.objects[key] = changed
		} else {
			store.createdContent = changed
		}
		if err := PublishAttemptObject(t.Context(), store, attemptObjectTestBounds(), "records", hash, data); err == nil || !bytes.Equal(store.objects[key], changed) {
			t.Fatalf("corrupt immutable winner was accepted or overwritten: %v", err)
		}
	}
}

// Reader controls preserve errors and verify closure on every read outcome.
type attemptObjectTestReader struct {
	read   func([]byte) (int, error)
	close  func() error
	closed int
}

// Each reader has exactly one call owner, just like a real blob response.
func (self *attemptObjectTestReader) Read(data []byte) (int, error) { return self.read(data) }

// Retains the actual terminal close count without a shared global hook.
func (self *attemptObjectTestReader) Close() error {
	self.closed++
	if self.close != nil {
		return self.close()
	}
	return nil
}

// Content EOF does not erase a joined transport error, even for valid bytes.
func TestAttemptObjectReadRetainsJoinedEOFAndCloseFailures(t *testing.T) {
	t.Parallel()
	data := []byte("original")
	failure := errors.New("terminal storage failure")
	for _, failureAt := range []string{"read", "close", "get"} {
		reader := &attemptObjectTestReader{read: bytes.NewReader(data).Read}
		if failureAt == "read" {
			reader.read = func(value []byte) (int, error) { return copy(value, data), errors.Join(io.EOF, failure) }
		} else {
			reader.close = func() error { return failure }
		}
		store := &attemptObjectTestStore{BlobStore: newImmutableRaceBlobStore(""), get: func(context.Context, string) (io.ReadCloser, error) {
			if failureAt == "get" {
				return reader, failure
			}
			return reader, nil
		}}
		if _, err := ReadAttemptObjectTo(t.Context(), store, attemptObjectTestBounds(), "records", attemptObjectTestHash(data), io.Discard); !errors.Is(err, failure) || reader.closed != 1 {
			t.Fatalf("%s failure was lost: error=%v closes=%d", failureAt, err, reader.closed)
		}
	}
}

// A same-sized corruption, missing byte, extra byte and typed-limit overflow
// all remain errors; exact content at the configured bound still needs EOF.
func TestAttemptObjectReadRejectsCorruptionTruncationAndExcess(t *testing.T) {
	t.Parallel()
	data := []byte("original")
	hash := attemptObjectTestHash(data)
	for _, changed := range [][]byte{[]byte("modified"), []byte("origina"), []byte("originalx"), bytes.Repeat([]byte("x"), 257)} {
		store := newImmutableRaceBlobStore("")
		key, _ := AttemptObjectKey(store, "records", hash)
		store.objects[key] = changed
		if _, err := ReadAttemptObjectTo(t.Context(), store, attemptObjectTestBounds(), "records", hash, io.Discard); err == nil {
			t.Fatalf("corrupt bytes accepted: length=%d", len(changed))
		}
	}
	store := newImmutableRaceBlobStore("")
	bounds := AttemptObjectBounds{MetadataBytes: 8, RecordBytes: 8, ProofBytes: 8}
	if err := PublishAttemptObject(t.Context(), store, bounds, "records", hash, data); err != nil {
		t.Fatalf("exact bounded EOF rejected: %v", err)
	}
}

// Cancellation at actual read/close boundaries cannot return a successful
// object; pre-cancellation has no storage effects at all.
func TestAttemptObjectCancellationOwnsReadAndClose(t *testing.T) {
	t.Parallel()
	data := []byte("original")
	for _, cancelAt := range []string{"before", "read", "close"} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		reader := &attemptObjectTestReader{read: bytes.NewReader(data).Read}
		switch cancelAt {
		case "before":
			cancel()
		case "read":
			reader.read = func(value []byte) (int, error) { cancel(); return copy(value, data), io.EOF }
		case "close":
			reader.close = func() error { cancel(); return nil }
		}
		store := &attemptObjectTestStore{BlobStore: newImmutableRaceBlobStore(""), get: func(context.Context, string) (io.ReadCloser, error) {
			calls++
			return reader, nil
		}}
		_, err := ReadAttemptObjectTo(ctx, store, attemptObjectTestBounds(), "records", attemptObjectTestHash(data), io.Discard)
		cancel()
		if !errors.Is(err, context.Canceled) || (cancelAt == "before" && calls != 0) || (cancelAt != "before" && reader.closed != 1) {
			t.Fatalf("%s cancellation lost: error=%v calls=%d closes=%d", cancelAt, err, calls, reader.closed)
		}
	}
}

// A backend returning no progress has a finite failure path independent of
// scheduler luck, and bounded copies never request a whole large object.
func TestAttemptObjectReadBoundsMemoryAndNoProgress(t *testing.T) {
	t.Parallel()
	reads, maximumRead := 0, 0
	reader := &attemptObjectTestReader{read: func(value []byte) (int, error) {
		reads++
		maximumRead = max(maximumRead, len(value))
		return 0, nil
	}}
	store := &attemptObjectTestStore{BlobStore: newImmutableRaceBlobStore(""), get: func(context.Context, string) (io.ReadCloser, error) { return reader, nil }}
	bounds := AttemptObjectBounds{MetadataBytes: 128, RecordBytes: 1024 * 1024, ProofBytes: 128}
	_, err := ReadAttemptObjectTo(t.Context(), store, bounds, "records", attemptObjectTestHash([]byte("one")), io.Discard)
	if !errors.Is(err, io.ErrNoProgress) || reads != 100 || maximumRead != 32*1024 || reader.closed != 1 {
		t.Fatalf("unbounded/no-progress read: error=%v reads=%d maximum=%d closes=%d", err, reads, maximumRead, reader.closed)
	}
}

// The upload sees a closed private file with exact bytes and media type;
// cancellation after creation still fails and temporary ownership is released.
func TestAttemptObjectPublicationOwnsPrivateTemporaryFile(t *testing.T) {
	t.Parallel()
	data := []byte("proof\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	filePath := ""
	store := &attemptObjectTestStore{BlobStore: newImmutableRaceBlobStore(""), put: func(_ context.Context, _ string, candidatePath, contentType string) (bool, error) {
		filePath = candidatePath
		info, err := os.Stat(candidatePath)
		if err != nil || info.Mode().Perm() != 0o600 || contentType != "application/x-ndjson" {
			return false, errors.New("upload lost private typed file ownership")
		}
		actual, err := os.ReadFile(candidatePath)
		if err != nil || !bytes.Equal(actual, data) {
			return false, errors.New("upload bytes changed")
		}
		cancel()
		return true, nil
	}}
	err := PublishAttemptObject(ctx, store, attemptObjectTestBounds(), "proofs", attemptObjectTestHash(data), data)
	_, statErr := os.Stat(filePath)
	if !errors.Is(err, context.Canceled) || filePath == "" || !os.IsNotExist(statErr) {
		t.Fatalf("canceled upload lost ownership: error=%v file=%q stat=%v", err, filePath, statErr)
	}
}
