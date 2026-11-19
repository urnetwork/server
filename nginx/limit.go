package nginx

// The D8 limits (owner: s-http): nginx's limit_req and limit_conn, with
// alt/rate_limit.go's semantics and nginx's keys.
//
// nginx renders one config per lb interface, with zones keyed on
// $binary_remote_addr, so its limits are per (interface, address). One
// connect process serves every interface of its host, so the state here is
// keyed by interface name and normalized client IP, and one source on two
// interfaces never shares a quota (review, "Limit identity").
//
//   - The request zone is a token bucket of burst+1 tokens refilled at the
//     interface's rate. It is nginx's `limit_req ... burst=N nodelay`
//     exactly: nginx's excess, kept in milli-requests, is the bucket read
//     from the other side, and a refused request isn't charged. A route with
//     a zone of its own (route_rate_limits, on service names only) charges
//     its bucket instead of the shared one.
//   - The concurrency cap counts requests in flight per (interface,
//     address): every h2 and h3 stream is a request, and an upgraded
//     connection holds its slot until its tunnel closes. The bucket is
//     charged first, as nginx runs limit_req before limit_conn, so a request
//     the cap refuses has spent its token.
//   - QUIC connections have a counter of their own under the same cap
//     (alt's per-address connection cap, refused with 0x1001).
//   - An address in the interface's exclude_subnets keeps no state and is
//     never refused. The lb status exemptions are the router's: it doesn't
//     call the limiter for them, as nginx's empty $limit_key skips both
//     zones.
//
// State is bounded: idle state is swept once its buckets refill, and at
// LimitSettings.MaxAddressStates the least recently used idle state makes
// room. A state with something in flight is never dropped, so the in-flight
// counts are exact.
//
// frontLimits is safe for concurrent use.

