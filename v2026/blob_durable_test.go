// Explicit local-blob custody tests use real descriptors and synthetic kernel
// facts. No test accesses a live database, bucket or production declaration.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/durablevolume"
	"golang.org/x/sys/unix"
)

// Each test controls facts on its own host; filesystem validation stays real.
type durableBlobTestHost struct {
	stateLock  sync.Mutex
	mounts     []durablevolume.Mount
	device     durablevolume.Device
	filesystem durablevolume.Filesystem
}

func (self *durableBlobTestHost) Mounts() ([]durablevolume.Mount, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]durablevolume.Mount(nil), self.mounts...), nil
}

func (self *durableBlobTestHost) DeviceUuid(string) (durablevolume.Device, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.device, nil
}

func (self *durableBlobTestHost) Filesystem(*os.File) (durablevolume.Filesystem, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.filesystem, nil
}

func (self *durableBlobTestHost) reserve(bytes, inodes uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.filesystem.AvailableBytes, self.filesystem.AvailableInodes = bytes, inodes
}

type durableBlobFixture struct {
	root      string
	reference durablevolume.Reference
	host      *durableBlobTestHost
	store     *localBlobStore
	source    string
	raw       []byte
}

func durableBlobDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// Metadata and the lease live outside the precreated data root. No constructor
// under test provisions or repairs those required declarations.
func newDurableBlobFixture(t *testing.T) *durableBlobFixture {
	t.Helper()
	mount := t.TempDir()
	if err := os.Chmod(mount, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(mount, "objects")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	var state unix.Stat_t
	if err := unix.Stat(mount, &state); err != nil {
		t.Fatal(err)
	}
	device := durablevolume.Device{Major: unix.Major(uint64(state.Dev)), Minor: unix.Minor(uint64(state.Dev))}
	host := &durableBlobTestHost{device: device, filesystem: durablevolume.Filesystem{Id: [2]int32{37, 41}, Type: 0xef53, AvailableBytes: 1024 * 1024, AvailableInodes: 1024}, mounts: []durablevolume.Mount{
		{Id: 1, ParentId: 1, Root: "/", Path: "/", Device: durablevolume.Device{Major: device.Major ^ 1, Minor: device.Minor}, FilesystemType: "ext4"},
		{Id: 7, ParentId: 1, Root: "/", Path: mount, Device: device, FilesystemType: "ext4"},
	}}
	marker, lease := []byte("synthetic-blob-volume\n"), []byte("synthetic-blob-root-lease\n")
	markerPath, leasePath := filepath.Join(mount, "volume-marker"), filepath.Join(mount, "root-lease")
	for path, raw := range map[string][]byte{markerPath: marker, leasePath: lease} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	nonce := make([]byte, durablevolume.RootGenerationBytes)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(root, durablevolume.RootGenerationAttribute, nonce, unix.XATTR_CREATE); err != nil {
		t.Fatal(err)
	}
	var rootState unix.Stat_t
	if err := unix.Stat(root, &rootState); err != nil {
		t.Fatal(err)
	}
	config := durablevolume.Config{Schema: durablevolume.Schema, Volumes: []durablevolume.VolumeSpec{{MountPath: mount, FilesystemUuid: "1234-abcd", FilesystemType: "ext4", MarkerPath: markerPath, MarkerSha256: durableBlobDigest(marker), StateRoots: []durablevolume.StateRootSpec{{Path: root, LeasePath: leasePath, LeaseSha256: durableBlobDigest(lease), RootInode: rootState.Ino, GenerationSha256: durableBlobDigest(nonce)}}, MinAvailableBytes: 1024, MinAvailableInodes: 8}}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	reference := durablevolume.Reference{Path: filepath.Join(mount, "declaration.json"), Sha256: durableBlobDigest(raw)}
	if err := os.WriteFile(reference.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := newDurableLocalBlobStore(t.Context(), root, "synthetic/", 4096, reference, host)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("synthetic retained provider evidence\n")
	source := filepath.Join(mount, "source")
	if err := os.WriteFile(source, content, 0600); err != nil {
		t.Fatal(err)
	}
	return &durableBlobFixture{root: root, reference: reference, host: host, store: store, source: source, raw: content}
}

// Ending each operation must not enroll an empty replacement root on the same
// filesystem. A restored path also cannot revive this already refused store.
func TestDurableLocalBlobStoreRejectsReplacementBetweenOperations(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	if created, err := fixture.store.PutIfAbsent(t.Context(), "retained", fixture.source, ""); err != nil || !created {
		t.Fatal("initial retained write", created, err)
	}
	if err := os.Rename(fixture.root, fixture.root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.root, 0700); err != nil {
		t.Fatal(err)
	}
	created, err := fixture.store.PutIfAbsent(t.Context(), "fresh", fixture.source, "")
	if created || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatalf("same store admitted a new empty root after closing its prior operation: created=%v err=%v", created, err)
	}
	if objects, err := fixture.store.List(t.Context(), ""); !errors.Is(err, durablevolume.ErrIdentity) || len(objects) != 0 {
		t.Fatal("replacement became an empty acknowledged listing", objects, err)
	}
	if reader, err := fixture.store.Get(t.Context(), "retained"); !errors.Is(err, durablevolume.ErrIdentity) || reader != nil {
		t.Fatal("replacement became a missing-object read", err)
	}
	entries, err := os.ReadDir(fixture.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("refused replacement acquired new state", entries, err)
	}
	if err := os.Remove(fixture.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.root+"-original", fixture.root); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.CheckRetention(t.Context()); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("restoration revived a refused store instance", err)
	}
	reopened, err := newDurableLocalBlobStore(t.Context(), fixture.root, fixture.store.prefix, fixture.store.maxBytes, fixture.reference, fixture.host)
	if err != nil {
		t.Fatal("explicit original custody reopen", err)
	}
	reader, err := reopened.Get(t.Context(), "retained")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("original bytes changed", string(raw), err)
	}
}

