// server/cli/sockets (SOCK-M5-08; D-SOCK-10, D-SOCK-14, R-SOCK-6, I-12, I-13, §9.8, §19.4): the sockets engine at
// sockets.<root>, one warp block on every lb host, since each lb proxies only to its own blocks (D-SOCK-10), run by
// sidecarhost.Run with GET /status and §9.15's /socket/* routes on port 80, {vault: no, config: yes, site: no}, user
// 65532 and no Postgres: everything the engine keeps is soft state in Redis Cluster (§9.8), and api is reached only
// off the request paths, for the projection and the over-limit list (I-12). The block's secret_files hold
// redis.yml, its internal gate bearer (internal-gate-sockets.yml), the ticket key file (socket-ticket-keys.yml),
// the svc- key set's public jwks (oauth-jwks-svc.json, as evs.yml, gkm.yml and dns.yml mount it), its reader of
// the first metadata cluster (file-clusters-sockets.yml, for rev/ alone), and grafana-push.yml.
//
// What the block builds around socketsservice:
//
//   - ServiceHost: sidecarhost's, with Quota over api's internal gate at -api_url (quotahost.NewBlockQuota, whose
//     OverLimit is Find's spent list) and Push, pushservice's hint writer, which carries SOCKET_HINT on topic sock.
//     The flag's default is the $UR_API_URL sockets.yml's env_vars give a deployed block (sidecarhost.ServiceUrlFlag).
//   - The Authenticator: sidecarhost's NewRevocationAuthenticator, verifying the callers' 15-minute service tokens
//     of audience sockets (QD-1029) offline against oauth-jwks-svc.json, their ur_sidecar_id the caller's sidecar,
//     then §7.8's rev/ objects for each token's client, sidecar and member, read by sidecar.RevocationChecker from
//     the first metadata cluster and kept at most 5 s, never SQL. A token that does not verify, or that a rev/
//     object voids, answers 401; a block with no jwks, no cluster or a rev/ read that fails answers unavailable,
//     never an unauthenticated allow.
//   - The store: model.NewSocketStore on the block's Redis Cluster.
//   - The gate: socketsservice.NewSocketProjectionGate over api's internal projection route (QD-055,
//     AclGateApiSource) on the same bearer. With no bearer or no api the block has no source and stays not ready.
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
// file-clusters-sockets.yml missing or unreadable, is said once at start and leaves readiness as §19.4 has it: each
// authenticated route answers unavailable until it has one. What server has no implementation of yet fails
// closed, each said once at start: tls/'s ValidateListen (TLS-M5-22) opens only over a listen facts source, which
// the block reads from api's GET /internal/gate/listen-facts/<network_id> (plan-SOCK-gate-data), a route not built
// yet, so with no source in hostDeps tls/ is not registered; smtp/'s (MAIL-M7-22) does not exist. The engine refuses
// every entry of a namespace that is not registered. ur/ is the platform's (SOCK-M9-01) and opens with the block:
// urnetwork-edge's clients list its services, and any sidecar Finds them.
//
// The key file is the owner's to create in vault (SOCK.md's external list). The command that writes one is an
// operations command, which sidecarctl holds (owner, 2026-11-07; server/sidecarctl since QD-750), so it is not
// this binary's: question sock-m5-07-ticket-keygen-command asks for it, on socketsservice.SocketTicketKeyFileAdd.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it.
const hostService = "sockets"

// The ticket key file among the block's secret_files.
const hostTicketKeyFileName = "socket-ticket-keys.yml"

// vault's file-clusters-<service>.yml the block reads: the first metadata cluster alone, whose rev/ objects it reads
// and nothing else (QD-1029).
const hostFileClustersService = "sockets"

// The block's tunables beyond the engine's.
type hostSettings struct {
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
}

// The defaults.
func defaultHostSettings() *hostSettings {
	return &hostSettings{
		Sockets:                socketsservice.DefaultSocketsSettings(),
		TicketKeys:             socketsservice.DefaultSocketTicketKeySettings(),
		Revocation:             sidecar.DefaultRevocationSettings(),
		ControlQpsInterval:     60 * time.Second,
		KeyEventsRetryInterval: 5 * time.Second,
		Tls:                    socketsservice.DefaultTlsNamespaceSettings(),
	}
}

