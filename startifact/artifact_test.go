package startifact

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
)

func testArtifact(t *testing.T) *Artifact {
	t.Helper()
	providers := []ProviderInput{
		{ClientID: [16]byte{2}, NetworkID: [16]byte{20}, Coldkey: [32]byte{2}, UsageBytes: 100, Assignments: 10, Confirmations: 10, Eligible: true},
		{ClientID: [16]byte{1}, NetworkID: [16]byte{10}, Coldkey: [32]byte{1}, UsageBytes: 100, Assignments: 10, Confirmations: 5, Eligible: true},
		{ClientID: [16]byte{3}, NetworkID: [16]byte{30}, Coldkey: [32]byte{3}, UsageBytes: 900, Assignments: 10, Confirmations: 10, Eligible: true, HeadExcluded: true, ExclusionReason: "active_fleet_binding"},
	}
	a, err := Build(BuildInput{DeploymentID: "test-deployment", GenesisHash: "0x" + strings.Repeat("ab", 32), PolicyHash: "0x" + strings.Repeat("cd", 32), ChainID: 945, Netuid: 7, Coordinator: common.HexToAddress("0x100"), SettlementVault: common.HexToAddress("0x200"), Epoch: 4, NoID: 1, Start: Boundary{Number: 100, Hash: "0x" + strings.Repeat("01", 32)}, End: Boundary{Number: 200, Hash: "0x" + strings.Repeat("02", 32)}, OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32), Providers: providers, ReliabilityAMin: 8, CreatedAt: time.Unix(1_700_000_000, 123).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := Sign(a, key); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBuildSignVerifyAndCanonicalRoundTrip(t *testing.T) {
	a := testArtifact(t)
	if err := Verify(a); err != nil {
		t.Fatal(err)
	}
	if len(a.Leaves) != 2 || a.SharesTotalBPS != 10_000 || a.ExcludedUsageBytes != 900 {
		t.Fatalf("unexpected artifact summary: %+v", a)
	}
	b, err := Bytes(a)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Artifact
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := Verify(&decoded); err != nil {
		t.Fatalf("round-trip verify: %v", err)
	}
	decoded.Leaves[0].ShareBPS++
	if err := Verify(&decoded); err == nil {
		t.Fatal("tampered artifact verified")
	}
	malicious := testArtifact(t)
	malicious.TotalUsageBytes++
	if err := Sign(malicious, mustArtifactKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := Verify(malicious); err == nil {
		t.Fatal("self-consistent signature hid a false usage total")
	}
}

func mustArtifactKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestPublishIsContentAddressedAndImmutable(t *testing.T) {
	a := testArtifact(t)
	root := t.TempDir()
	store := server.NewLocalBlobStore(root, "blob")
	published, err := Publish(context.Background(), store, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(published.ContentKey))); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), store, a); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	contentPath := filepath.Join(root, filepath.FromSlash(published.ContentKey))
	if err := os.WriteFile(contentPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), store, a); err == nil {
		t.Fatal("publish overwrote different immutable bytes")
	}
}

func TestReadRejectsAlternateBytesForOneContentIdentity(t *testing.T) {
	artifact := testArtifact(t)
	root := t.TempDir()
	store := server.NewLocalBlobStore(root, "blob")
	published, err := Publish(context.Background(), store, artifact)
	if err != nil {
		t.Fatal(err)
	}
	read, value, err := Read(context.Background(), store, published.ContentHash)
	if err != nil || read.ContentHash != artifact.ContentHash || len(value) == 0 {
		t.Fatalf("canonical read = %+v, bytes=%d, %v", read, len(value), err)
	}
	path := filepath.Join(root, filepath.FromSlash(published.ContentKey))
	if err := os.WriteFile(path, append(value, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(context.Background(), store, published.ContentHash); err == nil {
		t.Fatal("non-canonical blob bytes were accepted")
	}
}

func TestEmptyEligibleSetPublishesAuditableMissedRootArtifact(t *testing.T) {
	in := BuildInput{DeploymentID: "empty", GenesisHash: "0x" + strings.Repeat("ab", 32), PolicyHash: "0x" + strings.Repeat("cd", 32), ChainID: 945,
		Netuid: 7, Coordinator: common.HexToAddress("0x100"), SettlementVault: common.HexToAddress("0x200"),
		Epoch: 8, NoID: 2, Start: Boundary{Number: 100, Hash: "0x" + strings.Repeat("01", 32)}, End: Boundary{Number: 200, Hash: "0x" + strings.Repeat("02", 32)},
		OperatorSnapshotHash: "sha256:" + strings.Repeat("10", 32), FleetSnapshotHash: "sha256:" + strings.Repeat("20", 32),
		Providers:       []ProviderInput{{ClientID: [16]byte{1}, UsageBytes: 42, ExclusionReason: "missing_payout_wallet"}},
		ReliabilityAMin: 8, CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
	a, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err := Sign(a, key); err != nil {
		t.Fatal(err)
	}
	if err := Verify(a); err != nil {
		t.Fatal(err)
	}
	if len(a.Leaves) != 0 || a.PayoutRoot != ([32]byte{}) || a.SharesTotalBPS != 0 || a.ExcludedUsageBytes != 42 {
		t.Fatalf("unexpected empty artifact: %+v", a)
	}
}
