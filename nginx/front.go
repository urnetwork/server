package nginx

// The front's API (LBALT-IMPL.md §2.7) and its assembly (owner: s-http).
// One Front per connect process serves every interface.
//
// NewFront builds the production parts: the name matcher, the two TLS
// configs, the body store and an upstream group per service. newFront takes
// them through interfaces (frontDependencies), so the routing, limits and
// responses can be tested with fakes. The front builds its routing table and
// one handler per (interface, scheme) once, and returns those cached
// handlers.
//
// Lifecycle: BeginDrain marks the front draining, so responses close their
// client connections (h1 Connection: close, which Go's h2 server turns into
// a GOAWAY), and arms the drain deadline, HttpSettings.DrainTimeout, after
// which whatever outlived the drain is cut: hijacked connections, the
// forwarders' requests and tunnels, and the body store's spill. BeginDrain
// never blocks. Close cuts at once if the deadline hasn't, waits at most
// HttpSettings.ForcedCloseTimeout for the cut to complete, and ends the
// front's goroutines; it is safe while handlers still run, which then fail
// on what was cut, and later requests are answered 503. Connect sends the
// server level GOAWAYs and closes idle keepalive connections itself (§2.8).

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// How the front treats a served name. Names match by nginx's server_name
// precedence: an exact name first, then the longest leading wildcard.
type NameClass int

const (
	// not a name the route table serves
	NameClassUnknown NameClass = iota
	// an lb name: lb status and the hidden per-block routes. The first lb
	// name is the TCP default server.
	NameClassLb
	// the connect service: served in-process on every protocol, never
	// forwarded
	NameClassConnect
	// the api service: forwarded, and admitted on h3/whodis
	NameClassApi
	// every other fronted service
	NameClassForwarded
	// a redirect alias: a 301 to its target
	NameClassRedirect
)

// A short lowercase name, bounded for logs and metric labels. A value outside
// the constants is "unknown".
func (self NameClass) String() string {
	switch self {
	case NameClassLb:
		return "lb"
	case NameClassConnect:
		return "connect"
	case NameClassApi:
		return "api"
	case NameClassForwarded:
		return "forwarded"
	case NameClassRedirect:
		return "redirect"
	default:
		return "unknown"
	}
}

// Host normalization and name matching, as routing needs them.
type frontNames interface {
	// nginx's $host for an authority, or false for an invalid one
	normalizeHost(host string) (string, bool)
	// the served name a normalized host matches, or false
	match(name string) (nameMatch, bool)
}

// frontNames over the route table's matcher, as NewFront builds it.
type matcherFrontNames struct {
	matcher *nameMatcher
}

// See frontNames.
func (self *matcherFrontNames) normalizeHost(host string) (string, bool) {
	return normalizeHost(host)
}

// See frontNames.
func (self *matcherFrontNames) match(name string) (nameMatch, bool) {
	return self.matcher.match(name)
}

// What the front closes besides the forwarders: the body store.
type frontBodyStore interface {
	close() error
}

// The parts of a front NewFront builds and a test replaces.
type frontDependencies struct {
	names frontNames
	// one per service of the route table, by service name
	serviceForwarders map[string]serviceForwarder
	tlsConfig         *tls.Config
	quicTlsConfig     *tls.Config
	bodyStore         frontBodyStore
	metrics           *frontMetrics
	accessLog         frontAccessLog
	// the clock every decision reads
	now func() time.Time
}

// One interface's limits and handlers, built once.
type frontInterface struct {
	limits       *limitInterface
	httpsHandler http.Handler
	httpHandler  http.Handler
	h3ApiHandler http.Handler
}

// The lb status json (§2.6), in this field order.
type frontLbStatus struct {
	Version string `json:"version"`
	Status  string `json:"status"`
	Front   string `json:"front"`
}