// The seams a test, or the change that lands a missing one, fills. Each nil takes the block's own.
type hostDeps struct {
	// ServiceHost.Quota: quotahost.NewBlockQuota over api's internal gate. nil leaves the gate never ready.
	Quota sidecar.Quota
	// The projection source: AclGateApiSource on the block's bearer. nil leaves the gate never ready.
	Source sidecar.AclGateSource
	// Reads the ticket key file: vault's socket-ticket-keys.yml.
	KeyFile func() ([]byte, error)
	// The first metadata cluster's reader, whose rev/ objects the Authenticator reads: vault's
	// file-clusters-sockets.yml. nil leaves the block no rev/ reader, so every authenticated route answers
	// unavailable.
	Blobs sidecar.BlobStore
	// The rev/ reader: sidecar.RevocationChecker over Blobs. A test's in its place.
	Revocations sidecarhost.RevocationChecker
	// Each replaces the block's: a test's token issuer in place of the service token Authenticator, a test's gates
	// file, a test's Redis.
	Authenticator sidecar.Authenticator
	Gates         sidecar.ServiceGates
	Redis         sidecar.Redis
	// The store: model.NewSocketStore.
	Store socketsservice.SocketStore
	// The hint writer: pushservice's on the block's Redis.
	Push sidecar.Push
	// The engine's clock: real time.
	Clock sidecar.Clock
	// Where the engine and the block log: glog. Ids and positions only (I-13).
	Log sidecar.Logger
	// What the key events reach: the engine. A test records them instead.
	Events hostEngineEvents
	// tls/'s listen facts (TLS-M5-22): api's GET /internal/gate/listen-facts/<network_id> on the block's bearer
	// (plan-SOCK-gate-data), which no route serves yet. nil leaves tls/ unregistered, refusing every entry.
	TlsListenFacts socketsservice.TlsListenFactsSource
}

// What the block's key-event subscriber calls on the engine (sweep.go's methods), none blocking or doing I/O.
type hostEngineEvents interface {
	ClientRemoved(clientId connect.Id)
	ProjectionChanged(networkId connect.Id)
	ProjectionsResync()
	KillChanged(networkId connect.Id)
	KillsResync()
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type socketsHost struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     *sidecar.ServiceHost
	settings *hostSettings
	engine   *socketsservice.SocketsService
	gate     *socketsservice.SocketProjectionGate
	// tls/'s hooks, nil while the block has no listen facts source
	tlsHooks *socketsservice.TlsNamespaceHooks
	events   hostEngineEvents
	log      sidecar.Logger
	// what the engine honors no request without and the block was not given, each said once
	missing []string
	group   sync.WaitGroup

	stateLock sync.Mutex
	// the key-event families subscribed, and what removes each
	removeKeyEvents []func()
	keyEventsReady  bool
	closed          bool
}

var _ sidecar.HttpEngine = (*socketsHost)(nil)

