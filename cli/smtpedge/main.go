// server/cli/smtpedge (SMTP-M9-20; D-SMTP-11, R-SMTP-13, R-SMTP-14, D-SMTP-5, D-OPS-10, I-13, X7, §14.9, §19.4): the
// smtpedge engine on the two sending hosts, run by sidecarhost.Run with GET /status on port 80 and no other port:
// it listens as ur/smtpedge through its platform client in urnetwork-edge and dials MXs out of its host's sending
// addresses, so it has no lb route and no inbound port (exposed: false, user 65532, cap_net_admin for SO_MARK: the
// block's warp settings, OPS's).
//
// What the block builds around smtpedgeservice:
//
//   - its settings: config/<env>/smtpedge.yml through smtpedgeservice.LoadSmtpEdgeConfig, with the block's
//     WARP_FWMARK (server.RequireFwMark) for a sending address that names no mark, and the X-UR-Ref keys from the
//     vault secret file smtpedge-x-ur-ref.yml, read at run time and never from the repository;
//   - ServiceHost: sidecarhost's, with its Postgres pool (config/main/db.yml's 4), Quota over api's internal gate
//     and the AbuseGate on the pool (model.NewAbuseGate);
//   - the store: model.NewSmtpEdgeStore, the send log, halts, DKIM keys and the halt rule's reads, never on a
//     preamble's path (I-12);
//   - the gate: gate.go's, from memory filled off the path;
//   - the clients: sidecar.NewEdgeClientPool, one platform client in urnetwork-edge (§14.9), its credential from
//     server.NewPlatformClientSource on the edge bearer; once it exists the engine serves ur/smtpedge on it,
//     registering at sockets on the client's sockets token, the service token of audience sockets api's edge
//     client route answers beside its jwt (QD-1054);
//   - the dialer: smtpedgeservice.SmtpMxNetDialer, SO_MARK per sending address.
//
// config/<env>/sidecar.yml's smtpedge gate is reread every 60 s through sidecarhost's Gates, and the engine
// answers a preamble out of the gate TRY_LATER while the block stays ready: it stays off until D-SMTP-16 passes
// (R-SMTP-13).
//
// The dialing sidecar, the subject mail_send is judged for, needs no seam here: each stream's admitted ur/ ticket
// names it in dialer_sidecar_id, which sockets filled at Find from the caller's token and signed (QD-989).
//
// What this draft does not have, each leaving the block not ready and saying so: a DNSSEC-validating MX resolver
// and a PTR resolver for FCrDNS (none is built for a server host yet) and the proof verifier
// (smtpproof.NewVerifier over a pinned KeySet: no release pins one, SMTP-M9-27, SMTP-M9-40). The gate's mail_send
// and breaker rows answer not loaded (gate.go).
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "smtpedge"

// The block's file under config/<env>, and its X-UR-Ref key file among its vault secret_files.
const (
	hostSettingsResourceName = "smtpedge.yml"
	hostXUrRefResourceName   = "smtpedge-x-ur-ref.yml"
)

// The block's tunables beyond the engine's.
type hostSettings struct {
	// The engine's, from smtpedge.yml and the key file.
	Config *smtpedgeservice.SmtpEdgeConfig
	// The pool's: one client (§14.9).
	Clients *sidecar.EdgeClientPoolSettings
	Gate    *hostGateSettings
	// How long the block waits before it makes its client again after the source could not answer: 5 s.
	ClientsRetryInterval time.Duration
}

func defaultHostSettings(config *smtpedgeservice.SmtpEdgeConfig) *hostSettings {
	clients := sidecar.DefaultEdgeClientPoolSettings()
	clients.ClientCount = 1
	return &hostSettings{
		Config:               config,
		Clients:              clients,
		Gate:                 defaultHostGateSettings(),
		ClientsRetryInterval: 5 * time.Second,
	}
}

// The block's client, as sidecar.EdgeClientPool is.
type hostClients interface {
	Clients() []*sidecar.SocketEdgeClient
	ClientIds() []sidecar.Id
	Ready() bool
	Close()
}

