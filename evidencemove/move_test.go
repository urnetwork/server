// The mover's rules against an in-memory store: copy, verify, then remove;
// reruns and resumes after a crash complete without losing an object; a
// missing or mismatched copy keeps the flat object; closed gates hold every
// request; objects never start faster than their rate; and failed requests
// back off, while refused ones end the run. Every key and signer is synthetic.
package evidencemove

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/minio/minio-go/v7"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

const testPrefix = "blob"

// One signed evidence envelope's exact stored bytes and content hash.
type testEvidence struct {
	hash  string // lowercase hex
	bytes []byte
}

func newTestEvidence(t testing.TB, index int) testEvidence {
	t.Helper()
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	envelope := &startifact.EvidenceEnvelope{
		DeploymentID: "test-deployment",
		ChainID:      945,
		GenesisHash:  "0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105",
		Netuid:       7,
		Kind:         startifact.ClientKeyRegistrationEvidenceKind,
		CreatedAt:    time.Unix(1_700_000_000+int64(index), 0).UTC().Format(time.RFC3339Nano),
		Payload:      json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)),
	}
	if err := startifact.SignEvidence(envelope, key); err != nil {
		t.Fatal(err)
	}
	encoded, err := startifact.EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return testEvidence{hash: strings.TrimPrefix(envelope.ContentHash, "sha256:"), bytes: encoded}
}

func testEvidenceSet(t testing.TB, count int) []testEvidence {
	evidence := make([]testEvidence, count)
	for index := range evidence {
		evidence[index] = newTestEvidence(t, index)
	}
	return evidence
}

// Items in source order for the evidence, as st_client_key_history rows
// would name them, with the database's size of each.
func testItems(evidence []testEvidence) []Item {
	items := make([]Item, len(evidence))
	for index, entry := range evidence {
		items[index] = Item{
			Cursor: Cursor{ClientId: server.NewId(), Generation: 1},
			Hash:   "sha256:" + entry.hash,
			Size:   int64(len(entry.bytes)),
		}
	}
	return items
}

func testKeys(t testing.TB, hash string) ContentKeys {
	t.Helper()
	keys, err := KeysFor(testPrefix, hash)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// An in-memory bucket with MinIO's answers: a missing key is NoSuchKey, a
// copy with a stale source ETag is refused, a delete of a missing key
// succeeds. Every request is logged; fault may fail any request first.
type memoryStore struct {
	lock    sync.Mutex
	objects map[string][]byte
	log     []string
	// called before each request; an error is the request's answer
	fault func(operation string, key string) error
	// rewrites what a copy writes
	copyRewrite func(data []byte) []byte
}

func newMemoryStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}}
}

func (self *memoryStore) put(key string, data []byte) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.objects[key] = bytes.Clone(data)
}

func (self *memoryStore) get(key string) ([]byte, bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	data, ok := self.objects[key]
	return data, ok
}

func (self *memoryStore) requests() []string {
	self.lock.Lock()
	defer self.lock.Unlock()
	return slices.Clone(self.log)
}

func (self *memoryStore) begin(ctx context.Context, operation string, key string) error {
	self.lock.Lock()
	self.log = append(self.log, operation+" "+key)
	fault := self.fault
	self.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if fault != nil {
		return fault(operation, key)
	}
	return nil
}

func (self *memoryStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := self.begin(ctx, "stat", key); err != nil {
		return ObjectInfo{}, err
	}
	data, ok := self.get(key)
	if !ok {
		return ObjectInfo{}, errNotFound(key)
	}
	return ObjectInfo{Key: key, Size: int64(len(data)), ETag: md5Hex(data)}, nil
}

func (self *memoryStore) Copy(ctx context.Context, src ObjectInfo, dstKey string) error {
	if err := self.begin(ctx, "copy", src.Key+" "+dstKey); err != nil {
		return err
	}
	data, ok := self.get(src.Key)
	if !ok {
		return errNotFound(src.Key)
	}
	if md5Hex(data) != src.ETag {
		return fmt.Errorf("copy: %w", errCopyPrecondition)
	}
	if self.copyRewrite != nil {
		data = self.copyRewrite(bytes.Clone(data))
		if data == nil {
			// the copy was acknowledged and lost
			return nil
		}
	}
	self.put(dstKey, data)
	return nil
}