// The lb_alt front of one connect process. It owns the name matcher, the two
// long-lived TLS configs, the upstream pools, the body store and the limit
// state, which is keyed by (interface name, normalized client IP), so one
// source on two interfaces never shares a quota. Its methods are safe for
// concurrent use, and so are the handlers they return.
type Front struct {
	ctx      context.Context
	cancel   context.CancelFunc
	revision string
	settings *FrontSettings

	names             frontNames
	tlsConfig         *tls.Config
	quicTlsConfig     *tls.Config
	connectHandler    http.Handler
	serviceForwarders map[string]serviceForwarder
	bodyStore         frontBodyStore
	metrics           *frontMetrics
	accessLog         frontAccessLog
	clock             func() time.Time

	routes       *routeHttpTable
	limits       *frontLimits
	interfaces   map[string]*frontInterface
	lbStatusBody []byte
	gzipWriters  sync.Pool
	// requests the access log has considered, for its sampling
	accessLogCount atomic.Uint64

	draining atomic.Bool
	closed   atomic.Bool

	stateLock     sync.Mutex
	hijackedConns map[*responseHijackedConn]bool
	cutStarted    bool
	// closed when the cut has completed
	cutDone chan struct{}
	// the body store's close error, set by the cut before cutDone closes
	cutErr    error
	closeOnce sync.Once
	closeErr  error
}

// Builds the front from a loaded route table and its certificates; revision
// is the route file's, from LoadRouteTable. connectHandler serves connect
// authorities in-process (the websocket and the H1+ upgrade): the front
// applies the ingress rewrite and the D8 limits around it and never forwards
// a connect name. version is connect's WARP_VERSION, which the lb status
// route reports. The front's own goroutines end with ctx; Close releases what
// it holds.
func NewFront(ctx context.Context, routeTable *RouteTable, revision string, certificateStore *CertificateStore, connectHandler http.Handler, version string, settings *FrontSettings) (*Front, error) {
	if routeTable == nil {
		return nil, errors.New("lbalt front: no route table")
	}
	if certificateStore == nil {
		return nil, errors.New("lbalt front: no certificate store")
	}
	if err := checkFrontArguments(connectHandler, settings); err != nil {
		return nil, err
	}

	matcher := newNameMatcher(routeTable)
	tlsConfig, quicTlsConfig := newTlsConfigs(certificateStore, matcher, routeTable.Lb.Names[0], routeTable.Listen, settings.Tls, defaultFrontMetrics)
	bodyStore := newBodyStore(settings.Body)
	serviceForwarders := map[string]serviceForwarder{}
	for _, service := range routeTable.Services {
		serviceForwarders[service.Name] = newUpstreamGroup(routeTable, service, bodyStore, settings)
	}
	front, err := newFront(ctx, routeTable, revision, &frontDependencies{
		names: &matcherFrontNames{
			matcher: matcher,
		},
		serviceForwarders: serviceForwarders,
		tlsConfig:         tlsConfig,
		quicTlsConfig:     quicTlsConfig,
		bodyStore:         bodyStore,
		metrics:           defaultFrontMetrics,
		accessLog:         &glogFrontAccessLog{},
		now:               time.Now,
	}, connectHandler, version, settings)
	if err != nil {
		for _, serviceForwarder := range serviceForwarders {
			serviceForwarder.close()
		}
		bodyStore.close()
		return nil, err
	}
	front.metrics.certificateExpiry.WithLabelValues().Set(float64(certificateStore.EarliestExpiry().Unix()))
	return front, nil
}

// Refuses a missing handler, or settings missing a group.
func checkFrontArguments(connectHandler http.Handler, settings *FrontSettings) error {
	if connectHandler == nil {
		return errors.New("lbalt front: no connect handler")
	}
	if settings == nil || settings.Tls == nil || settings.Forward == nil || settings.Body == nil || settings.Limit == nil || settings.Http == nil {
		return errors.New("lbalt front: settings must set every group")
	}
	return nil
}

