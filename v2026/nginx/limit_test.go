package nginx

// Tests of the D8 limiter (limit.go): nginx's limit_req and limit_conn per
// (interface, address), route zones, exclusions, the QUIC connection cap,
// and the bounds on state.

import (
	"math/rand/v2"
	"net/netip"
	"testing"
	"time"
)

// A limiter over a hand-moved clock, with no sweep.
func newTestFrontLimits(t *testing.T, settings *LimitSettings) (*frontLimits, *testClock) {
	t.Helper()
	clock := newTestClock()
	settings.PruneInterval = 0
	return newFrontLimits(t.Context(), settings, clock.now), clock
}

// An interface's limits from rate_limit values.
func newTestLimitInterface(name string, requestsPerMinute float64, burst int, netConnections int, excludeSubnets ...string) *limitInterface {
	return newLimitInterface(&RouteInterface{
		Name: name,
		RateLimit: &RouteRateLimit{
			RequestsPerMinute: requestsPerMinute,
			Burst:             burst,
			NetConnections:    netConnections,
			ExcludeSubnets:    excludeSubnets,
		},
	})
}

// nginx's limit_req for one key (ngx_http_limit_req_lookup), the reference:
// the excess in milli-requests and time in whole milliseconds; a new key is
// admitted with no excess; a refusal changes nothing.
type nginxLimitReqNode struct {
	exists bool
	excess int64
	last   int64
}

// nginx's decision for a request at nowMs, for a zone of rate requests per
// minute (`rate=Nr/m`, N*1000/60 milli-requests per second) and a burst.
func (self *nginxLimitReqNode) lookup(requestsPerMinute int64, burst int64, nowMs int64) bool {
	rate := requestsPerMinute * 1000 / 60
	if !self.exists {
		self.exists = true
		self.excess = 0
		self.last = nowMs
		return true
	}
	ms := nowMs - self.last
	if ms < -60000 {
		ms = 1
	} else if ms < 0 {
		ms = 0
	}
	excess := self.excess - rate*ms/1000 + 1000
	if excess < 0 {
		excess = 0
	}
	if burst*1000 < excess {
		return false
	}
	self.excess = excess
	if ms != 0 {
		self.last = nowMs
	}
	return true
}

// The shared zone decides as nginx's `limit_req ... burst=N nodelay` does,
// request for request, over a long irregular arrival sequence that both
// drains and refills the bucket. The gaps are multiples of 25 ms at 10
// requests per second, so both models count exact quarter tokens and no
// float rounding can decide a boundary.
func TestLimitBucketDecidesAsNginxLimitReq(t *testing.T) {
	const (
		requestsPerMinute = 600
		burst             = 3
	)
	limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", requestsPerMinute, burst, 0)
	addr := netip.MustParseAddr("198.51.100.7")
	reference := &nginxLimitReqNode{}

	// a fixed seed, so the sequence and its verdict are the same every run
	random := rand.New(rand.NewPCG(1, 2))
	elapsedMs := int64(0)
	admittedCount := 0
	refusedCount := 0
	for i := range 4000 {
		if 0 < i {
			gapMs := int64(25 * random.IntN(17))
			elapsedMs += gapMs
			clock.advance(time.Duration(gapMs) * time.Millisecond)
		}
		want := reference.lookup(requestsPerMinute, burst, elapsedMs)
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		got := outcome == limitOutcomeAdmitted
		if got != want {
			t.Fatalf("request %d at %d ms: admitted = %t, nginx admits = %t", i, elapsedMs, got, want)
		}
		if got {
			admittedCount += 1
			release()
		} else {
			refusedCount += 1
			if outcome != limitOutcomeRequestRefused {
				t.Fatalf("request %d refused as %s, want request", i, outcome)
			}
		}
	}
	if admittedCount == 0 || refusedCount == 0 {
		t.Fatalf("the sequence admitted %d and refused %d; it must do both to compare", admittedCount, refusedCount)
	}
}

// A rate of one per minute becomes 16 milli-requests per second in nginx.
// Frequent refusals and a sweep must preserve the last admitted request's time.
func TestLimitSlowRateMatchesNginxThroughRefusalsAndSweep(t *testing.T) {
	for _, route := range []bool{false, true} {
		limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
		limitInterface := newTestLimitInterface("eth1", 1, 0, 0)
		var zone *limitRouteZone
		if route {
			zone = newLimitRouteZone("api", &RouteRateLimitRule{Path: "/control", RequestsPerMinute: 1})
		}
		reference := &nginxLimitReqNode{}
		addr := netip.MustParseAddr("192.0.2.11")
		var lastMs int64
		for _, ms := range []int64{0, 59999, 60000, 62499, 62500, 62499 + 62500, 125000} {
			clock.advance(time.Duration(ms-lastMs) * time.Millisecond)
			lastMs = ms
			if ms == 60000 {
				limits.sweep()
			}
			release, outcome, _ := limits.acquireRequest(limitInterface, addr, zone, LimitIdentity{})
			if release != nil {
				release()
			}
			if want := reference.lookup(1, 0, ms); (outcome == limitOutcomeAdmitted) != want {
				t.Errorf("route %t at %d ms: got %s, nginx admitted %t", route, ms, outcome, want)
			}
		}
	}
}

