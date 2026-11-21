// Package smtpedgehost is server/cli/smtpedge's host (SMTP-M9-20; D-SMTP-11, R-SMTP-13, R-SMTP-14, D-SMTP-5, D-OPS-10,
// I-13, X7, §14.9, §19.4): sidecar's smtpedgeservice engine on the two sending hosts, run by sidecarhost.Run with GET
// /status on port 80 and no other port, and what a T3 or e2e test starts the engine through. It listens as
// ur/smtpedge through its platform client in urnetwork-edge and dials MXs out of its host's sending addresses, so it
// has no lb route and no inbound port (exposed: false, user 65532, cap_net_admin for SO_MARK: the block's warp
// settings, OPS's). server/cli/smtpedge reads smtpedge.yml, the X-UR-Ref keys and each credential into HostSettings
// and HostDeps; a test fills them with its own, so no credential of a host run in a test comes from vault.
//
// What the block builds around smtpedgeservice:
//
//   - its settings: config/<env>/smtpedge.yml through smtpedgeservice.LoadSmtpEdgeConfig, with the block's
//     WARP_FWMARK (server.RequireFwMark) for a sending address that names no mark, and the X-UR-Ref keys from the
//     vault secret file smtpedge-x-ur-ref.yml, read at run time and never from the repository, into the engine's
//     settings (HostSettings.Config.Settings, where a test sets keys of its own);
//   - ServiceHost: sidecarhost's, with its Postgres pool (config/main/db.yml's 4), Quota over api's internal gate
//     and the AbuseGate on the pool (model.NewAbuseGate);
//   - the store: model.NewSmtpEdgeStore, the send log, halts, DKIM keys and the halt rule's reads, never on a
//     preamble's path (I-12);
//   - the gate: gate.go's, from memory filled off the path;
//   - the clients: sidecar.NewEdgeClientPool, one platform client in urnetwork-edge (§14.9), its credential from
//     server.NewPlatformClientSource on the edge bearer; once it exists the engine serves ur/smtpedge on it,
//     registering at sockets on the client's sockets token, the service token of audience sockets api's edge
//     client route answers beside its jwt (QD-1054);
//   - the dialer: smtpedgeservice.SmtpMxNetDialer, SO_MARK per sending address;
//   - the proof verifier: smtpproof.NewVerifier over smtpedge.yml's proof_key_sets (OPS-M9-02; DESIGN-DETAILS
//     3.2.7; NewProofVerifier), each set's directory opened against its id, a set past its not_after dropped at
//     start. A file that lists none gives a verifier that holds none, so every preamble answers KEYS; a listed set
//     that does not open leaves the block with no verifier and not ready.
//
// config/<env>/sidecar.yml's smtpedge gate is reread every 60 s through sidecarhost's Gates, and the engine
// answers a preamble out of the gate TRY_LATER while the block stays ready: it stays off until D-SMTP-16 passes
// (R-SMTP-13).
//
// The dialing sidecar, the subject mail_send is judged for, needs no seam here: each stream's admitted ur/ ticket
// names it in dialer_sidecar_id, which sockets filled at Find from the caller's token and signed (QD-989).
//
// A verifier without configured proof keys accepts no preambles.
//
// What stays the process's when several hosts run in one test process (e2e-hosts-in-process): the Redis key-event
// subscriber and the metrics registry, which sidecarhost shares by design; server's Postgres pools, which the store,
// the gate's facts and the abuse gate read; sidecar.yml, which server reads once per process
// (server.Testing_PushSidecarConfig sets it for every block at once, and HostDeps.Gates gives one block its own);
// model.Pro()'s tiers, which the gate's Community limits read; and the test hooks (sidecartest.InstallTestHooks).
// Run sets no process-wide state of its own: the main's RunOptions.ProcessEnv sets operator's global Env, and a test
// leaves it nil.
package smtpedgehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"
	"github.com/urnetwork/sidecar/smtpproof"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "smtpedge"

// The block's file under config/<env>. Its X-UR-Ref key file among its vault secret_files is
// model.SmtpXUrRefKeyFileName, which mxedge's platform listener reads too.
const HostSettingsResourceName = "smtpedge.yml"

