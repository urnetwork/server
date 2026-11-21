// Package socketshost is server/cli/sockets's host (SOCK-M5-08; D-SOCK-10, D-SOCK-14, R-SOCK-6, I-12, I-13, §9.8,
// §19.4): sidecar's socketsservice engine on server's stores, run as one warp block at sockets.<root> by
// sidecarhost.Run, and what a T3 or e2e test starts the engine through. A block runs on every lb host, since each lb
// proxies only to its own blocks (D-SOCK-10), with GET /status and §9.15's /socket/* routes on port 80, {vault: no,
// config: yes, site: no}, user 65532 and no Postgres: everything the engine keeps is soft state in Redis Cluster
// (§9.8), and api is reached only off the request paths, for the projection and the over-limit list (I-12). The
// block's secret_files hold redis.yml, its internal gate bearer (internal-gate-sockets.yml), the ticket key file
// (socket-ticket-keys.yml), the svc- key set's public jwks (oauth-jwks-svc.json, as evs.yml, gkm.yml and dns.yml
// mount it), its reader of the first metadata cluster (file-clusters-sockets.yml, for rev/ alone), and
// grafana-push.yml. server/cli/sockets reads each from vault into HostDeps; a test fills HostDeps with keys of its
// own, so no credential of a host run in a test comes from vault.
//
// What the block builds around socketsservice:
//
//   - ServiceHost: sidecarhost's, with Quota over api's internal gate (quotahost.NewBlockQuota, whose OverLimit is
//     Find's spent list) and Push, pushservice's hint writer, which carries SOCKET_HINT on topic sock.
//   - The Authenticator: sidecarhost's NewRevocationAuthenticator, verifying the callers' 15-minute service tokens
//     of audience sockets (QD-1029) offline against oauth-jwks-svc.json (HostDeps.ServiceTokens in a test), their
//     ur_sidecar_id the caller's sidecar, then §7.8's rev/ objects for each token's client, sidecar and member, read
//     by sidecar.RevocationChecker from the first metadata cluster and kept at most 5 s, never SQL. A token that does
//     not verify, or that a rev/ object voids, answers 401; a block with no jwks, no cluster or a rev/ read that
//     fails answers unavailable, never an unauthenticated allow.
//   - The store: model.NewSocketStore on the block's Redis Cluster.
//   - The gate: socketsservice.NewSocketProjectionGate over api's internal projection route (QD-055,
//     AclGateApiSource) on the same bearer (HostDeps.SetGateApiSources). With no bearer or no api the block has no
//     source and stays not ready.
//   - The ticket keys: socketsservice.NewSocketTicketKeySet over socket-ticket-keys.yml, reread every minute: the
//     newest settled key signs, every listed key verifies, and GET /socket/keys publishes them.
//   - One key-event subscriber per process (sidecarhost's), never one per client (server/model/peer_model.go:32-37):
//     {*}c_removed reaches ClientRemoved, {acl_*}v ProjectionChanged and {tes_*}n KillChanged; after a
//     resubscription ProjectionsResync and KillsResync run, since events in the gap are gone. A removal lost in a gap
//     has nothing to resync from, as nothing lists removed clients: the client's token fails at its rev/ object, a
//     later one at the exchange, and its leases lapse within one lease.
//   - config/<env>/sidecar.yml through sidecarhost's Gates: the sockets gate and urnetwork_edge, which the engine
//     reads per call (a closed gate answers 503 with Retry-After and the block stays ready), and sockets_control_qps,
//     which the block lays over the engine's bound each ControlQpsInterval.
//
// Readiness is §19.4's sockets column: Redis answering the engine's probe, a ticket key that signs, the gate ready
// (a source and one over-limit answer) and the key-event families subscribed. A block with no rev/ reader, its
// file-clusters-sockets.yml missing or unreadable, is said once at start and is not ready: each authenticated route
// answers unavailable until it has one. TLS and SMTP share the sockets-only facts read from the internal API. Both
// namespaces require that source before the block reports ready. Each registration still requires its own verified
// listen grant. ur/ opens with the block.
//
// What stays the process's when several hosts run in one test process (e2e-hosts-in-process): the Redis key-event
// subscriber and the metrics registry, which sidecarhost shares by design; sidecar.yml, which server reads once per
// process (server.Testing_PushSidecarConfig sets it for every block at once, and HostDeps.Gates gives one block its
// own); the svc- verifier a host given no HostDeps.ServiceTokens reads (oauth.Testing_SetServiceTokenVerifier); and
// the test hooks (sidecartest.InstallTestHooks). Run sets no process-wide state of its own: the main's
// RunOptions.ProcessEnv sets operator's global Env, and a test leaves it nil.
//
// The key file is the owner's to create in vault (SOCK.md's external list). The command that writes one is an
// operations command, which sidecarctl holds (owner, 2026-11-07; server/sidecarctl since QD-750), so it is not
// this binary's: question sock-m5-07-ticket-keygen-command asks for it, on socketsservice.SocketTicketKeyFileAdd.
package socketshost

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "sockets"