// Cancellation before acquiring ownership cannot create a quota lock or data.
func TestDurableLocalBlobStoreCanceledAdmissionHasNoEffects(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if created, err := fixture.store.PutIfAbsent(ctx, "unpublished", fixture.source, ""); created || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled write admitted", created, err)
	}
	entries, err := os.ReadDir(fixture.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled admission created state", entries, err)
	}
}

// Reserve pressure precedes any mutation, so the same admitted batch can retry
// after capacity recovers. Read-only inspection remains available while full.
func TestDurableLocalBlobStoreReserveRecoversWithoutReset(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{fixture.store}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	fixture.host.reserve(0, 0)
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "retained", fixture.source, ""); created || !errors.Is(err, durablevolume.ErrUnavailable) || errors.Is(err, ErrLocalBlobPublicationUncertain) {
		t.Fatal("reserve pressure was not refused before publication", created, err)
	}
	fixture.host.reserve(1024*1024, 1024)
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "retained", fixture.source, ""); err != nil || !created {
		t.Fatal("same batch could not recover its reserve", created, err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.host.reserve(0, 0)
	reader, err := fixture.store.Get(t.Context(), "retained")
	if err != nil {
		t.Fatal("full media hid original bytes", err)
	}
	raw, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("full-media read changed original bytes", err)
	}
	objects, err := fixture.store.List(t.Context(), "")
	if err != nil || len(objects) != 1 || objects[0].Key != "retained" || fixture.store.CheckRetention(t.Context()) != nil {
		t.Fatal("full-media retained inspection failed", objects, err)
	}
}

// An unacknowledged atomic link has real bytes but no successful response. The
// batch stops before another mutation; a fresh owner reconciles the same key.
func TestDurableLocalBlobStoreLostSyncRequiresOriginalReopen(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{fixture.store}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	lostSync := errors.New("synthetic blob parent sync failure")
	writes := 0
	fixture.store.syncDirectoryForTest = func(*os.File) error { writes++; return lostSync }
	created, err := fixture.store.PutIfAbsent(batch.Context(), "retained", fixture.source, "")
	if created || !errors.Is(err, lostSync) || !errors.Is(err, ErrLocalBlobPublicationUncertain) || errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("lost acknowledgement misclassified", created, err)
	}
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "different", fixture.source, ""); created || !errors.Is(err, ErrLocalBlobPublicationUncertain) || writes != 1 {
		t.Fatal("uncertain batch retried mutation", created, err, writes)
	}
	if err := batch.Close(); !errors.Is(err, ErrLocalBlobPublicationUncertain) {
		t.Fatal("batch close erased uncertainty", err)
	}
	fixture.store.syncDirectoryForTest = nil
	if created, err := fixture.store.PutIfAbsent(t.Context(), "retained", fixture.source, ""); created || err != nil {
		t.Fatal("reopened original key was recreated", created, err)
	}
	reader, err := fixture.store.Get(t.Context(), "retained")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("uncertain original object changed", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "different")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dependent object was published", err)
	}
}