import (
	"container/list"
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// One interface's per-address limits, from its rate_limit, in the units the
// limiter uses. Read-only once built.
type limitInterface struct {
	// limit state is keyed by it
	name string
	// the shared request zone's rate per second; non-positive means no
	// request limit, as an absent limit_req does
	requestsPerSecond float64
	// requests the shared zone admits above its rate at once, nodelay
	burst int
	// the most requests in flight per address, and separately the most QUIC
	// connections per address; non-positive means no cap
	netConnections int
	// addresses exempt from every limit
	excludePrefixes []netip.Prefix
}

// The limits of one route file interface. Its exclude_subnets were checked
// when the table was parsed.
func newLimitInterface(routeInterface *RouteInterface) *limitInterface {
	excludePrefixes := []netip.Prefix{}
	for _, excludeSubnet := range routeInterface.RateLimit.ExcludeSubnets {
		prefix, err := netip.ParsePrefix(excludeSubnet)
		if err != nil {
			continue
		}
		// as nginx's geo, which keys on the network
		excludePrefixes = append(excludePrefixes, prefix.Masked())
	}
	return &limitInterface{
		name:              routeInterface.Name,
		requestsPerSecond: routeInterface.RateLimit.RequestsPerMinute / 60,
		burst:             routeInterface.RateLimit.Burst,
		netConnections:    routeInterface.RateLimit.NetConnections,
		excludePrefixes:   excludePrefixes,
	}
}

// Whether a normalized address is inside one of the interface's
// exclude_subnets.
func (self *limitInterface) excluded(addr netip.Addr) bool {
	for _, prefix := range self.excludePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// A route zone: an exact path of one service whose request bucket replaces
// the shared one there (route_rate_limits). Read-only once built; its
// address is its identity.
type limitRouteZone struct {
	serviceName string
	// service-relative, as the route file writes it
	path string
	// always positive: nginx refuses a zero rate
	requestsPerSecond float64
	burst             int
}

// The route zone of one route file rule.
func newLimitRouteZone(serviceName string, routeRateLimit *RouteRateLimitRule) *limitRouteZone {
	return &limitRouteZone{
		serviceName:       serviceName,
		path:              routeRateLimit.Path,
		requestsPerSecond: routeRateLimit.RequestsPerMinute / 60,
		burst:             routeRateLimit.Burst,
	}
}

// How one limiter call ended. Each refusal is a 429 (or 0x1001 for QUIC).
type limitOutcome int

const (
	limitOutcomeAdmitted limitOutcome = iota
	// the shared request zone was empty
	limitOutcomeRequestRefused
	// the route's own zone was empty
	limitOutcomeRouteRefused
	// the address had its cap of requests in flight
	limitOutcomeConcurrencyRefused
	// the address had its cap of QUIC connections
	limitOutcomeQuicRefused
	// the state was at MaxAddressStates with nothing idle to drop
	limitOutcomeStateRefused
)

// A short lowercase name, for the refusal counter's closed label set.
func (self limitOutcome) String() string {
	switch self {
	case limitOutcomeAdmitted:
		return "admitted"
	case limitOutcomeRequestRefused:
		return "request"
	case limitOutcomeRouteRefused:
		return "route"
	case limitOutcomeConcurrencyRefused:
		return "concurrency"
	case limitOutcomeQuicRefused:
		return "quic"
	case limitOutcomeStateRefused:
		return "state"
	default:
		return "unknown"
	}
}

// A token bucket with nginx's `burst=N nodelay` semantics: burst+1 tokens,
// refilled continuously at the rate, one taken per admitted request. A new
// bucket is full, as a new nginx node starts with no excess. Guarded by the
// limiter's stateLock.
type limitBucket struct {
	tokens     float64
	updateTime time.Time
}

// A full bucket.
func newLimitBucket(burst int, now time.Time) *limitBucket {
	return &limitBucket{
		tokens:     limitBucketCapacity(burst),
		updateTime: now,
	}
}

// The tokens a bucket holds when full: a rate with no burst still admits one
// request per refill interval, which is nginx's excess of zero.
func limitBucketCapacity(burst int) float64 {
	return float64(max(burst, 0) + 1)
}

// Refills the bucket for the time since its last update. A clock that went
// backwards adds nothing.
func (self *limitBucket) fill(requestsPerSecond float64, burst int, now time.Time) {
	if requestsPerSecond <= 0 {
		self.tokens = limitBucketCapacity(burst)
		self.updateTime = now
		return
	}
	if elapsed := now.Sub(self.updateTime); 0 < elapsed {
		self.tokens = min(limitBucketCapacity(burst), self.tokens+elapsed.Seconds()*requestsPerSecond)
	}
	self.updateTime = now
}

// Takes one token, or false, leaving the bucket as it was, when it holds
// less than one. A non-positive rate always admits.
func (self *limitBucket) take(requestsPerSecond float64, burst int, now time.Time) bool {
	if requestsPerSecond <= 0 {
		return true
	}
	self.fill(requestsPerSecond, burst, now)
	if self.tokens < 1 {
		return false
	}
	self.tokens -= 1
	return true
}

// Whether the bucket has refilled, so dropping it changes no decision.
func (self *limitBucket) full(requestsPerSecond float64, burst int, now time.Time) bool {
	self.fill(requestsPerSecond, burst, now)
	return limitBucketCapacity(burst) <= self.tokens
}

// The key of one state: an interface and a normalized client IP.
type limitAddrKey struct {
	interfaceName string
	addr          netip.Addr
}

// One (interface, address)'s buckets and counts. Guarded by the limiter's
// stateLock.
type limitAddrState struct {
	key            limitAddrKey
	limitInterface *limitInterface
	// the state's place in the idle order while nothing is in flight, else
	// nil
	idleElement  *list.Element
	sharedBucket *limitBucket
	// created on a route's first request
	routeBuckets        map[*limitRouteZone]*limitBucket
	requestCount        int
	quicConnectionCount int
	// the last admission, refusal or release
	touchTime time.Time
}

// Whether nothing is in flight.
func (self *limitAddrState) idle() bool {
	return self.requestCount == 0 && self.quicConnectionCount == 0
}

// Whether every bucket has refilled.
func (self *limitAddrState) full(now time.Time) bool {
	if !self.sharedBucket.full(self.limitInterface.requestsPerSecond, self.limitInterface.burst, now) {
		return false
	}
	for routeZone, routeBucket := range self.routeBuckets {
		if !routeBucket.full(routeZone.requestsPerSecond, routeZone.burst, now) {
			return false
		}
	}
	return true
}

// The front's limit state, for every interface. Safe for concurrent use.
type frontLimits struct {
	ctx      context.Context
	settings *LimitSettings
	now      func() time.Time

	stateLock  sync.Mutex
	addrStates map[limitAddrKey]*limitAddrState
	// the states with nothing in flight, the least recently touched at the
	// back: the eviction and sweep order
	idleAddrStates *list.List
}

// Builds empty state and starts its sweep, which ends with ctx. now is the
// clock every decision reads: time.Now, or a test's.
func newFrontLimits(ctx context.Context, settings *LimitSettings, now func() time.Time) *frontLimits {
	limits := &frontLimits{
		ctx:            ctx,
		settings:       settings,
		now:            now,
		addrStates:     map[limitAddrKey]*limitAddrState{},
		idleAddrStates: list.New(),
	}
	go server.HandleError(limits.run)
	return limits
}

// Sweeps idle state every PruneInterval until ctx ends.
func (self *frontLimits) run() {
	if self.settings.PruneInterval <= 0 {
		return
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.PruneInterval):
		}
		self.sweep()
	}
}

