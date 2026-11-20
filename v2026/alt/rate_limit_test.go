package alt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func testLimitsSettings(t testing.TB) *LimitsSettings {
	popResource := server.Vault.PushSimpleResource(servicesResourceName, []byte(testServicesYml))
	defer popResource()
	rateLimit, err := loadDefaultRateLimit()
	if err != nil {
		t.Fatal(err)
	}
	return LimitsSettingsFromRateLimit(rateLimit)
}

// The limits come from the same `default_rate_limit` block warpctl renders
// into nginx, in the units the limiter uses.
func TestLimitsSettingsComeFromTheServicesRateLimitBlock(t *testing.T) {
	settings := testLimitsSettings(t)
	if settings.RequestsPerSecond != 2 {
		t.Fatalf("requests per second = %f, want 2", settings.RequestsPerSecond)
	}
	if settings.Burst != 2 || settings.NetConnections != 2 {
		t.Fatalf("burst = %d, net connections = %d, want 2 and 2", settings.Burst, settings.NetConnections)
	}
	wantPrefixes := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	if !slices.Equal(settings.ExcludePrefixes, wantPrefixes) {
		t.Fatalf("exclude prefixes = %v, want %v", settings.ExcludePrefixes, wantPrefixes)
	}
}

// A services.yml with no anchor block falls back to the warp default, which
// is what nginx applies to a block that declares no rate limit of its own.
func TestLimitsSettingsFallBackToTheWarpDefault(t *testing.T) {
	popResource := server.Vault.PushSimpleResource(servicesResourceName, []byte("versions:\n-   services: {}\n"))
	defer popResource()
	if _, err := loadDefaultRateLimit(); err == nil {
		t.Fatal("a services.yml with no default_rate_limit was accepted")
	}
	settings := DefaultLimitsSettings()
	if settings.RequestsPerSecond != 2 || settings.Burst != 120 {
		t.Fatalf("default requests per second = %f, burst = %d", settings.RequestsPerSecond, settings.Burst)
	}
}

// nginx admits the first request of a new key and then allows an excess of at
// most burst, so a full bucket is burst+1 requests. Refilling at the
// configured rate admits exactly one more.
func TestLimitsRequestBucketHoldsTheBurstAndRefillsAtTheRate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits := NewLimits(ctx, testLimitsSettings(t))
	addr := netip.MustParseAddr("198.51.100.7")

	now := time.Now()
	for i := range 3 {
		release, ok := limits.AcquireRequest(addr, now)
		if !ok {
			t.Fatalf("request %d of the burst was refused", i)
		}
		release()
	}
	if _, ok := limits.AcquireRequest(addr, now); ok {
		t.Fatal("a request above the burst was admitted")
	}

	// one token at 2/s
	release, ok := limits.AcquireRequest(addr, now.Add(500*time.Millisecond))
	if !ok {
		t.Fatal("the refilled token was refused")
	}
	release()
	if _, ok := limits.AcquireRequest(addr, now.Add(500*time.Millisecond)); ok {
		t.Fatal("a second request in the same refill interval was admitted")
	}
}

// The concurrency caps count an address's live api requests and its live
// connect connections separately, at the same `net_connections` limit.
func TestLimitsCapConcurrentRequestsAndConnectionsPerAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := testLimitsSettings(t)
	// keep the bucket out of this test's way
	settings.RequestsPerSecond = 0
	limits := NewLimits(ctx, settings)
	addr := netip.MustParseAddr("198.51.100.8")
	otherAddr := netip.MustParseAddr("198.51.100.9")

	now := time.Now()
	releaseFirst, ok := limits.AcquireRequest(addr, now)
	if !ok {
		t.Fatal("the first request was refused")
	}
	releaseSecond, ok := limits.AcquireRequest(addr, now)
	if !ok {
		t.Fatal("the second request was refused")
	}
	if _, ok := limits.AcquireRequest(addr, now); ok {
		t.Fatal("a request above the concurrency cap was admitted")
	}
	// the connection counter is independent of the request counter
	releaseConnection, ok := limits.AcquireConnection(addr, now)
	if !ok {
		t.Fatal("the first connection was refused")
	}
	releaseOtherConnection, ok := limits.AcquireConnection(addr, now)
	if !ok {
		t.Fatal("the second connection was refused")
	}
	if _, ok := limits.AcquireConnection(addr, now); ok {
		t.Fatal("a connection above the concurrency cap was admitted")
	}
	// another address has its own counters
	releaseOther, ok := limits.AcquireRequest(otherAddr, now)
	if !ok {
		t.Fatal("another address was refused")
	}
	releaseOther()

	releaseFirst()
	release, ok := limits.AcquireRequest(addr, now)
	if !ok {
		t.Fatal("a request was refused after a slot was released")
	}
	release()
	releaseSecond()
	releaseConnection()
	releaseOtherConnection()
	release, ok = limits.AcquireConnection(addr, now)
	if !ok {
		t.Fatal("a connection was refused after a slot was released")
	}
	release()
}