// A staged-file sync failure has not published a key. Retrying does not need
// journal recovery and preserves all previously acknowledged objects.
func TestDurableLocalBlobStoreStagedSyncFailureKeepsPriorBytes(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	if err := fixture.store.Put(t.Context(), "prior", fixture.source, ""); err != nil {
		t.Fatal(err)
	}
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{fixture.store}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	stageFailure := errors.New("synthetic staged object sync failure")
	fixture.store.syncFileForTest = func(*os.File) error { return stageFailure }
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "next", fixture.source, ""); created || !errors.Is(err, stageFailure) || errors.Is(err, ErrLocalBlobPublicationUncertain) {
		t.Fatal("unpublished stage misclassified", created, err)
	}
	fixture.store.syncFileForTest = nil
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "next", fixture.source, ""); !created || err != nil {
		t.Fatal("prepublication failure poisoned batch", created, err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(filepath.Join(fixture.root, "prior"))
	if err != nil || !bytes.Equal(prior, fixture.raw) {
		t.Fatal("staging failure changed prior object", err)
	}
}

// A live read holds the root lease and checks the original leaf on every read;
// identical bytes at a replacement inode cannot satisfy retained custody.
func TestDurableLocalBlobReaderReplacementRemainsRefusedAfterRestore(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	if err := fixture.store.Put(t.Context(), "retained", fixture.source, ""); err != nil {
		t.Fatal(err)
	}
	reader, err := fixture.store.Get(t.Context(), "retained")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if snapshot, err := durablevolume.OpenWithHost(fixture.reference, fixture.root, durablevolume.Snapshot, fixture.host); !errors.Is(err, durablevolume.ErrBusy) || snapshot != nil {
		t.Fatal("active reader did not retain its snapshot lease", err)
	}
	path := filepath.Join(fixture.root, "retained")
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture.raw, 0600); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := reader.Read(one[:]); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("reader accepted a new inode", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+"-original", path); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(one[:]); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("restoration revived failed read custody", err)
	}
	if err := reader.Close(); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("reader close erased loss", err)
	}
	snapshot, err := durablevolume.OpenWithHost(fixture.reference, fixture.root, durablevolume.Snapshot, fixture.host)
	if err != nil {
		t.Fatal("joined read left a snapshot lease", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

// A declared local service never creates an absent mount-root, and a mixed
// legacy/guarded batch cannot choose its less restrictive member implicitly.
func TestDurableLocalBlobStoreRefusesMissingRootAndMixedPolicy(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	legacy := NewLocalBlobStoreWithMaxBytes(fixture.root, "synthetic/", 4096)
	for _, stores := range [][]BlobStore{{legacy, fixture.store}, {fixture.store, legacy}} {
		if batch, err := BeginLocalBlobWriteBatch(t.Context(), stores, 4); err == nil || batch != nil {
			t.Fatal("mixed physical policy admitted", err)
		}
	}
	if err := os.Rename(fixture.root, fixture.root+"-original"); err != nil {
		t.Fatal(err)
	}
	if created, err := fixture.store.PutIfAbsent(t.Context(), "fresh", fixture.source, ""); created || err == nil {
		t.Fatal("missing root admitted", created, err)
	}
	if _, err := os.Lstat(fixture.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing root was recreated", err)
	}
	if opened, err := NewDurableLocalBlobStore(t.Context(), fixture.root, "synthetic/", 4096, durablevolume.Reference{}); opened != nil || err == nil {
		t.Fatal("public durable constructor accepted no policy", err)
	}
}

// A fresh process must authenticate the externally approved inode and nonce,
// even if the replacement repeats the same path and retained public bytes.
func TestDurableLocalBlobReopenRequiresApprovedRootGeneration(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	if created, err := fixture.store.PutIfAbsent(t.Context(), "retained", fixture.source, ""); !created || err != nil {
		t.Fatal(created, err)
	}
	if err := os.Rename(fixture.root, fixture.root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(fixture.root, 0700); err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, durablevolume.RootGenerationBytes)
	if _, err := unix.Getxattr(fixture.root+"-original", durablevolume.RootGenerationAttribute, nonce); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(fixture.root, durablevolume.RootGenerationAttribute, nonce, unix.XATTR_CREATE); err != nil {
		t.Fatal(err)
	}
	if opened, err := newDurableLocalBlobStore(t.Context(), fixture.root, fixture.store.prefix, fixture.store.maxBytes, fixture.reference, fixture.host); opened != nil || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("fresh store enrolled a different inode with the old nonce", err)
	}
	if err := os.Remove(fixture.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.root+"-original", fixture.root); err != nil {
		t.Fatal(err)
	}
	nonce[0] ^= 1
	if err := unix.Setxattr(fixture.root, durablevolume.RootGenerationAttribute, nonce, unix.XATTR_REPLACE); err != nil {
		t.Fatal(err)
	}
	if opened, err := newDurableLocalBlobStore(t.Context(), fixture.root, fixture.store.prefix, fixture.store.maxBytes, fixture.reference, fixture.host); opened != nil || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("fresh store admitted an altered generation on the original inode", err)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.root, "retained"))
	if err != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("rejected generation reset original content", err)
	}
}

