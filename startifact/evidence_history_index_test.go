// Client-key evidence is stored by content key only; every other kind keeps
// both routes. Real local stores count the actual writes and readbacks.
package startifact

import (
	"slices"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/server"
)

// A signed envelope of the given kind with the run-less history identity the
// client-key kinds use.
func historyIndexEvidenceTest(t *testing.T, kind string) *EvidenceEnvelope {
	t.Helper()
	envelope := testEvidence(t)
	envelope.Kind = kind
	envelope.RunID = ""
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := SignEvidence(envelope, key); err != nil {
		t.Fatal(err)
	}
	return envelope
}

// Exactly the two client-key kinds leave the history index.
func TestEvidenceHistoryIndexExcludesOnlyClientKeyKinds(t *testing.T) {
	for _, kind := range []string{ClientKeyRegistrationEvidenceKind, ClientKeyObservationEvidenceKind} {
		if EvidenceHistoryIndexed(kind) {
			t.Fatalf("%s is still history indexed", kind)
		}
	}
	for _, kind := range []string{"scenario-result", "deployment-manifest", "scenario-evidence-file", "client-key-registration-v2", ""} {
		if !EvidenceHistoryIndexed(kind) {
			t.Fatalf("%q left the history index", kind)
		}
	}
	unindexed := EvidenceHistoryUnindexedKinds()
	slices.Sort(unindexed)
	if !slices.Equal(unindexed, []string{ClientKeyObservationEvidenceKind, ClientKeyRegistrationEvidenceKind}) {
		t.Fatalf("unindexed kinds = %v", unindexed)
	}
	unindexed[0] = "mutated"
	if !slices.Contains(EvidenceHistoryUnindexedKinds(), ClientKeyObservationEvidenceKind) {
		t.Fatal("a caller mutated the shared unindexed kinds")
	}
}

// One write and its readback, one verification read, and no history object
// for either client-key kind; the receipt names no history key.
func TestClientKeyEvidencePublishesOnlyItsContentKey(t *testing.T) {
	for _, kind := range []string{ClientKeyRegistrationEvidenceKind, ClientKeyObservationEvidenceKind} {
		envelope := historyIndexEvidenceTest(t, kind)
		prepared, err := PrepareEvidence(envelope)
		if err != nil {
			t.Fatal(err)
		}
		store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}
		published, err := prepared.Publish(t.Context(), store)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.VerifyPublished(t.Context(), store, published); err != nil {
			t.Fatal(err)
		}
		if published.HistoryKey != "" {
			t.Fatalf("%s receipt named a history route: %+v", kind, published)
		}
		if store.puts != ClientKeyEvidenceStoredObjects || store.gets != 2 || store.closes != 2 {
			t.Fatalf("%s: puts=%d gets=%d closes=%d, want one write, its readback and one verification read", kind, store.puts, store.gets, store.closes)
		}
		historyKey, err := EvidenceHistoryKey(store, envelope.DeploymentID, envelope.Netuid, kind, EvidenceDeploymentHistoryRunID, envelope.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := store.BlobStore.Get(t.Context(), historyKey); err == nil {
			reader.Close()
			t.Fatalf("%s wrote its history copy", kind)
		}
		contentKey, err := EvidenceContentKey(store, envelope.ContentHash)
		if err != nil || published.ContentKey != contentKey {
			t.Fatalf("%s content route changed: %s %v", kind, published.ContentKey, err)
		}
	}
}

// An indexed kind with the same run-less identity still writes and verifies
// both routes.
func TestIndexedEvidenceKeepsItsHistoryKey(t *testing.T) {
	envelope := historyIndexEvidenceTest(t, "deployment-manifest")
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	store := &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(t.TempDir(), "blob")}
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifyPublished(t.Context(), store, published); err != nil {
		t.Fatal(err)
	}
	historyKey, err := EvidenceHistoryKey(store, envelope.DeploymentID, envelope.Netuid, envelope.Kind, EvidenceDeploymentHistoryRunID, envelope.ContentHash)
	if err != nil || published.HistoryKey != historyKey {
		t.Fatalf("indexed receipt history key = %q, want %q (%v)", published.HistoryKey, historyKey, err)
	}
	if store.puts != 2 || store.gets != 4 || store.closes != 4 {
		t.Fatalf("indexed: puts=%d gets=%d closes=%d, want both routes written, read back and verified", store.puts, store.gets, store.closes)
	}
}

// A receipt that still names a history key for a client-key kind no longer
// matches the store's routes.
func TestClientKeyEvidenceRefusesAReceiptWithAHistoryKey(t *testing.T) {
	envelope := historyIndexEvidenceTest(t, ClientKeyRegistrationEvidenceKind)
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	store := server.NewLocalBlobStore(t.TempDir(), "blob")
	published, err := prepared.Publish(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	stale := *published
	stale.HistoryKey, err = EvidenceHistoryKey(store, envelope.DeploymentID, envelope.Netuid, envelope.Kind, EvidenceDeploymentHistoryRunID, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifyPublished(t.Context(), store, &stale); err == nil {
		t.Fatal("a receipt with a history key verified for a content-only kind")
	}
}

// Replica publication writes and verifies only the content route of a
// client-key envelope at every store.
func TestClientKeyEvidenceReplicasWriteOnlyTheContentKey(t *testing.T) {
	envelope := historyIndexEvidenceTest(t, ClientKeyObservationEvidenceKind)
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*preparedEvidenceStoreTest{
		{BlobStore: server.NewLocalBlobStore(t.TempDir(), "operator-1")},
		{BlobStore: server.NewLocalBlobStore(t.TempDir(), "operator-2")},
	}
	if err := prepared.PublishAndVerifyReplicas(t.Context(), []server.BlobStore{stores[0], stores[1]}); err != nil {
		t.Fatal(err)
	}
	for index, store := range stores {
		if store.puts != ClientKeyEvidenceStoredObjects || store.gets != 2 {
			t.Fatalf("replica %d: puts=%d gets=%d, want one content write, its readback and one verification read", index, store.puts, store.gets)
		}
		historyKey, err := EvidenceHistoryKey(store, envelope.DeploymentID, envelope.Netuid, envelope.Kind, EvidenceDeploymentHistoryRunID, envelope.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := store.BlobStore.Get(t.Context(), historyKey); err == nil {
			reader.Close()
			t.Fatalf("replica %d wrote a history copy", index)
		}
	}
}