// The block's tunables beyond the engine's.
type HostSettings struct {
	// The engine's, from smtpedge.yml and the key file.
	Config *smtpedgeservice.SmtpEdgeConfig
	// The directory smtpedge.yml sits in, which a proof key set's relative directory is under. Empty when the
	// file has none, and then only an absolute directory opens.
	ConfigHome string
	// The pool's: one client (§14.9).
	Clients *sidecar.EdgeClientPoolSettings
	Gate    *HostGateSettings
	// How long the block waits before it makes its client again after the source could not answer: 5 s.
	ClientsRetryInterval time.Duration
	// sidecarhost.Run's: readiness's bounds and the status server's drain. nil: sidecarhost.DefaultRunSettings.
	Run *sidecarhost.RunSettings
}

// The defaults over the engine's config.
func DefaultHostSettings(config *smtpedgeservice.SmtpEdgeConfig) *HostSettings {
	clients := sidecar.DefaultEdgeClientPoolSettings()
	clients.ClientCount = 1
	return &HostSettings{
		Config:               config,
		Clients:              clients,
		Gate:                 DefaultHostGateSettings(),
		ClientsRetryInterval: 5 * time.Second,
		Run:                  sidecarhost.DefaultRunSettings(),
	}
}

// The block's client, as sidecar.EdgeClientPool is.
type HostClients interface {
	Clients() []*sidecar.SocketEdgeClient
	ClientIds() []sidecar.Id
	Ready() bool
	Close()
}

// The durable send records and reporting queries the executable takes from its Postgres adapter.
type HostSmtpStore interface {
	smtpedgeservice.SmtpEdgeStore
	smtpedgeservice.SmtpReportLookups
	smtpedgeservice.SmtpHaltLookups
}

var _ HostSmtpStore = (*model.SmtpEdgeStore)(nil)

// The seams a test, or the change that lands a missing one, fills, and what a block's main builds from vault's
// credentials, which a test builds from its own. Each nil one leaves the block not ready and is said once.
type HostDeps struct {
	// The block's ServiceHost, NewBlockHost's and Run's alone (NewHost takes a host built):
	//
	// ServiceHost.Quota: quotahost.NewBlockQuota over api's internal gate on internal-gate-smtpedge.yml.
	Quota sidecar.Quota
	// ServiceHost.ServiceClient: server.NewPlatformClientSource on internal-gate-edge.yml's bearer.
	Source sidecar.ServiceClientSource
	// ServiceHost.Abuse: model.NewAbuseGate on the block's Postgres pool.
	Abuse sidecar.AbuseGate
	// Each replaces the block's own: a test's.
	Gates   sidecar.ServiceGates
	Redis   sidecar.Redis
	Metrics sidecar.Metrics
	Clock   sidecar.Clock

	// The engine's, NewHost's:
	Store HostSmtpStore
	Gate  smtpedgeservice.SmtpGate
	// The stock gate verifies the signed projection retrieved with the block's own bearer,
	// internal-gate-smtpedge.yml's.
	Projection sidecar.AclGateSource
	Facts      HostGateSource
	Resolver   sidecar.SmtpMxResolver
	Ptr        smtpedgeservice.SmtpPtrResolver
	Dialer     sidecar.SmtpMxDialer
	// NewProofVerifier's over smtpedge.yml's proof_key_sets.
	Verifier sidecar.SmtpProofVerifier
	// Makes the block's client. Called again until it answers.
	NewClients func(ctx context.Context) (HostClients, error)
	// How the client's ur/smtpedge registration reaches sockets.
	Registrations *sidecar.SocketPlatformRegistrationsArgs
	Log           sidecar.Logger
}

// The engine as the block runs it. Safe for concurrent use.
type Host struct {
	ctx       context.Context
	cancel    context.CancelFunc
	host      *sidecar.ServiceHost
	settings  *HostSettings
	deps      *HostDeps
	engine    *smtpedgeservice.SmtpEdgeService
	log       sidecar.Logger
	closeGate func()
	// what the engine sends no mail without and the block was not given
	missing []string
	group   sync.WaitGroup
	// what NewBlockHost built for the host, ended after it
	closeAfter []func()

	stateLock sync.Mutex
	clients   HostClients
	serving   bool
	closed    bool
}

var _ sidecar.Engine = (*Host)(nil)