func (self *memoryStore) Read(ctx context.Context, key string, limit int64) ([]byte, ObjectInfo, error) {
	if err := self.begin(ctx, "read", key); err != nil {
		return nil, ObjectInfo{}, err
	}
	data, ok := self.get(key)
	if !ok {
		return nil, ObjectInfo{}, errNotFound(key)
	}
	info := ObjectInfo{Key: key, Size: int64(len(data)), ETag: md5Hex(data)}
	return bytes.Clone(data[:min(int64(len(data)), limit+1)]), info, nil
}

func (self *memoryStore) Remove(ctx context.Context, key string) error {
	if err := self.begin(ctx, "remove", key); err != nil {
		return err
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	delete(self.objects, key)
	return nil
}

// A gate whose state a test sets.
type testGate struct {
	lock  sync.Mutex
	state GateState
	name  string
}

func newTestGate(open bool) *testGate {
	return &testGate{state: GateState{Open: open, Reason: "test"}, name: "test"}
}

func (self *testGate) Name() string            { return self.name }
func (self *testGate) Interval() time.Duration { return 10 * time.Millisecond }
func (self *testGate) Check(ctx context.Context) GateState {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.state
}
func (self *testGate) set(state GateState) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.state = state
}

// Settings for tests: fast, few workers, short backoffs.
func testSettings() Settings {
	settings := DefaultSettings()
	settings.ObjectsPerSecond = 5000
	settings.Workers = 4
	settings.BatchRows = 7
	settings.MaxAttempts = 3
	settings.RequestTimeout = 5 * time.Second
	settings.ProgressInterval = time.Hour
	return settings
}

func testMover(t testing.TB, store ObjectStore, items []Item, checkpointPath string, gates ...Gate) *Mover {
	t.Helper()
	if len(gates) == 0 {
		gates = []Gate{newTestGate(true)}
	}
	keeper := NewGateKeeper(t.Logf, gates...)
	keeper.CheckNow(context.Background())
	keeper.minBackoff, keeper.maxBackoff = 10*time.Millisecond, 40*time.Millisecond
	mover := &Mover{
		Store:    store,
		Prefix:   testPrefix,
		Source:   NewSliceSource(items),
		Gates:    keeper,
		Pacer:    NewPacer(testSettings().ObjectsPerSecond),
		Settings: testSettings(),
		Logf:     t.Logf,
	}
	if checkpointPath != "" {
		checkpoint, err := OpenCheckpoint(checkpointPath, ClientKeyHistorySourceName, "bucket", testPrefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { checkpoint.Close() })
		mover.Checkpoint = checkpoint
	}
	return mover
}

// Every evidence's flat object is gone and its sharded object holds exactly
// its bytes: nothing was lost or altered.
func requireAllMoved(t testing.TB, store *memoryStore, evidence []testEvidence) {
	t.Helper()
	for _, entry := range evidence {
		keys := testKeys(t, entry.hash)
		if _, ok := store.get(keys.Flat); ok {
			t.Fatalf("%s: the flat object is still stored", entry.hash)
		}
		data, ok := store.get(keys.Sharded)
		if !ok || !bytes.Equal(data, entry.bytes) {
			t.Fatalf("%s: the sharded object is missing or altered", entry.hash)
		}
	}
}

