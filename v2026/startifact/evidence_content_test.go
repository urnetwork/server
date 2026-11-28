// The sharded evidence content layout: where new content is written, that the
// shards come from the signed content hash, and that content written under
// the legacy flat key stays readable through every reader. Real local stores,
// and a synthetic S3 endpoint reached through the actual MinIO client, whose
// GetObject reports a missing key only at the first read. Every key, name and
// endpoint is synthetic.
package startifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Writes content at key through the store's unconditional Put, the way an
// object written before the sharded layout sits under its legacy flat key.
func putEvidenceContentTest(t *testing.T, store server.BlobStore, key string, content []byte) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "content.json")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), key, source, "application/json"); err != nil {
		t.Fatal(err)
	}
}

// Reads a reader to its end and closes it, joining both errors.
func readEvidenceContentTest(reader io.ReadCloser) ([]byte, error) {
	content, readErr := io.ReadAll(reader)
	return content, errors.Join(readErr, reader.Close())
}

// A synthetic S3 endpoint holding objects by bucket path. A GET of a missing
// object answers 404 NoSuchKey, and a conditional PUT of a taken key answers
// 412, as MinIO does. Records every request's method and path.
type evidenceContentS3Test struct {
	server *httptest.Server

	stateLock sync.Mutex
	objects   map[string][]byte
	requests  []string
}

func newEvidenceContentS3Test(t *testing.T) *evidenceContentS3Test {
	t.Helper()
	fake := &evidenceContentS3Test{objects: map[string][]byte{}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

// A MinIO-backed store of bucket "synthetic" under prefix "blob".
func (self *evidenceContentS3Test) store(t *testing.T) server.BlobStore {
	t.Helper()
	store, err := server.NewBlobStore(&server.BlobStoreConfig{Authority: strings.TrimPrefix(self.server.URL, "http://"), Bucket: "synthetic", Prefix: "blob", AccessKey: "synthetic-access", SecretKey: "synthetic-secret", Region: server.DefaultBlobRegion})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (self *evidenceContentS3Test) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, r.Method+" "+path)
	}()
	var reader io.Reader = r.Body
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		reader = httputil.NewChunkedReader(r.Body)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, "synthetic body read failed", http.StatusBadRequest)
		return
	}
	writeXml := func(status int, content string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, content)
	}
	switch r.Method {
	case http.MethodPut:
		created := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if _, ok := self.objects[path]; ok && r.Header.Get("If-None-Match") == "*" {
				return false
			}
			self.objects[path] = body
			return true
		}()
		if !created {
			writeXml(http.StatusPreconditionFailed, `<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
			return
		}
		w.Header().Set("ETag", `"synthetic"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		content, ok := self.object(path)
		if !ok {
			writeXml(http.StatusNotFound, `<Error><Code>NoSuchKey</Code><Message>none</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"synthetic"`)
		_, _ = w.Write(content)
	default:
		writeXml(http.StatusBadRequest, `<Error><Code>InvalidRequest</Code><Message>unexpected</Message></Error>`)
	}
}

func (self *evidenceContentS3Test) object(path string) ([]byte, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[path]
	return content, ok
}

// Places an object directly, without a request.
func (self *evidenceContentS3Test) seed(path string, content []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.objects[path] = bytes.Clone(content)
}

func (self *evidenceContentS3Test) requestLog() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.requests...)
}

// A store whose first read of one key answers fault instead of the object.
type evidenceContentFaultStoreTest struct {
	*preparedEvidenceStoreTest
	faultKey string
	fault    func(value []byte) (int, error)
	opened   []string
}

func (self *evidenceContentFaultStoreTest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	self.opened = append(self.opened, key)
	reader, err := self.preparedEvidenceStoreTest.Get(ctx, key)
	if err != nil || key != self.faultKey {
		return reader, err
	}
	return &evidenceContentFaultReaderTest{ReadCloser: reader, fault: self.fault}, nil
}

type evidenceContentFaultReaderTest struct {
	io.ReadCloser
	fault func(value []byte) (int, error)
}

func (self *evidenceContentFaultReaderTest) Read(value []byte) (int, error) {
	return self.fault(value)
}