// Builds a front from its parts. NewFront passes the production ones.
func newFront(ctx context.Context, routeTable *RouteTable, revision string, dependencies *frontDependencies, connectHandler http.Handler, version string, settings *FrontSettings) (*Front, error) {
	if err := checkFrontArguments(connectHandler, settings); err != nil {
		return nil, err
	}
	routes, err := newRouteHttpTable(routeTable, dependencies.serviceForwarders)
	if err != nil {
		return nil, err
	}
	lbStatusBody, err := json.Marshal(&frontLbStatus{
		Version: version,
		Status:  "ok",
		Front:   "lbalt",
	})
	if err != nil {
		return nil, fmt.Errorf("lbalt front: status: %w", err)
	}

	frontCtx, cancel := context.WithCancel(ctx)
	front := &Front{
		ctx:               frontCtx,
		cancel:            cancel,
		revision:          revision,
		settings:          settings,
		names:             dependencies.names,
		tlsConfig:         dependencies.tlsConfig,
		quicTlsConfig:     dependencies.quicTlsConfig,
		connectHandler:    connectHandler,
		serviceForwarders: dependencies.serviceForwarders,
		bodyStore:         dependencies.bodyStore,
		metrics:           dependencies.metrics,
		accessLog:         dependencies.accessLog,
		clock:             dependencies.now,
		routes:            routes,
		limits:            newFrontLimits(frontCtx, settings.Limit, dependencies.now),
		interfaces:        map[string]*frontInterface{},
		lbStatusBody:      append(lbStatusBody, '\n'),
		hijackedConns:     map[*responseHijackedConn]bool{},
		cutDone:           make(chan struct{}),
	}
	gzipLevel := min(settings.Http.GzipLevel, gzip.BestCompression)
	front.gzipWriters.New = func() any {
		gzipWriter, err := gzip.NewWriterLevel(io.Discard, gzipLevel)
		if err != nil {
			// gzipLevel is checked positive before a writer is taken
			panic(err)
		}
		return gzipWriter
	}
	for _, routeInterface := range routeTable.Interfaces {
		frontInterface := &frontInterface{
			limits: newLimitInterface(routeInterface),
		}
		frontInterface.httpsHandler = &routeHttpHandler{
			front:          front,
			frontInterface: frontInterface,
			scheme:         routeHttpSchemeHttps,
		}
		frontInterface.httpHandler = &routeHttpHandler{
			front:          front,
			frontInterface: frontInterface,
			scheme:         routeHttpSchemeHttp,
		}
		frontInterface.h3ApiHandler = &routeHttpHandler{
			front:          front,
			frontInterface: frontInterface,
			scheme:         routeHttpSchemeH3Api,
		}
		front.interfaces[routeInterface.Name] = frontInterface
	}
	return front, nil
}

// The class of a name, normalized first as nginx's $host is.
// NameClassUnknown when the table doesn't serve it. Connect dispatches QUIC
// by it: connect names to its own handler, api names to H3ApiHandler, and
// any other name is closed with 0x1000.
func (self *Front) ClassifyName(name string) NameClass {
	host, ok := self.names.normalizeHost(name)
	if !ok {
		return NameClassUnknown
	}
	hostMatch, ok := self.names.match(host)
	if !ok {
		return NameClassUnknown
	}
	return hostMatch.class
}

// The base TLS config for every TCP 443 socket: one long-lived config, so
// session tickets resume across the block's sockets (D6c).
// GetConfigForClient picks the certificate by SNI, the default server's for
// a missing or unknown one, and ALPN by class: http/1.1 only for connect
// names, h2 and http/1.1 for the rest. NewTlsListener and http.Server.Serve
// share this configuration across sockets.
func (self *Front) TlsConfig() *tls.Config {
	return self.tlsConfig
}

// The base TLS config for every QUIC socket: one long-lived config, for
// connect and api names, with alt's ALPN rule (the client's offer intersected
// with [h3], so raw-QUIC connect without ALPN still passes). The public QUIC
// config that uses it sets Allow0RTT false. It counts each ClientHello by
// port kind, the class of its name and whether it offered h3, in
// urnetwork_front_quic_client_hellos_total (newTlsConfigs).
func (self *Front) QuicTlsConfig() *tls.Config {
	return self.quicTlsConfig
}

