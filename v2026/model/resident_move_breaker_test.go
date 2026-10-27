package model

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func residentMoveTargetBreakerSettingsForTest() ResidentMoveTargetBreakerSettings {
	return ResidentMoveTargetBreakerSettings{
		Ratio:    0.2,
		MinSends: 20,
		Timeout:  10 * time.Minute,
	}
}

func recordResidentMoveTargetSendsForTest(ctx context.Context, t testing.TB, ip netip.Addr, count int) {
	t.Helper()
	for range count {
		if err := RecordResidentMoveTargetSend(ctx, ip, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
}

func recordResidentMoveTargetFailuresForTest(ctx context.Context, t testing.TB, ip netip.Addr, count int) {
	t.Helper()
	for range count {
		counted, err := RecordResidentMoveTargetFailure(ctx, ip)
		if err != nil || !counted {
			t.Fatalf("failure counted = %t, %v", counted, err)
		}
	}
}

func isResidentMoveTargetBreakerOpenForTest(ctx context.Context, t testing.TB, ip netip.Addr) bool {
	t.Helper()
	open, err := IsResidentMoveTargetBreakerOpen(ctx, ip, residentMoveTargetBreakerSettingsForTest())
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// The breaker opens only past the ratio, with at least the minimum of sends,
// and only for the ip that failed.
func TestResidentMoveTargetBreakerOpensAtRatioAndMinimumSends(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ip := netip.MustParseAddr("192.0.2.10")
		otherIp := netip.MustParseAddr("2001:db8::10")

		// every send failed, but below the minimum sample
		recordResidentMoveTargetSendsForTest(ctx, t, ip, 19)
		recordResidentMoveTargetFailuresForTest(ctx, t, ip, 19)
		if isResidentMoveTargetBreakerOpenForTest(ctx, t, ip) {
			t.Fatal("the breaker opened below the minimum sends")
		}
		// at the minimum, 19 of 20 is past the ratio
		recordResidentMoveTargetSendsForTest(ctx, t, ip, 1)
		if !isResidentMoveTargetBreakerOpenForTest(ctx, t, ip) {
			t.Fatal("the breaker stayed closed past the ratio")
		}
		// it stays open, for the timeout
		if !isResidentMoveTargetBreakerOpenForTest(ctx, t, ip) {
			t.Fatal("the open breaker closed")
		}
		var openTtl time.Duration
		var windowExists int64
		server.Redis(ctx, func(r server.RedisClient) {
			openTtl = r.PTTL(ctx, residentMoveTargetBreakerOpenKey(ip)).Val()
			windowExists = r.Exists(ctx, residentMoveTargetBreakerWindowKey(ip)).Val()
		})
		if openTtl <= 0 || 10*time.Minute < openTtl {
			t.Fatalf("open ttl = %s, want the breaker timeout", openTtl)
		}
		// opening ended the window, so a closed breaker starts fresh
		if windowExists != 0 {
			t.Fatal("the window outlived the opening")
		}

		// the other family's ip of the same host is untouched
		if isResidentMoveTargetBreakerOpenForTest(ctx, t, otherIp) {
			t.Fatal("one ip's failures opened another ip's breaker")
		}
	})
}

// The ratio is exceeded, not met: 4 failures of 20 sends at 0.2 keep the
// breaker closed and a fifth opens it.
func TestResidentMoveTargetBreakerRatioBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ip := netip.MustParseAddr("198.51.100.20")

		recordResidentMoveTargetSendsForTest(ctx, t, ip, 20)
		recordResidentMoveTargetFailuresForTest(ctx, t, ip, 4)
		if isResidentMoveTargetBreakerOpenForTest(ctx, t, ip) {
			t.Fatal("the breaker opened at the ratio")
		}
		recordResidentMoveTargetFailuresForTest(ctx, t, ip, 1)
		if !isResidentMoveTargetBreakerOpenForTest(ctx, t, ip) {
			t.Fatal("the breaker stayed closed past the ratio")
		}
	})
}

// A failure report counts only inside a window of sends, and a disabled
// breaker never opens.
func TestResidentMoveTargetBreakerCountsFailuresOnlyAgainstSends(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ip := netip.MustParseAddr("203.0.113.30")

		counted, err := RecordResidentMoveTargetFailure(ctx, ip)
		if err != nil || counted {
			t.Fatalf("a failure without sends counted = %t, %v", counted, err)
		}
		var exists int64
		server.Redis(ctx, func(r server.RedisClient) {
			exists = r.Exists(ctx, residentMoveTargetBreakerWindowKey(ip)).Val()
		})
		if exists != 0 {
			t.Fatal("a failure report started a window without a ttl")
		}

		// a non-positive window counts no sends
		if err := RecordResidentMoveTargetSend(ctx, ip, 0); err != nil {
			t.Fatal(err)
		}
		if counted, err := RecordResidentMoveTargetFailure(ctx, ip); err != nil || counted {
			t.Fatalf("a failure counted against a disabled window = %t, %v", counted, err)
		}

		recordResidentMoveTargetSendsForTest(ctx, t, ip, 20)
		recordResidentMoveTargetFailuresForTest(ctx, t, ip, 20)
		var windowTtl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			windowTtl = r.PTTL(ctx, residentMoveTargetBreakerWindowKey(ip)).Val()
		})
		if windowTtl <= 0 || 10*time.Minute < windowTtl {
			t.Fatalf("window ttl = %s, want the window", windowTtl)
		}
		disabled := residentMoveTargetBreakerSettingsForTest()
		disabled.Timeout = 0
		if open, err := IsResidentMoveTargetBreakerOpen(ctx, ip, disabled); err != nil || open {
			t.Fatalf("a disabled breaker opened = %t, %v", open, err)
		}
	})
}

// Each ip's keys share one hash tag, so its scripts touch one cluster slot;
// different ips do not share one, so the family spreads over the cluster.
func TestResidentMoveTargetBreakerKeysShareOneTagPerIp(t *testing.T) {
	for _, ip := range []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("2001:db8::10"),
		// a mapped address is its v4 address
		netip.MustParseAddr("::ffff:192.0.2.10"),
	} {
		tag := residentMoveTargetBreakerTag(ip)
		for _, key := range []string{residentMoveTargetBreakerWindowKey(ip), residentMoveTargetBreakerOpenKey(ip)} {
			if !strings.HasPrefix(key, tag) || strings.Count(key, "{") != 1 {
				t.Fatalf("key %q does not carry the tag %q", key, tag)
			}
		}
	}
	if residentMoveTargetBreakerTag(netip.MustParseAddr("::ffff:192.0.2.10")) != residentMoveTargetBreakerTag(netip.MustParseAddr("192.0.2.10")) {
		t.Fatal("a mapped address has its own breaker")
	}
	if residentMoveTargetBreakerTag(netip.MustParseAddr("192.0.2.10")) == residentMoveTargetBreakerTag(netip.MustParseAddr("192.0.2.11")) {
		t.Fatal("two ips share a breaker")
	}
}
