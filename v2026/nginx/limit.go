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
//     its bucket instead of the shared one. A refused request is counted by
//     the zone it was charged to, whichever limit refused it
//     (limitRefusalZoneLabel): the route zone's nginx name, or default.
//   - The concurrency cap counts requests in flight per (interface,
//     address): every h2 and h3 stream is a request, and an upgraded
//     connection holds its slot until its tunnel closes. The bucket is
//     charged first, as nginx runs limit_req before limit_conn, so a request
//     the cap refuses has spent its token.
//   - QUIC connections have counters of their own under the same cap
//     (alt's per-address connection cap, refused with 0x1001), one for each
//     class of connection, api and connect, so an address's api connections
//     never refuse its connect transport (LBALTOPT2 R15).
//   - An address in the interface's exclude_subnets keeps no state and is
//     never refused. The lb status exemptions are the router's: it doesn't
//     call the limiter for them, as nginx's empty $limit_key skips both
//     zones.
//   - A refusal of a bucket reports the wait until that bucket would admit
//     the request, which the router sends as Retry-After
//     (limitRetryAfterSeconds); a refusal of a cap or of the state reports
//     none.
//
// The identity limit (LBALTOPT2 R15) is off unless an interface's rate_limit
// sets identity_requests_per_minute. With it on, a request of the shared
// zone whose credential verified (LimitSettings.IdentifyRequest) is charged
// to its identity's bucket, at the identity rate and burst, and to its
// address's ceiling bucket, at the shared rate and burst times
// identity_address_factor; both are checked before either is charged, as
// nginx checks every zone of a request before it charges one. It is held to
// the interface's net_connections per identity, and to net_connections times
// the factor per address, counted apart from the requests without an
// identity. So many clients behind one shared address are each limited as
// one address was, and the address as a whole is still bounded. A request
// without a credential, or with one that doesn't verify, is charged exactly
// as without the identity limit, and a route zone's requests are never
// charged to an identity. A request whose verified SN25 validator rate class
// token names the same client (LimitIdentity.Validator) is charged at the
// validator class's limits in place of the identity limit's, in state of its
// own; its address ceiling is the same.
//
// State is bounded: idle state is swept once its buckets refill, and at
// LimitSettings.MaxAddressStates the least recently used idle address state
// makes room; identity state is bounded the same way by MaxIdentityStates.
// A state with something in flight is never dropped, so the in-flight counts
// are exact. An identity the state can't hold is charged as a request
// without one, and only a verified credential ever creates identity state.
//
// frontLimits is safe for concurrent use.