// An address inside `exclude_subnets` bypasses every counter, exactly as
// nginx maps an excluded prefix to an empty limit key.
func TestLimitsExcludeTheConfiguredSubnets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits := NewLimits(ctx, testLimitsSettings(t))

	now := time.Now()
	for _, addr := range []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("2001:db8::10"),
	} {
		if !limits.Excluded(addr) {
			t.Fatalf("%s is not excluded", addr)
		}
		// well past the burst and the concurrency cap
		releases := []func(){}
		for i := range 16 {
			release, ok := limits.AcquireRequest(addr, now)
			if !ok {
				t.Fatalf("excluded %s request %d was refused", addr, i)
			}
			releases = append(releases, release)
			release, ok = limits.AcquireConnection(addr, now)
			if !ok {
				t.Fatalf("excluded %s connection %d was refused", addr, i)
			}
			releases = append(releases, release)
		}
		for _, release := range releases {
			release()
		}
	}
	if limits.Excluded(netip.MustParseAddr("198.51.100.10")) {
		t.Fatal("an ordinary address is excluded")
	}
}

// The api front answers 429 above the bucket, which is the status the
// rendered nginx `limit_req` and `limit_conn` directives both return.
func TestLimitedHandlerAnswers429AboveTheBucket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := 0
	handler := NewLimitedHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served += 1
			w.WriteHeader(http.StatusOK)
		}),
		NewLimits(ctx, testLimitsSettings(t)),
	)

	request := func(remoteAddr string) int {
		r := httptest.NewRequest(http.MethodGet, "/hello", nil)
		r.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := range 3 {
		if code := request("198.51.100.11:4000"); code != http.StatusOK {
			t.Fatalf("request %d of the burst = %d", i, code)
		}
	}
	if code := request("198.51.100.11:4000"); code != http.StatusTooManyRequests {
		t.Fatalf("request above the burst = %d, want 429", code)
	}
	if served != 3 {
		t.Fatalf("served = %d, want 3", served)
	}
	// an excluded address is never refused
	for i := range 8 {
		if code := request("192.0.2.11:4000"); code != http.StatusOK {
			t.Fatalf("excluded request %d = %d", i, code)
		}
	}
}

// Idle address state carries no decision, so it is discarded rather than
// retained for every address ever seen.
func TestLimitsSweepDiscardsIdleAddressState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits := NewLimits(ctx, testLimitsSettings(t))
	addr := netip.MustParseAddr("198.51.100.12")

	now := time.Now()
	release, ok := limits.AcquireRequest(addr, now)
	if !ok {
		t.Fatal("the first request was refused")
	}
	// a live request keeps its state
	addrCount := func() int {
		limits.stateLock.Lock()
		defer limits.stateLock.Unlock()
		return len(limits.addrLimits)
	}
	limits.sweep(now)
	if count := addrCount(); count != 1 {
		t.Fatalf("live address state = %d entries, want 1", count)
	}
	release()
	// the bucket is not yet full, so the state still carries a decision
	if count := addrCount(); count != 1 {
		t.Fatalf("spent address state = %d entries, want 1", count)
	}
	limits.sweep(now.Add(time.Minute))
	if count := addrCount(); count != 0 {
		t.Fatalf("idle address state = %d entries, want 0", count)
	}
}

// The block is read the way the nginx template reads it: `requests_per_minute`
// wins over `requests_per_second`, and a zero value leaves that directive out
// entirely rather than becoming a limit of zero, which would refuse everything.
func TestLimitsSettingsFromRateLimit(t *testing.T) {
	cases := []struct {
		name              string
		rateLimit         *RateLimit
		requestsPerSecond float64
		burst             int
		netConnections    int
		excludePrefixes   []netip.Prefix
	}{
		{
			name:              "requests per minute wins",
			rateLimit:         &RateLimit{RequestsPerMinute: 120, RequestsPerSecond: 99, Burst: 3, NetConnections: 4},
			requestsPerSecond: 2,
			burst:             3,
			netConnections:    4,
			excludePrefixes:   []netip.Prefix{},
		},
		{
			name:              "requests per second when there is no minute rate",
			rateLimit:         &RateLimit{RequestsPerSecond: 5},
			requestsPerSecond: 5,
			excludePrefixes:   []netip.Prefix{},
		},
		{
			name:            "no rate at all leaves the bucket out",
			rateLimit:       &RateLimit{NetConnections: 2},
			netConnections:  2,
			excludePrefixes: []netip.Prefix{},
		},
		{
			name:      "the exclusions are the block's own",
			rateLimit: &RateLimit{ExcludeSubnets: []string{"192.0.2.0/24", "2001:db8::/32"}},
			excludePrefixes: []netip.Prefix{
				netip.MustParsePrefix("192.0.2.0/24"),
				netip.MustParsePrefix("2001:db8::/32"),
			},
		},
	}
	for _, c := range cases {
		settings := LimitsSettingsFromRateLimit(c.rateLimit)
		if settings.RequestsPerSecond != c.requestsPerSecond {
			t.Errorf("%s: requests per second = %f, want %f", c.name, settings.RequestsPerSecond, c.requestsPerSecond)
		}
		if settings.Burst != c.burst || settings.NetConnections != c.netConnections {
			t.Errorf(
				"%s: burst/net connections = %d/%d, want %d/%d",
				c.name,
				settings.Burst,
				settings.NetConnections,
				c.burst,
				c.netConnections,
			)
		}
		if !slices.Equal(settings.ExcludePrefixes, c.excludePrefixes) {
			t.Errorf("%s: exclude prefixes = %v, want %v", c.name, settings.ExcludePrefixes, c.excludePrefixes)
		}
		if settings.SweepTimeout <= 0 {
			t.Errorf("%s: sweep timeout = %s", c.name, settings.SweepTimeout)
		}
	}
}

