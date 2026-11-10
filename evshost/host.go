// Package evshost is server/cli/evs's host (EVS1-M2-21; §11.20, §19.4, R-EVS-15, D-EVS-13): sidecar's
// evsservice engine on server's stores, run as one warp block at evs.<root> by sidecarhost.Run, and what a T3
// test starts the engine through.
//
// evs is stateless over §11.8's objects: the block holds no Postgres pool, so sidecarhost.Run never reads
// migration_audit for it and no pending group gates it, and Redis carries only the soft state the engine rebuilds
// (owner decisions 2 and 136). The engine never imports server, so go-redis, minio-go and prometheus reach evs only
// through what this package builds:
//
//   - Blobs: every cluster vault's file-clusters-evs.yml names (server.LoadFileClusters), one bucket each, with
//     the segment cluster's DELETE from v1;
//   - ClockCheck: chronyd on the services network (server.NewChronyClockCheck), so an unknown offset or one past
//     50 ms answers pushes 503 before step 4 (§11.8's fence row);
//   - Budget: §11.13's write budget for this process over the metadata clusters (evsservice.NewWriteBudget);
//   - Push: pushservice's hint writer on the block's Redis, which no sidecar_id resolver feeds yet
//     (plan-KEYS-hint-addressing);
//   - Redis, Metrics, Gates and the service token Authenticator from sidecarhost.NewServiceHost;
//   - the segment route's receipt keys (HostReceiptKeysFileName), storage gate, HDD share and usage producer, and
//     the cohort limit (evsservice.ClusterLimit, EVSB-M3-02) over the write budget's at_limit flags, so a repo on a
//     metadata cluster at 80% raw or 70% inodes gets 507 CLUSTER_FULL for what §20.7's storage limit refuses.
//
// Ready is §19.4's evs row: the engine's probe of the first cluster, every cluster's sentinel read with this
// block's credentials and one pool shown (server.FileClusters.Ready), and every seam no request is honored without.
// Server has no implementation yet of two of them, so a real block stays not ready, failing closed, until each
// lands: ServiceHost.Quota, which QUOTA-M2-11's NewApiQuota gives a host with no Postgres once
// plan-QUOTA-host-reach's route exists, and the EvsAuthState ACL-M2-14 builds over AclGateCache (C-09: a real host
// with no projection source stays not ready). HostDeps hands each to a test, or to the change that lands it.
//
// The block's parts run on a context of the host's own, which ends only at Close, so a SIGTERM's drain still has
// its clusters, clock check, budget and batches while requests in flight finish.
package evshost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The block's own files, each one of its secret_files under its vault root (§19.4's evs row).
const (
	// vault's file-clusters-<service>.yml: every cluster this block holds credentials for.
	HostFileClustersService = "evs"
	// The upload receipts' MAC keys, current and previous (§11.13's keys row), as evsservice.ParseReceiptKeys
	// reads them. The design names no file; this is the plan's (proposed), and OPS-M2-03's fragment mounts it.
	HostReceiptKeysFileName = "evs-receipt-keys.json"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "evs"

// The block's tunables beyond the engine's.
type HostSettings struct {
	// The engine's. nil: evsservice.DefaultEvsSettings.
	Evs *evsservice.EvsSettings
	// chronyd's check (§19.6's clocks row). nil: server.DefaultChronyClockCheckSettings, warp's services
	// network name.
	Chrony *server.ChronyClockCheckSettings
	// How long Drain waits for the usage producer to send its open window and hear back, as a SIGTERM asks
	// (§20.6): 10 s, well inside sidecarhost.Run's 60 s shutdown, since a window not sent costs at most a minute
	// of this process's bytes. Non-positive sends nothing at the drain.
	UsageDrainTimeout time.Duration
	// The segment cluster's HDD share. nil: evsservice.DefaultHddAdmissionSettings.
	Hdd *evsservice.HddAdmissionSettings
}

// The defaults.
func DefaultHostSettings() *HostSettings {
	return &HostSettings{
		Evs:               evsservice.DefaultEvsSettings(),
		Chrony:            server.DefaultChronyClockCheckSettings(),
		UsageDrainTimeout: 10 * time.Second,
	}
}

// The clusters a block holds, as server.FileClusters is: the store, §19.4's readiness and an end.
type HostClusters interface {
	sidecar.BlobStore
	// Whether every cluster's sentinel reads with this block's credentials and its admin info shows one pool.
	Ready() bool
	// Ends the clusters' checks.
	Close()
}

// The seams a test, or the change that lands a missing one, fills. Each nil takes the block's own, or, where
// server has none yet, leaves the block not ready.
type HostDeps struct {
	// The newest verified authorization state. nil: none until ACL-M2-14's over AclGateCache, and the block
	// stays not ready, since the engine honors nothing without it.
	AuthState evsservice.EvsAuthState
	// The newest verified state of each network as a push's 5b and 5d read it (evsservice.NewAclGateViews over the
	// process's AclGateCache, C-09). nil: none until a process hosts the cache (ACL-M2-14), and the block stays not
	// ready, since every push answers 503 without it.
	GateViews evsservice.EvsGateViews
	// The upload receipts' keys. nil: HostReceiptKeysFileName from the block's vault, and a block whose file
	// does not load stays not ready.
	ReceiptKeys *evsservice.ReceiptKeys
	// evs-7's service log. nil: through glog, one line a request.
	ServiceLog evsservice.EvsServiceLog
	// Whether the clusters are ready, beside the engine's own probe of the first. nil: the engine's probe alone,
	// as for a store that keeps no sentinel. RunHost gives HostClusters.Ready.
	ClustersReady func() bool

	// RunHost's alone, each nil taking the block's own:
	//
	// The clusters: vault's file-clusters-evs.yml.
	Clusters HostClusters
	// The clock check: chronyd at HostSettings.Chrony's address.
	ClockCheck sidecar.ClockCheck
	// ServiceHost.Quota: none until QUOTA-M2-11's NewApiQuota, and the block stays not ready.
	Quota sidecar.Quota
	// The write budget: evsservice.NewWriteBudget over the clusters' metadata clusters.
	Budget sidecar.WriteBudget
	// The engine's clock: real time.
	Clock sidecar.Clock
	// The block's /status latch, so a test reads it: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type Host struct {
	engine   *evsservice.EvsEngine
	settings *HostSettings
	clock    sidecar.Clock
	// what this host's readiness also waits on: nil for the engine's probe alone
	clustersReady func() bool
	// what the engine cannot honor a request without and the block was not given, each said once
	missing []string

	quotaGate    *evsservice.QuotaGate
	hdd          *evsservice.HddAdmission
	usage        *evsservice.UsageBatcher
	clusterLimit *evsservice.ClusterLimit
	// what RunHost built for the engine, ended after it, in this order
	closeAfter []func()
	closeOnce  sync.Once
}

var _ sidecar.HttpEngine = (*Host)(nil)

// The engine on host, with the segment route's parts built here: the storage gate and usage producer over
// host.Quota, and the HDD share over host.Redis for the segment cluster. host needs Blobs, Metrics and Budget;
// a missing Quota, segment cluster, deps' AuthState or receipt keys leaves the host not ready, never an error, so
// a block serves /status and says why. ctx bounds everything built here and should outlive the drain: RunHost
// gives one that ends only at Close.
func NewHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if host == nil || host.Blobs == nil {
		return nil, errors.New("evs host: no clusters")
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	if deps == nil {
		deps = &HostDeps{}
	}
	self := &Host{
		settings:      settings,
		clock:         host.ClockOrReal(),
		clustersReady: deps.ClustersReady,
	}
	fail := func(err error) (*Host, error) {
		self.closeParts()
		return nil, err
	}
	if host.Quota == nil {
		self.missing = append(self.missing, "ServiceHost.Quota, which QUOTA-M2-11's NewApiQuota gives a host with no Postgres (plan-QUOTA-host-reach)")
	}
	if deps.AuthState == nil {
		self.missing = append(self.missing, "the newest verified authorization state, ACL-M2-14's EvsAuthState over AclGateCache (C-09)")
	}
	if deps.GateViews == nil {
		self.missing = append(self.missing, "the gate views a push's 5b and 5d read, evsservice.NewAclGateViews over the process's AclGateCache (C-09)")
	}
	keys := deps.ReceiptKeys
	if keys == nil {
		// the block's own, from its vault
		load := func() (*evsservice.ReceiptKeys, error) {
			resource, err := server.Vault.SimpleResource(HostReceiptKeysFileName)
			if err != nil {
				return nil, err
			}
			data, err := resource.BytesE()
			if err != nil {
				return nil, err
			}
			return evsservice.ParseReceiptKeys(data, nil)
		}
		var err error
		if keys, err = load(); err != nil {
			self.missing = append(self.missing, fmt.Sprintf("the receipt keys in %s (%v)", HostReceiptKeysFileName, err))
		}
	}
	// the budget's at_limit flags, which a budget that keeps none reads as never full
	clusterLimit, err := evsservice.NewClusterLimitOfHost(host, nil)
	if err != nil {
		return fail(err)
	}
	self.clusterLimit = clusterLimit
	uploads := &evsservice.EvsUploadDeps{Keys: keys, ClusterLimit: clusterLimit}
	if host.Quota != nil {
		quotaGate, err := evsservice.NewQuotaGate(ctx, host, nil)
		if err != nil {
			return fail(err)
		}
		self.quotaGate = quotaGate
		uploads.Quota = quotaGate
		usage, err := evsservice.NewUsageBatcher(ctx, host, nil)
		if err != nil {
			return fail(err)
		}
		self.usage = usage
		uploads.Usage = usage
	}
	segmentCluster, err := host.Blobs.SegmentCluster()
	switch {
	case err != nil:
		// every upload would answer 503, so the block says so rather than serve
		self.missing = append(self.missing, fmt.Sprintf("a segment cluster among the clusters (%v)", err))
	case host.Redis != nil:
		hdd, err := evsservice.NewHddAdmission(ctx, host, segmentCluster.ClusterId(), settings.Hdd)
		if err != nil {
			return fail(err)
		}
		self.hdd = hdd
		uploads.Hdd = hdd
	}
	engine, err := evsservice.NewEvsEngine(ctx, host, settings.Evs, &evsservice.EvsEngineDeps{
		AuthState:  deps.AuthState,
		ServiceLog: deps.ServiceLog,
		Log:        hostLogger{},
		Uploads:    uploads,
		GateViews:  deps.GateViews,
	})
	if err != nil {
		return fail(err)
	}
	self.engine = engine
	if 0 < len(self.missing) {
		glog.Errorf("[evs]not ready without %s\n", strings.Join(self.missing, "; "))
	}
	return self, nil
}

// The engine, for a test that drives its parts.
func (self *Host) Engine() *evsservice.EvsEngine {
	return self.engine
}

// What the block was not given that the engine honors no request without, each a reason: empty once every seam
// is there.
func (self *Host) Missing() []string {
	return append([]string{}, self.missing...)
}

// §19.4's evs row: every seam given, the clusters ready and the engine's probe of the first answered.
func (self *Host) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	if self.clustersReady != nil && !self.clustersReady() {
		return false
	}
	return self.engine.Ready()
}

