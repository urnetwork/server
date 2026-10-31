// The rate limits alt enforces for itself (EXTENDER.md L5).
//
// Behind the load balancer nginx owns these limits and the go services
// enforce none. Alt has no load balancer in front of it, so it applies the
// same `default_rate_limit` block of services.yml that warpctl renders into
// the nginx configuration: a per-address request bucket with `nodelay`
// semantics on the api front, a per-address concurrent request cap on the api
// front, and a per-address concurrent connection cap on the connect front.
// Both nginx directives answer 429 in this deployment, so both limits here do
// too. An address inside `exclude_subnets` is exempt from all three, exactly
// as nginx maps an excluded prefix to an empty limit key.
//
// The api service's `route_rate_limits` give exact routes a per-address
// bucket of their own instead of the shared one, as their nginx locations do
// (support inbox 8093, 8077): /connect/control carries one request per
// contract, and the api limits it per client id. Such a route still counts
// against the address's concurrent request cap.
//
// The state is per process and never shared. The nginx zones it replaces are
// per lb front, and alt is one block per proxy host, so a shared store would
// be a different limit rather than the same one.
package alt

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The limits one alt process applies, in the units the limiter uses.
// A non-positive rate or cap disables that limiter, as an absent nginx
// directive does.
type LimitsSettings struct {
	// the `requests_per_minute` (or `requests_per_second`) rate, per second
	RequestsPerSecond float64
	// requests admitted above the rate before the bucket refuses
	Burst int
	// concurrent requests and concurrent connections allowed for one address
	NetConnections  int
	ExcludePrefixes []netip.Prefix
	// how often idle address state is discarded
	SweepTimeout time.Duration
	// exact api routes with a bucket of their own
	Routes []*RouteLimitsSettings
}

// One exact api route whose requests charge a per-address bucket of their own
// instead of the shared one, in the units the limiter uses. A non-positive
// rate leaves the route unlimited by rate, as for the shared bucket.
type RouteLimitsSettings struct {
	Path              string
	RequestsPerSecond float64
	Burst             int
}

// Loads the `default_rate_limit` block of the environment's services.yml,
// the same document warpctl renders the nginx limits from (ServicesConfig).
// A missing file or block falls back to the warp default, which is what
// nginx applies to a block that declares no rate limit of its own.
//
// Alt never reads `WARP_LIMIT_EXCLUDE_SUBNETS`: the exclusions are the
// parsed block's, so alt and nginx exempt exactly the same addresses.
//
// The api service's `route_rate_limits` come from the same document.
func DefaultLimitsSettings() *LimitsSettings {
	rateLimit, err := loadDefaultRateLimit()
	if err != nil {
		glog.Infof("[alt]no services.yml default rate limit (%s). Using the warp default.\n", err)
		rateLimit = DefaultRateLimit()
	}
	settings := LimitsSettingsFromRateLimit(rateLimit)
	if servicesConfig, err := LoadServicesConfig(); err == nil {
		settings.Routes = RouteLimitsSettingsFromServices(servicesConfig, ApiServiceName)
	}
	return settings
}

// Translates a service's `route_rate_limits` as the nginx template does:
// `requests_per_minute` wins over `requests_per_second`.
func RouteLimitsSettingsFromServices(servicesConfig *ServicesConfig, service string) []*RouteLimitsSettings {
	routes := []*RouteLimitsSettings{}
	serviceConfig, ok := servicesConfig.Versions[0].Services[service]
	if !ok || serviceConfig == nil {
		return routes
	}
	for _, routeRateLimit := range serviceConfig.RouteRateLimits {
		if routeRateLimit == nil || routeRateLimit.Path == "" {
			continue
		}
		route := &RouteLimitsSettings{
			Path:  routeRateLimit.Path,
			Burst: routeRateLimit.Burst,
		}
		if 0 < routeRateLimit.RequestsPerMinute {
			route.RequestsPerSecond = float64(routeRateLimit.RequestsPerMinute) / 60.0
		} else if 0 < routeRateLimit.RequestsPerSecond {
			route.RequestsPerSecond = float64(routeRateLimit.RequestsPerSecond)
		}
		routes = append(routes, route)
	}
	return routes
}

func loadDefaultRateLimit() (*RateLimit, error) {
	servicesConfig, err := LoadServicesConfig()
	if err != nil {
		return nil, err
	}
	if servicesConfig.DefaultRateLimit == nil {
		return nil, fmt.Errorf("%s has no default_rate_limit", servicesResourceName)
	}
	return servicesConfig.DefaultRateLimit, nil
}