// The engine on host, its client made in the background on a context of the block's own, so ctx's end, a SIGTERM,
// starts the drain without ending what the drain needs. host is the block's ServiceHost, NewBlockHost's from deps
// for a block: it needs Redis, Metrics and Gates, its Quota is the quota surface and its Abuse the standing the send
// gate reads. A missing seam leaves the block not ready, never an error.
func NewHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if host == nil {
		return nil, errors.New("no smtpedge ServiceHost")
	}
	if settings == nil || settings.Config == nil || settings.Clients == nil || settings.Gate == nil || settings.ClientsRetryInterval <= 0 {
		return nil, errors.New("no smtpedge settings")
	}
	if deps == nil {
		deps = &HostDeps{}
	}
	log := deps.Log
	if log == nil {
		log = hostLogger{}
	}
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	self := &Host{
		ctx:       hostCtx,
		cancel:    cancel,
		host:      host,
		settings:  settings,
		deps:      deps,
		log:       log,
		closeGate: func() {},
	}
	gate := deps.Gate
	if gate == nil && deps.Projection != nil && deps.Facts != nil {
		cache, closeCache, err := sidecarhost.WatchAclGate(hostCtx, host, deps.Projection, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		hostGate := newHostGate(hostCtx, deps.Facts, host.ClockOrReal(), settings.Gate)
		hostGate.mail = sidecar.NewAclGateView(cache, host.Abuse, nil)
		gate = hostGate
		self.closeGate = func() {
			hostGate.wait()
			closeCache()
		}
	}
	need := func(present bool, what string) {
		if !present {
			self.missing = append(self.missing, what)
		}
	}
	need(host.Quota != nil, "ServiceHost.Quota, api's internal quota surface (QD-055)")
	need(deps.Store != nil, "the store on the block's Postgres pool")
	need(gate != nil, "the send gate")
	need(deps.Resolver != nil, "a DNSSEC-validating MX resolver")
	need(deps.Ptr != nil, "a PTR resolver for the sending addresses' FCrDNS")
	need(deps.Dialer != nil, "the MX dialer")
	need(deps.Verifier != nil, "the proof verifier (smtpproof.NewVerifier over smtpedge.yml's proof_key_sets)")
	need(deps.NewClients != nil && deps.Registrations != nil, "the platform client, api's edge client route on the edge bearer (QD-812)")
	var store smtpedgeservice.SmtpEdgeStore
	engineDeps := &smtpedgeservice.SmtpEdgeDeps{
		Gate:     gate,
		Resolver: deps.Resolver,
		Dialer:   deps.Dialer,
		Ptr:      deps.Ptr,
		Sink:     model.NewAbuseSmtpSink(nil),
	}
	if deps.Store != nil {
		store = deps.Store
		engineDeps.Lookups = deps.Store
		engineDeps.Halts = deps.Store
	}
	engine, err := smtpedgeservice.NewSmtpEdgeServiceWithDeps(hostCtx, host, store, deps.Verifier, engineDeps, settings.Config.Settings)
	if err != nil {
		cancel()
		self.closeGate()
		return nil, err
	}
	self.engine = engine
	if 0 < len(self.missing) {
		log.Errorf("[%s]not ready without %s", hostService, strings.Join(self.missing, "; "))
	}
	if deps.NewClients != nil && deps.Registrations != nil {
		self.group.Add(1)
		go self.runClients()
	}
	return self, nil
}

// The block's ServiceHost from deps on ctx, which bounds its gates' reads: sidecarhost's with its Postgres pool and
// deps' Quota, Source, Abuse and Clock, and deps' Gates, Redis and Metrics in place of the block's when given.
func newBlockServiceHost(ctx context.Context, deps *HostDeps) *sidecar.ServiceHost {
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Postgres:      true,
		Quota:         deps.Quota,
		ServiceClient: deps.Source,
		Abuse:         deps.Abuse,
		Clock:         deps.Clock,
	})
	if deps.Gates != nil {
		host.Gates = deps.Gates
	}
	if deps.Redis != nil {
		host.Redis = deps.Redis
	}
	if deps.Metrics != nil {
		host.Metrics = deps.Metrics
	}
	return host
}

