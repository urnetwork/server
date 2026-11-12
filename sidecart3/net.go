package sidecart3

// The T3 Net (§22.4): sidecartest's Net with NetSettings.Engines set, so its engines run on a
// ServiceHost from sidecarhost.NewServiceHost over server's real stores, and NetSettings.Platform, so its
// connect clients run on server's exchange (exchange.go). A sidecar, which enrolls through an api, is
// refused until server's api carries its enrollment.
//
// What is real: Redis is server's (sidecarhost's adapter on server's pools, which test-env.sh pins to
// the attested private Redis, a database of the test's own); Blobs are server.FileClusters, one metadata
// and one segment cluster in buckets of the Net's own, which its close empties and removes, on the local
// stack's MinIO, or with Settings.Layout on T3's 4-host file-cluster layouts, where QuorumLoss, DriveMiss,
// HostDown and HostUp run (cluster_faults.go); Tasks write the test's Postgres database; the Authenticator
// verifies server's service tokens; Metrics declare on prometheus's registry. Commits reach the Net's
// plans through server.Tx's hook while the Net is open. Each store is wrapped so the Net's plans reach it
// (faults.go).
//
// What stays the Net's fake: Gates, which are sidecar.yml's text that a row sets as it does on T2; Push,
// Quota, ServiceClient, ClockCheck, Budget and Abuse, whose real ones come from their own plans' packages
// (QUOTA-M2-11's quota host, chrony's clock check, evsservice's budget client on the host, ABUSE's gate),
// and which a row's T3 test hands its engine where it needs them.
//
// Time: engines and FileClusters read the Net's fake clock, so a deadline an engine sets and the store
// client's cancel at it agree with the schedule; MinIO and Redis keep their own real time. Every request's
// deadline header is carried in the store's time, and with Settings.StoreClock on, LastModified and the
// ttls of the families a row names are translated onto the schedule's clock (clock.go, plan-TEST-clock-seam
// #2), for the cases catalog.yml flags store_clock.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
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
	// T3's 4-host file-cluster layouts, which a Net's buckets go on in place of the local stack's MinIO, and on
	// which QuorumLoss, DriveMiss, HostDown and HostUp run (cluster_faults.go). nil: the local stack.
	Layout *ClusterLayout
	// How those faults run on a layout.
	ClusterFaults *ClusterFaultSettings
	// The store clock (clock.go), which puts LastModified and the ttls of the families it names on the Net's
	// clock: on for a case catalog.yml flags store_clock, which RunRow holds. nil: off, and the stores' times are
	// their own.
	StoreClock *StoreClockSettings
	// The stores' own clock, which their LastModified, ttls and deadlines run on: real time.
	Store sidecar.Clock
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
		ClusterFaults:  DefaultClusterFaultSettings(),
		Store:          sidecar.RealClock{},
	}
}

// Where server's local/file-cluster-t3 is from this package's directory, which go test runs in.
const layoutDirectory = "../local/file-cluster-t3"

// The default settings on T3's file-cluster layouts, as local/file-cluster-t3 starts them (t3.sh up): a Net's
// metadata cluster on meta-1 (4 x 2 at EC:4) and its segment cluster on seg-1 (4 x 4 at EC:8). Fails t when the
// layout's files do not read.
func LayoutSettings(t testing.TB) *Settings {
	t.Helper()
	layout, err := LoadClusterLayout(layoutDirectory, metadataClusterId, segmentClusterId)
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultSettings()
	settings.Layout = layout
	return settings
}

// Whether each NetSettings this package made turns the store clock on, so RunRow can hold a case to its flag.
var netSettingsStoreClocks sync.Map

// The NetSettings T3's Nets build from, which fault's rules take to run a T3 row: settings.Net with
// Engines hosting on server's stores, Platform running the clients on server's exchange, the platform
// fakes refused, and a row's authorization log on server's sequencer. Each Net arms server.Tx's pg-commit hook on t while it is open, so Nets are built one at
// a time, as sidecartest's hooks already require. A Net that opens a client needs server's schema, so a
// row with clients runs in a TestEnv that applies the migrations. settings nil takes the defaults.
func NetSettings(t testing.TB, settings *Settings) *sidecartest.NetSettings {
	if settings == nil {
		settings = DefaultSettings()
	}
	netSettings := *settings.Net
	netSettings.RefusePlatformFakes = refusePlatformFakes
	// the oracle's read access to server's tables (store_view.go)
	netSettings.Postgres = PostgresReader()
	netSettings.Engines = func(ctx context.Context, net *sidecartest.Net) (*sidecar.ServiceHost, func(), error) {
		return newHost(ctx, t, net, settings)
	}
	netSettings.Platform = func(ctx context.Context, net *sidecartest.Net) (sidecartest.NetPlatform, error) {
		return newExchangePlatform(ctx, t, settings.Exchange)
	}
	// a row's authorization log on server's sequencer over the Net's first metadata cluster (config_log.go)
	netSettings.ConfigLogApi = ConfigLogApi()
	netSettings.ConfigLogSettings = ConfigLogSuiteSettings()
	netSettingsStoreClocks.Store(&netSettings, settings.StoreClock != nil)
	return &netSettings
}