// A flat object is copied, read back and verified, and only then removed;
// the sharded object holds exactly its bytes.
func TestMoveCopiesVerifiesThenRemoves(t *testing.T) {
	evidence := testEvidenceSet(t, 3)
	store := newMemoryStore()
	for _, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
	}
	items := testItems(evidence)
	mover := testMover(t, store, items, filepath.Join(t.TempDir(), "checkpoint.json"))
	mover.Settings.Workers = 1
	report, err := mover.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Finished || report.Counts.Moved != 3 || report.Counts.Items != 3 {
		t.Fatalf("report %+v", report)
	}
	requireAllMoved(t, store, evidence)
	for _, entry := range evidence {
		keys := testKeys(t, entry.hash)
		var sequence []string
		for _, request := range store.requests() {
			if strings.Contains(request, keys.Hash) && !strings.HasPrefix(request, "stat") {
				sequence = append(sequence, strings.Fields(request)[0])
			}
		}
		if !slices.Equal(sequence, []string{"copy", "read", "remove"}) {
			t.Fatalf("%s: requests after the HEADs %v, want copy, read, remove", keys.Hash, sequence)
		}
		for _, request := range store.requests() {
			if strings.HasPrefix(request, "remove") && strings.Contains(request, keys.Hash) && request != "remove "+keys.Flat {
				t.Fatalf("removed a key other than the flat one: %s", request)
			}
		}
	}
	if mover.Checkpoint.State.Cursor != items[len(items)-1].Cursor || mover.Checkpoint.State.Counts.Moved != 3 {
		t.Fatalf("checkpoint %+v", mover.Checkpoint.State)
	}
}

// Every layout an object can be in: flat only moves, both deduplicates (the
// server wrote the sharded copy), sharded only and neither are left alone.
// A second run finds everything done and writes nothing.
func TestMoveRerunIsIdempotent(t *testing.T) {
	evidence := testEvidenceSet(t, 8)
	store := newMemoryStore()
	for index, entry := range evidence {
		keys := testKeys(t, entry.hash)
		switch index % 4 {
		case 0:
			store.put(keys.Flat, entry.bytes)
		case 1:
			store.put(keys.Flat, entry.bytes)
			store.put(keys.Sharded, entry.bytes)
		case 2:
			store.put(keys.Sharded, entry.bytes)
		}
	}
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
	items := testItems(evidence)
	report, err := testMover(t, store, items, checkpointPath).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := (Counts{Items: 8, Moved: 2, Deduped: 2, Done: 2, Absent: 2}); report.Counts != want {
		t.Fatalf("first run %+v, want %+v", report.Counts, want)
	}
	// a fresh run over the same rows, from the start
	before := len(store.requests())
	rerun, err := testMover(t, store, items, filepath.Join(t.TempDir(), "other.json")).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := (Counts{Items: 8, Done: 6, Absent: 2}); rerun.Counts != want {
		t.Fatalf("rerun %+v, want %+v", rerun.Counts, want)
	}
	for _, request := range store.requests()[before:] {
		if !strings.HasPrefix(request, "stat ") {
			t.Fatalf("a rerun sent %s", request)
		}
	}
	for index, entry := range evidence {
		if index%4 == 3 {
			continue
		}
		keys := testKeys(t, entry.hash)
		if _, ok := store.get(keys.Flat); ok {
			t.Fatalf("%s: flat object kept", entry.hash)
		}
		if data, _ := store.get(keys.Sharded); !bytes.Equal(data, entry.bytes) {
			t.Fatalf("%s: sharded object missing or altered", entry.hash)
		}
	}
}

// A crash mid-batch: the run stops after one object's copy and before its
// removal, and the durable checkpoint is a batch behind (its last write never
// happened). A new run from that checkpoint finishes every object, and no
// object is lost.
func TestMoveResumesAfterCrashMidBatch(t *testing.T) {
	evidence := testEvidenceSet(t, 40)
	store := newMemoryStore()
	for _, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
	}
	items := testItems(evidence)
	directory := t.TempDir()
	checkpointPath := filepath.Join(directory, "checkpoint.json")
	ctx, crash := context.WithCancel(t.Context())
	crashKey := testKeys(t, evidence[23].hash).Sharded
	var durable []byte
	store.fault = func(operation string, key string) error {
		if operation == "copy" && strings.HasSuffix(key, " "+crashKey) {
			// the checkpoint as it stood before this batch: what a crash leaves
			durable, _ = os.ReadFile(checkpointPath)
			defer crash()
		}
		return nil
	}
	mover := testMover(t, store, items, checkpointPath)
	if _, err := mover.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mover.Checkpoint.Close()
	if data, _ := store.get(crashKey); !bytes.Equal(data, evidence[23].bytes) {
		t.Fatalf("the crashed object's copy is missing")
	}
	if _, ok := store.get(testKeys(t, evidence[23].hash).Flat); !ok {
		t.Fatalf("the crashed object's flat object was removed before its verification")
	}
	if len(durable) == 0 {
		t.Fatal("no checkpoint was durable before the crash")
	}
	if err := os.WriteFile(checkpointPath, durable, 0o600); err != nil {
		t.Fatal(err)
	}
	store.fault = nil
	resumed := testMover(t, store, items, checkpointPath)
	if resumed.Checkpoint.State.Cursor == (Cursor{}) || 23 <= slices.IndexFunc(items, func(item Item) bool { return item.Cursor == resumed.Checkpoint.State.Cursor }) {
		t.Fatalf("resumed at %s, want a batch before the crash", resumed.Checkpoint.State.Cursor)
	}
	report, err := resumed.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Finished || report.Counts.Deduped < 1 {
		t.Fatalf("resumed run %+v", report)
	}
	requireAllMoved(t, store, evidence)
}

