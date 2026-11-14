// Pins the seed-phrase sign-up budget of a source network
// (network_create_source_limit.go): the ipv4 /24 or ipv6 /48 that holds the
// caller's address, counted in Redis beside the address budget.
package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// The address of address bucket `bucket` (0-31, one ipv4 /29 each) of the
// source network 203.0.113.0/24.
func sourceNetworkV4Address(bucket int, port int) string {
	return fmt.Sprintf("203.0.113.%d:%d", 8*bucket+1, port)
}

// The address of address bucket `bucket` (0-255, one ipv6 /56 each) of the
// source network 2001:db8:a::/48.
func sourceNetworkV6Address(bucket int, port int) string {
	return fmt.Sprintf("[2001:db8:a:%x::1]:%d", bucket<<8, port)
}

// The first address bucket a test has not used after `used` sign-ups spread
// over whole address budgets from bucket 0.
func nextSourceNetworkBucket(used int) int {
	return (used + NetworkCreateDailyLimit - 1) / NetworkCreateDailyLimit
}

func seedphraseSignupAdmissionsCount(outcome seedphraseSignupOutcome, family string) float64 {
	return testutil.ToFloat64(seedphraseSignupAdmissionsCounter.WithLabelValues(string(outcome), family))
}

func requireSourceNetworkRefusal(t testing.TB, err error, what string) int {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was allowed; the source network budget is not enforced", what)
	}
	if !strings.HasPrefix(err.Error(), "429 ") {
		t.Fatalf("%s was refused with %q, want the 429 refusal the apps show", what, err)
	}
	if !strings.Contains(err.Error(), "address range") {
		t.Fatalf("%s was refused with %q, want the source network refusal, which names the shared address range", what, err)
	}
	var retryAfter interface{ RetryAfterSeconds() int }
	if !errors.As(err, &retryAfter) {
		t.Fatalf("%s refusal %q carries no retry hint for Retry-After", what, err)
	}
	return retryAfter.RetryAfterSeconds()
}

// The source network's admitted sign-ups in the short window `now` falls in,
// read from Redis.
func sourceNetworkShortCount(t testing.TB, ctx context.Context, address string, now time.Time) int64 {
	t.Helper()
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		t.Fatal(err)
	}
	shortKey, _, _, _ := networkCreateSourceNetworkKeys(addrPort.Addr().Unmap(), now, DefaultNetworkCreateSourceLimitSettings())
	return redisCount(t, ctx, shortKey)
}

func redisCount(t testing.TB, ctx context.Context, key string) (count int64) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, key).Result()
		if errors.Is(err, server.RedisNil) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		count, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	})
	return
}

// TestSeedphraseSignupOverItsSourceNetworkHourlyBudgetIsRefused is the root
// cause of the 2026-11-13 fix: the only budget a seed-phrase sign-up had was its
// address bucket's, so one caller rotating through the 32 address buckets of a
// /24 got 160 accounts a day. With the source network budget, the sign-up after
// the /24's hourly budget is refused even from an address bucket that has not
// signed up at all, with the 429 the apps already show; the refused attempt is
// not recorded against that address bucket, and another /24 is unaffected.
func TestSeedphraseSignupOverItsSourceNetworkHourlyBudgetIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		limit := DefaultNetworkCreateSourceLimitSettings().ShortLimit
		allowedBefore := seedphraseSignupAdmissionsCount(seedphraseSignupAllowed, "4")
		limitedBefore := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedNetworkShort, "4")

		for i := 0; i < limit; i += 1 {
			clientSession := regressionSession(ctx, sourceNetworkV4Address(i/NetworkCreateDailyLimit, 41000+i))
			mustSeedphraseCreate(t, clientSession, fmt.Sprintf("signup %d of the source network's %d", i+1, limit))
			clientSession.Cancel()
		}

		over := regressionSession(ctx, sourceNetworkV4Address(nextSourceNetworkBucket(limit), 42000))
		defer over.Cancel()
		_, err := seedphraseCreate(over)
		retryAfterSeconds := requireSourceNetworkRefusal(t, err, fmt.Sprintf("signup %d from a fresh address bucket of the same /24", limit+1))
		if retryAfterSeconds < 1 || 3600 < retryAfterSeconds {
			t.Fatalf("the hourly refusal says retry after %d seconds, want 1 to 3600", retryAfterSeconds)
		}

		// the refusal spent nothing: only the admitted sign-ups are recorded
		// in their address budgets
		if count := networkCreateAttemptRowCount(t, ctx); count != limit {
			t.Fatalf("network_create_attempt holds %d rows after %d sign-ups and 1 refusal, want %d", count, limit, limit)
		}

		// another /24 has its own budget
		other := regressionSession(ctx, "198.51.100.20:43000")
		defer other.Cancel()
		mustSeedphraseCreate(t, other, "the first signup from another /24")

		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupAllowed, "4") - allowedBefore; delta != float64(limit+1) {
			t.Fatalf("allowed sign-ups counted %v, want %d", delta, limit+1)
		}
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedNetworkShort, "4") - limitedBefore; delta != 1 {
			t.Fatalf("limited_network_short sign-ups counted %v, want 1", delta)
		}
	})
}