// With main's values, 450 r/m and burst 150, a new address gets burst+1
// requests at once, then one per 133.3 ms (450 per minute), as nginx's
// nodelay does.
func TestLimitBucketAdmitsBurstPlusOneThenTheRate(t *testing.T) {
	limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 450, 150, 0)
	addr := netip.MustParseAddr("198.51.100.7")

	for i := range 151 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("request %d of the burst refused as %s", i, outcome)
		}
		release()
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("request past burst+1 = %s, want request", outcome)
	}

	// a token every 133.3 ms
	clock.advance(66 * time.Millisecond)
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("after half a refill interval = %s, want request", outcome)
	}
	clock.advance(68 * time.Millisecond)
	release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("after one refill interval = %s, want admitted", outcome)
	}
	release()
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("a second request in the interval = %s, want request", outcome)
	}
}

// The concurrency cap counts requests in flight per address; a release frees
// a slot; the bucket is charged before the cap, as nginx runs limit_req
// before limit_conn, so a request the cap refuses spent its token.
func TestLimitConcurrencyCapAfterTheBucket(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	// a bucket of two tokens that never refills within the test
	limitInterface := newTestLimitInterface("eth1", 1, 1, 1)
	addr := netip.MustParseAddr("198.51.100.7")

	releaseFirst, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("first request = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("a second request in flight = %s, want concurrency", outcome)
	}
	releaseFirst()
	// the refused request took the bucket's second token
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("after the release = %s, want request: the cap's refusal spent a token", outcome)
	}
}

// With no request rate, only the cap limits; with no cap, only the rate;
// with neither, everything is admitted.
func TestLimitZeroDisablesEachLimit(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	addr := netip.MustParseAddr("198.51.100.7")

	noRate := newTestLimitInterface("eth1", 0, 0, 2)
	releases := []func(){}
	for i := range 2 {
		release, outcome, _ := limits.acquireRequest(noRate, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("no rate, request %d = %s", i, outcome)
		}
		releases = append(releases, release)
	}
	if _, outcome, _ := limits.acquireRequest(noRate, addr, nil, LimitIdentity{}); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("no rate, past the cap = %s, want concurrency", outcome)
	}
	for _, release := range releases {
		release()
	}

	noCap := newTestLimitInterface("eth2", 0, 0, 0)
	for i := range 1000 {
		if _, outcome, _ := limits.acquireRequest(noCap, addr, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
			t.Fatalf("no limits, request %d = %s", i, outcome)
		}
	}
}

// A route zone replaces the shared zone on its route: its requests charge
// only its bucket, and the shared bucket's state doesn't touch them. The
// concurrency cap still counts them.
func TestLimitRouteZoneReplacesTheSharedZone(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 1, 0, 2)
	routeZone := newLimitRouteZone("api", &RouteRateLimitRule{
		Path:              "/connect/control",
		RequestsPerMinute: 1,
		Burst:             2,
	})
	addr := netip.MustParseAddr("198.51.100.7")

	release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("shared = %s", outcome)
	}
	release()
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("shared, empty = %s, want request", outcome)
	}
	routeReleases := []func(){}
	for i := range 2 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, routeZone, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("route request %d with the shared zone empty = %s", i, outcome)
		}
		routeReleases = append(routeReleases, release)
	}
	// two in flight: the cap counts route requests
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, routeZone, LimitIdentity{}); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("route request past the cap = %s, want concurrency", outcome)
	}
	for _, release := range routeReleases {
		release()
	}
	// that refusal spent the route bucket's last token
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, routeZone, LimitIdentity{}); outcome != limitOutcomeRouteRefused {
		t.Fatalf("route, empty = %s, want route", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("shared, still empty = %s, want request", outcome)
	}
}

// State is per (interface, address): one source on two interfaces has two
// quotas, and two sources on one interface have one each.
func TestLimitStateIsPerInterfaceAndAddress(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	eth1 := newTestLimitInterface("eth1", 1, 0, 0)
	eth2 := newTestLimitInterface("eth2", 1, 0, 0)
	addr := netip.MustParseAddr("198.51.100.7")
	otherAddr := netip.MustParseAddr("198.51.100.8")

	if _, outcome, _ := limits.acquireRequest(eth1, addr, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("eth1 = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(eth1, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("eth1 again = %s, want request", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(eth2, addr, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("the same source on eth2 = %s, want its own quota", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(eth1, otherAddr, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("another source on eth1 = %s, want its own quota", outcome)
	}
}

// An IPv4-mapped IPv6 address is its IPv4 address, and a zone is ignored.
func TestLimitNormalizesTheAddress(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 1, 0, 0)

	if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("::ffff:198.51.100.7"), nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("mapped = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("198.51.100.7"), nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("the same address unmapped = %s, want request", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("2001:db8::7%eth0"), nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("zoned = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("2001:db8::7"), nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("the same address without the zone = %s, want request", outcome)
	}
}

