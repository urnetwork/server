//go:build sidecar_filecluster_t3

// FS-41's storage losses on T3 (EVSB-M2-20; §19.10's site-loss runbook, D-OPS-14), on the clusters
// local/file-cluster-t3 starts and through local/file-cluster-t3/wipe.sh: drives lost past parity first, when a head
// still reads and the segment it names does not, so a reader alarms and takes nothing in its place; then the site
// lost, every bucket new and empty and passing §22.9's suite before any re-seed; then the writes a re-seed makes,
// which the new buckets take as a first write and a second run of the same re-seed finds standing.
//
// This is the store's half. What evs's /reseed admits and refuses, and what a mirror returns through it, is
// sidecar's (evsservice/reseed_test.go, sidecartest/evs_reseed_world_test.go), on its fakes; the authority's and
// Postgres' steps of FS-41 are their owners'. The test needs T3 up (t3.sh build, then up), fails rather than skips
// when it is down, and leaves the clusters up and empty. It runs nightly and at Gn-out, never in the whole-suite
// scripts: it removes every drive T3 has and waits on MinIO's start in real time.

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

// T3's clusters as one store, under the kinds clusters.yml gives them, checked once and closed at the test's end.
func fileClusterReseedStore(t *testing.T, targets map[string]*FileClusterSuiteTarget) *FileClusters {
	t.Helper()
	configs := []*FileClusterConfig{}
	for _, clusterId := range []string{"meta-1", "meta-2", "seg-1"} {
		target := targets[clusterId]
		if target == nil {
			t.Fatalf("T3 names no cluster %s", clusterId)
		}
		configs = append(configs, target.Config)
	}
	settings := DefaultFileClusterSettings()
	clusters, err := newFileClusters(t.Context(), configs, settings, newFileClusterBaseTransport(settings), CurrentSidecarConfig)
	if err != nil {
		t.Fatalf("%v (is T3 up? local/file-cluster-t3/t3.sh up)", err)
	}
	t.Cleanup(clusters.Close)
	// a write waits on a cluster's admin info, which the store's own pass reads in its time: read it now
	clusters.Check(t.Context())
	return clusters
}

func fileClusterReseedCluster(t *testing.T, clusters *FileClusters, clusterId string) sidecar.BlobCluster {
	t.Helper()
	cluster, err := clusters.Cluster(clusterId)
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

// Runs wipe.sh with the go that runs this test.
func fileClusterReseedWipe(t *testing.T, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "bash", append([]string{filepath.Join("local", "file-cluster-t3", "wipe.sh")}, args...)...)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("wipe.sh %v: %v\n%s", args, err, output)
	}
}

// key's bytes, or the store's error.
func fileClusterReseedRead(ctx context.Context, cluster sidecar.BlobCluster, key string) ([]byte, error) {
	object, err := cluster.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer object.Body.Close()
	return io.ReadAll(object.Body)
}

