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
		release, outcome := limits.acquireRequest(limitInterface, addr, nil)
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
			release, outcome := limits.acquireRequest(limitInterface, addr, zone)
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
		release, outcome := limits.acquireRequest(limitInterface, addr, nil)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("request %d of the burst refused as %s", i, outcome)
		}
		release()
	}
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeRequestRefused {
		t.Fatalf("request past burst+1 = %s, want request", outcome)
	}

	// a token every 133.3 ms
	clock.advance(66 * time.Millisecond)
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeRequestRefused {
		t.Fatalf("after half a refill interval = %s, want request", outcome)
	}
	clock.advance(68 * time.Millisecond)
	release, outcome := limits.acquireRequest(limitInterface, addr, nil)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("after one refill interval = %s, want admitted", outcome)
	}
	release()
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeRequestRefused {
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

	releaseFirst, outcome := limits.acquireRequest(limitInterface, addr, nil)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("first request = %s", outcome)
	}
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("a second request in flight = %s, want concurrency", outcome)
	}
	releaseFirst()
	// the refused request took the bucket's second token
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeRequestRefused {
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
		release, outcome := limits.acquireRequest(noRate, addr, nil)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("no rate, request %d = %s", i, outcome)
		}
		releases = append(releases, release)
	}
	if _, outcome := limits.acquireRequest(noRate, addr, nil); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("no rate, past the cap = %s, want concurrency", outcome)
	}
	for _, release := range releases {
		release()
	}

	noCap := newTestLimitInterface("eth2", 0, 0, 0)
	for i := range 1000 {
		if _, outcome := limits.acquireRequest(noCap, addr, nil); outcome != limitOutcomeAdmitted {
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

	release, outcome := limits.acquireRequest(limitInterface, addr, nil)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("shared = %s", outcome)
	}
	release()
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeRequestRefused {
		t.Fatalf("shared, empty = %s, want request", outcome)
	}
	routeReleases := []func(){}
	for i := range 2 {
		release, outcome := limits.acquireRequest(limitInterface, addr, routeZone)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("route request %d with the shared zone empty = %s", i, outcome)
		}
		routeReleases = append(routeReleases, release)
	}
	// two in flight: the cap counts route requests
	if _, outcome := limits.acquireRequest(limitInterface, addr, routeZone); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("route request past the cap = %s, want concurrency", outcome)
	}
	for _, release := range routeReleases {
		release()
	}
	// that refusal spent the route bucket's last token
	if _, outcome := limits.acquireRequest(limitInterface, addr, routeZone); outcome != limitOutcomeRouteRefused {
		t.Fatalf("route, empty = %s, want route", outcome)
	}
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeRequestRefused {
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

	if _, outcome := limits.acquireRequest(eth1, addr, nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("eth1 = %s", outcome)
	}
	if _, outcome := limits.acquireRequest(eth1, addr, nil); outcome != limitOutcomeRequestRefused {
		t.Fatalf("eth1 again = %s, want request", outcome)
	}
	if _, outcome := limits.acquireRequest(eth2, addr, nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("the same source on eth2 = %s, want its own quota", outcome)
	}
	if _, outcome := limits.acquireRequest(eth1, otherAddr, nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("another source on eth1 = %s, want its own quota", outcome)
	}
}