// The ticket key file among the block's secret_files.
const hostTicketKeyFileName = "socket-ticket-keys.yml"

// vault's file-clusters-<service>.yml the block reads: the first metadata cluster alone, whose rev/ objects it reads
// and nothing else (QD-1029).
const HostFileClustersService = "sockets"

// The block's tunables beyond the engine's.
type HostSettings struct {
	// The engine's. nil: socketsservice.DefaultSocketsSettings.
	Sockets *socketsservice.SocketsSettings
	// The ticket key set's.
	TicketKeys *socketsservice.SocketTicketKeySettings
	// The rev/ reader's (§7.8's check row: each subject's answer kept at most 5 s). nil:
	// sidecar.DefaultRevocationSettings.
	Revocation *sidecar.RevocationSettings
	// How often sockets_control_qps is laid over the engine's bound: 60 s, §19.11's reread.
	ControlQpsInterval time.Duration
	// How long the block waits before it subscribes a key-event family again after a subscription failed: 5 s.
	KeyEventsRetryInterval time.Duration
	// tls/'s hooks' (TLS-M5-22). nil: socketsservice.DefaultTlsNamespaceSettings.
	Tls *socketsservice.TlsNamespaceSettings
	// Bounds one internal response for a large Team's listen facts.
	ListenFactsMaxByteCount int64
	// sidecarhost.Run's: readiness's bounds and the status server's drain. nil: sidecarhost.DefaultRunSettings.
	Run *sidecarhost.RunSettings
}

// The defaults.
func DefaultHostSettings() *HostSettings {
	return &HostSettings{
		Sockets:                 socketsservice.DefaultSocketsSettings(),
		TicketKeys:              socketsservice.DefaultSocketTicketKeySettings(),
		Revocation:              sidecar.DefaultRevocationSettings(),
		ControlQpsInterval:      60 * time.Second,
		KeyEventsRetryInterval:  5 * time.Second,
		Tls:                     socketsservice.DefaultTlsNamespaceSettings(),
		ListenFactsMaxByteCount: 16 * 1024 * 1024,
		Run:                     sidecarhost.DefaultRunSettings(),
	}
}