import (
	"container/list"
	"context"
	"math"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

// One interface's per-address limits, from its rate_limit, in the units the
// limiter uses. Read-only once built.
type limitInterface struct {
	// limit state is keyed by it
	name string
	// the shared request zone's rate in milli-requests per second; non-positive means no
	// request limit, as an absent limit_req does
	milliRequestsPerSecond float64
	// requests the shared zone admits above its rate at once, nodelay
	burst int
	// the most requests in flight per address, and separately the most QUIC
	// connections of each class per address; non-positive means no cap
	netConnections int
	// addresses exempt from every limit
	excludePrefixes []netip.Prefix

	// the identity limit's own rate in milli-requests per second, per
	// identity; non-positive means the identity limit is off, and every
	// identity field is zero
	identityMilliRequestsPerSecond float64
	// requests one identity's bucket admits above its rate at once, nodelay
	identityBurst int
	// an address's ceiling for requests whose credential verified: the
	// shared zone's rate, burst and concurrency cap times
	// identity_address_factor, where non-positive means no limit, as for the
	// shared zone
	ceilingMilliRequestsPerSecond float64
	ceilingBurst                  int
	ceilingNetConnections         int
}

// The limits of one route file interface. Its exclude_subnets and identity
// fields were checked when the table was parsed.
func newLimitInterface(routeInterface *RouteInterface) *limitInterface {
	rateLimit := routeInterface.RateLimit
	excludePrefixes := []netip.Prefix{}
	for _, excludeSubnet := range rateLimit.ExcludeSubnets {
		prefix, err := netip.ParsePrefix(excludeSubnet)
		if err != nil {
			continue
		}
		// as nginx's geo, which keys on the network
		excludePrefixes = append(excludePrefixes, prefix.Masked())
	}
	limits := &limitInterface{
		name:                   routeInterface.Name,
		milliRequestsPerSecond: limitMilliRequestsPerSecond(rateLimit.RequestsPerMinute),
		burst:                  rateLimit.Burst,
		netConnections:         rateLimit.NetConnections,
		excludePrefixes:        excludePrefixes,
	}
	if 0 < rateLimit.IdentityRequestsPerMinute {
		factor := max(1, rateLimit.IdentityAddressFactor)
		limits.identityMilliRequestsPerSecond = limitMilliRequestsPerSecond(rateLimit.IdentityRequestsPerMinute)
		limits.identityBurst = rateLimit.IdentityBurst
		limits.ceilingMilliRequestsPerSecond = limits.milliRequestsPerSecond * float64(factor)
		limits.ceilingBurst = rateLimit.Burst * factor
		limits.ceilingNetConnections = rateLimit.NetConnections * factor
	}
	return limits
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

// Whether the interface's rate_limit turns the identity limit on.
func (self *limitInterface) identityEnabled() bool {
	return 0 < self.identityMilliRequestsPerSecond
}

// A route zone: an exact path of one service whose request bucket replaces
// the shared one there (route_rate_limits). Read-only once built; its
// address is its identity.
type limitRouteZone struct {
	serviceName string
	// service-relative, as the route file writes it
	path string
	// nginx's name for the zone (limitRouteZoneName), the refusal counter's
	// zone label for the requests it charges
	name string
	// always positive: nginx refuses a zero rate
	milliRequestsPerSecond float64
	burst                  int
}

// The route zone of one route file rule.
func newLimitRouteZone(serviceName string, routeRateLimit *RouteRateLimitRule) *limitRouteZone {
	return &limitRouteZone{
		serviceName:            serviceName,
		path:                   routeRateLimit.Path,
		name:                   limitRouteZoneName(serviceName, routeRateLimit.Path),
		milliRequestsPerSecond: limitMilliRequestsPerSecond(routeRateLimit.RequestsPerMinute),
		burst:                  routeRateLimit.Burst,
	}
}

// The zone label of a refusal under the interface's shared request zone,
// which every request outside a route zone charges, and of a refused QUIC
// connection, which the interface's own cap refuses.
const limitDefaultZone = "default"

// The zone label of a refusal of a request charged to its identity: the
// identity limit's refusals (limitOutcome.identity).
const limitIdentityZone = "identity"

// nginx's name for a route zone, as the nginx generator declares it
// (warp/services/services.go:RouteRateLimitZoneName): route_, the service,
// _, then the path without its leading /, every rune but an ASCII letter or
// digit replaced by _. Built from the route table only, so as a label it is
// bounded by the table's route_rate_limits and never carries a request's
// path or a hidden route prefix.
func limitRouteZoneName(serviceName string, path string) string {
	zonePath := strings.Map(func(r rune) rune {
		if ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			return r
		}
		return '_'
	}, strings.TrimPrefix(path, "/"))
	return "route_" + serviceName + "_" + zonePath
}

// The refusal counter's zone label of a request charged to routeZone and
// refused with outcome: the identity zone's for a refusal of the identity
// limit, else routeZone's name, or the default zone's when nil, the shared
// zone charging it.
func limitRefusalZoneLabel(routeZone *limitRouteZone, outcome limitOutcome) string {
	if outcome.identity() {
		return limitIdentityZone
	}
	if routeZone == nil {
		return limitDefaultZone
	}
	return routeZone.name
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
	// the address had its cap of QUIC connections of the class
	limitOutcomeQuicRefused
	// the state was at MaxAddressStates with nothing idle to drop
	limitOutcomeStateRefused
	// the identity's bucket was empty
	limitOutcomeIdentityRefused
	// the identity had its cap of requests in flight
	limitOutcomeIdentityConcurrencyRefused
	// the address's ceiling bucket, which requests with an identity charge,
	// was empty
	limitOutcomeCeilingRefused
	// the address had its ceiling's cap of requests with an identity in
	// flight
	limitOutcomeCeilingConcurrencyRefused
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
	case limitOutcomeIdentityRefused:
		return "identity"
	case limitOutcomeIdentityConcurrencyRefused:
		return "identity_concurrency"
	case limitOutcomeCeilingRefused:
		return "ceiling"
	case limitOutcomeCeilingConcurrencyRefused:
		return "ceiling_concurrency"
	default:
		return "unknown"
	}
}