// The TCP 443 handler for one interface. It routes each request by its
// authority (Host on h1, each stream's :authority on h2), not by SNI, with
// nginx's server_name semantics, and carries everything per request: the
// ingress rewrite, the D8 limits, body handling, forwarding with retries,
// redirects, the /status deny and the added response headers. It expects
// r.RemoteAddr to be the accepted peer and r.TLS to be set.
//
// The handler applies nginx's header limits itself, and its keepalive_requests
// and keepalive_time, on a server ConfigureServer has set up. An interface
// the route table doesn't list gets a handler that answers 503.
func (self *Front) HttpsHandler(interfaceName string) http.Handler {
	frontInterface, ok := self.lookupInterface(interfaceName)
	if !ok {
		return self.unknownInterfaceHandler(interfaceName)
	}
	return frontInterface.httpsHandler
}

// The TCP 80 handler for one interface: on lb names the lb status routes,
// which answer 200 with the status json and never redirect (Route53's health
// check depends on it), and otherwise a 301 to https for every served name
// and a deny for any other. It expects r.RemoteAddr to be the accepted peer.
func (self *Front) HttpHandler(interfaceName string) http.Handler {
	frontInterface, ok := self.lookupInterface(interfaceName)
	if !ok {
		return self.unknownInterfaceHandler(interfaceName)
	}
	return frontInterface.httpHandler
}

// The h3 handler for one interface's api connections. It serves only api
// authorities, so a connection whose SNI is an api name can't carry a request
// for another service (D3), and forwards them as HttpsHandler does. Any other
// authority is a 421 (Misdirected Request), and /status is alt's 404.
func (self *Front) H3ApiHandler(interfaceName string) http.Handler {
	frontInterface, ok := self.lookupInterface(interfaceName)
	if !ok {
		return self.unknownInterfaceHandler(interfaceName)
	}
	return frontInterface.h3ApiHandler
}

// The interface named interfaceName, or false.
func (self *Front) lookupInterface(interfaceName string) (*frontInterface, bool) {
	frontInterface, ok := self.interfaces[interfaceName]
	return frontInterface, ok
}

// The handler of an interface the route table doesn't list: every request is
// a 503, so nothing is served without that interface's limits. Asking for
// one is a wiring error at start, logged once there; an interface name is
// never an address.
func (self *Front) unknownInterfaceHandler(interfaceName string) http.Handler {
	glog.Errorf("[front]%s: %q\n", errRouteHttpUnknownInterface, interfaceName)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	})
}

// Admits one QUIC connection from addr on the interface, under alt's
// per-address connection cap (D8): the interface's rate_limit.net_connections,
// counted per (interface name, normalized client IP) apart from HTTP
// concurrency, with exclude_subnets exempt and zero meaning no cap. ok is false
// when the address is at its cap, and connect then closes the connection with
// application error 0x1001. A true result must be paired with exactly one call
// of release, when the connection ends. A refusal is counted in the
// interface's default zone. An interface the route table doesn't list admits
// nothing, counted under the limit "interface" and no zone rather than
// logged, since connections set its pace.
func (self *Front) AcquireQuicConnection(interfaceName string, addr netip.Addr) (release func(), ok bool) {
	frontInterface, ok := self.lookupInterface(interfaceName)
	if !ok {
		self.metrics.limitRefusals.WithLabelValues("interface", routeHttpNoLabel).Inc()
		return nil, false
	}
	release, outcome := self.limits.acquireQuicConnection(frontInterface.limits, addr)
	if outcome != limitOutcomeAdmitted {
		self.metrics.limitRefusals.WithLabelValues(outcome.String(), limitDefaultZone).Inc()
		return nil, false
	}
	return release, true
}