// The flat object stays whenever its sharded copy cannot prove itself: a copy
// acknowledged but missing, a copy with other bytes, a sharded object that
// already held other bytes, a flat object that is not valid evidence for its
// key, and a flat object whose size is not the database's.
func TestMoveRefusesMissingOrMismatchedCopy(t *testing.T) {
	evidence := testEvidenceSet(t, 2)
	other := newTestEvidence(t, 99)
	type fixture struct {
		name    string
		prepare func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item)
		// the outcome, or "" when the run must fail with the flat object kept
		outcome Outcome
		// the sharded object the run must leave: nil for none
		sharded []byte
	}
	for _, test := range []fixture{
		{
			name: "copy acknowledged but lost",
			prepare: func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item) {
				store.put(keys.Flat, entry.bytes)
				store.copyRewrite = func([]byte) []byte { return nil }
			},
		},
		{
			name: "copy with other bytes",
			prepare: func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item) {
				store.put(keys.Flat, entry.bytes)
				store.copyRewrite = func(data []byte) []byte { return append(data[:len(data)-1], ' ') }
			},
			outcome: OutcomeRefused,
		},
		{
			name: "sharded object holds other evidence",
			prepare: func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item) {
				store.put(keys.Flat, entry.bytes)
				store.put(keys.Sharded, other.bytes)
			},
			outcome: OutcomeRefused,
			// not this run's copy: left for the operator
			sharded: other.bytes,
		},
		{
			name: "flat object is other evidence",
			prepare: func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item) {
				store.put(keys.Flat, other.bytes)
				item.Size = int64(len(other.bytes))
			},
			outcome: OutcomeRefused,
		},
		{
			name: "flat object is not evidence",
			prepare: func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item) {
				store.put(keys.Flat, []byte(`{"schema":"other"}`))
				item.Size = 0
			},
			outcome: OutcomeRefused,
		},
		{
			name: "flat size differs from the database",
			prepare: func(store *memoryStore, keys ContentKeys, entry testEvidence, item *Item) {
				store.put(keys.Flat, entry.bytes)
				item.Size += 1
			},
			outcome: OutcomeRefused,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newMemoryStore()
			entry := evidence[0]
			keys := testKeys(t, entry.hash)
			items := testItems(evidence[:1])
			test.prepare(store, keys, entry, &items[0])
			flatBefore, _ := store.get(keys.Flat)
			checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
			mover := testMover(t, store, items, checkpointPath)
			refusals, err := OpenJSONL(mover.Checkpoint.RefusalsPath())
			if err != nil {
				t.Fatal(err)
			}
			mover.Refusals = refusals
			report, err := mover.Run(t.Context())
			if test.outcome == "" {
				if err == nil || !isFatal(err) {
					t.Fatalf("run %+v, %v; want a fatal error", report, err)
				}
				if mover.Checkpoint.State.Cursor != (Cursor{}) {
					t.Fatalf("the checkpoint passed the failed object")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if report.Counts.Refused != 1 || report.Counts.Removed() != 0 {
					t.Fatalf("report %+v, want one refusal", report.Counts)
				}
				data, err := os.ReadFile(mover.Checkpoint.RefusalsPath())
				if err != nil || !strings.Contains(string(data), keys.Hash) {
					t.Fatalf("refusal not recorded: %s %v", data, err)
				}
			}
			for _, request := range store.requests() {
				if request == "remove "+keys.Flat {
					t.Fatalf("sent %s", request)
				}
			}
			if flatAfter, ok := store.get(keys.Flat); !ok || !bytes.Equal(flatAfter, flatBefore) {
				t.Fatalf("the flat object was removed or changed")
			}
			// a refused copy of the run's own never shadows the flat object
			if sharded, ok := store.get(keys.Sharded); ok != (test.sharded != nil) || !bytes.Equal(sharded, test.sharded) {
				t.Fatalf("sharded object %q left, want %q", sharded, test.sharded)
			}
		})
	}
}

