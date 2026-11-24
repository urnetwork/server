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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

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
// sidecar enrolls through an api and opens its clients with what that api mints, and a Net's sidecars are
// sidecartest's, enrolled through T2's fake api. A T3 test enrolls its sidecars through server's api in
// process instead: T3Api, and as many as it needs in each of a T3World's networks (world_networks.go).
const refusePlatformFakes = "a T3 Net's sidecars enroll through sidecartest's fake api, so their clients would run on the fake issuer and ControlId sink, which proves nothing on T3: enroll them through server's api, a T3World's networks (TEST-M1-08)"

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
	// The AclExchangeTier each Net names (acl_exchange_tier.go): FS-21's sidecars on an exchange of its own under
	// server's contract gate. nil: its defaults.
	AclExchangeTier *AclExchangeTierSettings
	// The evs tier each Net names (evs_tier.go). nil: its defaults.
	EvsTier *EvsTierSettings
	// How many times more each of the Net's setup calls on its stores is sent after a transport failure, a request
	// that got no answer (a connection reset, refused or closed, or no answer in time): the MakeBucket of each bucket,
	// and the clusters' first admin read. Under load the local stack's MinIO, behind Docker's port forward, resets new
	// connections, which says nothing of the row a Net is built for (QD-1395). A refusal, such as AccessDenied, or any
	// other answer is never sent again, and nor is a call once ctx has ended. 5. A MakeBucket also rides minio-go's own
	// retries of each request, ten with about 4 s of backoff in all, so a stack that refuses every dial, truly down,
	// reports about 25 s later than with none, under the same message, and an admin read about five SetupRetryWaits
	// later. Non-positive: no call is sent again.
	SetupRetryCount int
	// How long the setup waits, in real time, which the stores keep, before sending a call again: 1 s, about as long
	// as a burst of resets lasted on a loaded machine. Non-positive: at once.
	SetupRetryWait time.Duration
	// How far the Net's clock moves past the last reread of the clusters' admin info before a request reads it again
	// (faults.go's faultBlobs): the file clusters' CheckInterval, 30 s, how often their own checks read it, well inside
	// CheckStaleAfter. So a clock that follows real time in small steps rereads about once in 30 s, not before every
	// request after a step (QD-1395), while a schedule's jump past it still rereads before its next request. It must
	// stay below FileClusters.CheckStaleAfter, which a Net refuses to be built past, or a request after a move just
	// short of it would find admin info stale. Non-positive: every move rereads.
	AdminInfoRereadAfter time.Duration

	// What this package's tests see of each Net's catch-ups with its clock: the admin info reread (faults.go) and
	// the store clock's aging (clock.go). nil: nothing.
	catchUpHooks *catchUpHooks
}