// Translates one rate limit block the way the nginx template does:
// `requests_per_minute` wins over `requests_per_second`, and a zero value
// leaves that directive out entirely. An unparseable exclusion is invalid
// infrastructure configuration and panics at startup, as it does for the
// shared caller-ip exclusions.
func LimitsSettingsFromRateLimit(rateLimit *RateLimit) *LimitsSettings {
	settings := &LimitsSettings{
		Burst:           rateLimit.Burst,
		NetConnections:  rateLimit.NetConnections,
		ExcludePrefixes: rateLimit.ExcludePrefixes(),
		SweepTimeout:    60 * time.Second,
	}
	if 0 < rateLimit.RequestsPerMinute {
		settings.RequestsPerSecond = float64(rateLimit.RequestsPerMinute) / 60.0
	} else if 0 < rateLimit.RequestsPerSecond {
		settings.RequestsPerSecond = float64(rateLimit.RequestsPerSecond)
	}
	return settings
}

// One address's bucket and live counters. Guarded by the owning Limits'
// stateLock.
type limitsAddr struct {
	// requests the bucket may admit immediately, refilled at the configured
	// rate and capped at burst+1. nginx admits the first request of a new key
	// unconditionally and then allows an excess of at most burst, which is
	// the same capacity read from the other side.
	tokens     float64
	updateTime time.Time
	// outstanding api requests and connect connections
	requestCount    int
	connectionCount int
}

// The alt front's limiter. Safe for concurrent use.
type Limits struct {
	ctx      context.Context
	settings *LimitsSettings

	stateLock  sync.Mutex
	addrLimits map[netip.Addr]*limitsAddr

	// each exact route's own bucket, by path; read-only once constructed
	routes map[string]*Limits
}

func NewLimits(ctx context.Context, settings *LimitsSettings) *Limits {
	limits := &Limits{
		ctx:        ctx,
		settings:   settings,
		addrLimits: map[netip.Addr]*limitsAddr{},
		routes:     map[string]*Limits{},
	}
	for _, route := range settings.Routes {
		// the route's bucket only: the concurrent request cap stays the
		// address's, counted here
		limits.routes[route.Path] = NewLimits(ctx, &LimitsSettings{
			RequestsPerSecond: route.RequestsPerSecond,
			Burst:             route.Burst,
			ExcludePrefixes:   settings.ExcludePrefixes,
			SweepTimeout:      settings.SweepTimeout,
		})
	}
	go server.HandleError(limits.run)
	return limits
}

// Discards idle address state. An address with no outstanding request or
// connection and a full bucket is indistinguishable from one never seen, so
// dropping it changes no decision and bounds the map by the live address
// count rather than by every address ever seen.
func (self *Limits) run() {
	sweepTimeout := self.settings.SweepTimeout
	if sweepTimeout <= 0 {
		sweepTimeout = 60 * time.Second
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(sweepTimeout):
		}
		self.sweep(time.Now())
	}
}

func (self *Limits) sweep(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for addr, addrLimit := range self.addrLimits {
		if addrLimit.requestCount != 0 || addrLimit.connectionCount != 0 {
			continue
		}
		self.fillWithLock(addrLimit, now)
		if self.capacity() <= addrLimit.tokens {
			delete(self.addrLimits, addr)
		}
	}
}