// Whether the outcome is a refusal of the identity limit, of a request
// charged to its identity.
func (self limitOutcome) identity() bool {
	switch self {
	case limitOutcomeIdentityRefused,
		limitOutcomeIdentityConcurrencyRefused,
		limitOutcomeCeilingRefused,
		limitOutcomeCeilingConcurrencyRefused:
		return true
	default:
		return false
	}
}

// The classes of QUIC connection the cap counts apart, each against the
// interface's net_connections, so an address's api h3 connections can never
// refuse its connect transport (LBALTOPT2 R15).
type limitQuicClass int

const (
	limitQuicClassConnect limitQuicClass = iota
	limitQuicClassApi
	// the number of classes
	limitQuicClassCount
)

// The QUIC class of a connection dispatched by its name's class, or false
// for a class the front doesn't serve on QUIC.
func limitQuicClassOf(nameClass NameClass) (limitQuicClass, bool) {
	switch nameClass {
	case NameClassConnect:
		return limitQuicClassConnect, true
	case NameClassApi:
		return limitQuicClassApi, true
	default:
		return 0, false
	}
}

// The longest Retry-After the front sends, in seconds.
const limitMaxRetryAfterSeconds = 60

// The Retry-After of a refusal, in whole seconds: the refused bucket's wait
// until it would admit the request, rounded up, at least 1 and at most
// limitMaxRetryAfterSeconds. A refusal that reports no wait, a cap's or the
// state's, is 1.
func limitRetryAfterSeconds(wait time.Duration) int {
	if wait <= 0 {
		return 1
	}
	// waits are held to limitMaxWait, so this can't overflow
	seconds := int((wait + time.Second - 1) / time.Second)
	return min(max(seconds, 1), limitMaxRetryAfterSeconds)
}

// The longest wait a bucket reports: past any Retry-After, so a rate too
// slow to compute with is still refused with the longest one.
const limitMaxWait = time.Hour

// nginx truncates configured rates to milli-requests per second. The route
// schema also accepts fractional rates unavailable in nginx configuration.
func limitMilliRequestsPerSecond(requestsPerMinute float64) float64 {
	rate := requestsPerMinute * 1000 / 60
	if math.Trunc(requestsPerMinute) == requestsPerMinute {
		return math.Floor(rate)
	}
	return rate
}

// Charges and decay are in whole milli-requests, as in ngx_http_limit_req.
// Only an admission commits the accumulated decay and its timestamp.
type limitBucket struct {
	charged float64
	lastMs  int64
}

func (self *limitBucket) remainingWithLock(rate float64, nowMs int64) float64 {
	elapsedMs := nowMs - self.lastMs
	if elapsedMs <= 0 || self.charged == 0 {
		return self.charged
	}
	return max(0, self.charged-math.Floor(rate*float64(elapsedMs)/1000))
}

// Whether the bucket would admit a request now, without charging it.
func (self *limitBucket) admitsWithLock(rate float64, burst int, nowMs int64) bool {
	return rate <= 0 || self.remainingWithLock(rate, nowMs) <= float64(burst)*1000
}

func (self *limitBucket) takeWithLock(rate float64, burst int, nowMs int64) bool {
	if rate <= 0 {
		return true
	}
	remaining := self.remainingWithLock(rate, nowMs)
	if remaining > float64(burst)*1000 {
		return false
	}
	self.charged = remaining + 1000
	self.lastMs = nowMs
	return true
}