// The seams a test, or the change that lands a missing one, fills. Each nil one leaves the block not ready and
// is said once.
type hostDeps struct {
	Quota    sidecar.Quota
	Source   sidecar.ServiceClientSource
	Abuse    sidecar.AbuseGate
	Store    *model.SmtpEdgeStore
	Gate     smtpedgeservice.SmtpGate
	Resolver sidecar.SmtpMxResolver
	Ptr      smtpedgeservice.SmtpPtrResolver
	Dialer   sidecar.SmtpMxDialer
	Verifier sidecar.SmtpProofVerifier
	// Makes the block's client. Called again until it answers.
	NewClients func(ctx context.Context) (hostClients, error)
	// How the client's ur/smtpedge registration reaches sockets.
	Registrations *sidecar.SocketPlatformRegistrationsArgs
	// Each replaces the block's own: a test's.
	Gates sidecar.ServiceGates
	Redis sidecar.Redis
	Clock sidecar.Clock
	Log   sidecar.Logger
}

// The engine as the block runs it. Safe for concurrent use.
type smtpEdgeHost struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     *sidecar.ServiceHost
	settings *hostSettings
	deps     *hostDeps
	engine   *smtpedgeservice.SmtpEdgeService
	log      sidecar.Logger
	// what the engine sends no mail without and the block was not given
	missing []string
	group   sync.WaitGroup

	stateLock sync.Mutex
	clients   hostClients
	serving   bool
	closed    bool
}

var _ sidecar.Engine = (*smtpEdgeHost)(nil)