// TestSeedphraseSignupSourceNetworkIsTheIpv6Slash48: every /56 address bucket
// of a /48 counts in the /48's budget (a /48 holds 256 address buckets, 1,280
// accounts a day without it), and another /48 and an ipv4 /24 keep their own.
func TestSeedphraseSignupSourceNetworkIsTheIpv6Slash48(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		limit := DefaultNetworkCreateSourceLimitSettings().ShortLimit
		limitedBefore := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedNetworkShort, "6")
		check := func(address string) error {
			clientSession := regressionSession(ctx, address)
			defer clientSession.Cancel()
			return CheckNetworkCreateRateLimit(ctx, clientSession)
		}

		for i := 0; i < limit; i += 1 {
			if err := check(sourceNetworkV6Address(i/NetworkCreateDailyLimit, 41000+i)); err != nil {
				t.Fatalf("signup %d of the /48's %d was refused: %v", i+1, limit, err)
			}
		}
		requireSourceNetworkRefusal(t, check(sourceNetworkV6Address(nextSourceNetworkBucket(limit), 42000)), "a signup from a fresh /56 of the same /48")

		if err := check("[2001:db8:b::1]:43000"); err != nil {
			t.Fatalf("the first signup from another /48 was refused: %v", err)
		}
		if err := check("198.51.100.20:43001"); err != nil {
			t.Fatalf("the first signup from an ipv4 /24 was refused: %v", err)
		}
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedNetworkShort, "6") - limitedBefore; delta != 1 {
			t.Fatalf("limited_network_short ipv6 sign-ups counted %v, want 1", delta)
		}
	})
}

// TestSeedphraseSignupSourceNetworkBudgetsResetWithTheirWindows: the short
// budget refuses until its hour ends and the long budget until its UTC day
// ends, each refusal saying exactly how long, and a new window admits again.
// Every attempt comes from a fresh address bucket, so only the source network
// budget can refuse.
func TestSeedphraseSignupSourceNetworkBudgetsResetWithTheirWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultNetworkCreateSourceLimitSettings()
		settings.ShortLimit = 3
		settings.LongLimit = 5
		limitedLongBefore := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedNetworkLong, "4")

		bucket := 0
		check := func(now time.Time) error {
			clientSession := regressionSession(ctx, sourceNetworkV4Address(bucket, 41000+bucket))
			defer clientSession.Cancel()
			bucket += 1
			return checkNetworkCreateRateLimit(ctx, clientSession, now, settings)
		}
		admit := func(now time.Time, what string) {
			t.Helper()
			if err := check(now); err != nil {
				t.Fatalf("%s was refused: %v", what, err)
			}
		}
		refuse := func(now time.Time, what string, wantRetryAfterSeconds int) {
			t.Helper()
			if retryAfterSeconds := requireSourceNetworkRefusal(t, check(now), what); retryAfterSeconds != wantRetryAfterSeconds {
				t.Fatalf("%s says retry after %d seconds, want %d", what, retryAfterSeconds, wantRetryAfterSeconds)
			}
		}

		hour0 := time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC)
		for i := 0; i < settings.ShortLimit; i += 1 {
			admit(hour0, fmt.Sprintf("signup %d of the first hour", i+1))
		}
		// to the end of the hour, 10:15 to 11:00
		refuse(hour0, "the signup over the hourly budget", 45*60)

		hour1 := hour0.Add(time.Hour)
		admit(hour1, "the first signup of the next hour")
		admit(hour1, "the long budget's last signup")
		// to the end of the UTC day, 11:15 to 00:00
		refuse(hour1, "the signup over the daily budget", 12*3600+45*60)
		refuse(hour1.Add(time.Hour), "a signup in a fresh hour of the spent day", 11*3600+45*60)

		admit(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), "the first signup of the next day")

		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedNetworkLong, "4") - limitedLongBefore; delta != 2 {
			t.Fatalf("limited_network_long sign-ups counted %v, want 2", delta)
		}
	})
}