// New content is written two hex levels deep under its own leading hex. The
// hash is accepted with or without its algorithm prefix, in any case and with
// surrounding space, and anything but 32 bytes of hex is refused.
func TestEvidenceContentKeyShardsUnderTwoHexLevels(t *testing.T) {
	store := server.NewLocalBlobStore(t.TempDir(), "blob/operator-1")
	h := "ab12" + strings.Repeat("cd", 30)
	wantSharded := "blob/operator-1/st/v1/evidence/content/sha256/ab/12/" + h + ".json"
	wantLegacy := "blob/operator-1/st/v1/evidence/content/sha256/" + h + ".json"
	for _, hash := range []string{"sha256:" + h, h, " SHA256:" + strings.ToUpper(h) + " "} {
		sharded, err := EvidenceContentKey(store, hash)
		if err != nil || sharded != wantSharded {
			t.Fatalf("content key for %q = %q, %v; want %q", hash, sharded, err, wantSharded)
		}
		legacy, err := evidenceLegacyContentKey(store, hash)
		if err != nil || legacy != wantLegacy {
			t.Fatalf("legacy content key for %q = %q, %v; want %q", hash, legacy, err, wantLegacy)
		}
		suffixes, err := EvidenceContentKeySuffixes(hash)
		if err != nil || len(suffixes) != 2 || "blob/operator-1/"+suffixes[0] != wantSharded || "blob/operator-1/"+suffixes[1] != wantLegacy {
			t.Fatalf("content key suffixes for %q = %q, %v", hash, suffixes, err)
		}
	}
	for _, hash := range []string{"", "sha256:", "sha256:" + h[:62], "sha256:" + h + "00", "sha256:" + h[:63] + "g", "sha512:" + h} {
		if key, err := EvidenceContentKey(store, hash); err == nil {
			t.Errorf("invalid content hash %q produced content key %q", hash, key)
		}
		if key, err := evidenceLegacyContentKey(store, hash); err == nil {
			t.Errorf("invalid content hash %q produced legacy content key %q", hash, key)
		}
		if suffixes, err := EvidenceContentKeySuffixes(hash); err == nil {
			t.Errorf("invalid content hash %q produced content key suffixes %q", hash, suffixes)
		}
	}
	if key, err := EvidenceContentKey(nil, "sha256:"+h); err == nil {
		t.Fatalf("a missing store produced content key %q", key)
	}
}

// The shards are the first two bytes of the content hash the envelope signs:
// the sha256 of its unsigned canonical Json, computed here independently. The
// published object is at exactly that path on disk.
func TestEvidenceContentKeyShardsByTheSignedContentHash(t *testing.T) {
	envelope := testEvidence(t)
	unsigned, err := evidenceUnsignedBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(unsigned)
	h := hex.EncodeToString(digest[:])
	if envelope.ContentHash != "sha256:"+h {
		t.Fatalf("content hash %s is not the sha256 of the unsigned envelope %s", envelope.ContentHash, h)
	}
	root := t.TempDir()
	store := server.NewLocalBlobStore(root, "blob")
	published, err := PublishEvidence(t.Context(), store, envelope)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("blob/st/v1/evidence/content/sha256/%x/%x/%s.json", digest[0:1], digest[1:2], h)
	if published.ContentKey != want {
		t.Fatalf("content key = %q, want %q", published.ContentKey, want)
	}
	stored, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(want)))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EvidenceBytes(envelope)
	if err != nil || !bytes.Equal(stored, encoded) {
		t.Fatalf("the object at the sharded path is not the envelope's canonical bytes: %v", err)
	}
}

// A publication writes the sharded content key and the unchanged flat history
// key, and nothing under the legacy flat content directory but the first
// shard directory. A second publication of the same evidence finds both
// routes taken and still writes nothing flat.
func TestEvidencePublicationWritesOnlyTheShardedContentKey(t *testing.T) {
	envelope := testEvidence(t)
	h := strings.TrimPrefix(envelope.ContentHash, "sha256:")
	root := t.TempDir()
	store := server.NewLocalBlobStore(root, "blob")
	for range 2 {
		published, err := PublishEvidence(t.Context(), store, envelope)
		if err != nil {
			t.Fatal(err)
		}
		wantContent, err := EvidenceContentKey(store, envelope.ContentHash)
		if err != nil || published.ContentKey != wantContent {
			t.Fatalf("content key = %q, want %q, %v", published.ContentKey, wantContent, err)
		}
		historyPrefix, err := EvidenceHistoryRunPrefix(store, envelope.DeploymentID, envelope.Netuid, envelope.Kind, envelope.RunID)
		if err != nil || published.HistoryKey != historyPrefix+h+".json" {
			t.Fatalf("history key = %q, want %q, %v", published.HistoryKey, historyPrefix+h+".json", err)
		}
	}
	legacy, err := evidenceLegacyContentKey(store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(legacy))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the legacy flat content key was written: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(filepath.Dir(legacy))))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() || entries[0].Name() != h[0:2] {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the flat content directory holds %q, want only the shard directory %q", names, h[0:2])
	}
}

