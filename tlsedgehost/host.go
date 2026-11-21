// Package tlsedgehost is server/cli/tlsedge's host (TLS-M5-11; R-TLS-2, D-TLS-1, D-TLS-3, D-TLS-4, D-TLS-9, I-3, I-12,
// I-13, §13.5, §13.6, §19.4): sidecar's tlsedgeservice engine, §13.5's layer-4 router by SNI, run as one warp block on
// every lb host by sidecarhost.Run, since each lb proxies only to its own blocks, and what a T3 or e2e test starts the
// engine through. A block serves GET /status on port 80, {vault: no, config: yes, site: no} and user 65532. It mounts
// no vault and holds no TLS key (D-TLS-1): its secret_files hold redis.yml, the restricted Postgres role the label
// map and the kill rows are read under (pg-tlsedge.yml, plan-TLS-edge-data-source), its internal gate bearer for the
// quota surface (internal-gate-tlsedge.yml), the edge bearer its platform clients are minted on
// (internal-gate-edge.yml, QD-812) and grafana-push.yml. server's main pool reads pg-tlsedge.yml, the file of the
// process's own service, in place of the platform's pg.yml, which the block does not hold (QD-992; server/db.go's
// pgServiceVaultResourceName), and falls back to pg.yml only where pg-tlsedge.yml is absent, as in the local stack.
// Until the owner creates the role and its file, the store cannot connect and the block stays not ready.
// server/cli/tlsedge reads tlsedge.yml and each credential into HostSettings and HostDeps; a test fills them with its
// own, so no credential of a host run in a test comes from vault.
//
// What the block builds around tlsedgeservice:
//
//   - ServiceHost: sidecarhost's, with Quota over api's internal gate (quotahost.NewBlockQuota) and ServiceClient,
//     server.NewPlatformClientSource over api's edge client route on the edge bearer, for the deploy slot warp
//     hands the process in WARP_SLOT (server.LoadEdgeClientSlot, QD-873, QD-1011), one no other running tlsedge
//     process on the host holds, so beta's and g1's processes and a deploy's new process beside either each hold
//     clients of their own.
//   - The store: model.NewTlsEdgeStore on the block's Postgres role, pg-tlsedge.yml's, read only by the engine's
//     loops and record batches, never by a connection (I-12).
//   - The clients: sidecar.NewEdgeClientPool, K platform clients in urnetwork-edge, Required and relay-only,
//     providing Service alone (D-TLS-4, I-3). The pool is made once the source answers every index, tried again
//     each ClientsRetryInterval until then.
//   - The dialer: sidecar.NewSocketEdgeDialer over the pool's clients and the sockets service, each Find on the
//     first client's sockets token, the service token of audience sockets api's edge client route answers beside
//     its jwt (QD-1054; the source's SocketFindHttpArgs), as tlsedgeservice.TlsEdgeDialer (hostDialer).
//   - One key-event subscriber per process (sidecarhost's), never one per client or network: {tes_*}n, the family
//     model.TlsSuspensionKeyEventPattern names, reaches KillChanged, and after a resubscription KillsResync runs,
//     since events in the gap are gone. The engine reloads every kill each 60 s besides.
//   - config/<env>/sidecar.yml through sidecarhost's Gates, reread every 60 s: the engine reads the tlsedge gate
//     per connection, and a closed gate answers the visitor's hello with internal_error while the block stays
//     ready (§19.11).
//   - One tlsedgeservice.NewProxyHeaderListener for each of TCP 443, 8443, 8883 and 5671 and no other port, with
//     Serve on each (owner decisions 68 and 121). A test gives each its own bound listener (RunOptions.Listeners),
//     which still answers its service port.
//
// Readiness is §19.4's tlsedge column: every seam given, the {tes_} family subscribed, the engine ready (the label
// map's first snapshot read, the kill rows and keys read) and all K clients connected. The block never serves on
// an empty map: a store that cannot answer, a binary that does not link the label map's migrations, or a
// tlsedge.yml the engine refuses leaves it serving /status not ready and nothing else.
//
// What stays the process's when several hosts run in one test process (e2e-hosts-in-process): the Redis key-event
// subscriber and the metrics registry, which sidecarhost shares by design; server's Postgres pools, which the store
// reads; sidecar.yml, which server reads once per process (server.Testing_PushSidecarConfig sets it for every block
// at once, and HostDeps.Gates gives one block its own); and the test hooks (sidecartest.InstallTestHooks). Run sets
// no process-wide state of its own: the main's RunOptions.ProcessEnv sets operator's global Env, and a test leaves
// it nil.
//
// What this draft does not do as §13.5 words it: a hello past sidecar.MaxStreamEarlyDataByteCount has its rest
// written once the listener has accepted, not before, since SocketEdgeDialer.Dial returns only then.
package tlsedgehost

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it, and the block as config/<env>/edge.yml lists it.
const hostService = "tlsedge"

