//go:build sidecar_localstack

// T3 for `sidecarctl evs drop-reindex` (§22.9, D-TEST-2): the binary's registered command with its default settings,
// vault's file-clusters-taskworker.yml read by server.LoadFileClusters, the clusters' first check, place/ and
// Reindex, on server.FileClusters over the local stack's MinIO (sidecar's local/docker-compose.yml: one process, 8
// drive directories at EC:4, container sidecar-test-minio on 127.0.0.1:59000). The vault file is a synthetic one the
// test pushes, naming the stack's synthetic root user and buckets of this run's own, which the test makes unversioned,
// as xops makes production's, and removes with what they hold when it ends. The job's history is built on the fakes,
// as the unit tests build it, and copied object by object into the buckets.
//
// The stack runs a stock MinIO, so its settle gate stays closed until the test's sidecar.yml names the build its hosts
// report, which is how the rewrite path runs here: nothing here shows D-OPS-20's settle rules hold. The command reads
// WARP_ENV, so this runs through .stack/server-test.sh with -tags sidecar_localstack, and it fails rather than skips
// when the stack is down.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
)

// The local stack's MinIO: loopback above port 50000, with the synthetic root user its compose file starts it with,
// which reaches nothing but this stack.
const (
	evsDropReindexStackEndpoint  = "127.0.0.1:59000"
	evsDropReindexStackAccessKey = "sidecar-local-test"
	evsDropReindexStackSecretKey = "sidecar-local-test-only"
)