// TestNonSeedphraseSignupIsNotLimitedOrCountedBySourceNetwork: the budget is
// for seed-phrase accounts only. An email sign-up from a /24 whose seed-phrase
// budget is spent goes ahead and is not counted in it, while a seed-phrase
// sign-up from the same address is refused.
func TestNonSeedphraseSignupIsNotLimitedOrCountedBySourceNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		limit := DefaultNetworkCreateSourceLimitSettings().ShortLimit
		for i := 0; i < limit; i += 1 {
			clientSession := regressionSession(ctx, sourceNetworkV4Address(i/NetworkCreateDailyLimit, 41000+i))
			if err := CheckNetworkCreateRateLimit(ctx, clientSession); err != nil {
				t.Fatalf("signup %d of the source network's %d was refused: %v", i+1, limit, err)
			}
			clientSession.Cancel()
		}

		address := sourceNetworkV4Address(nextSourceNetworkBucket(limit), 42000)
		clientSession := regressionSession(ctx, address)
		defer clientSession.Cancel()

		userAuth := "source-network-email@example.com"
		password := "SomeValidPassword123!"
		result, err := NetworkCreate(NetworkCreateArgs{
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: "sourcenetemail",
			Terms:       true,
		}, clientSession)
		if err != nil {
			t.Fatalf("an email signup from a /24 with no seed-phrase budget left was refused: %v", err)
		}
		if result.Error != nil {
			t.Fatalf("the email signup returned the body error %q", result.Error.Message)
		}
		if result.VerificationRequired == nil {
			t.Fatalf("the email signup returned %+v, want a verification-required result", result)
		}
		if count := sourceNetworkShortCount(t, ctx, address, server.NowUtc()); count != int64(limit) {
			t.Fatalf("the source network counts %d sign-ups after %d seed-phrase sign-ups and 1 email sign-up, want %d", count, limit, limit)
		}

		_, err = seedphraseCreate(clientSession)
		requireSourceNetworkRefusal(t, err, "a seed-phrase signup from the same address")
	})
}

// TestSeedphraseSignupRefusedByItsAddressBudgetDoesNotSpendItsSourceNetwork:
// repeated attempts from one address bucket that the address budget refuses
// never reach the source network's budget, so a caller hammering a spent
// address cannot spend the budget of everyone else on its /24.
func TestSeedphraseSignupRefusedByItsAddressBudgetDoesNotSpendItsSourceNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		limit := DefaultNetworkCreateSourceLimitSettings().ShortLimit
		limitedBefore := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedAddress, "4")
		check := func(address string) error {
			clientSession := regressionSession(ctx, address)
			defer clientSession.Cancel()
			return CheckNetworkCreateRateLimit(ctx, clientSession)
		}

		address := sourceNetworkV4Address(0, 41000)
		for i := 0; i < NetworkCreateDailyLimit; i += 1 {
			if err := check(address); err != nil {
				t.Fatalf("signup %d of the address budget was refused: %v", i+1, err)
			}
		}
		const hammering = 10
		for i := 0; i < hammering; i += 1 {
			err := check(address)
			if err == nil || !strings.HasPrefix(err.Error(), "429 ") || strings.Contains(err.Error(), "address range") {
				t.Fatalf("attempt %d over the address budget returned %v, want the address budget's 429", i+1, err)
			}
		}
		if count := sourceNetworkShortCount(t, ctx, address, server.NowUtc()); count != NetworkCreateDailyLimit {
			t.Fatalf("the source network counts %d sign-ups after %d admitted and %d refused by the address budget, want %d", count, NetworkCreateDailyLimit, hammering, NetworkCreateDailyLimit)
		}
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedAddress, "4") - limitedBefore; delta != hammering {
			t.Fatalf("limited_address sign-ups counted %v, want %d", delta, hammering)
		}

		// the rest of the source network's budget is still there
		for i := NetworkCreateDailyLimit; i < limit; i += 1 {
			if err := check(sourceNetworkV4Address(i/NetworkCreateDailyLimit, 41000+i)); err != nil {
				t.Fatalf("signup %d of the source network's %d was refused: %v", i+1, limit, err)
			}
		}
	})
}