// §9.7's protocol filter for every Find the block sends.
const hostFindProtocol = "tls"

// The ports tlsedge serves, and no other (owner decisions 68 and 121): identifiers of the protocol's ports, not
// tunables, so a test binds its own listener for each rather than another port.
var HostServicePorts = []int{443, 8443, 8883, 5671}

// The block's tunables beyond the engine's.
type HostSettings struct {
	// The engine's, from tlsedge.yml. It has no default here: root and blocks are the environment's.
	TlsEdge *tlsedgeservice.TlsEdgeSettings
	// The pool's. Its ClientCount is the engine's K.
	Clients *sidecar.EdgeClientPoolSettings
	// How long the block waits before it makes its clients again after the source could not answer: 5 s.
	ClientsRetryInterval time.Duration
	// How long the block waits before it subscribes the key-event family again after a subscription failed: 5 s.
	KeyEventsRetryInterval time.Duration
	// The most of a hello sent as StreamOpen's early data: sidecar.MaxStreamEarlyDataByteCount, the protocol's.
	EarlyDataMaxByteCount int
	// sidecarhost.Run's: readiness's bounds and the status server's drain. nil: sidecarhost.DefaultRunSettings.
	Run *sidecarhost.RunSettings
}

// The defaults over the engine's settings.
func DefaultHostSettings(tlsEdge *tlsedgeservice.TlsEdgeSettings) *HostSettings {
	clients := sidecar.DefaultEdgeClientPoolSettings()
	if tlsEdge != nil {
		clients.ClientCount = tlsEdge.Clients
	}
	return &HostSettings{
		TlsEdge:                tlsEdge,
		Clients:                clients,
		ClientsRetryInterval:   5 * time.Second,
		KeyEventsRetryInterval: 5 * time.Second,
		EarlyDataMaxByteCount:  sidecar.MaxStreamEarlyDataByteCount,
		Run:                    sidecarhost.DefaultRunSettings(),
	}
}

// The block's K clients, as sidecar.EdgeClientPool is.
type HostClients interface {
	Clients() []*sidecar.SocketEdgeClient
	ClientIds() []sidecar.Id
	// Whether all K are connected now.
	Ready() bool
	Close()
}

// The seams a test, or the change that lands a missing one, fills, and what a block's main builds from vault's
// credentials, which a test builds from its own. A nil Quota, Store, NewClients or NewDialer leaves the block not
// ready, and said.
type HostDeps struct {
	// The block's ServiceHost, NewBlockHost's and Run's alone (NewHost takes a host built):
	//
	// ServiceHost.Quota: quotahost.NewBlockQuota over api's internal gate on internal-gate-tlsedge.yml.
	Quota sidecar.Quota
	// ServiceHost.ServiceClient: server.NewPlatformClientSource on internal-gate-edge.yml's bearer.
	Source sidecar.ServiceClientSource
	// Each replaces the block's: a test's gates file, a test's Redis, a test's metrics registry.
	Gates   sidecar.ServiceGates
	Redis   sidecar.Redis
	Metrics sidecar.Metrics
	// The engine's clock: real time.
	Clock sidecar.Clock

	// The engine's, NewHost's:
	//
	// The engine's store: model.NewTlsEdgeStore on pg-tlsedge.yml's role.
	Store tlsedgeservice.TlsEdgeStore
	// Makes the block's K clients: sidecar.NewEdgeClientPool on Source. Called again until it answers.
	NewClients func(ctx context.Context) (HostClients, error)
	// Find and open over the clients, as the engine's request: sidecar.NewSocketEdgeDialer.
	NewDial func(clients HostClients) (HostSocketDial, error)
	// Where the block logs: glog. Indexes and counts only (I-13).
	Log sidecar.Logger
	// What the key events reach: the engine. A test records them instead.
	Events HostEngineEvents
}