// Reports whether the address bypasses every alt limit.
func (self *Limits) Excluded(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range self.settings.ExcludePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Admits one api request. The returned release must be called exactly once
// when the response is complete; a refused request returns a nil release.
// The bucket is charged before the concurrency cap, as nginx evaluates
// limit_req before limit_conn, so a request refused by the cap has already
// spent its token.
func (self *Limits) AcquireRequest(addr netip.Addr, now time.Time) (func(), bool) {
	return self.acquireRequest(addr, now, true)
}

// Admits one api request to path. A route with a bucket of its own charges
// that bucket and not the shared one, as its nginx location's own limit_req
// replaces the shared one, and still counts against the address's
// concurrent request cap, as limit_conn applies to every location.
func (self *Limits) AcquireRouteRequest(addr netip.Addr, path string, now time.Time) (func(), bool) {
	route, ok := self.routes[path]
	if !ok {
		return self.AcquireRequest(addr, now)
	}
	routeRelease, ok := route.AcquireRequest(addr, now)
	if !ok {
		return nil, false
	}
	release, ok := self.acquireRequest(addr, now, false)
	if !ok {
		routeRelease()
		return nil, false
	}
	return func() {
		release()
		routeRelease()
	}, true
}

// AcquireRequest, charging the shared bucket only when chargeBucket.
func (self *Limits) acquireRequest(addr netip.Addr, now time.Time, chargeBucket bool) (func(), bool) {
	if self.Excluded(addr) {
		return func() {}, true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	addrLimit := self.addrLimitWithLock(addr, now)
	if chargeBucket && !self.takeTokenWithLock(addrLimit, now) {
		self.releaseAddrWithLock(addr, addrLimit)
		return nil, false
	}
	if 0 < self.settings.NetConnections && self.settings.NetConnections <= addrLimit.requestCount {
		self.releaseAddrWithLock(addr, addrLimit)
		return nil, false
	}
	addrLimit.requestCount += 1
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		addrLimit.requestCount -= 1
		if addrLimit.requestCount < 0 {
			panic("alt limits request count became negative")
		}
		self.releaseAddrWithLock(addr, addrLimit)
	}, true
}

// Admits one connect connection against the same concurrency cap on its own
// counter. The connect front has no request rate, so the bucket is not
// charged here: a connection's admission rate is already governed by the
// exchange's own ConnectionRateLimit.
func (self *Limits) AcquireConnection(addr netip.Addr, now time.Time) (func(), bool) {
	if self.Excluded(addr) {
		return func() {}, true
	}
	if self.settings.NetConnections <= 0 {
		return func() {}, true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	addrLimit := self.addrLimitWithLock(addr, now)
	if self.settings.NetConnections <= addrLimit.connectionCount {
		self.releaseAddrWithLock(addr, addrLimit)
		return nil, false
	}
	addrLimit.connectionCount += 1
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		addrLimit.connectionCount -= 1
		if addrLimit.connectionCount < 0 {
			panic("alt limits connection count became negative")
		}
		self.releaseAddrWithLock(addr, addrLimit)
	}, true
}

// The instantaneous bucket size. A rate with no burst still admits one
// request per refill interval, which is nginx's excess of zero.
func (self *Limits) capacity() float64 {
	return float64(max(self.settings.Burst, 0) + 1)
}

func (self *Limits) addrLimitWithLock(addr netip.Addr, now time.Time) *limitsAddr {
	addr = addr.Unmap()
	if addrLimit, ok := self.addrLimits[addr]; ok {
		return addrLimit
	}
	addrLimit := &limitsAddr{
		tokens:     self.capacity(),
		updateTime: now,
	}
	self.addrLimits[addr] = addrLimit
	return addrLimit
}

// Drops state that carries no decision, so a refused or completed caller does
// not retain a map entry.
func (self *Limits) releaseAddrWithLock(addr netip.Addr, addrLimit *limitsAddr) {
	if addrLimit.requestCount != 0 || addrLimit.connectionCount != 0 {
		return
	}
	if addrLimit.tokens < self.capacity() {
		return
	}
	delete(self.addrLimits, addr.Unmap())
}

func (self *Limits) fillWithLock(addrLimit *limitsAddr, now time.Time) {
	if self.settings.RequestsPerSecond <= 0 {
		addrLimit.tokens = self.capacity()
		addrLimit.updateTime = now
		return
	}
	if elapsed := now.Sub(addrLimit.updateTime); 0 < elapsed {
		addrLimit.tokens = min(
			self.capacity(),
			addrLimit.tokens+elapsed.Seconds()*self.settings.RequestsPerSecond,
		)
	}
	addrLimit.updateTime = now
}

func (self *Limits) takeTokenWithLock(addrLimit *limitsAddr, now time.Time) bool {
	if self.settings.RequestsPerSecond <= 0 {
		return true
	}
	self.fillWithLock(addrLimit, now)
	if addrLimit.tokens < 1 {
		return false
	}
	addrLimit.tokens -= 1
	return true
}

// Wraps the api router with the alt front's limits. Behind the lb this is
// what the rendered `limit_req ... nodelay` and `limit_conn` directives do.
// A caller whose address cannot be parsed is served unlimited: alt only ever
// serves real udp peers, and refusing a caller the limiter cannot identify
// would drop valid traffic on a parse quirk rather than on a budget.
func NewLimitedHandler(handler http.Handler, limits *Limits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addrPort, err := server.ParseClientAddress(r.RemoteAddr)
		if err != nil {
			if glog.V(1) {
				glog.Infof("[alt]unlimited request from an unparseable address err = %s\n", err)
			}
			handler.ServeHTTP(w, r)
			return
		}
		release, ok := limits.AcquireRouteRequest(addrPort.Addr(), r.URL.Path, time.Now())
		if !ok {
			http.Error(w, "Too many requests.", http.StatusTooManyRequests)
			return
		}
		defer release()
		handler.ServeHTTP(w, r)
	})
}