// TestSeedphraseSignupSourceNetworkFailsOpen: when the source network budget
// cannot be read the sign-up goes ahead on the address budget alone, counted
// as unavailable.
func TestSeedphraseSignupSourceNetworkFailsOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultNetworkCreateSourceLimitSettings()
		// unreadable: no window
		settings.ShortWindow = 0
		unavailableBefore := seedphraseSignupAdmissionsCount(seedphraseSignupUnavailable, "4")

		clientSession := regressionSession(ctx, sourceNetworkV4Address(0, 41000))
		defer clientSession.Cancel()
		if err := checkNetworkCreateRateLimit(ctx, clientSession, server.NowUtc(), settings); err != nil {
			t.Fatalf("a signup whose source network budget cannot be read was refused: %v", err)
		}
		if count := networkCreateAttemptRowCount(t, ctx); count != 1 {
			t.Fatalf("network_create_attempt holds %d rows, want the address budget's 1", count)
		}
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupUnavailable, "4") - unavailableBefore; delta != 1 {
			t.Fatalf("unavailable sign-ups counted %v, want 1", delta)
		}
	})
}

// TestSeedphraseSignupSourceNetworkKeysNameTheNetworkNotTheAddress: every
// address of a /24 or a /48 (an ipv4-mapped one included) shares one hash tag,
// another network has another, both windows' counters share the tag (one
// cluster slot for the script), and no key carries the address.
func TestSeedphraseSignupSourceNetworkKeysNameTheNetworkNotTheAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := DefaultNetworkCreateSourceLimitSettings()
		tag := func(address string) string {
			return networkCreateSourceNetworkTag(netip.MustParseAddr(address).Unmap(), settings)
		}
		for _, c := range []struct {
			a    string
			b    string
			same bool
		}{
			{a: "203.0.113.1", b: "203.0.113.254", same: true},
			{a: "203.0.113.1", b: "::ffff:203.0.113.77", same: true},
			{a: "203.0.113.1", b: "198.51.100.1", same: false},
			{a: "2001:db8:a::1", b: "2001:db8:a:ff00::1", same: true},
			{a: "2001:db8:a::1", b: "2001:db8:b::1", same: false},
		} {
			if same := tag(c.a) == tag(c.b); same != c.same {
				t.Errorf("%s and %s share a source network: %t, want %t", c.a, c.b, same, c.same)
			}
		}

		now := time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC)
		for _, address := range []string{"203.0.113.1", "2001:db8:a::1"} {
			shortKey, _, longKey, _ := networkCreateSourceNetworkKeys(netip.MustParseAddr(address), now, settings)
			wantTag := "{" + tag(address) + "}"
			if !strings.HasPrefix(shortKey, wantTag) || !strings.HasPrefix(longKey, wantTag) {
				t.Errorf("the counters %q and %q of %s do not share the hash tag %q", shortKey, longKey, address, wantTag)
			}
			if shortKey == longKey {
				t.Errorf("the short and long counters of %s are one key %q", address, shortKey)
			}
			for _, key := range []string{shortKey, longKey} {
				if strings.Contains(key, "203.0.113") || strings.Contains(key, "2001:db8") {
					t.Errorf("the key %q carries the address", key)
				}
			}
		}
	})
}