// The seams a test, or the change that lands a missing one, fills, and the credentials a block's main reads from
// vault, which a test fills with its own. Each nil takes the block's own.
type HostDeps struct {
	// The block's ServiceHost, NewBlockHost's and Run's alone (NewHost takes a host built):
	//
	// ServiceHost.Quota: quotahost.NewBlockQuota over api's internal gate on internal-gate-sockets.yml. nil leaves
	// the gate never ready.
	Quota sidecar.Quota
	// The hint writer: pushservice's on the block's Redis.
	Push sidecar.Push
	// The first metadata cluster's reader, whose rev/ objects the Authenticator reads: vault's
	// file-clusters-sockets.yml. nil leaves the block no rev/ reader, so every authenticated route answers
	// unavailable.
	Blobs sidecar.BlobStore
	// The engine's clock: real time.
	Clock sidecar.Clock
	// Each replaces the block's: a test's gates file, a test's Redis.
	Gates sidecar.ServiceGates
	Redis sidecar.Redis

	// The engine's, NewHost's:
	//
	// The projection source: AclGateApiSource on the block's bearer (SetGateApiSources). nil leaves the gate never
	// ready.
	Source sidecar.AclGateSource
	// Reads the ticket key file: vault's socket-ticket-keys.yml.
	KeyFile func() ([]byte, error)
	// The svc- key set's public verifier, which the callers' service tokens and the rev/ objects are verified
	// under: vault's oauth-jwks-svc.json, read once by the process (oauth.VerifyServiceToken). A test's over keys
	// of its own.
	ServiceTokens *oauth.ServiceTokenVerifier
	// The rev/ reader: sidecar.RevocationChecker over the host's Blobs. A test's in its place.
	Revocations sidecarhost.RevocationChecker
	// Replaces the block's service token Authenticator: a test's token issuer.
	Authenticator sidecar.Authenticator
	// The store: model.NewSocketStore.
	Store socketsservice.SocketStore
	// Where the engine and the block log: glog. Ids and positions only (I-13).
	Log sidecar.Logger
	// What the key events reach: the engine. A test records them instead.
	Events HostEngineEvents
	// tls/'s listen facts (TLS-M5-22): api's GET /internal/gate/listen-facts/<network_id> on the block's bearer
	// (plan-SOCK-gate-data, SetGateApiSources). Both public namespaces use its cache. A missing source leaves the
	// host unready.
	TlsListenFacts socketsservice.TlsListenFactsSource
}

// What the block's key-event subscriber calls on the engine (sweep.go's methods), none blocking or doing I/O.
type HostEngineEvents interface {
	ClientRemoved(clientId connect.Id)
	ProjectionChanged(networkId connect.Id)
	ProjectionsResync()
	KillChanged(networkId connect.Id)
	KillsResync()
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type Host struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     *sidecar.ServiceHost
	settings *HostSettings
	engine   *socketsservice.SocketsService
	gate     *socketsservice.SocketProjectionGate
	// tls/'s hooks, nil while the block has no listen facts source
	tlsHooks *socketsservice.TlsNamespaceHooks
	events   HostEngineEvents
	log      sidecar.Logger
	// what the engine honors no request without and the block was not given, each said once
	missing []string
	group   sync.WaitGroup
	// what NewBlockHost built for the host, ended after it
	closeAfter []func()

	stateLock sync.Mutex
	// the key-event families subscribed, and what removes each
	removeKeyEvents []func()
	keyEventsReady  bool
	closed          bool
}

var _ sidecar.HttpEngine = (*Host)(nil)

