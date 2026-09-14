package server

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// mainRateLimitExcludeSubnetsTestValue mirrors the address-family and prefix
// shapes supplied by main's services.yml. The Warp config tests separately
// prove that this semicolon-delimited value reaches the server environment.
const mainRateLimitExcludeSubnetsTestValue = "10.0.0.0/8;" +
	"172.16.0.0/12;" +
	"192.168.0.0/16;" +
	"65.19.157.32/27;" +
	"2001:470:173::/48;" +
	"65.49.70.64/27;" +
	"2001:470:99::/48;" +
	"185.217.1.200/29;" +
	"2a0b:c041:8::/48"

// Exclusion parsing and caller classification are the common boundary for
// every ip-owned limit. Both address families must take the same path so a
// route cannot accidentally exempt proxy ipv4 while limiting proxy ipv6.
func TestRateLimitClientRecognizesExcludedIpv4AndIpv6(t *testing.T) {
	prefixes, err := parseRateLimitExcludePrefixes(
		"198.51.100.0/24; 2001:db8:1200::/48",
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, clientAddress := range []string{
		"198.51.100.27:41000",
		"[::ffff:198.51.100.27]:41000",
		"[2001:db8:1200::27]:41000",
		"2001:db8:1200::27:41000",
	} {
		client, err := newRateLimitClient(clientAddress, prefixes)
		if err != nil {
			t.Errorf("classifying %q: %v", clientAddress, err)
			continue
		}
		if !client.Excluded() {
			t.Errorf("%q was not excluded", clientAddress)
		}
	}

	client, err := newRateLimitClientIp("2001:db8:1200::27", prefixes)
	if err != nil {
		t.Fatal(err)
	}
	if !client.Excluded() {
		t.Fatal("raw ipv6 was not excluded")
	}
}

// Every deployed exclusion must classify both its first and last address as
// excluded. Checking both edges catches accidental host-prefix parsing and
// off-by-one range implementations in the canonical server classifier.
func TestRateLimitClientExcludesEveryConfiguredRange(t *testing.T) {
	prefixes, err := parseRateLimitExcludePrefixes(mainRateLimitExcludeSubnetsTestValue)
	if err != nil {
		t.Fatal(err)
	}

	excludedClientIps := []string{
		"10.0.0.0",
		"10.255.255.255",
		"172.16.0.0",
		"172.31.255.255",
		"192.168.0.0",
		"192.168.255.255",
		"65.19.157.32",
		"65.19.157.63",
		"2001:470:173::",
		"2001:470:173:ffff:ffff:ffff:ffff:ffff",
		"65.49.70.64",
		"65.49.70.95",
		"2001:470:99::",
		"2001:470:99:ffff:ffff:ffff:ffff:ffff",
		"185.217.1.200",
		"185.217.1.207",
		"2a0b:c041:8::",
		"2a0b:c041:8:ffff:ffff:ffff:ffff:ffff",
	}
	for _, clientIp := range excludedClientIps {
		client, err := newRateLimitClientIp(clientIp, prefixes)
		if err != nil {
			t.Errorf("classifying excluded address %q: %v", clientIp, err)
			continue
		}
		if !client.Excluded() {
			t.Errorf("configured address %q was not excluded", clientIp)
		}
		if client.IpHashHex() != "" {
			t.Errorf("excluded address %q retained counter identity %q", clientIp, client.IpHashHex())
		}
	}

	if got := len(prefixes); got != len(strings.Split(mainRateLimitExcludeSubnetsTestValue, ";")) {
		t.Fatalf("parsed %d exclusion prefixes, want every configured prefix", got)
	}
}

// Addresses immediately outside each non-private deployed range must remain
// ordinary callers. This proves an exclusion cannot silently widen and remove
// neighboring internet clients from abuse controls.
func TestRateLimitClientLimitsAddressesAdjacentToConfiguredRanges(t *testing.T) {
	prefixes, err := parseRateLimitExcludePrefixes(mainRateLimitExcludeSubnetsTestValue)
	if err != nil {
		t.Fatal(err)
	}

	ordinaryClientIps := []string{
		"65.19.157.31",
		"65.19.157.64",
		"2001:470:172:ffff:ffff:ffff:ffff:ffff",
		"2001:470:174::",
		"65.49.70.63",
		"65.49.70.96",
		"2001:470:98:ffff:ffff:ffff:ffff:ffff",
		"2001:470:9a::",
		"185.217.1.199",
		"185.217.1.208",
		"2a0b:c041:7:ffff:ffff:ffff:ffff:ffff",
		"2a0b:c041:9::",
	}
	for _, clientIp := range ordinaryClientIps {
		clientAddr, err := netip.ParseAddr(clientIp)
		if err != nil {
			t.Errorf("classifying adjacent address %q: %v", clientIp, err)
			continue
		}
		if isRateLimitExcluded(clientAddr, prefixes) {
			t.Errorf("adjacent address %q was excluded", clientIp)
		}
	}
}

// A typo in the trusted infrastructure list must fail startup instead of
// silently turning the intended subnet back into ordinary rate-limited traffic.
func TestRateLimitExcludePrefixesRejectInvalidConfiguration(t *testing.T) {
	if _, err := parseRateLimitExcludePrefixes("198.51.100.0/24;not-a-cidr"); err == nil {
		t.Fatal("invalid exclusion was accepted")
	}
}

// The exclusion belongs outside the storage operation. This pins that an
// excluded infrastructure caller neither trips a limit nor consumes its
// counter, regardless of which route invokes the canonical limiter.
func TestRateLimitWindowDoesNotCountExcludedClient(t *testing.T) {
	prefixes, err := parseRateLimitExcludePrefixes("198.51.100.0/24;2001:db8:1200::/48")
	if err != nil {
		t.Fatal(err)
	}
	settings := RateLimitWindowSettings{
		Namespace: "test",
		Name:      "excluded",
		Duration:  time.Minute,
		Limit:     1,
	}

	for _, clientAddress := range []string{
		"198.51.100.27:41000",
		"[2001:db8:1200::27]:41000",
	} {
		client, err := newRateLimitClient(clientAddress, prefixes)
		if err != nil {
			t.Errorf("classifying %q: %v", clientAddress, err)
			continue
		}
		counterCalls := 0
		result, err := checkRateLimitWindow(
			context.Background(),
			client,
			settings,
			func(context.Context, string, time.Duration) (int64, error) {
				counterCalls += 1
				return 2, nil
			},
		)
		if err != nil {
			t.Errorf("checking %q: %v", clientAddress, err)
			continue
		}
		if counterCalls != 0 {
			t.Errorf("%q invoked the counter %d times, want 0", clientAddress, counterCalls)
		}
		if !result.Allowed || !result.Excluded || result.Count != 0 {
			t.Errorf("%q result = %+v, want allowed excluded zero-count", clientAddress, result)
		}
	}
}

// A non-excluded caller reaches the shared counter and is refused only after
// its configured allowance has been consumed.
func TestRateLimitWindowCountsNonExcludedClient(t *testing.T) {
	client := &RateLimitClient{clientIpHashHex: "clienthash"}
	settings := RateLimitWindowSettings{
		Namespace: "test",
		Name:      "ordinary",
		Duration:  time.Minute,
		Limit:     1,
	}
	seenKey := ""
	result, err := checkRateLimitWindow(
		context.Background(),
		client,
		settings,
		func(_ context.Context, key string, duration time.Duration) (int64, error) {
			seenKey = key
			if duration != time.Minute {
				t.Errorf("duration = %s, want %s", duration, time.Minute)
			}
			return 2, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if seenKey == "" {
		t.Fatal("counter was not invoked")
	}
	if result.Allowed || result.Excluded || result.Count != 2 {
		t.Fatalf("result = %+v, want refused non-excluded count 2", result)
	}
}

// Connect used to increment both Redis counters before checking its exclusion
// flag. Running this outside a test environment is deliberate: any Redis touch
// would require configuration and fail, while the correct early return is
// entirely local and supplies the required no-op release callback.
func TestExcludedConnectionRateLimitDoesNotTouchRedis(t *testing.T) {
	rateLimit := newConnectionRateLimit(
		context.Background(),
		&RateLimitClient{excluded: true},
		NewId(),
		DefaultConnectionRateLimitSettings(),
	)
	err, disconnect := rateLimit.Connect()
	if err != nil {
		t.Fatal(err)
	}
	if disconnect == nil {
		t.Fatal("excluded admission returned no release callback")
	}
	disconnect()
}

// Database-backed ip limits share the same no-storage exclusion contract.
// These calls run without a test database so any accidental query fails the
// test instead of being hidden by an empty result.
func TestExcludedDatabaseIpRateLimitsDoNotTouchPostgres(t *testing.T) {
	client := &RateLimitClient{excluded: true}
	result := CheckNetworkCreateIpRateLimit(
		context.Background(),
		client,
		5,
		24*time.Hour,
	)
	if result != (NetworkCreateIpRateLimitResult{}) {
		t.Fatalf("network-create result = %+v, want zero", result)
	}

	attemptId, allowed := CheckWalletChallengeIpRateLimit(
		context.Background(),
		client,
		41000,
		5*time.Minute,
		5,
	)
	if !allowed || attemptId != (Id{}) {
		t.Fatalf("wallet challenge allowed=%t id=%s, want true and zero", allowed, attemptId)
	}
	SetWalletChallengeIpRateLimitSuccess(context.Background(), attemptId, true)
}

// Exclusions remove the ip-owned history without disabling an identified
// account's global abuse budget. A successful operation must also clear that
// global-only history even though no address key exists.
func TestExcludedIpRateLimitAttemptRetainsGlobalLimit(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		client := &RateLimitClient{
			clientIpHashHex: "excluded-client",
			excluded:        true,
		}
		settings := IpRateLimitAttemptSettings{
			KeyPrefix:       "ip_ratelimit_test.",
			AddressLookback: time.Minute,
			AddressLimit:    2,
			GlobalLookback:  10 * time.Minute,
			GlobalLimit:     3,
		}
		now := NowUtc()
		var attemptId IpRateLimitAttemptId
		for attempt := 0; attempt < settings.GlobalLimit; attempt += 1 {
			var allowed bool
			var err error
			attemptId, allowed, err = CheckIpRateLimitAttempt(
				ctx,
				client,
				"identity_test",
				now.Add(time.Duration(attempt)*time.Millisecond),
				settings,
			)
			if err != nil {
				t.Fatal(err)
			}
			wantAllowed := attempt < settings.GlobalLimit-1
			if allowed != wantAllowed {
				t.Fatalf("attempt %d allowed = %t, want %t", attempt+1, allowed, wantAllowed)
			}
		}
		if attemptId.AddressRedisKey != "" {
			t.Fatalf("excluded address key = %q, want empty", attemptId.AddressRedisKey)
		}
		if attemptId.GlobalRedisKey == "" {
			t.Fatal("identified caller lost its global key")
		}
		Redis(ctx, func(r RedisClient) {
			if count := r.ZCard(ctx, attemptId.GlobalRedisKey).Val(); count != int64(settings.GlobalLimit) {
				t.Fatalf("global count = %d, want %d", count, settings.GlobalLimit)
			}
		})

		if err := ClearIpRateLimitAttempt(ctx, attemptId); err != nil {
			t.Fatal(err)
		}
		Redis(ctx, func(r RedisClient) {
			if count := r.Exists(ctx, attemptId.GlobalRedisKey).Val(); count != 0 {
				t.Fatalf("global key remains after success: count=%d", count)
			}
		})

		anonymousId, allowed, err := CheckIpRateLimitAttempt(
			ctx,
			client,
			"",
			now,
			settings,
		)
		if err != nil || !allowed {
			t.Fatalf("identity-less excluded caller allowed=%t err=%v", allowed, err)
		}
		if anonymousId.AddressRedisKey != "" || anonymousId.GlobalRedisKey != "" {
			t.Fatalf("identity-less excluded keys = %+v, want none", anonymousId)
		}
	})
}

// A continuously active caller receives a fresh counter at the exact next
// fixed-window boundary. The former unsuffixed INCR+EXPIRE implementation
// never reset while traffic continued and eventually locked out every honest
// long-running validator.
func TestIncrementRateLimitWindowResetsAtExactBoundary(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		duration := time.Minute
		start := time.Unix(1_800_000_000, 0).UTC().Truncate(duration)
		baseKey := "rate_limit_fixed_window_test_" + NewId().String()
		firstKey, err := rateLimitWindowKey(baseKey, duration, start)
		if err != nil {
			t.Fatal(err)
		}
		nextKey, err := rateLimitWindowKey(baseKey, duration, start.Add(duration))
		if err != nil {
			t.Fatal(err)
		}
		defer Redis(ctx, func(r RedisClient) {
			r.Del(ctx, firstKey, nextKey)
		})

		first, err := incrementRateLimitWindowAt(ctx, baseKey, duration, start)
		if err != nil || first != 1 {
			t.Fatalf("first count = %d, error = %v", first, err)
		}
		second, err := incrementRateLimitWindowAt(ctx, baseKey, duration, start.Add(duration-time.Millisecond))
		if err != nil || second != 2 {
			t.Fatalf("same-window count = %d, error = %v", second, err)
		}
		reset, err := incrementRateLimitWindowAt(ctx, baseKey, duration, start.Add(duration))
		if err != nil || reset != 1 {
			t.Fatalf("next-window count = %d, error = %v", reset, err)
		}
		if firstKey == nextKey {
			t.Fatal("adjacent fixed windows selected the same Redis key")
		}
	})
}
