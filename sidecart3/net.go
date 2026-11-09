package sidecart3

// The T3 Net (§22.4): sidecartest's Net with NetSettings.Engines set, so its engines run on a
// ServiceHost from sidecarhost.NewServiceHost over server's real stores, and NetSettings.Platform, so its
// connect clients run on server's exchange (exchange.go). A sidecar, which enrolls through an api, is
// refused until server's api carries its enrollment.
//
// What is real: Redis is server's (sidecarhost's adapter on server's pools, which test-env.sh pins to
// the attested private Redis, a database of the test's own); Blobs are server.FileClusters on the local
// stack's MinIO, one metadata and one segment cluster in buckets of the Net's own, which its close empties
// and removes; Tasks write the test's Postgres database; the Authenticator verifies server's service
// tokens; Metrics declare on prometheus's registry. Commits reach the Net's plans through server.Tx's
// hook while the Net is open. Each store is wrapped so the Net's plans reach it (faults.go).
//
// What stays the Net's fake: Gates, which are sidecar.yml's text that a row sets as it does on T2; Push,
// Quota, ServiceClient, ClockCheck, Budget and Abuse, whose real ones come from their own plans' packages
// (QUOTA-M2-11's quota host, chrony's clock check, evsservice's budget client on the host, ABUSE's gate),
// and which a row's T3 test hands its engine where it needs them.
//
// Time: engines and FileClusters read the Net's fake clock, so a deadline an engine sets and the store
// client's cancel at it agree with the schedule; MinIO and Redis keep their own real time, so what they
// age, LastModified and ttls, does not move with an Advance. Translating that is TEST-M2-07's
// sidecart3/clock.go (plan-TEST-clock-seam #2), and a clause that needs it does not pass here yet.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// Why a T3 Net refuses the fake contract issuer and ControlId sink, which on T3 only a sidecar reaches: a
// sidecar enrolls through an api and opens its clients with what that api mints, and sidecart3 hosts
// server's exchange but not yet an api a sidecar's enrollment runs through.
const refusePlatformFakes = "a T3 sidecar enrolls through server's api, which sidecart3 does not host yet, so its clients would run on the fake issuer and ControlId sink: a row with sidecars proves nothing on T3 until it does (TEST-M1-08)"

// What a T3 Net is built from.
type Settings struct {
	// The Net T2 would build: its clock, barriers and fakes.
	Net *sidecartest.NetSettings
	// The local stack's MinIO (sidecar/local), loopback above port 50000: its address and the synthetic
	// root user its compose file starts it with, which reaches nothing but that stack.
	MinioEndpoint  string
	MinioAccessKey string
	MinioSecretKey string
	MinioRegion    string
	// The file clusters' tunables. Their clock is the Net's.
	FileClusters *server.FileClusterSettings
	// The Redis adapter's tunables.
	Redis *sidecarhost.RedisSettings
	// Where each Net's engines record their scripts and multi-key calls, for RunCrossslot.
	CrossslotCalls *CrossslotCalls
	// Server's exchange, which each Net's connect clients run on.
	Exchange *ExchangeSettings
}

func DefaultSettings() *Settings {
	return &Settings{
		Net:            sidecartest.DefaultNetSettings(),
		MinioEndpoint:  "127.0.0.1:59000",
		MinioAccessKey: "sidecar-local-test",
		MinioSecretKey: "sidecar-local-test-only",
		MinioRegion:    "us-east-1",
		FileClusters:   server.DefaultFileClusterSettings(),
		Redis:          sidecarhost.DefaultRedisSettings(),
		CrossslotCalls: NewCrossslotCalls(),
		Exchange:       DefaultExchangeSettings(),
	}
}

// The NetSettings T3's Nets build from, which fault's rules take to run a T3 row: settings.Net with
// Engines hosting on server's stores, Platform running the clients on server's exchange, and the platform
// fakes refused. Each Net arms server.Tx's pg-commit hook on t while it is open, so Nets are built one at
// a time, as sidecartest's hooks already require. A Net that opens a client needs server's schema, so a
// row with clients runs in a TestEnv that applies the migrations. settings nil takes the defaults.
func NetSettings(t testing.TB, settings *Settings) *sidecartest.NetSettings {
	if settings == nil {
		settings = DefaultSettings()
	}
	netSettings := *settings.Net
	netSettings.RefusePlatformFakes = refusePlatformFakes
	netSettings.Engines = func(ctx context.Context, net *sidecartest.Net) (*sidecar.ServiceHost, func(), error) {
		return newHost(ctx, t, net, settings)
	}
	netSettings.Platform = func(ctx context.Context, net *sidecartest.Net) (sidecartest.NetPlatform, error) {
		return newExchangePlatform(ctx, t, settings.Exchange)
	}
	return &netSettings
}

// A Net on T3, which the caller closes. Building one needs the private test stores up: run it inside
// server.TestEnv, with the local stack's MinIO answering.
func NewNet(t testing.TB, settings *Settings) *sidecartest.Net {
	return sidecartest.NewNet(t.Context(), NetSettings(t, settings))
}