func DefaultSettings() *Settings {
	fileClusters := server.DefaultFileClusterSettings()
	return &Settings{
		Net:                  sidecartest.DefaultNetSettings(),
		MinioEndpoint:        "127.0.0.1:59000",
		MinioAccessKey:       "sidecar-local-test",
		MinioSecretKey:       "sidecar-local-test-only",
		MinioRegion:          "us-east-1",
		FileClusters:         fileClusters,
		Redis:                sidecarhost.DefaultRedisSettings(),
		CrossslotCalls:       NewCrossslotCalls(),
		Exchange:             DefaultExchangeSettings(),
		ClusterFaults:        DefaultClusterFaultSettings(),
		Store:                sidecar.RealClock{},
		AclExchangeTier:      DefaultAclExchangeTierSettings(),
		SetupRetryCount:      5,
		SetupRetryWait:       time.Second,
		AdminInfoRereadAfter: fileClusters.CheckInterval,
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

// Whether each NetSettings this package made turns the store clock on, so cases and clauses use their catalog flag.
var netSettingsStoreClocks sync.Map

// Each open T3 Net's Redis wrapper, so a scenario finds the keys the sidecar wrote.
var netFaultRedis sync.Map

// The NetSettings T3's Nets build from, which fault's rules take to run a T3 row: settings.Net with
// Engines hosting on server's stores, Platform running the clients on server's exchange, the platform
// fakes refused, and a row's authorization log on server's sequencer. Each Net arms server.Tx's pg-commit hook on t while it is open, so Nets are built one at
// a time, as sidecartest's hooks already require. A Net that opens a client needs server's schema, so a
// row with clients runs in a TestEnv that applies the migrations. What its Close leaves running fails t
// (net_close_failed.go), unless settings.Net takes it itself. settings nil takes the defaults.
func NetSettings(t testing.TB, settings *Settings) *sidecartest.NetSettings {
	if settings == nil {
		settings = DefaultSettings()
	}
	netSettings := *settings.Net
	netSettings.RefusePlatformFakes = refusePlatformFakes
	if netSettings.CloseFailed == nil {
		netSettings.CloseFailed = t3NetCloseFailed(t, netSettings.CloseDumpByteCount)
	}
	// the oracle's read access to server's tables (store_view.go)
	netSettings.Postgres = PostgresReader()
	clockWitness := sync.OnceFunc(func() { t.Logf("%s %s", fault.GateStoreClockWitness, t.Name()) })
	netSettings.Engines = func(ctx context.Context, net *sidecartest.Net) (*sidecar.ServiceHost, func(), error) {
		return newHost(ctx, t, net, settings, clockWitness)
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
func newHost(ctx context.Context, t testing.TB, net *sidecartest.Net, settings *Settings, clockWitness func()) (*sidecar.ServiceHost, func(), error) {
	if 0 < settings.AdminInfoRereadAfter && settings.FileClusters.CheckStaleAfter <= settings.AdminInfoRereadAfter {
		return nil, nil, fmt.Errorf("AdminInfoRereadAfter %s is not below the file clusters' CheckStaleAfter %s, so a request could find admin info stale", settings.AdminInfoRereadAfter, settings.FileClusters.CheckStaleAfter)
	}
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
	if err := firstAdminRead(ctx, clusters, buckets, settings); err != nil {
		clusters.Close()
		buckets.remove()
		return nil, nil, err
	}
	clocks := &netClocks{store: settings.Store}
	var blobs sidecar.BlobStore = clusters
	if settings.StoreClock != nil {
		clocks.storeClock = newStoreClock(net, settings.StoreClock, settings.Store, settings.catchUpHooks)
		clocks.storeClock.witness = clockWitness
		blobs = &storeClockBlobs{blobs: clusters, clock: clocks.storeClock}
	}
	netClockNets.Store(net, clocks)
	if layoutFaults != nil {
		clusterFaultNets.Store(net, layoutFaults)
	}
	fakes := net.Fakes()
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Postgres:      true,
		Blobs:         newFaultBlobs(blobs, clusters.Check, net, layoutFaults, settings.AdminInfoRereadAfter, settings.catchUpHooks),
		Push:          fakes.Push,
		Quota:         fakes.Quota,
		ServiceClient: fakes.ServiceClient,
		ClockCheck:    fakes.ClockCheck,
		Budget:        fakes.Budget,
		Abuse:         fakes.Abuse,
		Clock:         net.Clock(),
		Redis:         settings.Redis,
	})
	redisWrapper := newFaultRedis(host.Redis, net, settings.CrossslotCalls, clocks.storeClock)
	host.Redis = redisWrapper
	netFaultRedis.Store(net, redisWrapper)
	host.Gates = fakes.Gates
	net.Faults().SetCheck(checkPlan)
	net.Faults().SetRealStore(admitClusterFault(layoutFaults))
	stopPgCommits := armPgCommitFaults(t, net)
	// an evs world's instances run evshost's block, its devices on oauth's tokens (evs_tier.go)
	evsTier, removeEvsTier := newEvsTier(settings.EvsTier)
	fault.SetEvsTier(net, evsTier)
	// FS-21's sidecars on server's exchange under its contract gate (acl_exchange_tier.go)
	aclExchangeTier, closeAclExchangeTier := newAclExchangeTier(ctx, t, settings.AclExchangeTier)
	fault.SetAclExchangeTier(net, aclExchangeTier)
	closeHost := func() {
		closeAclExchangeTier()
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
		netFaultRedis.Delete(net)
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
		err := retrySetup(ctx, settings, func() error {
			err := cluster.client.MakeBucket(ctx, cluster.bucket, minio.MakeBucketOptions{Region: cluster.region})
			// the name is this Net's alone, so one this user already owns is a create sent again, by minio-go or by
			// retrySetup, after a reset lost the answer to one the store carried out
			if err != nil && minio.ToErrorResponse(err).Code != "BucketAlreadyOwnedByYou" {
				return err
			}
			return nil
		})
		if err != nil {
			self.remove()
			return nil, fmt.Errorf("%s made no bucket %s: %w", self.where(), cluster.bucket, err)
		}
	}
	return self, nil
}

// What the clusters' first admin read returns when Check kept no admin info for a cluster that answered the read
// after it: Check's read met a transport failure, whose error it logs and keeps nothing of, so retrySetup sends the
// read again as it would after a reset.
var errAdminInfoUnkept = errors.New("the check kept no admin info, and the store answered the read after it")

// The clusters' first admin read, which leaves every cluster of the Net with admin info kept before any request, as
// a cluster refuses a write without it: clusters.Check, sent again as retrySetup sends a setup call while a cluster
// has none after a transport failure. Check logs a failed read and keeps nothing of it, so a cluster it left
// without admin info is read once more through ReadAdminInfo, whose error tells a reset from a refusal: a refusal,
// or any other answer that is not admin info, fails the Net at once. That read answering means Check's met a
// transport failure (errAdminInfoUnkept), and Check is sent again.
func firstAdminRead(ctx context.Context, clusters *server.FileClusters, buckets *buckets, settings *Settings) error {
	return retrySetup(ctx, settings, func() error {
		clusters.Check(ctx)
		for _, clusterId := range buckets.clusterIds() {
			cluster, err := clusters.Cluster(clusterId)
			if err != nil {
				return err
			}
			if cluster.(*server.FileCluster).LastAdminInfo() != nil {
				continue
			}
			if _, err := clusters.ReadAdminInfo(ctx, clusterId); err != nil {
				return fmt.Errorf("%s gave no admin info for %s: %w", buckets.where(), clusterId, err)
			}
			return fmt.Errorf("%s gave no admin info for %s: %w", buckets.where(), clusterId, errAdminInfoUnkept)
		}
		return nil
	})
}

// Calls call, one of the Net's setup calls on its stores, and while it fails on a transport failure sends it again
// after SetupRetryWait, at most SetupRetryCount times more (QD-1395): a stack resetting connections under load fails
// no Net, while a refusal or any other answer the store gave returns at once, as does ctx's end. What it returns is
// call's last error.
//
// A transport failure is a request that got no answer: a connection reset, refused or closed before an answer, or
// none in time, which net/http reports as a *url.Error and minio-go passes on as one, an EOF included (its
// "Connection closed by foreign host"); a body cut off mid-read; and errAdminInfoUnkept. Not one: an answer, such as
// minio-go's ErrorResponse (AccessDenied, NoSuchBucket) or an admin read's status, size or parse failure; a
// certificate the store showed, the same each time; and a failure once ctx has ended, a deadline of its own included.
func retrySetup(ctx context.Context, settings *Settings, call func() error) error {
	transportFailure := func(err error) bool {
		if ctx.Err() != nil {
			return false
		}
		if errors.Is(err, errAdminInfoUnkept) {
			return true
		}
		var certificateError *tls.CertificateVerificationError
		if errors.As(err, &certificateError) {
			return false
		}
		// *url.Error, *net.OpError, a syscall's errno and a timeout are each a net.Error
		var netError net.Error
		return errors.As(err, &netError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	}
	for retry := 0; ; retry += 1 {
		err := call()
		if err == nil || settings.SetupRetryCount <= retry || !transportFailure(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(settings.SetupRetryWait):
		}
	}
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