// The engine on host, its store, gate and ticket keys, with the key events subscribed on host's Redis, all on a
// context of its own that Close ends, so ctx's end, a block's SIGTERM, starts the drain without ending what the
// drain needs. host is the block's ServiceHost, NewBlockHost's from deps for a block: it needs Redis, Metrics and
// Gates, its Quota is the over-limit list, its Blobs the rev/ objects, and its Authenticator is replaced by the
// block's, deps' own or the service token authenticator over rev/, so no host reaches the engine with an
// Authenticator that skips rev/. A host with no Push is given the block's hint writer. A missing seam leaves the
// block not ready, never an error, so it serves /status and says why.
func NewHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if host == nil {
		return nil, fmt.Errorf("%s host: no ServiceHost", hostService)
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	if deps == nil {
		deps = &HostDeps{}
	}
	log := deps.Log
	if log == nil {
		log = hostLogger{}
	}
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// the callers' service tokens and their rev/ objects (QD-1029), unless a test issues its own
	noRevocations := false
	if deps.Authenticator != nil {
		host.Authenticator = deps.Authenticator
	} else {
		revocations := deps.Revocations
		if revocations == nil && host.Blobs != nil {
			revocationSettings := sidecar.DefaultRevocationSettings()
			if settings.Revocation != nil {
				revocationSettingsCopy := *settings.Revocation
				revocationSettings = &revocationSettingsCopy
			}
			if revocationSettings.Clock == nil {
				revocationSettings.Clock = host.Clock
			}
			revocations = sidecar.NewRevocationChecker(host.Blobs, sidecarhost.RevocationVerify(deps.ServiceTokens), revocationSettings)
		}
		noRevocations = revocations == nil
		host.Authenticator = sidecarhost.NewRevocationAuthenticatorWithVerifier(socketsservice.SocketsAudience, revocations, deps.ServiceTokens)
	}
	self := &Host{ctx: hostCtx, cancel: cancel, host: host, settings: settings, log: log}
	fail := func(err error) (*Host, error) {
		cancel()
		return nil, err
	}
	if host.Push == nil {
		// the hint writer resolves clients only; sockets hints clients, never sidecars
		writer, err := pushservice.NewHintWriter(host.Redis, nil, nil, host.Metrics)
		if err != nil {
			return fail(fmt.Errorf("the hint writer: %w", err))
		}
		host.Push = writer
	}
	if host.Quota == nil {
		self.missing = append(self.missing, "ServiceHost.Quota, api's internal over-limit list (QD-055)")
	}
	if deps.Source == nil {
		self.missing = append(self.missing, "a projection source, api's internal projection route (QD-055, ACL-M4-02)")
	}
	if noRevocations {
		self.missing = append(self.missing, "a revocation reader for authenticated sockets routes")
	}
	if deps.TlsListenFacts == nil {
		self.missing = append(self.missing, "the sockets listen facts source for TLS and SMTP")
	}
	store := deps.Store
	if store == nil {
		store = model.NewSocketStore(host.ClockOrReal(), nil)
	}
	gateSettings := *settings.Sockets.Gate
	if gateSettings.Cache != nil && gateSettings.Cache.Log == nil {
		cacheSettings := *gateSettings.Cache
		cacheSettings.Log = log
		gateSettings.Cache = &cacheSettings
	}
	gate, err := socketsservice.NewSocketProjectionGate(hostCtx, host, deps.Source, &gateSettings)
	if err != nil {
		return fail(err)
	}
	self.gate = gate
	engine, err := socketsservice.NewSocketsService(hostCtx, host, store, gate, settings.Sockets)
	if err != nil {
		gate.Close()
		return fail(err)
	}
	self.engine = engine
	// ur/ is the platform's own (SOCK-M9-01): its rules are the engine's, so it opens with the block
	if err := engine.RegisterPlatformNamespace(); err != nil {
		engine.Close()
		gate.Close()
		return fail(err)
	}
	// tls/ opens over its listen facts (TLS-M5-22), which the block reads from api
	if deps.TlsListenFacts != nil {
		tlsHooks, err := socketsservice.NewTlsNamespaceHooks(hostCtx, deps.TlsListenFacts, host.ClockOrReal(), settings.Tls)
		if err == nil {
			// a scope host's scope and consent are the projection's, read through the gate (TLS-M6-03)
			tlsHooks.SetScopeConsent(gate)
			err = engine.RegisterNamespace(sidecar.SocketNamespaceTls, tlsHooks)
		}
		if err == nil {
			mail := mxedgeservice.NewSmtpListenHooks(&mxedgeservice.SmtpListenHooksArgs{Networks: tlsHooks, Scopes: gate})
			err = engine.RegisterSmtpNamespace(mail, gate)
		}
		if err != nil {
			if tlsHooks != nil {
				tlsHooks.Close()
			}
			engine.Close()
			gate.Close()
			return fail(err)
		}
		self.tlsHooks = tlsHooks
	}
	self.events = deps.Events
	if self.events == nil {
		self.events = engine
	}
	keyFile := deps.KeyFile
	if keyFile == nil {
		keyFile = readHostTicketKeyFile
	}
	keySet, err := socketsservice.NewSocketTicketKeySet(hostCtx, keyFile, host.ClockOrReal(), log, settings.TicketKeys)
	if err != nil {
		engine.Close()
		if self.tlsHooks != nil {
			self.tlsHooks.Close()
		}
		gate.Close()
		return fail(err)
	}
	engine.SetTicketKeys(keySet)
	if 0 < len(self.missing) {
		log.Errorf("[%s]not ready without %s", hostService, strings.Join(self.missing, "; "))
	}
	if noRevocations {
		log.Errorf("[%s]every authenticated route answers unavailable: no rev/ reader, since the block has no first metadata cluster (file-clusters-sockets.yml)", hostService)
	}
	// the first try at once, so a block whose Redis answers is ready as soon as the engine is
	if failed := self.subscribe(self.keyEventFamilies()); 0 < len(failed) {
		self.group.Add(1)
		go self.resubscribe(failed)
	}
	self.group.Add(1)
	go self.followControlQps()
	return self, nil
}