// TestNetworkCreateSourceRetryAfterIsTheWindowEnd: the windows are aligned to
// the unix epoch, and the wait is rounded up so that a retry after it is never
// early.
func TestNetworkCreateSourceRetryAfterIsTheWindowEnd(t *testing.T) {
	for _, c := range []struct {
		now    time.Time
		window time.Duration
		want   int
	}{
		{now: time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC), window: time.Hour, want: 45 * 60},
		{now: time.Date(2026, 11, 13, 10, 59, 59, int(time.Millisecond), time.UTC), window: time.Hour, want: 1},
		{now: time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC), window: time.Hour, want: 3600},
		{now: time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC), window: 24 * time.Hour, want: 13*3600 + 45*60},
	} {
		index, end := networkCreateSourceWindow(c.now, c.window)
		if !end.After(c.now) || end.Sub(c.now) > c.window {
			t.Errorf("the %s window of %s ends at %s", c.window, c.now, end)
		}
		if want := c.now.UnixMilli() / c.window.Milliseconds(); index != want {
			t.Errorf("the %s window of %s has index %d, want %d", c.window, c.now, index, want)
		}
		if got := networkCreateSourceRetryAfterSeconds(c.now, end); got != c.want {
			t.Errorf("the %s window of %s says retry after %d seconds, want %d", c.window, c.now, got, c.want)
		}
	}
}

// TestDefaultNetworkCreateSourceLimitSettings pins the production budgets and
// that every call returns its own instance.
func TestDefaultNetworkCreateSourceLimitSettings(t *testing.T) {
	settings := DefaultNetworkCreateSourceLimitSettings()
	want := NetworkCreateSourceLimitSettings{
		Ipv4PrefixLength: 24,
		Ipv6PrefixLength: 48,
		ShortWindow:      time.Hour,
		ShortLimit:       20,
		LongWindow:       24 * time.Hour,
		LongLimit:        50,
		AsnShortWindow:   time.Hour,
		AsnShortLimit:    200,
		AsnLongWindow:    24 * time.Hour,
		AsnLongLimit:     1500,
	}
	if *settings != want {
		t.Fatalf("default settings %+v, want %+v", *settings, want)
	}
	settings.ShortLimit = 1
	if DefaultNetworkCreateSourceLimitSettings().ShortLimit != want.ShortLimit {
		t.Fatal("the default settings share an instance")
	}
}

// Writes a GeoLite2 ASN database with one record per network.
func writeSourceLimitTestAsnDb(t testing.TB, networkAsns map[string]uint32) string {
	t.Helper()
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            server.GeoLite2AsnDatabaseType,
		IncludeReservedNetworks: true,
		RecordSize:              24,
	})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, asn := range networkAsns {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		record := mmdbtype.Map{
			"autonomous_system_number":       mmdbtype.Uint32(asn),
			"autonomous_system_organization": mmdbtype.String(fmt.Sprintf("documentation as %d", asn)),
		}
		if err := writer.Insert(network, record); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "geolite2-asn.mmdb")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteTo(file); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSeedphraseSignupCountsItsAutonomousSystemAdmittedSignupsOnly: admitted
// sign-ups are counted in both windows of their autonomous system, and one its
// source network refuses, which the autonomous system admitted and counted
// first, is taken back out of both.
func TestSeedphraseSignupCountsItsAutonomousSystemAdmittedSignupsOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := useSourceLimitTestAsnDb(t)
		defer restore()

		settings := DefaultNetworkCreateSourceLimitSettings()
		settings.ShortLimit = 2
		now := time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC)
		check := func(address string) error {
			clientSession := regressionSession(ctx, address)
			defer clientSession.Cancel()
			return checkNetworkCreateRateLimit(ctx, clientSession, now, settings)
		}

		for _, address := range []string{sourceNetworkV4Address(0, 41000), sourceNetworkV4Address(1, 41001)} {
			if err := check(address); err != nil {
				t.Fatalf("signup from %s was refused: %v", address, err)
			}
		}
		requireSourceNetworkRefusal(t, check(sourceNetworkV4Address(2, 41002)), "the signup over the /24's budget of 2")
		// the same autonomous system on another /24
		for i := 0; i < 2; i += 1 {
			if err := check(fmt.Sprintf("198.51.100.%d:%d", 8*i+1, 42000+i)); err != nil {
				t.Fatalf("signup %d from another /24 of the same autonomous system was refused: %v", i+1, err)
			}
		}
		if err := check("192.0.2.1:43000"); err != nil {
			t.Fatalf("signup from another autonomous system was refused: %v", err)
		}

		for _, c := range []struct {
			asn  uint32
			want int64
		}{
			{asn: 64500, want: 4},
			{asn: 64501, want: 1},
		} {
			shortKey, _, longKey, _ := networkCreateSourceAsnKeys(c.asn, now, settings)
			if count := redisCount(t, ctx, shortKey); count != c.want {
				t.Errorf("autonomous system %d counts %d admitted sign-ups in the hour, want %d (the one its /24 refused taken back out)", c.asn, count, c.want)
			}
			if count := redisCount(t, ctx, longKey); count != c.want {
				t.Errorf("autonomous system %d counts %d admitted sign-ups in the day, want %d", c.asn, count, c.want)
			}
		}
	})
}

