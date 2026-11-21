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
//     metadata cluster at 80% raw or 70% inodes gets 507 CLUSTER_FULL for what §20.7's storage limit refuses;
//   - the versionless purge (EVSB-M10-01's evsservice.Purger, purge_wiring.go), which the engine hands each certain
//     commit's candidates and which deletes them once the readers' 60 s have passed, on the engine's format2 gate,
//     newest-version reader and hold cache and the usage producer the engine counts with. It runs on the host's
//     context, drains with the block and ends at Close.
//
// Ready is §19.4's evs row: the engine's probe of the first cluster, every cluster's sentinel read with this
// block's credentials and one pool shown (server.FileClusters.Ready), and every seam no request is honored without.
// ServiceHost.Quota is server/cli/evs's quotahost.NewBlockQuota over api's internal gate (QUOTA-M2-11, QD-055), which
// a block builds only once it holds its bearer and api's url, and which is ready only once it has read its first
// over-limit list. The block reads signed revocations on its first cluster and verifies
// authorization and write facts from a single watched cache of the network log.
//
// A spent network reads at evs_unpaid_read_rate across the env's evs blocks with no shared counter (§20.6), so each
// process paces at the rate ÷ the env's block count, which only the operator knows: sidecar.yml's evs gate carries
// it as blocks, beside services.yml's evs block count (QD-639; server.SidecarConfig.EvsBlocks), and NewHost passes
// it as QuotaGateSettings.Read.Blocks. It has no default, as tlsedge.yml's blocks has none (§13.9): a block whose
// gate is enabled with no count does not start, since a count taken as 1 would hand a spent network the whole rate
// from every process. A block is built once, so the count is read when it starts: a block that started under a
// disabled gate with no count stays not ready once the gate reads enabled, until it restarts with one, and a count
// changed under a running block takes that block's restart, shifting the rate until then and nothing else.
//
// The storage format a block runs as is its config's (block_config.go; QD-854): NewHost reads evs.yml's
// instance_format into the engine's EvsSettings.InstanceFormat, 2 where the file is absent, so a rollback deploys this
// binary with 1 and it runs format 1's rules and guard over format-2 data. A config that does not read, or names a
// format this code does not know, is an error: the block does not start, and serves /status not ready.
//
// The block's parts run on a context of the host's own, which ends only at Close, so a SIGTERM's drain still has
// its clusters, clock check, budget and batches while requests in flight finish.
//
// Each credential the block reads from its secret_files is a HostDeps field a test fills with its own, so a host run
// in a test reads none from vault: the clusters (file-clusters-evs.yml), the receipt keys (evs-receipt-keys.json),
// the svc- verifier its Authenticator and rev/ reader verify under (oauth-jwks-svc.json) and the Quota over
// internal-gate-evs.yml. What stays the process's when several hosts run in one test process (e2e-hosts-in-process):
// the Redis key-event subscriber and the metrics registry, which sidecarhost shares by design; sidecar.yml, whose evs
// gate and blocks count NewHost reads, and evs.yml, the block config, both of which server reads once per process
// (server.Testing_PushSidecarConfig sets sidecar.yml for every block at once); the svc- verifier a host given no
// HostDeps.ServiceTokens reads (oauth.Testing_SetServiceTokenVerifier); and the test hooks
// (sidecartest.InstallTestHooks). RunHost sets no process-wide state: the main sets operator's global Env.
package evshost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
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
	// The engine's. nil: evsservice.DefaultEvsSettings. Its InstanceFormat is not the host's to set: NewHost takes
	// the format from the block's config (QD-854), whatever this one holds.
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
	// The storage gate and a spent network's reads. nil: evsservice.DefaultQuotaGateSettings. Its Read.Blocks is
	// not the host's to set: NewHost takes the count from sidecar.yml's evs gate (QD-639), and keeps this one only
	// for a block that starts under a disabled gate with no count, which serves nothing.
	QuotaGate *evsservice.QuotaGateSettings
	// The versionless purge's: §11.13's 60 s allowance, 200 deletes a second, 1M ids queued and a 70 s drain. nil:
	// evsservice.DefaultPurgerSettings. Settings past §11.13's floors are an error: the block does not start.
	Purger *evsservice.PurgerSettings
	// sidecarhost.Run's: readiness's bounds and the status server's drain. nil: sidecarhost.DefaultRunSettings.
	Run *sidecarhost.RunSettings
}

