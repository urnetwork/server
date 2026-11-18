// Tests for `sidecarctl evs drop-reindex` (owner decision 512, QD-965). The reindex runs for real: evsservice's
// DropResolve on sidecartest's fake clusters, over a repo whose ref wrote a path and then dropped its versions, and
// whose drop job's resolve wrote the list files, built through evsservice's own writers as its drop_resolve_test.go
// builds its world. The repo is placed on the second metadata cluster, so a run that did not read place/ finds no
// history. A corrupt list file is written again with resolve's bytes and its sequence printed; with no drops named
// they come from a whole list file, and with none whole from the job's snap-0, printed (owner decision 570, QD-1021);
// drops named are written only as the set snap-0 covers, a set missing one, holding another or naming one otherwise
// refused with nothing written, as is a run that needs a snap-0 which does not read; a job with no snap-0 runs as
// owner decision 512 gave it, the named drops taken unchecked; a refusal exits 1 having written nothing. A recording
// reindex holds the rest: what the command passes Reindex, a line it refuses before opening anything, its pace and
// its exit codes.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The metadata cluster place/ names for the world's repo: the second, so the first holds only place/.
const evsDropReindexTestClusterId = "meta-2"

// A 16-byte id from a label.
func evsDropReindexTestId(label string) sidecar.Id {
	digest := sha256.Sum256([]byte("evs drop-reindex test " + label))
	return sidecar.Id(digest[:16])
}

// A one-token path from a label.
func evsDropReindexTestPath(label string) sidecar.EvsTokenPath {
	return sidecar.EvsTokenPath{sidecar.EvsToken(evsDropReindexTestId("token " + label))}
}

// Drop settings whose read pace never waits, and whose files hold fileRowCount rows each.
func evsDropReindexTestDropSettings(fileRowCount int) *evsservice.DropSettings {
	settings := evsservice.DefaultDropSettings()
	settings.ReadBytesPerSecond = 1000 * 1000 * 1000 * 1000 * 1000
	settings.FileRowCount = fileRowCount
	return settings
}

// One change a push makes: the path's next entry version on base, its chain one segment of its own, or one inline
// record.
type evsDropReindexChange struct {
	path    sidecar.EvsTokenPath
	base    uint64
	segment [32]byte
	inline  bool
}

// A repo on fake clusters, placed on the second metadata cluster, with one file-mode ref and the job id its drop job
// writes its files under.
type evsDropReindexWorld struct {
	t            *testing.T
	ctx          context.Context
	clock        *sidecartest.FakeClock
	host         *sidecartest.FakeHost
	cluster      sidecar.BlobCluster
	segment      sidecar.BlobCluster
	dropSettings *evsservice.DropSettings
	heads        *evsservice.HeadStore
	batches      *evsservice.BatchWriter
	builder      *sidecar.EvsStatementBuilder
	paidCtx      context.Context
	repoId       sidecar.Id
	refId        sidecar.Id
	signerId     sidecar.Id
	jobId        sidecar.Id
	head         *evsservice.EvsStoredHead
	pushCount    int
	// what a run's settings counted
	openCount    int
	closeCount   int
	reindexCount int
}