// What the block's key-event subscriber calls on the engine, neither blocking nor doing I/O.
type HostEngineEvents interface {
	KillChanged(networkId sidecar.Id)
	KillsResync()
}

// One Find and open through sidecar.SocketEdgeDialer, its stream as the engine holds it.
type HostSocketDial func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error)

// The clients and the dial over them, once both exist.
type hostDialerState struct {
	clients HostClients
	dial    HostSocketDial
}

// tlsedgeservice.TlsEdgeDialer over the block's clients, which come after the engine: until they do it holds no
// client id, so the engine is not ready, and every dial answers unavailable. Safe for concurrent use.
type hostDialer struct {
	earlyDataMaxByteCount int

	stateLock sync.Mutex
	state     *hostDialerState
}

var _ tlsedgeservice.TlsEdgeDialer = (*hostDialer)(nil)

func (self *hostDialer) current() *hostDialerState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.state
}

func (self *hostDialer) set(state *hostDialerState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.state = state
}

// The K client ids every Find sends, none before the clients exist.
func (self *hostDialer) ClientIds() []sidecar.Id {
	state := self.current()
	if state == nil {
		return nil
	}
	return state.clients.ClientIds()
}

// Whether the clients exist and all K are connected.
func (self *hostDialer) Ready() bool {
	state := self.current()
	return state != nil && state.clients.Ready()
}

// See tlsedgeservice.TlsEdgeDialer: the engine's request as SocketEdgeDialer's, the hello's first
// EarlyDataMaxByteCount bytes as early data and its rest written as soon as the stream is open. A rest that
// cannot be written ends the stream and answers unavailable: a listener never sees half a hello and no more.
func (self *hostDialer) Dial(ctx context.Context, request *tlsedgeservice.TlsEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
	state := self.current()
	if state == nil || request == nil || request.Target == nil {
		return nil, &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}
	}
	earlyData := request.HelloBytes
	var rest []byte
	if self.earlyDataMaxByteCount < len(earlyData) {
		earlyData, rest = earlyData[:self.earlyDataMaxByteCount], earlyData[self.earlyDataMaxByteCount:]
	}
	stream, err := state.dial(ctx, &sidecar.SocketEdgeDialRequest{
		NetworkId:    request.Target.NetworkId,
		NetworkLabel: request.Target.Label,
		Name:         request.Target.Name,
		Protocol:     hostFindProtocol,
		Alpns:        request.AlpnIds,
		// an alias's or a custom host's name, which Find matches against certified listeners alone
		// (TLS-M6-02; DESIGN-DETAILS 17.2.10)
		ServerName:  request.ServerName,
		AffinityKey: request.AffinityKey,
		Metadata:    request.Metadata,
		EarlyData:   earlyData,
	})
	if err != nil {
		return nil, err
	}
	if 0 < len(rest) {
		if _, err := stream.Write(rest); err != nil {
			stream.Close()
			return nil, &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}
		}
	}
	return stream, nil
}