// The engine on its host, its client made in the background on a context of the block's own, so ctx's end, a
// SIGTERM, starts the drain without ending what the drain needs. A missing seam leaves the block not ready,
// never an error.
func newSmtpEdgeHost(ctx context.Context, settings *hostSettings, deps *hostDeps) (*smtpEdgeHost, error) {
	if settings == nil || settings.Config == nil || settings.Clients == nil || settings.Gate == nil || settings.ClientsRetryInterval <= 0 {
		return nil, errors.New("no smtpedge settings")
	}
	if deps == nil {
		deps = &hostDeps{}
	}
	log := deps.Log
	if log == nil {
		log = hostLogger{}
	}
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	host := sidecarhost.NewServiceHost(hostCtx, &sidecarhost.ServiceHostOptions{
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
	self := &smtpEdgeHost{
		ctx:      hostCtx,
		cancel:   cancel,
		host:     host,
		settings: settings,
		deps:     deps,
		log:      log,
	}
	need := func(present bool, what string) {
		if !present {
			self.missing = append(self.missing, what)
		}
	}
	need(deps.Quota != nil, "ServiceHost.Quota, api's internal quota surface (QD-055)")
	need(deps.Store != nil, "the store on the block's Postgres pool")
	need(deps.Gate != nil, "the send gate")
	need(deps.Resolver != nil, "a DNSSEC-validating MX resolver")
	need(deps.Ptr != nil, "a PTR resolver for the sending addresses' FCrDNS")
	need(deps.Dialer != nil, "the MX dialer")
	need(deps.Verifier != nil, "the proof verifier (smtpproof.NewVerifier over a pinned KeySet)")
	need(deps.NewClients != nil && deps.Registrations != nil, "the platform client, api's edge client route on the edge bearer (QD-812)")
	var store smtpedgeservice.SmtpEdgeStore
	engineDeps := &smtpedgeservice.SmtpEdgeDeps{
		Gate:     deps.Gate,
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

// Makes the client again each ClientsRetryInterval until it exists, then serves ur/smtpedge on it until the
// block closes.
func (self *smtpEdgeHost) runClients() {
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
func (self *smtpEdgeHost) serve(clients hostClients) {
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
		Clock:         self.deps.Clock,
		Log:           self.log,
	})
	if err != nil {
		self.log.Errorf("[%s]ur/smtpedge: %v", hostService, err)
	}
}

// What the block was not given that the engine sends no mail without: empty once every seam is there.
func (self *smtpEdgeHost) Missing() []string {
	return slices.Clone(self.missing)
}

// §19.4's smtpedge column: every seam given, the engine ready (smtp_* tables reachable, {smq_} on Redis, a
// sending address bound with its FCrDNS), and ur/smtpedge served on a connected client.
func (self *smtpEdgeHost) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	clients, serving, closed := func() (hostClients, bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.clients, self.serving, self.closed
	}()
	return !closed && serving && clients != nil && clients.Ready() && self.engine.Ready()
}

// The engine takes no new stream and its connections finish, up to its DrainTimeout.
func (self *smtpEdgeHost) Drain() {
	self.engine.Drain()
}

// Ends the engine, the block's loop and its client. Safe after Drain, and more than once.
func (self *smtpEdgeHost) Close() {
	clients, closed := func() (hostClients, bool) {
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
	if clients != nil {
		clients.Close()
	}
}

// The X-UR-Ref key file: the keys by id, base64, and the one new references are issued under.
type hostXUrRefFile struct {
	IssueKeyId int `yaml:"issue_key_id"`
	Keys       []struct {
		Id  int    `yaml:"id"`
		Key string `yaml:"key"`
	} `yaml:"keys"`
}

// Lays the key file in data over settings. A refusal names the part, never a key.
func applyXUrRefKeys(settings *smtpedgeservice.SmtpEdgeSettings, data []byte) error {
	file := &hostXUrRefFile{}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(file); err != nil {
		return fmt.Errorf("%s does not parse", hostXUrRefResourceName)
	}
	keys := []*smtpedgeservice.SmtpXUrRefKey{}
	for _, entry := range file.Keys {
		if entry.Id < 1 || 255 < entry.Id {
			return fmt.Errorf("%s holds a key id past one octet", hostXUrRefResourceName)
		}
		key, err := base64.StdEncoding.DecodeString(entry.Key)
		if err != nil {
			return fmt.Errorf("%s holds a key that is not base64", hostXUrRefResourceName)
		}
		keys = append(keys, &smtpedgeservice.SmtpXUrRefKey{KeyId: uint8(entry.Id), Key: key})
	}
	settings.XUrRefKeys = keys
	settings.XUrRefIssueKeyId = file.IssueKeyId
	return settings.Validate()
}

// smtpedge.yml from config, with the X-UR-Ref keys from the vault.
func loadSmtpEdgeConfig() (*smtpedgeservice.SmtpEdgeConfig, error) {
	resource, err := server.Config.SimpleResource(hostSettingsResourceName)
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	config, err := smtpedgeservice.LoadSmtpEdgeConfig(data, server.RequireFwMark())
	if err != nil {
		return nil, err
	}
	keyResource, err := server.Vault.SimpleResource(hostXUrRefResourceName)
	if err != nil {
		return nil, fmt.Errorf("no %s", hostXUrRefResourceName)
	}
	keyData, err := keyResource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("no %s", hostXUrRefResourceName)
	}
	if err := applyXUrRefKeys(config.Settings, keyData); err != nil {
		return nil, err
	}
	return config, nil
}

// The pool as the block's client.
type poolClients struct {
	*sidecar.EdgeClientPool
}

// The block's own deps: Quota over api's internal gate, the store and the AbuseGate on the block's Postgres
// pool, the gate, the dialer, and the client with its registrations at socketsUrl. Each is left out, and said,
// when what it needs is missing. The returned call ends what it started.
func blockDeps(ctx context.Context, settings *hostSettings, apiUrl string, socketsUrl string) (*hostDeps, func()) {
	deps := &hostDeps{
		Store:  model.NewSmtpEdgeStore(nil),
		Dialer: smtpedgeservice.NewSmtpMxNetDialer(settings.Config.Settings),
		Abuse:  model.NewAbuseGate(ctx, model.NewAbuseHardDenySnapshot(ctx, nil), nil),
	}
	ends := []func(){}
	end := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(ctx, server.InternalGateCallerSmtpedge, apiUrl, nil); err != nil {
		glog.Errorf("[%s]no ServiceHost.Quota: %v\n", hostService, err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	gate := newHostGate(ctx, &modelGateSource{root: settings.Config.Settings.Root, quota: deps.Quota}, sidecar.RealClock{}, settings.Gate)
	deps.Gate = gate
	ends = append(ends, gate.wait)
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerEdge)
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	blockHost, err := server.Host()
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	sourceSettings := server.DefaultPlatformClientSourceSettings()
	sourceSettings.ClientCount = settings.Clients.ClientCount
	// the deploy slot warp hands the process (QD-873, QD-1011), one no other running smtpedge process on the host
	// holds, so a deploy's new process holds other clients than the old one draining beside it
	slot, err := server.LoadEdgeClientSlot(sourceSettings)
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	source, err := server.NewPlatformClientSource(ctx, &server.PlatformClientSourceArgs{
		ApiUrl:   apiUrl,
		Block:    hostService,
		Host:     blockHost,
		Slot:     slot,
		Bearer:   bearer,
		Settings: sourceSettings,
	})
	if err != nil {
		glog.Errorf("[%s]no platform client: %v\n", hostService, err)
		return deps, end
	}
	ends = append(ends, source.Close)
	if socketsUrl == "" {
		glog.Errorf("[%s]no platform client: no sockets url for its registration\n", hostService)
		return deps, end
	}
	deps.Source = source
	deps.NewClients = func(ctx context.Context) (hostClients, error) {
		made, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
			Source:   source,
			Settings: settings.Clients,
			Log:      hostLogger{},
		})
		if err != nil {
			return nil, err
		}
		return poolClients{EdgeClientPool: made}, nil
	}
	// a registration is the client's own call, on its own sockets token, the service token of audience sockets
	// api's edge client route answers beside its jwt (QD-1054); a client the source holds no answer for has none
	deps.Registrations = source.SocketPlatformRegistrationsArgs(socketsUrl)
	return deps, end
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

// Runs the block until ctx ends through sidecarhost.Run (§19.4): GET /status from the first moment, then at
// ctx's end the drain and Close. A block that cannot start logs why and serves /status not ready.
func run(ctx context.Context, port int, settings *hostSettings, deps *hostDeps) error {
	if settings != nil {
		host, err := newSmtpEdgeHost(ctx, settings, deps)
		if err == nil {
			return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
				Service:    hostService,
				StatusPort: port,
				Engine:     host,
				Postgres:   true,
				Groups:     hostGroups(),
			})
		}
		glog.Errorf("[%s]not ready: %v\n", hostService, err)
	}
	return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
		Service:    hostService,
		StatusPort: port,
		Engine:     notReadyEngine{},
	})
}

var (
	port       = flag.Int("port", 80, "the block's status port")
	apiUrl     = flag.String("api_url", "", "api's base URL, whose internal gate the quota surface and the edge client route are read from; empty leaves the block not ready")
	socketsUrl = flag.String("sockets_url", "", "the sockets service's base URL, which ur/smtpedge registers with; empty leaves the block not ready")
)

func main() {
	// before anything can log (server.ScrubCommandLogs)
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logs.Exit(0)
		}
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()

	var settings *hostSettings
	var deps *hostDeps
	config, err := loadSmtpEdgeConfig()
	if err != nil {
		// a refused file names the key, never a value, and the key file never a key
		glog.Errorf("[%s]%s: %v\n", hostService, hostSettingsResourceName, err)
	} else {
		settings = defaultHostSettings(config)
		// on a context of its own, so a SIGTERM's drain can still send the open usage window
		depsCtx, depsCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer depsCancel()
		var end func()
		deps, end = blockDeps(depsCtx, settings, *apiUrl, *socketsUrl)
		defer end()
		defer depsCancel()
	}
	if err := run(ctx, *port, settings, deps); err != nil {
		panic(err)
	}
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
