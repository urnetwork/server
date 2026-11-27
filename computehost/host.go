// Package computehost is server/cli/compute's host (CSRV-M12-03; D-CSRV-1, DESIGN-COMPUTE §14.2): sidecar's
// computeservice engine, the broker and orchestrator, on server's stores, run as one warp block at compute.<root> by
// sidecarhost.Run, and what a test starts the engine through.
//
// The block holds a Postgres pool (§19.4's state column; core.ProcessGrants' compute), which its engine's handlers
// reach through the process's Env (owner decision 594), and readiness waits for MG-C1 and MG-C2, the ledger and the
// providers, through sidecarhost.Run's groups and router.CheckStartupReadiness, so a production block stays unready
// until M12's Gn-out promotes them; MG-C3, the enclaves, is read per request on the enclave routes alone, so the block
// is ready between M12's exit and M14's. What it builds for the engine:
//
//   - Redis, Metrics, Gates and the service token Authenticator from sidecarhost.NewServiceHost, the gates answering
//     §19.11's compute line, closed in every env's sidecar.yml until the owner opens it;
//   - the first metadata cluster read-only (file-clusters-compute.yml), which holds proj/ and rev/: the process's
//     AclGateCache over it, following {acl_}v on the host's Redis as tlsedge's does, and the RevocationChecker under
//     the svc- verifier, together the engine's ComputeAuthState;
//   - the block's own vault files for the areas that read their credentials there (HostDeps.SecretFile): the
//     providers' adapters (PROV), K_P's signer (GAMMA) and the mTLS client to searchsigner (SRCH), each by its name
//     alone, none read until its area asks;
//   - the engine's drain started at the block's SIGTERM, so a stream ends under the engine's own bound and not at the
//     status server's close;
//   - §17.2's abuse controls (CARCH-M12-01): ServiceHost.Abuse, §21.7's standing in memory over the block's Postgres
//     (model.NewAbuseGate), which every admission reads, and the region lookup, the process's GeoLite2 City reader, which
//     answers every admission's address from memory and keeps nothing of it (hostRegionLookup).
//
// The second Redis instance §14.4 gives {cmpa}, persistence off and outside every backup, is HostDeps.AdmissionsRedis:
// server's Redis pools read redis.yml alone, so a block builds none of its own yet, and every route that needs {cmpa}
// (SRCH's search admissions, M13b) answers 503 until one is given.
//
// Each credential the block reads is a HostDeps field a test fills with its own, so a host run in a test reads no
// vault file. RunHost sets no process-wide state: the main sets operator's global Env.
package computehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/glog"
	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/computeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "compute"

// The block's own files, each one of its secret_files under its vault root (warp's compute.yml). The design names
// none; these are the plan's (proposed).
const (
	// vault's file-clusters-<service>.yml: the first metadata cluster, read-only, for proj/ and rev/.
	HostFileClustersService = "compute"
	// The {cmpa} instance's credentials (§14.4), which server cannot open a second pool from yet.
	HostAdmissionsRedisFileName = "redis-compute-admissions.yml"
	// The provider adapters' credentials (PROV, §4.6).
	HostProvidersFileName = "compute-providers.yml"
	// K_P's signer key (GAMMA, §10.5).
	HostGammaSignerFileName = "compute-gamma-signer.yml"
	// The broker's mTLS client to searchsigner (SRCH, §21.5).
	HostSearchSignerClientFileName = "compute-searchsigner-client.yml"
)

// The files an area may read through HostDeps.SecretFile: the areas' own, never the block's other credentials.
var hostAreaSecretFileNames = []string{HostProvidersFileName, HostGammaSignerFileName, HostSearchSignerClientFileName}

// The block's tunables beyond the engine's.
type HostSettings struct {
	// The engine's, which carry the projection cache's and rev/'s settings too. nil:
	// computeservice.DefaultComputeEngineSettings.
	Compute *computeservice.ComputeEngineSettings
	// sidecarhost.Run's: readiness's bounds and the status server's drain. nil: sidecarhost.DefaultRunSettings.
	Run *sidecarhost.RunSettings
}

// The defaults.
func DefaultHostSettings() *HostSettings {
	return &HostSettings{
		Compute: computeservice.DefaultComputeEngineSettings(),
		Run:     sidecarhost.DefaultRunSettings(),
	}
}