func TestFileClusterReseedStorageLossesOnT3(t *testing.T) {
	ctx := t.Context()
	targets := fileClusterSuiteT3Targets(t)
	repoId, refId := sidecar.NewId(), sidecar.NewId()
	segment := bytes.Repeat([]byte("a segment's sealed bytes "), 4096)
	segmentId := sha256.Sum256(segment)
	headKey, segmentKey, reseedKey := evsservice.HeadKey(repoId, refId), evsservice.SegmentKey(repoId, segmentId), evsservice.ReseedKey(repoId, refId)
	// the store reads no object's bytes, so the head here is the id of the segment it names
	head := segmentId[:]

	// a head on the metadata cluster and the segment it names on the segment cluster
	clusters := fileClusterReseedStore(t, targets)
	metadata, segments := fileClusterReseedCluster(t, clusters, "meta-1"), fileClusterReseedCluster(t, clusters, "seg-1")
	if _, err := segments.PutStream(ctx, segmentKey, bytes.NewReader(segment), int64(len(segment)), segmentId[:]); err != nil {
		t.Fatalf("the segment: %v (is T3 up? local/file-cluster-t3/t3.sh up)", err)
	}
	if _, err := metadata.PutIfNoneMatch(ctx, headKey, head, nil); err != nil {
		t.Fatalf("the head: %v", err)
	}
	if body, err := fileClusterReseedRead(ctx, segments, segmentKey); err != nil || !bytes.Equal(body, segment) {
		t.Fatalf("the segment before any loss: %d bytes, %v", len(body), err)
	}

	// drives lost past parity: 3 of the segment cluster's 4 hosts, 12 of its 16 drives at EC:8. The head still
	// reads; the segment it names answers an error or nothing, never bytes, which is what a reader alarms on
	fileClusterReseedWipe(t, "drives", "seg-1", "1", "2", "3")
	if body, err := fileClusterReseedRead(ctx, metadata, headKey); err != nil || !bytes.Equal(body, head) {
		t.Fatalf("the head with the segment cluster's drives lost: %x, %v", body, err)
	}
	body, err := fileClusterReseedRead(ctx, segments, segmentKey)
	if err == nil {
		t.Fatalf("a segment read from 4 of 16 drives at EC:8: %d bytes", len(body))
	}
	t.Logf("the segment with its drives lost past parity: %v", err)
	if _, err := segments.Head(ctx, segmentKey); err == nil {
		t.Fatal("a segment described from 4 of 16 drives at EC:8")
	}

	// the site lost: every cluster on new empty drives, its bucket new, holding nothing of the repo
	fileClusterReseedWipe(t, "site")
	clusters = fileClusterReseedStore(t, targets)
	metadata, segments = fileClusterReseedCluster(t, clusters, "meta-1"), fileClusterReseedCluster(t, clusters, "seg-1")
	for _, clusterId := range []string{"meta-1", "meta-2", "seg-1"} {
		objects, more, err := fileClusterReseedCluster(t, clusters, clusterId).List(ctx, "", "", 16)
		if err != nil || more || len(objects) != 0 {
			t.Fatalf("%s after the site's loss: %d objects, more %v, %v", clusterId, len(objects), more, err)
		}
	}
	if _, err := fileClusterReseedRead(ctx, metadata, headKey); !errors.Is(err, sidecar.ErrBlobNotFound) {
		t.Fatalf("the head after the site's loss: %v", err)
	}
	if _, err := fileClusterReseedRead(ctx, segments, segmentKey); !errors.Is(err, sidecar.ErrBlobNotFound) {
		t.Fatalf("the segment after the site's loss: %v", err)
	}

	// the new buckets pass the suite before any re-seed (§19.10's hardware step)
	for _, clusterId := range []string{"meta-1", "meta-2", "seg-1"} {
		report, err := RunFileClusterSuite(ctx, targets[clusterId])
		if err != nil {
			t.Fatalf("%s: %v", clusterId, err)
		}
		if _, ok := report.SettlePassedBuild(); !ok {
			for _, c := range report.Cases {
				if c.Outcome != FileClusterSuitePass {
					t.Errorf("%s on its new bucket: %s %s: %s", clusterId, c.Name, c.Outcome, c.Detail)
				}
			}
			t.Fatalf("%s does not pass the suite on its new bucket", clusterId)
		}
		// the suite leaves nothing of its own behind for a re-seed to meet
		objects, more, err := fileClusterReseedCluster(t, clusters, clusterId).List(ctx, "", "", 16)
		if err != nil || more || len(objects) != 0 {
			t.Fatalf("%s after its suite: %d objects, more %v, %v", clusterId, len(objects), more, err)
		}
	}

	// the re-seed's writes: the segment by its id, then the head and the admin's EvsReseed as first writes. A second
	// run of the same re-seed finds each standing, and a later EvsReseed replaces the first at its etag
	if _, err := segments.PutStream(ctx, segmentKey, bytes.NewReader(segment), int64(len(segment)), segmentId[:]); err != nil {
		t.Fatalf("the segment's return: %v", err)
	}
	if _, err := metadata.PutIfNoneMatch(ctx, headKey, head, nil); err != nil {
		t.Fatalf("the head's return: %v", err)
	}
	statementEtag, err := metadata.PutIfNoneMatch(ctx, reseedKey, []byte("an EvsReseed's signed bytes"), nil)
	if err != nil {
		t.Fatalf("the EvsReseed: %v", err)
	}
	if _, err := metadata.PutIfNoneMatch(ctx, headKey, head, nil); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		t.Fatalf("the head's second return: %v", err)
	}
	if _, err := metadata.PutIfNoneMatch(ctx, reseedKey, []byte("an EvsReseed's signed bytes"), nil); !errors.Is(err, sidecar.ErrBlobPrecondition) {
		t.Fatalf("the EvsReseed's second filing: %v", err)
	}
	if _, err := metadata.PutIfMatch(ctx, reseedKey, statementEtag, []byte("a later EvsReseed's signed bytes"), nil); err != nil {
		t.Fatalf("a later EvsReseed: %v", err)
	}
	if body, err := fileClusterReseedRead(ctx, segments, segmentKey); err != nil || !bytes.Equal(body, segment) {
		t.Fatalf("the returned segment: %d bytes, %v", len(body), err)
	}
	if body, err := fileClusterReseedRead(ctx, metadata, headKey); err != nil || !bytes.Equal(body, head) {
		t.Fatalf("the returned head: %x, %v", body, err)
	}
	if body, err := fileClusterReseedRead(ctx, metadata, reseedKey); err != nil || string(body) != "a later EvsReseed's signed bytes" {
		t.Fatalf("the EvsReseed read back: %q, %v", body, err)
	}

	// the clusters are left up and empty, as a run after this one expects them
	for _, key := range []string{headKey, reseedKey} {
		if err := metadata.Delete(ctx, key); err != nil {
			t.Errorf("removing %s: %v", key, err)
		}
	}
	if err := segments.Delete(ctx, segmentKey); err != nil {
		t.Errorf("removing %s: %v", segmentKey, err)
	}
}