// An IPv4-mapped IPv6 address is its IPv4 address, and a zone is ignored.
func TestLimitNormalizesTheAddress(t *testing.T) {
	limits, _ := newTestFrontLimits(t, DefaultLimitSettings())
	limitInterface := newTestLimitInterface("eth1", 1, 0, 0)

	if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr("::ffff:198.51.100.7"), nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("mapped = %s", outcome)
	}
	if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr("198.51.100.7"), nil); outcome != limitOutcomeRequestRefused {
		t.Fatalf("the same address unmapped = %s, want request", outcome)
	}
	if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr("2001:db8::7%eth0"), nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("zoned = %s", outcome)
	}
	if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr("2001:db8::7"), nil); outcome != limitOutcomeRequestRefused {
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
			if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr(addr), nil); outcome != limitOutcomeAdmitted {
				t.Fatalf("%s request %d = %s, want admitted", addr, i, outcome)
			}
			if _, outcome := limits.acquireQuicConnection(limitInterface, netip.MustParseAddr(addr)); outcome != limitOutcomeAdmitted {
				t.Fatalf("%s quic connection %d = %s, want admitted", addr, i, outcome)
			}
		}
	}
	if count := limits.stateCount(); count != 0 {
		t.Fatalf("excluded addresses keep %d states, want 0", count)
	}
	if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr("203.0.114.5"), nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("outside the subnet = %s", outcome)
	}
	if _, outcome := limits.acquireRequest(limitInterface, netip.MustParseAddr("203.0.114.5"), nil); outcome != limitOutcomeRequestRefused {
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
		release, outcome := limits.acquireRequest(limitInterface, addr, nil)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("request %d = %s", i, outcome)
		}
		requestReleases = append(requestReleases, release)
	}
	if _, outcome := limits.acquireRequest(limitInterface, addr, nil); outcome != limitOutcomeConcurrencyRefused {
		t.Fatalf("a third request = %s, want concurrency", outcome)
	}
	quicReleases := []func(){}
	for i := range 2 {
		release, outcome := limits.acquireQuicConnection(limitInterface, addr)
		if outcome != limitOutcomeAdmitted {
			t.Fatalf("connection %d = %s", i, outcome)
		}
		quicReleases = append(quicReleases, release)
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr); outcome != limitOutcomeQuicRefused {
		t.Fatalf("a third connection = %s, want quic", outcome)
	}
	quicReleases[0]()
	// a second release of the same connection frees nothing more
	quicReleases[0]()
	release, outcome := limits.acquireQuicConnection(limitInterface, addr)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("after a release = %s", outcome)
	}
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr); outcome != limitOutcomeQuicRefused {
		t.Fatalf("at the cap again = %s, want quic: a double release freed two", outcome)
	}
	release()
	quicReleases[1]()
	for _, requestRelease := range requestReleases {
		requestRelease()
	}

	noCap := newTestLimitInterface("eth2", 1, 0, 0)
	for i := range 100 {
		if _, outcome := limits.acquireQuicConnection(noCap, addr); outcome != limitOutcomeAdmitted {
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

	release, _ := limits.acquireRequest(limitInterface, addrA, nil)
	release()
	clock.advance(time.Second)
	release, _ = limits.acquireRequest(limitInterface, addrB, nil)
	release()
	clock.advance(time.Second)
	// A is the least recently used: C replaces it
	release, outcome := limits.acquireRequest(limitInterface, addrC, nil)
	if outcome != limitOutcomeAdmitted {
		t.Fatalf("a new address at the cap = %s", outcome)
	}
	release()
	if count := limits.stateCount(); count != 2 {
		t.Fatalf("%d states, want the cap of 2", count)
	}
	if _, outcome := limits.acquireRequest(limitInterface, addrB, nil); outcome != limitOutcomeRequestRefused {
		t.Fatalf("B, still held = %s, want request", outcome)
	}
	// A's state, and its empty bucket, were forgotten
	if _, outcome := limits.acquireRequest(limitInterface, addrA, nil); outcome != limitOutcomeAdmitted {
		t.Fatalf("A, evicted = %s, want a fresh bucket", outcome)
	}

	busySettings := DefaultLimitSettings()
	busySettings.MaxAddressStates = 2
	busyLimits, _ := newTestFrontLimits(t, busySettings)
	busyInterface := newTestLimitInterface("eth1", 0, 0, 0)
	releaseA, _ := busyLimits.acquireRequest(busyInterface, addrA, nil)
	releaseB, _ := busyLimits.acquireQuicConnection(newTestLimitInterface("eth1", 0, 0, 1), addrB)
	if _, outcome := busyLimits.acquireRequest(busyInterface, addrC, nil); outcome != limitOutcomeStateRefused {
		t.Fatalf("a new address with every state in flight = %s, want state", outcome)
	}
	releaseA()
	if _, outcome := busyLimits.acquireRequest(busyInterface, addrC, nil); outcome != limitOutcomeAdmitted {
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
		release, outcome := limits.acquireRequest(limitInterface, addr, nil)
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
	release, outcome := limits.acquireQuicConnection(limitInterface, addr)
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
	if _, outcome := limits.acquireQuicConnection(limitInterface, addr); outcome != limitOutcomeQuicRefused {
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
		release, outcome := limits.acquireRequest(limitInterface, addr, nil)
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
	release, outcome := limits.acquireQuicConnection(fast, retouched)
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

	release, _ := limits.acquireRequest(limitInterface, idleAddr, nil)
	release()
	release, _ = limits.acquireRequest(limitInterface, routeAddr, routeZone)
	release()
	limits.acquireRequest(limitInterface, busyAddr, nil)

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
	release, _ := limits.acquireRequest(limitInterface, netip.MustParseAddr("198.51.100.7"), nil)
	if count := limits.stateCount(); count != 1 {
		t.Fatalf("in flight, %d states, want 1", count)
	}
	release()
	if count := limits.stateCount(); count != 0 {
		t.Fatalf("released, %d states, want 0", count)
	}
}
