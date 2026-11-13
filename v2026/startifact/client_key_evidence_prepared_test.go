// A prepared client-key wrapper reports whether a publication created its
// object, and its verify-first publication sends no write for stored bytes.
// Real local stores count the actual writes and reads.
package startifact

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// One genuine operator-signed registration wrapper for a synthetic client.
func sealedClientKeyRegistrationTest(t *testing.T, client byte) ([]byte, string) {
	t.Helper()
	rootKey, err := crypto.HexToECDSA(strings.Repeat("21", 32))
	if err != nil {
		t.Fatal(err)
	}
	artifactKey, err := crypto.HexToECDSA(strings.Repeat("22", 32))
	if err != nil {
		t.Fatal(err)
	}
	deployment := "synthetic-prepared"
	domain := protocol.ClientKeyHistoryDomain{ChainID: 31337, GenesisHash: [32]byte{1}, Netuid: 77, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte(deployment)), PolicyHash: [32]byte{4}, NoID: 1}
	registration := protocol.ClientKeyRegistration{Domain: domain, ClientID: [16]byte{client}, NetworkID: [16]byte{7}, Generation: 1, Present: true, PublicKey: [32]byte{8}, EffectiveBoundary: protocol.ClientKeyEffectiveBoundary{Block: 70, Hash: [32]byte{9}}}
	if err := protocol.SignClientKeyRegistration(&registration, rootKey); err != nil {
		t.Fatal(err)
	}
	encoded, contentHash, err := SealClientKeyRegistrationEvidence(deployment, registration, artifactKey, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return encoded, contentHash
}

func newPreparedClientKeyStoreTest(t *testing.T) *preparedEvidenceStoreTest {
	t.Helper()
	return &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}
}

// The first publication creates its content route; an identical one finds it
// taken, reads it back exactly and reports that it created nothing.
func TestPreparedClientKeyEvidenceReportsCreation(t *testing.T) {
	encoded, contentHash := sealedClientKeyRegistrationTest(t, 1)
	evidence, err := PrepareClientKeyEvidence(encoded, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ContentHash() != contentHash || evidence.Size() != len(encoded) {
		t.Fatal("prepared evidence names another identity or size")
	}
	store := newPreparedClientKeyStoreTest(t)
	for attempt, want := range []bool{true, false} {
		created, err := evidence.Publish(t.Context(), store)
		if err != nil || created != want {
			t.Fatalf("publication %d created=%t err=%v, want created=%t", attempt, created, err, want)
		}
		if store.puts != attempt+1 || store.gets != attempt+1 || store.closes != attempt+1 {
			t.Fatalf("publication %d: puts=%d gets=%d closes=%d", attempt, store.puts, store.gets, store.closes)
		}
	}
	key, err := evidence.ContentKey(store)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := EvidenceContentKey(store, contentHash); key != want {
		t.Fatalf("content key %s, want %s", key, want)
	}
}

// Stored identical bytes cost one read and no write; absent evidence is read,
// created and read back; other bytes under the key are a conflict, not a write.
func TestPreparedClientKeyEvidenceVerifyFirstWritesOnlyWhenAbsent(t *testing.T) {
	encoded, contentHash := sealedClientKeyRegistrationTest(t, 2)
	evidence, err := PrepareClientKeyEvidence(encoded, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	store := newPreparedClientKeyStoreTest(t)
	created, err := evidence.VerifyOrPublish(t.Context(), store)
	if err != nil || !created || store.puts != 1 || store.gets != 2 {
		t.Fatalf("absent evidence: created=%t err=%v puts=%d gets=%d", created, err, store.puts, store.gets)
	}
	created, err = evidence.VerifyOrPublish(t.Context(), store)
	if err != nil || created || store.puts != 1 || store.gets != 3 || store.closes != 2 {
		t.Fatalf("stored evidence: created=%t err=%v puts=%d gets=%d closes=%d", created, err, store.puts, store.gets, store.closes)
	}

	other, otherHash := sealedClientKeyRegistrationTest(t, 3)
	otherEvidence, err := PrepareClientKeyEvidence(other, otherHash)
	if err != nil {
		t.Fatal(err)
	}
	conflicting := newPreparedClientKeyStoreTest(t)
	key, err := otherEvidence.ContentKey(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(source, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := conflicting.BlobStore.Put(t.Context(), key, source, "application/json"); err != nil {
		t.Fatal(err)
	}
	created, err = otherEvidence.VerifyOrPublish(t.Context(), conflicting)
	var conflict *ImmutableConflictError
	if created || !errors.As(err, &conflict) || conflict.Key != key || conflicting.puts != 0 {
		t.Fatalf("conflicting object: created=%t err=%v puts=%d", created, err, conflicting.puts)
	}
	// the conditional create reports the same typed conflict
	if created, err := otherEvidence.Publish(t.Context(), conflicting); created || !errors.As(err, &conflict) {
		t.Fatalf("conflicting create: created=%t err=%v", created, err)
	}
}

// Preparation applies the complete wrapper checks before any storage.
func TestPrepareClientKeyEvidenceRefusesForeignWrappers(t *testing.T) {
	encoded, contentHash := sealedClientKeyRegistrationTest(t, 4)
	for _, test := range []struct {
		name        string
		encoded     []byte
		contentHash string
	}{
		{name: "another content hash", encoded: encoded, contentHash: "sha256:" + strings.Repeat("ab", 32)},
		{name: "empty", encoded: nil, contentHash: contentHash},
		{name: "oversize", encoded: make([]byte, MaxClientKeyEvidenceBytes+1), contentHash: contentHash},
		{name: "noncanonical", encoded: append([]byte(" "), encoded...), contentHash: contentHash},
	} {
		if evidence, err := PrepareClientKeyEvidence(test.encoded, test.contentHash); err == nil || evidence != nil {
			t.Fatalf("%s wrapper was prepared", test.name)
		}
	}
	scenario := historyIndexEvidenceTest(t, "scenario-result")
	scenarioBytes, err := EvidenceBytes(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareClientKeyEvidence(scenarioBytes, scenario.ContentHash); err == nil {
		t.Fatal("a wrapper of an unrelated kind was prepared as client-key evidence")
	}
	if _, err := PublishClientKeyEvidence(t.Context(), nil, encoded, contentHash); err == nil {
		t.Fatal("publication without a store was admitted")
	}
}