// An address in exclude_subnets is never refused and keeps no state, in
// either family, including an IPv4-mapped one.
func TestLimitExcludedSubnetsAreExempt(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 1, 0, 1, "203.0.113.0/24", "2001:db8:5::/48")

	for _, addr := range []string{"203.0.113.5", "::ffff:203.0.113.5", "2001:db8:5::9"} {
		for i := range 10 {
			if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr(addr), nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
				t.Fatalf("%s request %d = %s, want admitted", addr, i, outcome)
			}
			if _, outcome := limits.acquireQuicConnection(limitInterface, netip.MustParseAddr(addr), limitQuicClassConnect); outcome != limitOutcomeAdmitted {
				t.Fatalf("%s quic connection %d = %s, want admitted", addr, i, outcome)
			}
		}
	}
	if count := limits.stateCount(); count != 0 {
		t.Fatalf("excluded addresses keep %d states, want 0", count)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("203.0.114.5"), nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("outside the subnet = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("203.0.114.5"), nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("outside the subnet, again = %s, want request", outcome)
	}
}

// QUIC connections have their own counter under net_connections, apart from
// requests in flight; a release frees one; zero means no cap.
func TestLimitQuicConnectionCap(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 0, 0, 2)
	addr := netip.MustParseAddr("2001:db8::7")

	// the request side at its cap doesn't touch the connection side
	requestReleases := []func(){}
	for i := range 2 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("request %d = %s", i, outcome)
		}
		requestReleases = append(requestReleases, release)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("a third request = %s, want concurrency", outcome)
	}
	quicReleases := []func(){}
	for i := range 2 {
		release, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("connection %d = %s", i, outcome)
		}
		quicReleases = append(quicReleases, release)
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect); outcome != limitOutcomeQuicRefused {
		t.Fatalf("a third connection = %s, want quic", outcome)
	}
	quicReleases[0]()
	// a second release of the same connection frees nothing more
	quicReleases[0]()
	release, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("after a release = %s", outcome)
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect); outcome != limitOutcomeQuicRefused {
		t.Fatalf("at the cap again = %s, want quic: a double release freed two", outcome)
	}
	release()
	quicReleases[1]()
	for _, requestRelease := range requestReleases {
		requestRelease()
	}

	noCap := newTestLimitInterface("eth2", 1, 0, 0)
	for i := range 100 {
		if _, outcome := limits.acquireQuicConnection(noCap, addr, limitQuicClassConnect); outcome != limitOutcomeAdmitted {
			t.Fatalf("no cap, connection %d = %s", i, outcome)
		}
	}
}

// At MaxAddressStates a new address replaces the least recently used idle
// state, which forgets that address's bucket, and keeps the others'. When
// every state has something in flight, a new address is refused.
func TestLimitStateCapEvictsTheLeastRecentlyUsedIdleState(t *testing.T) {
	settings := DefaultLimitSettings()
	settings.MaxAddressStates = 2
	limits, clock := newTestFrontLimits(t, settings)
	limitInterface := newTestLimitInterface("eth1", 1, 0, 0)
	addrA := netip.MustParseAddr("198.51.100.1")
	addrB := netip.MustParseAddr("198.51.100.2")
	addrC := netip.MustParseAddr("198.51.100.3")

	release, _, _ := limits.acquireRequest(limitInterface, addrA, nil, LimitIdentity{})
	release()
	clock.advance(time.Second)
	release, _, _ = limits.acquireRequest(limitInterface, addrB, nil, LimitIdentity{})
	release()
	clock.advance(time.Second)
	// A is the least recently used: C replaces it
	release, outcome, _ := limits.acquireRequest(limitInterface, addrC, nil, LimitIdentity{})
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("a new address at the cap = %s", outcome)
	}
	release()
	if count := limits.stateCount(); count != 2 {
		t.Fatalf("%d states, want the cap of 2", count)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addrB, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("B, still held = %s, want request", outcome)
	}
	// A's state, and its empty bucket, were forgotten
	if _, outcome, _ := limits.acquireRequest(limitInterface, addrA, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("A, evicted = %s, want a fresh bucket", outcome)
	}

	busySettings := DefaultLimitSettings()
	busySettings.MaxAddressStates = 2
	busyLimits, _ := newTestFrontLimits(t, busySettings)
	busyInterface := newTestLimitInterface("eth1", 0, 0, 0)
	releaseA, _, _ := busyLimits.acquireRequest(busyInterface, addrA, nil, LimitIdentity{})
	releaseB, _ := busyLimits.acquireQuicConnection(newTestLimitInterface("eth1", 0, 0, 1), addrB, limitQuicClassConnect)
	if _, outcome, _ := busyLimits.acquireRequest(busyInterface, addrC, nil, LimitIdentity{}); outcome != limitOutcomeStateRefused {
		t.Fatalf("a new address with every state in flight = %s, want state", outcome)
	}
	releaseA()
	if _, outcome, _ := busyLimits.acquireRequest(busyInterface, addrC, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("a new address once A is idle = %s", outcome)
	}
	releaseB()
}