// The wait from nowMs until the bucket would admit one more request: zero
// while it admits, otherwise the least whole milliseconds of decay, counted
// from its last charge as remainingWithLock counts them, that bring it
// within its burst, and at most limitMaxWait.
func (self *limitBucket) waitWithLock(rate float64, burst int, nowMs int64) time.Duration {
	if self.admitsWithLock(rate, burst, nowMs) {
		return 0
	}
	limit := float64(burst) * 1000
	// it admits once decay of at least this much has accumulated since
	// lastMs: charged - floor(rate*elapsedMs/1000) <= limit
	decay := self.charged - limit
	elapsedMs := math.Ceil(decay * 1000 / rate)
	if float64(limitMaxWait.Milliseconds()) < elapsedMs-float64(nowMs-self.lastMs) {
		return limitMaxWait
	}
	admitsAfter := func(elapsedMs float64) bool {
		return self.charged-math.Floor(rate*elapsedMs/1000) <= limit
	}
	// the division can round across a whole millisecond: settle it with the
	// bucket's own arithmetic, so a wait that ends on a whole second rounds
	// up to that second and no further
	if 1 < elapsedMs && admitsAfter(elapsedMs-1) {
		elapsedMs -= 1
	} else if !admitsAfter(elapsedMs) {
		elapsedMs += 1
	}
	waitMs := self.lastMs + int64(elapsedMs) - nowMs
	return time.Duration(max(1, waitMs)) * time.Millisecond
}

// A verified credential's identity, as LimitSettings.IdentifyRequest
// returns it: the client id of a client token, or the network id of a
// network token, which names no client. The zero LimitIdentity is none.
type LimitIdentity struct {
	Id server.Id
	// whether Id is a network's
	Network bool
	// whether a verified validator rate class token beside the bearer names
	// this client (session.ValidatorClassVerifier): its requests are charged
	// at the validator class's limits in place of the identity limit's, to
	// state of their own
	Validator bool
}

// The rate_class label of the front's limit counters.
const (
	limitRateClassDefault   = "default"
	limitRateClassValidator = "validator"
)

// The SN25 validator rate class: an admitted validator's requests, charged to
// its identity at these limits in place of the identity limit's, and still to
// its address's ceiling. On wherever the identity limit is on.
const (
	validatorClassRequestsPerMinute = 3000
	validatorClassBurst             = 1500
	validatorClassNetConnections    = 100
)

// The rate, burst and cap of requests in flight that an identity is charged
// at: the validator class's for a validator, else the interface's identity
// limit's.
func (self *limitInterface) identityLimits(identity LimitIdentity) (milliRequestsPerSecond float64, burst int, netConnections int) {
	if identity.Validator {
		return limitMilliRequestsPerSecond(validatorClassRequestsPerMinute), validatorClassBurst, validatorClassNetConnections
	}
	return self.identityMilliRequestsPerSecond, self.identityBurst, self.netConnections
}

// Whether the identity is none.
func (self LimitIdentity) none() bool {
	return self.Id == server.Id{}
}

// The bookkeeping every limit state has, for its table's idle order.
// Guarded by the limiter's stateLock.
type limitStateNode struct {
	// the state's place in the idle order while nothing is in flight, else
	// nil
	idleElement *list.Element
	// the last admission, refusal or release
	touchTime time.Time
}

// One kind of limit state, kept in a limitStateTable by its key.
type limitState[K comparable] interface {
	stateKey() K
	stateNode() *limitStateNode
	// whether nothing is in flight
	idleWithLock() bool
	// whether every bucket has refilled
	fullWithLock(nowMs int64) bool
}

// The states of one kind: each by its key, and the idle ones in the order
// the sweep and the eviction walk, the least recently touched at the back.
// Guarded by the limiter's stateLock.
type limitStateTable[K comparable, S limitState[K]] struct {
	// the most states held; non-positive means no cap
	maxStates int
	states    map[K]S
	idle      *list.List
	// Next idle state to examine when a sweep yields the admission lock.
	sweepNext *list.Element
}

// An empty table that holds at most maxStates states.
func newLimitStateTable[K comparable, S limitState[K]](maxStates int) *limitStateTable[K, S] {
	return &limitStateTable[K, S]{
		maxStates: maxStates,
		states:    map[K]S{},
		idle:      list.New(),
	}
}

// The state of key, or false.
func (self *limitStateTable[K, S]) lookupWithLock(key K) (S, bool) {
	state, ok := self.states[key]
	return state, ok
}

// Adds a new state, idle, at the front of the idle order. At maxStates the
// least recently used idle state is dropped to make room; false, and nothing
// added, when every state has something in flight.
func (self *limitStateTable[K, S]) addWithLock(state S) bool {
	if 0 < self.maxStates && self.maxStates <= len(self.states) {
		oldestElement := self.idle.Back()
		if oldestElement == nil {
			return false
		}
		self.removeWithLock(oldestElement.Value.(S))
	}
	state.stateNode().idleElement = self.idle.PushFront(state)
	self.states[state.stateKey()] = state
	return true
}

