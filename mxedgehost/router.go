package mxedgehost

// The router role (MAIL-M7-19; §15.4, §19.4, D-MAIL-1, D-MAIL-2, I-12, I-13): the mxedge warp service on
// every lb interface but rpt.bringyour.com's two, mxedgeservice's router on TCP 25 behind the lb's PROXY
// header. It holds the platform and feed credentials and never the platform-mail key, which only the
// platform block mounts: no secret file of the platform role is read here.
//
//   - ServiceHost: sidecarhost's, with Quota over api's internal gate (quotahost.NewBlockQuota on the mxedge
//     bearer) and ServiceClient, server.NewPlatformClientSource on the edge bearer, which server/cli/mxedge
//     builds and hands in as RouterDeps.
//   - The store: model.NewMxEdgeStore, read only by the engine's probe and its 10 s record batches, never by
//     a session (I-12), so the block holds a Postgres pool and needs MG7.
//   - The dialer: K platform clients in urnetwork-edge (sidecar.NewEdgeClientPool) made in the background,
//     tried again each ClientsRetryInterval until the source answers, and sidecar.NewSocketEdgeDialer over
//     them with Find at the sockets service, adapted to mxedgeservice.MxEdgeDialer (routerDialer). Until
//     the clients exist the engine holds no client id and is not ready.
//   - The PTR: the host's own resolver, net.DefaultResolver unless RouterDeps gives one.
//   - sidecar.yml's mxedge gate through sidecarhost's Gates, reread every 60 s: the service gate at each
//     hello and plaintext transaction, and its network list once SNI names a network.
//
// Readiness is §19.4's mxedge column: every seam given, the engine ready (a probe found the store and
// today's k), and all K clients connected. Nothing here logs an address, a name or a network (I-13).

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The router block as urnetwork_<svc>_* and its log name it.
const hostRouterService = "mxedge"

// The router's tunables beyond mxedge.yml's.
type RouterSettings struct {
	// The pool's. Its ClientCount is mxedge.yml's clients.
	Clients *sidecar.EdgeClientPoolSettings
	// How long the block waits before it makes its clients again after the source could not answer: 5 s.
	ClientsRetryInterval time.Duration
}

// The defaults, the pool's K taken from mxedge.yml's clients when settings are given.
func DefaultRouterSettings(mxEdge *mxedgeservice.MxEdgeSettings) *RouterSettings {
	clients := sidecar.DefaultEdgeClientPoolSettings()
	if mxEdge != nil {
		clients.ClientCount = mxEdge.Clients
	}
	return &RouterSettings{
		Clients:              clients,
		ClientsRetryInterval: 5 * time.Second,
	}
}

// The process's K clients, as sidecar.EdgeClientPool is.
type RouterClients interface {
	Clients() []*sidecar.SocketEdgeClient
	ClientIds() []sidecar.Id
	// Whether all K are connected now.
	Ready() bool
	Close()
}

// One Find and open through sidecar.SocketEdgeDialer.
type RouterSocketDial func(ctx context.Context, request *sidecar.SocketEdgeDialRequest) (*sidecar.SocketEdgeStream, error)

// What the router block is given: what server/cli/mxedge builds over api and the sockets service, or a
// test's fakes. A nil Quota, Store, NewClients or NewDial leaves the block not ready, and said.
type RouterDeps struct {
	// ServiceHost.Quota: quotahost.NewBlockQuota over api's internal gate on the mxedge bearer.
	Quota sidecar.Quota
	// ServiceHost.ServiceClient: server.NewPlatformClientSource on the edge bearer.
	Source sidecar.ServiceClientSource
	// The engine's store: model.NewMxEdgeStore.
	Store mxedgeservice.MxEdgeStore
	// The pending groups Store reads: MG7.
	StoreGroups []*sidecarhost.RunGroup
	// Makes the block's K clients: sidecar.NewEdgeClientPool on Source. Called again until it answers.
	NewClients func(ctx context.Context) (RouterClients, error)
	// Find and open over the clients: sidecar.NewSocketEdgeDialer.
	NewDial func(clients RouterClients) (RouterSocketDial, error)
	// The PTR's resolver. nil: net.DefaultResolver.
	LookupPtr  func(ctx context.Context, sender netip.Addr) ([]string, error)
	LookupHost func(ctx context.Context, name string) ([]netip.Addr, error)
	// Each replaces the block's: a test's gates, a test's Redis.
	Gates sidecar.ServiceGates
	Redis sidecar.Redis
	// The engine's clock. nil: real time.
	Clock sidecar.Clock
	// The engine's log. nil: glog.
	Log sidecar.Logger
	// The block's /status latch, so a test reads it. nil: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
}