// Every route, behind the engine's guard.
func (self *Host) Handler() http.Handler {
	return self.engine.Handler()
}

// The engine finishes what is in flight, then the usage producer sends its open window, within
// UsageDrainTimeout.
func (self *Host) Drain() {
	self.engine.Drain()
	if self.usage == nil || self.settings.UsageDrainTimeout <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := self.clock.AfterFunc(self.settings.UsageDrainTimeout, cancel)
	defer stop()
	if err := self.usage.Drain(ctx); err != nil {
		glog.Warningf("[evs]usage not all sent at the drain: %v\n", err)
	}
}

// Ends the engine, then what it used: the segment route's parts, then what RunHost built for it. Safe after
// Drain, and more than once.
func (self *Host) Close() {
	self.closeOnce.Do(func() {
		self.engine.Close()
		self.closeParts()
	})
}

// Ends the segment route's parts and what RunHost built, the engine already ended or never started.
func (self *Host) closeParts() {
	if self.quotaGate != nil {
		self.quotaGate.Close()
	}
	if self.hdd != nil {
		self.hdd.Close()
	}
	if self.usage != nil {
		self.usage.Close()
	}
	for _, close := range self.closeAfter {
		close()
	}
	self.closeAfter = nil
}

// A block that could not start: never ready, serving nothing but /status.
type notReadyEngine struct{}

