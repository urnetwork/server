// server/cli/tlsedge (TLS-M5-11; R-TLS-2, D-TLS-1, D-TLS-3, D-TLS-4, D-TLS-9, I-3, I-12, I-13, §13.5, §13.6, §19.4):
// the tlsedge engine, §13.5's layer-4 router by SNI, one warp block on every lb host, since each lb proxies only
// to its own blocks, run by sidecarhost.Run with GET /status on port 80, {vault: no, config: yes, site: no} and
// user 65532. It mounts no vault and holds no TLS key (D-TLS-1): its secret_files hold redis.yml, the restricted
// Postgres role the label map and the kill rows are read under (plan-TLS-edge-data-source), its internal gate
// bearer for the quota surface (internal-gate-tlsedge.yml), the edge bearer its platform clients are minted on
// (internal-gate-edge.yml, QD-812) and grafana-push.yml.
//
// What the block builds around tlsedgeservice:
//
//   - ServiceHost: sidecarhost's, with Quota over api's internal gate at -api_url (quotahost.NewBlockQuota) and
//     ServiceClient, server.NewPlatformClientSource over api's edge client route on the edge bearer.
//   - The store: model.NewTlsEdgeStore on the block's Postgres role, read only by the engine's loops and record
//     batches, never by a connection (I-12).
//   - The clients: sidecar.NewEdgeClientPool, K platform clients in urnetwork-edge, Required and relay-only,
//     providing Service alone (D-TLS-4, I-3). The pool is made once the source answers every index, tried again
//     each ClientsRetryInterval until then.
//   - The dialer: sidecar.NewSocketEdgeDialer over the pool's clients and the sockets service at -sockets_url,
//     each Find on the first client's own jwt, as tlsedgeservice.TlsEdgeDialer (hostDialer).
//   - One key-event subscriber per process (sidecarhost's), never one per client or network: {tes_*}n, the family
//     model.TlsSuspensionKeyEventPattern names, reaches KillChanged, and after a resubscription KillsResync runs,
//     since events in the gap are gone. The engine reloads every kill each 60 s besides.
//   - config/<env>/sidecar.yml through sidecarhost's Gates, reread every 60 s: the engine reads the tlsedge gate
//     per connection, and a closed gate answers the visitor's hello with internal_error while the block stays
//     ready (§19.11).
//   - One tlsedgeservice.NewProxyHeaderListener for each of TCP 443, 8443, 8883 and 5671 and no other port, with
//     Serve on each (owner decisions 68 and 121).
//
// Readiness is §19.4's tlsedge column: every seam given, the {tes_} family subscribed, the engine ready (the label
// map's first snapshot read, the kill rows and keys read) and all K clients connected. The block never serves on
// an empty map: a store that cannot answer, a binary that does not link the label map's migrations, or a
// tlsedge.yml the engine refuses leaves it serving /status not ready and nothing else.
//
// What this draft does not do as §13.5 words it: a hello past sidecar.MaxStreamEarlyDataByteCount has its rest
// written once the listener has accepted, not before, since SocketEdgeDialer.Dial returns only then.
package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecarhost"
)

// The service as urnetwork_<svc>_* and the block's log name it, and the block as config/<env>/edge.yml lists it.
const hostService = "tlsedge"

// The block's file under config/<env>.
const hostSettingsResourceName = "tlsedge.yml"

// §9.7's protocol filter for every Find the block sends.
const hostFindProtocol = "tls"

// The ports tlsedge serves, and no other (owner decisions 68 and 121).
var hostServicePorts = []int{443, 8443, 8883, 5671}

// The block's tunables beyond the engine's.
type hostSettings struct {
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
}

// The defaults over the engine's settings.
func defaultHostSettings(tlsEdge *tlsedgeservice.TlsEdgeSettings) *hostSettings {
	clients := sidecar.DefaultEdgeClientPoolSettings()
	if tlsEdge != nil {
		clients.ClientCount = tlsEdge.Clients
	}
	return &hostSettings{
		TlsEdge:                tlsEdge,
		Clients:                clients,
		ClientsRetryInterval:   5 * time.Second,
		KeyEventsRetryInterval: 5 * time.Second,
		EarlyDataMaxByteCount:  sidecar.MaxStreamEarlyDataByteCount,
	}
}

