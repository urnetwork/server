package startifact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
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

func TestEvidenceReservesDeploymentHistoryRunID(t *testing.T) {
	e := testEvidence(t)
	e.RunID = EvidenceDeploymentHistoryRunID
	if err := validateEvidenceIdentity(e); err == nil {
		t.Fatal("deployment history sentinel accepted as a signed run id")
	}
	key, err := crypto.HexToECDSA("1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := SignEvidence(e, key); err == nil {
		t.Fatal("deployment history sentinel was signed as a named run")
	}
	e.RunID = "deployment"
	if err := SignEvidence(e, key); err != nil {
		t.Fatalf("legacy-valid named deployment run was rejected: %v", err)
	}
}

func TestEvidencePublishesEmptyRunUnderReservedDeploymentHistory(t *testing.T) {
	e := testEvidence(t)
	e.RunID = ""
	key, err := crypto.HexToECDSA("1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := SignEvidence(e, key); err != nil {
		t.Fatal(err)
	}
	store := server.NewLocalBlobStore(t.TempDir(), "blob")
	published, err := PublishEvidence(context.Background(), store, e)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := EvidenceHistoryRunPrefix(store, e.DeploymentID, e.Netuid, e.Kind, EvidenceDeploymentHistoryRunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(published.HistoryKey, prefix) {
		t.Fatalf("deployment history key = %q, want prefix %q", published.HistoryKey, prefix)
	}
}

func TestEvidenceHistoryRunPrefixAcceptsSignedRunGrammar(t *testing.T) {
	store := server.NewLocalBlobStore(t.TempDir(), "blob/operator-1")
	got, err := EvidenceHistoryRunPrefix(store, "test-deployment", 521, "scenario-bundle", "release.v1-attempt-4")
	want := "blob/operator-1/st/v1/evidence/history/test-deployment/521/scenario-bundle/release.v1-attempt-4/"
	if err != nil || got != want {
		t.Fatalf("run prefix = %q, %v; want %q", got, err, want)
	}
	invalidRunIDs := []string{"", "../escape", "a/b", " a", "a ", strings.Repeat("a", 129)}
	for _, runID := range invalidRunIDs {
		if prefix, err := EvidenceHistoryRunPrefix(store, "test-deployment", 521, "scenario-bundle", runID); err == nil {
			t.Errorf("unsafe run id %q produced %q", runID, prefix)
		}
	}
	if prefix, err := EvidenceHistoryRunPrefix(store, "test-deployment", 521, "", "run-1"); err == nil {
		t.Errorf("empty kind produced %q", prefix)
	}
}

func TestEvidenceHistoryKeyBindsExactRunAndContentHash(t *testing.T) {
	store := server.NewLocalBlobStore(t.TempDir(), "blob/operator-1")
	hash := "sha256:" + strings.Repeat("ab", 32)
	got, err := EvidenceHistoryKey(store, "test-deployment", 521, "scenario-bundle", "release.v1", hash)
	want := "blob/operator-1/st/v1/evidence/history/test-deployment/521/scenario-bundle/release.v1/" + strings.Repeat("ab", 32) + ".json"
	if err != nil || got != want {
		t.Fatalf("history key = %q, %v; want %q", got, err, want)
	}
	if key, err := EvidenceHistoryKey(store, "test-deployment", 521, "scenario-bundle", "release.v1", "sha256:not-hex"); err == nil {
		t.Fatalf("invalid content hash produced history key %q", key)
	}
}