// An empty world whose repo place/ names, with the drop job's settings dropSettings.
func newEvsDropReindexWorld(t *testing.T, dropSettings *evsservice.DropSettings) *evsDropReindexWorld {
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	hostSettings := sidecartest.DefaultFakeHostSettings()
	hostSettings.Blobs.MetadataClusterIds = []string{"meta-1", evsDropReindexTestClusterId}
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, hostSettings)
	sidecartest.RegisterEvsTwins(host.Redis)
	evsSettings := evsservice.DefaultEvsSettings()
	evsSettings.BatchFlushDelay = 0
	cluster, err := host.Blobs.Cluster(evsDropReindexTestClusterId)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := host.Blobs.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	signerId := evsDropReindexTestId("signer")
	world := &evsDropReindexWorld{
		t: t, ctx: ctx, clock: clock, host: host, cluster: cluster, segment: segment, dropSettings: dropSettings,
		heads:   evsservice.NewHeadStore(clock, host.ClockCheck, host.Redis, evsSettings, sidecar.NewNoopLogger()),
		batches: evsservice.NewBatchWriter(ctx, clock, host.ClockCheck, host.Blobs, nil, evsSettings, sidecar.NewNoopLogger()),
		builder: sidecar.NewEvsStatementBuilder(func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
			return &bridge.Signed{Body: bytes.Clone(body), SignerId: signerId.Bytes(), Signature: make([]byte, 64), Type: uint32(signedType)}, nil
		}),
		repoId: evsDropReindexTestId("repo"), refId: evsDropReindexTestId("ref"), signerId: signerId, jobId: evsDropReindexTestId("job"),
		head: &evsservice.EvsStoredHead{},
	}
	networkId := [16]byte(evsDropReindexTestId("network"))
	grant, err := host.Budget.AdmitP1(ctx, evsDropReindexTestClusterId, networkId, sidecar.WriteCost{ImmediateByteCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(grant.Close)
	world.paidCtx = grant.Context()
	firstGrant, err := host.Budget.AdmitP1(ctx, "meta-1", networkId, sidecar.WriteCost{ImmediateByteCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(firstGrant.Close)
	world.place(firstGrant.Context(), evsDropReindexTestClusterId)
	repoBytes, err := proto.Marshal(&protocol.EvsRepoObject{
		RepoId: world.repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL, State: protocol.EvsRepoState_EVS_REPO_ACTIVE, Format: 2,
		NetworkId: networkId[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.PutIfNoneMatch(world.paidCtx, evsservice.RepoKey(world.repoId), repoBytes, nil); err != nil {
		t.Fatal(err)
	}
	return world
}

// Writes the repo's place/ on the first cluster, naming clusterId.
func (self *evsDropReindexWorld) place(ctx context.Context, clusterId string) {
	self.t.Helper()
	placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: clusterId, CreatorSidecarId: self.signerId}).Marshal()
	if err != nil {
		self.t.Fatal(err)
	}
	firstCluster, err := self.host.Blobs.FirstCluster()
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := firstCluster.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(self.repoId), placeBytes, nil); err != nil {
		self.t.Fatal(err)
	}
}

// Commits a push of changes, with flags and a DROP's sections, on the ref at its next version, as evs commits one:
// the statement through the builder, its record through the batch writer, the head through HeadStore. Returns the
// version, the push's id and each change's entry version bytes.
func (self *evsDropReindexWorld) commit(changes []*evsDropReindexChange, flags uint32, dropBody []byte, dropLists []byte) (uint64, sidecar.Id, [][]byte) {
	self.t.Helper()
	self.pushCount += 1
	pushId := evsDropReindexTestId(fmt.Sprintf("push %d", self.pushCount))
	baseVersion := self.head.Version()
	baseChain := sidecar.EvsChainStart(self.repoId, self.refId)
	if !self.head.Absent() && 0 < baseVersion {
		baseChain = [32]byte(self.head.Head.GetChain())
	}
	plan := &sidecar.EvsPushPlan{
		RepoId: self.repoId, RefId: self.refId, RefKind: protocol.EvsRefKind_EVS_REF_SHARED, RefMode: protocol.EvsRefMode_EVS_REF_FILE,
		PushId: pushId, BaseVersion: baseVersion, BaseChain: baseChain, Generation: 1, GroupId: evsDropReindexTestId("group"),
		GenerationCommitment: make([]byte, 32), PathEpoch: 1, Auth: &bridge.AuthPin{Version: 1, Chain: make([]byte, 32)},
		SignerSidecarId: self.signerId, ClientId: evsDropReindexTestId("client"), TimeMs: self.clock.Now().UnixMilli(),
		SealedMetadata: []byte("sealed metadata"), Flags: flags, DropBody: dropBody, DropLists: dropLists,
	}
	entries := [][]byte{}
	changedPaths := []sidecar.EvsTokenPath{}
	createsPath := false
	for _, change := range changes {
		entry := &protocol.EvsEntryVersion{
			Version: change.base + 1, PushId: pushId.Bytes(), BaseVersion: baseVersion, Kind: uint32(protocol.EvsEntryKind_EVS_ENTRY_FILE),
			PaddedSize: 4096, SealedFields: []byte(fmt.Sprintf("sealed %d", self.pushCount)), TimeMs: self.clock.Now().UnixMilli(),
		}
		if change.inline {
			entry.Chain = []*protocol.EvsLocator{{Kind: uint32(protocol.EvsLocatorKind_EVS_LOCATOR_INLINE), Inline: []byte("inline record")}}
		} else {
			entry.Chain = []*protocol.EvsLocator{{Kind: uint32(protocol.EvsLocatorKind_EVS_LOCATOR_SEGMENT), Id: bytes.Clone(change.segment[:]), Length: 4096}}
		}
		entryBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(entry)
		if err != nil {
			self.t.Fatal(err)
		}
		entries = append(entries, entryBytes)
		plan.Changes = append(plan.Changes, &sidecar.EvsPlannedChange{TokenPath: change.path, BaseEntryVersion: change.base, EntryVersion: entryBytes})
		changedPaths = append(changedPaths, change.path)
		createsPath = createsPath || change.base == 0
	}
	built, err := self.builder.BuildFormat2(plan)
	if err != nil {
		self.t.Fatal(err)
	}
	if len(built.Parts) != 0 {
		self.t.Fatal("a test push built in parts")
	}
	place, err := self.batches.Append(self.paidCtx, evsDropReindexTestClusterId, &sidecar.EvsBatchRecord{
		Type: protocol.EvsBatchRecordType_EVS_BATCH_PUSH, RepoId: self.repoId, RefId: self.refId, PushId: pushId,
		SignerSidecarId: self.signerId, TimeMs: plan.TimeMs, Body: sidecar.EvsPushRecordBody(built.Statement.Bytes, built.Payload),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	var coveringPrefixes []sidecar.EvsTokenPath
	if len(built.Statement.Push.GetChangesRoot()) != 0 {
		coveringPrefixes = built.Statement.ChangedPrefixes
	}
	windowEntry := sidecar.NewEvsWindowEntry(built.Statement.Sha256, built.Statement.Push, changedPaths, coveringPrefixes, createsPath, place)
	next := evsservice.CloneEvsHead(self.head.Head)
	if self.head.Absent() {
		next = &protocol.EvsHead{
			RefKind: uint32(protocol.EvsRefKind_EVS_REF_SHARED), RefMode: uint32(protocol.EvsRefMode_EVS_REF_FILE),
			PathEpoch: &protocol.EvsPathEpoch{Current: 1}, WindowBaseChain: baseChain[:],
		}
	}
	chain := sidecar.EvsChainNext(baseChain, pushId, built.Statement.Sha256)
	next.Version = baseVersion + 1
	next.Chain = chain[:]
	next.PushId = pushId.Bytes()
	next.Window = append(bytes.Clone(next.GetWindow()), windowEntry.Encode()...)
	written, err := self.heads.Commit(self.ctx, self.cluster, self.repoId, self.refId, self.head, next)
	if err != nil {
		self.t.Fatal(err)
	}
	self.head = written
	return next.Version, pushId, entries
}

// Writes path count times, each version's chain a segment of its own, and returns the entry versions' bytes.
func (self *evsDropReindexWorld) write(path sidecar.EvsTokenPath, count int) [][]byte {
	self.t.Helper()
	entries := [][]byte{}
	for i := range count {
		segment := sha256.Sum256([]byte(fmt.Sprintf("segment %d", i)))
		_, _, written := self.commit([]*evsDropReindexChange{{path: path, base: uint64(i), segment: segment}}, 0, nil, nil)
		entries = append(entries, written...)
	}
	return entries
}

// Commits a DROP of every version listed, which path's history holds below the DROP's own write: through the next
// version, which the DROP writes as a kept head of one inline record (§11.7's written boundary). The drop as a list
// file's header names it.
func (self *evsDropReindexWorld) drop(dropId sidecar.Id, path sidecar.EvsTokenPath, listed [][]byte) *evsservice.DropListDrop {
	self.t.Helper()
	hashes := [][]byte{}
	for _, entryBytes := range listed {
		digest := sha256.Sum256(entryBytes)
		hashes = append(hashes, digest[:])
	}
	through := uint64(len(listed)) + 1
	dropPath := &protocol.EvsDropPath{
		Lineage:        []*protocol.EvsSourcePath{{Epoch: 1, TokenPath: path.Components()}},
		ThroughVersion: through,
		Boundary:       sidecar.EvsDropBoundaryWritten,
	}
	lists, err := sidecar.EvsBuildDropLists([]*protocol.EvsDropPath{dropPath}, [][][]byte{hashes})
	if err != nil {
		self.t.Fatal(err)
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.EvsDropHistory{
		DropId: dropId.Bytes(), Action: 1, NotBeforeMs: self.clock.Now().UnixMilli(), Paths: []*protocol.EvsDropPath{dropPath},
	})
	if err != nil {
		self.t.Fatal(err)
	}
	version, pushId, _ := self.commit([]*evsDropReindexChange{{path: path, base: through - 1, inline: true}}, uint32(protocol.EvsPushFlag_EVS_PUSH_DROP), body, lists)
	return &evsservice.DropListDrop{DropId: dropId, RefId: self.refId, Version: version, PushId: pushId}
}

// The job's resolve over drops, as the drop job runs it: its list and cand files written under the job's id.
func (self *evsDropReindexWorld) resolve(drops ...*evsservice.DropListDrop) {
	self.t.Helper()
	resolver, err := evsservice.NewDropResolve(self.host.ServiceHost(), &evsservice.DropResolveDeps{Settings: self.dropSettings})
	if err != nil {
		self.t.Fatal(err)
	}
	covered := []*evsservice.DropCovered{}
	for _, drop := range drops {
		covered = append(covered, &evsservice.DropCovered{DropId: drop.DropId, RefId: drop.RefId, Version: drop.Version, PushId: drop.PushId})
	}
	result, err := resolver.Resolve(self.ctx, &evsservice.DropResolveRequest{
		Cluster: self.cluster, SegmentCluster: self.segment, RepoId: self.repoId, JobId: self.jobId, Drops: covered,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	if len(result.Mismatch) != 0 {
		self.t.Fatalf("the job's resolve refused %v", result.Mismatch)
	}
}

// Writes the job's snap-0 covering drops, as the drop job writes it once its resolve has: each drop's four ids with
// the place and seen_ms its table entry held.
func (self *evsDropReindexWorld) snap(drops ...*evsservice.DropListDrop) {
	self.t.Helper()
	covered := []*evsservice.DropCovered{}
	for i, drop := range drops {
		covered = append(covered, &evsservice.DropCovered{
			DropId: drop.DropId, RefId: drop.RefId, Version: drop.Version, PushId: drop.PushId,
			Place:  &protocol.EvsPlace{Minute: 29000000, Instance: 1, Seq: uint32(i + 1), Length: 512},
			SeenMs: self.clock.Now().UnixMilli(),
		})
	}
	key := evsservice.DropFileKey(self.repoId, self.jobId, evsservice.DropFileSnap, 0)
	if _, err := self.segment.PutIfNoneMatch(self.ctx, key, evsservice.EncodeDropCoveredFile(self.repoId, self.jobId, covered), nil); err != nil {
		self.t.Fatal(err)
	}
}

// Replaces the job's snap-0 with its bytes, the last one, the trailer's, flipped.
func (self *evsDropReindexWorld) corruptSnap() {
	self.t.Helper()
	key := evsservice.DropFileKey(self.repoId, self.jobId, evsservice.DropFileSnap, 0)
	original := self.object(self.segment.ClusterId(), key)
	if len(original) == 0 {
		self.t.Fatal("no snap-0")
	}
	corrupt := bytes.Clone(original)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := evsservice.DecodeDropFile(corrupt, self.repoId, self.jobId, evsservice.DropFileSnap, 0); !errors.Is(err, evsservice.ErrDropFileInvalid) {
		self.t.Fatalf("a flipped trailer reads as %v", err)
	}
	if err := self.segment.Delete(self.ctx, key); err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.segment.PutIfNoneMatch(self.ctx, key, corrupt, nil); err != nil {
		self.t.Fatal(err)
	}
}

// The name the fake store's Dump gives the job's list file of sequence. Dump's values are a privacy scan's, the key
// and metadata before the body, so they compare with each other and never with an object's bytes.
func (self *evsDropReindexWorld) listFileName(sequence uint32) string {
	return self.segment.ClusterId() + "/" + evsservice.DropFileKey(self.repoId, self.jobId, evsservice.DropFileList, sequence)
}

// The bytes of the object the world's cluster clusterId holds at key, nil when it holds none.
func (self *evsDropReindexWorld) object(clusterId string, key string) []byte {
	self.t.Helper()
	cluster, err := self.host.Blobs.Cluster(clusterId)
	if err != nil {
		self.t.Fatal(err)
	}
	object, err := cluster.Get(self.ctx, key)
	if errors.Is(err, sidecar.ErrBlobNotFound) {
		return nil
	}
	if err != nil {
		self.t.Fatal(err)
	}
	defer object.Body.Close()
	objectBytes, err := io.ReadAll(object.Body)
	if err != nil {
		self.t.Fatal(err)
	}
	return objectBytes
}

// The bytes of the job's list file of sequence, nil when there is none.
func (self *evsDropReindexWorld) listFile(sequence uint32) []byte {
	return self.object(self.segment.ClusterId(), evsservice.DropFileKey(self.repoId, self.jobId, evsservice.DropFileList, sequence))
}

// Replaces the job's list file of sequence with its bytes, the last one, the trailer's, flipped, and returns the bytes
// resolve wrote.
func (self *evsDropReindexWorld) corruptListFile(sequence uint32) []byte {
	self.t.Helper()
	original := self.listFile(sequence)
	if len(original) == 0 {
		self.t.Fatalf("no list file %d", sequence)
	}
	if _, err := evsservice.DecodeDropListFile(original, self.repoId, self.jobId, sequence); err != nil {
		self.t.Fatalf("resolve's list file %d does not read: %v", sequence, err)
	}
	corrupt := bytes.Clone(original)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := evsservice.DecodeDropListFile(corrupt, self.repoId, self.jobId, sequence); !errors.Is(err, evsservice.ErrDropFileInvalid) {
		self.t.Fatalf("a flipped trailer reads as %v", err)
	}
	key := evsservice.DropFileKey(self.repoId, self.jobId, evsservice.DropFileList, sequence)
	if err := self.segment.Delete(self.ctx, key); err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.segment.PutIfNoneMatch(self.ctx, key, corrupt, nil); err != nil {
		self.t.Fatal(err)
	}
	return original
}

// A run's settings on the world's clusters and clock, under the world's drop settings, counting each open, close and
// reindex built.
func (self *evsDropReindexWorld) settings() *evsDropReindexSettings {
	settings := defaultEvsDropReindexSettings()
	settings.dropSettings = self.dropSettings
	settings.clock = self.clock
	settings.openBlobs = func(ctx context.Context) (sidecar.BlobStore, func(), error) {
		self.openCount += 1
		return self.host.Blobs, func() { self.closeCount += 1 }, nil
	}
	newReindexer := settings.newReindexer
	settings.newReindexer = func(blobs sidecar.BlobStore, dropSettings *evsservice.DropSettings) (evsDropReindexer, error) {
		self.reindexCount += 1
		return newReindexer(blobs, dropSettings)
	}
	return settings
}

// A command set holding the command under settings, so a test reads the exit code a runbook step reads.
func evsDropReindexTestCommands(settings *evsDropReindexSettings) *sidecarctlCommandSet {
	commands := newSidecarctlCommandSet()
	commands.add(&sidecarctlCommand{
		words:   "evs drop-reindex",
		usage:   "<repo_id> <job_id> [<drop_id>:<ref_id>:<version>:<push_id>...]",
		summary: "the command under test",
		run: func(args []string, out io.Writer) error {
			return runEvsDropReindexWithSettings(context.Background(), args, settings, out)
		},
	})
	return commands
}

// Runs the command line after `evs drop-reindex` under settings, returning its exit code and output.
func runEvsDropReindexTest(settings *evsDropReindexSettings, args ...string) (int, string, string) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := evsDropReindexTestCommands(settings).run(append([]string{"evs", "drop-reindex"}, args...), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// A drop as the command line names it.
func evsDropReindexTuple(drop *evsservice.DropListDrop) string {
	return fmt.Sprintf("%s:%s:%d:%s", sidecar.EvsKeyId(drop.DropId), sidecar.EvsKeyId(drop.RefId), drop.Version, sidecar.EvsKeyId(drop.PushId))
}

// The fake store's objects other than the job's list file of sequence, which a run may write.
func evsDropReindexObjectsBesides(world *evsDropReindexWorld, sequence uint32) map[string][]byte {
	objects := world.host.Blobs.Dump()
	delete(objects, world.listFileName(sequence))
	return objects
}

// DESIGN-DETAILS 10.5's corrupt list file row through the command: of a job's two list files, the second fails its
// trailer. With no drops named the run takes them from the first, which reads whole, deletes the second and writes it
// again with the bytes resolve wrote, prints its sequence as rewritten and nothing else as changed, and touches no
// other object; a second run rewrites nothing.
func TestEvsDropReindexWritesACorruptListFileAgainAndNamesItsSequence(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	path := evsDropReindexTestPath("x")
	listed := world.write(path, 2)
	drop := world.drop(evsDropReindexTestId("drop x"), path, listed)
	world.resolve(drop)
	if world.listFile(1) == nil {
		t.Fatal("the job's resolve wrote one list file, so this tests no whole file beside the corrupt one")
	}
	original := world.corruptListFile(1)
	before := evsDropReindexObjectsBesides(world, 1)
	world.clock.Advance(time.Minute)

	code, stdout, stderr := runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if !bytes.Equal(world.listFile(1), original) {
		t.Fatal("the list file was written again with bytes other than resolve's")
	}
	if !maps.EqualFunc(evsDropReindexObjectsBesides(world, 1), before, bytes.Equal) {
		t.Fatal("a run changed an object besides the corrupt list file")
	}
	wantLines := []string{
		fmt.Sprintf("job %s of repo %s: its list files hold the bytes its drops resolve to", sidecar.EvsKeyId(world.jobId), sidecar.EvsKeyId(world.repoId)),
		"rewritten 1",
		"written none",
		"stray none",
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 5 || !slices.Equal(lines[:4], wantLines) || !strings.HasPrefix(lines[4], "scanned_bytes ") || lines[4] == "scanned_bytes 0" {
		t.Fatalf("printed %q, want %q and the bytes scanned", stdout, wantLines)
	}
	// ids and counts only (I-13): no key, so no path of any kind
	if strings.Contains(stdout, "/") || strings.Contains(stdout, "dropscan") {
		t.Fatalf("printed %q, which names a key", stdout)
	}
	if stderr != "" {
		t.Fatalf("printed %q to stderr", stderr)
	}
	if world.openCount != 1 || world.closeCount != 1 {
		t.Fatalf("opened the clusters %d times and closed them %d", world.openCount, world.closeCount)
	}

	code, stdout, stderr = runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 || !strings.Contains(stdout, "\nrewritten none\nwritten none\n") {
		t.Fatalf("a second run exited %d, printing %q and %q", code, stdout, stderr)
	}
}

// A job with no snap-0, as one whose cleanup ran before snap-0 was kept, runs as owner decision 512 gave it. Of one
// list file, corrupt: with no drops named nothing reads whole, so the run refuses, exit 1, writing nothing; named, the
// drops are taken as named, printed as unchecked, and the file is written again with resolve's bytes.
func TestEvsDropReindexTakesTheNamedDropsWhenNoListFileReadsWhole(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	path := evsDropReindexTestPath("x")
	drop := world.drop(evsDropReindexTestId("drop x"), path, world.write(path, 3))
	world.resolve(drop)
	original := world.corruptListFile(0)
	before := world.host.Blobs.Dump()
	world.clock.Advance(time.Minute)

	code, stdout, stderr := runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 1 || !strings.Contains(stderr, "name its drops") || stdout != "" {
		t.Fatalf("with no drops named: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !maps.EqualFunc(world.host.Blobs.Dump(), before, bytes.Equal) {
		t.Fatal("a refused run changed the store")
	}

	code, stdout, stderr = runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId), evsDropReindexTuple(drop))
	if code != 0 || !strings.Contains(stdout, "\ndrops 1 named, unchecked: the job has no snap-0\nrewritten 0\nwritten none\nstray none\n") {
		t.Fatalf("with the drop named: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(world.listFile(0), original) {
		t.Fatal("the list file was written again with bytes other than resolve's")
	}
}

// A job of two drops on a world's ref, each of a path of its own with two listed versions, resolved into one list
// file, with a third drop the job does not cover committed after them. Returns the job's two and the third.
func newEvsDropReindexTwoDropJob(world *evsDropReindexWorld) (*evsservice.DropListDrop, *evsservice.DropListDrop, *evsservice.DropListDrop) {
	world.t.Helper()
	pathX := evsDropReindexTestPath("x")
	pathY := evsDropReindexTestPath("y")
	pathZ := evsDropReindexTestPath("z")
	listedX := world.write(pathX, 2)
	listedY := world.write(pathY, 2)
	listedZ := world.write(pathZ, 2)
	dropX := world.drop(evsDropReindexTestId("drop x"), pathX, listedX)
	dropY := world.drop(evsDropReindexTestId("drop y"), pathY, listedY)
	world.resolve(dropX, dropY)
	dropZ := world.drop(evsDropReindexTestId("drop z"), pathZ, listedZ)
	if world.listFile(0) == nil || world.listFile(1) != nil {
		world.t.Fatal("the job's resolve did not write one list file")
	}
	return dropX, dropY, dropZ
}

// Owner decision 570 (QD-1021): a job of two drops whose one list file is corrupt, so none reads whole, with its
// snap-0. With no drops named the run takes snap-0's, deletes the file and writes it again with the bytes resolve
// wrote, prints the drops it took as the line names a drop, ids only, and touches no other object, snap-0 among them.
func TestEvsDropReindexTakesTheDropsFromSnapZeroWhenNoListFileReadsWhole(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	dropX, dropY, _ := newEvsDropReindexTwoDropJob(world)
	// snap-0 in the other order than the list file's, which is by drop id: the set is what counts
	snapDrops := []*evsservice.DropListDrop{dropX, dropY}
	if bytes.Compare(dropX.DropId[:], dropY.DropId[:]) < 0 {
		snapDrops = []*evsservice.DropListDrop{dropY, dropX}
	}
	world.snap(snapDrops...)
	original := world.corruptListFile(0)
	before := evsDropReindexObjectsBesides(world, 0)
	world.clock.Advance(time.Minute)

	code, stdout, stderr := runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if !bytes.Equal(world.listFile(0), original) {
		t.Fatal("the list file was written again with bytes other than resolve's")
	}
	if !maps.EqualFunc(evsDropReindexObjectsBesides(world, 0), before, bytes.Equal) {
		t.Fatal("a run changed an object besides the corrupt list file")
	}
	wantLines := []string{
		fmt.Sprintf("job %s of repo %s: its list files hold the bytes its drops resolve to", sidecar.EvsKeyId(world.jobId), sidecar.EvsKeyId(world.repoId)),
		"drops 2 taken from the job's snap-0, no list file of it reading whole",
		"drop " + evsDropReindexTuple(snapDrops[0]),
		"drop " + evsDropReindexTuple(snapDrops[1]),
		"rewritten 0",
		"written none",
		"stray none",
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 8 || !slices.Equal(lines[:7], wantLines) || !strings.HasPrefix(lines[7], "scanned_bytes ") || lines[7] == "scanned_bytes 0" {
		t.Fatalf("printed %q, want %q and the bytes scanned", stdout, wantLines)
	}
	// ids and counts only (I-13): no key, so no path of any kind
	if strings.Contains(stdout, "/") || strings.Contains(stdout, "dropscan") {
		t.Fatalf("printed %q, which names a key", stdout)
	}
	if stderr != "" {
		t.Fatalf("printed %q to stderr", stderr)
	}

	// a second run reads the list file whole, so it takes nothing from snap-0 and rewrites nothing
	code, stdout, stderr = runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 || !strings.Contains(stdout, "\nrewritten none\nwritten none\n") || strings.Contains(stdout, "\ndrop") {
		t.Fatalf("a second run exited %d, printing %q and %q", code, stdout, stderr)
	}

	// a list file that is gone, as a run stopped between its DELETE and its write leaves it, is written fresh from
	// snap-0's drops too
	if err := world.segment.Delete(world.ctx, evsservice.DropFileKey(world.repoId, world.jobId, evsservice.DropFileList, 0)); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 || !strings.Contains(stdout, "\ndrops 2 taken from the job's snap-0, no list file of it reading whole\n") || !strings.Contains(stdout, "\nrewritten none\nwritten 0\nstray none\n") {
		t.Fatalf("a missing file: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(world.listFile(0), original) {
		t.Fatal("the list file was written fresh with bytes other than resolve's")
	}
}

// Drops named that are the set snap-0 covers, in whichever order, are written: the corrupt file again with resolve's
// bytes, the run saying the named drops are snap-0's. So are they when a list file reads whole.
func TestEvsDropReindexWritesNamedDropsThatAreTheOnesSnapZeroCovers(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	dropX, dropY, _ := newEvsDropReindexTwoDropJob(world)
	world.snap(dropX, dropY)
	original := world.corruptListFile(0)
	before := evsDropReindexObjectsBesides(world, 0)
	world.clock.Advance(time.Minute)

	repo, job := sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId)
	code, stdout, stderr := runEvsDropReindexTest(world.settings(), repo, job, evsDropReindexTuple(dropY), evsDropReindexTuple(dropX))
	if code != 0 || !strings.Contains(stdout, "\ndrops 2 named, the ones the job's snap-0 covers\nrewritten 0\nwritten none\nstray none\n") {
		t.Fatalf("exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(world.listFile(0), original) {
		t.Fatal("the list file was written again with bytes other than resolve's")
	}
	if !maps.EqualFunc(evsDropReindexObjectsBesides(world, 0), before, bytes.Equal) {
		t.Fatal("a run changed an object besides the corrupt list file")
	}
	code, stdout, stderr = runEvsDropReindexTest(world.settings(), repo, job, evsDropReindexTuple(dropX), evsDropReindexTuple(dropY))
	if code != 0 || !strings.Contains(stdout, "\ndrops 2 named, the ones the job's snap-0 covers\nrewritten none\nwritten none\nstray none\n") {
		t.Fatalf("with the list file whole: exited %d, printing %q and %q", code, stdout, stderr)
	}
}

// Drops named that are not the set snap-0 covers are refused, exit 1, before Reindex reads or writes anything, with
// the ids that differ and nothing else on stderr's line, whether the job's list file is corrupt or whole: a set
// leaving out one of the job's drops, which would resolve and be written without that drop's rows; a set with a drop
// of the ref that the job does not cover, which resolves too; one drop named at another version, another push or
// another ref; and all three at once.
func TestEvsDropReindexRefusesNamedDropsThatDifferFromSnapZero(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	dropX, dropY, dropZ := newEvsDropReindexTwoDropJob(world)
	world.snap(dropX, dropY)
	world.clock.Advance(time.Minute)
	otherVersion := *dropY
	otherVersion.Version = dropZ.Version
	otherPush := *dropY
	otherPush.PushId = dropZ.PushId
	otherRef := *dropY
	otherRef.RefId = evsDropReindexTestId("another ref")
	idX, idY, idZ := sidecar.EvsKeyId(dropX.DropId), sidecar.EvsKeyId(dropY.DropId), sidecar.EvsKeyId(dropZ.DropId)
	cases := []struct {
		label string
		drops []*evsservice.DropListDrop
		want  string
	}{
		{label: "one of the job's left out", drops: []*evsservice.DropListDrop{dropX}, want: "it covers and the line leaves out " + idY + ";"},
		{label: "the other left out", drops: []*evsservice.DropListDrop{dropY}, want: "it covers and the line leaves out " + idX + ";"},
		{label: "a drop the job does not cover", drops: []*evsservice.DropListDrop{dropX, dropY, dropZ}, want: "the line names and it does not cover " + idZ + ";"},
		{label: "another version", drops: []*evsservice.DropListDrop{dropX, &otherVersion}, want: "the line names with another ref, version or push " + idY + ";"},
		{label: "another push", drops: []*evsservice.DropListDrop{dropX, &otherPush}, want: "the line names with another ref, version or push " + idY + ";"},
		{label: "another ref", drops: []*evsservice.DropListDrop{&otherRef, dropX}, want: "the line names with another ref, version or push " + idY + ";"},
		{
			label: "all three",
			drops: []*evsservice.DropListDrop{dropZ, &otherVersion},
			want:  "it covers and the line leaves out " + idX + "; the line names and it does not cover " + idZ + "; the line names with another ref, version or push " + idY + ";",
		},
	}
	for _, listFile := range []string{"whole", "corrupt"} {
		if listFile == "corrupt" {
			world.corruptListFile(0)
		}
		for _, c := range cases {
			reindexer := &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{}}
			args := []string{sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId)}
			for _, drop := range c.drops {
				args = append(args, evsDropReindexTuple(drop))
			}
			before := world.host.Blobs.Dump()
			// the reindex for real, so a set this let through would be written
			code, stdout, stderr := runEvsDropReindexTest(world.settings(), args...)
			if code != 1 || !strings.Contains(stderr, "stopped: the drops named are not the 2 its snap-0 covers, so nothing was written: "+c.want+" name none") {
				t.Errorf("%s, the list file %s: exited %d with %q, want 1 and %q", c.label, listFile, code, stderr, c.want)
			}
			if stdout != "" {
				t.Errorf("%s, the list file %s: printed %q", c.label, listFile, stdout)
			}
			if !maps.EqualFunc(world.host.Blobs.Dump(), before, bytes.Equal) {
				t.Errorf("%s, the list file %s: a refused run changed the store", c.label, listFile)
			}
			// and Reindex is never asked
			if code, _, _ := runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), args...); code != 1 || len(reindexer.requests) != 0 {
				t.Errorf("%s, the list file %s: exited %d having asked Reindex %d times", c.label, listFile, code, len(reindexer.requests))
			}
			if strings.Contains(stderr, "/") || strings.Contains(stderr, "dropscan") {
				t.Errorf("%s: printed %q, which names a key", c.label, stderr)
			}
		}
	}
}

// What the refused set would have cost, shown on a job with no snap-0, where named drops are taken unchecked: naming
// one of the job's two drops resolves and writes a list file without the other's rows. The check against snap-0 is
// what stands between a typo and that file.
func TestEvsDropReindexWithNoSnapZeroWritesTheNamedSetAsItIs(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	dropX, _, _ := newEvsDropReindexTwoDropJob(world)
	original := world.corruptListFile(0)
	world.clock.Advance(time.Minute)
	code, stdout, stderr := runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId), evsDropReindexTuple(dropX))
	if code != 0 || !strings.Contains(stdout, "\ndrops 1 named, unchecked: the job has no snap-0\nrewritten 0\n") {
		t.Fatalf("exited %d, printing %q and %q", code, stdout, stderr)
	}
	written, err := evsservice.DecodeDropListFile(world.listFile(0), world.repoId, world.jobId, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Drops) != 1 || bytes.Equal(world.listFile(0), original) {
		t.Fatalf("the file names %d drops, where the job covered 2", len(written.Drops))
	}
}

// A snap-0 that is there and does not read whole stops every run that needs it, exit 1, writing nothing: one naming
// drops, which cannot be held to it, and one naming none while no list file reads whole. A run that names none while
// a list file reads whole never reads snap-0, and runs.
func TestEvsDropReindexRefusesWhenSnapZeroDoesNotRead(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	dropX, dropY, _ := newEvsDropReindexTwoDropJob(world)
	world.snap(dropX, dropY)
	world.corruptSnap()
	world.clock.Advance(time.Minute)
	repo, job := sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId)

	code, stdout, stderr := runEvsDropReindexTest(world.settings(), repo, job)
	if code != 0 || !strings.Contains(stdout, "\nrewritten none\nwritten none\n") || strings.Contains(stdout, "\ndrop") {
		t.Fatalf("none named, the list file whole: exited %d, printing %q and %q", code, stdout, stderr)
	}

	world.corruptListFile(0)
	cases := []struct {
		label string
		drops []string
		want  string
	}{
		{label: "drops named", drops: []string{evsDropReindexTuple(dropX), evsDropReindexTuple(dropY)}, want: "its snap-0 does not read, so the drops named cannot be held to the ones it covers and nothing was written"},
		{label: "none named", want: "no list file of it reads whole and its snap-0 does not read, so nothing was written"},
	}
	for _, c := range cases {
		before := world.host.Blobs.Dump()
		code, stdout, stderr := runEvsDropReindexTest(world.settings(), append([]string{repo, job}, c.drops...)...)
		if code != 1 || !strings.Contains(stderr, c.want) || !strings.Contains(stderr, "report it") || stdout != "" {
			t.Errorf("%s: exited %d, printing %q and %q, want 1 and %q", c.label, code, stdout, stderr, c.want)
		}
		if !maps.EqualFunc(world.host.Blobs.Dump(), before, bytes.Equal) {
			t.Errorf("%s: a refused run changed the store", c.label)
		}
	}
}

// A snap-0 that covers no drop, as a job whose every drop its resolve refused writes it, gives a run nothing to take:
// with no list file it refuses, and any drop named is one it does not cover. A snap-0 naming a drop twice, which no
// job writes, is refused as one that does not read.
func TestEvsDropReindexRefusesASnapZeroThatCoversNoDropOrOneTwice(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	path := evsDropReindexTestPath("x")
	drop := world.drop(evsDropReindexTestId("drop x"), path, world.write(path, 2))
	world.snap()
	world.clock.Advance(time.Minute)
	repo, job := sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId)
	before := world.host.Blobs.Dump()
	code, stdout, stderr := runEvsDropReindexTest(world.settings(), repo, job)
	if code != 1 || !strings.Contains(stderr, "its snap-0 covers no drop") || stdout != "" {
		t.Fatalf("none named: exited %d, printing %q and %q", code, stdout, stderr)
	}
	code, stdout, stderr = runEvsDropReindexTest(world.settings(), repo, job, evsDropReindexTuple(drop))
	if code != 1 || !strings.Contains(stderr, "the drops named are not the 0 its snap-0 covers") || !strings.Contains(stderr, "the line names and it does not cover "+sidecar.EvsKeyId(drop.DropId)) || stdout != "" {
		t.Fatalf("one named: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !maps.EqualFunc(world.host.Blobs.Dump(), before, bytes.Equal) {
		t.Fatal("a refused run changed the store")
	}

	twice := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	twiceDrop := twice.drop(evsDropReindexTestId("drop x"), path, twice.write(path, 2))
	twice.snap(twiceDrop, twiceDrop)
	twice.clock.Advance(time.Minute)
	before = twice.host.Blobs.Dump()
	for _, drops := range [][]string{nil, {evsDropReindexTuple(twiceDrop)}} {
		code, stdout, stderr := runEvsDropReindexTest(twice.settings(), append([]string{sidecar.EvsKeyId(twice.repoId), sidecar.EvsKeyId(twice.jobId)}, drops...)...)
		if code != 1 || !strings.Contains(stderr, "its snap-0 does not read") || !strings.Contains(stderr, "twice") || stdout != "" {
			t.Errorf("a drop twice, %d named: exited %d, printing %q and %q", len(drops), code, stdout, stderr)
		}
	}
	if !maps.EqualFunc(twice.host.Blobs.Dump(), before, bytes.Equal) {
		t.Fatal("a refused run changed the store")
	}
}

// With a recording reindex: a run naming no drops asks Reindex with none first, and only on its answer that no list
// file reads whole asks again with snap-0's, in snap-0's order; any other error ends the run with no second ask; and
// an error from the second ask says what it changed, as any error does.
func TestEvsDropReindexAsksAgainWithSnapZerosDropsOnlyWhenNoListFileReadsWhole(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	drops := []*evsservice.DropListDrop{
		{DropId: evsDropReindexTestId("drop b"), RefId: evsDropReindexTestId("ref b"), Version: 7, PushId: evsDropReindexTestId("push b")},
		{DropId: evsDropReindexTestId("drop a"), RefId: evsDropReindexTestId("ref a"), Version: 9, PushId: evsDropReindexTestId("push a")},
	}
	world.snap(drops...)
	repo, job := sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId)

	noDrops := fmt.Errorf("%w: no list file reads whole", evsservice.ErrDropReindexNoDrops)
	reindexer := &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{Rewritten: []uint32{0}}}
	reindexer.onReindex = func(ctx context.Context, request *evsservice.DropReindexRequest) error {
		if request.Drops == nil {
			return noDrops
		}
		return nil
	}
	code, stdout, stderr := runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), repo, job)
	if code != 0 || !strings.Contains(stdout, "\ndrops 2 taken from the job's snap-0, no list file of it reading whole\ndrop "+evsDropReindexTuple(drops[0])+"\ndrop "+evsDropReindexTuple(drops[1])+"\nrewritten 0\n") {
		t.Fatalf("exited %d, printing %q and %q", code, stdout, stderr)
	}
	if len(reindexer.requests) != 2 || reindexer.requests[0].Drops != nil {
		t.Fatalf("%d requests, the first with %v", len(reindexer.requests), reindexer.requests[0].Drops)
	}
	second := reindexer.requests[1]
	if len(second.Drops) != 2 || *second.Drops[0] != *drops[0] || *second.Drops[1] != *drops[1] || second.Pace == nil ||
		second.Cluster.ClusterId() != evsDropReindexTestClusterId || second.SegmentCluster.ClusterId() != world.segment.ClusterId() || second.RepoId != world.repoId || second.JobId != world.jobId {
		t.Fatalf("the second request: %+v", second)
	}

	// a whole list file: one ask, snap-0 unread
	reindexer = &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{}}
	code, stdout, stderr = runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), repo, job)
	if code != 0 || len(reindexer.requests) != 1 || strings.Contains(stdout, "\ndrop") {
		t.Fatalf("exited %d after %d requests, printing %q and %q", code, len(reindexer.requests), stdout, stderr)
	}

	// another error: one ask
	reindexer = &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{}, err: errors.New("the store went away")}
	code, stdout, stderr = runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), repo, job)
	if code != 1 || len(reindexer.requests) != 1 || !strings.Contains(stderr, "stopped: the store went away") || stdout != "" {
		t.Fatalf("exited %d after %d requests, printing %q and %q", code, len(reindexer.requests), stdout, stderr)
	}

	// the second ask failing after a write says so, and prints nothing
	reindexer = &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{Written: []uint32{1}}}
	reindexer.onReindex = func(ctx context.Context, request *evsservice.DropReindexRequest) error {
		if request.Drops == nil {
			return noDrops
		}
		reindexer.err = errors.New("the store went away")
		return nil
	}
	code, stdout, stderr = runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), repo, job)
	if code != 1 || len(reindexer.requests) != 2 || !strings.Contains(stderr, "stopped after writing list files 1: the store went away") || stdout != "" {
		t.Fatalf("exited %d after %d requests, printing %q and %q", code, len(reindexer.requests), stdout, stderr)
	}
}

