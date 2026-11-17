// The mover and the readers against a real MinIO: a throwaway container this
// test starts with its own random credentials on a loopback port, so no
// request can reach a production store. Docker must be reachable; without it
// the tests skip and say so.
package evidencemove

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/server"
)

// MinIO RELEASE.2026-11-16T00-11-40Z, pinned by digest. MinIO publishes no
// community images since 2025; this build is Chainguard's from source.
const testMinioImage = "cgr.dev/chainguard/minio@sha256:59953701f8f8d0de81aab456f0b0b6c0a1804119415559bf7ede79d944108879"

type testMinio struct {
	authority string
	access    string
	secret    string
	client    *minio.Client
}

func randomTestHex(t testing.TB, bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(value)
}

func startTestMinio(t *testing.T) *testMinio {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not installed: the MinIO container tests need a container runtime")
	}
	if output, err := exec.Command(docker, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("the docker daemon is unreachable: %v %s", err, bytes.TrimSpace(output))
	}
	name := "evidencemove-test-" + randomTestHex(t, 6)
	access := "evidencemove" + randomTestHex(t, 4)
	secret := randomTestHex(t, 20)
	run := exec.Command(
		docker, "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER="+access, "-e", "MINIO_ROOT_PASSWORD="+secret,
		testMinioImage, "server", "/tmp/data",
	)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run %s: %v %s", testMinioImage, err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command(docker, "rm", "-f", name).Run()
	})
	output, err := exec.Command(docker, "port", name, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	authority := strings.TrimSpace(strings.Split(string(output), "\n")[0])
	if !strings.HasPrefix(authority, "127.0.0.1:") {
		t.Fatalf("the container's port %q is not on loopback", authority)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		response, err := http.Get("http://" + authority + "/minio/health/live")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("MinIO at %s did not become live: %v", authority, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	client, err := minio.New(authority, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return &testMinio{authority: authority, access: access, secret: secret, client: client}
}

// A fresh bucket and the blob configuration naming it.
func (self *testMinio) bucket(t *testing.T, versioned bool) *server.BlobStoreConfig {
	t.Helper()
	bucket := "evidence-" + randomTestHex(t, 4)
	if err := self.client.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if versioned {
		if err := self.client.EnableVersioning(t.Context(), bucket); err != nil {
			t.Fatal(err)
		}
	}
	return &server.BlobStoreConfig{
		Authority: self.authority,
		AccessKey: self.access,
		SecretKey: self.secret,
		Bucket:    bucket,
		Prefix:    testPrefix,
		Region:    "us-east-1",
	}
}

func (self *testMinio) put(t *testing.T, config *server.BlobStoreConfig, key string, data []byte) {
	t.Helper()
	_, err := self.client.PutObject(t.Context(), config.Bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/json", DisableMultipart: true})
	if err != nil {
		t.Fatal(err)
	}
}

// The object at key, or nil when the key holds none.
func (self *testMinio) get(t *testing.T, config *server.BlobStoreConfig, key string) ([]byte, *minio.ObjectInfo) {
	t.Helper()
	info, err := self.client.StatObject(t.Context(), config.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, nil
		}
		t.Fatal(err)
	}
	object, err := self.client.GetObject(t.Context(), config.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	data := new(bytes.Buffer)
	if _, err := data.ReadFrom(object); err != nil {
		t.Fatal(err)
	}
	return data.Bytes(), &info
}

func newTestMinioMover(t *testing.T, minioServer *testMinio, config *server.BlobStoreConfig, store ObjectStore, items []Item, checkpointPath string) *Mover {
	t.Helper()
	latency := NewLatencies()
	health := &MinioHealthGate{Url: "http://" + minioServer.authority + "/minio/health/cluster", MaxLatency: 5 * time.Second, Latency: latency}
	keeper := NewGateKeeper(t.Logf, newTestGate(true), health)
	keeper.CheckNow(t.Context())
	if open, reason := keeper.Open(); !open {
		t.Fatalf("gates closed: %s", reason)
	}
	keeper.minBackoff, keeper.maxBackoff = 10*time.Millisecond, 40*time.Millisecond
	checkpoint, err := OpenCheckpoint(checkpointPath, ClientKeyHistorySourceName, config.Bucket, config.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { checkpoint.Close() })
	return &Mover{
		Store:      store,
		Prefix:     config.Prefix,
		Source:     NewSliceSource(items),
		Gates:      keeper,
		Pacer:      NewPacer(2000),
		Checkpoint: checkpoint,
		Settings:   testSettings(),
		Logf:       t.Logf,
		Latency:    latency,
	}
}

// A store that cancels the run right after the copy of one key: the process
// dies between the copy and the removal.
type crashingStore struct {
	ObjectStore
	key   string
	crash func()
}

func (self *crashingStore) Copy(ctx context.Context, src ObjectInfo, dstKey string) error {
	err := self.ObjectStore.Copy(ctx, src, dstKey)
	if dstKey == self.key {
		self.crash()
	}
	return err
}

func TestMinio(t *testing.T) {
	minioServer := startTestMinio(t)

	// Every layout: flat only moves, both deduplicates, sharded only and
	// neither are left, and a sharded object holding other bytes keeps the
	// flat object. Afterwards every stored hash reads through the readers
	// (startifact.OpenEvidenceContent) from its sharded key, its copy has the
	// flat object's ETag, and Verify accepts the journal.
	t.Run("moves every layout end to end", func(t *testing.T) {
		config := minioServer.bucket(t, false)
		store, err := NewMinioObjectStore(config, 8)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Preflight(t.Context()); err != nil {
			t.Fatal(err)
		}
		evidence := testEvidenceSet(t, 25)
		conflict := newTestEvidence(t, 100)
		stranger := newTestEvidence(t, 101)
		flatEtags := map[string]string{}
		for index, entry := range evidence {
			keys := testKeys(t, entry.hash)
			switch index % 5 {
			case 0, 1, 2:
				minioServer.put(t, config, keys.Flat, entry.bytes)
				flatEtags[entry.hash] = md5Hex(entry.bytes)
			case 3:
				minioServer.put(t, config, keys.Flat, entry.bytes)
				minioServer.put(t, config, keys.Sharded, entry.bytes)
			case 4:
				minioServer.put(t, config, keys.Sharded, entry.bytes)
			}
		}
		conflictKeys := testKeys(t, conflict.hash)
		minioServer.put(t, config, conflictKeys.Flat, conflict.bytes)
		minioServer.put(t, config, conflictKeys.Sharded, stranger.bytes)
		absent := []testEvidence{newTestEvidence(t, 200), newTestEvidence(t, 201), newTestEvidence(t, 202)}
		items := testItems(append(append(append([]testEvidence{}, evidence...), conflict), absent...))
		directory := t.TempDir()
		mover := newTestMinioMover(t, minioServer, config, store, items, filepath.Join(directory, "checkpoint.json"))
		journal, err := OpenJSONL(filepath.Join(directory, "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		mover.Journal = journal
		report, err := mover.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := (Counts{Items: 29, Moved: 15, Deduped: 5, Done: 5, Absent: 3, Refused: 1}); report.Counts != want {
			t.Fatalf("counts %+v, want %+v", report.Counts, want)
		}
		for index, entry := range evidence {
			keys := testKeys(t, entry.hash)
			if flat, _ := minioServer.get(t, config, keys.Flat); flat != nil {
				t.Fatalf("%d: the flat object is still stored", index)
			}
			sharded, info := minioServer.get(t, config, keys.Sharded)
			if !bytes.Equal(sharded, entry.bytes) {
				t.Fatalf("%d: the sharded object is missing or altered", index)
			}
			if etag, moved := flatEtags[entry.hash]; moved && strings.Trim(info.ETag, `"`) != etag {
				t.Fatalf("%d: the copy's ETag %s is not the flat object's %s", index, info.ETag, etag)
			}
		}
		if flat, _ := minioServer.get(t, config, conflictKeys.Flat); !bytes.Equal(flat, conflict.bytes) {
			t.Fatal("the flat object of a refused move is gone")
		}
		if sharded, _ := minioServer.get(t, config, conflictKeys.Sharded); !bytes.Equal(sharded, stranger.bytes) {
			t.Fatal("a sharded object the run did not write was changed")
		}
		reader, err := server.NewBlobStore(config)
		if err != nil {
			t.Fatal(err)
		}
		for index, entry := range evidence {
			if err := ReadThroughReaders(t.Context(), reader, entry.hash, 1<<20); err != nil {
				t.Fatalf("%d: %v", index, err)
			}
		}
		journal.Close()
		moved, err := ReadJournalRemovals(filepath.Join(directory, "journal.jsonl"))
		if err != nil || len(moved) != 20 {
			t.Fatalf("journal removals %d, %v", len(moved), err)
		}
		verified, err := Verify(t.Context(), store, reader, config.Prefix, moved, nil, mover.Gates, NewPacer(1000), 4, 1<<20)
		if err != nil || verified.Readable != 20 || verified.FlatGone != 20 || len(verified.Failures) != 0 {
			t.Fatalf("verify %+v, %v", verified, err)
		}
		// a rerun moves nothing more
		rerun := newTestMinioMover(t, minioServer, config, store, items, filepath.Join(directory, "rerun.json"))
		report, err = rerun.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := (Counts{Items: 29, Done: 25, Absent: 3, Refused: 1}); report.Counts != want {
			t.Fatalf("rerun counts %+v, want %+v", report.Counts, want)
		}
	})

	// The readers find content in each layout through real MinIO, which
	// reports a missing key only at the first read: flat only (the fallback),
	// both, and sharded only; content stored nowhere is not found.
	t.Run("readers find content in every layout", func(t *testing.T) {
		config := minioServer.bucket(t, false)
		reader, err := server.NewBlobStore(config)
		if err != nil {
			t.Fatal(err)
		}
		evidence := testEvidenceSet(t, 4)
		layouts := []string{"flat only", "both", "sharded only", "neither"}
		for index, entry := range evidence {
			keys := testKeys(t, entry.hash)
			if index == 0 || index == 1 {
				minioServer.put(t, config, keys.Flat, entry.bytes)
			}
			if index == 1 || index == 2 {
				minioServer.put(t, config, keys.Sharded, entry.bytes)
			}
		}
		for index, entry := range evidence {
			err := ReadThroughReaders(t.Context(), reader, entry.hash, 1<<20)
			if index < 3 && err != nil {
				t.Fatalf("%s: %v", layouts[index], err)
			}
			if index == 3 && (err == nil || !server.IsBlobNotFound(err)) {
				t.Fatalf("%s: %v, want not found", layouts[index], err)
			}
		}
	})

	// The process dies after one object's copy, before its removal, and its
	// last checkpoint write never happened. A new run from the durable
	// checkpoint verifies the orphaned copy, removes its flat object and
	// finishes the rest: no object is lost.
	t.Run("resumes after a crash mid-batch", func(t *testing.T) {
		config := minioServer.bucket(t, false)
		store, err := NewMinioObjectStore(config, 8)
		if err != nil {
			t.Fatal(err)
		}
		evidence := testEvidenceSet(t, 30)
		for _, entry := range evidence {
			minioServer.put(t, config, testKeys(t, entry.hash).Flat, entry.bytes)
		}
		items := testItems(evidence)
		checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
		ctx, cancel := context.WithCancel(t.Context())
		var durable []byte
		crashing := &crashingStore{ObjectStore: store, key: testKeys(t, evidence[17].hash).Sharded, crash: func() {
			durable, _ = os.ReadFile(checkpointPath)
			cancel()
		}}
		mover := newTestMinioMover(t, minioServer, config, crashing, items, checkpointPath)
		if _, err := mover.Run(ctx); err != nil {
			t.Fatal(err)
		}
		mover.Checkpoint.Close()
		crashed := testKeys(t, evidence[17].hash)
		if flat, _ := minioServer.get(t, config, crashed.Flat); !bytes.Equal(flat, evidence[17].bytes) {
			t.Fatal("the crashed object's flat object was removed before its copy verified")
		}
		if sharded, _ := minioServer.get(t, config, crashed.Sharded); !bytes.Equal(sharded, evidence[17].bytes) {
			t.Fatal("the crashed object's copy is missing")
		}
		if err := os.WriteFile(checkpointPath, durable, 0o600); err != nil {
			t.Fatal(err)
		}
		resumed := newTestMinioMover(t, minioServer, config, store, items, checkpointPath)
		report, err := resumed.Run(t.Context())
		if err != nil || !report.Finished || report.Counts.Deduped < 1 {
			t.Fatalf("resumed %+v, %v", report, err)
		}
		for index, entry := range evidence {
			keys := testKeys(t, entry.hash)
			if flat, _ := minioServer.get(t, config, keys.Flat); flat != nil {
				t.Fatalf("%d: the flat object is still stored", index)
			}
			if sharded, _ := minioServer.get(t, config, keys.Sharded); !bytes.Equal(sharded, entry.bytes) {
				t.Fatalf("%d: lost", index)
			}
		}
	})

	// A copy whose source changed since its HEAD is refused by MinIO, and a
	// bucket where a delete would leave a delete marker is refused before
	// anything moves.
	t.Run("copy and preflight refuse", func(t *testing.T) {
		config := minioServer.bucket(t, false)
		store, err := NewMinioObjectStore(config, 2)
		if err != nil {
			t.Fatal(err)
		}
		entry := newTestEvidence(t, 0)
		keys := testKeys(t, entry.hash)
		minioServer.put(t, config, keys.Flat, entry.bytes)
		info, err := store.Stat(t.Context(), keys.Flat)
		if err != nil || info.ETag != md5Hex(entry.bytes) || info.Size != int64(len(entry.bytes)) {
			t.Fatalf("stat %+v, %v", info, err)
		}
		stale := info
		stale.ETag = md5Hex([]byte("other"))
		if err := store.Copy(t.Context(), stale, keys.Sharded); !errors.Is(err, errCopyPrecondition) {
			t.Fatalf("a stale copy gave %v", err)
		}
		if sharded, _ := minioServer.get(t, config, keys.Sharded); sharded != nil {
			t.Fatal("a refused copy wrote the sharded key")
		}
		if _, err := store.Stat(t.Context(), keys.Sharded); !server.IsBlobNotFound(err) {
			t.Fatalf("a missing key's HEAD gave %v", err)
		}
		if err := store.Remove(t.Context(), keys.Sharded); err != nil {
			t.Fatalf("removing a missing key gave %v", err)
		}
		versioned := minioServer.bucket(t, true)
		versionedStore, err := NewMinioObjectStore(versioned, 2)
		if err != nil {
			t.Fatal(err)
		}
		if err := versionedStore.Preflight(t.Context()); err == nil || !strings.Contains(err.Error(), "versioning") {
			t.Fatalf("a versioned bucket's preflight gave %v", err)
		}
		missing := *config
		missing.Bucket = "missing-" + randomTestHex(t, 4)
		missingStore, err := NewMinioObjectStore(&missing, 2)
		if err != nil {
			t.Fatal(err)
		}
		if err := missingStore.Preflight(t.Context()); err == nil {
			t.Fatal("a missing bucket passed its preflight")
		}
		// a refused request (here a bad signature) ends a run
		denied := *config
		denied.SecretKey = "wrong" + randomTestHex(t, 8)
		deniedStore, err := NewMinioObjectStore(&denied, 2)
		if err != nil {
			t.Fatal(err)
		}
		mover := newTestMinioMover(t, minioServer, config, deniedStore, testItems([]testEvidence{entry}), filepath.Join(t.TempDir(), "checkpoint.json"))
		if _, err := mover.Run(t.Context()); err == nil || !isFatal(err) {
			t.Fatalf("a run with refused requests gave %v", err)
		}
		if flat, _ := minioServer.get(t, config, keys.Flat); !bytes.Equal(flat, entry.bytes) {
			t.Fatal("the flat object changed")
		}
	})
}