// A maintenance pass yields the admission lock with work still pending.
func TestLimitSweepBoundsEachAdmissionLockHold(t *testing.T) {
	settings := DefaultLimitSettings()
	limits, clock := newTestFrontLimits(t, settings)
	limitInterface := newTestLimitInterface("eth1", 60, 0, 1)
	addr := netip.MustParseAddr("2001:db8::1")
	const addressCount = 2048
	for range addressCount {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatal(outcome)
		}
		release()
		addr = addr.Next()
	}

	clock.advance(2 * time.Minute)
	limits.sweep()
	remaining := limits.stateCount()
	if want := addressCount - settings.PruneBatchSize; remaining != want {
		t.Fatalf("first sweep left %d of %d addresses, want %d", remaining, addressCount, want)
	}
	release, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect)
	if outcome != limitOutcomeAdmitted {
		t.Fatal(outcome)
	}
	defer release()
	for limits.stateCount() > 1 {
		count := limits.stateCount()
		limits.sweep()
		if limits.stateCount() >= count {
			t.Fatal("sweep failed to advance past the previous batch")
		}
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect); outcome != limitOutcomeQuicRefused {
		t.Fatalf("sweep forgot the active connection: %s", outcome)
	}
}

// An expired address with an outstanding charge counts against the batch.
// Taking the next address out of idle order must preserve the rest of the walk.
func TestLimitSweepResumesPastDepletedAndRetouchedAddresses(t *testing.T) {
	settings := DefaultLimitSettings()
	settings.PruneBatchSize = 2
	limits, clock := newTestFrontLimits(t, settings)
	slow := newTestLimitInterface("slow", 1, 0, 1)
	fast := newTestLimitInterface("fast", 60, 0, 1)
	addr := netip.MustParseAddr("192.0.2.1")
	var retouched netip.Addr
	for i, limitInterface := range []*limitInterface{slow, slow, fast, fast, fast} {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatal(outcome)
		}
		release()
		if i == 2 {
			retouched = addr
		}
		addr = addr.Next()
	}
	clock.advance(61 * time.Second)
	if more := limits.sweep(); !more || limits.stateCount() != 5 {
		t.Fatalf("first batch should retain both depleted buckets, got %d states, more %t", limits.stateCount(), more)
	}
	release, outcome := limits.acquireQuicConnection(fast, retouched, limitQuicClassConnect)
	if outcome != limitOutcomeAdmitted {
		t.Fatal(outcome)
	}
	defer release()
	if more := limits.sweep(); more || limits.stateCount() != 3 {
		t.Fatalf("second batch should free the two remaining idle addresses, got %d states, more %t", limits.stateCount(), more)
	}
	clock.advance(2 * time.Second)
	if more := limits.sweep(); more || limits.stateCount() != 1 {
		t.Fatalf("after refill only the active connection should remain, got %d states, more %t", limits.stateCount(), more)
	}
}

// The sweep drops a state once it has been idle for StateIdleTimeout with
// its buckets refilled, and never one with something in flight.
func TestLimitSweepDropsIdleRefilledState(t *testing.T) {
	limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 60, 0, 0)
	// a token every 120 s
	routeZone := newLimitRouteZone("api", &RouteRateLimitRule{
		Path:              "/connect/control",
		RequestsPerMinute: 0.5,
		Burst:             0,
	})
	idleAddr := netip.MustParseAddr("198.51.100.1")
	routeAddr := netip.MustParseAddr("198.51.100.2")
	busyAddr := netip.MustParseAddr("198.51.100.3")

	release, _, _ := limits.acquireRequest(limitInterface, idleAddr, nil, LimitIdentity{})
	release()
	release, _, _ = limits.acquireRequest(limitInterface, routeAddr, routeZone, LimitIdentity{})
	release()
	limits.acquireRequest(limitInterface, busyAddr, nil, LimitIdentity{})

	clock.advance(30 * time.Second)
	limits.sweep()
	if count := limits.stateCount(); count != 3 {
		t.Fatalf("after 30 s, %d states, want 3: none idle long enough", count)
	}
	clock.advance(31 * time.Second)
	limits.sweep()
	// the shared bucket refills in 1 s, the route's in 120 s
	if count := limits.stateCount(); count != 2 {
		t.Fatalf("after 61 s, %d states, want the route's and the busy one", count)
	}
	clock.advance(time.Hour)
	limits.sweep()
	if count := limits.stateCount(); count != 1 {
		t.Fatalf("after an hour, %d states, want only the one in flight", count)
	}
}

// With a non-positive StateIdleTimeout, a release drops refilled state at
// once.
func TestLimitZeroIdleTimeoutDropsStateAtRelease(t *testing.T) {
	settings := DefaultLimitSettings()
	settings.StateIdleTimeout = 0
	limits, _ := newTestFrontLimits(t, settings)
	// no request rate: the bucket is always full
	limitInterface := newTestLimitInterface("eth1", 0, 0, 1)
	release, _, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("198.51.100.7"), nil, LimitIdentity{})
	if count := limits.stateCount(); count != 1 {
		t.Fatalf("in flight, %d states, want 1", count)
	}
	release()
	if count := limits.stateCount(); count != 0 {
		t.Fatalf("released, %d states, want 0", count)
	}
}

// An interface's limits with the identity limit on: identity_* values beside
// the address's.
func newTestIdentityLimitInterface(name string, requestsPerMinute float64, burst int, netConnections int, identityRequestsPerMinute float64, identityBurst int, identityAddressFactor int) *limitInterface {
	return newLimitInterface(&RouteInterface{
		Name: name,
		RateLimit: &RouteRateLimit{
			RequestsPerMinute:         requestsPerMinute,
			Burst:                     burst,
			NetConnections:            netConnections,
			ExcludeSubnets:            []string{},
			IdentityRequestsPerMinute: identityRequestsPerMinute,
			IdentityBurst:             identityBurst,
			IdentityAddressFactor:     identityAddressFactor,
		},
	})
}