// Marks activity: an idle state moves to the front of the idle order.
func (self *limitStateTable[K, S]) touchWithLock(state S, now time.Time) {
	node := state.stateNode()
	node.touchTime = now
	if node.idleElement != nil {
		if self.sweepNext == node.idleElement {
			self.sweepNext = self.sweepNext.Prev()
		}
		self.idle.MoveToFront(node.idleElement)
	}
}

// Removes a state from the idle order and preserves the sweep cursor.
func (self *limitStateTable[K, S]) removeIdleWithLock(state S) {
	node := state.stateNode()
	if node.idleElement != nil {
		if self.sweepNext == node.idleElement {
			self.sweepNext = self.sweepNext.Prev()
		}
		self.idle.Remove(node.idleElement)
		node.idleElement = nil
	}
}

// After a count went down: a state with nothing left in flight rejoins the
// idle order, and with a non-positive idleTimeout it is dropped at once when
// its buckets have refilled.
func (self *limitStateTable[K, S]) releaseWithLock(state S, now time.Time, nowMs int64, idleTimeout time.Duration) {
	node := state.stateNode()
	node.touchTime = now
	if !state.idleWithLock() {
		return
	}
	if node.idleElement == nil {
		node.idleElement = self.idle.PushFront(state)
	}
	if idleTimeout <= 0 && state.fullWithLock(nowMs) {
		self.removeWithLock(state)
	}
}

// Drops an idle state.
func (self *limitStateTable[K, S]) removeWithLock(state S) {
	self.removeIdleWithLock(state)
	delete(self.states, state.stateKey())
}

// Examines at most batchSize idle states in idle order, from where the last
// batch stopped, and drops each that has been idle for idleTimeout with its
// buckets refilled. True when states are left for a next batch. Depleted
// buckets retain their place in the eviction order even when their refill
// takes several sweeps.
func (self *limitStateTable[K, S]) sweepWithLock(now time.Time, nowMs int64, idleTimeout time.Duration, batchSize int) bool {
	element := self.sweepNext
	if element == nil {
		element = self.idle.Back()
	}
	for range max(1, batchSize) {
		if element == nil {
			break
		}
		state := element.Value.(S)
		if now.Sub(state.stateNode().touchTime) < idleTimeout {
			element = nil
			break
		}
		element = element.Prev()
		if state.fullWithLock(nowMs) {
			self.removeWithLock(state)
		}
	}
	self.sweepNext = element
	return element != nil
}

// The key of one address state: an interface and a normalized client IP.
type limitAddrKey struct {
	interfaceName string
	addr          netip.Addr
}

// One (interface, address)'s buckets and counts. Guarded by the limiter's
// stateLock.
type limitAddrState struct {
	limitStateNode
	key            limitAddrKey
	limitInterface *limitInterface
	sharedBucket   limitBucket
	// the identity limit's ceiling, which requests with an identity charge
	// at the ceiling's rate and burst
	ceilingBucket limitBucket
	// created on a route's first request
	routeBuckets map[*limitRouteZone]*limitBucket
	// requests in flight without an identity
	requestCount int
	// requests in flight with an identity, under the ceiling's cap
	identityRequestCount int
	// QUIC connections, by class
	quicConnectionCounts [limitQuicClassCount]int
}

var _ limitState[limitAddrKey] = (*limitAddrState)(nil)

// See limitState.
func (self *limitAddrState) stateKey() limitAddrKey {
	return self.key
}

// See limitState.
func (self *limitAddrState) stateNode() *limitStateNode {
	return &self.limitStateNode
}

// See limitState.
func (self *limitAddrState) idleWithLock() bool {
	if self.requestCount != 0 || self.identityRequestCount != 0 {
		return false
	}
	for _, quicConnectionCount := range self.quicConnectionCounts {
		if quicConnectionCount != 0 {
			return false
		}
	}
	return true
}