// No storage request is sent while a gate is closed; the run starts when it
// opens. A validator window that ends closes the gates with no new check.
func TestMoveWaitsWhileAGateIsClosed(t *testing.T) {
	evidence := testEvidenceSet(t, 5)
	store := newMemoryStore()
	for _, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
	}
	gate := newTestGate(false)
	mover := testMover(t, store, testItems(evidence), filepath.Join(t.TempDir(), "checkpoint.json"), gate)
	done := make(chan error, 1)
	go func() {
		_, err := mover.Run(t.Context())
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if requests := store.requests(); len(requests) != 0 {
		t.Fatalf("sent %v while the gate was closed", requests)
	}
	gate.set(GateState{Open: true, Reason: "opened"})
	mover.Gates.CheckNow(t.Context())
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start when the gate opened")
	}
	requireAllMoved(t, store, evidence)

	now := time.Now()
	keeper := NewGateKeeper(t.Logf, gate)
	keeper.now = func() time.Time { return now }
	gate.set(GateState{Open: true, Reason: "window", Until: now.Add(time.Minute)})
	keeper.CheckNow(t.Context())
	if open, reason := keeper.Open(); !open {
		t.Fatalf("closed inside its window: %s", reason)
	}
	keeper.now = func() time.Time { return now.Add(time.Minute) }
	if open, _ := keeper.Open(); open {
		t.Fatal("open after its window ended")
	}
}

// The validator gate opens only while the intent of the chain's current
// native epoch is finalized or applied and at least the margin is left, and
// closes on any unreadable input; open, it carries the time it closes.
func TestValidatorGate(t *testing.T) {
	// the release gates read 2026-11-17 15:24:45Z: epoch 26398, boundary in 120 s
	if epoch, left := NativeEpoch(nativeEpochOriginBlock + 103310); epoch != 26398 || left != 120*time.Second {
		t.Fatalf("NativeEpoch = %d, %s", epoch, left)
	}
	if epoch, left := NativeEpoch(nativeEpochOriginBlock + 360); epoch != 26112 || left != 0 {
		t.Fatalf("at a boundary NativeEpoch = %d, %s", epoch, left)
	}
	if epoch, left := NativeEpoch(nativeEpochOriginBlock + 361); epoch != 26113 || left != 359*blockTime {
		t.Fatalf("after a boundary NativeEpoch = %d, %s", epoch, left)
	}
	now := time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)
	head := uint64(nativeEpochOriginBlock + 103310 + 100) // epoch 26399, 270 blocks (54 min) left
	for _, test := range []struct {
		name     string
		head     uint64
		progress ValidatorProgress
		err      error
		open     bool
	}{
		{name: "finalized", head: head, progress: ValidatorProgress{26399, "finalized"}, open: true},
		{name: "applied", head: head, progress: ValidatorProgress{26399, "applied"}, open: true},
		{name: "pending", head: head, progress: ValidatorProgress{26399, "pending"}},
		{name: "previous epoch's intent", head: head, progress: ValidatorProgress{26398, "applied"}},
		{name: "just enough left", head: head + 190, progress: ValidatorProgress{26399, "finalized"}, open: true},
		{name: "too close to the boundary", head: head + 200, progress: ValidatorProgress{26399, "finalized"}},
		{name: "progress unreadable", head: head, err: errors.New("ssh: timeout")},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := &ValidatorGate{
				Progress:     testProgressReader{progress: test.progress, err: test.err},
				Head:         testHeadReader(test.head),
				MinRemaining: 15 * time.Minute,
				Now:          func() time.Time { return now },
			}
			state := gate.Check(t.Context())
			if state.Open != test.open {
				t.Fatalf("open %v (%s), want %v", state.Open, state.Reason, test.open)
			}
			if state.Open {
				_, left := NativeEpoch(test.head)
				if want := now.Add(left - 15*time.Minute); !state.Until.Equal(want) {
					t.Fatalf("until %s, want %s", state.Until, want)
				}
			}
		})
	}
	progress, err := ParseValidatorProgress([]byte(`{"intent":{"current":{},"value":{"native_epoch":26398,"status":"applied","vector_hash":"0x"}},"steering":{}}`))
	if err != nil || progress != (ValidatorProgress{26398, "applied"}) {
		t.Fatalf("parsed %+v, %v", progress, err)
	}
	if _, err := ParseValidatorProgress([]byte(`{"intent":{}}`)); err == nil {
		t.Fatal("parsed progress with no intent")
	}
}