// A client's identity, from a fixed byte so a test can name several.
func newTestLimitIdentity(b byte) LimitIdentity {
	identity := LimitIdentity{}
	identity.Id[0] = b
	identity.Id[15] = 1
	return identity
}

// A Retry-After is the wait in whole seconds, rounded up, from 1 to 60, and
// 1 for a refusal that reports no wait.
func TestLimitRetryAfterSecondsRoundsUpAndClamps(t *testing.T) {
	for _, c := range []struct {
		wait    time.Duration
		seconds int
	}{
		{wait: 0, seconds: 1},
		{wait: -time.Second, seconds: 1},
		{wait: time.Millisecond, seconds: 1},
		{wait: 134 * time.Millisecond, seconds: 1},
		{wait: time.Second, seconds: 1},
		{wait: time.Second + time.Millisecond, seconds: 2},
		{wait: 2 * time.Second, seconds: 2},
		{wait: 59*time.Second + 500*time.Millisecond, seconds: 60},
		{wait: 60 * time.Second, seconds: 60},
		{wait: 62500 * time.Millisecond, seconds: 60},
		{wait: limitMaxWait, seconds: 60},
	} {
		if seconds := limitRetryAfterSeconds(c.wait); seconds != c.seconds {
			t.Errorf("limitRetryAfterSeconds(%s) = %d, want %d", c.wait, seconds, c.seconds)
		}
	}
}

// A refused bucket's wait is exact: one millisecond less still refuses, and
// the wait itself admits, across integer and fractional rates, bursts, and
// a bucket refused part way into its refill. Each is the wait of the
// bucket's own arithmetic, nginx's, so the rounding of the division can't
// move a wait across a whole second.
func TestLimitBucketWaitIsTheTimeUntilAdmission(t *testing.T) {
	for _, c := range []struct {
		requestsPerMinute float64
		burst             int
		// time after the bucket emptied when the refusal is read
		laterMs int64
		// the wait, when the case pins it
		wantMs int64
	}{
		// main's values: one token per 133.3 ms
		{requestsPerMinute: 450, burst: 150, wantMs: 134},
		{requestsPerMinute: 450, burst: 150, laterMs: 100, wantMs: 34},
		// a whole second per token, so the wait ends on a second
		{requestsPerMinute: 60, burst: 0, wantMs: 1000},
		{requestsPerMinute: 30, burst: 2, wantMs: 2000},
		{requestsPerMinute: 600, burst: 3, wantMs: 100},
		// nginx's 16 milli-requests per second for 1 r/m
		{requestsPerMinute: 1, burst: 0, wantMs: 62500},
		// fractional rates, which nginx can't configure but the schema can
		{requestsPerMinute: 0.5, burst: 0},
		{requestsPerMinute: 0.7, burst: 1},
		{requestsPerMinute: 450.5, burst: 150},
		{requestsPerMinute: 12000, burst: 6000, wantMs: 5},
	} {
		limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
		limitInterface := newTestLimitInterface("eth1", c.requestsPerMinute, c.burst, 0)
		addr := netip.MustParseAddr("198.51.100.7")
		for i := range c.burst + 1 {
			release, outcome, wait := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
			if outcome != limitOutcomeAdmitted || wait != 0 {
				t.Fatalf("%v r/m burst %d: request %d = %s, wait %s", c.requestsPerMinute, c.burst, i, outcome, wait)
			}
			release()
		}
		clock.advance(time.Duration(c.laterMs) * time.Millisecond)
		_, outcome, wait := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeRequestRefused {
			t.Fatalf("%v r/m burst %d: past the burst = %s, want request", c.requestsPerMinute, c.burst, outcome)
		}
		if wait < time.Millisecond || wait%time.Millisecond != 0 {
			t.Fatalf("%v r/m burst %d: wait %s, want whole milliseconds", c.requestsPerMinute, c.burst, wait)
		}
		if c.wantMs != 0 && wait != time.Duration(c.wantMs)*time.Millisecond {
			t.Errorf("%v r/m burst %d after %d ms: wait %s, want %d ms", c.requestsPerMinute, c.burst, c.laterMs, wait, c.wantMs)
		}
		clock.advance(wait - time.Millisecond)
		if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
			t.Fatalf("%v r/m burst %d: a millisecond before the wait = %s, want request", c.requestsPerMinute, c.burst, outcome)
		}
		clock.advance(time.Millisecond)
		if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
			t.Fatalf("%v r/m burst %d: at the wait = %s, want admitted", c.requestsPerMinute, c.burst, outcome)
		}
	}
}