// The block's K clients, as sidecar.EdgeClientPool is.
type hostClients interface {
	Clients() []*sidecar.SocketEdgeClient
	ClientIds() []sidecar.Id
	// Whether all K are connected now.
	Ready() bool
	Close()
}

// The seams a test, or the change that lands a missing one, fills. A nil Quota, Store, NewClients or NewDialer
// leaves the block not ready, and said.
type hostDeps struct {
	// ServiceHost.Quota: quotahost.NewBlockQuota over api's internal gate.
	Quota sidecar.Quota
	// ServiceHost.ServiceClient: server.NewPlatformClientSource.
	Source sidecar.ServiceClientSource
	// The engine's store: model.NewTlsEdgeStore.
	Store tlsedgeservice.TlsEdgeStore
	// Makes the block's K clients: sidecar.NewEdgeClientPool on Source. Called again until it answers.
	NewClients func(ctx context.Context) (hostClients, error)
	// Find and open over the clients, as the engine's request: sidecar.NewSocketEdgeDialer.
	NewDial func(clients hostClients) (hostSocketDial, error)
	// Each replaces the block's: a test's gates file, a test's Redis.
	Gates sidecar.ServiceGates
	Redis sidecar.Redis
	// The engine's clock: real time.
	Clock sidecar.Clock
	// Where the block logs: glog. Indexes and counts only (I-13).
	Log sidecar.Logger
	// What the key events reach: the engine. A test records them instead.
	Events hostEngineEvents
}

// What the block's key-event subscriber calls on the engine, neither blocking nor doing I/O.
type hostEngineEvents interface {
	KillChanged(networkId sidecar.Id)
	KillsResync()
}

// One Find and open through sidecar.SocketEdgeDialer, its stream as the engine holds it.
type hostSocketDial func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error)

// The clients and the dial over them, once both exist.
type hostDialerState struct {
	clients hostClients
	dial    hostSocketDial
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
		AffinityKey:  request.AffinityKey,
		Metadata:     request.Metadata,
		EarlyData:    earlyData,
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
type tlsEdgeHost struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     *sidecar.ServiceHost
	settings *hostSettings
	deps     *hostDeps
	engine   *tlsedgeservice.TlsEdgeService
	dialer   *hostDialer
	events   hostEngineEvents
	log      sidecar.Logger
	// what the engine routes no connection without and the block was not given, each said once
	missing []string
	group   sync.WaitGroup

	// closes once the clients and the dial over them exist: what a test waits on before it asks Ready
	clientsMade chan struct{}

	stateLock sync.Mutex
	// what removes the key-event family's subscription
	removeKeyEvents []func()
	keyEventsReady  bool
	closed          bool
}

var _ sidecar.StreamEngine = (*tlsEdgeHost)(nil)