// TestSeedphraseSignupSpreadOverSlash24sOfOneAutonomousSystemIsRefused: the
// 2026-11-14 farm spread its sign-ups over many /24s of one autonomous system,
// each /24 inside its own budget. Here the sign-ups spread evenly over three
// /24s of one system, 15 each against each /24's 20, and the one after the
// system's budget is refused from a /24 with room and an address bucket that
// has made none, with the 429 that names the shared provider. The refusal
// spends no budget: not its address bucket's, its /24's or the system's. The
// budget here is 45 because only three documentation /24s exist; the
// production budget is pinned over /48s
// (TestSeedphraseSignupOverItsAutonomousSystemHourlyBudgetIsRefused).
func TestSeedphraseSignupSpreadOverSlash24sOfOneAutonomousSystemIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore, err := server.Testing_UseIpAsnDatabase(writeSourceLimitTestAsnDb(t, map[string]uint32{
			"203.0.113.0/24":  64500,
			"198.51.100.0/24": 64500,
			"192.0.2.0/24":    64500,
		}))
		if err != nil {
			t.Fatal(err)
		}
		defer restore()

		settings := DefaultNetworkCreateSourceLimitSettings()
		settings.AsnShortLimit = 45
		now := time.Date(2026, 11, 14, 13, 20, 0, 0, time.UTC)
		allowedBefore := seedphraseSignupAdmissionsCount(seedphraseSignupAllowed, "4")
		limitedBefore := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedAsnShort, "4")
		check := func(address string) error {
			clientSession := regressionSession(ctx, address)
			defer clientSession.Cancel()
			return checkNetworkCreateRateLimit(ctx, clientSession, now, settings)
		}

		slash24s := []string{"203.0.113", "198.51.100", "192.0.2"}
		perSlash24 := settings.AsnShortLimit / len(slash24s)
		if settings.ShortLimit <= perSlash24 {
			t.Fatalf("%d sign-ups a /24 would spend its own budget of %d", perSlash24, settings.ShortLimit)
		}
		for i := 0; i < settings.AsnShortLimit; i += 1 {
			slash24 := slash24s[i%len(slash24s)]
			bucket := (i / len(slash24s)) / NetworkCreateDailyLimit
			if err := check(fmt.Sprintf("%s.%d:%d", slash24, 8*bucket+1, 41000+i)); err != nil {
				t.Fatalf("signup %d of the autonomous system's %d, from %s.0/24, was refused: %v", i+1, settings.AsnShortLimit, slash24, err)
			}
		}

		overBucket := nextSourceNetworkBucket(perSlash24)
		over := fmt.Sprintf("192.0.2.%d:42000", 8*overBucket+1)
		if retryAfterSeconds := requireAsnRefusal(t, check(over), "a signup from a /24 with room in a spent autonomous system"); retryAfterSeconds != 40*60 {
			t.Fatalf("the refusal says retry after %d seconds, want the 2,400 to the end of the hour", retryAfterSeconds)
		}

		if count := networkCreateAttemptRowCount(t, ctx); count != settings.AsnShortLimit {
			t.Fatalf("network_create_attempt holds %d rows after %d sign-ups and 1 refusal, want %d", count, settings.AsnShortLimit, settings.AsnShortLimit)
		}
		if count := sourceNetworkShortCount(t, ctx, over, now); count != int64(perSlash24) {
			t.Fatalf("the refused sign-up's /24 counts %d sign-ups, want its %d admitted", count, perSlash24)
		}
		shortKey, _, longKey, _ := networkCreateSourceAsnKeys(64500, now, settings)
		if short, long := redisCount(t, ctx, shortKey), redisCount(t, ctx, longKey); short != int64(settings.AsnShortLimit) || long != int64(settings.AsnShortLimit) {
			t.Fatalf("the autonomous system counts %d in the hour and %d in the day, want its %d admitted in both", short, long, settings.AsnShortLimit)
		}
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupAllowed, "4") - allowedBefore; delta != float64(settings.AsnShortLimit) {
			t.Fatalf("allowed sign-ups counted %v, want %d", delta, settings.AsnShortLimit)
		}
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedAsnShort, "4") - limitedBefore; delta != 1 {
			t.Fatalf("limited_asn_short sign-ups counted %v, want 1", delta)
		}
	})
}