// Drops every state idle for StateIdleTimeout whose buckets have refilled.
// The idle order is by touch time, so the walk from the back stops at the
// first state touched more recently.
func (self *frontLimits) sweep() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := self.now()

	element := self.idleAddrStates.Back()
	for element != nil {
		previousElement := element.Prev()
		addrState := element.Value.(*limitAddrState)
		if now.Sub(addrState.touchTime) < self.settings.StateIdleTimeout {
			break
		}
		if addrState.full(now) {
			self.removeWithLock(addrState)
		}
		element = previousElement
	}
}

// The number of states held.
func (self *frontLimits) stateCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.addrStates)
}

// Admits one request from addr on the interface: limit_req on the route's
// zone when routeZone is set, else on the shared zone, then limit_conn. An
// admitted request's release must be called when the request ends; it is
// safe to call more than once. A refused request gets a nil release.
func (self *frontLimits) acquireRequest(limitInterface *limitInterface, addr netip.Addr, routeZone *limitRouteZone) (func(), limitOutcome) {
	addr = normalizeLimitAddr(addr)
	if limitInterface.excluded(addr) {
		return func() {}, limitOutcomeAdmitted
	}

	var addrState *limitAddrState
	outcome := func() limitOutcome {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := self.now()
		var ok bool
		addrState, ok = self.addrStateWithLock(limitInterface, addr, now)
		if !ok {
			return limitOutcomeStateRefused
		}
		self.touchWithLock(addrState, now)

		if routeZone != nil {
			routeBucket, ok := addrState.routeBuckets[routeZone]
			if !ok {
				routeBucket = newLimitBucket(routeZone.burst, now)
				if addrState.routeBuckets == nil {
					addrState.routeBuckets = map[*limitRouteZone]*limitBucket{}
				}
				addrState.routeBuckets[routeZone] = routeBucket
			}
			if !routeBucket.take(routeZone.requestsPerSecond, routeZone.burst, now) {
				return limitOutcomeRouteRefused
			}
		} else if !addrState.sharedBucket.take(limitInterface.requestsPerSecond, limitInterface.burst, now) {
			return limitOutcomeRequestRefused
		}

		if 0 < limitInterface.netConnections && limitInterface.netConnections <= addrState.requestCount {
			return limitOutcomeConcurrencyRefused
		}
		addrState.requestCount += 1
		self.markBusyWithLock(addrState)
		return limitOutcomeAdmitted
	}()
	if outcome != limitOutcomeAdmitted {
		return nil, outcome
	}

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			addrState.requestCount -= 1
			if addrState.requestCount < 0 {
				panic("lbalt: limit request count became negative")
			}
			self.releaseWithLock(addrState, self.now())
		})
	}, limitOutcomeAdmitted
}