// The engine as the block runs it, with what it owns. Safe for concurrent use.
type Host struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     *sidecar.ServiceHost
	settings *HostSettings
	deps     *HostDeps
	engine   *tlsedgeservice.TlsEdgeService
	dialer   *hostDialer
	events   HostEngineEvents
	log      sidecar.Logger
	// what the engine routes no connection without and the block was not given, each said once
	missing []string
	group   sync.WaitGroup
	// what NewBlockHost built for the host, ended after it
	closeAfter []func()

	// closes once the clients and the dial over them exist: what a test waits on before it asks Ready
	clientsMade chan struct{}

	stateLock sync.Mutex
	// what removes the key-event family's subscription
	removeKeyEvents []func()
	keyEventsReady  bool
	closed          bool
}

var _ sidecar.StreamEngine = (*Host)(nil)

// The engine on host, its store and dialer, with the key events subscribed on host's Redis and the clients made in
// the background, all on a context of its own that Close ends, so ctx's end, a block's SIGTERM, starts the drain
// without ending what the drain needs. host is the block's ServiceHost, NewBlockHost's from deps for a block: it
// needs Redis, Metrics and Gates, and its Quota is the quota surface. A missing seam leaves the block not ready,
// never an error, so it serves /status and says why.
func NewHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*Host, error) {
	if host == nil {
		return nil, errors.New("no tlsedge ServiceHost")
	}
	if settings == nil || settings.TlsEdge == nil || settings.Clients == nil {
		return nil, errors.New("no tlsedge settings")
	}
	if settings.ClientsRetryInterval <= 0 || settings.KeyEventsRetryInterval <= 0 || settings.EarlyDataMaxByteCount < 0 ||
		sidecar.MaxStreamEarlyDataByteCount < settings.EarlyDataMaxByteCount {
		return nil, errors.New("the block's retry intervals are positive and its early data within the protocol's")
	}
	if settings.Clients.ClientCount != settings.TlsEdge.Clients {
		return nil, errors.New("the pool's client count is not the engine's K")
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
		ctx:         hostCtx,
		cancel:      cancel,
		host:        host,
		settings:    settings,
		deps:        deps,
		dialer:      &hostDialer{earlyDataMaxByteCount: settings.EarlyDataMaxByteCount},
		log:         log,
		clientsMade: make(chan struct{}),
	}
	if host.Quota == nil {
		self.missing = append(self.missing, "ServiceHost.Quota, api's internal quota surface (QD-055)")
	}
	if deps.Store == nil {
		self.missing = append(self.missing, "the store, the label map and the kill rows in Postgres (plan-TLS-edge-data-source)")
	}
	if deps.NewClients == nil || deps.NewDial == nil {
		self.missing = append(self.missing, "the platform clients, api's edge client route on the edge bearer (QD-812)")
	}
	engine, err := tlsedgeservice.NewTlsEdgeService(hostCtx, host, deps.Store, self.dialer, settings.TlsEdge)
	if err != nil {
		cancel()
		return nil, err
	}
	self.engine = engine
	self.events = deps.Events
	if self.events == nil {
		self.events = engine
	}
	if 0 < len(self.missing) {
		log.Errorf("[%s]not ready without %s", hostService, strings.Join(self.missing, "; "))
	}
	// the first try at once, so a block whose Redis answers is ready as soon as the engine is
	if !self.subscribe() {
		self.group.Add(1)
		go self.resubscribe()
	}
	if deps.NewClients != nil && deps.NewDial != nil {
		self.group.Add(1)
		go self.runClients()
	}
	return self, nil
}

