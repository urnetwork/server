package startifact

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
)

// immutableRaceBlobStore provides both the former split List/Put behavior and
// the atomic capability so the same barrier deterministically distinguishes
// them. The first two operations for raceKey take their absence snapshot (for
// List) or arrive before creation (for PutIfAbsent) before either may proceed.
type immutableRaceBlobStore struct {
	stateLock      sync.Mutex
	prefix         string
	objects        map[string][]byte
	raceKey        string
	raceArrivals   int
	raceReady      chan struct{}
	createdContent []byte
}

func newImmutableRaceBlobStore(raceKey string) *immutableRaceBlobStore {
	store := &immutableRaceBlobStore{
		prefix:  "blob",
		objects: map[string][]byte{},
		raceKey: raceKey,
	}
	if raceKey != "" {
		store.raceReady = make(chan struct{})
	}
	return store
}

func (self *immutableRaceBlobStore) waitAtRace(ctx context.Context) error {
	self.stateLock.Lock()
	self.raceArrivals++
	if self.raceArrivals == 2 {
		close(self.raceReady)
	}
	ready := self.raceReady
	self.stateLock.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self *immutableRaceBlobStore) Put(ctx context.Context, key string, localPath string, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	self.objects[key] = append([]byte(nil), content...)
	self.stateLock.Unlock()
	return nil
}

func (self *immutableRaceBlobStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	if self.createdContent != nil {
		content = self.createdContent
	}
	if key == self.raceKey {
		if err := self.waitAtRace(ctx); err != nil {
			return false, err
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.objects[key]; ok {
		return false, nil
	}
	self.objects[key] = append([]byte(nil), content...)
	return true, nil
}

func (self *immutableRaceBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %s does not exist", key)
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), content...))), nil
}

func (self *immutableRaceBlobStore) List(ctx context.Context, keyPrefix string) ([]server.BlobObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	objects := []server.BlobObject{}
	for key, content := range self.objects {
		if strings.HasPrefix(key, keyPrefix) {
			objects = append(objects, server.BlobObject{Key: key, Size: int64(len(content))})
		}
	}
	self.stateLock.Unlock()
	if keyPrefix == self.raceKey {
		if err := self.waitAtRace(ctx); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func (self *immutableRaceBlobStore) ListPage(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]server.BlobObject, bool, error) {
	objects, err := self.List(ctx, keyPrefix)
	if err != nil {
		return nil, false, err
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	page := []server.BlobObject{}
	for _, object := range objects {
		if object.Key <= startAfter {
			continue
		}
		if len(page) == limit {
			return page, true, nil
		}
		page = append(page, object)
	}
	return page, false, nil
}

func (self *immutableRaceBlobStore) SetLifecycle(context.Context, []server.BlobLifecycleRule) error {
	return nil
}

func (self *immutableRaceBlobStore) Bucket() string    { return "race-bucket" }
func (self *immutableRaceBlobStore) Prefix() string    { return self.prefix }
func (self *immutableRaceBlobStore) Authority() string { return "race" }

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

func TestPutImmutableConcurrentWritersKeepTheStoredWinner(t *testing.T) {
	key := "blob/st/v1/content/sha256/collision.json"
	store := newImmutableRaceBlobStore(key)
	values := [][]byte{[]byte(`{"writer":1}`), []byte(`{"writer":2}`)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type writeResult struct {
		writerIndex int
		err         error
	}
	results := make(chan writeResult, len(values))
	for i, value := range values {
		writerIndex := i
		value := value
		go func() {
			results <- writeResult{writerIndex: writerIndex, err: putImmutable(ctx, store, key, value)}
		}()
	}

	winnerIndex := -1
	conflictCount := 0
	for range values {
		select {
		case result := <-results:
			if result.err == nil {
				if winnerIndex != -1 {
					t.Fatalf("writers %d and %d both reported success", winnerIndex, result.writerIndex)
				}
				winnerIndex = result.writerIndex
			} else if strings.Contains(result.err.Error(), "already contains different bytes") {
				conflictCount++
			} else {
				t.Fatalf("writer %d returned %v", result.writerIndex, result.err)
			}
		case <-ctx.Done():
			t.Fatalf("concurrent immutable writes did not finish: %v", ctx.Err())
		}
	}
	if winnerIndex == -1 || conflictCount != 1 {
		t.Fatalf("winner = %d, conflict count = %d", winnerIndex, conflictCount)
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(stored, values[winnerIndex]) {
		t.Fatalf("stored winner = %q, read error = %v, close error = %v", stored, readErr, closeErr)
	}
}

func TestPutImmutableVerifiesBytesAfterCreating(t *testing.T) {
	store := newImmutableRaceBlobStore("")
	store.createdContent = []byte(`{"corrupt":true}`)
	err := putImmutable(context.Background(), store, "blob/st/v1/content/sha256/value.json", []byte(`{"expected":true}`))
	if err == nil || !strings.Contains(err.Error(), "already contains different bytes") {
		t.Fatalf("created bytes were not verified: %v", err)
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