// The defaults.
func DefaultHostSettings() *HostSettings {
	return &HostSettings{
		Evs:               evsservice.DefaultEvsSettings(),
		Chrony:            server.DefaultChronyClockCheckSettings(),
		UsageDrainTimeout: 10 * time.Second,
		QuotaGate:         evsservice.DefaultQuotaGateSettings(),
		Purger:            evsservice.DefaultPurgerSettings(),
		Run:               sidecarhost.DefaultRunSettings(),
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
	// The newest verified state. RunHost builds it over the block's cache and signed revocations.
	AuthState evsservice.EvsAuthState
	// The newest verified state of each network as a push's 5b and 5d read it (evsservice.NewAclGateViews over the
	// process's AclGateCache, C-09). RunHost builds it over the same cache as AuthState.
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
	// ServiceHost.Quota: server/cli/evs's quotahost.NewBlockQuota. nil leaves the block not ready.
	Quota sidecar.Quota
	// The write budget: evsservice.NewWriteBudget over the clusters' metadata clusters.
	Budget sidecar.WriteBudget
	// The engine's clock: real time.
	Clock sidecar.Clock
	// The svc- key set's public verifier, which the Authenticator verifies callers' service tokens under and the rev/
	// reader each rev/ object's signature: vault's oauth-jwks-svc.json, read once by the process
	// (oauth.VerifyServiceToken). A test's over keys of its own.
	ServiceTokens *oauth.ServiceTokenVerifier
	// The block's /status latch, so a test reads it: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
	// A listener already bound for /status and evs's routes, which RunHost serves in place of binding its status
	// port: a test's on 127.0.0.1:0, so the OS picks the port. RunHost takes it. nil: the port, as warp maps it.
	StatusListener net.Listener
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type Host struct {
	engine   *evsservice.EvsEngine
	settings *HostSettings
	clock    sidecar.Clock
	// what this host's readiness also waits on: nil for the engine's probe alone
	clustersReady func() bool
	// the Quota's own readiness, for one that says whether it has read an over-limit list (quotahost.HostQuota):
	// an error until it has. nil for a Quota that says nothing of it.
	quotaReady func() error
	// what the engine cannot honor a request without and the block was not given, each said once
	missing []string
	// sidecar.yml's evs block count as the read gate took it at the start, 0 for a block that started under a
	// disabled gate with none
	blocks int
	// the env's sidecar.yml as Ready reads it, for whether the gate is enabled now
	sidecarConfig func() *server.SidecarConfig
	// the storage format the block's config named at the start, which the engine runs as
	instanceFormat uint32
	// set once a block with no count has said the gate reads enabled
	blocksMissingLogged atomic.Bool

	quotaGate    *evsservice.QuotaGate
	hdd          *evsservice.HddAdmission
	usage        *evsservice.UsageBatcher
	clusterLimit *evsservice.ClusterLimit
	// the versionless purge, nil when the host could not build it, which leaves the block not ready
	purger *evsservice.Purger
	// what RunHost built for the engine, ended after it, in this order
	closeAfter []func()
	closeOnce  sync.Once
}

var _ sidecar.HttpEngine = (*Host)(nil)

// The engine on host, with the segment route's parts built here: the storage gate and usage producer over
// host.Quota, the HDD share over host.Redis for the segment cluster, and the versionless purge the engine hands each
// certain commit. host needs Blobs, Metrics and Budget; a missing Quota, segment cluster, deps' AuthState or receipt
// keys, or a purge the host cannot build, leaves the host not ready, never an error, so a block serves /status and
// says why. A host whose env enables the evs gate with no blocks count is an error, and so is one whose block config
// does not read or names an instance format this code does not know, or whose purge settings break §11.13's floors:
// see the package's header. The engine runs as the config's format, never settings'. ctx bounds everything built
// here and should outlive the drain: RunHost gives one that ends only at Close.
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
	purgerSettings := settings.Purger
	if purgerSettings == nil {
		purgerSettings = evsservice.DefaultPurgerSettings()
	}
	// as the engine's own settings are: a block whose purge could never run does not start
	if err := purgerSettings.Validate(); err != nil {
		return nil, fmt.Errorf("evs host: %w", err)
	}
	// the env's count, never one a host works out from its peers (QD-639)
	sidecarConfig := server.CurrentSidecarConfig()
	blocks, ok := sidecarConfig.EvsBlocks()
	if !ok && sidecarConfig.Service(server.SidecarGateEvs).Enabled() {
		return nil, errors.New("evs host: sidecar.yml's evs gate is enabled with no blocks, the env's evs block count, which has no default (QD-639)")
	}
	// the format this block was deployed as, from its config alone (QD-854)
	blockConfig, err := LoadBlockConfig()
	if err != nil {
		return nil, fmt.Errorf("evs host: %w", err)
	}
	evsSettings := blockConfig.EvsSettings(settings.Evs)
	self := &Host{
		settings:       settings,
		clock:          host.ClockOrReal(),
		clustersReady:  deps.ClustersReady,
		blocks:         blocks,
		sidecarConfig:  server.CurrentSidecarConfig,
		instanceFormat: blockConfig.InstanceFormat,
	}
	fail := func(err error) (*Host, error) {
		self.closeParts()
		return nil, err
	}
	if host.Quota == nil {
		self.missing = append(self.missing, "ServiceHost.Quota, which server/cli/evs builds over api's internal gate once the block holds its bearer and api's url (QUOTA-M2-11's quotahost.NewBlockQuota)")
	} else if readiness, ok := host.Quota.(interface{ Ready() error }); ok {
		// a process that has never loaded the over-limit list stays not ready (plan-SOCK-gate-data)
		self.quotaReady = readiness.Ready
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
		quotaGate, err := evsservice.NewQuotaGate(ctx, host, hostQuotaGateSettings(settings.QuotaGate, blocks))
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
	// hold/ as the engine's slot takeovers and the purge's DELETEs read it: one read at most 5 s old serves both
	holds := evsservice.NewHoldCache(host.Blobs, self.clock, nil)
	// the purge takes the engine's newest-version reader, so the engine starts with the sink the purge fills below
	purgeSink := &hostPurgeSink{}
	engine, err := evsservice.NewEvsEngine(ctx, host, evsSettings, &evsservice.EvsEngineDeps{
		AuthState:  deps.AuthState,
		ServiceLog: deps.ServiceLog,
		Log:        hostLogger{},
		Uploads:    uploads,
		GateViews:  deps.GateViews,
		Purge:      purgeSink,
		Holds:      holds,
	})
	if err != nil {
		return fail(err)
	}
	self.engine = engine
	self.startPurger(ctx, host, purgerSettings, holds, purgeSink)
	if 0 < len(self.missing) {
		glog.Errorf("[evs]not ready without %s\n", strings.Join(self.missing, "; "))
	}
	return self, nil
}

// The quota gate's settings with the env's block count as the read gate's: settings, the defaults when nil, copied
// so the caller's stay its own. A count below 1, a block that started with none, leaves settings' own.
func hostQuotaGateSettings(settings *evsservice.QuotaGateSettings, blocks int) *evsservice.QuotaGateSettings {
	if settings == nil {
		settings = evsservice.DefaultQuotaGateSettings()
	}
	gateSettings := *settings
	if gateSettings.Read == nil {
		gateSettings.Read = evsservice.DefaultQuotaReadSettings()
	}
	readSettings := *gateSettings.Read
	if 1 <= blocks {
		readSettings.Blocks = blocks
	}
	gateSettings.Read = &readSettings
	return &gateSettings
}

// The env's evs block count as this block took it at its start, 0 when it started with none.
func (self *Host) Blocks() int {
	return self.blocks
}

// The storage format the engine runs as, which the block's config named at the start.
func (self *Host) InstanceFormat() uint32 {
	return self.instanceFormat
}

// Whether the block's read gate holds the env's block count wherever the gate is enabled: false only for a block
// that started under a disabled gate with no count once the gate reads enabled, which it says once.
func (self *Host) blocksReady() bool {
	if 1 <= self.blocks || !self.sidecarConfig().Service(server.SidecarGateEvs).Enabled() {
		return true
	}
	if self.blocksMissingLogged.CompareAndSwap(false, true) {
		glog.Errorf("[evs]not ready: sidecar.yml's evs gate reads enabled and this block started with no blocks count; it takes one at a restart (QD-639)\n")
	}
	return false
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

// §19.4's evs row: every seam given, the env's block count held wherever the gate is enabled, the clusters ready,
// the Quota's first over-limit list read and the engine's probe of the first cluster answered.
func (self *Host) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	if !self.blocksReady() {
		return false
	}
	if self.clustersReady != nil && !self.clustersReady() {
		return false
	}
	if self.quotaReady != nil && self.quotaReady() != nil {
		return false
	}
	return self.engine.Ready()
}

// Every route, behind the engine's guard.
func (self *Host) Handler() http.Handler {
	return self.engine.Handler()
}

// The engine finishes what is in flight, then the purge deletes what falls due within its DrainTimeout, taking no
// new commit, and the usage producer sends its open window, with what the purge freed, within UsageDrainTimeout.
// What the purge leaves is the sweep's.
func (self *Host) Drain() {
	self.engine.Drain()
	if self.purger != nil {
		self.purger.Drain()
	}
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

// Ends the engine, then what it used: the purge, whose queue is the sweep's, the segment route's parts, then what
// RunHost built for it. Safe after Drain, and more than once.
func (self *Host) Close() {
	self.closeOnce.Do(func() {
		self.engine.Close()
		self.closeParts()
	})
}

// Ends the purge, the segment route's parts and what RunHost built, the engine already ended or never started: the
// purge before the usage producer it reports to.
func (self *Host) closeParts() {
	if self.purger != nil {
		self.purger.Close()
	}
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
	if settings == nil {
		settings = DefaultHostSettings()
	}
	engine := newBlockEngine(ctx, settings, deps)
	options := blockRunOptions(engine, statusPort, deps)
	options.StatusListener = deps.StatusListener
	options.Settings = settings.Run
	return sidecarhost.Run(ctx, options)
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
	// before any store is touched, so a config this code does not run under is said whatever else is missing;
	// NewHost reads it again for the format the engine takes
	blockConfig, err := LoadBlockConfig()
	if err != nil {
		return notReady(err)
	}
	if blockConfig.InstanceFormat < sidecar.EvsRepoFormat2 {
		// a rollback's block, said at every start so nobody reads its refusals as a fault
		glog.Warningf("[evs]%s sets instance format %d, a rollback's: this block refuses format-2 pushes and writes nothing to a format-2 repo\n", BlockConfigFileName, blockConfig.InstanceFormat)
	} else {
		glog.Infof("[evs]instance format %d\n", blockConfig.InstanceFormat)
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
		Blobs:         clusters,
		ClockCheck:    clockCheck,
		Quota:         deps.Quota,
		Clock:         deps.Clock,
		ServiceTokens: deps.ServiceTokens,
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
	// evs's topics are a wake class: a hint to a token holder is followed by §18.7's enqueue while the
	// wake switch is on (PROTO-M8-01)
	wake, err := pushservice.NewWakeEnqueuer(host.Redis, nil, host.ClockOrReal(), func() bool {
		return host.GatesOrClosed().Switch(sidecar.GateSwitchWake)
	}, host.Metrics)
	if err != nil {
		return notReady(fmt.Errorf("the wake enqueuer: %w", err))
	}
	push, err := pushservice.NewWakingHintWriter(host.Redis, nil, nil, host.Metrics, wake)
	if err != nil {
		return notReady(fmt.Errorf("the hint writer: %w", err))
	}
	host.Push = push
	hostDeps := *deps
	if hostDeps.AuthState == nil || hostDeps.GateViews == nil {
		cache, closeCache, err := sidecarhost.WatchAclGate(hostCtx, host, nil, nil)
		if err != nil {
			return notReady(fmt.Errorf("the authorization cache: %w", err))
		}
		closers = append(closers, closeCache)
		if hostDeps.AuthState == nil {
			revSettings := sidecar.DefaultRevocationSettings()
			revSettings.Clock = host.Clock
			checker := sidecar.NewRevocationChecker(host.Blobs, sidecarhost.RevocationVerify(deps.ServiceTokens), revSettings)
			hostDeps.AuthState = evsservice.NewAclEvsAuthState(cache, checker)
		}
		if hostDeps.GateViews == nil {
			hostDeps.GateViews = evsservice.NewAclGateViews(cache)
		}
	}
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
