// Package gkm is server/cli/gkm's host (KEYS-M2-17; D-OPS-2, D-REPO-3, §10.14, §19.4): sidecar's gkmservice engine on
// server's stores, run as one warp block at gkm.<root> by sidecarhost.Run, and what a T3 test starts the engine
// through. gkm is its own service so api's daily deploys cannot stall MLS commits (D-OPS-2); its reconciler runs in
// taskworker (taskworker/gkm_reconciler.go).
//
// gkm is stateless over §10.6's objects: the block holds no Postgres pool, so sidecarhost.Run never reads
// migration_audit for it, no migration is ever its (§10.14's tables: none), and §22.7 asserts it reaches none
// (plan-KEYS-hint-addressing). Redis carries only {gka_<group_id>}, the hints' {scp_} and §11.13's {nvb_}. The engine
// never imports server, so go-redis, minio-go and prometheus reach it only through what this package builds:
//
//   - Blobs: every metadata cluster vault's file-clusters-gkm.yml names (server.LoadFileClusters), one bucket each;
//   - Budget: §11.13's write budget for this process over those clusters (evsservice.NewWriteBudget), which every gkm
//     write spends before it is sent (gkmservice's budget.go);
//   - Push: pushservice's hint writer on the block's Redis, with no sidecar_id resolver until
//     plan-KEYS-hint-addressing's internal route, POST /sidecar/identity/clients, and its credential land, so a hint
//     by sidecar id is counted as unresolved and lost, which the clients' hourly resync covers (§6.8);
//   - Redis, Metrics, Gates and the service token Authenticator from sidecarhost.NewServiceHost, the gates read from
//     sidecar.yml's gkm: {enabled, networks, percent} and reseed every 60 s (§19.11).
//
// Ready is §19.4's gkm row, §10.6's objects reachable: the engine's probe of the first cluster, every cluster's
// sentinel read with this block's credentials and one pool shown (server.FileClusters.Ready), and every seam no
// request is honored without. Server has no implementation yet of one: GkmAuthState, gkm's newest verified
// authorization state over §8.9's gate cache (C-09: AclGateCache, with ACL-M2-14's HonorScope for the gkm audience,
// sidecar rows and §7.8's rev/ reader). Until it lands a real block stays not ready, failing closed; HostDeps hands
// it to a test, or to the change that lands it.
//
// The block's parts run on a context of the host's own, which ends only at Close, so a SIGTERM's drain still has its
// clusters, budget and hint queue while requests in flight finish.
package gkm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// vault's file-clusters-<service>.yml the block reads: every metadata cluster it holds credentials for (§19.4's gkm
// row, "metadata clusters").
const HostFileClustersService = "gkm"

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "gkm"

// The block's tunables beyond the engine's.
type HostSettings struct {
	// The engine's. nil: gkmservice.DefaultGkmServiceSettings.
	Gkm *gkmservice.GkmServiceSettings
	// The process's write budget's. nil: evsservice.DefaultWriteBudgetSettings.
	Budget *evsservice.WriteBudgetSettings
}