// Starts the drain (D12): the front stops admitting new HTTP work, idle
// keepalive connections close, and responses carry Connection: close.
// Accepted requests and forwarded websockets run on until they end or the
// drain timeout passes. Connect calls it right after SetDrainingIfReady,
// while its transports can still send.
//
// The front's part: every h1 and h2 response from here carries Connection:
// close (Go's h2 server sends a GOAWAY for it), and an h1 upgrade request is
// refused with 503 rather than starting a tunnel that would hold the old
// generation for the whole drain; the client's next connection reaches the
// new generation. A request that races the drain is otherwise served.
// Closing idle keepalive connections and the h2 and h3 GOAWAYs are the
// servers', so connect's (§2.8).
func (self *Front) BeginDrain() {
	if !self.draining.CompareAndSwap(false, true) {
		return
	}
	drainTimeout := self.settings.Http.DrainTimeout
	if drainTimeout <= 0 {
		return
	}
	go server.HandleError(func() {
		select {
		case <-self.ctx.Done():
		case <-time.After(drainTimeout):
			self.cut()
		}
	})
}

// Whether BeginDrain has run.
func (self *Front) Draining() bool {
	return self.draining.Load()
}

// The revision of the route file the front was built from, the lowercase hex
// sha256 of its bytes, reported in X-UR-Front-Route-Revision.
func (self *Front) Revision() string {
	return self.revision
}

// Releases the upstream pools and spill files, cutting whatever outlived the
// drain, and waits for that at most HttpSettings.ForcedCloseTimeout. New
// requests are answered 503 from the first call. Later calls return the
// first call's result.
func (self *Front) Close() error {
	self.closeOnce.Do(func() {
		self.closed.Store(true)
		self.cut()
		forcedCloseTimeout := self.settings.Http.ForcedCloseTimeout
		if 0 < forcedCloseTimeout {
			select {
			case <-self.cutDone:
				self.closeErr = self.cutErr
			case <-time.After(forcedCloseTimeout):
				self.closeErr = fmt.Errorf("lbalt front: the forced close did not complete within %s", forcedCloseTimeout)
			}
		} else {
			select {
			case <-self.cutDone:
				self.closeErr = self.cutErr
			default:
			}
		}
		self.cancel()
	})
	return self.closeErr
}

// Cuts whatever outlived the drain, once, in the background: the hijacked
// connections, each forwarder's requests, pools and tunnels, and the body
// store, all at once. A close can block, a TLS close_notify to a client that
// stopped reading for one, so each runs on its own and none waits on
// another: the forwarders cut the upstream sides of forwarded websockets
// however long a client side takes. cutDone closes when every close has
// returned, and cutErr is final then.
func (self *Front) cut() {
	started := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.cutStarted {
			return false
		}
		self.cutStarted = true
		return true
	}()
	if !started {
		return
	}
	go server.HandleError(func() {
		defer close(self.cutDone)
		hijackedConns := func() []*responseHijackedConn {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			hijackedConns := []*responseHijackedConn{}
			for hijackedConn := range self.hijackedConns {
				hijackedConns = append(hijackedConns, hijackedConn)
			}
			return hijackedConns
		}()
		// a panicking close ends only its own goroutine
		var closes sync.WaitGroup
		for _, hijackedConn := range hijackedConns {
			closes.Go(func() {
				server.HandleError(func() {
					hijackedConn.Close()
				})
			})
		}
		for _, serviceForwarder := range self.serviceForwarders {
			closes.Go(func() {
				server.HandleError(serviceForwarder.close)
			})
		}
		if self.bodyStore != nil {
			closes.Go(func() {
				server.HandleError(func() {
					self.cutErr = self.bodyStore.close()
				})
			})
		}
		closes.Wait()
	})
}

// Tracks a hijacked connection until it closes, so the cut can close it.
// False once the cut has started, when the caller must close it, since the
// cut may already have taken its list.
func (self *Front) trackHijackedConn(hijackedConn *responseHijackedConn) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.cutStarted {
		return false
	}
	self.hijackedConns[hijackedConn] = true
	return true
}

// Ends the tracking of a closed hijacked connection.
func (self *Front) untrackHijackedConn(hijackedConn *responseHijackedConn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.hijackedConns, hijackedConn)
}

// The number of hijacked connections still open.
func (self *Front) hijackedConnCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.hijackedConns)
}