// The block's ServiceHost from deps on ctx, which bounds its gates' reads: sidecarhost's with deps' Quota, Push,
// Blobs and Clock, and deps' Gates and Redis in place of the block's when given.
func newBlockServiceHost(ctx context.Context, deps *HostDeps) *sidecar.ServiceHost {
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Quota:         deps.Quota,
		Push:          deps.Push,
		Blobs:         deps.Blobs,
		Clock:         deps.Clock,
		ServiceTokens: deps.ServiceTokens,
	})
	if deps.Gates != nil {
		host.Gates = deps.Gates
	}
	if deps.Redis != nil {
		host.Redis = deps.Redis
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

// vault's ticket key file.
func readHostTicketKeyFile() ([]byte, error) {
	resource, err := server.Vault.SimpleResource(hostTicketKeyFileName)
	if err != nil {
		return nil, fmt.Errorf("no %s: %w", hostTicketKeyFileName, err)
	}
	return resource.BytesE()
}

// The c_removed family as a key pattern, from server/model's channel pattern, so one definition names the key.
var hostClientRemovedKeyPattern = strings.TrimPrefix(model.ClientRemovedKeyEventPattern(0), "__keyspace@0__:")

// The client a c_removed key names.
func parseHostClientRemovedKey(key string) (connect.Id, bool) {
	clientId, ok := model.ParseClientRemovedKeyEvent("__keyspace@0__:" + key)
	return connect.Id(clientId), ok
}

// One key-event family the block follows: what an event's key reaches, and what a resubscription runs.
type hostKeyEventFamily struct {
	pattern string
	event   func(key string)
	resync  func()
}

// The families, each reaching the engine with no I/O on the subscriber's goroutine.
func (self *Host) keyEventFamilies() []*hostKeyEventFamily {
	return []*hostKeyEventFamily{
		{
			pattern: hostClientRemovedKeyPattern,
			event: func(key string) {
				if clientId, ok := parseHostClientRemovedKey(key); ok {
					self.events.ClientRemoved(clientId)
				}
			},
		},
		{
			pattern: sidecar.AclVersionKeyPattern,
			event: func(key string) {
				if networkId, ok := sidecar.ParseAclVersionKey(key); ok {
					self.events.ProjectionChanged(connect.Id(networkId))
				}
			},
			resync: self.events.ProjectionsResync,
		},
		{
			pattern: sidecar.TlsKillTriggerKeyPattern,
			event: func(key string) {
				if networkId, ok := sidecar.ParseTlsKillTriggerKey(key); ok {
					self.events.KillChanged(connect.Id(networkId))
				}
			},
			resync: self.events.KillsResync,
		},
	}
}

// Subscribes each family on the host's one subscriber, and the families that failed. With all held the block's
// key events are ready.
func (self *Host) subscribe(families []*hostKeyEventFamily) []*hostKeyEventFamily {
	if self.host.Redis == nil {
		return families
	}
	failed := []*hostKeyEventFamily{}
	for _, family := range families {
		remove, err := self.host.Redis.AddKeyEventCallback(family.pattern, func(event sidecar.RedisKeyEvent) {
			if event.Resync {
				if family.resync != nil {
					family.resync()
				}
				return
			}
			family.event(event.Key)
		})
		if err != nil {
			failed = append(failed, family)
			continue
		}
		kept := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return false
			}
			self.removeKeyEvents = append(self.removeKeyEvents, remove)
			return true
		}()
		if !kept {
			remove()
		}
	}
	if len(failed) == 0 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.keyEventsReady = true
	}
	return failed
}

// Subscribes the families that failed again each KeyEventsRetryInterval until all are held or the block closes.
func (self *Host) resubscribe(failed []*hostKeyEventFamily) {
	defer self.group.Done()
	for 0 < len(failed) {
		self.log.Warningf("[%s]%d key-event families not subscribed yet", hostService, len(failed))
		select {
		case <-self.ctx.Done():
			return
		case <-self.host.ClockOrReal().After(self.settings.KeyEventsRetryInterval):
		}
		failed = self.subscribe(failed)
	}
}

