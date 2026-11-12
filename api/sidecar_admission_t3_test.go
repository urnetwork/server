//go:build sidecar_localstack

package api_test

// §22.7's admission suite on T3 (TEAM-M2-19): each row of sidecartest's suite, its own top-level test, through server's
// real sequencer (sidecart3.ConfigLogApi, model.ConfigLogSequencer) over the local stack's MinIO as api's first
// metadata cluster, one process with 8 drive directories at EC:4 (sidecar's local/docker-compose.yml), in a bucket of
// this run's own. Every entry is read back from MinIO and applied by the suite's reference, a new device and a gate at
// each base, and the writer's follower. The rows' activation ticks and forged appends run on T2 alone, as
// sidecart3.ConfigLogSuiteSettings says, and each row logs what it left. It carries the sidecar_localstack tag and
// fails rather than skips when the stack is down (local/stack.sh up).

import (
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// The local stack's MinIO: loopback above port 50000, with the synthetic root user its compose file starts it with,
// which reaches nothing but this stack.
const (
	admissionStackEndpoint  = "127.0.0.1:59000"
	admissionStackAccessKey = "sidecar-local-test"
	admissionStackSecretKey = "sidecar-local-test-only"
	admissionStackRegion    = "us-east-1"
)

// T3's tier: a first metadata cluster in a bucket of this test's own on the local stack's MinIO, removed after it, and
// server's sequencer.
func admissionT3Tier(t *testing.T) *sidecartest.AdmissionTier {
	t.Helper()
	ctx := t.Context()
	if err := localstack.CheckStackAddress(admissionStackEndpoint); err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(admissionStackEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(admissionStackAccessKey, admissionStackSecretKey, ""),
		Region: admissionStackRegion,
	})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "sidecar-t3-admission-" + strings.ToLower(sidecar.NewId().String())
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: admissionStackRegion}); err != nil {
		t.Fatalf("the local stack's MinIO made no bucket (local/stack.sh up): %v", err)
	}
	t.Cleanup(func() {
		ctx := t.Context()
		for object := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if object.Err == nil {
				client.RemoveObject(ctx, bucket, object.Key, minio.RemoveObjectOptions{})
			}
		}
		client.RemoveBucket(ctx, bucket)
	})
	clusters, err := server.NewFileClusters(ctx, []*server.FileClusterConfig{{
		ClusterId: "meta-1",
		Kind:      server.FileClusterKindMetadata,
		First:     true,
		Endpoints: []string{admissionStackEndpoint},
		AccessKey: admissionStackAccessKey,
		SecretKey: admissionStackSecretKey,
		Bucket:    bucket,
		Region:    admissionStackRegion,
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
	return &sidecartest.AdmissionTier{Cluster: cluster, NewApi: sidecart3.ConfigLogApi(), Settings: sidecart3.ConfigLogSuiteSettings()}
}

func TestAdmissionSignersOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "signers", admissionT3Tier(t))
}

func TestAdmissionOutsideTheAdminSetOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "outside the AdminSet", admissionT3Tier(t))
}

func TestAdmissionRecoveryWindowOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "the recovery window", admissionT3Tier(t))
}

func TestAdmissionTwoFirstDevicesRacingOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "two first devices racing", admissionT3Tier(t))
}

func TestAdmissionOwnerConsentOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "owner consent", admissionT3Tier(t))
}

func TestAdmissionCommittersOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "committers", admissionT3Tier(t))
}

func TestAdmissionSeatsOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "seats", admissionT3Tier(t))
}

func TestAdmissionAgentsCostNoCommitOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "agents cost no commit", admissionT3Tier(t))
}

func TestAdmissionReadOnlyPageWorkersOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "read-only page Workers", admissionT3Tier(t))
}

func TestAdmissionCompromiseCascadeOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "the compromise cascade", admissionT3Tier(t))
}

func TestTheCascadeReadsEveryConfirmationOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "the cascade reads every confirmation", admissionT3Tier(t))
}

func TestAdmissionAuthKeysOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "auth keys", admissionT3Tier(t))
}

func TestAdmissionGeneratedLogsOnT3(t *testing.T) {
	sidecartest.RunAdmissionRow(t, "generated logs", admissionT3Tier(t))
}