// The host of one T3 Net, and what closes it.
func newHost(ctx context.Context, t testing.TB, net *sidecartest.Net, settings *Settings) (*sidecar.ServiceHost, func(), error) {
	buckets, err := newBuckets(ctx, settings)
	if err != nil {
		return nil, nil, err
	}
	clusterSettings := *settings.FileClusters
	clusterSettings.Clock = net.Clock()
	clusters, err := server.NewFileClusters(ctx, buckets.configs(), &clusterSettings)
	if err != nil {
		buckets.remove()
		return nil, nil, err
	}
	clusters.Check(ctx)
	for _, clusterId := range buckets.clusterIds() {
		cluster, err := clusters.Cluster(clusterId)
		if err != nil {
			clusters.Close()
			buckets.remove()
			return nil, nil, err
		}
		if cluster.(*server.FileCluster).LastAdminInfo() == nil {
			clusters.Close()
			buckets.remove()
			return nil, nil, fmt.Errorf("the local stack's MinIO gave no admin info for %s (sidecar/local/stack.sh up)", clusterId)
		}
	}
	fakes := net.Fakes()
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Postgres:      true,
		Blobs:         newFaultBlobs(clusters, net),
		Push:          fakes.Push,
		Quota:         fakes.Quota,
		ServiceClient: fakes.ServiceClient,
		ClockCheck:    fakes.ClockCheck,
		Budget:        fakes.Budget,
		Abuse:         fakes.Abuse,
		Clock:         net.Clock(),
		Redis:         settings.Redis,
	})
	host.Redis = newFaultRedis(host.Redis, net, settings.CrossslotCalls)
	host.Gates = fakes.Gates
	net.Faults().SetCheck(checkPlan)
	stopPgCommits := armPgCommitFaults(t, net)
	closeHost := func() {
		stopPgCommits()
		clusters.Close()
		buckets.remove()
	}
	return host, closeHost, nil
}

// The buckets of one Net: a metadata and a segment cluster on the local stack's MinIO, made empty for it
// and removed with it, so a row starts on empty stores as it does on T2's fakes.
type buckets struct {
	settings *Settings
	client   *minio.Client
	// by cluster id
	clusterIdBuckets map[string]string
}

const (
	metadataClusterId = "meta-1"
	segmentClusterId  = "seg-1"
)

func newBuckets(ctx context.Context, settings *Settings) (*buckets, error) {
	if err := localstack.CheckStackAddress(settings.MinioEndpoint); err != nil {
		return nil, err
	}
	client, err := minio.New(settings.MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(settings.MinioAccessKey, settings.MinioSecretKey, ""),
		Region: settings.MinioRegion,
	})
	if err != nil {
		return nil, err
	}
	run := strings.ToLower(server.NewId().String())
	self := &buckets{
		settings: settings,
		client:   client,
		clusterIdBuckets: map[string]string{
			metadataClusterId: "sidecar-t3-" + run + "-meta",
			segmentClusterId:  "sidecar-t3-" + run + "-seg",
		},
	}
	for _, clusterId := range self.clusterIds() {
		bucket := self.clusterIdBuckets[clusterId]
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: settings.MinioRegion}); err != nil {
			self.remove()
			return nil, fmt.Errorf("the local stack's MinIO made no bucket %s (sidecar/local/stack.sh up): %w", bucket, err)
		}
	}
	return self, nil
}

func (self *buckets) clusterIds() []string {
	return []string{metadataClusterId, segmentClusterId}
}

func (self *buckets) configs() []*server.FileClusterConfig {
	config := func(clusterId string, kind string, first bool) *server.FileClusterConfig {
		return &server.FileClusterConfig{
			ClusterId: clusterId,
			Kind:      kind,
			First:     first,
			Endpoints: []string{self.settings.MinioEndpoint},
			AccessKey: self.settings.MinioAccessKey,
			SecretKey: self.settings.MinioSecretKey,
			Bucket:    self.clusterIdBuckets[clusterId],
			Region:    self.settings.MinioRegion,
		}
	}
	return []*server.FileClusterConfig{
		config(metadataClusterId, server.FileClusterKindMetadata, true),
		config(segmentClusterId, server.FileClusterKindSegment, false),
	}
}

// Empties and removes each bucket. A bucket a failure leaves behind is the stack's to drop
// (sidecar/local/stack.sh down), never another Net's to read: every Net's buckets are new.
func (self *buckets) remove() {
	ctx := context.Background()
	for _, bucket := range self.clusterIdBuckets {
		exists, err := self.client.BucketExists(ctx, bucket)
		if err != nil || !exists {
			continue
		}
		objects := self.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true})
		for removal := range self.client.RemoveObjects(ctx, bucket, objects, minio.RemoveObjectsOptions{}) {
			_ = removal
		}
		self.client.RemoveBucket(ctx, bucket)
	}
}
