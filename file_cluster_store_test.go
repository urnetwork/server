//go:build sidecar_localstack

// T3 for FileClusters (§22.9, D-TEST-2): blobsuite on the local stack's MinIO, one process with 8
// drive directories at EC:4, container sidecar-test-minio on 127.0.0.1:59000 (sidecar's
// local/docker-compose.yml). The stack is shared by tracks, so this works in buckets of its own. It
// carries the sidecar_localstack tag, and fails rather than skips when the stack is down.
//
// The stack runs a stock MinIO until the fork has a build, so the settle cases wait (blobsuite logs
// them by name) and SettlePassed stays false: nothing here shows D-OPS-20's rules hold.

package server

import (
	"context"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar/sidecartest/blobsuite"
	"github.com/urnetwork/sidecar/sidecartest/localstack"
)

// The local stack's MinIO: loopback above port 50000, with the synthetic root user its compose file
// starts it with, which reaches nothing but this stack.
const (
	fileClusterStackEndpoint  = "127.0.0.1:59000"
	fileClusterStackAccessKey = "sidecar-local-test"
	fileClusterStackSecretKey = "sidecar-local-test-only"
)

func fileClusterStackConfigs() []*FileClusterConfig {
	return []*FileClusterConfig{
		{
			ClusterId: "meta-1",
			Kind:      FileClusterKindMetadata,
			First:     true,
			Endpoints: []string{fileClusterStackEndpoint},
			AccessKey: fileClusterStackAccessKey,
			SecretKey: fileClusterStackSecretKey,
			Bucket:    "sidecar-test-server-meta-1",
			Region:    "us-east-1",
		},
		{
			ClusterId: "seg-1",
			Kind:      FileClusterKindSegment,
			Endpoints: []string{fileClusterStackEndpoint},
			AccessKey: fileClusterStackAccessKey,
			SecretKey: fileClusterStackSecretKey,
			Bucket:    "sidecar-test-server-seg-1",
			Region:    "us-east-1",
		},
	}
}

// Makes each bucket, unversioned, as xops makes production's: FileClusters makes none.
func makeFileClusterStackBuckets(ctx context.Context, t *testing.T, configs []*FileClusterConfig) {
	t.Helper()
	if err := localstack.CheckStackAddress(fileClusterStackEndpoint); err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(fileClusterStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(fileClusterStackAccessKey, fileClusterStackSecretKey, ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range configs {
		exists, err := client.BucketExists(ctx, config.Bucket)
		if err != nil {
			t.Fatalf("the local stack's MinIO does not answer (local/stack.sh up): %v", err)
		}
		if !exists {
			if err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{Region: config.Region}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestFileClustersConformOnTheLocalStack(t *testing.T) {
	ctx := t.Context()
	configs := fileClusterStackConfigs()
	makeFileClusterStackBuckets(ctx, t, configs)
	clusters, err := NewFileClusters(ctx, configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	clusters.Check(ctx)

	blobsuite.RunStore(t, clusters)
	for _, clusterId := range []string{"meta-1", "seg-1"} {
		cluster := clusters.clusterIdClusters[clusterId]
		info := cluster.LastAdminInfo()
		if info == nil || info.PoolCount != 1 {
			t.Fatalf("%s: admin info %+v", clusterId, info)
		}
		if cluster.SettlePassed() {
			t.Errorf("%s: a stock build passed the settle gate", clusterId)
		}
		blobsuite.Run(t, &blobsuite.Target{
			Cluster: cluster,
			Now:     time.Now,
			// a real store rounds LastModified to the second
			ClockTolerance: 2 * time.Second,
		})
	}
}

// §19.4's readiness on a real store: the sentinel xops writes, read with this block's credentials.
func TestFileClustersReadyOnceTheSentinelIsThere(t *testing.T) {
	ctx := t.Context()
	configs := fileClusterStackConfigs()
	makeFileClusterStackBuckets(ctx, t, configs)
	clusters, err := NewFileClusters(ctx, configs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	for _, config := range configs {
		cluster := clusters.clusterIdClusters[config.ClusterId]
		cluster.Delete(ctx, fileClusterSentinelKey)
	}
	clusters.Check(ctx)
	if clusters.Ready() {
		t.Fatal("ready with no sentinel")
	}
	for _, config := range configs {
		cluster := clusters.clusterIdClusters[config.ClusterId]
		if _, err := cluster.PutIfNoneMatch(ctx, fileClusterSentinelKey, []byte("sentinel"), nil); err != nil {
			t.Fatal(err)
		}
	}
	clusters.Check(ctx)
	if !clusters.Ready() {
		t.Error("not ready with the sentinel there")
	}
}