// Each refusal reports its wait: a route zone's from its own bucket, the
// shared zone's from the shared bucket, and none for the concurrency cap and
// the state cap, whose refusals are 1 s Retry-Afters.
func TestLimitRefusalsReportTheRefusedBucketsWait(t *testing.T) {
	settings := DefaultLimitSettings()
	settings.MaxAddressStates = 1
	limits, _ := newTestFrontLimits(t, settings)
	// two seconds per shared token, one in flight
	limitInterface := newTestLimitInterface("eth1", 30, 0, 1)
	// four seconds per route token
	routeZone := newLimitRouteZone("api", &RouteRateLimitRule{Path: "/connect/control", RequestsPerMinute: 15})
	addr := netip.MustParseAddr("198.51.100.7")

	release, _, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
	release()
	if _, outcome, wait := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused || wait != 2*time.Second {
		t.Fatalf("shared = %s, wait %s, want request after 2s", outcome, wait)
	}
	routeRelease, _, _ := limits.acquireRequest(limitInterface, addr, routeZone, LimitIdentity{})
	if _, outcome, wait := limits.acquireRequest(limitInterface, addr, routeZone, LimitIdentity{}); outcome != limitOutcomeRouteRefused || wait != 4*time.Second {
		t.Fatalf("route = %s, wait %s, want route after 4s", outcome, wait)
	}
	// the route request still in flight: the cap refuses, with no wait
	noRate := newTestLimitInterface("eth1", 0, 0, 1)
	if _, outcome, wait := limits.acquireRequest(noRate, addr, nil, LimitIdentity{}); outcome != limitOutcomeConcurrencyRefused || wait != 0 {
		t.Fatalf("cap = %s, wait %s, want concurrency with no wait", outcome, wait)
	}
	// and the one state is in flight, so another address has none
	if _, outcome, wait := limits.acquireRequest(noRate, netip.MustParseAddr("198.51.100.8"), nil, LimitIdentity{}); outcome != limitOutcomeStateRefused || wait != 0 {
		t.Fatalf("state = %s, wait %s, want state with no wait", outcome, wait)
	}
	routeRelease()
}

// A bucket too slow to compute a wait with still reports one, the longest.
func TestLimitBucketWaitIsBounded(t *testing.T) {
	bucket := &limitBucket{
		charged: 1000,
		lastMs:  0,
	}
	if wait := bucket.waitWithLock(1e-9, 0, 1); wait != limitMaxWait {
		t.Fatalf("wait %s, want %s", wait, limitMaxWait)
	}
	if seconds := limitRetryAfterSeconds(bucket.waitWithLock(1e-9, 0, 1)); seconds != limitMaxRetryAfterSeconds {
		t.Fatalf("Retry-After %d, want %d", seconds, limitMaxRetryAfterSeconds)
	}
}

// Each class of QUIC connection has the cap to itself: an address at its api
// cap still gets connect connections, and one at its connect cap api ones,
// and a release frees only its own class's slot. The address's state stays
// while any class holds a connection.
func TestLimitQuicConnectionCapIsPerClass(t *testing.T) {
	limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 0, 0, 1)
	addr := netip.MustParseAddr("192.0.2.31")

	apiRelease, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassApi)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("api = %s", outcome)
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassApi); outcome != limitOutcomeQuicRefused {
		t.Fatalf("api at its cap = %s, want quic", outcome)
	}
	connectRelease, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("connect with the api cap full = %s, want admitted", outcome)
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect); outcome != limitOutcomeQuicRefused {
		t.Fatalf("connect at its cap = %s, want quic", outcome)
	}
	apiRelease()
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr, limitQuicClassConnect); outcome != limitOutcomeQuicRefused {
		t.Fatalf("connect after the api release = %s, want quic: the api slot freed a connect one", outcome)
	}

	// idle for an hour but for the connect connection: kept
	clock.advance(time.Hour)
	limits.sweep()
	if count := limits.stateCount(); count != 1 {
		t.Fatalf("%d states with a connection open, want 1", count)
	}
	connectRelease()
	clock.advance(time.Hour)
	limits.sweep()
	if count := limits.stateCount(); count != 0 {
		t.Fatalf("%d states once every class closed, want 0", count)
	}
	if _, ok := limitQuicClassOf(NameClassForwarded); ok {
		t.Fatal("a forwarded name has a QUIC class")
	}
}

// With the identity limit on, a request with an identity is charged to its
// identity's bucket, so clients behind one address each get the identity's
// budget, where without it they shared the address's; one identity on two
// addresses shares its own. A request without an identity on the same
// address is still charged to the address's shared bucket, which identities
// never touch.
func TestLimitIdentityIsChargedPerIdentity(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	// the address: two requests, then one a minute; an identity: three, then
	// one a second; the ceiling is ten addresses'
	limitInterface := newTestIdentityLimitInterface("eth1", 1, 1, 0, 60, 2, 10)
	addr := netip.MustParseAddr("198.51.100.7")
	otherAddr := netip.MustParseAddr("198.51.100.8")
	identityA := newTestLimitIdentity(1)
	identityB := newTestLimitIdentity(2)

	for _, identity := range []LimitIdentity{identityA, identityB} {
		for i := range 3 {
			release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, identity)
			if outcome != limitOutcomeAdmitted {
				t.Fatalf("identity %d request %d = %s, want its own budget", identity.Id[0], i, outcome)
			}
			release()
		}
		_, outcome, wait := limits.acquireRequest(limitInterface, addr, nil, identity)
		if outcome != limitOutcomeIdentityRefused || wait != time.Second {
			t.Fatalf("identity %d past its burst = %s, wait %s, want identity after 1s", identity.Id[0], outcome, wait)
		}
	}
	// A's bucket follows it to another address
	if _, outcome, _ := limits.acquireRequest(limitInterface, otherAddr, nil, identityA); outcome != limitOutcomeIdentityRefused {
		t.Fatalf("identity A on another address = %s, want identity", outcome)
	}
	// the address's shared bucket is untouched: two requests without an
	// identity, then a refusal
	for i := range 2 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("no identity, request %d = %s", i, outcome)
		}
		release()
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeRequestRefused {
		t.Fatalf("no identity, past the burst = %s, want request", outcome)
	}
	if count := limits.identityStateCount(); count != 2 {
		t.Fatalf("%d identity states, want 2", count)
	}
}