// TestSeedphraseSignupAutonomousSystemBudgetsResetWithTheirWindows: the short
// budget refuses until its hour ends and the long budget until its UTC day
// ends, each refusal saying exactly how long, and a new window admits again.
// Every attempt comes from a fresh address bucket, inside its /24's budgets, so
// only the autonomous system's budgets can refuse.
func TestSeedphraseSignupAutonomousSystemBudgetsResetWithTheirWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := useSourceLimitTestAsnDb(t)
		defer restore()

		settings := DefaultNetworkCreateSourceLimitSettings()
		settings.AsnShortLimit = 3
		settings.AsnLongLimit = 5
		limitedLongBefore := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedAsnLong, "4")

		bucket := 0
		check := func(now time.Time) error {
			clientSession := regressionSession(ctx, sourceNetworkV4Address(bucket, 41000+bucket))
			defer clientSession.Cancel()
			bucket += 1
			return checkNetworkCreateRateLimit(ctx, clientSession, now, settings)
		}
		admit := func(now time.Time, what string) {
			t.Helper()
			if err := check(now); err != nil {
				t.Fatalf("%s was refused: %v", what, err)
			}
		}
		refuse := func(now time.Time, what string, wantRetryAfterSeconds int) {
			t.Helper()
			if retryAfterSeconds := requireAsnRefusal(t, check(now), what); retryAfterSeconds != wantRetryAfterSeconds {
				t.Fatalf("%s says retry after %d seconds, want %d", what, retryAfterSeconds, wantRetryAfterSeconds)
			}
		}

		hour0 := time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC)
		for i := 0; i < settings.AsnShortLimit; i += 1 {
			admit(hour0, fmt.Sprintf("signup %d of the first hour", i+1))
		}
		// to the end of the hour, 10:15 to 11:00
		refuse(hour0, "the signup over the hourly budget", 45*60)

		hour1 := hour0.Add(time.Hour)
		admit(hour1, "the first signup of the next hour")
		admit(hour1, "the long budget's last signup")
		// to the end of the UTC day, 11:15 to 00:00
		refuse(hour1, "the signup over the daily budget", 12*3600+45*60)
		refuse(hour1.Add(time.Hour), "a signup in a fresh hour of the spent day", 11*3600+45*60)

		admit(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), "the first signup of the next day")

		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupLimitedAsnLong, "4") - limitedLongBefore; delta != 2 {
			t.Fatalf("limited_asn_long sign-ups counted %v, want 2", delta)
		}
	})
}