// The engine on its host, store, gate and ticket keys, with the key events subscribed on the host's Redis, all on a
// context of its own that Close ends, so ctx's end, a block's SIGTERM, starts the drain without ending what the
// drain needs. A missing seam leaves the block not ready, never an error, so it serves /status and says why.
func newSocketsHost(ctx context.Context, settings *hostSettings, deps *hostDeps) (*socketsHost, error) {
	if settings == nil {
		settings = defaultHostSettings()
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
		Quota: deps.Quota,
		Push:  deps.Push,
		Blobs: deps.Blobs,
		Clock: deps.Clock,
	})
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
			revocations = sidecar.NewRevocationChecker(host.Blobs, oauth.VerifyRevocation, revocationSettings)
		}
		noRevocations = revocations == nil
		host.Authenticator = sidecarhost.NewRevocationAuthenticator(socketsservice.SocketsAudience, revocations)
	}
	if deps.Gates != nil {
		host.Gates = deps.Gates
	}
	if deps.Redis != nil {
		host.Redis = deps.Redis
	}
	self := &socketsHost{ctx: hostCtx, cancel: cancel, host: host, settings: settings, log: log}
	fail := func(err error) (*socketsHost, error) {
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
	if deps.Quota == nil {
		self.missing = append(self.missing, "ServiceHost.Quota, api's internal over-limit list (QD-055)")
	}
	if deps.Source == nil {
		self.missing = append(self.missing, "a projection source, api's internal projection route (QD-055, ACL-M4-02)")
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
	if self.tlsHooks == nil {
		log.Infof("[%s]tls/ refuses every entry until the block has a listen facts source (plan-SOCK-gate-data)", hostService)
	}
	log.Infof("[%s]smtp/ refuses every entry until its ValidateListen lands (MAIL-M7-22)", hostService)
	// the first try at once, so a block whose Redis answers is ready as soon as the engine is
	if failed := self.subscribe(self.keyEventFamilies()); 0 < len(failed) {
		self.group.Add(1)
		go self.resubscribe(failed)
	}
	self.group.Add(1)
	go self.followControlQps()
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
func (self *socketsHost) keyEventFamilies() []*hostKeyEventFamily {
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
func (self *socketsHost) subscribe(families []*hostKeyEventFamily) []*hostKeyEventFamily {
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
func (self *socketsHost) resubscribe(failed []*hostKeyEventFamily) {
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
func (self *socketsHost) followControlQps() {
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

// What the block was not given that the engine honors no request without: empty once every seam is there.
func (self *socketsHost) Missing() []string {
	return append([]string{}, self.missing...)
}

// §19.4's sockets column: every seam given, the key-event families held, and the engine ready.
func (self *socketsHost) Ready() bool {
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
func (self *socketsHost) Handler() http.Handler {
	return self.engine.Handler()
}

// The engine takes no new work.
func (self *socketsHost) Drain() {
	self.engine.Drain()
}

// Ends the key events, the engine, the gate and the block's loops. Safe after Drain, and more than once.
func (self *socketsHost) Close() {
	removes := func() []func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		self.closed = true
		removes := self.removeKeyEvents
		self.removeKeyEvents = nil
		return removes
	}()
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
}

// A block that could not start: never ready, serving nothing but /status.
type notReadyEngine struct{}

func (notReadyEngine) Ready() bool { return false }
func (notReadyEngine) Drain()      {}
func (notReadyEngine) Close()      {}

// The block's own deps: Quota and the projection source over api's internal gate at apiUrl on the block's bearer,
// and the first metadata cluster's reader for rev/ (vault's file-clusters-sockets.yml), each left out, and said,
// when it is missing. quotaCtx outlives the drain, so the open usage window is sent; the clusters close with it.
func blockDeps(quotaCtx context.Context, apiUrl string) (*hostDeps, func()) {
	deps := &hostDeps{}
	ends := []func(){}
	drain := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerSockets, apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[%s]no ServiceHost.Quota: %v\n", hostService, err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	if clusters, err := server.LoadFileClusters(quotaCtx, hostFileClustersService, nil); err != nil {
		// the error names the file, never a credential
		glog.Errorf("[%s]no rev/ reader: file-clusters-%s.yml: %v\n", hostService, hostFileClustersService, err)
	} else {
		deps.Blobs = clusters
		ends = append(ends, clusters.Close)
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerSockets)
	if err != nil {
		glog.Errorf("[%s]no projection source: %v\n", hostService, err)
		return deps, drain
	}
	source, err := sidecar.NewAclGateApiSource(apiUrl, &http.Client{}, func() (string, error) { return bearer, nil }, nil)
	if err != nil {
		glog.Errorf("[%s]no projection source: %v\n", hostService, err)
		return deps, drain
	}
	deps.Source = source
	return deps, drain
}

// What a block's main gives run.
type runOptions struct {
	// GET /status's port, which also serves the routes: 80 in a block.
	Port int
	// nil: the defaults.
	Settings *hostSettings
	// nil: the block's own (blockDeps).
	Deps *hostDeps
}

// Runs the block until ctx ends through sidecarhost.Run (§19.4): GET /status from the first moment, the routes
// beside it once ready, then at ctx's end the drain and Close. A block that cannot start logs why and serves
// /status not ready until ctx ends.
func run(ctx context.Context, options *runOptions) error {
	var engine sidecar.Engine = notReadyEngine{}
	host, err := newSocketsHost(ctx, options.Settings, options.Deps)
	if err != nil {
		glog.Errorf("[%s]not ready: %v\n", hostService, err)
		setProcessEnv(&sidecar.ServiceHost{})
	} else {
		engine = host
		setProcessEnv(host.host)
	}
	runOptions := &sidecarhost.RunOptions{
		Service:    hostService,
		StatusPort: options.Port,
		Engine:     engine,
	}
	if httpEngine, ok := engine.(sidecar.HttpEngine); ok {
		runOptions.Http = httpEngine
	}
	return sidecarhost.Run(ctx, runOptions)
}

// Sets the process's Env, operator's global one (QD-1027), before the block serves: over the engine's own
// ServiceHost, or an empty one for a block that could not start, which holds no service, so every gate reads
// closed and the rest refuses. sockets holds no Postgres, so its Env refuses the db either way.
func setProcessEnv(host *sidecar.ServiceHost) {
	if _, err := core.SetProcessEnv(hostService, host); err != nil {
		panic(err)
	}
}

var (
	port   = flag.Int("port", 80, "the block's status port, which also serves the /socket/* routes")
	apiUrl = sidecarhost.NewServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName, "api's base URL, whose internal gate the projection and the over-limit list are read from; empty leaves the block not ready")
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

	// on a context of its own, so a SIGTERM's drain can still send the open usage window, drained after the block
	quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer quotaCancel()
	deps, drainQuota := blockDeps(quotaCtx, apiUrl.Url())
	defer drainQuota()
	if err := run(ctx, &runOptions{Port: *port, Deps: deps}); err != nil {
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
