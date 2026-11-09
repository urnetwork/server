//go:build sidecar_localstack

package model

// T3 for api's log sequencer (TEAM-M2-13): sidecartest's shared store suite on the local stack's MinIO,
// one process with 8 drive directories at EC:4, container sidecar-test-minio on 127.0.0.1:59000
// (sidecar's local/docker-compose.yml), through server's FileClusters as api's first metadata cluster.
// It carries the sidecar_localstack tag, works in a bucket of its own, and fails rather than skips when
// the stack is down. §22.7's 4 x 2 run waits on T3's real sets.

import (
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
)

// The local stack's MinIO: loopback above port 50000, with the synthetic root user its compose file starts it
// with, which reaches nothing but this stack.
const (
	configLogStackEndpoint  = "127.0.0.1:59000"
	configLogStackAccessKey = "sidecar-local-test"
	configLogStackSecretKey = "sidecar-local-test-only"
	configLogStackBucket    = "sidecar-test-server-config-log"
)

func TestConfigLogSequencerPassesTheStoreSuiteOnMinio(t *testing.T) {
	ctx := t.Context()
	if err := localstack.CheckStackAddress(configLogStackEndpoint); err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(configLogStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(configLogStackAccessKey, configLogStackSecretKey, ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := client.BucketExists(ctx, configLogStackBucket)
	if err != nil {
		t.Fatalf("the local stack's MinIO does not answer (local/stack.sh up): %v", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, configLogStackBucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
	}
	clusters, err := server.NewFileClusters(ctx, []*server.FileClusterConfig{{
		ClusterId: "meta-1",
		Kind:      server.FileClusterKindMetadata,
		First:     true,
		Endpoints: []string{configLogStackEndpoint},
		AccessKey: configLogStackAccessKey,
		SecretKey: configLogStackSecretKey,
		Bucket:    configLogStackBucket,
		Region:    "us-east-1",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	clusters.Check(ctx)
	cluster, err := clusters.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	sidecartest.RunConfigLogStoreSuite(t, cluster, testConfigLogSuiteFactory(nil))
}