// A list file that is gone, as a run stopped between its DELETE and its write leaves it, is written fresh from the
// named drops, and printed as written.
func TestEvsDropReindexWritesAMissingListFileFresh(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	path := evsDropReindexTestPath("x")
	drop := world.drop(evsDropReindexTestId("drop x"), path, world.write(path, 1))
	world.resolve(drop)
	original := world.listFile(0)
	if err := world.segment.Delete(world.ctx, evsservice.DropFileKey(world.repoId, world.jobId, evsservice.DropFileList, 0)); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId), evsDropReindexTuple(drop))
	if code != 0 || !strings.Contains(stdout, "\nrewritten none\nwritten 0\nstray none\n") {
		t.Fatalf("exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(world.listFile(0), original) {
		t.Fatal("the list file was written with bytes other than resolve's")
	}
}

// Reindex's refusals exit 1 with its words on stderr, print nothing to stdout and change nothing in the store: a
// named drop that does not resolve on its ref's history, and a corrupt file on a segment cluster that has not passed
// the settle cases, which is never deleted.
func TestEvsDropReindexExitsNonZeroAndWritesNothingWhenReindexRefuses(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	path := evsDropReindexTestPath("x")
	drop := world.drop(evsDropReindexTestId("drop x"), path, world.write(path, 2))
	world.resolve(drop)
	world.corruptListFile(0)
	world.clock.Advance(time.Minute)

	past := *drop
	past.Version = drop.Version + 1
	notDrop := *drop
	notDrop.Version = drop.Version - 1
	cases := []struct {
		label string
		drop  *evsservice.DropListDrop
		edit  func()
		want  string
	}{
		{label: "a version past the ref's head", drop: &past, edit: func() {}, want: "no longer resolves"},
		{label: "a version that is no DROP", drop: &notDrop, edit: func() {}, want: "no longer resolves"},
		{label: "another push's id", drop: &evsservice.DropListDrop{DropId: drop.DropId, RefId: drop.RefId, Version: drop.Version, PushId: evsDropReindexTestId("another push")}, edit: func() {}, want: "no longer resolves"},
		{label: "the segment cluster unsettled", drop: drop, edit: func() { world.host.Blobs.SetSettlePassed(world.segment.ClusterId(), false) }, want: "has not passed the settle cases"},
	}
	for _, c := range cases {
		c.edit()
		before := world.host.Blobs.Dump()
		code, stdout, stderr := runEvsDropReindexTest(world.settings(), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId), evsDropReindexTuple(c.drop))
		if code != 1 || !strings.Contains(stderr, c.want) || !strings.Contains(stderr, "stopped: ") {
			t.Errorf("%s: exited %d with %q, want 1 and %q", c.label, code, stderr, c.want)
		}
		if stdout != "" {
			t.Errorf("%s: printed %q", c.label, stdout)
		}
		if !maps.EqualFunc(world.host.Blobs.Dump(), before, bytes.Equal) {
			t.Errorf("%s: a refused run changed the store", c.label)
		}
	}
}