// A Net on T3, which the caller closes. Building one needs the private test stores up: run it inside
// server.TestEnv, with the local stack's MinIO answering.
func NewNet(t testing.TB, settings *Settings) *sidecartest.Net {
	return sidecartest.NewNet(t.Context(), NetSettings(t, settings))
}

// The host of one T3 Net, and what closes it.
func newHost(ctx context.Context, t testing.TB, net *sidecartest.Net, settings *Settings) (*sidecar.ServiceHost, func(), error) {
	var layoutFaults *clusterFaults
	if settings.Layout != nil {
		if err := checkLayoutUp(ctx, settings.Layout, settings.ClusterFaults); err != nil {
			return nil, nil, err
		}
		layoutFaults = newClusterFaults(net, settings.Layout, settings.ClusterFaults)
	}
	buckets, err := newBuckets(ctx, settings)
	if err != nil {
		return nil, nil, err
	}
	if layoutFaults != nil {
		layoutFaults.configs = buckets.configs()
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
			return nil, nil, fmt.Errorf("%s gave no admin info for %s", buckets.where(), clusterId)
		}
	}
	clocks := &netClocks{store: settings.Store}
	var blobs sidecar.BlobStore = clusters
	if settings.StoreClock != nil {
		clocks.storeClock = newStoreClock(net, settings.StoreClock, settings.Store)
		blobs = &storeClockBlobs{blobs: clusters, clock: clocks.storeClock}
	}
	netClockNets.Store(net, clocks)
	if layoutFaults != nil {
		clusterFaultNets.Store(net, layoutFaults)
	}
	fakes := net.Fakes()
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Postgres:      true,
		Blobs:         newFaultBlobs(blobs, clusters, net, layoutFaults),
		Push:          fakes.Push,
		Quota:         fakes.Quota,
		ServiceClient: fakes.ServiceClient,
		ClockCheck:    fakes.ClockCheck,
		Budget:        fakes.Budget,
		Abuse:         fakes.Abuse,
		Clock:         net.Clock(),
		Redis:         settings.Redis,
	})
	host.Redis = newFaultRedis(host.Redis, net, settings.CrossslotCalls, clocks.storeClock)
	host.Gates = fakes.Gates
	net.Faults().SetCheck(checkPlan)
	net.Faults().SetRealStore(admitClusterFault(layoutFaults))
	stopPgCommits := armPgCommitFaults(t, net)
	// an evs world's instances run evshost's block, its devices on oauth's tokens (evs_tier.go)
	evsTier, removeEvsTier := newEvsTier()
	fault.SetEvsTier(net, evsTier)
	closeHost := func() {
		removeEvsTier()
		stopPgCommits()
		// a translation that did not hold, or a fault the store did not carry out, fails the run that relied on it
		for _, problem := range storeClockProblems(net) {
			t.Errorf("%s", problem)
		}
		if layoutFaults != nil {
			for _, problem := range layoutFaults.problems() {
				t.Errorf("%s", problem)
			}
			// no host is left down for whatever runs on the layout next
			if err := layoutFaults.restoreAll(context.Background()); err != nil {
				t.Errorf("a host of T3's layout stayed down: %v", err)
			}
			clusterFaultNets.Delete(net)
		}
		netClockNets.Delete(net)
		clusters.Close()
		buckets.remove()
	}
	return host, closeHost, nil
}

const (
	metadataClusterId = "meta-1"
	segmentClusterId  = "seg-1"
)

// One cluster a Net's bucket goes on: where it is, the user it signs as, and the client that makes and removes the
// bucket.
type bucketCluster struct {
	clusterId string
	kind      string
	first     bool
	endpoints []string
	accessKey string
	secretKey string
	region    string
	bucket    string
	client    *minio.Client
}