// The clusters a block holds, as server.FileClusters is: the store and an end.
type HostClusters interface {
	sidecar.BlobStore
	// Ends the clusters' checks.
	Close()
}

// The seams a test, or the change that lands a missing one, fills. Each nil takes the block's own.
type HostDeps struct {
	// The newest verified state. nil: built over the block's gate cache and rev/ reader.
	AuthState computeservice.ComputeAuthState
	// The process's Env. nil: operator's global one, which the block's main sets before it serves.
	Env operator.Env
	// The {cmpa} instance. nil: none, as the package header says.
	AdmissionsRedis sidecar.Redis
	// The block's vault files by name, for the areas. nil: the block's own vault, refusing any name but the areas'.
	SecretFile func(name string) ([]byte, error)
	// Whether this binary links a group. nil: server's SidecarMgC1Linked to SidecarMgC3Linked.
	GroupLinked func(group computeservice.ComputeMigrationGroup) bool
	// The service log. nil: through glog, one line a request.
	ServiceLog computeservice.ComputeServiceLog
	// Where an admission's address is (§17.2). nil: the process's GeoLite2 City reader, which refuses every admission
	// where this process has no database (hostRegionLookup).
	RegionLookup computeservice.ComputeRegionLookup

	// RunHost's alone, each nil taking the block's own:
	//
	// The first metadata cluster: vault's file-clusters-compute.yml.
	Clusters HostClusters
	// The engine's clock: real time.
	Clock sidecar.Clock
	// §21.7's standing and network_verified from memory: model.NewAbuseGate over the block's Postgres.
	Abuse sidecar.AbuseGate
	// The svc- key set's public verifier, which the Authenticator and the rev/ reader verify under: vault's
	// oauth-jwks-svc.json, read once by the process.
	ServiceTokens *oauth.ServiceTokenVerifier
	// The block's /status latch, so a test reads it: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
	// A listener already bound for /status and the routes, which RunHost serves in place of binding its port: a test's
	// on 127.0.0.1:0. nil: the port, as warp maps it.
	StatusListener net.Listener
}