// NewHost on the block's own ServiceHost, built from deps as a block's is, on a context of its own that Close
// ends.
func NewBlockHost(ctx context.Context, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if deps == nil {
		deps = &HostDeps{}
	}
	blockCtx, blockCancel := context.WithCancel(context.WithoutCancel(ctx))
	self, err := NewHost(blockCtx, newBlockServiceHost(blockCtx, deps), settings, deps)
	if err != nil {
		blockCancel()
		return nil, err
	}
	self.closeAfter = append(self.closeAfter, blockCancel)
	return self, nil
}

// The block's ServiceHost, which the engine runs on and a main sets the process's Env over.
func (self *Host) ServiceHost() *sidecar.ServiceHost {
	return self.host
}

// The engine, for a test that drives it.
func (self *Host) Engine() *smtpedgeservice.SmtpEdgeService {
	return self.engine
}

// Makes the client again each ClientsRetryInterval until it exists, then serves ur/smtpedge on it until the
// block closes.
func (self *Host) runClients() {
	defer self.group.Done()
	for {
		clients, err := self.deps.NewClients(self.ctx)
		if err == nil {
			kept := func() bool {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.closed {
					return false
				}
				self.clients = clients
				return true
			}()
			if !kept {
				clients.Close()
				return
			}
			self.serve(clients)
			return
		}
		if self.ctx.Err() != nil {
			return
		}
		// the pool's and the source's errors name an index, a part or a status, never a secret or an address
		self.log.Warningf("[%s]no platform client yet: %v", hostService, err)
		select {
		case <-self.ctx.Done():
			return
		case <-self.host.ClockOrReal().After(self.settings.ClientsRetryInterval):
		}
	}
}

// Serves ur/smtpedge on clients until the engine closes: urnetwork-edge's id comes from sidecar.yml.
func (self *Host) serve(clients HostClients) {
	edge, ok := self.host.GatesOrClosed().UrnetworkEdge()
	if !ok {
		self.log.Errorf("[%s]sidecar.yml names no urnetwork_edge: ur/smtpedge is not served", hostService)
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.serving = true
	}()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.serving = false
	}()
	err := self.engine.Serve(&sidecar.SocketPlatformListenerArgs{
		UrnetworkEdge: sidecar.Id(edge),
		Clients:       clients.Clients(),
		Registrations: self.deps.Registrations,
		Clock:         self.host.Clock,
		Log:           self.log,
	})
	if err != nil {
		self.log.Errorf("[%s]ur/smtpedge: %v", hostService, err)
	}
}

// What the block was not given that the engine sends no mail without: empty once every seam is there.
func (self *Host) Missing() []string {
	return slices.Clone(self.missing)
}

// §19.4's smtpedge column: every seam given, the engine ready (smtp_* tables reachable, {smq_} on Redis, a
// sending address bound with its FCrDNS), and ur/smtpedge served on a connected client.
func (self *Host) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	clients, serving, closed := func() (HostClients, bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.clients, self.serving, self.closed
	}()
	return !closed && serving && clients != nil && clients.Ready() && self.engine.Ready()
}

// The engine takes no new stream and its connections finish, up to its DrainTimeout.
func (self *Host) Drain() {
	self.engine.Drain()
}

// Ends the engine, the block's loop and its client, then what NewBlockHost built. Safe after Drain, and more than
// once.
func (self *Host) Close() {
	clients, closed := func() (HostClients, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil, true
		}
		self.closed = true
		clients := self.clients
		self.clients = nil
		return clients, false
	}()
	if closed {
		return
	}
	self.engine.Close()
	self.cancel()
	self.group.Wait()
	self.closeGate()
	if clients != nil {
		clients.Close()
	}
	for _, close := range self.closeAfter {
		close()
	}
	self.closeAfter = nil
}