// Lays sockets_control_qps over the engine's bound now and each ControlQpsInterval until the block closes. A
// rate takes the nearest whole number of calls a second, at least 1.
func (self *Host) followControlQps() {
	defer self.group.Done()
	for {
		if rate, burst, ok := self.host.GatesOrClosed().SocketsControlQps(); ok {
			self.engine.SetControlRate(int(max(1, math.Round(rate))), burst)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.host.ClockOrReal().After(self.settings.ControlQpsInterval):
		}
	}
}

// The block's ServiceHost, which the engine runs on and a main sets the process's Env over.
func (self *Host) ServiceHost() *sidecar.ServiceHost {
	return self.host
}

// The engine, for a test that drives it.
func (self *Host) Engine() *socketsservice.SocketsService {
	return self.engine
}

// What the block was not given that the engine honors no request without: empty once every seam is there.
func (self *Host) Missing() []string {
	return append([]string{}, self.missing...)
}

// §19.4's sockets column: every seam given, the key-event families held, and the engine ready.
func (self *Host) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	ready := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.keyEventsReady && !self.closed
	}()
	return ready && self.engine.Ready()
}

// The engine's routes.
func (self *Host) Handler() http.Handler {
	return self.engine.Handler()
}

// The engine takes no new work.
func (self *Host) Drain() {
	self.engine.Drain()
}

// Ends the key events, the engine, the gate and the block's loops, then what NewBlockHost built. Safe after Drain,
// and more than once.
func (self *Host) Close() {
	removes, closed := func() ([]func(), bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil, true
		}
		self.closed = true
		removes := self.removeKeyEvents
		self.removeKeyEvents = nil
		return removes, false
	}()
	if closed {
		return
	}
	for _, remove := range removes {
		remove()
	}
	self.cancel()
	self.engine.Close()
	if self.tlsHooks != nil {
		self.tlsHooks.Close()
	}
	self.gate.Close()
	self.group.Wait()
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

// What a block's main, or a test, gives Run.
type RunOptions struct {
	// GET /status's port, which also serves the routes: 80 in a block.
	Port int
	// A listener already bound for /status and the routes, which Run serves in place of binding Port: a test's on
	// 127.0.0.1:0, so the OS picks the port. Run takes it. nil: Port, as warp maps it.
	StatusListener net.Listener
	// nil: the defaults.
	Settings *HostSettings
	// nil: none, so never ready. A block's main fills them from vault.
	Deps *HostDeps
	// The block's /status latch, so a test reads it. nil: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
	// Called once before the block serves with the engine's ServiceHost, or an empty one for a block that could not
	// start: a block's main sets the process's Env here (QD-1027). nil sets nothing, as a test running several
	// blocks in one process leaves it.
	ProcessEnv func(host *sidecar.ServiceHost)
}

// Runs the block until ctx ends through sidecarhost.Run (§19.4): GET /status from the first moment, the routes
// beside it once ready, then at ctx's end the drain and Close. A block that cannot start logs why and serves
// /status not ready until ctx ends.
func Run(ctx context.Context, options *RunOptions) error {
	settings := options.Settings
	if settings == nil {
		settings = DefaultHostSettings()
	}
	var engine sidecar.Engine = notReadyEngine{}
	processHost := &sidecar.ServiceHost{}
	host, err := NewBlockHost(ctx, settings, options.Deps)
	if err != nil {
		glog.Errorf("[%s]not ready: %v\n", hostService, err)
	} else {
		engine = host
		processHost = host.host
	}
	if options.ProcessEnv != nil {
		options.ProcessEnv(processHost)
	}
	runOptions := &sidecarhost.RunOptions{
		Service:        hostService,
		StatusPort:     options.Port,
		StatusListener: options.StatusListener,
		Engine:         engine,
		WarpStatus:     options.WarpStatus,
		Settings:       settings.Run,
	}
	if httpEngine, ok := engine.(sidecar.HttpEngine); ok {
		runOptions.Http = httpEngine
	}
	return sidecarhost.Run(ctx, runOptions)
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
