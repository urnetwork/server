package alt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
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