// The world's three clusters as buckets of this run's own on the stack, and vault's file-clusters-taskworker.yml
// naming them, pushed until the test ends; the buckets go, with what they hold, when it ends. Returns clusters of the
// test's own over the same buckets, checked, for its own reads and writes.
func evsDropReindexStackClusters(t *testing.T) *server.FileClusters {
	t.Helper()
	ctx := t.Context()
	if err := localstack.CheckStackAddress(evsDropReindexStackEndpoint); err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(evsDropReindexStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(evsDropReindexStackAccessKey, evsDropReindexStackSecretKey, ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	configs := []*server.FileClusterConfig{}
	vaultFile := &strings.Builder{}
	vaultFile.WriteString("clusters:\n")
	for _, clusterId := range []string{"meta-1", evsDropReindexTestClusterId, "seg-1"} {
		config := &server.FileClusterConfig{
			ClusterId: clusterId,
			Kind:      server.FileClusterKindMetadata,
			First:     clusterId == "meta-1",
			Endpoints: []string{evsDropReindexStackEndpoint},
			AccessKey: evsDropReindexStackAccessKey,
			SecretKey: evsDropReindexStackSecretKey,
			Bucket:    "sidecar-test-server-sidecarctl-" + hex.EncodeToString(nonce) + "-" + clusterId,
			Region:    "us-east-1",
		}
		if clusterId == "seg-1" {
			config.Kind = server.FileClusterKindSegment
		}
		configs = append(configs, config)
		fmt.Fprintf(vaultFile, "  %s:\n    kind: %s\n    first: %t\n    endpoints: [%s]\n    access_key: %s\n    secret_key: %s\n    bucket: %s\n    tls: false\n    region: %s\n",
			config.ClusterId, config.Kind, config.First, evsDropReindexStackEndpoint, config.AccessKey, config.SecretKey, config.Bucket, config.Region)
		if err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{Region: config.Region}); err != nil {
			t.Fatalf("the local stack's MinIO does not make a bucket (local/stack.sh up): %v", err)
		}
		bucket := config.Bucket
		t.Cleanup(func() {
			// its own context, since the test's has ended by now
			cleanupCtx := context.Background()
			for object := range client.ListObjects(cleanupCtx, bucket, minio.ListObjectsOptions{Recursive: true}) {
				if object.Err == nil {
					client.RemoveObject(cleanupCtx, bucket, object.Key, minio.RemoveObjectOptions{})
				}
			}
			if err := client.RemoveBucket(cleanupCtx, bucket); err != nil {
				t.Errorf("bucket %s left on the stack: %v", bucket, err)
			}
		})
	}
	t.Cleanup(server.Vault.PushSimpleResource("file-clusters-"+evsDropReindexFileClustersService+".yml", []byte(vaultFile.String())))
	clusters, err := server.NewFileClusters(ctx, configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	clusters.Check(ctx)
	return clusters
}

// The binary's own command, default settings and all, on the env WARP_ENV names: a job's missing list file written
// fresh; a corrupt one refused while the segment cluster's build has not passed the settle cases, the file untouched;
// and once sidecar.yml names the build the stack's hosts report, the corrupt file deleted and written again with the
// bytes resolve wrote, a second run rewriting nothing.
func TestEvsDropReindexOnTheLocalStack(t *testing.T) {
	ctx := t.Context()
	clusters := evsDropReindexStackClusters(t)
	world := newEvsDropReindexWorld(t, evsDropReindexTestDropSettings(evsservice.DefaultDropSettings().FileRowCount))
	path := evsDropReindexTestPath("x")
	drop := world.drop(evsDropReindexTestId("drop x"), path, world.write(path, 2))
	world.resolve(drop)
	original := world.listFile(0)
	if len(original) == 0 {
		t.Fatal("the job's resolve wrote no list file")
	}
	// the world's history, every object but the list file, in the stack's buckets: Dump names them, and each one's
	// bytes are read from its fake cluster
	for name := range world.host.Blobs.Dump() {
		if name == world.listFileName(0) {
			continue
		}
		clusterId, key, _ := strings.Cut(name, "/")
		cluster, err := clusters.Cluster(clusterId)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cluster.PutIfNoneMatch(ctx, key, world.object(clusterId, key), nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	segment, err := clusters.SegmentCluster()
	if err != nil {
		t.Fatal(err)
	}
	listKey := evsservice.DropFileKey(world.repoId, world.jobId, evsservice.DropFileList, 0)
	readList := func() []byte {
		t.Helper()
		object, err := segment.Get(ctx, listKey)
		if err != nil {
			t.Fatal(err)
		}
		defer object.Body.Close()
		listBytes, err := io.ReadAll(object.Body)
		if err != nil {
			t.Fatal(err)
		}
		return listBytes
	}
	run := func() (int, string, string) {
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		code := sidecarctlCommands.run([]string{"evs", "drop-reindex", sidecar.EvsKeyId(world.repoId), sidecar.EvsKeyId(world.jobId), evsDropReindexTuple(drop)}, stdout, stderr)
		return code, stdout.String(), stderr.String()
	}

	// a sidecar.yml naming no cluster's build, so no gate is open whatever the env's own file says
	popUnsettled := server.Testing_PushSidecarConfig("sidecar: {}\n")
	// popped once: a second pop would take whatever override reused its slot
	unsettledPopped := false
	t.Cleanup(func() {
		if !unsettledPopped {
			popUnsettled()
		}
	})

	// gone, as a run stopped between its DELETE and its write leaves it: written fresh, which no gate stands before
	code, stdout, stderr := run()
	if code != 0 || !strings.Contains(stdout, "\nrewritten none\nwritten 0\nstray none\n") {
		t.Fatalf("a missing file: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(readList(), original) {
		t.Fatal("the list file was written with bytes other than resolve's")
	}

	// corrupt, on a stock build no sidecar.yml names: refused, the file as it was
	corrupt := bytes.Clone(original)
	corrupt[len(corrupt)-1] ^= 1
	if err := segment.Delete(ctx, listKey); err != nil {
		t.Fatal(err)
	}
	if _, err := segment.PutIfNoneMatch(ctx, listKey, corrupt, nil); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = run()
	if code != 1 || !strings.Contains(stderr, "has not passed the settle cases") || stdout != "" {
		t.Fatalf("an unsettled cluster: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(readList(), corrupt) {
		t.Fatal("a refused run changed the list file")
	}

	// corrupt, with sidecar.yml naming the build every host of the segment cluster reports: rewritten
	info, err := clusters.ReadAdminInfo(ctx, segment.ClusterId())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Hosts) == 0 || info.Hosts[0].CommitId == "" {
		t.Fatalf("the stack's MinIO reports no build to name: %+v", info.Hosts)
	}
	build := info.Hosts[0].CommitId
	for _, host := range info.Hosts {
		if host.CommitId != build {
			t.Fatalf("the stack's hosts report builds %q and %q", build, host.CommitId)
		}
	}
	popUnsettled()
	unsettledPopped = true
	t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  file_clusters:\n    %s: {settle_passed_build: %q}\n", segment.ClusterId(), build)))
	code, stdout, stderr = run()
	if code != 0 || !strings.Contains(stdout, "\nrewritten 0\nwritten none\nstray none\n") {
		t.Fatalf("a settled cluster: exited %d, printing %q and %q", code, stdout, stderr)
	}
	if !bytes.Equal(readList(), original) {
		t.Fatal("the list file was written again with bytes other than resolve's")
	}
	code, stdout, stderr = run()
	if code != 0 || !strings.Contains(stdout, "\nrewritten none\nwritten none\n") {
		t.Fatalf("a second run: exited %d, printing %q and %q", code, stdout, stderr)
	}
}
