package startifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server/v2026"
)

func testEvidence(t *testing.T) *EvidenceEnvelope {
	t.Helper()
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	e := &EvidenceEnvelope{DeploymentID: "test-deployment", ChainID: 945, GenesisHash: "0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105", Netuid: 7, Kind: "scenario-result", RunID: "run-1", CreatedAt: time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339Nano), Payload: json.RawMessage(`{"result":"pass"}`)}
	if err := SignEvidence(e, key); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEvidenceSignVerifyTamperAndHistory(t *testing.T) {
	e := testEvidence(t)
	if err := VerifyEvidence(e); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store := server.NewLocalBlobStore(root, "blob")
	published, err := PublishEvidence(context.Background(), store, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(published.ContentKey))); err != nil {
		t.Fatal(err)
	}
	prefix, err := EvidenceHistoryPrefix(store, e.DeploymentID, e.Netuid, e.Kind)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), prefix)
	if err != nil || len(objects) != 1 || objects[0].Key != published.HistoryKey {
		t.Fatalf("history = %+v, %v", objects, err)
	}
	if _, err := PublishEvidence(context.Background(), store, e); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	e.Payload = json.RawMessage(`{"result":"fail"}`)
	if err := VerifyEvidence(e); err == nil {
		t.Fatal("tampered evidence verified")
	}
}

func TestEvidenceRejectsUnsafeHistorySegments(t *testing.T) {
	e := testEvidence(t)
	e.RunID = "../escape"
	if err := VerifyEvidence(e); err == nil {
		t.Fatal("unsafe run id accepted")
	}
}