// A held owner cannot follow a new mount generation. Reopening is a separate
// admission after joining the old batch, never a repair of its stale guard.
func TestDurableLocalBlobBatchRefusesRemountUntilReopen(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{fixture.store}, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	fixture.host.stateLock.Lock()
	fixture.host.mounts[1].Id++
	fixture.host.stateLock.Unlock()
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "next", fixture.source, ""); created || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("held batch crossed mount generation", created, err)
	}
	fixture.host.stateLock.Lock()
	fixture.host.mounts[1].Id--
	fixture.host.stateLock.Unlock()
	if created, err := fixture.store.PutIfAbsent(batch.Context(), "next", fixture.source, ""); created || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("mount restoration revived old batch", created, err)
	}
	if err := batch.Close(); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("batch close erased generation loss", err)
	}
	if created, err := fixture.store.PutIfAbsent(t.Context(), "next", fixture.source, ""); !created || err != nil {
		t.Fatal("fresh original-volume owner could not resume", created, err)
	}
}

// Configuration parsing carries the exact reference; the public backend
// selector cannot use the legacy local API when that policy is absent.
func TestConfiguredLocalBlobRequiresExactDeclaration(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	raw, err := json.Marshal(map[string]any{"authority": "local", "path": fixture.root, "max_bytes": 4096, "durable_volumes": fixture.reference})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := Vault.PushSimpleResource("minio.yml", raw)
	config, present := LoadBlobStoreConfig()
	cleanup()
	if !present || !config.Local || config.DurableVolumes != fixture.reference {
		t.Fatal("explicit local policy reference was lost", present, config)
	}
	raw, err = json.Marshal(map[string]any{"authority": "local", "path": fixture.root, "max_bytes": 4096})
	if err != nil {
		t.Fatal(err)
	}
	cleanup = Vault.PushSimpleResource("minio.yml", raw)
	store, admitted := LoadBlobStore()
	cleanup()
	if admitted || store != nil {
		t.Fatal("configured local backend fell back without a declaration")
	}
	entries, err := os.ReadDir(fixture.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("refused config created local state", entries, err)
	}
}

// The expiry decision belongs to one observed inode and timestamp. A named
// replacement at the deterministic commit boundary must retain its own bytes.
func TestDurableLocalBlobReaperRefusesReplacedExpiryCandidate(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	if err := fixture.store.Put(t.Context(), "retained", fixture.source, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.root, "retained")
	old := time.Unix(1, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	fixture.store.rules = []BlobLifecycleRule{{KeyPrefix: "", TTL: time.Hour}}
	fired := false
	replacement := []byte("new synthetic object, not the expired candidate\n")
	fixture.store.beforeReapCommitForTest = func(key string) {
		if key != "retained" || fired {
			t.Fatal("unexpected expiry boundary", key)
		}
		fired = true
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, replacement, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := fixture.store.reapPass(t.Context())
	retained, readErr := os.ReadFile(path)
	if !fired || !errors.Is(err, durablevolume.ErrIdentity) || readErr != nil || !bytes.Equal(retained, replacement) {
		t.Fatalf("expiry removed or admitted a different object: boundary=%t err=%v retained=%q read=%v", fired, err, retained, readErr)
	}
}

// Directory sync uncertainty is an unacknowledged removal, not a claim that
// physical identity changed. A new pass inspects the surviving original root.
func TestDurableLocalBlobReaperUncertainSyncReopensOriginalRoot(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	if err := fixture.store.Put(t.Context(), "expired", fixture.source, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.root, "expired")
	old := time.Unix(1, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	fixture.store.rules = []BlobLifecycleRule{{KeyPrefix: "", TTL: time.Hour}}
	lostSync := errors.New("synthetic expiry directory acknowledgement loss")
	syncs := 0
	fixture.store.syncDirectoryForTest = func(*os.File) error { syncs++; return lostSync }
	err := fixture.store.reapPass(t.Context())
	if !errors.Is(err, lostSync) || !errors.Is(err, ErrLocalBlobPublicationUncertain) || errors.Is(err, durablevolume.ErrIdentity) || syncs != 1 {
		t.Fatal("uncertain deletion was acknowledged or misclassified", err, syncs)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fault did not follow the actual unlink", err)
	}
	fixture.store.syncDirectoryForTest = nil
	if err := fixture.store.reapPass(t.Context()); err != nil {
		t.Fatal("fresh owner did not inspect the original root", err)
	}
	if objects, err := fixture.store.List(t.Context(), ""); err != nil || len(objects) != 0 {
		t.Fatal("recovery recreated the expired object", objects, err)
	}
}