// Content written before the sharded layout is read at its legacy flat key
// once the sharded key holds no object. A local store reports the missing
// sharded object at Get; the reader is the legacy object's exact bytes.
func TestOpenEvidenceContentReadsLegacyFlatObject(t *testing.T) {
	envelope := testEvidence(t)
	encoded, err := EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}
	legacy, err := evidenceLegacyContentKey(store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	putEvidenceContentTest(t, store.BlobStore, legacy, encoded)
	reader, err := OpenEvidenceContent(t.Context(), store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	content, err := readEvidenceContentTest(reader)
	if err != nil || !bytes.Equal(content, encoded) {
		t.Fatalf("legacy content was not read exactly: %v", err)
	}
	if store.gets != 2 || store.closes != 1 {
		t.Fatalf("gets=%d closes=%d, want the sharded miss and one legacy read", store.gets, store.closes)
	}
}

// Through the actual MinIO client, whose GetObject succeeds for a missing key
// and reports NoSuchKey at the first read: legacy content is still found, and
// sharded content is read with its first byte replayed exactly.
func TestOpenEvidenceContentReadsBothLayoutsBehindLazyNotFound(t *testing.T) {
	fake := newEvidenceContentS3Test(t)
	store := fake.store(t)
	legacyEnvelope := testEvidence(t)
	shardedEnvelope := preparedEvidenceEnvelopeTest(t, "sharded")
	legacyEncoded, err := EvidenceBytes(legacyEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := evidenceLegacyContentKey(store, legacyEnvelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	fake.seed("synthetic/"+legacy, legacyEncoded)
	shardedPrepared, err := PrepareEvidence(shardedEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	published, err := shardedPrepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.object("synthetic/" + published.ContentKey); !ok {
		t.Fatal("the publication did not write its sharded key")
	}
	shardedLegacy, err := evidenceLegacyContentKey(store, shardedEnvelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.object("synthetic/" + shardedLegacy); ok {
		t.Fatal("the publication wrote the legacy flat key")
	}
	sharded, err := EvidenceContentKey(store, legacyEnvelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	before := len(fake.requestLog())
	reader, err := OpenEvidenceContent(t.Context(), store, legacyEnvelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	content, err := readEvidenceContentTest(reader)
	if err != nil || !bytes.Equal(content, legacyEncoded) {
		t.Fatalf("legacy content behind a lazy miss was not read exactly: %v", err)
	}
	requests := fake.requestLog()[before:]
	if len(requests) != 2 || requests[0] != "GET synthetic/"+sharded || requests[1] != "GET synthetic/"+legacy {
		t.Fatalf("requests = %q, want the sharded GET then the legacy GET", requests)
	}
	before = len(fake.requestLog())
	reader, err = OpenEvidenceContent(t.Context(), store, shardedEnvelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	content, err = readEvidenceContentTest(reader)
	if err != nil || !bytes.Equal(content, shardedPrepared.encoded) {
		t.Fatalf("sharded content was not read exactly: %v", err)
	}
	if requests := fake.requestLog()[before:]; len(requests) != 1 || requests[0] != "GET synthetic/"+published.ContentKey {
		t.Fatalf("requests = %q, want one sharded GET", requests)
	}
}

// When both keys hold an object the sharded one is the answer and the legacy
// key is never opened. The objects differ here only to tell them apart.
func TestOpenEvidenceContentPrefersTheShardedObject(t *testing.T) {
	hash := "sha256:" + strings.Repeat("5e", 32)
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}
	sharded, err := EvidenceContentKey(store, hash)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := evidenceLegacyContentKey(store, hash)
	if err != nil {
		t.Fatal(err)
	}
	putEvidenceContentTest(t, store.BlobStore, sharded, []byte(`{"layout":"sharded"}`))
	putEvidenceContentTest(t, store.BlobStore, legacy, []byte(`{"layout":"legacy"}`))
	reader, err := OpenEvidenceContent(t.Context(), store, hash)
	if err != nil {
		t.Fatal(err)
	}
	content, err := readEvidenceContentTest(reader)
	if err != nil || string(content) != `{"layout":"sharded"}` {
		t.Fatalf("content = %q, %v; want the sharded object", content, err)
	}
	if store.gets != 1 || store.closes != 1 {
		t.Fatalf("gets=%d closes=%d, want only the sharded read", store.gets, store.closes)
	}
}

// A sharded object whose first read fails other than by being absent is the
// answer: its failure reaches the caller and the legacy key is never opened.
// So is a reader that makes no progress, after a bounded number of reads.
func TestOpenEvidenceContentKeepsShardedReadFailures(t *testing.T) {
	hash := "sha256:" + strings.Repeat("6f", 32)
	sentinel := errors.New("synthetic sharded read failure")
	emptyReads := 0
	for _, test := range []struct {
		fault func(value []byte) (int, error)
		want  error
	}{
		{fault: func(value []byte) (int, error) { return 0, sentinel }, want: sentinel},
		{fault: func(value []byte) (int, error) { emptyReads++; return 0, nil }, want: io.ErrNoProgress},
	} {
		store := &evidenceContentFaultStoreTest{preparedEvidenceStoreTest: &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}, fault: test.fault}
		sharded, err := EvidenceContentKey(store, hash)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := evidenceLegacyContentKey(store, hash)
		if err != nil {
			t.Fatal(err)
		}
		store.faultKey = sharded
		putEvidenceContentTest(t, store.BlobStore, sharded, []byte(`{"layout":"sharded"}`))
		putEvidenceContentTest(t, store.BlobStore, legacy, []byte(`{"layout":"legacy"}`))
		reader, err := OpenEvidenceContent(t.Context(), store, hash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readEvidenceContentTest(reader); !errors.Is(err, test.want) {
			t.Fatalf("read error = %v, want %v", err, test.want)
		}
		if len(store.opened) != 1 || store.opened[0] != sharded || store.closes != 1 {
			t.Fatalf("opened %q with %d closes, want only the sharded key, closed once", store.opened, store.closes)
		}
	}
	if emptyReads != 100 {
		t.Fatalf("%d empty reads before refusal, want 100", emptyReads)
	}
}

// Content stored under neither key is reported exactly as one Get of its
// legacy key reports it: at Get by a local store, at the first read through
// MinIO. Both keys are tried once and the sharded miss is closed.
func TestOpenEvidenceContentReportsAbsentContentLikeOneGet(t *testing.T) {
	hash := "sha256:" + strings.Repeat("7a", 32)
	local := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}
	if reader, err := OpenEvidenceContent(t.Context(), local, hash); !server.IsBlobNotFound(err) || reader != nil || local.gets != 2 {
		t.Fatalf("absent local content: reader=%v err=%v gets=%d", reader, err, local.gets)
	}
	fake := newEvidenceContentS3Test(t)
	store := fake.store(t)
	reader, err := OpenEvidenceContent(t.Context(), store, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readEvidenceContentTest(reader); !server.IsBlobNotFound(err) {
		t.Fatalf("absent remote content read error = %v, want NoSuchKey", err)
	}
	sharded, err := EvidenceContentKey(store, hash)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := evidenceLegacyContentKey(store, hash)
	if err != nil {
		t.Fatal(err)
	}
	if requests := fake.requestLog(); len(requests) != 2 || requests[0] != "GET synthetic/"+sharded || requests[1] != "GET synthetic/"+legacy {
		t.Fatalf("requests = %q, want one GET of each key", requests)
	}
	if _, err := OpenEvidenceContent(t.Context(), store, "sha256:short"); err == nil || len(fake.requestLog()) != 2 {
		t.Fatal("an invalid content hash reached the store")
	}
}

// Verify-first client-key publication accepts identical bytes at the legacy
// flat key: no write, no sharded copy, one read of each key. Through the
// actual MinIO client, so the sharded miss surfaces at the first read.
func TestPreparedClientKeyEvidenceVerifyFirstAcceptsLegacyFlatObject(t *testing.T) {
	encoded, contentHash := sealedClientKeyRegistrationTest(t, 5)
	evidence, err := PrepareClientKeyEvidence(encoded, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	fake := newEvidenceContentS3Test(t)
	store := fake.store(t)
	legacy, err := evidenceLegacyContentKey(store, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	sharded, err := evidence.ContentKey(store)
	if err != nil {
		t.Fatal(err)
	}
	fake.seed("synthetic/"+legacy, encoded)
	created, err := evidence.VerifyOrPublish(t.Context(), store)
	if err != nil || created {
		t.Fatalf("legacy evidence: created=%t err=%v", created, err)
	}
	if requests := fake.requestLog(); len(requests) != 2 || requests[0] != "GET synthetic/"+sharded || requests[1] != "GET synthetic/"+legacy {
		t.Fatalf("requests = %q, want the sharded GET then the legacy GET and no PUT", requests)
	}
	if _, ok := fake.object("synthetic/" + sharded); ok {
		t.Fatal("verified legacy evidence was copied to its sharded key")
	}
	// absent evidence is still created at its sharded key alone
	other, otherHash := sealedClientKeyRegistrationTest(t, 6)
	otherEvidence, err := PrepareClientKeyEvidence(other, otherHash)
	if err != nil {
		t.Fatal(err)
	}
	created, err = otherEvidence.VerifyOrPublish(t.Context(), store)
	if err != nil || !created {
		t.Fatalf("absent evidence: created=%t err=%v", created, err)
	}
	otherSharded, err := otherEvidence.ContentKey(store)
	if err != nil {
		t.Fatal(err)
	}
	otherLegacy, err := evidenceLegacyContentKey(store, otherHash)
	if err != nil {
		t.Fatal(err)
	}
	if content, ok := fake.object("synthetic/" + otherSharded); !ok || !bytes.Equal(content, other) {
		t.Fatal("absent evidence was not created at its sharded key")
	}
	if _, ok := fake.object("synthetic/" + otherLegacy); ok {
		t.Fatal("absent evidence was written at its legacy flat key")
	}
}

// Other bytes at the legacy flat key are a conflict named by that key, never
// a reason to write the sharded key.
func TestPreparedClientKeyEvidenceVerifyFirstRefusesConflictingLegacyObject(t *testing.T) {
	encoded, contentHash := sealedClientKeyRegistrationTest(t, 7)
	other, _ := sealedClientKeyRegistrationTest(t, 8)
	evidence, err := PrepareClientKeyEvidence(encoded, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	store := newPreparedClientKeyStoreTest(t)
	legacy, err := evidenceLegacyContentKey(store, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	putEvidenceContentTest(t, store.BlobStore, legacy, other)
	created, err := evidence.VerifyOrPublish(t.Context(), store)
	var conflict *ImmutableConflictError
	if created || !errors.As(err, &conflict) || conflict.Key != legacy || store.puts != 0 {
		t.Fatalf("conflicting legacy object: created=%t err=%v puts=%d", created, err, store.puts)
	}
}

// A receipt may name the sharded key or the legacy flat key of its content,
// and is read at exactly the key it names. Any other content key, such as a
// single shard level, is refused before any read.
func TestPreparedEvidenceVerifyPublishedAcceptsEitherCanonicalContentKey(t *testing.T) {
	envelope := preparedEvidenceEnvelopeTest(t, "signed")
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	h := strings.TrimPrefix(envelope.ContentHash, "sha256:")
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "operator-1")}
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := evidenceLegacyContentKey(store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	legacyReceipt := *published
	legacyReceipt.ContentKey = legacy
	// the legacy object does not exist yet: the named key is read, not another
	if err := prepared.VerifyPublished(t.Context(), store, &legacyReceipt); !server.IsBlobNotFound(err) {
		t.Fatalf("a legacy receipt with no legacy object: %v", err)
	}
	putEvidenceContentTest(t, store.BlobStore, legacy, prepared.encoded)
	if err := prepared.VerifyPublished(t.Context(), store, &legacyReceipt); err != nil {
		t.Fatalf("a legacy receipt with its legacy object: %v", err)
	}
	if err := prepared.VerifyPublished(t.Context(), store, published); err != nil {
		t.Fatalf("the sharded receipt: %v", err)
	}
	for _, key := range []string{
		"operator-1/st/v1/evidence/content/sha256/" + h[0:2] + "/" + h + ".json",
		"operator-1/st/v1/evidence/content/sha256/" + h[2:4] + "/" + h[0:2] + "/" + h + ".json",
		"operator-2/st/v1/evidence/content/sha256/" + h + ".json",
	} {
		wrong := *published
		wrong.ContentKey = key
		before := store.gets
		if err := prepared.VerifyPublished(t.Context(), store, &wrong); err == nil || store.gets != before {
			t.Fatalf("receipt content key %q was accepted or read", key)
		}
	}
}

// Stored evidence is found at either layout with its history route; content
// under neither key, or a missing history route, is not stored.
func TestPreparedEvidenceVerifyStoredFindsEitherLayout(t *testing.T) {
	envelope := preparedEvidenceEnvelopeTest(t, "stored")
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := server.NewLocalBlobStore(root, "operator-1")
	if err := prepared.VerifyStored(t.Context(), store); !server.IsBlobNotFound(err) {
		t.Fatalf("unpublished evidence: %v", err)
	}
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifyStored(t.Context(), store); err != nil {
		t.Fatalf("sharded evidence: %v", err)
	}
	legacy, err := evidenceLegacyContentKey(store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	putEvidenceContentTest(t, store, legacy, prepared.encoded)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(published.ContentKey))); err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifyStored(t.Context(), store); err != nil {
		t.Fatalf("legacy evidence: %v", err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(published.HistoryKey))); err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifyStored(t.Context(), store); !server.IsBlobNotFound(err) {
		t.Fatalf("evidence without its history route: %v", err)
	}
}