// The clients and the dial over them, once both exist.
type routerDialerState struct {
	clients RouterClients
	dial    RouterSocketDial
}

// mxedgeservice.MxEdgeDialer over the block's clients, which come after the engine: until they do it holds
// no client id, so the engine is not ready, and every dial answers unavailable. Safe for concurrent use.
type routerDialer struct {
	stateLock sync.Mutex
	state     *routerDialerState
}

var _ mxedgeservice.MxEdgeDialer = (*routerDialer)(nil)

func (self *routerDialer) current() *routerDialerState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.state
}

func (self *routerDialer) set(state *routerDialerState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.state = state
}

// See mxedgeservice.MxEdgeDialer.
func (self *routerDialer) ClientIds() []sidecar.Id {
	state := self.current()
	if state == nil {
		return nil
	}
	return state.clients.ClientIds()
}

// See mxedgeservice.MxEdgeDialer.
func (self *routerDialer) Ready() bool {
	state := self.current()
	return state != nil && state.clients.Ready()
}

// See mxedgeservice.MxEdgeDialer: the engine's request as SocketEdgeDialer's, protocol smtp-starttls,
// the whole hello as early data, which OpenStream sends as its first 2,048 B in StreamOpen and the rest in
// StreamData right behind it, before the accept (§15.4).
func (self *routerDialer) Dial(ctx context.Context, request *mxedgeservice.MxEdgeDialRequest) (mxedgeservice.MxEdgeStream, uint64, error) {
	state := self.current()
	if state == nil || request == nil {
		return nil, 0, &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}
	}
	stream, err := state.dial(ctx, &sidecar.SocketEdgeDialRequest{
		NetworkId:    request.NetworkId,
		NetworkLabel: request.NetworkLabel,
		Name:         request.Name,
		Protocol:     mxedgeservice.MxFindProtocol,
		Metadata:     request.Metadata,
		EarlyData:    request.HelloBytes,
	})
	if err != nil {
		return nil, 0, err
	}
	if stream == nil {
		return nil, 0, &sidecar.SocketCodeError{Err: sidecar.ErrSocketUnavailable}
	}
	return stream, stream.Ticket().GetGrantExpireTimeMillis(), nil
}

// The router role as the block runs it. Safe for concurrent use.
type RouterHost struct {
	ctx      context.Context
	cancel   context.CancelFunc
	host     *sidecar.ServiceHost
	settings *RouterSettings
	deps     *RouterDeps
	engine   *mxedgeservice.MxEdgeService
	dialer   *routerDialer
	log      sidecar.Logger
	// what the engine routes no session without and the block was not given, each said once
	missing []string
	group   sync.WaitGroup
	// closes once the clients and the dial over them exist, or the block gave up on them
	clientsMade chan struct{}

	stateLock sync.Mutex
	closed    bool
}

var _ sidecar.StreamEngine = (*RouterHost)(nil)

// The host's resolver as the engine's PTR lookups.
func hostLookupPtr(ctx context.Context, sender netip.Addr) ([]string, error) {
	return net.DefaultResolver.LookupAddr(ctx, sender.String())
}

func hostLookupHost(ctx context.Context, name string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
}