// settings with the providers' env the block's own, WARP_ENV's, where they name none (PROV-M12-01), so the engine's
// provider registry enables what the providers' settings name for this env; a copy, the caller's left as they were.
// With no WARP_ENV the env stays unnamed, which reads as production and enables no adapter.
func hostComputeSettings(settings *computeservice.ComputeEngineSettings) *computeservice.ComputeEngineSettings {
	if settings == nil {
		settings = computeservice.DefaultComputeEngineSettings()
	}
	copied := *settings
	providers := computeservice.DefaultComputeProviderSettings()
	if settings.Providers != nil {
		providersCopy := *settings.Providers
		providers = &providersCopy
	}
	if providers.Environment == "" {
		if env, err := server.Env(); err == nil {
			providers.Environment = env
		}
	}
	copied.Providers = providers
	return &copied
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type Host struct {
	engine *computeservice.ComputeEngine
	// what the engine honors no request without and the block was not given, each said once
	missing []string
	// what RunHost built for the engine, ended after it, in this order
	closeAfter []func()
	closeOnce  sync.Once
}

var _ sidecar.HttpEngine = (*Host)(nil)

// The engine on host, which needs Metrics. A deps' AuthState the host could not build, and a binary that does not
// link MG-C1 and MG-C2, leave the host not ready, never an error, so the block serves /status and says why. ctx bounds
// the engine and should outlive the drain.
func NewHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if host == nil || host.Metrics == nil {
		return nil, errors.New("compute host: no metrics")
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	if deps == nil {
		deps = &HostDeps{}
	}
	self := &Host{}
	if deps.AuthState == nil {
		self.missing = append(self.missing, "the newest verified authorization state, computeservice's ComputeAuthState over the block's AclGateCache (C-09)")
	}
	groupLinked := deps.GroupLinked
	if groupLinked == nil {
		groupLinked = serverGroupLinked
	}
	// the block's own groups, which sidecarhost.Run's readiness waits for too, said here so the host says why
	for _, group := range []computeservice.ComputeMigrationGroup{computeservice.ComputeMigrationGroupC1, computeservice.ComputeMigrationGroupC2} {
		if !groupLinked(group) {
			self.missing = append(self.missing, fmt.Sprintf("%s's tables, which this binary does not link (the sidecar_pending build, until M12's Gn-out registers the group)", group))
		}
	}
	secretFile := deps.SecretFile
	if secretFile == nil {
		secretFile = vaultSecretFile
	}
	var serviceLog computeservice.ComputeServiceLog
	if deps.ServiceLog != nil {
		serviceLog = deps.ServiceLog
	}
	regionLookup := deps.RegionLookup
	if regionLookup == nil {
		regionLookup = hostRegionLookup{}
	}
	engine, err := computeservice.NewComputeEngine(ctx, host, hostComputeSettings(settings.Compute), &computeservice.ComputeEngineDeps{
		AuthState:       deps.AuthState,
		Env:             deps.Env,
		AdmissionsRedis: deps.AdmissionsRedis,
		GroupLinked:     groupLinked,
		SecretFile:      secretFile,
		RegionLookup:    regionLookup,
		ServiceLog:      serviceLog,
		Log:             hostLogger{},
	})
	if err != nil {
		return nil, fmt.Errorf("compute host: %w", err)
	}
	self.engine = engine
	if 0 < len(self.missing) {
		glog.Errorf("[compute]not ready without %s\n", strings.Join(self.missing, "; "))
	}
	return self, nil
}

// Whether this binary links a compute group: server's markers.
func serverGroupLinked(group computeservice.ComputeMigrationGroup) bool {
	switch group {
	case computeservice.ComputeMigrationGroupC1:
		return server.SidecarMgC1Linked()
	case computeservice.ComputeMigrationGroupC2:
		return server.SidecarMgC2Linked()
	case computeservice.ComputeMigrationGroupC3:
		return server.SidecarMgC3Linked()
	}
	return false
}

// One of the areas' vault files, read from the block's vault: an error for a name that is no area's, so no area reads
// the block's other credentials through it, and for a file the vault does not hold. The error names the file and
// nothing it holds.
func vaultSecretFile(name string) ([]byte, error) {
	if !slices.Contains(hostAreaSecretFileNames, name) {
		return nil, fmt.Errorf("%s is no compute area's file", name)
	}
	resource, err := server.Vault.SimpleResource(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%s did not read", name)
	}
	return data, nil
}

// The engine, for a test that drives it.
func (self *Host) Engine() *computeservice.ComputeEngine {
	return self.engine
}

// What the block was not given that the engine honors no request without: empty once every seam is there.
func (self *Host) Missing() []string {
	return append([]string{}, self.missing...)
}

// Every seam given, MG-C1 and MG-C2 linked, and the engine serving. A closed gate leaves this alone: the engine answers
// 503 and the block stays ready (§19.11).
func (self *Host) Ready() bool {
	return len(self.missing) == 0 && self.engine.Ready()
}

// Every route, behind the engine's guard.
func (self *Host) Handler() http.Handler {
	return self.engine.Handler()
}

// The engine finishes what is in flight, within its own bound.
func (self *Host) Drain() {
	self.engine.Drain()
}

// Ends the engine, then what RunHost built for it. Safe after Drain, and more than once.
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

// Runs the block until ctx ends, through sidecarhost.Run (§19.4): GET /status on statusPort from the first moment,
// the engine's routes beside it once ready, then at ctx's end the engine's drain, the status server's, and Close. A
// block that cannot start logs why and serves /status not ready until ctx ends. settings and deps may be nil.
func RunHost(ctx context.Context, settings *HostSettings, deps *HostDeps, statusPort int) error {
	if deps == nil {
		deps = &HostDeps{}
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	engine := newBlockEngine(ctx, settings, deps)
	if computeHost, ok := engine.(*Host); ok {
		// the block's SIGTERM starts the engine's drain at once, so its streams end under the engine's bound while the
		// status server waits for them; sidecarhost.Run's later Drain finds it begun
		stop := context.AfterFunc(ctx, computeHost.Drain)
		defer stop()
	}
	options := blockRunOptions(engine, statusPort, deps)
	options.Settings = settings.Run
	return sidecarhost.Run(ctx, options)
}

// What sidecarhost.Run takes for engine: Postgres, and the groups its readiness waits for, MG-C1 and MG-C2.
func blockRunOptions(engine sidecar.Engine, statusPort int, deps *HostDeps) *sidecarhost.RunOptions {
	options := &sidecarhost.RunOptions{
		Service:        hostService,
		StatusPort:     statusPort,
		StatusListener: deps.StatusListener,
		Engine:         engine,
		WarpStatus:     deps.WarpStatus,
		Postgres:       true,
		Groups: []*sidecarhost.RunGroup{
			{Name: string(computeservice.ComputeMigrationGroupC1), Linked: server.SidecarMgC1Linked},
			{Name: string(computeservice.ComputeMigrationGroupC2), Linked: server.SidecarMgC2Linked},
		},
	}
	if httpEngine, ok := engine.(sidecar.HttpEngine); ok {
		options.Http = httpEngine
	}
	return options
}

// The block's engine on its own stores and deps', or a never-ready one when it cannot start, having said why.
// Everything is built on a context of its own that the engine's Close ends, so ctx's end starts the drain without
// ending what the drain needs.
func newBlockEngine(ctx context.Context, settings *HostSettings, deps *HostDeps) sidecar.Engine {
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
		glog.Errorf("[compute]not ready: %v\n", err)
		return notReadyEngine{}
	}
	computeSettings := settings.Compute
	if computeSettings == nil {
		computeSettings = computeservice.DefaultComputeEngineSettings()
	}
	clusters := deps.Clusters
	if clusters == nil {
		fileClusters, err := server.LoadFileClusters(hostCtx, HostFileClustersService, nil)
		if err != nil {
			// the cause may quote the file, which holds credentials: it is not passed on
			return notReady(fmt.Errorf("file-clusters-%s.yml did not load", HostFileClustersService))
		}
		clusters = fileClusters
	}
	closers = append(closers, clusters.Close)
	abuse := deps.Abuse
	if abuse == nil {
		// §21.7's standing, loaded from the block's Postgres off every admission's path, which reads it from memory
		// (CARCH-M12-01); a network not loaded yet answers 503 while the gate loads it
		abuseGate := model.NewAbuseGate(hostCtx, nil, nil)
		closers = append(closers, abuseGate.Close)
		abuse = abuseGate
	}
	host := sidecarhost.NewServiceHost(hostCtx, &sidecarhost.ServiceHostOptions{
		Postgres:      true,
		Blobs:         clusters,
		Clock:         deps.Clock,
		ServiceTokens: deps.ServiceTokens,
		Abuse:         abuse,
	})
	hostDeps := *deps
	if hostDeps.AuthState == nil {
		cache, closeCache, err := sidecarhost.WatchAclGate(hostCtx, host, nil, computeSettings.AclGate)
		if err != nil {
			return notReady(fmt.Errorf("the authorization cache: %w", err))
		}
		closers = append(closers, closeCache)
		revocationSettings := computeSettings.Revocation
		if revocationSettings == nil {
			revocationSettings = sidecar.DefaultRevocationSettings()
		}
		revocationSettingsCopy := *revocationSettings
		revocationSettingsCopy.Clock = host.Clock
		checker := sidecar.NewRevocationChecker(host.Blobs, sidecarhost.RevocationVerify(deps.ServiceTokens), &revocationSettingsCopy)
		hostDeps.AuthState = computeservice.NewComputeAuthState(cache, checker, host.Clock)
	}
	computeHost, err := NewHost(hostCtx, host, &HostSettings{Compute: computeSettings, Run: settings.Run}, &hostDeps)
	if err != nil {
		return notReady(err)
	}
	for i := len(closers) - 1; 0 <= i; i -= 1 {
		computeHost.closeAfter = append(computeHost.closeAfter, closers[i])
	}
	return computeHost
}

// The broker's region lookup (§17.2, CARCH-M12-01): the process's GeoLite2 City reader, the database every api lookup
// answers from, read from memory. An address it places nowhere is a zero region. A process with no database, and a
// lookup that fails, answer an error, by which the broker refuses every admission. Neither the region nor any error
// here carries the address, and nothing here logs (I-13).
type hostRegionLookup struct{}

func (hostRegionLookup) Region(addr netip.Addr) (region *computeservice.ComputeRegion, returnErr error) {
	if !server.IpInfoDatabaseDeployed() {
		return nil, errors.New("this compute block has no GeoLite2 City database")
	}
	defer func() {
		// a reader's panic text could quote what it was asked, so it is not kept
		if recover() != nil {
			region, returnErr = nil, errors.New("the geo database did not answer")
		}
	}()
	info, err := server.GetIpInfo(addr)
	if err != nil || info == nil {
		return nil, errors.New("the geo database did not answer")
	}
	return &computeservice.ComputeRegion{CountryCode: info.CountryCode, SubdivisionGeonameId: info.RegionGeonameId}, nil
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