// See limitState.
func (self *limitAddrState) fullWithLock(nowMs int64) bool {
	if self.sharedBucket.remainingWithLock(self.limitInterface.milliRequestsPerSecond, nowMs) > 0 {
		return false
	}
	if self.ceilingBucket.remainingWithLock(self.limitInterface.ceilingMilliRequestsPerSecond, nowMs) > 0 {
		return false
	}
	for routeZone, routeBucket := range self.routeBuckets {
		if routeBucket.remainingWithLock(routeZone.milliRequestsPerSecond, nowMs) > 0 {
			return false
		}
	}
	return true
}

// The key of one identity state: an interface and a verified identity.
type limitIdentityKey struct {
	interfaceName string
	identity      LimitIdentity
}

// One (interface, identity)'s bucket and count, for the identity limit.
// Guarded by the limiter's stateLock.
type limitIdentityState struct {
	limitStateNode
	key            limitIdentityKey
	limitInterface *limitInterface
	bucket         limitBucket
	// requests in flight, under the interface's net_connections
	requestCount int
}

var _ limitState[limitIdentityKey] = (*limitIdentityState)(nil)

// See limitState.
func (self *limitIdentityState) stateKey() limitIdentityKey {
	return self.key
}

// See limitState.
func (self *limitIdentityState) stateNode() *limitStateNode {
	return &self.limitStateNode
}

// See limitState.
func (self *limitIdentityState) idleWithLock() bool {
	return self.requestCount == 0
}

// See limitState.
func (self *limitIdentityState) fullWithLock(nowMs int64) bool {
	milliRequestsPerSecond, _, _ := self.limitInterface.identityLimits(self.key.identity)
	return self.bucket.remainingWithLock(milliRequestsPerSecond, nowMs) <= 0
}

// The front's limit state, for every interface. Safe for concurrent use.
type frontLimits struct {
	ctx      context.Context
	settings *LimitSettings
	now      func() time.Time
	// All buckets use whole milliseconds on the same monotonic clock.
	startTime time.Time

	stateLock      sync.Mutex
	addrStates     *limitStateTable[limitAddrKey, *limitAddrState]
	identityStates *limitStateTable[limitIdentityKey, *limitIdentityState]
}

// Builds empty state and starts its sweep, which ends with ctx. now is the
// clock every decision reads: time.Now, or a test's.
func newFrontLimits(ctx context.Context, settings *LimitSettings, now func() time.Time) *frontLimits {
	limits := &frontLimits{
		ctx:            ctx,
		settings:       settings,
		now:            now,
		startTime:      now(),
		addrStates:     newLimitStateTable[limitAddrKey, *limitAddrState](settings.MaxAddressStates),
		identityStates: newLimitStateTable[limitIdentityKey, *limitIdentityState](settings.MaxIdentityStates),
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
		for self.sweep() {
			if self.ctx.Err() != nil {
				return
			}
		}
	}
}

// Examines one batch of each kind of state in idle order, each batch under
// its own hold of the admission lock. A true result schedules the next
// batch after releasing the admission lock.
func (self *frontLimits) sweep() bool {
	addrsLeft := self.sweepAddrs()
	identitiesLeft := self.sweepIdentities()
	return addrsLeft || identitiesLeft
}

// One batch of the address states' sweep.
func (self *frontLimits) sweepAddrs() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.now()
	return self.addrStates.sweepWithLock(now, self.nowMs(now), self.settings.StateIdleTimeout, self.settings.PruneBatchSize)
}

// One batch of the identity states' sweep.
func (self *frontLimits) sweepIdentities() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.now()
	return self.identityStates.sweepWithLock(now, self.nowMs(now), self.settings.StateIdleTimeout, self.settings.PruneBatchSize)
}

// The bucket clock's reading at now.
func (self *frontLimits) nowMs(now time.Time) int64 {
	return now.Sub(self.startTime).Milliseconds()
}

// The number of address states held.
func (self *frontLimits) stateCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.addrStates.states)
}

// The number of identity states held.
func (self *frontLimits) identityStateCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.identityStates.states)
}