// What a run hands Reindex, recorded: the request, and the answer it gives.
type recordingEvsDropReindexer struct {
	requests []*evsservice.DropReindexRequest
	result   *evsservice.DropReindexResult
	err      error
	// called with each request before it answers
	onReindex func(ctx context.Context, request *evsservice.DropReindexRequest) error
}

// See evsDropReindexer.
func (self *recordingEvsDropReindexer) Reindex(ctx context.Context, request *evsservice.DropReindexRequest) (*evsservice.DropReindexResult, error) {
	self.requests = append(self.requests, request)
	if self.onReindex != nil {
		if err := self.onReindex(ctx, request); err != nil {
			return nil, err
		}
	}
	return self.result, self.err
}

// Settings on world's clusters whose reindex is reindexer.
func recordingEvsDropReindexSettings(world *evsDropReindexWorld, reindexer *recordingEvsDropReindexer) *evsDropReindexSettings {
	settings := world.settings()
	settings.newReindexer = func(blobs sidecar.BlobStore, dropSettings *evsservice.DropSettings) (evsDropReindexer, error) {
		world.reindexCount += 1
		return reindexer, nil
	}
	return settings
}

// The request Reindex gets: the metadata cluster place/ names and the segment cluster, the repo and job, the drops in
// the order named whichever form each id was written in, and a pace. A line naming no drops passes nil, so Reindex
// reads them from the job's list files rather than taking an empty list.
func TestEvsDropReindexPassesWhatTheLineNamesToReindex(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	reindexer := &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{}}
	settings := recordingEvsDropReindexSettings(world, reindexer)
	drops := []*evsservice.DropListDrop{
		{DropId: evsDropReindexTestId("drop b"), RefId: evsDropReindexTestId("ref b"), Version: 7, PushId: evsDropReindexTestId("push b")},
		{DropId: evsDropReindexTestId("drop a"), RefId: evsDropReindexTestId("ref a"), Version: 1 << 40, PushId: evsDropReindexTestId("push a")},
	}
	// the second drop written with uuid-form ids, as server.Id prints them
	secondTuple := fmt.Sprintf("%s:%s:%d:%s", drops[1].DropId.String(), drops[1].RefId.String(), drops[1].Version, drops[1].PushId.String())
	code, stdout, stderr := runEvsDropReindexTest(settings, world.repoId.String(), sidecar.EvsKeyId(world.jobId), evsDropReindexTuple(drops[0]), secondTuple)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	code, _, stderr = runEvsDropReindexTest(settings, sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if len(reindexer.requests) != 2 {
		t.Fatalf("%d requests", len(reindexer.requests))
	}
	for i, request := range reindexer.requests {
		if request.Cluster.ClusterId() != evsDropReindexTestClusterId || request.SegmentCluster.ClusterId() != world.segment.ClusterId() {
			t.Errorf("request %d on %s and %s, want %s, which place/ names, and %s", i, request.Cluster.ClusterId(), request.SegmentCluster.ClusterId(), evsDropReindexTestClusterId, world.segment.ClusterId())
		}
		if request.RepoId != world.repoId || request.JobId != world.jobId || request.Pace == nil {
			t.Errorf("request %d: repo %s job %s pace %t", i, request.RepoId, request.JobId, request.Pace != nil)
		}
	}
	named := reindexer.requests[0].Drops
	if len(named) != 2 || *named[0] != *drops[0] || *named[1] != *drops[1] {
		t.Fatalf("named drops reached Reindex as %v, want %v", named, drops)
	}
	if reindexer.requests[1].Drops != nil {
		t.Fatalf("a line naming no drops passed %v, want nil", reindexer.requests[1].Drops)
	}
	if !strings.Contains(stdout, "\nrewritten none\nwritten none\nstray none\nscanned_bytes 0\n") {
		t.Fatalf("printed %q", stdout)
	}
}