// The block's ServiceHost from deps on ctx, which bounds its gates' reads: sidecarhost's with deps' Quota, Source
// and Clock, and deps' Gates, Redis and Metrics in place of the block's when given.
func newBlockServiceHost(ctx context.Context, deps *HostDeps) *sidecar.ServiceHost {
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Quota:         deps.Quota,
		ServiceClient: deps.Source,
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
func (self *Host) Engine() *tlsedgeservice.TlsEdgeService {
	return self.engine
}

// Closes once the clients and the dial over them exist: what a test waits on before it asks Ready.
func (self *Host) ClientsMade() <-chan struct{} {
	return self.clientsMade
}

// Subscribes the {tes_*}n family on the host's one subscriber, and whether it is held.
func (self *Host) subscribe() bool {
	if self.host.Redis == nil {
		return false
	}
	remove, err := self.host.Redis.AddKeyEventCallback(sidecar.TlsKillTriggerKeyPattern, func(event sidecar.RedisKeyEvent) {
		if event.Resync {
			self.events.KillsResync()
			return
		}
		if networkId, ok := sidecar.ParseTlsKillTriggerKey(event.Key); ok {
			self.events.KillChanged(networkId)
		}
	})
	if err != nil {
		return false
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.removeKeyEvents = append(self.removeKeyEvents, remove)
		self.keyEventsReady = true
		return true
	}()
	if !kept {
		remove()
	}
	return true
}

// Subscribes the family again each KeyEventsRetryInterval until it is held or the block closes.
func (self *Host) resubscribe() {
	defer self.group.Done()
	for {
		self.log.Warningf("[%s]the kill switch's key events are not subscribed yet", hostService)
		select {
		case <-self.ctx.Done():
			return
		case <-self.host.ClockOrReal().After(self.settings.KeyEventsRetryInterval):
		}
		if self.subscribe() {
			// what was published while nothing was subscribed is gone, as in any gap
			self.events.KillsResync()
			return
		}
	}
}

// Makes the block's clients and the dial over them, again each ClientsRetryInterval until both exist or the block
// closes: the source answers nothing until api does.
func (self *Host) runClients() {
	defer self.group.Done()
	for {
		state, err := func() (*hostDialerState, error) {
			clients, err := self.deps.NewClients(self.ctx)
			if err != nil {
				return nil, err
			}
			dial, err := self.deps.NewDial(clients)
			if err != nil {
				clients.Close()
				return nil, err
			}
			return &hostDialerState{clients: clients, dial: dial}, nil
		}()
		if err == nil {
			kept := func() bool {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.closed {
					return false
				}
				self.dialer.set(state)
				return true
			}()
			if !kept {
				state.clients.Close()
			}
			close(self.clientsMade)
			return
		}
		if self.ctx.Err() != nil {
			return
		}
		// the pool's and the source's errors name an index, a part or a status, never a secret or an address
		self.log.Warningf("[%s]no platform clients yet: %v", hostService, err)
		select {
		case <-self.ctx.Done():
			return
		case <-self.host.ClockOrReal().After(self.settings.ClientsRetryInterval):
		}
	}
}

// What the block was not given that the engine routes no connection without: empty once every seam is there.
func (self *Host) Missing() []string {
	return append([]string{}, self.missing...)
}

// §19.4's tlsedge column: every seam given, the key events held, the engine ready and all K clients connected.
func (self *Host) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	ready := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.keyEventsReady && !self.closed
	}()
	return ready && self.engine.Ready() && self.dialer.Ready()
}

// See sidecar.StreamEngine: the engine's, which refuses any port but the four.
func (self *Host) Serve(listener net.Listener) error {
	return self.engine.Serve(listener)
}

// The engine takes no new connection and waits out the ones it has, up to drain_s.
func (self *Host) Drain() {
	self.engine.Drain()
}

// Ends the key events, the engine, the block's loops and its clients, then what NewBlockHost built. Safe after
// Drain, and more than once.
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
	self.group.Wait()
	if state := self.dialer.current(); state != nil {
		self.dialer.set(nil)
		state.clients.Close()
	}
	for _, close := range self.closeAfter {
		close()
	}
	self.closeAfter = nil
}

// A listener that names the service port it serves. A block binds the host port warp maps the service port to
// (sidecarhost.Run), while the engine and the PROXY listener read the port a visitor came in on from the
// listener's address, so the address answers the service port.
type servicePortListener struct {
	net.Listener
	servicePort int
}