// The verifier over smtpedge.yml's proof_key_sets (DESIGN-DETAILS 3.2.7): each set the file lists that is not
// past its not_after at now, its directory checked against its id and opened, its verifying keys checked
// against its keyset.json. A session verifies under the set its preamble names, and a preamble that names any
// other set, an expired one included, answers KEYS. A file that lists none gives a verifier that holds none.
// A listed set that does not open is an error and no verifier: the block then stays not ready, since a
// verifier holding fewer sets than the file lists would answer KEYS to hosts the operator meant to accept.
// The error names the entry by its place and the id it was listed under, which is public, and never a path.
func NewProofVerifier(config *smtpedgeservice.SmtpEdgeConfig, configHome string, now time.Time) (*smtpproof.Verifier, error) {
	verifierSettings := smtpproof.DefaultVerifierSettings()
	verifierSettings.KeySetMaxCount = config.Settings.ProofKeySetMaxCount
	keySets := []*smtpproof.VerifierKeySet{}
	for _, proofKeySet := range config.ProofKeySetsAt(now) {
		directory, err := config.ProofKeySetDirectory(proofKeySet, configHome)
		if err != nil {
			return nil, err
		}
		keySet, err := smtpproof.OpenKeySet(directory, proofKeySet.Id)
		if err != nil {
			return nil, fmt.Errorf("proof key set %x does not open", proofKeySet.Id)
		}
		keySets = append(keySets, &smtpproof.VerifierKeySet{KeySet: keySet, NotAfter: proofKeySet.NotAfter})
	}
	verifier, err := smtpproof.NewVerifier(verifierSettings, keySets)
	if err != nil {
		return nil, errors.New("the proof key sets' verifying keys do not load")
	}
	return verifier, nil
}

// The block's platform client: SMTP receives ticketed opens and the sender's protocol messages on each one.
func NewEdgeClients(ctx context.Context, args sidecar.EdgeClientPoolArgs) (HostClients, error) {
	args.Inbound = smtpedgeservice.Inbound
	pool, err := sidecar.NewEdgeClientPool(ctx, &args)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// A block that could not start: never ready, serving nothing but /status.
type notReadyEngine struct{}

func (notReadyEngine) Ready() bool { return false }
func (notReadyEngine) Drain()      {}
func (notReadyEngine) Close()      {}

// The pending groups the block reads: MG5's complaints (the halt rule's counts) and MG9's sends, halts and keys.
func hostGroups() []*sidecarhost.RunGroup {
	return []*sidecarhost.RunGroup{
		{Name: "MG5", Linked: server.SidecarMg5Linked},
		{Name: "MG9", Linked: server.SidecarMg9Linked},
	}
}

// What a block's main, or a test, gives Run.
type RunOptions struct {
	// GET /status's port: 80 in a block.
	Port int
	// A listener already bound for /status, which Run serves in place of binding Port: a test's on 127.0.0.1:0, so
	// the OS picks the port. Run takes it. nil: Port, as warp maps it.
	StatusListener net.Listener
	// nil: the block could not read its smtpedge.yml, and serves /status not ready.
	Settings *HostSettings
	// nil: no seam, so never ready. A block's main fills them from vault.
	Deps *HostDeps
	// The block's /status latch, so a test reads it. nil: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
	// Called once before the block serves with the engine's ServiceHost, or an empty one for a block that could not
	// start: a block's main sets the process's Env here (QD-1027). nil sets nothing, as a test running several
	// blocks in one process leaves it.
	ProcessEnv func(host *sidecar.ServiceHost)
}

// Runs the block until ctx ends through sidecarhost.Run (§19.4): GET /status from the first moment, then at
// ctx's end the drain and Close. A block that cannot start logs why and serves /status not ready.
func Run(ctx context.Context, options *RunOptions) error {
	if options.Settings != nil {
		host, err := NewBlockHost(ctx, options.Settings, options.Deps)
		if err == nil {
			if options.ProcessEnv != nil {
				options.ProcessEnv(host.host)
			}
			return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
				Service:        hostService,
				StatusPort:     options.Port,
				StatusListener: options.StatusListener,
				Engine:         host,
				Postgres:       true,
				Groups:         hostGroups(),
				WarpStatus:     options.WarpStatus,
				Settings:       options.Settings.Run,
			})
		}
		glog.Errorf("[%s]not ready: %v\n", hostService, err)
	}
	if options.ProcessEnv != nil {
		options.ProcessEnv(&sidecar.ServiceHost{})
	}
	var runSettings *sidecarhost.RunSettings
	if options.Settings != nil {
		runSettings = options.Settings.Run
	}
	return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
		Service:        hostService,
		StatusPort:     options.Port,
		StatusListener: options.StatusListener,
		Engine:         notReadyEngine{},
		WarpStatus:     options.WarpStatus,
		Settings:       runSettings,
	})
}

// glog as a sidecar.Logger, for what the block's main builds around the engine.
func HostLogger() sidecar.Logger {
	return hostLogger{}
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