// What Reindex answers is printed as it is: several sequences of each kind, and a stray file with the note that it was
// left as it is.
func TestEvsDropReindexPrintsEachSequenceAndTheStray(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	reindexer := &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{
		Rewritten: []uint32{0, 2}, Written: []uint32{1}, Stray: []uint32{3, 4}, ScanByteCount: 123456,
	}}
	code, stdout, stderr := runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	for _, want := range []string{"\nrewritten 0 2\n", "\nwritten 1\n", "\nstray 3 4\n", "\nscanned_bytes 123456\n", "left as it is"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("printed %q, want %q in it", stdout, want)
		}
	}
}

// An error after some files changed says which, before Reindex's words, and exits 1 printing nothing to stdout; one
// before any changed says only that the run stopped.
func TestEvsDropReindexSaysWhatChangedBeforeAnError(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	cases := []struct {
		result *evsservice.DropReindexResult
		want   string
	}{
		{result: &evsservice.DropReindexResult{Rewritten: []uint32{0}, Written: []uint32{2}}, want: "stopped after rewriting list files 0 and writing list files 2: the store went away"},
		{result: &evsservice.DropReindexResult{Written: []uint32{1}}, want: "stopped after writing list files 1: the store went away"},
		{result: &evsservice.DropReindexResult{}, want: "stopped: the store went away"},
		{result: nil, want: "stopped: the store went away"},
	}
	for _, c := range cases {
		reindexer := &recordingEvsDropReindexer{result: c.result, err: errors.New("the store went away")}
		code, stdout, stderr := runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
		if code != 1 || !strings.Contains(stderr, c.want) || stdout != "" {
			t.Errorf("%+v: exited %d, printing %q and %q, want 1 and %q", c.result, code, stdout, stderr, c.want)
		}
	}
	// a reindex that answers nothing at all is an error too, never a run that did nothing
	reindexer := &recordingEvsDropReindexer{}
	code, stdout, stderr := runEvsDropReindexTest(recordingEvsDropReindexSettings(world, reindexer), sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 1 || !strings.Contains(stderr, "answered no result") || stdout != "" {
		t.Fatalf("exited %d, printing %q and %q", code, stdout, stderr)
	}
}

// A line the command cannot take is a usage mistake, exit 2 with the usage, and opens no cluster and builds no reindex:
// too few ids, an id that does not parse or is zero, a drop that is not four fields, a version that is not a positive
// decimal, a drop named twice and a flag the command does not have.
func TestEvsDropReindexRefusesCommandLinesItCannotTake(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	repo := sidecar.EvsKeyId(world.repoId)
	job := sidecar.EvsKeyId(world.jobId)
	drop := &evsservice.DropListDrop{DropId: evsDropReindexTestId("drop"), RefId: evsDropReindexTestId("ref"), Version: 3, PushId: evsDropReindexTestId("push")}
	tuple := evsDropReindexTuple(drop)
	dropId, refId, pushId := sidecar.EvsKeyId(drop.DropId), sidecar.EvsKeyId(drop.RefId), sidecar.EvsKeyId(drop.PushId)
	zero := sidecar.EvsKeyId(sidecar.Id{})
	cases := [][]string{
		{},
		{repo},
		{"not-an-id", job},
		{repo, job[:31]},
		{repo, job + "0"},
		{repo, strings.Replace(job, job[:1], "g", 1)},
		{zero, job},
		{repo, zero},
		{repo, job, dropId + ":" + refId + ":3"},
		{repo, job, tuple + ":" + pushId},
		{repo, job, ""},
		{repo, job, dropId + ":" + refId + ":0:" + pushId},
		{repo, job, dropId + ":" + refId + ":x:" + pushId},
		{repo, job, dropId + ":" + refId + ":-3:" + pushId},
		{repo, job, dropId + ":" + refId + ":+3:" + pushId},
		{repo, job, dropId + ":" + refId + ":18446744073709551616:" + pushId},
		{repo, job, "x:" + refId + ":3:" + pushId},
		{repo, job, dropId + ":" + zero + ":3:" + pushId},
		{repo, job, dropId + ":" + refId + ":3:" + pushId[:30]},
		{repo, job, tuple, tuple},
		{repo, job, tuple, dropId + ":" + refId + ":4:" + pushId},
		{repo, job, "--dry-run"},
	}
	for _, args := range cases {
		reindexCount := world.reindexCount
		openCount := world.openCount
		code, stdout, stderr := runEvsDropReindexTest(world.settings(), args...)
		if code != 2 || !strings.Contains(stderr, "sidecarctl evs drop-reindex <repo_id> <job_id> [<drop_id>:<ref_id>:<version>:<push_id>...]") {
			t.Errorf("%q: exited %d with %q, want 2 and the usage", args, code, stderr)
		}
		if stdout != "" || world.openCount != openCount || world.reindexCount != reindexCount {
			t.Errorf("%q: printed %q, opened %d and built %d reindexes", args, stdout, world.openCount-openCount, world.reindexCount-reindexCount)
		}
	}
}

// A repo place/ does not name, or names a cluster the env's vault file does not reach, exits 1 before any reindex is
// built; so do clusters that do not open, and a run whose bound has passed opens nothing.
func TestEvsDropReindexRefusesWhenTheRepoOrItsClustersDoNotResolve(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	unplacedRepo := evsDropReindexTestId("unplaced repo")
	elsewhereRepo := evsDropReindexTestId("elsewhere repo")
	firstCluster, err := world.host.Blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: "meta-9", CreatorSidecarId: world.signerId}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstCluster.PutIfNoneMatch(world.paidCtx, sidecar.EvsPlaceKey(elsewhereRepo), placeBytes, nil); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label      string
		repoId     sidecar.Id
		edit       func(settings *evsDropReindexSettings)
		want       string
		wantOpened bool
	}{
		{label: "an unplaced repo", repoId: unplacedRepo, edit: func(settings *evsDropReindexSettings) {}, want: "is not placed", wantOpened: true},
		{label: "a cluster not reached", repoId: elsewhereRepo, edit: func(settings *evsDropReindexSettings) {}, want: "file-clusters-taskworker.yml reaches", wantOpened: true},
		{
			label:  "clusters that do not open",
			repoId: world.repoId,
			edit: func(settings *evsDropReindexSettings) {
				settings.openBlobs = func(ctx context.Context) (sidecar.BlobStore, func(), error) {
					world.openCount += 1
					return nil, nil, errors.New("the vault holds no file-clusters-taskworker.yml")
				}
			},
			want:       "did not open, so nothing was read: the vault holds no file-clusters-taskworker.yml",
			wantOpened: true,
		},
		{
			label:  "clusters that open as nothing",
			repoId: world.repoId,
			edit: func(settings *evsDropReindexSettings) {
				settings.openBlobs = func(ctx context.Context) (sidecar.BlobStore, func(), error) {
					world.openCount += 1
					return nil, nil, nil
				}
			},
			want:       "opened as no store",
			wantOpened: true,
		},
		{label: "a bound passed", repoId: world.repoId, edit: func(settings *evsDropReindexSettings) { settings.timeout = 0 }, want: "bound passed"},
		{label: "drop settings under their floors", repoId: world.repoId, edit: func(settings *evsDropReindexSettings) { settings.dropSettings = &evsservice.DropSettings{} }, want: "nothing was read"},
	}
	for _, c := range cases {
		reindexer := &recordingEvsDropReindexer{result: &evsservice.DropReindexResult{}}
		settings := recordingEvsDropReindexSettings(world, reindexer)
		c.edit(settings)
		openCount := world.openCount
		code, stdout, stderr := runEvsDropReindexTest(settings, sidecar.EvsKeyId(c.repoId), sidecar.EvsKeyId(world.jobId))
		if code != 1 || !strings.Contains(stderr, c.want) || stdout != "" {
			t.Errorf("%s: exited %d, printing %q and %q, want 1 and %q", c.label, code, stdout, stderr, c.want)
		}
		if len(reindexer.requests) != 0 {
			t.Errorf("%s: reindexed", c.label)
		}
		if (world.openCount != openCount) != c.wantOpened {
			t.Errorf("%s: opened the clusters %d times", c.label, world.openCount-openCount)
		}
	}
}

