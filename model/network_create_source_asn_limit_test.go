// Pins the seed-phrase sign-up budget of an autonomous system
// (network_create_source_limit.go) at its production numbers. These tests use
// only what the limiter had before the budget, so they also run against it.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The documentation networks of the autonomous system tests (RFC 5737, RFC
// 3849) and the documentation autonomous systems they belong to (RFC 5398):
// 203.0.113.0/24, 198.51.100.0/24 and 2001:db8::/32 are AS64500, and
// 192.0.2.0/24 is AS64501. Returns the restore of the process's database.
func useSourceLimitTestAsnDb(t testing.TB) func() {
	t.Helper()
	restore, err := server.Testing_UseIpAsnDatabase(writeSourceLimitTestAsnDb(t, map[string]uint32{
		"203.0.113.0/24":  64500,
		"198.51.100.0/24": 64500,
		"2001:db8::/32":   64500,
		"192.0.2.0/24":    64501,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return restore
}

// The address of address bucket `bucket` (one /56) of the ipv6 /48 numbered
// `network` in 2001:db8::/32, AS64500.
func asnSpreadV6Address(network int, bucket int, port int) string {
	return fmt.Sprintf("[2001:db8:%x:%x::1]:%d", network+1, bucket<<8, port)
}

// Makes `count` seed-phrase sign-ups spread over the /48s of 2001:db8::/32,
// each /48 inside its hourly budget and each /56 inside its address budget, as
// a farm holding many networks of one autonomous system does. Returns the
// first /48 it did not use.
func spreadAsnSignups(t testing.TB, ctx context.Context, now time.Time, count int) int {
	t.Helper()
	perNetwork := DefaultNetworkCreateSourceLimitSettings().ShortLimit
	for i := 0; i < count; i += 1 {
		network := i / perNetwork
		bucket := (i % perNetwork) / NetworkCreateDailyLimit
		clientSession := regressionSession(ctx, asnSpreadV6Address(network, bucket, 41000+i))
		err := checkNetworkCreateRateLimit(ctx, clientSession, now, DefaultNetworkCreateSourceLimitSettings())
		clientSession.Cancel()
		if err != nil {
			t.Fatalf("signup %d of %d, from /48 %d, was refused: %v", i+1, count, network, err)
		}
	}
	return (count + perNetwork - 1) / perNetwork
}

func requireAsnRefusal(t testing.TB, err error, what string) int {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was allowed; the autonomous system budget is not enforced", what)
	}
	if !strings.HasPrefix(err.Error(), "429 ") {
		t.Fatalf("%s was refused with %q, want the 429 refusal the apps show", what, err)
	}
	if !strings.Contains(err.Error(), "internet provider") {
		t.Fatalf("%s was refused with %q, want the autonomous system refusal, which names the shared internet provider", what, err)
	}
	var retryAfter interface{ RetryAfterSeconds() int }
	if !errors.As(err, &retryAfter) {
		t.Fatalf("%s refusal %q carries no retry hint for Retry-After", what, err)
	}
	return retryAfter.RetryAfterSeconds()
}

// TestSeedphraseSignupOverItsAutonomousSystemHourlyBudgetIsRefused is the root
// cause of the 2026-11-14 change at the production budget: a farm spread its
// sign-ups over many networks of one autonomous system, every network inside
// its own budget, so nothing refused it (one system made 940 to 1,530 sign-ups
// an hour). After 200 sign-ups over ten /48s of AS64500 in an hour, the next
// one from a /48 and an address bucket that have made none is refused with the
// 429 the apps show, until the hour ends.
func TestSeedphraseSignupOverItsAutonomousSystemHourlyBudgetIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := useSourceLimitTestAsnDb(t)
		defer restore()
		now := time.Date(2026, 11, 14, 13, 20, 0, 0, time.UTC)

		next := spreadAsnSignups(t, ctx, now, 200)

		over := regressionSession(ctx, asnSpreadV6Address(next, 0, 42000))
		defer over.Cancel()
		err := checkNetworkCreateRateLimit(ctx, over, now, DefaultNetworkCreateSourceLimitSettings())
		if retryAfterSeconds := requireAsnRefusal(t, err, "signup 201 of the autonomous system, from a fresh /48"); retryAfterSeconds != 40*60 {
			t.Fatalf("the refusal says retry after %d seconds, want the 2,400 to the end of the hour", retryAfterSeconds)
		}
		// the refused attempt is not recorded in its address budget
		if count := networkCreateAttemptRowCount(t, ctx); count != 200 {
			t.Fatalf("network_create_attempt holds %d rows after 200 sign-ups and 1 refusal, want 200", count)
		}
	})
}

// TestSeedphraseSignupAutonomousSystemsHaveIndependentBudgets: a spent
// autonomous system's budget refuses none of another's sign-ups, ipv4 or not,
// and the next hour admits the spent one again.
func TestSeedphraseSignupAutonomousSystemsHaveIndependentBudgets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := useSourceLimitTestAsnDb(t)
		defer restore()
		now := time.Date(2026, 11, 14, 13, 20, 0, 0, time.UTC)
		check := func(address string, now time.Time) error {
			clientSession := regressionSession(ctx, address)
			defer clientSession.Cancel()
			return checkNetworkCreateRateLimit(ctx, clientSession, now, DefaultNetworkCreateSourceLimitSettings())
		}

		next := spreadAsnSignups(t, ctx, now, 200)
		// AS64501, 192.0.2.0/24
		for i := 0; i < 3; i += 1 {
			if err := check(fmt.Sprintf("192.0.2.%d:%d", 8*i+1, 43000+i), now); err != nil {
				t.Fatalf("signup %d from another autonomous system was refused: %v", i+1, err)
			}
		}
		// the hour after: AS64500 admits again
		if err := check(asnSpreadV6Address(next, 0, 44000), now.Add(time.Hour)); err != nil {
			t.Fatalf("the first signup of the next hour from the spent autonomous system was refused: %v", err)
		}
	})
}