// The buckets of one Net: a metadata and a segment cluster on the local stack's MinIO, or on T3's layouts, made
// empty for it and removed with it, so a row starts on empty stores as it does on T2's fakes.
type buckets struct {
	layout   bool
	clusters []*bucketCluster
}

func newBuckets(ctx context.Context, settings *Settings) (*buckets, error) {
	run := strings.ToLower(server.NewId().String())
	self := &buckets{layout: settings.Layout != nil}
	if settings.Layout != nil {
		for _, layoutCluster := range []*LayoutCluster{settings.Layout.Metadata, settings.Layout.Segment} {
			config := layoutCluster.Config
			self.clusters = append(self.clusters, &bucketCluster{
				clusterId: config.ClusterId,
				kind:      config.Kind,
				first:     config.Kind == server.FileClusterKindMetadata,
				endpoints: config.Endpoints,
				accessKey: config.AccessKey,
				secretKey: config.SecretKey,
				region:    config.Region,
				bucket:    "sidecar-t3-" + run + "-" + config.ClusterId,
			})
		}
	} else {
		local := func(clusterId string, kind string, suffix string) *bucketCluster {
			return &bucketCluster{
				clusterId: clusterId,
				kind:      kind,
				first:     kind == server.FileClusterKindMetadata,
				endpoints: []string{settings.MinioEndpoint},
				accessKey: settings.MinioAccessKey,
				secretKey: settings.MinioSecretKey,
				region:    settings.MinioRegion,
				bucket:    "sidecar-t3-" + run + "-" + suffix,
			}
		}
		self.clusters = []*bucketCluster{
			local(metadataClusterId, server.FileClusterKindMetadata, "meta"),
			local(segmentClusterId, server.FileClusterKindSegment, "seg"),
		}
	}
	for _, cluster := range self.clusters {
		for _, endpoint := range cluster.endpoints {
			if err := localstack.CheckStackAddress(endpoint); err != nil {
				return nil, err
			}
		}
		client, err := minio.New(cluster.endpoints[0], &minio.Options{
			Creds:  credentials.NewStaticV4(cluster.accessKey, cluster.secretKey, ""),
			Region: cluster.region,
		})
		if err != nil {
			return nil, err
		}
		cluster.client = client
	}
	for _, cluster := range self.clusters {
		err := cluster.client.MakeBucket(ctx, cluster.bucket, minio.MakeBucketOptions{Region: cluster.region})
		// the name is this Net's alone, so one this user already owns is the create minio-go retried
		if err != nil && minio.ToErrorResponse(err).Code != "BucketAlreadyOwnedByYou" {
			self.remove()
			return nil, fmt.Errorf("%s made no bucket %s: %w", self.where(), cluster.bucket, err)
		}
	}
	return self, nil
}

// What a failure names as the stores a Net's buckets are on.
func (self *buckets) where() string {
	if self.layout {
		return "T3's file clusters (server/local/file-cluster-t3/t3.sh up)"
	}
	return "the local stack's MinIO (sidecar/local/stack.sh up)"
}

func (self *buckets) clusterIds() []string {
	clusterIds := []string{}
	for _, cluster := range self.clusters {
		clusterIds = append(clusterIds, cluster.clusterId)
	}
	return clusterIds
}

func (self *buckets) configs() []*server.FileClusterConfig {
	configs := []*server.FileClusterConfig{}
	for _, cluster := range self.clusters {
		configs = append(configs, &server.FileClusterConfig{
			ClusterId: cluster.clusterId,
			Kind:      cluster.kind,
			First:     cluster.first,
			Endpoints: cluster.endpoints,
			AccessKey: cluster.accessKey,
			SecretKey: cluster.secretKey,
			Bucket:    cluster.bucket,
			Region:    cluster.region,
		})
	}
	return configs
}

// Empties and removes each bucket. A bucket a failure leaves behind is the stack's to drop
// (sidecar/local/stack.sh down, t3.sh down), never another Net's to read: every Net's buckets are new.
func (self *buckets) remove() {
	ctx := context.Background()
	for _, cluster := range self.clusters {
		if cluster.client == nil {
			continue
		}
		exists, err := cluster.client.BucketExists(ctx, cluster.bucket)
		if err != nil || !exists {
			continue
		}
		objects := cluster.client.ListObjects(ctx, cluster.bucket, minio.ListObjectsOptions{Recursive: true})
		for removal := range cluster.client.RemoveObjects(ctx, cluster.bucket, objects, minio.RemoveObjectsOptions{}) {
			_ = removal
		}
		cluster.client.RemoveBucket(ctx, cluster.bucket)
	}
}