func (notReadyEngine) Ready() bool { return false }
func (notReadyEngine) Drain()      {}
func (notReadyEngine) Close()      {}

// Runs the block until ctx ends, through sidecarhost.Run (§19.4): GET /status on statusPort from the first
// moment, the engine's routes beside it once ready, then at ctx's end the drain and Close. A block that cannot
// start logs why and serves /status not ready until ctx ends. settings and deps may be nil.
func RunHost(ctx context.Context, settings *HostSettings, deps *HostDeps, statusPort int) error {
	if deps == nil {
		deps = &HostDeps{}
	}
	engine := newBlockEngine(ctx, settings, deps)
	return sidecarhost.Run(ctx, blockRunOptions(engine, statusPort, deps))
}

// What sidecarhost.Run takes for engine: no Postgres and no pending group, since evs keeps nothing in Postgres
// (R-EVS-15), and its routes on the status port.
func blockRunOptions(engine sidecar.Engine, statusPort int, deps *HostDeps) *sidecarhost.RunOptions {
	options := &sidecarhost.RunOptions{
		Service:    hostService,
		StatusPort: statusPort,
		Engine:     engine,
		WarpStatus: deps.WarpStatus,
	}
	if httpEngine, ok := engine.(sidecar.HttpEngine); ok {
		options.Http = httpEngine
	}
	return options
}