// The defaults.
func DefaultHostSettings() *HostSettings {
	return &HostSettings{
		Gkm:    gkmservice.DefaultGkmServiceSettings(),
		Budget: evsservice.DefaultWriteBudgetSettings(),
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

// The seams a test, or the change that lands a missing one, fills. Each nil takes the block's own, or, where server
// has none yet, leaves the block not ready.
type HostDeps struct {
	// gkm's newest verified authorization state. nil: none until GkmAuthState over AclGateCache lands, and the block
	// stays not ready, since the engine honors nothing without it.
	AuthState gkmservice.GkmAuthState
	// gkm-9's request log. nil: through glog, one line a request.
	ServiceLog gkmservice.GkmServiceLog
	// Whether the clusters are ready, beside the engine's own probe of the first. nil: the engine's probe alone. Run
	// gives HostClusters.Ready.
	ClustersReady func() bool

	// Run's alone, each nil taking the block's own:
	//
	// The clusters: vault's file-clusters-gkm.yml.
	Clusters HostClusters
	// The write budget: evsservice.NewWriteBudget over the clusters' metadata clusters.
	Budget sidecar.WriteBudget
	// ServiceHost.Quota, whose limits weigh each network's writes in the write budget (store_weight):
	// server/cli/gkm's quotahost.NewBlockQuota over api's internal gate (QUOTA-M2-11). nil weighs every network as
	// Community's, and readiness does not wait on it.
	Quota sidecar.Quota
	// The hint writer: pushservice's on the block's Redis, with no resolver (plan-KEYS-hint-addressing).
	Push sidecar.Push
	// The engine's clock: real time.
	Clock sidecar.Clock
	// The block's /status latch, so a test reads it: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type Host struct {
	engine *gkmservice.GkmEngine
	// what this host's readiness also waits on: nil for the engine's probe alone
	clustersReady func() bool
	// what the engine cannot honor a request without and the block was not given, each said once
	missing []string
	// what Run built for the engine, ended after it, in this order
	closeAfter []func()
	closeOnce  sync.Once
}

var _ sidecar.HttpEngine = (*Host)(nil)

// The engine on host, which needs Blobs, Budget and Metrics. A missing AuthState leaves the host not ready, never an
// error, so a block serves /status and says why: the engine still answers through its guard, every route 503. ctx
// bounds everything built here and should outlive the drain: Run gives one that ends only at Close.
func NewHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if host == nil || host.Blobs == nil {
		return nil, errors.New("gkm host: no clusters")
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	if deps == nil {
		deps = &HostDeps{}
	}
	self := &Host{clustersReady: deps.ClustersReady}
	if deps.AuthState == nil {
		self.missing = append(self.missing, "gkm's newest verified authorization state, GkmAuthState over §8.9's gate cache with ACL-M2-14's HonorScope (C-09)")
	}
	engine, err := gkmservice.NewGkmEngine(ctx, host, settings.Gkm, &gkmservice.GkmEngineDeps{
		AuthState:  deps.AuthState,
		ServiceLog: deps.ServiceLog,
		Log:        hostLogger{},
	})
	if err != nil {
		return nil, err
	}
	self.engine = engine
	if 0 < len(self.missing) {
		glog.Errorf("[gkm]not ready without %s\n", strings.Join(self.missing, "; "))
	}
	return self, nil
}

// The engine, for a test that drives it.
func (self *Host) Engine() *gkmservice.GkmEngine {
	return self.engine
}

// What the block was not given that the engine honors no request without, each a reason: empty once every seam is
// there.
func (self *Host) Missing() []string {
	return append([]string{}, self.missing...)
}

// §19.4's gkm row: every seam given, the clusters ready and the engine's probe of the first answered.
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

// The engine finishes what is in flight.
func (self *Host) Drain() {
	self.engine.Drain()
}

// Ends the engine, then what Run built for it. Safe after Drain, and more than once.
func (self *Host) Close() {
	self.closeOnce.Do(func() {
		self.engine.Close()
		for _, close := range self.closeAfter {
			close()
		}
		self.closeAfter = nil
	})
}

// A block that could not start: never ready, serving nothing but /status.
type notReadyEngine struct{}

func (notReadyEngine) Ready() bool { return false }
func (notReadyEngine) Drain()      {}
func (notReadyEngine) Close()      {}

// What a block's main gives Run.
type RunOptions struct {
	// GET /status's port, which also serves gkm's routes: 80 in a block.
	Port int
	// nil: DefaultHostSettings.
	Settings *HostSettings
	// nil: the block's own stores, and no authorization state yet (the package header).
	Deps *HostDeps
}

// Runs the block until ctx ends, through sidecarhost.Run (§19.4): GET /status on the port from the first moment, the
// engine's routes beside it once ready, then at ctx's end the drain and Close. A block that cannot start logs why and
// serves /status not ready until ctx ends.
func Run(ctx context.Context, options RunOptions) error {
	deps := options.Deps
	if deps == nil {
		deps = &HostDeps{}
	}
	engine := newBlockEngine(ctx, options.Settings, deps)
	return sidecarhost.Run(ctx, blockRunOptions(engine, options.Port, deps))
}

// What sidecarhost.Run takes for engine: no Postgres and no pending group, since gkm keeps nothing in Postgres
// (§10.14), and its routes on the status port.
func blockRunOptions(engine sidecar.Engine, port int, deps *HostDeps) *sidecarhost.RunOptions {
	options := &sidecarhost.RunOptions{
		Service:    hostService,
		StatusPort: port,
		Engine:     engine,
		WarpStatus: deps.WarpStatus,
	}
	if httpEngine, ok := engine.(sidecar.HttpEngine); ok {
		options.Http = httpEngine
	}
	return options
}

// The block's engine on its own stores and deps', or a never-ready one when it cannot start, having said why.
// Everything is built on a context of its own that the engine's Close ends, so ctx's end, a block's SIGTERM, starts
// the drain without ending what the drain needs.
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
		glog.Errorf("[gkm]not ready: %v\n", err)
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
	host := sidecarhost.NewServiceHost(hostCtx, &sidecarhost.ServiceHostOptions{
		Blobs: clusters,
		Quota: deps.Quota,
		Clock: deps.Clock,
	})
	budget := deps.Budget
	if budget == nil {
		client, err := evsservice.NewWriteBudget(hostCtx, host, nil, settings.Budget)
		if err != nil {
			return notReady(fmt.Errorf("the write budget: %w", err))
		}
		closers = append(closers, client.Close)
		budget = client
	}
	host.Budget = budget
	push := deps.Push
	if push == nil {
		// no resolver: a hint by sidecar id is lost and counted until plan-KEYS-hint-addressing's route lands
		writer, err := pushservice.NewHintWriter(host.Redis, nil, nil, host.Metrics)
		if err != nil {
			return notReady(fmt.Errorf("the hint writer: %w", err))
		}
		push = writer
	}
	host.Push = push
	hostDeps := *deps
	if hostDeps.ClustersReady == nil {
		hostDeps.ClustersReady = clusters.Ready
	}
	gkmHost, err := NewHost(hostCtx, host, settings, &hostDeps)
	if err != nil {
		return notReady(err)
	}
	for i := len(closers) - 1; 0 <= i; i -= 1 {
		gkmHost.closeAfter = append(gkmHost.closeAfter, closers[i])
	}
	return gkmHost
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
