// Real immutable store operations prove detached wire ownership, unchanged
// authentication and complete direct readbacks without elapsed-time guesses.
package startifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/server"
)

// Counts actual native operations; injected read/Close errors cannot replace
// the underlying descriptor's final Close.
type preparedEvidenceStoreTest struct {
	server.BlobStore
	puts     int
	gets     int
	closes   int
	readErr  error
	closeErr error
}

// Retain the existing immutable writer and its exact staged bytes.
func (self *preparedEvidenceStoreTest) PutIfAbsent(ctx context.Context, key, localPath, contentType string) (bool, error) {
	self.puts++
	return self.BlobStore.PutIfAbsent(ctx, key, localPath, contentType)
}

// Return an owned real descriptor, including when error injection is active.
func (self *preparedEvidenceStoreTest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	self.gets++
	reader, err := self.BlobStore.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &preparedEvidenceReaderTest{ReadCloser: reader, store: self}, nil
}

// One observer owns each actual store descriptor through its final Close.
type preparedEvidenceReaderTest struct {
	io.ReadCloser
	store *preparedEvidenceStoreTest
}

// Inject a real reader failure only after the original descriptor is owned.
func (self *preparedEvidenceReaderTest) Read(value []byte) (int, error) {
	if self.store.readErr != nil {
		return 0, self.store.readErr
	}
	return self.ReadCloser.Read(value)
}

// A failed Close is still discharged, reported and never treated as success.
func (self *preparedEvidenceReaderTest) Close() error {
	self.store.closes++
	return errors.Join(self.ReadCloser.Close(), self.store.closeErr)
}

// Existing signing binds a caller-selected payload and the full run grammar.
func preparedEvidenceEnvelopeTest(t *testing.T, runId string) *EvidenceEnvelope {
	t.Helper()
	envelope := testEvidence(t)
	envelope.RunID = runId
	envelope.Payload = json.RawMessage("{ \"value\" : \"<>&\\u2028\", \"data\" : \"" + strings.Repeat("synthetic", 4096) + "\" }")
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := SignEvidence(envelope, key); err != nil {
		t.Fatal(err)
	}
	return envelope
}

// The complete old canonical encoding is the oracle. Mutating every input
// field and its backing payload after admission cannot change either replica.
func TestPreparedEvidenceDetachesCanonicalWireAndRoutingIdentity(t *testing.T) {
	envelope := preparedEvidenceEnvelopeTest(t, "wire.owner-v1")
	want, err := EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.identity.Payload) != 0 || !bytes.Equal(prepared.encoded, want) {
		t.Fatal("prepared owner retained a duplicate payload or changed canonical wire")
	}
	wireStart := &prepared.encoded[0]
	for index := range envelope.Payload {
		envelope.Payload[index] = 'x'
	}
	*envelope = EvidenceEnvelope{Payload: envelope.Payload, DeploymentID: "changed", RunID: "changed", ContentHash: "sha256:" + strings.Repeat("ff", 32)}
	for _, prefix := range []string{"operator-1", "operator-2"} {
		store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), prefix)}
		published, err := prepared.Publish(t.Context(), store)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.VerifyPublished(t.Context(), store, published); err != nil {
			t.Fatal(err)
		}
		if store.puts != 2 || store.gets != 4 || store.closes != 4 || &prepared.encoded[0] != wireStart {
			t.Fatal("replica work changed its sealed owner or omitted a route read/Close")
		}
		for _, key := range []string{published.ContentKey, published.HistoryKey} {
			reader, err := store.BlobStore.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(reader)
			if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("stored original canonical wire changed: %v", err)
			}
		}
		if _, err := PublishEvidence(t.Context(), store, envelope); err == nil || store.puts != 2 {
			t.Fatal("legacy publication reused authentication for mutated input")
		}
	}
}

// Every untrusted input still traverses the original schema, content-hash and
// signature validator; no owner or store write is returned on refusal.
func TestPreparedEvidenceRejectsUnauthenticatedInputs(t *testing.T) {
	for _, mutate := range []func(*EvidenceEnvelope){
		func(value *EvidenceEnvelope) { value.Schema = "wrong" },
		func(value *EvidenceEnvelope) { value.RunID = "../escape" },
		func(value *EvidenceEnvelope) { value.Payload = json.RawMessage("{") },
		func(value *EvidenceEnvelope) { value.Payload = json.RawMessage(`{"different":true}`) },
		func(value *EvidenceEnvelope) { value.ContentHash = "sha256:" + strings.Repeat("00", 32) },
		func(value *EvidenceEnvelope) { value.Signature = "0x" + strings.Repeat("00", crypto.SignatureLength) },
	} {
		envelope := preparedEvidenceEnvelopeTest(t, "signed")
		mutate(envelope)
		if prepared, err := PrepareEvidence(envelope); prepared != nil || err == nil {
			t.Fatal("unauthenticated input acquired a prepared wire owner")
		}
		store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "operator-1")}
		if _, err := PublishEvidence(t.Context(), store, envelope); err == nil || store.puts != 0 || store.gets != 0 {
			t.Fatal("legacy publisher bypassed fresh envelope authentication")
		}
	}
	if prepared, err := PrepareEvidence(nil); prepared != nil || err == nil {
		t.Fatal("nil envelope acquired a prepared owner")
	}
}