// TestSeedphraseSignupAutonomousSystemBudgetFailsOpen: when the autonomous
// system's budget cannot be read the sign-up goes on to its source network's
// budget, which still refuses over its own budget, and an admitted one is
// counted as unavailable.
func TestSeedphraseSignupAutonomousSystemBudgetFailsOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := useSourceLimitTestAsnDb(t)
		defer restore()

		settings := DefaultNetworkCreateSourceLimitSettings()
		// unreadable: no window
		settings.AsnShortWindow = 0
		settings.AsnShortLimit = 1
		settings.ShortLimit = 2
		now := time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC)
		unavailableBefore := seedphraseSignupAdmissionsCount(seedphraseSignupUnavailable, "4")
		check := func(bucket int) error {
			clientSession := regressionSession(ctx, sourceNetworkV4Address(bucket, 41000+bucket))
			defer clientSession.Cancel()
			return checkNetworkCreateRateLimit(ctx, clientSession, now, settings)
		}

		for bucket := 0; bucket < settings.ShortLimit; bucket += 1 {
			if err := check(bucket); err != nil {
				t.Fatalf("signup %d with an unreadable autonomous system budget was refused: %v", bucket+1, err)
			}
		}
		requireSourceNetworkRefusal(t, check(settings.ShortLimit), "the signup over its /24's budget")
		if delta := seedphraseSignupAdmissionsCount(seedphraseSignupUnavailable, "4") - unavailableBefore; delta != float64(settings.ShortLimit) {
			t.Fatalf("unavailable sign-ups counted %v, want %d", delta, settings.ShortLimit)
		}
	})
}

// TestSeedphraseSignupWithNoKnownAutonomousSystemHasNoAsnBudget: an address
// the database names no autonomous system for, or a process with no database,
// has only its address and source network budgets.
func TestSeedphraseSignupWithNoKnownAutonomousSystemHasNoAsnBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultNetworkCreateSourceLimitSettings()
		settings.AsnShortLimit = 1
		now := time.Date(2026, 11, 13, 10, 15, 0, 0, time.UTC)
		check := func(address string) error {
			clientSession := regressionSession(ctx, address)
			defer clientSession.Cancel()
			return checkNetworkCreateRateLimit(ctx, clientSession, now, settings)
		}

		// a database with no record for 198.51.100.0/24
		restore, err := server.Testing_UseIpAsnDatabase(writeSourceLimitTestAsnDb(t, map[string]uint32{
			"203.0.113.0/24": 64500,
		}))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i += 1 {
			if err := check(fmt.Sprintf("198.51.100.%d:%d", 8*i+1, 41000+i)); err != nil {
				restore()
				t.Fatalf("signup %d from an address with no autonomous system was refused: %v", i+1, err)
			}
		}
		restore()

		// no database
		restore, err = server.Testing_UseIpAsnDatabase("")
		if err != nil {
			t.Fatal(err)
		}
		defer restore()
		for i := 0; i < 2; i += 1 {
			if err := check(fmt.Sprintf("203.0.113.%d:%d", 8*i+1, 42000+i)); err != nil {
				t.Fatalf("signup %d with no autonomous system database was refused: %v", i+1, err)
			}
		}
	})
}

// TestSeedphraseSignupAutonomousSystemKeysNameTheSystem: both windows'
// counters of a system share one hash tag (one cluster slot for the script),
// each names its window, and the hourly counter is the key the histogram
// counted in before the budget, so the hour's count carries over a deploy.
func TestSeedphraseSignupAutonomousSystemKeysNameTheSystem(t *testing.T) {
	settings := DefaultNetworkCreateSourceLimitSettings()
	now := time.Date(2026, 11, 14, 13, 20, 0, 0, time.UTC)
	shortKey, shortEnd, longKey, longEnd := networkCreateSourceAsnKeys(64500, now, settings)
	hour := now.UnixMilli() / time.Hour.Milliseconds()
	day := now.UnixMilli() / (24 * time.Hour).Milliseconds()
	if want := fmt.Sprintf("{ncsrc_asn_64500}w3600000_%d", hour); shortKey != want {
		t.Errorf("hourly key %q, want %q", shortKey, want)
	}
	if want := fmt.Sprintf("{ncsrc_asn_64500}w86400000_%d", day); longKey != want {
		t.Errorf("daily key %q, want %q", longKey, want)
	}
	if !shortEnd.Equal(time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)) || !longEnd.Equal(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the windows end at %s and %s, want the hour's and the UTC day's ends", shortEnd, longEnd)
	}
	otherShortKey, _, _, _ := networkCreateSourceAsnKeys(64501, now, settings)
	if otherShortKey == shortKey {
		t.Error("two autonomous systems share a counter")
	}
}