// The address's ceiling still bounds one address: requests with identities,
// however many, are refused once the address's ceiling bucket, the shared
// rate and burst times identity_address_factor, is empty, with the
// ceiling's wait. Another address has a ceiling of its own.
func TestLimitIdentityAddressCeilingBoundsOneAddress(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	// the address: two tokens, then 30 a minute; times 2, a ceiling of a
	// burst of two, three tokens, then 60 a minute; each identity far more
	limitInterface := newTestIdentityLimitInterface("eth1", 30, 1, 0, 6000, 100, 2)
	addr := netip.MustParseAddr("198.51.100.7")
	otherAddr := netip.MustParseAddr("198.51.100.8")

	for i := range 3 {
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, newTestLimitIdentity(byte(i+1)))
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("identity %d = %s", i+1, outcome)
		}
		release()
	}
	_, outcome, wait := limits.acquireRequest(limitInterface, addr, nil, newTestLimitIdentity(5))
	if outcome != limitOutcomeCeilingRefused || wait != time.Second {
		t.Fatalf("a fourth identity on the address = %s, wait %s, want ceiling after 1s", outcome, wait)
	}
	if !outcome.identity() || limitRefusalZoneLabel(nil, outcome) != limitIdentityZone {
		t.Fatalf("a ceiling refusal counts in zone %q, want %q", limitRefusalZoneLabel(nil, outcome), limitIdentityZone)
	}
	// the refusal charged nothing: identity 5's bucket is full
	release, outcome, _ := limits.acquireRequest(limitInterface, otherAddr, nil, newTestLimitIdentity(5))
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("identity 5 on another address = %s, want admitted", outcome)
	}
	release()
	// the shared bucket, for requests without an identity, is apart
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeAdmitted {
		t.Fatalf("no identity on the address at its ceiling = %s, want the shared bucket's", outcome)
	}
}

// Requests with an identity in flight are held to net_connections per
// identity and to net_connections times the factor per address, counted
// apart from requests without one, which keep their own cap.
func TestLimitIdentityConcurrency(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	// no rates; one in flight per identity and per address, two per
	// address's ceiling
	limitInterface := newTestIdentityLimitInterface("eth1", 0, 0, 1, 6000, 100, 2)
	addr := netip.MustParseAddr("198.51.100.7")
	identityA := newTestLimitIdentity(1)
	identityB := newTestLimitIdentity(2)

	releaseA, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, identityA)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("A = %s", outcome)
	}
	if _, outcome, wait := limits.acquireRequest(limitInterface, addr, nil, identityA); outcome != limitOutcomeIdentityConcurrencyRefused || wait != 0 {
		t.Fatalf("A twice = %s, wait %s, want identity_concurrency with no wait", outcome, wait)
	}
	releaseB, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, identityB)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("B beside A = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, newTestLimitIdentity(3)); outcome != limitOutcomeCeilingConcurrencyRefused {
		t.Fatalf("a third identity = %s, want ceiling_concurrency", outcome)
	}
	release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{})
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("no identity with the ceiling full = %s, want its own cap", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, LimitIdentity{}); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("a second without an identity = %s, want concurrency", outcome)
	}
	release()
	releaseA()
	// a second release of A frees nothing more
	releaseA()
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, newTestLimitIdentity(3)); outcome != limitOutcomeAdmitted {
		t.Fatalf("a third identity once A ended = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, newTestLimitIdentity(4)); outcome != limitOutcomeCeilingConcurrencyRefused {
		t.Fatalf("a fourth = %s, want ceiling_concurrency: a double release freed two", outcome)
	}
	releaseB()
}

// Off, the identity limit changes nothing: an identity passed on an
// interface without identity_requests_per_minute is charged as nginx's
// limit_req would charge its address, request for request, and keeps no
// identity state. On, a route zone's request is still charged to the route
// zone alone.
func TestLimitIdentityOffChargesTheAddressAsBefore(t *testing.T) {
	limits, clock := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 600, 3, 0)
	if limitInterface.identityEnabled() {
		t.Fatal("an interface without identity fields has the identity limit on")
	}
	addr := netip.MustParseAddr("198.51.100.7")
	reference := &nginxLimitReqNode{}
	random := rand.New(rand.NewPCG(3, 4))
	elapsedMs := int64(0)
	for i := range 500 {
		if 0 < i {
			gapMs := int64(25 * random.IntN(17))
			elapsedMs += gapMs
			clock.advance(time.Duration(gapMs) * time.Millisecond)
		}
		want := reference.lookup(600, 3, elapsedMs)
		release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, newTestLimitIdentity(byte(i%7+1)))
		if got := outcome == limitOutcomeAdmitted; got != want {
			t.Fatalf("request %d at %d ms: admitted = %t, nginx admits = %t", i, elapsedMs, got, want)
		}
		if release != nil {
			release()
		}
	}
	if count := limits.identityStateCount(); count != 0 {
		t.Fatalf("%d identity states with the identity limit off, want 0", count)
	}

	on := newTestIdentityLimitInterface("eth2", 1, 0, 0, 6000, 100, 10)
	routeZone := newLimitRouteZone("api", &RouteRateLimitRule{Path: "/connect/control", RequestsPerMinute: 1})
	if _, outcome, _ := limits.acquireRequest(on, addr, routeZone, newTestLimitIdentity(1)); outcome != limitOutcomeAdmitted {
		t.Fatalf("route zone, first = %s", outcome)
	}
	if _, outcome, _ := limits.acquireRequest(on, addr, routeZone, newTestLimitIdentity(1)); outcome != limitOutcomeRouteRefused {
		t.Fatalf("route zone, second = %s, want route: an identity doesn't replace a route zone", outcome)
	}
	if count := limits.identityStateCount(); count != 0 {
		t.Fatalf("%d identity states from route zone requests, want 0", count)
	}
}