// Admits one request from addr on the interface: limit_req on the route's
// zone when routeZone is set, else on the shared zone, then limit_conn. With
// an identity, a request of the shared zone on an interface with the
// identity limit is charged to the identity limit instead (the file
// header). An admitted request's release must be called when the request
// ends; it is safe to call more than once. A refused request gets a nil
// release, and wait, the time until the refused bucket would admit it, or
// zero for a refusal of a cap or of the state.
func (self *frontLimits) acquireRequest(limitInterface *limitInterface, addr netip.Addr, routeZone *limitRouteZone, identity LimitIdentity) (release func(), outcome limitOutcome, wait time.Duration) {
	addr = normalizeLimitAddr(addr)
	if limitInterface.excluded(addr) {
		return func() {}, limitOutcomeAdmitted, 0
	}
	if routeZone != nil || !limitInterface.identityEnabled() {
		identity = LimitIdentity{}
	}

	var addrState *limitAddrState
	var identityState *limitIdentityState
	outcome = func() limitOutcome {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := self.now()
		var ok bool
		addrState, ok = self.addrStateWithLock(limitInterface, addr, now)
		if !ok {
			return limitOutcomeStateRefused
		}
		self.addrStates.touchWithLock(addrState, now)
		nowMs := self.nowMs(now)

		if !identity.none() {
			// an identity the state can't hold is charged as a request
			// without one
			if identityState, ok = self.identityStateWithLock(limitInterface, identity, now); ok {
				self.identityStates.touchWithLock(identityState, now)
				var identityOutcome limitOutcome
				identityOutcome, wait = self.chargeIdentityWithLock(addrState, identityState, nowMs)
				return identityOutcome
			}
			identityState = nil
		}

		if routeZone != nil {
			routeBucket, ok := addrState.routeBuckets[routeZone]
			if !ok {
				routeBucket = &limitBucket{}
				if addrState.routeBuckets == nil {
					addrState.routeBuckets = map[*limitRouteZone]*limitBucket{}
				}
				addrState.routeBuckets[routeZone] = routeBucket
			}
			if !routeBucket.takeWithLock(routeZone.milliRequestsPerSecond, routeZone.burst, nowMs) {
				wait = routeBucket.waitWithLock(routeZone.milliRequestsPerSecond, routeZone.burst, nowMs)
				return limitOutcomeRouteRefused
			}
		} else if !addrState.sharedBucket.takeWithLock(limitInterface.milliRequestsPerSecond, limitInterface.burst, nowMs) {
			wait = addrState.sharedBucket.waitWithLock(limitInterface.milliRequestsPerSecond, limitInterface.burst, nowMs)
			return limitOutcomeRequestRefused
		}

		if 0 < limitInterface.netConnections && limitInterface.netConnections <= addrState.requestCount {
			return limitOutcomeConcurrencyRefused
		}
		addrState.requestCount += 1
		self.addrStates.removeIdleWithLock(addrState)
		return limitOutcomeAdmitted
	}()
	if outcome != limitOutcomeAdmitted {
		return nil, outcome, wait
	}

	var releaseOnce sync.Once
	if identityState != nil {
		return func() {
			releaseOnce.Do(func() {
				self.releaseIdentityRequest(addrState, identityState)
			})
		}, limitOutcomeAdmitted, 0
	}
	return func() {
		releaseOnce.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			addrState.requestCount -= 1
			if addrState.requestCount < 0 {
				panic("lbalt: limit request count became negative")
			}
			now := self.now()
			self.addrStates.releaseWithLock(addrState, now, self.nowMs(now), self.settings.StateIdleTimeout)
		})
	}, limitOutcomeAdmitted, 0
}