type testProgressReader struct {
	progress ValidatorProgress
	err      error
}

func (self testProgressReader) ReadProgress(ctx context.Context) (ValidatorProgress, error) {
	return self.progress, self.err
}

type testHeadReader uint64

func (self testHeadReader) BlockNumber(ctx context.Context) (uint64, error) {
	return uint64(self), nil
}

// Objects never start faster than the rate, across all workers.
func TestMovePacesObjects(t *testing.T) {
	evidence := testEvidenceSet(t, 31)
	store := newMemoryStore()
	for _, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
	}
	mover := testMover(t, store, testItems(evidence), filepath.Join(t.TempDir(), "checkpoint.json"))
	mover.Settings.Workers = 8
	mover.Pacer = NewPacer(100)
	start := time.Now()
	report, err := mover.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); report.Counts.Moved != 31 || elapsed < 300*time.Millisecond {
		t.Fatalf("31 objects at 100 a second took %s (%+v)", elapsed, report.Counts)
	}
	pacer := NewPacer(1000)
	start = time.Now()
	for range 201 {
		if err := pacer.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("201 starts at 1000 a second took %s", elapsed)
	}
}

// A failed request is retried after a backoff with the gates closed, and the
// object completes; a refused request ends the run with the flat object kept
// and the checkpoint before it; an object that keeps failing ends the run.
func TestMoveBacksOffAndRetries(t *testing.T) {
	evidence := testEvidenceSet(t, 3)
	newStore := func() *memoryStore {
		store := newMemoryStore()
		for _, entry := range evidence {
			store.put(testKeys(t, entry.hash).Flat, entry.bytes)
		}
		return store
	}

	store := newStore()
	var failures int
	var lock sync.Mutex
	store.fault = func(operation string, key string) error {
		lock.Lock()
		defer lock.Unlock()
		if operation == "remove" && failures < 2 {
			failures += 1
			return minio.ErrorResponse{StatusCode: http.StatusServiceUnavailable, Code: "SlowDown", Message: "synthetic"}
		}
		return nil
	}
	mover := testMover(t, store, testItems(evidence), filepath.Join(t.TempDir(), "checkpoint.json"))
	mover.Settings.Workers = 1
	report, err := mover.Run(t.Context())
	if err != nil || report.Counts.Moved != 3 || failures != 2 {
		t.Fatalf("report %+v, %v after %d failures", report, err, failures)
	}
	if report.Paused <= 0 {
		t.Fatal("the gates never closed for the backoff")
	}
	requireAllMoved(t, store, evidence)

	store = newStore()
	store.fault = func(operation string, key string) error {
		if operation == "copy" {
			return minio.ErrorResponse{StatusCode: http.StatusForbidden, Code: "AccessDenied", Message: "synthetic"}
		}
		return nil
	}
	mover = testMover(t, store, testItems(evidence), filepath.Join(t.TempDir(), "checkpoint.json"))
	if _, err := mover.Run(t.Context()); err == nil || !isFatal(err) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refused request gave %v", err)
	}
	if mover.Checkpoint.State.Cursor != (Cursor{}) {
		t.Fatal("the checkpoint passed a refused object")
	}

	store = newStore()
	store.fault = func(operation string, key string) error {
		if operation == "read" {
			return errors.New("connection reset")
		}
		return nil
	}
	mover = testMover(t, store, testItems(evidence), filepath.Join(t.TempDir(), "checkpoint.json"))
	if _, err := mover.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("an object failing every attempt gave %v", err)
	}
	for _, entry := range evidence {
		if _, ok := store.get(testKeys(t, entry.hash).Flat); !ok {
			t.Fatal("removed a flat object whose copy was never read back")
		}
	}
}