// The bound address with the service port.
func (self *servicePortListener) Addr() net.Addr {
	address := &net.TCPAddr{Port: self.servicePort}
	if bound, ok := self.Listener.Addr().(*net.TCPAddr); ok {
		address.IP = bound.IP
		address.Zone = bound.Zone
	}
	return address
}

// RunListener's Wrap for one of the block's ports: the lb's PROXY header off each connection before the engine
// sees it, and one with no header dropped, unanswered. settings is copied, its Clock replaced by clock. Ending
// ctx closes the listener.
func newProxyListenerWrap(ctx context.Context, servicePort int, settings *tlsedgeservice.ProxyHeaderSettings, clock sidecar.Clock) func(listener net.Listener) (net.Listener, error) {
	proxySettings := *settings
	proxySettings.Clock = clock
	return func(listener net.Listener) (net.Listener, error) {
		return tlsedgeservice.NewProxyHeaderListener(ctx, &servicePortListener{Listener: listener, servicePort: servicePort}, &proxySettings)
	}
}

// A block that could not start: never ready, serving nothing but /status.
type notReadyEngine struct{}

func (notReadyEngine) Ready() bool { return false }
func (notReadyEngine) Drain()      {}
func (notReadyEngine) Close()      {}

// What a block's main, or a test, gives Run.
type RunOptions struct {
	// GET /status's port: 80 in a block.
	Port int
	// A listener already bound for /status, which Run serves in place of binding Port: a test's on 127.0.0.1:0, so
	// the OS picks the port. Run takes it. nil: Port, as warp maps it.
	StatusListener net.Listener
	// A listener already bound for each of HostServicePorts the block serves, by that service port, which Run serves
	// behind its PROXY listener in place of binding the port: a test's on 127.0.0.1:0, still answering the service
	// port to the engine. Run takes them. A port with none is bound as warp maps it.
	Listeners map[int]net.Listener
	// The engine's settings and the block's. nil: the block could not read its tlsedge.yml, and serves /status
	// not ready.
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

// What sidecarhost.Run takes for the engine: the four ports, each behind its own PROXY listener, on options'
// bound listener for it where there is one. No Postgres check: the block's role reads only the label map's and the
// kill switch's tables (plan-TLS-edge-data-source), not the migration audit the shared check reads, and a store
// that cannot answer leaves the engine not ready.
func hostRunOptions(ctx context.Context, engine *Host, options *RunOptions) *sidecarhost.RunOptions {
	runOptions := &sidecarhost.RunOptions{
		Service:        hostService,
		StatusPort:     options.Port,
		StatusListener: options.StatusListener,
		Engine:         engine,
		Stream:         engine,
		WarpStatus:     options.WarpStatus,
		Settings:       engine.settings.Run,
	}
	for _, servicePort := range HostServicePorts {
		runOptions.Listeners = append(runOptions.Listeners, &sidecarhost.RunListener{
			Port:     servicePort,
			Listener: options.Listeners[servicePort],
			Wrap:     newProxyListenerWrap(ctx, servicePort, engine.settings.TlsEdge.ProxyHeader, engine.host.ClockOrReal()),
		})
	}
	return runOptions
}

// Runs the block until ctx ends through sidecarhost.Run (§19.4): GET /status from the first moment, the four
// ports once ready, then at ctx's end the drain and Close. A block that cannot start logs why and serves /status
// not ready until ctx ends, and closes the bound listeners it was given.
func Run(ctx context.Context, options *RunOptions) error {
	if options.Settings != nil {
		host, err := NewBlockHost(ctx, options.Settings, options.Deps)
		if err == nil {
			if options.ProcessEnv != nil {
				options.ProcessEnv(host.host)
			}
			// on the host's own context, so the listeners outlive ctx until the drain closes them
			return sidecarhost.Run(ctx, hostRunOptions(host.ctx, host, options))
		}
		glog.Errorf("[%s]not ready: %v\n", hostService, err)
	}
	if options.ProcessEnv != nil {
		options.ProcessEnv(&sidecar.ServiceHost{})
	}
	for _, listener := range options.Listeners {
		listener.Close()
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