// The router on its host, store and dialer, with the clients made in the background, all on a context of
// its own that Close ends, so ctx's end, a block's SIGTERM, starts the drain without ending what the drain
// needs. A missing seam leaves the block not ready, never an error, so it serves /status and says why;
// settings the engine refuses are an error.
func NewRouterHost(ctx context.Context, settings *HostSettings, routerSettings *RouterSettings, deps *RouterDeps) (*RouterHost, error) {
	if settings == nil || settings.MxEdge == nil {
		return nil, errors.New("mxedge router: no mxedge.yml")
	}
	if err := settings.MxEdge.ValidateRouter(); err != nil {
		return nil, err
	}
	if routerSettings == nil {
		routerSettings = DefaultRouterSettings(settings.MxEdge)
	}
	if routerSettings.Clients == nil || routerSettings.ClientsRetryInterval <= 0 {
		return nil, errors.New("mxedge router: the clients' settings and retry interval")
	}
	if routerSettings.Clients.ClientCount != settings.MxEdge.Clients {
		return nil, errors.New("mxedge router: the pool's client count is not mxedge.yml's clients")
	}
	if deps == nil {
		deps = &RouterDeps{}
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
		Clock:         deps.Clock,
	})
	if deps.Gates != nil {
		host.Gates = deps.Gates
	}
	if deps.Redis != nil {
		host.Redis = deps.Redis
	}
	self := &RouterHost{
		ctx:         hostCtx,
		cancel:      cancel,
		host:        host,
		settings:    routerSettings,
		deps:        deps,
		dialer:      &routerDialer{},
		log:         log,
		clientsMade: make(chan struct{}),
	}
	if deps.Quota == nil {
		self.missing = append(self.missing, "ServiceHost.Quota, api's internal quota surface (QD-055)")
	}
	if deps.Store == nil {
		self.missing = append(self.missing, "the store, mail_connection_record in Postgres (MAIL-M7-21)")
	}
	if deps.NewClients == nil || deps.NewDial == nil {
		self.missing = append(self.missing, "the platform clients, api's edge client route on the edge bearer (QD-812)")
	}
	lookupPtr, lookupHost := deps.LookupPtr, deps.LookupHost
	if lookupPtr == nil || lookupHost == nil {
		lookupPtr, lookupHost = hostLookupPtr, hostLookupHost
	}
	engineDeps := NewEngineDeps(host, deps.Log)
	engine, err := mxedgeservice.NewMxEdgeRouterService(hostCtx, host, deps.Store, engineDeps, &mxedgeservice.MxEdgeRouterDeps{
		Dialer:     self.dialer,
		LookupPtr:  lookupPtr,
		LookupHost: lookupHost,
	}, settings.MxEdge)
	if err != nil {
		cancel()
		return nil, err
	}
	self.engine = engine
	if 0 < len(self.missing) {
		log.Errorf("[%s]not ready without %s", hostRouterService, strings.Join(self.missing, "; "))
	}
	if deps.NewClients != nil && deps.NewDial != nil {
		self.group.Add(1)
		go self.runClients()
	} else {
		close(self.clientsMade)
	}
	return self, nil
}

// Makes the block's clients and the dial over them, again each ClientsRetryInterval until both exist or
// the block closes: the source answers nothing until api does.
func (self *RouterHost) runClients() {
	defer self.group.Done()
	defer close(self.clientsMade)
	for {
		state, err := func() (*routerDialerState, error) {
			clients, err := self.deps.NewClients(self.ctx)
			if err != nil {
				return nil, err
			}
			dial, err := self.deps.NewDial(clients)
			if err != nil {
				clients.Close()
				return nil, err
			}
			return &routerDialerState{clients: clients, dial: dial}, nil
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
			return
		}
		if self.ctx.Err() != nil {
			return
		}
		// the pool's and the source's errors name an index, a part or a status, never a secret or an address
		self.log.Warningf("[%s]no platform clients yet: %v", hostRouterService, err)
		select {
		case <-self.ctx.Done():
			return
		case <-self.host.ClockOrReal().After(self.settings.ClientsRetryInterval):
		}
	}
}