// Admits one QUIC connection from addr on the interface under the cap of
// connections per address, a counter apart from the requests'. An excluded
// address or a zero cap is always admitted. The release rules are
// acquireRequest's.
func (self *frontLimits) acquireQuicConnection(limitInterface *limitInterface, addr netip.Addr) (func(), limitOutcome) {
	addr = normalizeLimitAddr(addr)
	if limitInterface.excluded(addr) || limitInterface.netConnections <= 0 {
		return func() {}, limitOutcomeAdmitted
	}

	var addrState *limitAddrState
	outcome := func() limitOutcome {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := self.now()
		var ok bool
		addrState, ok = self.addrStateWithLock(limitInterface, addr, now)
		if !ok {
			return limitOutcomeStateRefused
		}
		self.touchWithLock(addrState, now)
		if limitInterface.netConnections <= addrState.quicConnectionCount {
			return limitOutcomeQuicRefused
		}
		addrState.quicConnectionCount += 1
		self.markBusyWithLock(addrState)
		return limitOutcomeAdmitted
	}()
	if outcome != limitOutcomeAdmitted {
		return nil, outcome
	}

	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			addrState.quicConnectionCount -= 1
			if addrState.quicConnectionCount < 0 {
				panic("lbalt: limit quic connection count became negative")
			}
			self.releaseWithLock(addrState, self.now())
		})
	}, limitOutcomeAdmitted
}

// The state of (interface, addr), created full when absent. At
// MaxAddressStates the least recently used idle state is dropped to make
// room; false when every state has something in flight.
func (self *frontLimits) addrStateWithLock(limitInterface *limitInterface, addr netip.Addr, now time.Time) (*limitAddrState, bool) {
	key := limitAddrKey{
		interfaceName: limitInterface.name,
		addr:          addr,
	}
	if addrState, ok := self.addrStates[key]; ok {
		return addrState, true
	}
	if 0 < self.settings.MaxAddressStates && self.settings.MaxAddressStates <= len(self.addrStates) {
		oldestElement := self.idleAddrStates.Back()
		if oldestElement == nil {
			return nil, false
		}
		self.removeWithLock(oldestElement.Value.(*limitAddrState))
	}
	addrState := &limitAddrState{
		key:            key,
		limitInterface: limitInterface,
		sharedBucket:   newLimitBucket(limitInterface.burst, now),
		touchTime:      now,
	}
	addrState.idleElement = self.idleAddrStates.PushFront(addrState)
	self.addrStates[key] = addrState
	return addrState, true
}

// Marks activity: an idle state moves to the front of the idle order.
func (self *frontLimits) touchWithLock(addrState *limitAddrState, now time.Time) {
	addrState.touchTime = now
	if addrState.idleElement != nil {
		self.idleAddrStates.MoveToFront(addrState.idleElement)
	}
}

// Takes a state with something in flight out of the idle order, so it is
// never dropped.
func (self *frontLimits) markBusyWithLock(addrState *limitAddrState) {
	if addrState.idleElement != nil {
		self.idleAddrStates.Remove(addrState.idleElement)
		addrState.idleElement = nil
	}
}

// After a count went down: a state with nothing left in flight rejoins the
// idle order, and with a non-positive StateIdleTimeout it is dropped at once
// when its buckets have refilled.
func (self *frontLimits) releaseWithLock(addrState *limitAddrState, now time.Time) {
	addrState.touchTime = now
	if !addrState.idle() {
		return
	}
	if addrState.idleElement == nil {
		addrState.idleElement = self.idleAddrStates.PushFront(addrState)
	}
	if self.settings.StateIdleTimeout <= 0 && addrState.full(now) {
		self.removeWithLock(addrState)
	}
}

// Drops an idle state.
func (self *frontLimits) removeWithLock(addrState *limitAddrState) {
	if addrState.idleElement != nil {
		self.idleAddrStates.Remove(addrState.idleElement)
		addrState.idleElement = nil
	}
	delete(self.addrStates, addrState.key)
}

// The address limit state is keyed by: an IPv4-mapped IPv6 address as its
// IPv4 address, as nginx's separate IPv4 sockets see it, and no zone.
func normalizeLimitAddr(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}
