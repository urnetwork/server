// Exact-object fake retention forces an unknown old PUT to complete after a
// later successful archive. No timing, service, MinIO or production data is used.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// Stored versions and latest pointers are independent, as in immutable MinIO.
type competitionArchiveRaceObject struct {
	key     string
	content []byte
}

// The first manifest PUT returns unknown but its backend commit remains owned
// until Close joins it. Ordinary object versions are authenticated immediately.
type competitionArchiveRaceStore struct {
	server.RetainedBlobStore
	stateLock   sync.Mutex
	next        int
	versions    map[string]competitionArchiveRaceObject
	latest      map[string]string
	delayNext   bool
	delayed     bool
	pending     chan competitionArchiveRaceObject
	release     chan struct{}
	done        chan struct{}
	releaseOnce sync.Once
}

// A bounded test store has exactly one deliberately delayed remote commit.
func newCompetitionArchiveRaceStore(t testing.TB) *competitionArchiveRaceStore {
	t.Helper()
	store := &competitionArchiveRaceStore{versions: map[string]competitionArchiveRaceObject{}, latest: map[string]string{}, delayNext: true,
		pending: make(chan competitionArchiveRaceObject, 1), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(store.Close)
	return store
}

// All synthetic keys share a visibly non-production retention namespace.
func (self *competitionArchiveRaceStore) Prefix() string { return "synthetic-retention" }

// A real compliance mode is modeled; no local-store exemption hides the proof.
func (self *competitionArchiveRaceStore) Bucket() string { return "synthetic-bucket" }

// A new immutable object version can change metadata without changing bytes.
func (self *competitionArchiveRaceStore) commit(key string, content []byte, until time.Time) *server.BlobRetention {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.next++
	version := fmt.Sprintf("synthetic-version-%d", self.next)
	self.versions[version] = competitionArchiveRaceObject{key: key, content: bytes.Clone(content)}
	self.latest[key] = version
	return &server.BlobRetention{Key: key, VersionId: version, Size: int64(len(content)), Mode: "COMPLIANCE", RetainUntil: until}
}

// The exact lost-ack ordering is explicit, not a guessed network timeout.
func (self *competitionArchiveRaceStore) PutRetained(_ context.Context, key, path, _ string, until time.Time) (*server.BlobRetention, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	delay := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.delayNext || !strings.HasSuffix(key, "/artifact-manifest.json") {
			return false
		}
		self.delayNext, self.delayed = false, true
		return true
	}()
	if delay {
		self.pending <- competitionArchiveRaceObject{key: key, content: bytes.Clone(content)}
		go func() { <-self.release; self.commit(key, content, until); close(self.done) }()
		return nil, context.DeadlineExceeded
	}
	return self.commit(key, content, until), nil
}

// The trusted archive verifier must select the exact uploaded version.
func (self *competitionArchiveRaceStore) GetVersion(_ context.Context, key, version string) (io.ReadCloser, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	object, present := self.versions[version]
	if !present || object.key != key {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(object.content))), nil
}

// Review packets intentionally read the persisted manifest key's latest body.
func (self *competitionArchiveRaceStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	object, present := self.versions[self.latest[key]]
	if !present {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(object.content))), nil
}

// Every delayed backend write is joined, including assertion-failure cleanup.
func (self *competitionArchiveRaceStore) Close() {
	self.releaseOnce.Do(func() { close(self.release) })
	self.stateLock.Lock()
	delayed := self.delayed
	self.stateLock.Unlock()
	if delayed {
		<-self.done
	}
}

// Both a superseded owner and a retry of the same owner can have an unknown
// archival PUT. Neither may alias the manifest of the later committed outcome.
func TestCompetitionDistributedLateArchiveCannotReplaceCurrentManifest(t *testing.T) {
	for _, nextFence := range []int64{1, 2} {
		settings := validSettings()
		settings.CompetitionId = "synthetic-archive-race"
		settings.RetainUntil = server.NowUtc().Add(24 * time.Hour).Truncate(time.Second)
		store := newCompetitionArchiveRaceStore(t)
		archive := &blobArtifactArchive{store: store}
		job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest: testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest()}, AttemptCount: 1, ExecutorHostId: "synthetic-executor", SlotId: 1, SlotFencingToken: 1}
		directory := t.TempDir()
		patch := writeArchiveTestFile(t, directory, "canonical.patch", []byte("synthetic patch\n"))
		result := writeArchiveTestFile(t, directory, "worker-result.json", []byte("{\"schema\":1}\n"))
		stderr := writeArchiveTestFile(t, directory, "worker.stderr.log", nil)
		job.PatchSha256 = patch.Sha256
		manifest := artifactManifest{Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), Attempt: 1, EvaluatorImageDigest: job.EvaluatorImageDigest,
			ApiImageDigest: job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest, PatchSha256: patch.Sha256, ResultSha256: result.Sha256, StderrSha256: stderr.Sha256,
			Executor: &competitionExecutorArtifactIdentity{HostId: job.ExecutorHostId, SlotId: job.SlotId, SlotFencingToken: 1, RunId: competitionagent.RunId(job.JobId.String(), 0, 1)}}
		if _, err := archive.ArchiveAttempt(t.Context(), settings, job, directory, manifest); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("first archive did not reach the lost-acknowledgement barrier", err)
		}
		old := <-store.pending
		currentJob, currentManifest := *job, manifest
		currentJob.SlotFencingToken = nextFence
		executor := *manifest.Executor
		executor.SlotFencingToken = nextFence
		currentManifest.Executor = &executor
		currentBytes, err := archive.ArchiveAttempt(t.Context(), settings, &currentJob, directory, currentManifest)
		if err != nil {
			t.Fatal(err)
		}
		retained := artifactManifest{}
		if err := json.Unmarshal(currentBytes, &retained); err != nil || retained.Retention == nil {
			t.Fatal("current manifest was not retained", err)
		}
		if old.key == retained.Retention.ManifestKey {
			t.Fatal("unknown old PUT aliases the current immutable manifest key")
		}
		store.Close()
		reader, err := store.Get(t.Context(), retained.Retention.ManifestKey)
		if err != nil {
			t.Fatal(err)
		}
		latest, readErr := io.ReadAll(reader)
		if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(latest, currentBytes) {
			t.Fatal("late old archival changed the current review manifest", err)
		}
	}
}

// Historical epoch-one/manual-recovery keys retain their exact byte layout.
func TestCompetitionDistributedArchiveLeavesLegacyPrefixUnchanged(t *testing.T) {
	store := newCompetitionArchiveRaceStore(t)
	archive := &blobArtifactArchive{store: store}
	settings := validSettings()
	settings.CompetitionId = "synthetic-legacy-archive"
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId()}, AttemptCount: 2}
	expected := filepath.ToSlash(filepath.Join(store.Prefix(), "competition", "v1", settings.CompetitionId, "rounds", job.RoundId.String(), "jobs", job.JobId.String(), "attempt-02"))
	if actual := archive.attemptPrefix(settings, job); actual != expected {
		t.Fatalf("legacy archive prefix changed: got %s want %s", actual, expected)
	}
}