// A clock recording how long each wait asked for, whose After fires at once until neverFire is set, and then never.
type evsDropReindexRecordingClock struct {
	stateLock sync.Mutex
	waits     []time.Duration
	neverFire bool
}

// See sidecar.Clock.
func (self *evsDropReindexRecordingClock) Now() time.Time {
	return sidecartest.DefaultClockStart()
}

// See sidecar.Clock.
func (self *evsDropReindexRecordingClock) After(timeout time.Duration) <-chan time.Time {
	neverFire := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.waits = append(self.waits, timeout)
		neverFire = self.neverFire
	}()
	fired := make(chan time.Time, 1)
	if !neverFire {
		fired <- self.Now()
	}
	return fired
}

// See sidecar.Clock. Nothing here schedules a call, so it is called at once.
func (self *evsDropReindexRecordingClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	function()
	return func() bool { return false }
}

// The pace Reindex is handed waits as long as the bytes take at the drop job's ReadBytesPerSecond, its 32 MB/s by
// default, as the job's own pace does; nothing for no bytes; and a wait the clock never ends ends with the run's
// context, with its error.
func TestEvsDropReindexPacesReadsAtTheDropJobsRate(t *testing.T) {
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(1))
	clock := &evsDropReindexRecordingClock{}
	paceErrs := []error{}
	reindexer := &recordingEvsDropReindexer{
		result: &evsservice.DropReindexResult{},
		onReindex: func(ctx context.Context, request *evsservice.DropReindexRequest) error {
			for _, byteCount := range []int64{32 * 1000 * 1000, 0, 16 * 1000 * 1000, 1} {
				paceErrs = append(paceErrs, request.Pace(ctx, byteCount))
			}
			func() {
				clock.stateLock.Lock()
				defer clock.stateLock.Unlock()
				clock.neverFire = true
			}()
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			paceErrs = append(paceErrs, request.Pace(cancelled, 32*1000*1000))
			return nil
		},
	}
	settings := recordingEvsDropReindexSettings(world, reindexer)
	settings.dropSettings = evsservice.DefaultDropSettings()
	settings.clock = clock
	code, _, stderr := runEvsDropReindexTest(settings, sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId))
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	// one byte at 32 MB/s is 31.25 ns, which a Duration holds as 31
	if want := []time.Duration{time.Second, 500 * time.Millisecond, 31 * time.Nanosecond, time.Second}; !slices.Equal(clock.waits, want) {
		t.Fatalf("waited %v, want %v", clock.waits, want)
	}
	if len(paceErrs) != 5 || slices.ContainsFunc(paceErrs[:4], func(err error) bool { return err != nil }) || !errors.Is(paceErrs[4], context.Canceled) {
		t.Fatalf("the paces answered %v, want four nils and the run's cancellation", paceErrs)
	}
}