// Identity state is bounded as address state is: at MaxIdentityStates a new
// identity replaces the least recently used idle one, which forgets its
// bucket; with every identity in flight, a new identity is charged as a
// request without one, to its address's shared bucket, and creates no
// state. The sweep drops idle, refilled identities.
func TestLimitIdentityStateIsBounded(t *testing.T) {
	settings := DefaultLimitSettings()
	settings.MaxIdentityStates = 2
	limits, clock := newTestFrontLimits(t, settings)
	// an identity: one request a minute, with no cap in flight; the address's
	// ceiling: eleven at once
	limitInterface := newTestIdentityLimitInterface("eth1", 1, 1, 0, 1, 0, 10)
	addr := netip.MustParseAddr("198.51.100.7")
	identityA := newTestLimitIdentity(1)
	identityB := newTestLimitIdentity(2)
	identityC := newTestLimitIdentity(3)

	release, _, _ := limits.acquireRequest(limitInterface, addr, nil, identityA)
	release()
	clock.advance(time.Second)
	release, _, _ = limits.acquireRequest(limitInterface, addr, nil, identityB)
	release()
	clock.advance(time.Second)
	// A is the least recently used: C replaces it
	release, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, identityC)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("C at the cap = %s", outcome)
	}
	release()
	if count := limits.identityStateCount(); count != 2 {
		t.Fatalf("%d identity states, want the cap of 2", count)
	}
	if _, outcome, _ := limits.acquireRequest(limitInterface, addr, nil, identityB); outcome != limitOutcomeIdentityRefused {
		t.Fatalf("B, still held = %s, want identity", outcome)
	}

	// B and C in flight: a new identity has no state, and is charged to the
	// address's shared bucket, twice, then refused as the address is
	busySettings := DefaultLimitSettings()
	busySettings.MaxIdentityStates = 2
	busyLimits, busyClock := newTestFrontLimits(t, busySettings)
	busyInterface := newTestIdentityLimitInterface("eth1", 1, 1, 0, 6000, 100, 10)
	releaseB, outcome, _ := busyLimits.acquireRequest(busyInterface, addr, nil, identityB)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("busy B = %s", outcome)
	}
	releaseC, outcome, _ := busyLimits.acquireRequest(busyInterface, addr, nil, identityC)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("busy C = %s", outcome)
	}
	for i := range 2 {
		if _, outcome, _ := busyLimits.acquireRequest(busyInterface, addr, nil, identityA); outcome != limitOutcomeAdmitted {
			t.Fatalf("A with every identity in flight, request %d = %s, want the shared bucket's admission", i, outcome)
		}
	}
	if _, outcome, _ := busyLimits.acquireRequest(busyInterface, addr, nil, identityA); outcome != limitOutcomeRequestRefused {
		t.Fatalf("A a third time = %s, want request: charged as without an identity", outcome)
	}
	if count := busyLimits.identityStateCount(); count != 2 {
		t.Fatalf("%d identity states, want 2: a full table makes none", count)
	}
	releaseB()
	releaseC()
	busyClock.advance(2 * time.Minute)
	busyLimits.sweep()
	if count := busyLimits.identityStateCount(); count != 0 {
		t.Fatalf("%d identity states idle and refilled after the sweep, want 0", count)
	}
}

// With a non-positive StateIdleTimeout, the last release of an identity
// drops its refilled state at once, as an address's.
func TestLimitZeroIdleTimeoutDropsIdentityStateAtRelease(t *testing.T) {
	settings := DefaultLimitSettings()
	settings.StateIdleTimeout = 0
	limits, clock := newTestFrontLimits(t, settings)
	limitInterface := newTestIdentityLimitInterface("eth1", 0, 0, 1, 60, 0, 1)
	release, outcome, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("198.51.100.7"), nil, newTestLimitIdentity(1))
	if outcome != limitOutcomeAdmitted {
		t.Fatal(outcome)
	}
	clock.advance(time.Second)
	release()
	if count := limits.identityStateCount(); count != 0 {
		t.Fatalf("released and refilled, %d identity states, want 0", count)
	}
	if count := limits.stateCount(); count != 0 {
		t.Fatalf("released and refilled, %d address states, want 0", count)
	}
}