// Prefix, signed content identity, both exact routes and the final byte remain
// independent checks even though the verified carrier is prepared only once.
func TestPreparedEvidenceRejectsCrossReceiptAndChangedReplicaBytes(t *testing.T) {
	envelope := preparedEvidenceEnvelopeTest(t, "signed")
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(root, "operator-1")}
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Published){
		func(value *Published) { value.Bucket = "different" },
		func(value *Published) { value.ContentHash = "sha256:" + strings.Repeat("00", 32) },
		func(value *Published) {
			value.ContentKey = strings.Replace(value.ContentKey, "operator-1/", "operator-2/", 1)
		},
		func(value *Published) {
			value.HistoryKey = strings.Replace(value.HistoryKey, "/signed/", "/another/", 1)
		},
	} {
		wrong := *published
		mutate(&wrong)
		before := store.gets
		if err := prepared.VerifyPublished(t.Context(), store, &wrong); err == nil || store.gets != before {
			t.Fatal("foreign receipt reached a stored body")
		}
	}
	other, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "another"))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.VerifyPublished(t.Context(), store, published); err == nil {
		t.Fatal("one envelope's prepared wire accepted another envelope's receipt")
	}
	sameLength := bytes.Clone(prepared.encoded)
	sameLength[len(sameLength)/2] ^= 1
	for _, key := range []string{published.ContentKey, published.HistoryKey} {
		for _, changed := range [][]byte{prepared.encoded[:len(prepared.encoded)-1], append(bytes.Clone(prepared.encoded), ' '), sameLength, []byte(`{}`)} {
			path := filepath.Join(root, filepath.FromSlash(key))
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := prepared.VerifyPublished(t.Context(), store, published); err == nil || !strings.Contains(err.Error(), "differs") || store.gets != store.closes {
				t.Fatalf("changed complete replica was accepted or leaked a reader: %v", err)
			}
			if err := os.WriteFile(path, prepared.encoded, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := prepared.VerifyPublished(t.Context(), store, published); err != nil {
		t.Fatal(err)
	}
}

// Cancellation refuses before storage; read and Close failures both remain
// terminal and all acquired original descriptors are discharged.
func TestPreparedEvidencePreservesCancellationAndReaderFailures(t *testing.T) {
	prepared, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "signed"))
	if err != nil {
		t.Fatal(err)
	}
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "operator-1")}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := prepared.Publish(cancelled, store); !errors.Is(err, context.Canceled) || store.puts != 0 {
		t.Fatal("cancelled publication reached storage")
	}
	if err := prepared.VerifyPublished(cancelled, store, nil); !errors.Is(err, context.Canceled) || store.gets != 0 {
		t.Fatal("cancelled verifier reached storage")
	}
	if _, err := (&PreparedEvidence{}).Publish(t.Context(), store); err == nil || store.puts != 0 {
		t.Fatal("unprepared zero value reached storage")
	}
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("synthetic owned reader failure")
	for _, closeFailure := range []bool{false, true} {
		store.readErr, store.closeErr = nil, nil
		if closeFailure {
			store.closeErr = sentinel
		} else {
			store.readErr = sentinel
		}
		if err := prepared.VerifyPublished(t.Context(), store, published); !errors.Is(err, sentinel) || store.gets != store.closes {
			t.Fatalf("owned reader failure escaped or leaked its descriptor: %v", err)
		}
	}
}

// The legacy entry point retains exact canonical bytes, empty-run sentinel
// separation and the existing dotted/named-deployment run grammar.
func TestPreparedEvidenceLegacyAdapterPreservesHistoryAndWire(t *testing.T) {
	for _, runId := range []string{"", "deployment", "release.v1-attempt-4"} {
		envelope := preparedEvidenceEnvelopeTest(t, runId)
		want, err := EvidenceBytes(envelope)
		if err != nil {
			t.Fatal(err)
		}
		store := server.NewLocalBlobStore(t.TempDir(), "operator-1")
		published, err := PublishEvidence(t.Context(), store, envelope)
		if err != nil {
			t.Fatal(err)
		}
		storageRunId := runId
		if storageRunId == "" {
			storageRunId = EvidenceDeploymentHistoryRunID
		}
		prefix, err := EvidenceHistoryRunPrefix(store, envelope.DeploymentID, envelope.Netuid, envelope.Kind, storageRunId)
		if err != nil || !strings.HasPrefix(published.HistoryKey, prefix) {
			t.Fatalf("history run identity changed: %v", err)
		}
		for _, key := range []string{published.ContentKey, published.HistoryKey} {
			reader, err := store.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			got, readErr := io.ReadAll(reader)
			if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("legacy wire changed: %v", err)
			}
		}
	}
}