// A limiter with no rate admits every request the concurrency cap allows, and
// one with no concurrency cap admits every request the bucket allows. An absent
// nginx directive is no directive, not a limit of zero.
func TestLimitsWithNoRateOrNoCapLimitOnlyTheOther(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now()

	// no rate: only the cap of two concurrent requests applies
	uncapped := NewLimits(ctx, &LimitsSettings{NetConnections: 2})
	addr := netip.MustParseAddr("198.51.100.20")
	releases := []func(){}
	for i := range 2 {
		release, ok := uncapped.AcquireRequest(addr, now)
		if !ok {
			t.Fatalf("request %d was refused with no rate configured", i)
		}
		releases = append(releases, release)
	}
	if _, ok := uncapped.AcquireRequest(addr, now); ok {
		t.Fatal("a request above the cap was admitted with no rate configured")
	}
	for _, release := range releases {
		release()
	}
	// and many in sequence, which a bucket would have refused
	for i := range 32 {
		release, ok := uncapped.AcquireRequest(addr, now)
		if !ok {
			t.Fatalf("sequential request %d was refused with no rate configured", i)
		}
		release()
	}

	// no cap: only the bucket applies, and connections are never refused
	unlimited := NewLimits(ctx, &LimitsSettings{RequestsPerSecond: 1, Burst: 1})
	connectionReleases := []func(){}
	for i := range 32 {
		release, ok := unlimited.AcquireConnection(addr, now)
		if !ok {
			t.Fatalf("connection %d was refused with no cap configured", i)
		}
		connectionReleases = append(connectionReleases, release)
	}
	for _, release := range connectionReleases {
		release()
	}
	for i := range 2 {
		release, ok := unlimited.AcquireRequest(addr, now)
		if !ok {
			t.Fatalf("request %d of the burst was refused", i)
		}
		release()
	}
	if _, ok := unlimited.AcquireRequest(addr, now); ok {
		t.Fatal("a request above the bucket was admitted with no cap configured")
	}
}

// The bucket is charged before the concurrency cap, as nginx evaluates
// limit_req before limit_conn. A request refused by the cap has already spent
// its token, so the two limits cannot be played off against each other to get
// more requests through than either allows.
func TestLimitsChargeTheBucketBeforeTheConcurrencyCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// one concurrent request, and a bucket of three
	limits := NewLimits(ctx, &LimitsSettings{
		RequestsPerSecond: 2,
		Burst:             2,
		NetConnections:    1,
	})
	addr := netip.MustParseAddr("198.51.100.21")
	now := time.Now()

	release, ok := limits.AcquireRequest(addr, now)
	if !ok {
		t.Fatal("the first request was refused")
	}
	// two more are refused by the cap, and each spends a token doing it
	for i := range 2 {
		if _, ok := limits.AcquireRequest(addr, now); ok {
			t.Fatalf("request %d above the cap was admitted", i)
		}
	}
	release()

	// the bucket is now empty, so the next request is refused by the rate even
	// though the cap has a slot
	if _, ok := limits.AcquireRequest(addr, now); ok {
		t.Fatal("a request was admitted from an empty bucket")
	}
	// and it comes back at the configured rate
	release, ok = limits.AcquireRequest(addr, now.Add(500*time.Millisecond))
	if !ok {
		t.Fatal("the refilled token was refused")
	}
	release()
}

// A caller whose address the limiter cannot read is served rather than refused:
// alt only ever serves real udp peers, and dropping valid traffic on a parse
// quirk is worse than not counting it.
func TestLimitedHandlerServesAnUnparseableAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := 0
	handler := NewLimitedHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served += 1
			w.WriteHeader(http.StatusOK)
		}),
		NewLimits(ctx, testLimitsSettings(t)),
	)

	// well past the burst of the configured block
	const requestCount = 8
	for i := range requestCount {
		r := httptest.NewRequest(http.MethodGet, "/hello", nil)
		r.RemoteAddr = "not-an-address"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d from an unparseable address = %d", i, w.Code)
		}
	}
	if served != requestCount {
		t.Fatalf("served = %d, want %d", served, requestCount)
	}
}