// The engine, for a test that drives it.
func (self *RouterHost) Engine() *mxedgeservice.MxEdgeService {
	return self.engine
}

// What the block was not given that the engine routes no session without: empty once every seam is there.
func (self *RouterHost) Missing() []string {
	return append([]string{}, self.missing...)
}

// Closes once the clients and their dial exist, or the block stopped trying.
func (self *RouterHost) ClientsMade() <-chan struct{} {
	return self.clientsMade
}

// §19.4's mxedge column: every seam given, and the engine ready, which holds the store's and k's probe and
// all K clients connected.
func (self *RouterHost) Ready() bool {
	if 0 < len(self.missing) {
		return false
	}
	closed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.closed
	}()
	return !closed && self.engine.Ready()
}

// See sidecar.StreamEngine.
func (self *RouterHost) Serve(listener net.Listener) error {
	return self.engine.Serve(listener)
}

// See sidecar.Engine: the engine's, which resets what is still spliced once its wait is over and sends the
// last usage window.
func (self *RouterHost) Drain() {
	self.engine.Drain()
}

// Ends the engine, the block's loops and its clients. Safe after Drain, and more than once.
func (self *RouterHost) Close() {
	closed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.closed = true
		return false
	}()
	if closed {
		return
	}
	self.cancel()
	self.engine.Close()
	self.group.Wait()
	if state := self.dialer.current(); state != nil {
		self.dialer.set(nil)
		state.clients.Close()
	}
}

// What sidecarhost.Run takes for the router block: TCP 25 behind the PROXY wrap, the store's Postgres pool
// and its groups. ctx bounds the PROXY listener.
func routerRunOptions(ctx context.Context, engine sidecar.StreamEngine, settings *HostSettings, deps *RouterDeps, ports *HostPorts) *sidecarhost.RunOptions {
	clock := deps.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	proxySettings := settings.ProxyHeader
	if proxySettings == nil {
		proxySettings = DefaultHostSettings().ProxyHeader
	}
	return &sidecarhost.RunOptions{
		Service:    hostRouterService,
		StatusPort: ports.Status,
		Engine:     engine,
		Stream:     engine,
		Listeners: []*sidecarhost.RunListener{{
			Port: ports.Smtp,
			Wrap: NewProxyListenerWrap(ctx, proxySettings, clock),
		}},
		Postgres:   true,
		Groups:     deps.StoreGroups,
		WarpStatus: deps.WarpStatus,
	}
}

// Runs the router as a block until ctx ends, through sidecarhost.Run (§19.4): GET /status from the first
// moment, TCP 25 behind the PROXY wrap once ready, then at ctx's end the drain and Close. A router that
// cannot start logs why and serves /status not ready.
func RunRouterHost(ctx context.Context, settings *HostSettings, routerSettings *RouterSettings, deps *RouterDeps, ports *HostPorts) error {
	if settings == nil {
		settings = DefaultHostSettings()
	}
	if deps == nil {
		deps = &RouterDeps{}
	}
	routerHost, err := NewRouterHost(ctx, settings, routerSettings, deps)
	if err != nil {
		glog.Errorf("[%s]the router role is not ready: %v\n", hostRouterService, err)
		return sidecarhost.Run(ctx, &sidecarhost.RunOptions{
			Service:    hostRouterService,
			StatusPort: ports.Status,
			Engine:     notReadyStreamEngine{},
			WarpStatus: deps.WarpStatus,
		})
	}
	// on the host's own context, so the listener outlives ctx until the drain closes it
	return sidecarhost.Run(ctx, routerRunOptions(routerHost.ctx, routerHost, settings, deps, ports))
}

// MG7, the group the router's store reads (§19.7).
func RouterStoreGroups() []*sidecarhost.RunGroup {
	return []*sidecarhost.RunGroup{{Name: "MG7", Linked: server.SidecarMg7Linked}}
}