// The block's engine on its own stores and deps', or a never-ready one when it cannot start, having said why.
// Everything is built on a context of its own that the engine's Close ends, so ctx's end, a block's SIGTERM,
// starts the drain without ending what the drain needs.
func newBlockEngine(ctx context.Context, settings *HostSettings, deps *HostDeps) sidecar.Engine {
	if settings == nil {
		settings = DefaultHostSettings()
	}
	hostCtx, hostCancel := context.WithCancel(context.WithoutCancel(ctx))
	// ended last first, after the engine
	closers := []func(){hostCancel}
	closeAll := func() {
		for i := len(closers) - 1; 0 <= i; i -= 1 {
			closers[i]()
		}
	}
	notReady := func(err error) sidecar.Engine {
		closeAll()
		glog.Errorf("[evs]not ready: %v\n", err)
		return notReadyEngine{}
	}
	clusters := deps.Clusters
	if clusters == nil {
		fileClusters, err := server.LoadFileClusters(hostCtx, HostFileClustersService, nil)
		if err != nil {
			return notReady(fmt.Errorf("file-clusters-%s.yml: %w", HostFileClustersService, err))
		}
		clusters = fileClusters
	}
	closers = append(closers, clusters.Close)
	clockCheck := deps.ClockCheck
	if clockCheck == nil {
		// one to a process, ending with the host's context after the engine has closed
		clockCheck = server.NewChronyClockCheck(hostCtx, settings.Chrony)
	}
	host := sidecarhost.NewServiceHost(hostCtx, &sidecarhost.ServiceHostOptions{
		Blobs:      clusters,
		ClockCheck: clockCheck,
		Quota:      deps.Quota,
		Clock:      deps.Clock,
	})
	budget := deps.Budget
	if budget == nil {
		client, err := evsservice.NewWriteBudget(hostCtx, host, nil, nil)
		if err != nil {
			return notReady(fmt.Errorf("the write budget: %w", err))
		}
		closers = append(closers, client.Close)
		budget = client
	}
	host.Budget = budget
	push, err := pushservice.NewHintWriter(host.Redis, nil, nil, host.Metrics)
	if err != nil {
		return notReady(fmt.Errorf("the hint writer: %w", err))
	}
	host.Push = push
	hostDeps := *deps
	if hostDeps.ClustersReady == nil {
		hostDeps.ClustersReady = clusters.Ready
	}
	evsHost, err := NewHost(hostCtx, host, settings, &hostDeps)
	if err != nil {
		return notReady(err)
	}
	for i := len(closers) - 1; 0 <= i; i -= 1 {
		evsHost.closeAfter = append(evsHost.closeAfter, closers[i])
	}
	return evsHost
}

// glog as the engine's sidecar.Logger.
type hostLogger struct{}

func (self hostLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self hostLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self hostLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self hostLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self hostLogger) V(level int32) sidecar.Verbose {
	return hostVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the engine's sidecar.Verbose.
type hostVerbose glog.Verbose

func (self hostVerbose) Enabled() bool {
	return bool(self)
}

func (self hostVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self hostVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}