// A run stops once it removed its bound; the checkpoint covers what it did,
// and a later run continues from there.
func TestMoveStopsAtItsBound(t *testing.T) {
	evidence := testEvidenceSet(t, 30)
	store := newMemoryStore()
	for _, entry := range evidence {
		store.put(testKeys(t, entry.hash).Flat, entry.bytes)
	}
	items := testItems(evidence)
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
	mover := testMover(t, store, items, checkpointPath)
	mover.Settings.MaxMoves = 10
	report, err := mover.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Finished || report.Counts.Moved < 10 || 10+int64(mover.Settings.Workers) < report.Counts.Moved {
		t.Fatalf("bounded run %+v", report)
	}
	mover.Checkpoint.Close()
	rest := testMover(t, store, items, checkpointPath)
	report, err = rest.Run(t.Context())
	if err != nil || !report.Finished {
		t.Fatalf("rest %+v, %v", report, err)
	}
	requireAllMoved(t, store, evidence)
	if counts := rest.Checkpoint.State.Counts; counts.Moved+counts.Deduped != 30 {
		t.Fatalf("checkpoint counts %+v", counts)
	}
}

// A dry run sends only HEADs and writes no checkpoint.
func TestMoveDryRunChangesNothing(t *testing.T) {
	evidence := testEvidenceSet(t, 4)
	store := newMemoryStore()
	for index, entry := range evidence {
		keys := testKeys(t, entry.hash)
		store.put(keys.Flat, entry.bytes)
		if index%2 == 1 {
			store.put(keys.Sharded, entry.bytes)
		}
	}
	mover := testMover(t, store, testItems(evidence), "")
	mover.Settings.DryRun = true
	report, err := mover.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := (Counts{Items: 4, WouldMove: 2, WouldDedupe: 2}); report.Counts != want {
		t.Fatalf("dry run %+v", report.Counts)
	}
	for _, request := range store.requests() {
		if !strings.HasPrefix(request, "stat ") {
			t.Fatalf("a dry run sent %s", request)
		}
	}
}

// A checkpoint is held by one run at a time, and refuses another bucket.
func TestCheckpointIsExclusiveAndBoundToItsStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	checkpoint, err := OpenCheckpoint(path, ClientKeyHistorySourceName, "blob", "blob")
	if err != nil {
		t.Fatal(err)
	}
	cursor := Cursor{ClientId: server.NewId(), Generation: 3}
	if err := checkpoint.Advance(cursor, Counts{Items: 2, Moved: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCheckpoint(path, ClientKeyHistorySourceName, "blob", "blob"); err == nil {
		t.Fatal("a second run took a held checkpoint")
	}
	checkpoint.Close()
	if _, err := OpenCheckpoint(path, ClientKeyHistorySourceName, "other", "blob"); err == nil {
		t.Fatal("a checkpoint of another bucket was accepted")
	}
	reopened, err := OpenCheckpoint(path, ClientKeyHistorySourceName, "blob", "blob")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.State.Cursor != cursor || reopened.State.Counts.Moved != 2 {
		t.Fatalf("reopened %+v", reopened.State)
	}
}