// Charges a request with an identity: the identity's bucket and the
// address's ceiling bucket, both checked before either is charged, then the
// identity's cap of requests in flight and the ceiling's. A refused bucket's
// wait is the wait until both would admit.
func (self *frontLimits) chargeIdentityWithLock(addrState *limitAddrState, identityState *limitIdentityState, nowMs int64) (limitOutcome, time.Duration) {
	limitInterface := addrState.limitInterface
	identityRate, identityBurst, identityNetConnections := limitInterface.identityLimits(identityState.key.identity)
	identityAdmits := identityState.bucket.admitsWithLock(identityRate, identityBurst, nowMs)
	ceilingAdmits := addrState.ceilingBucket.admitsWithLock(limitInterface.ceilingMilliRequestsPerSecond, limitInterface.ceilingBurst, nowMs)
	if !identityAdmits || !ceilingAdmits {
		wait := max(
			identityState.bucket.waitWithLock(identityRate, identityBurst, nowMs),
			addrState.ceilingBucket.waitWithLock(limitInterface.ceilingMilliRequestsPerSecond, limitInterface.ceilingBurst, nowMs),
		)
		if !identityAdmits {
			return limitOutcomeIdentityRefused, wait
		}
		return limitOutcomeCeilingRefused, wait
	}
	identityState.bucket.takeWithLock(identityRate, identityBurst, nowMs)
	addrState.ceilingBucket.takeWithLock(limitInterface.ceilingMilliRequestsPerSecond, limitInterface.ceilingBurst, nowMs)

	if 0 < identityNetConnections && identityNetConnections <= identityState.requestCount {
		return limitOutcomeIdentityConcurrencyRefused, 0
	}
	if 0 < limitInterface.ceilingNetConnections && limitInterface.ceilingNetConnections <= addrState.identityRequestCount {
		return limitOutcomeCeilingConcurrencyRefused, 0
	}
	identityState.requestCount += 1
	addrState.identityRequestCount += 1
	self.identityStates.removeIdleWithLock(identityState)
	self.addrStates.removeIdleWithLock(addrState)
	return limitOutcomeAdmitted, 0
}

// Ends an admitted request with an identity.
func (self *frontLimits) releaseIdentityRequest(addrState *limitAddrState, identityState *limitIdentityState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	identityState.requestCount -= 1
	addrState.identityRequestCount -= 1
	if identityState.requestCount < 0 || addrState.identityRequestCount < 0 {
		panic("lbalt: limit identity request count became negative")
	}
	now := self.now()
	nowMs := self.nowMs(now)
	self.identityStates.releaseWithLock(identityState, now, nowMs, self.settings.StateIdleTimeout)
	self.addrStates.releaseWithLock(addrState, now, nowMs, self.settings.StateIdleTimeout)
}

// Admits one QUIC connection of a class from addr on the interface under the
// cap of connections of that class per address, a counter apart from the
// requests' and the other class's. An excluded address or a zero cap is
// always admitted. The release rules are acquireRequest's.
func (self *frontLimits) acquireQuicConnection(limitInterface *limitInterface, addr netip.Addr, quicClass limitQuicClass) (func(), limitOutcome) {
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
		self.addrStates.touchWithLock(addrState, now)
		if limitInterface.netConnections <= addrState.quicConnectionCounts[quicClass] {
			return limitOutcomeQuicRefused
		}
		addrState.quicConnectionCounts[quicClass] += 1
		self.addrStates.removeIdleWithLock(addrState)
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
			addrState.quicConnectionCounts[quicClass] -= 1
			if addrState.quicConnectionCounts[quicClass] < 0 {
				panic("lbalt: limit quic connection count became negative")
			}
			now := self.now()
			self.addrStates.releaseWithLock(addrState, now, self.nowMs(now), self.settings.StateIdleTimeout)
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
	if addrState, ok := self.addrStates.lookupWithLock(key); ok {
		return addrState, true
	}
	addrState := &limitAddrState{
		limitStateNode: limitStateNode{
			touchTime: now,
		},
		key:            key,
		limitInterface: limitInterface,
	}
	if !self.addrStates.addWithLock(addrState) {
		return nil, false
	}
	return addrState, true
}

// The state of (interface, identity), created full when absent, as
// addrStateWithLock creates an address's, under MaxIdentityStates.
func (self *frontLimits) identityStateWithLock(limitInterface *limitInterface, identity LimitIdentity, now time.Time) (*limitIdentityState, bool) {
	key := limitIdentityKey{
		interfaceName: limitInterface.name,
		identity:      identity,
	}
	if identityState, ok := self.identityStates.lookupWithLock(key); ok {
		return identityState, true
	}
	identityState := &limitIdentityState{
		limitStateNode: limitStateNode{
			touchTime: now,
		},
		key:            key,
		limitInterface: limitInterface,
	}
	if !self.identityStates.addWithLock(identityState) {
		return nil, false
	}
	return identityState, true
}

// The address limit state is keyed by: an IPv4-mapped IPv6 address as its
// IPv4 address, as nginx's separate IPv4 sockets see it, and no zone.
func normalizeLimitAddr(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}