// The binary as built reaches the env its WARP_ENV names, and with none it exits 1 before it opens a cluster.
func TestEvsDropReindexRefusesWithNoWarpEnv(t *testing.T) {
	t.Setenv("WARP_ENV", "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	repoId := sidecar.EvsKeyId(evsDropReindexTestId("repo"))
	jobId := sidecar.EvsKeyId(evsDropReindexTestId("job"))
	if code := sidecarctlCommands.run([]string{"evs", "drop-reindex", repoId, jobId}, stdout, stderr); code != 1 {
		t.Fatalf("exited %d, want 1: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARP_ENV is not set") || !strings.Contains(stderr.String(), "nothing was read") || stdout.Len() != 0 {
		t.Fatalf("printed %q and %q", stdout.String(), stderr.String())
	}
}

// The binary's own registry holds evs drop-reindex under the usage owner decision 512 gives, beside evs purge-turn-on,
// and the default settings are the drop job's own.
func TestSidecarctlRegistersEvsDropReindex(t *testing.T) {
	command, args := sidecarctlCommands.find([]string{"evs", "drop-reindex", "a", "b", "c:d:1:e"})
	if command == nil || command.words != "evs drop-reindex" || command.usage != "<repo_id> <job_id> [<drop_id>:<ref_id>:<version>:<push_id>...]" ||
		!slices.Equal(args, []string{"a", "b", "c:d:1:e"}) {
		t.Fatalf("found %v with %v", command, args)
	}
	if other, _ := sidecarctlCommands.find([]string{"evs", "purge-turn-on", "test"}); other == nil || other.words != "evs purge-turn-on" {
		t.Fatalf("evs purge-turn-on is %v", other)
	}
	settings := defaultEvsDropReindexSettings()
	if *settings.dropSettings != *evsservice.DefaultDropSettings() || settings.timeout <= 0 {
		t.Fatalf("default settings %+v", settings)
	}
}