// The engine on its host, store and dialer, with the key events subscribed on the host's Redis and the clients
// made in the background, all on a context of its own that Close ends, so ctx's end, a block's SIGTERM, starts the
// drain without ending what the drain needs. A missing seam leaves the block not ready, never an error, so it
// serves /status and says why.
func newTlsEdgeHost(ctx context.Context, settings *hostSettings, deps *hostDeps) (*tlsEdgeHost, error) {
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
		deps = &hostDeps{}
	}
	log := deps.Log
	if log == nil {
		log = hostLogger{}
	}
	hostCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	host := sidecarhost.NewServiceHost(hostCtx, &sidecarhost.ServiceHostOptions{
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
	self := &tlsEdgeHost{
		ctx:         hostCtx,
		cancel:      cancel,
		host:        host,
		settings:    settings,
		deps:        deps,
		dialer:      &hostDialer{earlyDataMaxByteCount: settings.EarlyDataMaxByteCount},
		log:         log,
		clientsMade: make(chan struct{}),
	}
	if deps.Quota == nil {
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

// Subscribes the {tes_*}n family on the host's one subscriber, and whether it is held.
func (self *tlsEdgeHost) subscribe() bool {
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
func (self *tlsEdgeHost) resubscribe() {
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
func (self *tlsEdgeHost) runClients() {
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
func (self *tlsEdgeHost) Missing() []string {
	return append([]string{}, self.missing...)
}

// §19.4's tlsedge column: every seam given, the key events held, the engine ready and all K clients connected.
func (self *tlsEdgeHost) Ready() bool {
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
func (self *tlsEdgeHost) Serve(listener net.Listener) error {
	return self.engine.Serve(listener)
}

// The engine takes no new connection and waits out the ones it has, up to drain_s.
func (self *tlsEdgeHost) Drain() {
	self.engine.Drain()
}

// Ends the key events, the engine, the block's loops and its clients. Safe after Drain, and more than once.
func (self *tlsEdgeHost) Close() {
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

// tlsedge.yml from config. A file the engine refuses, main's without its blocks among them, starts no engine.
func loadTlsEdgeSettings() (*tlsedgeservice.TlsEdgeSettings, error) {
	resource, err := server.Config.SimpleResource(hostSettingsResourceName)
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return tlsedgeservice.LoadTlsEdgeSettings(data)
}

// The pool as the block's clients.
type poolClients struct {
	*sidecar.EdgeClientPool
}

// The block's own deps: Quota over api's internal gate at apiUrl on the tlsedge bearer, the store on the block's
// Postgres role, the source on the edge bearer, and the pool and the dialer over it with Find at socketsUrl. Each
// is left out, and said, when what it needs is missing. quotaCtx outlives the drain, so the open usage window is
// sent. The returned call drains the quota and ends the source.
func blockDeps(quotaCtx context.Context, settings *hostSettings, apiUrl string, socketsUrl string) (*hostDeps, func()) {
	deps := &hostDeps{Store: model.NewTlsEdgeStore(nil)}
	ends := []func(){}
	end := func() {
		for _, end := range ends {
			end()
		}
	}
	if quota, err := quotahost.NewBlockQuota(quotaCtx, server.InternalGateCallerTlsedge, apiUrl, nil); err != nil {
		// the error names what is missing, never the bearer
		glog.Errorf("[%s]no ServiceHost.Quota: %v\n", hostService, err)
	} else {
		deps.Quota = quota
		ends = append(ends, func() { quota.Drain(context.Background()) })
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerEdge)
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	blockHost, err := server.Host()
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	sourceSettings := server.DefaultPlatformClientSourceSettings()
	sourceSettings.ClientCount = settings.TlsEdge.Clients
	source, err := server.NewPlatformClientSource(quotaCtx, &server.PlatformClientSourceArgs{
		ApiUrl:   apiUrl,
		Block:    hostService,
		Host:     blockHost,
		Bearer:   bearer,
		Settings: sourceSettings,
	})
	if err != nil {
		glog.Errorf("[%s]no platform clients: %v\n", hostService, err)
		return deps, end
	}
	ends = append(ends, source.Close)
	if socketsUrl == "" {
		glog.Errorf("[%s]no platform clients: no sockets url for Find\n", hostService)
		return deps, end
	}
	deps.Source = source
	deps.NewClients = func(ctx context.Context) (hostClients, error) {
		pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
			Source:   source,
			Settings: settings.Clients,
			Log:      hostLogger{},
		})
		if err != nil {
			return nil, err
		}
		return poolClients{EdgeClientPool: pool}, nil
	}
	deps.NewDial = func(clients hostClients) (hostSocketDial, error) {
		dialer, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
			Clients: clients.Clients(),
			Find: &sidecar.SocketFindHttpArgs{
				SocketsUrl: socketsUrl,
				Transport:  &http.Transport{},
				// a Find is the block's own call, on the first of its K clients' jwts
				Bearer: func(ctx context.Context) (string, error) {
					auth, err := source.ServiceClient(ctx, 0)
					if err != nil {
						return "", err
					}
					return auth.ByJwt, nil
				},
			},
			Settings: &sidecar.SocketEdgeDialSettings{
				FindTimeout:   settings.TlsEdge.FindTimeout,
				FindCount:     settings.TlsEdge.FindMaxCount,
				AcceptTimeout: settings.TlsEdge.AcceptTicketTimeout,
			},
		})
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (tlsedgeservice.TlsEdgeStream, error) {
			stream, err := dialer.Dial(ctx, request)
			if err != nil {
				// never a typed nil as the engine's stream
				return nil, err
			}
			return stream, nil
		}, nil
	}
	return deps, end
}

// What a block's main gives run.
type runOptions struct {
	// GET /status's port: 80 in a block.
	Port int
	// The engine's settings and the block's. nil: the block could not read its tlsedge.yml, and serves /status
	// not ready.
	Settings *hostSettings
	// nil: no seam, so never ready.
	Deps *hostDeps
}

// What sidecarhost.Run takes for the engine: the four ports, each behind its own PROXY listener. No Postgres
// check: the block's role reads only the label map's and the kill switch's tables (plan-TLS-edge-data-source),
// not the migration audit the shared check reads, and a store that cannot answer leaves the engine not ready.
func hostRunOptions(ctx context.Context, port int, engine *tlsEdgeHost) *sidecarhost.RunOptions {
	options := &sidecarhost.RunOptions{
		Service:    hostService,
		StatusPort: port,
		Engine:     engine,
		Stream:     engine,
	}
	for _, servicePort := range hostServicePorts {
		options.Listeners = append(options.Listeners, &sidecarhost.RunListener{
			Port: servicePort,
			Wrap: newProxyListenerWrap(ctx, servicePort, engine.settings.TlsEdge.ProxyHeader, engine.host.ClockOrReal()),
		})
	}
	return options
}

// Runs the block until ctx ends through sidecarhost.Run (§19.4): GET /status from the first moment, the four
// ports once ready, then at ctx's end the drain and Close. A block that cannot start logs why and serves /status
// not ready until ctx ends.
func run(ctx context.Context, options *runOptions) error {
	if options.Settings != nil {
		host, err := newTlsEdgeHost(ctx, options.Settings, options.Deps)
		if err == nil {
			// on the host's own context, so the listeners outlive ctx until the drain closes them
			return sidecarhost.Run(ctx, hostRunOptions(host.ctx, options.Port, host))
		}
		glog.Errorf("[%s]not ready: %v\n", hostService, err)
	}
	return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
		Service:    hostService,
		StatusPort: options.Port,
		Engine:     notReadyEngine{},
	})
}

var (
	port       = flag.Int("port", 80, "the block's status port")
	apiUrl     = flag.String("api_url", "", "api's base URL, whose internal gate the quota surface and the edge client route are read from; empty leaves the block not ready")
	socketsUrl = flag.String("sockets_url", "", "the sockets service's base URL, which every Find is sent to; empty leaves the block not ready")
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

	options := &runOptions{Port: *port}
	tlsEdgeSettings, err := loadTlsEdgeSettings()
	if err != nil {
		// a tlsedge.yml the engine refuses names the key, never a value
		glog.Errorf("[%s]%s: %v\n", hostService, hostSettingsResourceName, err)
	} else {
		options.Settings = defaultHostSettings(tlsEdgeSettings)
		// on a context of its own, so a SIGTERM's drain can still send the open usage window, ended after the block
		quotaCtx, quotaCancel := context.WithCancel(context.WithoutCancel(ctx))
		defer quotaCancel()
		deps, end := blockDeps(quotaCtx, options.Settings, *apiUrl, *socketsUrl)
		defer end()
		options.Deps = deps
	}
	if err := run(ctx, options); err != nil {
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
